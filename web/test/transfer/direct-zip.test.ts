import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { FileGeometry } from '../../src/content/geometry'
import {
  V2RevisionCapacityBusyError,
  type V2OpenedRevision,
} from '../../src/content/v2-session-services'
import { V2_REVISION_CODE_QUOTA } from '../../src/content/v2-flow'
import {
  createFailureIdentity,
  createProtocolFailure,
} from '../../src/diagnostics/incident'
import type { DirectZipWriterCheckpointV1 } from '../../src/output/direct-zip/writer'
import {
  DirectZipCatalogSourceV1,
  DirectZipOrderedCoordinatorV1,
  DirectZipRuntimeUnsupportedError,
  DirectZipTransferOutputV1,
  createDirectZipExecutionV1,
  transferDirectZipFileV1,
  type DirectZipAuthenticatedRootV1,
  type DirectZipOutputSessionV1,
  type DirectZipOrderedFileV1,
  type DirectZipOrderedMemberV1,
  type DirectZipOrderedOutputV1,
  type DirectZipOrderedSourceV1,
  type DirectZipReplayAuthorityV1,
  type DirectZipMemberRollbackAuthorityV1,
} from '../../src/transfer/direct-zip'
import {
  V2RevisionCapacityCoordinator,
  type V2RevisionCapacityClock,
  type V2RevisionCapacityWaitSnapshot,
} from '../../src/transfer/revision-capacity/public'
import {
  candidateObservation,
  createWriterHarness,
  observationDigest,
} from '../output/direct-zip/writer/fault-model'

const SIGNAL = new AbortController().signal
const ROOT: DirectZipAuthenticatedRootV1 = Object.freeze({
  directoryId: 'root-id',
  generation: 'root-generation',
  discoveryEvidence: new TextEncoder().encode('root'),
})

describe('direct ZIP ordered transfer composition', () => {
  it('merges authenticated directory lookahead by unsigned artifact-path bytes', async () => {
    const rootId = id(1)
    const childId = id(2)
    const siblingFile = catalogFile(id(3), 'a.txt', 1n)
    const childFile = catalogFile(id(4), 'z.txt', 1n)
    const root = committed(rootId, 11, 2)
    const child = committed(childId, 12, 1)
    const entries = new Map([
      [root.directoryIdText, [catalogDirectory(childId, 'a'), siblingFile]],
      [child.directoryIdText, [childFile]],
    ])
    const directories = new Map([
      [root.directoryIdText, root],
      [child.directoryIdText, child],
    ])
    const source = new DirectZipCatalogSourceV1({
      catalog: {
        loadDirectory: async (requested: Uint8Array) => {
          const text = base64(requested)
          const found = directories.get(text)
          if (found === undefined) throw new Error('missing test directory')
          return found
        },
        entries: async function* (directory: V2CommittedDirectory) {
          for (const entry of entries.get(directory.directoryIdText) ?? []) yield entry
        },
      } as never,
      descriptor: { syntheticRoot: rootId } as never,
      selection: {
        defaultSelected: true,
        canonicalRules: [],
        selected: () => true,
        directorySelected: () => true,
        decision: () => 'default-rule',
        shouldDiscover: () => true,
      },
      intent: {
        plan: { kind: 'direct-resumable-zip' },
        artifact: {
          kind: 'zip-archive',
          layout: { anchor: { kind: 'synthetic-root' }, name: 'root' },
        },
      } as never,
      maximumNodeClaims: 10,
    })

    const paths: string[] = []
    for await (const member of source.members(SIGNAL)) paths.push(member.artifactPath.join('/'))
    expect(paths).toEqual(['root/a', 'root/a.txt', 'root/a/z.txt'])
  })

  it('ignores unselected siblings outside a frozen directory result-root anchor', async () => {
    const rootId = id(21)
    const unrelatedId = id(22)
    const anchorId = id(23)
    const anchorIdText = base64(anchorId)
    const root = committed(rootId, 31, 2)
    const anchor = committed(anchorId, 32, 1)
    const entries = new Map([
      [root.directoryIdText, [
        catalogDirectory(unrelatedId, 'a-unrelated'),
        catalogDirectory(anchorId, 'b-anchor'),
      ]],
      [anchor.directoryIdText, [catalogFile(id(24), 'z.txt', 1n)]],
    ])
    const source = new DirectZipCatalogSourceV1({
      catalog: {
        loadDirectory: async (requested: Uint8Array) => {
          const text = base64(requested)
          if (text === root.directoryIdText) return root
          if (text === anchor.directoryIdText) return anchor
          throw new Error('unselected sibling must not be discovered')
        },
        entries: async function* (directory: V2CommittedDirectory) {
          for (const entry of entries.get(directory.directoryIdText) ?? []) yield entry
        },
      } as never,
      descriptor: { syntheticRoot: rootId } as never,
      selection: {
        defaultSelected: false,
        canonicalRules: [],
        selected: (entry: V2CatalogEntry) => entry.idText === anchorIdText || entry.kind === 'file',
        directorySelected: (directoryId: string) => directoryId === anchorIdText,
        decision: () => 'default-rule',
        shouldDiscover: (directoryId: string) => directoryId === anchorIdText,
      },
      intent: {
        plan: { kind: 'direct-resumable-zip' },
        artifact: {
          kind: 'zip-archive',
          layout: {
            anchor: { kind: 'directory', directoryId: base64(anchorId), sourcePath: 'b-anchor' },
            name: 'root',
          },
        },
      } as never,
      maximumNodeClaims: 10,
    })
    const paths: string[] = []
    for await (const member of source.members(SIGNAL)) paths.push(member.artifactPath.join('/'))
    expect(paths).toEqual(['root/z.txt'])
  })

  it('keeps replay, admission, and file transfer in one canonical serial order', async () => {
    const members = [directory('root/a'), file('root/a.txt', 2n), file('root/a/z.txt', 1n)]
    const calls: string[] = []
    const output: DirectZipOrderedOutputV1 = {
      beginTraversal: async () => { calls.push('root') },
      visit: async (ordinal, member) => {
        calls.push(`visit:${ordinal.toString()}:${member.artifactPath.join('/')}`)
        if (ordinal === 1n) return 'replayed'
        return member.kind === 'file' ? 'transfer-file' : 'admitted'
      },
      finishTraversal: async ordinal => { calls.push(`finish:${ordinal.toString()}`) },
      materializationSummary: () => ({ entryCount: 4n, fileCount: 2n, directoryCount: 2n, rawBytes: 3n }),
    }
    const observed: bigint[] = []
    const replayed: bigint[] = []
    const source: DirectZipOrderedSourceV1 = {
      root: async () => ROOT,
      members: async function* () { for (const member of members) yield member },
    }
    const measure = await new DirectZipOrderedCoordinatorV1({
      source,
      output,
      signal: SIGNAL,
      observeSelectedFile: size => observed.push(size),
      observeReplayedFile: size => replayed.push(size),
      transferFile: async member => { calls.push(`transfer:${member.artifactPath.join('/')}`) },
      finishMeasure: () => ({ discoveredFiles: 2, discoveredBytes: 3n, discovery: 'complete', sizeClass: 'small' }),
    }).run()

    expect(calls).toEqual([
      'root',
      'visit:1:root/a',
      'visit:2:root/a.txt',
      'transfer:root/a.txt',
      'visit:3:root/a/z.txt',
      'transfer:root/a/z.txt',
      'finish:4',
    ])
    expect(observed).toEqual([2n, 1n])
    expect(replayed).toEqual([])
    expect(measure.discovery).toBe('complete')
  })

  it('treats content-block checkpoints as policy observations and creates one complete artifact', async () => {
    const harness = createWriterHarness()
    const output = transferOutput(harness.writer(), harness)
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    expect(await output.visit(1n, member, SIGNAL)).toBe('transfer-file')
    const transaction = await output.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    expect(await transaction.observeCheckpoint(SIGNAL)).toBe(0n)
    expect(harness.target.closeAttemptCount).toBe(0)
    await transaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
    expect(await transaction.observeCheckpoint(SIGNAL)).toBe(0n)
    await transaction.commit(SIGNAL)
    await output.finishTraversal(2n, SIGNAL)

    const published = await output.publish()
    expect(published.completion.exactArchiveBytes).toBe(BigInt(harness.target.visible.byteLength))
    expect(published.checkpoint.phase).toBe('closing')
    expect(harness.target.artifactCount).toBe(1)
    expect(harness.target.rangeReads).toEqual([])
    expect(harness.target.closeAttemptCount).toBe(2)
    expect(output.materializationSummary()).toEqual({
      entryCount: 2n,
      fileCount: 1n,
      directoryCount: 1n,
      rawBytes: 6n,
    })
  })

  it('forces an inside-member pause cut without claiming the incomplete file', async () => {
    const harness = createWriterHarness()
    const output = transferOutput(harness.writer(), harness)
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    await transaction.observeCheckpoint(SIGNAL)
    expect(harness.target.closeAttemptCount).toBe(0)

    const paused = await output.pause()
    expect(paused.checkpoint.phase).toBe('inside-member')
    expect(paused.checkpoint.member?.payloadOffset).toBe(3n)
    expect(paused.additionalTemporaryBytesUpperBound).toBe(paused.checkpoint.committedLength)
    expect(paused.materialization).toEqual({
      entryCount: 1n,
      fileCount: 0n,
      directoryCount: 1n,
      rawBytes: 0n,
    })
    expect(harness.target.closeAttemptCount).toBe(1)
  })

  it('resumes at the exact member source offset after a pause', async () => {
    const harness = createWriterHarness()
    const first = transferOutput(harness.writer(), harness)
    const member = file('root/a.txt', 6n)
    await first.beginTraversal(ROOT, SIGNAL)
    await first.visit(1n, member, SIGNAL)
    const transaction = await first.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    const paused = await first.pause()

    const resumed = transferOutput(harness.writer(paused.checkpoint), harness)
    await resumed.beginTraversal(ROOT, SIGNAL)
    expect(await resumed.visit(1n, member, SIGNAL)).toBe('transfer-file')
    const resumedTransaction = await resumed.beginFile(member, source(), SIGNAL)
    expect(resumedTransaction.resumeOffset).toBe(3n)
    await resumedTransaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
    await resumedTransaction.commit(SIGNAL)
    await resumed.finishTraversal(2n, SIGNAL)
    expect(resumed.materializationSummary().rawBytes).toBe(6n)
  })

  it('delegates revision change to member-only rollback authority', async () => {
    const harness = createWriterHarness()
    const member = file('root/a.txt', 6n)
    const first = transferOutput(harness.writer(), harness)
    await first.beginTraversal(ROOT, SIGNAL)
    await first.visit(1n, member, SIGNAL)
    const transaction = await first.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    const paused = await first.pause()
    const rollback = vi.fn(async ({ decision }: Parameters<DirectZipMemberRollbackAuthorityV1['rollbackMember']>[0]) => {
      expect(decision.reason).toBe('revision-changed')
      const active = paused.checkpoint.member!
      await harness.pages.restore(active.rollback.pages)
      harness.target.visible = harness.target.visible.slice(0, Number(active.rollback.archiveOffset))
      const checkpointBase = { ...paused.checkpoint }
      delete checkpointBase.member
      const checkpoint: DirectZipWriterCheckpointV1 = Object.freeze({
        ...checkpointBase,
        generation: paused.checkpoint.generation + 1n,
        phase: 'between-members',
        nextEntryOrdinal: active.rollback.nextEntryOrdinal,
        archiveOffset: active.rollback.archiveOffset,
        committedLength: active.rollback.archiveOffset,
        safeResumeBytes: active.rollback.safeResumeBytes,
        targetObservationDigest: observationDigest(harness.target.visible),
        epochRoot: active.rollback.epochRoot,
        pages: active.rollback.pages,
      })
      return harness.writer(checkpoint)
    })
    const resumed = transferOutput(harness.writer(paused.checkpoint), harness, rollback)
    await resumed.beginTraversal(ROOT, SIGNAL)
    await resumed.visit(1n, member, SIGNAL)
    const replacement = await resumed.beginFile(member, source('revision-2'), SIGNAL)
    expect(replacement.resumeOffset).toBe(0n)
    expect(rollback).toHaveBeenCalledOnce()
  })

  it('observes an after-publication close fault without retrying or duplicating output', async () => {
    const harness = createWriterHarness()
    harness.target.closeFaults.push('after-publish')
    const output = transferOutput(harness.writer(), harness)
    const member = file('root/a.txt', 0n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source('revision-1', 0n), SIGNAL)
    await transaction.commit(SIGNAL)
    await output.finishTraversal(2n, SIGNAL)
    const proof = await output.publish()
    expect(proof.checkpoint.phase).toBe('closing')
    expect(harness.target.artifactCount).toBe(1)
    expect(harness.target.closeAttemptCount).toBe(2)
  })

  it('surfaces ambiguous candidate ownership without truncating or creating another artifact', async () => {
    const harness = createWriterHarness()
    const output = transferOutput(harness.writer(), harness)
    const member = file('root/a.txt', 0n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source('revision-1', 0n), SIGNAL)
    await transaction.commit(SIGNAL)
    await output.finishTraversal(2n, SIGNAL)
    harness.target.observationOverrides.push(candidateObservation({
      ownership: 'ambiguous',
      length: 'other',
      observationMatch: 'neither',
    }))

    await expect(output.publish()).rejects.toMatchObject({ gate: 'target-verification-required' })
    expect(harness.target.truncateCount).toBe(0)
    expect(harness.target.artifactCount).toBe(1)
  })

  it('replays closing from its durable start after close-before-publication', async () => {
    const harness = createWriterHarness()
    const first = transferOutput(harness.writer(), harness)
    await first.beginTraversal(ROOT, SIGNAL)
    await first.finishTraversal(1n, SIGNAL)
    harness.target.closeFaults.push('before-publish')
    await expect(first.publish()).rejects.toThrow(/closing epoch did not publish/u)
    const closing = harness.cuts.closing.at(-1)
    expect(closing?.phase).toBe('closing')

    const resumed = transferOutput(harness.writer(closing), harness)
    await resumed.beginTraversal(ROOT, SIGNAL)
    await resumed.finishTraversal(1n, SIGNAL)
    const proof = await resumed.publish()
    expect(proof.checkpoint.phase).toBe('closing')
    expect(harness.target.artifactCount).toBe(1)
  })

  it('keeps runtime support default-off when evidence is absent', async () => {
    await expect(createDirectZipExecutionV1({} as never)).rejects.toBeInstanceOf(
      DirectZipRuntimeUnsupportedError,
    )
  })
})

describe('direct ZIP revision capacity composition', () => {
  it('stops charging capacity wait before Direct ZIP consumes the recovered range', async () => {
    const member = file('root/a.txt', 2n)
    const clock = new DirectZipCapacityClock()
    let reads = 0
    let recoveredSnapshot: V2RevisionCapacityWaitSnapshot | undefined
    let afterConsumerTime: V2RevisionCapacityWaitSnapshot | undefined
    const opened = directZipOpenedRevision(member)
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: { open: async () => opened },
      broker: {
        readRange: async function* (_descriptor, _lease, range) {
          reads += 1
          if (reads === 1) throw directZipCapacityError(10)
          yield { offset: range.start, data: Uint8Array.of(1) }
          recoveredSnapshot = coordinator.snapshot()
          clock.advance(25)
          afterConsumerTime = coordinator.snapshot()
          yield { offset: range.start + 1n, data: Uint8Array.of(2) }
        },
      },
    }, {
      clock,
      generation: {
        waitForProtocolSessionReplacement: (_identity, signal) =>
          new Promise((_resolve, reject) => {
            const abort = () => reject(signal.reason)
            signal.addEventListener('abort', abort, { once: true })
            if (signal.aborted) abort()
          }),
      },
      waitBudgetMilliseconds: 50,
      additiveJitterLimitMilliseconds: 0,
      visibilityThresholdMilliseconds: 0,
      random: () => 0,
      randomBytes: length => id(9).slice(0, length),
    })

    await transferDirectZipFileV1({
      descriptor: {
        shareInstance: opened.descriptor.shareInstance,
        chunkSize: 2,
      } as never,
      revisions: coordinator.revisions,
      broker: coordinator.broker,
      output: directZipContentOutput(),
      signal: SIGNAL,
      onWriteAcknowledged: () => undefined,
      onComplete: () => undefined,
    }, member)

    expect(reads).toBe(2)
    expect(recoveredSnapshot).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 10,
      attempts: 1,
    })
    expect(afterConsumerTime).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 10,
      attempts: 1,
    })
  })
})

function directZipOpenedRevision(member: DirectZipOrderedFileV1): V2OpenedRevision {
  const shareInstance = id(6)
  const fileRevision = id(7)
  return Object.freeze({
    descriptor: Object.freeze({
      shareInstance,
      shareInstanceId: base64(shareInstance),
      fileId: member.pending.entry.id,
      fileIdText: member.pending.entry.idText,
      fileRevision,
      fileRevisionText: base64(fileRevision),
      exactSize: member.expectedSize,
      geometry: new FileGeometry(member.expectedSize, 2n),
    }),
    leaseId: id(8),
    release: async () => undefined,
  })
}

function directZipContentOutput(): DirectZipOutputSessionV1 {
  return {
    identity: { backend: 'direct-zip-capacity-test', outputSessionId: 'capacity-session' },
    capabilities: {
      durability: 'None',
      randomWrite: false,
      fileFailureIsolation: false,
      modificationTime: false,
    },
    beginFile: async () => ({
      resumeOffset: 0n,
      write: async () => undefined,
      observeCheckpoint: async () => 0n,
      commit: async () => undefined,
    }),
  }
}

function directZipCapacityError(retryAfterMilliseconds: number): V2RevisionCapacityBusyError {
  const failure = Object.freeze({
    code: V2_REVISION_CODE_QUOTA,
    retryable: true as const,
    retryAfterMilliseconds,
  })
  return new V2RevisionCapacityBusyError(failure, createProtocolFailure({
    requestKind: 'open_revisions',
    wireScope: 'revision',
    wireCode: failure.code,
    retryable: true,
    retryAfterMilliseconds,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', id(10)),
      protocolOperationId: createFailureIdentity('protocol_operation', id(11)),
    },
  }))
}

class DirectZipCapacityClock implements V2RevisionCapacityClock {
  #now = 0

  now(): number { return this.#now }

  async sleep(milliseconds: number, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#now += milliseconds
  }

  advance(milliseconds: number): void {
    this.#now += milliseconds
  }
}

function transferOutput(
  writer: ReturnType<ReturnType<typeof createWriterHarness>['writer']>,
  harness: ReturnType<typeof createWriterHarness>,
  rollback: DirectZipMemberRollbackAuthorityV1['rollbackMember'] = defaultRollback().rollbackMember,
): DirectZipTransferOutputV1 {
  const replay: DirectZipReplayAuthorityV1 = {
    verifyRoot: async () => undefined,
    verifyMember: async () => undefined,
  }
  return new DirectZipTransferOutputV1({
    identity: { backend: 'direct-zip-test', outputSessionId: 'session-1' },
    capabilities: {
      durability: 'ProcessRestart',
      randomWrite: false,
      fileFailureIsolation: false,
      modificationTime: true,
    },
    writer,
    pages: harness.pages,
    replay,
    rollback: { rollbackMember: rollback },
  })
}

function defaultRollback() {
  return {
    rollbackMember: async (): Promise<never> => {
      throw new Error('unexpected member rollback')
    },
  }
}

function source(revision = 'revision-1', exactSize = 6n) {
  return Object.freeze({
    fileId: 'file-1',
    revision,
    exactSize,
    rangeAuthority: 'range-authority-1',
  })
}

function directory(path: string): DirectZipOrderedMemberV1 {
  const artifactPath = Object.freeze(path.split('/'))
  return Object.freeze({
    kind: 'directory',
    directoryId: `directory-${path}`,
    generation: `generation-${path}`,
    sourcePath: artifactPath,
    artifactPath,
    layoutEvidence: new TextEncoder().encode(`layout:${path}`),
    discoveryEvidence: new TextEncoder().encode(`discovery:${path}`),
  })
}

function file(path: string, expectedSize: bigint): DirectZipOrderedFileV1 {
  const artifactPath = Object.freeze(path.split('/'))
  return Object.freeze({
    kind: 'file',
    fileId: 'file-1',
    expectedSize,
    sourcePath: artifactPath,
    artifactPath,
    layoutEvidence: new TextEncoder().encode(`layout:${path}`),
    discoveryEvidence: new TextEncoder().encode(`discovery:${path}`),
    pending: Object.freeze({
      entry: Object.freeze({
        kind: 'file',
        id: new Uint8Array(16).fill(1),
        idText: 'file-1',
        name: artifactPath.at(-1)!,
        expectedSize,
      }),
      sourcePath: artifactPath,
      artifactPath,
      parent: Object.freeze({
        directoryId: 'parent-1',
        generation: 'generation-1',
        sourcePath: artifactPath.slice(0, -1),
        artifactPath: artifactPath.slice(0, -1),
      }),
      ready: Promise.resolve(),
    }),
  })
}

function id(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

function base64(value: Uint8Array): string {
  return encodeBase64Url(value)
}

function committed(directoryId: Uint8Array<ArrayBuffer>, generationSeed: number, entryCount: number) {
  const generation = id(generationSeed)
  return Object.freeze({
    directoryId,
    directoryIdText: base64(directoryId),
    generation,
    generationText: base64(generation),
    pageCount: 1,
    entryCount,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
}

function catalogDirectory(identity: Uint8Array<ArrayBuffer>, name: string): V2CatalogEntry {
  return Object.freeze({ kind: 'directory' as const, id: identity, idText: base64(identity), name })
}

function catalogFile(
  identity: Uint8Array<ArrayBuffer>,
  name: string,
  expectedSize: bigint,
): V2CatalogEntry {
  return Object.freeze({ kind: 'file' as const, id: identity, idText: base64(identity), name, expectedSize })
}
