import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { FileGeometry } from '../../src/content/geometry'
import { V2BlockLaneAttemptsError } from '../../src/content/v2-broker'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import {
  V2SupervisedContent,
  type V2ContentGeneration,
  type V2ContentGenerationProvider,
} from '../../src/receiver/v2-supervised-content'
import { TransferPauseRequestedError } from '../../src/transfer/output-session'
import { V2TransferProgressLedger } from '../../src/transfer/progress/v2-ledger'
import { createDirectZipProgressObservers, runDirectZipJob } from '../../src/transfer/v2-job-direct-zip'
import {
  V2RevisionCapacityBusyError,
  type V2OpenedRevision,
} from '../../src/content/v2-session-services'
import { V2_REVISION_CODE_QUOTA } from '../../src/content/v2-flow'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'
import {
  createFailureIdentity,
  createReceivedProtocolError,
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
  type DirectZipPayloadProgressV1,
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

})

describe('direct ZIP discovery pipeline', () => {
  it('publishes an exact ZIP total while the first serial file transfer is blocked', async () => {
    const discovered = coordinatorGate()
    const transferStarted = coordinatorGate()
    const releaseTransfer = coordinatorGate()
    const members = [file('root/a.txt', 2n), file('root/b.txt', 3n)]
    const transferred: string[] = []
    const observed: bigint[] = []
    const running = new DirectZipOrderedCoordinatorV1({
      source: { root: async () => ROOT, members: async function* () { yield* members } },
      output: coordinatorOutput(),
      signal: SIGNAL,
      observeSelectedFile: size => observed.push(size),
      observeReplayedFile: () => undefined,
      transferFile: async member => {
        transferStarted.resolve()
        await releaseTransfer.promise
        transferred.push(member.artifactPath.join('/'))
      },
      finishMeasure: () => {
        discovered.resolve()
        return { discoveredFiles: 2, discoveredBytes: 5n, discovery: 'complete', sizeClass: 'small' }
      },
    }).run()
    await Promise.all([discovered.promise, transferStarted.promise])
    expect(observed).toEqual([2n, 3n])
    expect(transferred).toEqual([])
    releaseTransfer.resolve()
    expect((await running).discovery).toBe('complete')
    expect(transferred).toEqual(['root/a.txt', 'root/b.txt'])
  })

  it('bounds ZIP discovery ahead of a blocked writer without changing member order', async () => {
    const reachedBound = coordinatorGate()
    const releaseTransfer = coordinatorGate()
    let observed = 0
    let finished = false
    const members = Array.from({ length: 300 }, (_, index) => file(`root/${index.toString().padStart(3, '0')}.txt`, 1n))
    const running = new DirectZipOrderedCoordinatorV1({
      source: { root: async () => ROOT, members: async function* () { yield* members } },
      output: coordinatorOutput(),
      signal: SIGNAL,
      observeSelectedFile: () => { if (++observed === 258) reachedBound.resolve() },
      observeReplayedFile: () => undefined,
      transferFile: async () => { await releaseTransfer.promise },
      finishMeasure: () => {
        finished = true
        return { discoveredFiles: 300, discoveredBytes: 300n, discovery: 'complete', sizeClass: 'large' }
      },
    }).run()
    await reachedBound.promise
    // One active member, 256 queued members, and one producer waiting for admission.
    expect(observed).toBe(258)
    expect(finished).toBe(false)
    releaseTransfer.resolve()
    expect((await running).discoveredFiles).toBe(300)
  })

  it('cancels and drains the active writer when catalog discovery fails', async () => {
    const transferStarted = coordinatorGate()
    const cancelled = coordinatorGate()
    const releaseCleanup = coordinatorGate()
    const failure = new Error('catalog discovery failed')
    let settled = false
    const running = new DirectZipOrderedCoordinatorV1({
      source: {
        root: async () => ROOT,
        members: async function* () {
          yield file('root/a.txt', 1n)
          await transferStarted.promise
          throw failure
        },
      },
      output: coordinatorOutput(),
      signal: SIGNAL,
      observeSelectedFile: () => undefined,
      observeReplayedFile: () => undefined,
      transferFile: async (_member, signal) => {
        const aborted = new Promise<void>(resolve => {
          signal.addEventListener('abort', () => { cancelled.resolve(); resolve() }, { once: true })
        })
        transferStarted.resolve()
        await aborted
        await releaseCleanup.promise
        signal.throwIfAborted()
      },
      finishMeasure: () => { throw new Error('Failed discovery cannot publish an exact total') },
    }).run()
    const observed = running.then(() => { settled = true }, () => { settled = true })
    await cancelled.promise
    expect(settled).toBe(false)
    releaseCleanup.resolve()
    await expect(running).rejects.toBe(failure)
    await observed
  })
})

describe('direct ZIP output settlement', () => {
  it('treats content-block checkpoints as policy observations and creates one complete artifact', async () => {
    const harness = createWriterHarness()
    const progress: DirectZipPayloadProgressV1[] = []
    const output = transferOutput(harness.writer(), harness, undefined, snapshot => progress.push(snapshot))
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    expect(await output.visit(1n, member, SIGNAL)).toBe('transfer-file')
    const transaction = await output.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    expect(await transaction.observeCheckpoint(SIGNAL)).toBe(0n)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 3n })
    expect(harness.target.closeAttemptCount).toBe(0)
    await transaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
    expect(await transaction.observeCheckpoint(SIGNAL)).toBe(0n)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 6n, writtenSelectedBytes: 6n })
    await transaction.commit(SIGNAL)
    await output.finishTraversal(2n, SIGNAL)

    const published = await output.publish()
    expect(published.completion.exactArchiveBytes).toBe(BigInt(harness.target.visible.byteLength))
    expect(published.checkpoint.phase).toBe('closing')
    expect(harness.target.artifactCount).toBe(1)
    expect(harness.target.rangeReads).toEqual([])
    expect(harness.target.closeAttemptCount).toBe(1)
    expect(progress).toEqual([
      { receivedSelectedBytes: 0n, writtenSelectedBytes: 0n },
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 0n },
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 3n },
      { receivedSelectedBytes: 6n, writtenSelectedBytes: 3n },
      { receivedSelectedBytes: 6n, writtenSelectedBytes: 6n },
    ])
    expect(output.materializationSummary()).toEqual({
      entryCount: 2n,
      fileCount: 1n,
      directoryCount: 1n,
      rawBytes: 6n,
    })
  })

  it('reports authenticated receipt while the payload write is still pending', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const progress: DirectZipPayloadProgressV1[] = []
    const output = transferOutput(writer, harness, undefined, snapshot => progress.push(snapshot))
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source(), SIGNAL)
    const started = coordinatorGate()
    const release = coordinatorGate()
    const writeMember = writer.writeMember.bind(writer)
    vi.spyOn(writer, 'writeMember').mockImplementationOnce(async (...args) => {
      started.resolve()
      await release.promise
      await writeMember(...args)
    })

    const writing = transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    await started.promise
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 0n })
    expect(writer.committedCheckpoint.safeResumeBytes).toBe(0n)
    await expect(transaction.write(0n, Uint8Array.of(1, 2, 3, 4), SIGNAL))
      .rejects.toThrow(/contiguous range/u)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 0n })
    release.resolve()
    await writing
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 3n })
    expect(progress.every(Object.isFrozen)).toBe(true)
    expect(writer.committedCheckpoint.safeResumeBytes).toBe(0n)
    await output.pause()
  })

  it('counts a retried payload range once and never acknowledges a rejected write', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const progress: DirectZipPayloadProgressV1[] = []
    const output = transferOutput(writer, harness, undefined, snapshot => progress.push(snapshot))
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source(), SIGNAL)
    const failure = new Error('payload write rejected')
    vi.spyOn(writer, 'writeMember').mockRejectedValueOnce(failure)
    await expect(transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)).rejects.toBe(failure)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 0n })
    await expect(transaction.write(1n, Uint8Array.of(1), SIGNAL)).rejects.toThrow(/contiguous range/u)

    await transaction.write(0n, Uint8Array.of(1, 2), SIGNAL)
    await transaction.write(2n, Uint8Array.of(3, 4), SIGNAL)
    expect(progress).toEqual([
      { receivedSelectedBytes: 0n, writtenSelectedBytes: 0n },
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 0n },
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 2n },
      { receivedSelectedBytes: 4n, writtenSelectedBytes: 2n },
      { receivedSelectedBytes: 4n, writtenSelectedBytes: 4n },
    ])
    expect(writer.committedCheckpoint.safeResumeBytes).toBe(0n)
    await output.pause()
  })

  it('keeps progress observers outside writer success and failure authority', async () => {
    const harness = createWriterHarness()
    const observe = vi.fn(() => { throw new Error('progress view failed') })
    const output = transferOutput(harness.writer(), harness, undefined, observe)
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3, 4, 5, 6), SIGNAL)
    await transaction.commit(SIGNAL)
    await output.finishTraversal(2n, SIGNAL)
    expect((await output.publish()).checkpoint.safeResumeBytes).toBe(6n)
    expect(observe).toHaveBeenCalledTimes(3)
  })

  it.each(['payload', 'descriptor'] as const)('drops a discarded epoch from progress after %s failure', async fault => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const progress: DirectZipPayloadProgressV1[] = []
    const output = transferOutput(writer, harness, undefined, snapshot => progress.push(snapshot))
    const member = file('root/a.txt', 6n)
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const transaction = await output.beginFile(member, source(), SIGNAL)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    if (fault === 'descriptor') await transaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
    harness.target.failNextWriteAtOrAfter = 0n
    await expect(fault === 'payload'
      ? transaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
      : transaction.commit(SIGNAL)).rejects.toThrow(/injected positioned write failure/u)
    expect(progress.at(-1)).toEqual({
      receivedSelectedBytes: 6n,
      writtenSelectedBytes: fault === 'payload' ? 3n : 6n,
    })
    const paused = await output.pause()
    expect(paused.checkpoint.safeResumeBytes).toBe(0n)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 0n, writtenSelectedBytes: 0n })

    const resumed = transferOutput(harness.writer(paused.checkpoint), harness, undefined,
      snapshot => progress.push(snapshot))
    await resumed.beginTraversal(ROOT, SIGNAL)
    await resumed.visit(1n, member, SIGNAL)
    const retry = await resumed.beginFile(member, source(), SIGNAL)
    expect(retry.resumeOffset).toBe(0n)
    await retry.write(0n, Uint8Array.of(1, 2, 3, 4, 5, 6), SIGNAL)
    await retry.commit(SIGNAL)
    await resumed.finishTraversal(2n, SIGNAL)
    await resumed.publish()
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 6n, writtenSelectedBytes: 6n })
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

    const progress: DirectZipPayloadProgressV1[] = []
    const resumed = transferOutput(harness.writer(paused.checkpoint), harness, undefined,
      snapshot => progress.push(snapshot))
    expect(progress).toEqual([{ receivedSelectedBytes: 3n, writtenSelectedBytes: 3n }])
    await resumed.beginTraversal(ROOT, SIGNAL)
    expect(await resumed.visit(1n, member, SIGNAL)).toBe('transfer-file')
    const resumedTransaction = await resumed.beginFile(member, source(), SIGNAL)
    expect(resumedTransaction.resumeOffset).toBe(3n)
    await resumedTransaction.write(3n, Uint8Array.of(4, 5, 6), SIGNAL)
    await resumedTransaction.commit(SIGNAL)
    await resumed.finishTraversal(2n, SIGNAL)
    expect(resumed.materializationSummary().rawBytes).toBe(6n)
    expect(progress).toEqual([
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 3n },
      { receivedSelectedBytes: 6n, writtenSelectedBytes: 3n },
      { receivedSelectedBytes: 6n, writtenSelectedBytes: 6n },
    ])
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
    const progress: DirectZipPayloadProgressV1[] = []
    const resumed = transferOutput(harness.writer(paused.checkpoint), harness, rollback,
      snapshot => progress.push(snapshot))
    await resumed.beginTraversal(ROOT, SIGNAL)
    await resumed.visit(1n, member, SIGNAL)
    const replacement = await resumed.beginFile(member, source('revision-2'), SIGNAL)
    expect(replacement.resumeOffset).toBe(0n)
    expect(rollback).toHaveBeenCalledOnce()
    expect(progress).toEqual([
      { receivedSelectedBytes: 3n, writtenSelectedBytes: 3n },
      { receivedSelectedBytes: 0n, writtenSelectedBytes: 0n },
    ])
    await replacement.write(0n, Uint8Array.of(1, 2, 3), SIGNAL)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 3n, writtenSelectedBytes: 3n })
    await resumed.pause()
  })

  it('seeds completed and partial payload once while replaying directories and files', async () => {
    const harness = createWriterHarness()
    const first = transferOutput(harness.writer(), harness)
    const completed = file('root/a.txt', 2n)
    const folder = directory('root/b')
    const partial = file('root/b/c.txt', 6n)
    await first.beginTraversal(ROOT, SIGNAL)
    await first.visit(1n, completed, SIGNAL)
    const completedTransaction = await first.beginFile(completed, source('revision-1', 2n), SIGNAL)
    await completedTransaction.write(0n, Uint8Array.of(1, 2), SIGNAL)
    await completedTransaction.commit(SIGNAL)
    await first.visit(2n, folder, SIGNAL)
    await first.visit(3n, partial, SIGNAL)
    const partialTransaction = await first.beginFile(partial, source(), SIGNAL)
    await partialTransaction.write(0n, Uint8Array.of(3, 4, 5), SIGNAL)
    const paused = await first.pause()
    expect(paused.checkpoint.safeResumeBytes).toBe(5n)

    const progress: DirectZipPayloadProgressV1[] = []
    const resumed = transferOutput(harness.writer(paused.checkpoint), harness, undefined,
      snapshot => progress.push(snapshot))
    await resumed.beginTraversal(ROOT, SIGNAL)
    expect(await resumed.visit(1n, completed, SIGNAL)).toBe('replayed')
    expect(await resumed.visit(2n, folder, SIGNAL)).toBe('replayed')
    await resumed.visit(3n, partial, SIGNAL)
    const transaction = await resumed.beginFile(partial, source(), SIGNAL)
    expect(progress).toEqual([{ receivedSelectedBytes: 5n, writtenSelectedBytes: 5n }])
    await transaction.write(3n, Uint8Array.of(6, 7, 8), SIGNAL)
    await transaction.commit(SIGNAL)
    await resumed.finishTraversal(4n, SIGNAL)
    await resumed.publish()
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 8n, writtenSelectedBytes: 8n })
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
    expect(harness.target.closeAttemptCount).toBe(1)
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

  it('replays finalization from its durable predecessor after close-before-publication', async () => {
    const harness = createWriterHarness()
    const first = transferOutput(harness.writer(), harness)
    await first.beginTraversal(ROOT, SIGNAL)
    await first.finishTraversal(1n, SIGNAL)
    harness.target.closeFaults.push('before-publish')
    await expect(first.publish()).rejects.toThrow(/closing epoch did not publish/u)
    expect(harness.cuts.retired.at(-1)?.kind).toBe('closing')
    expect(harness.cuts.promoted).toHaveLength(0)

    const resumed = transferOutput(harness.writer(), harness)
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

describe('direct ZIP receive lifecycle', () => {
  it('keeps one writer alive while a supervised network generation recovers', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const progress: DirectZipPayloadProgressV1[] = []
    const output = transferOutput(writer, harness, undefined, snapshot => progress.push(snapshot))
    const member = contentFile('root/a.txt', 4n)
    const opened = directZipOpenedRevision(member)
    const recovering = coordinatorGate()
    const recovered = coordinatorGate()
    const loss = new V2BlockLaneAttemptsError([new Error('network generation lost')])
    const generation = (generationId: number): V2ContentGeneration => ({
      id: generationId,
      revisions: { open: async () => opened } as never,
      broker: {
        readRouteAuthorizedRange: async function* (_descriptor, _lease, range) {
          if (generationId === 1 && range.start === 2n) throw loss
          yield { offset: range.start, data: Uint8Array.of(1, 2) }
        },
      },
      lanes: { size: 1 } as never,
    })
    const first = generation(1)
    const second = generation(2)
    let current = first
    const provider: V2ContentGenerationProvider = {
      execute: async (_signal, operation) => ({ generation: current, value: await operation(current) }),
      recover: async (_generation, error) => {
        expect(error).toBe(loss)
        recovering.resolve()
        await recovered.promise
        current = second
        return true
      },
      isCurrent: generation => generation === current,
      contentLaneCount: () => 1,
    }
    const content = new V2SupervisedContent(provider, length => new Uint8Array(length).fill(9))
    const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
    const acknowledged: bigint[] = []
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const running = transferDirectZipFileV1({
      descriptor: { shareInstance: opened.descriptor.shareInstance, chunkSize: 2 } as never,
      revisions: scoped.revisions,
      broker: scoped.broker,
      output,
      signal: SIGNAL,
      onInitialDurable: () => undefined,
      onWriteAcknowledged: bytes => { acknowledged.push(bytes) },
      onComplete: () => undefined,
    }, member)

    await recovering.promise
    expect(acknowledged).toEqual([2n])
    expect(writer.committedCheckpoint.safeResumeBytes).toBe(0n)
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 2n, writtenSelectedBytes: 2n })
    expect(harness.target.openEpochCount).toBe(1)
    expect(harness.target.closeAttemptCount).toBe(0)
    expect(harness.target.abortCount).toBe(0)
    recovered.resolve()
    await running
    await output.finishTraversal(2n, SIGNAL)
    const completed = await output.publish()
    expect(completed.checkpoint.safeResumeBytes).toBe(4n)
    expect(harness.target.openEpochCount).toBe(1)
    expect(harness.target.closeAttemptCount).toBe(1)
    expect(acknowledged).toEqual([2n, 2n])
    expect(progress.at(-1)).toEqual({ receivedSelectedBytes: 4n, writtenSelectedBytes: 4n })
    content.close()
  })

  it('checkpoints explicit Pause and counts resumed plus live bytes without doubling completed progress', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const output = transferOutput(writer, harness)
    const member = contentFile('root/a.txt', 4n)
    const opened = directZipOpenedRevision(member)
    const waiting = coordinatorGate()
    const controller = new AbortController()
    await output.beginTraversal(ROOT, SIGNAL)
    await output.visit(1n, member, SIGNAL)
    const running = transferDirectZipFileV1({
      descriptor: { shareInstance: opened.descriptor.shareInstance, chunkSize: 2 } as never,
      revisions: { open: async () => opened },
      broker: {
        readRange: async function* (_descriptor, _lease, range, options) {
          if (range.start === 2n) {
            waiting.resolve()
            await new Promise((_resolve, reject) => {
              options!.signal!.addEventListener('abort',
                () => reject(options!.signal!.reason), { once: true })
            })
          }
          yield { offset: range.start, data: Uint8Array.of(1, 2) }
        },
      },
      output,
      signal: controller.signal,
      onInitialDurable: () => undefined,
      onWriteAcknowledged: () => undefined,
      onComplete: () => undefined,
    }, member)
    const pause = new TransferPauseRequestedError()
    const rejected = expect(running).rejects.toBe(pause)
    await waiting.promise
    expect(harness.target.closeAttemptCount).toBe(0)
    controller.abort(pause)
    await rejected
    const checkpoint = (await output.pause()).checkpoint
    expect(checkpoint.safeResumeBytes).toBe(2n)
    expect(checkpoint.member?.payloadOffset).toBe(2n)
    expect(harness.target.closeAttemptCount).toBe(1)

    const resumed = transferOutput(harness.writer(checkpoint), harness)
    const progress = new V2TransferProgressLedger()
    const materialized: bigint[] = []
    const rootId = id(20)
    const root = committed(rootId, 21, 1)
    const measure = { discoveredFiles: 1, discoveredBytes: 4n,
      discovery: 'complete' as const, sizeClass: 'small' as const }
    const snapshot = () => { materialized.push(progress.snapshot(measure).materializedBytes) }
    await runDirectZipJob({
      descriptor: { syntheticRoot: rootId, shareInstance: opened.descriptor.shareInstance, chunkSize: 2 } as never,
      catalog: {
        loadDirectory: async () => root,
        entries: async function* () { yield member.pending.entry },
      } as never,
      selection: { canonicalRules: [], selected: () => true } as never,
      intent: { plan: { kind: 'direct-resumable-zip' }, artifact: {
        kind: 'zip-archive', layout: { anchor: { kind: 'synthetic-root' }, name: 'root' },
      } } as never,
      revisions: { open: async () => opened },
      broker: { readRange: async function* (_descriptor, _lease, range) {
        expect(range.start).toBe(2n)
        expect(progress.snapshot(measure).materializedBytes).toBe(2n)
        yield { offset: range.start, data: Uint8Array.of(3, 4) }
      } },
      execution: { output: resumed, ordered: resumed } as never,
      maximumNodeClaims: 10,
      signal: SIGNAL,
      observeSelectedFile: () => undefined,
      ...createDirectZipProgressObservers(progress, snapshot),
      observeDiscovery: () => undefined,
      finishMeasure: () => measure,
    })
    expect([...new Set(materialized)]).toEqual([2n, 4n])
    expect(progress.snapshot(measure)).toMatchObject({
      materializedBytes: 4n, writtenBytes: 2n, completedBytes: 4n, completedFiles: 1,
    })
    expect((await resumed.publish()).checkpoint.safeResumeBytes).toBe(4n)
    expect(harness.target.openEpochCount).toBe(2)
    expect(harness.target.closeAttemptCount).toBe(2)
  })

  it('leaves an EOF-sized checkpoint threshold to final archive publication', async () => {
    const member = contentFile('root/a.txt', 4n)
    const opened = directZipOpenedRevision(member)
    let written = 0n
    const automaticCut = vi.fn(async () => {
      if (written >= member.expectedSize) throw new Error('EOF would close the archive prefix')
      return 0n
    })
    const output = directZipContentOutput()
    await transferDirectZipFileV1({
      descriptor: { shareInstance: opened.descriptor.shareInstance, chunkSize: 2 } as never,
      revisions: { open: async () => opened },
      broker: { readRange: async function* (_descriptor, _lease, range) {
        yield { offset: range.start, data: Uint8Array.of(1, 2) }
      } },
      output: { ...output, beginFile: async () => ({
        resumeOffset: 0n,
        write: async (_offset, bytes) => { written += BigInt(bytes.byteLength) },
        observeCheckpoint: automaticCut,
        commit: async () => undefined,
      }) },
      signal: SIGNAL,
      onInitialDurable: () => undefined,
      onWriteAcknowledged: () => undefined,
      onComplete: () => undefined,
    }, member)
    expect(written).toBe(4n)
    expect(automaticCut).toHaveBeenCalledOnce()
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
      onInitialDurable: () => undefined,
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

function contentFile(path: string, expectedSize: bigint): DirectZipOrderedFileV1 {
  const member = file(path, expectedSize)
  const fileId = base64(member.pending.entry.id)
  return { ...member, fileId, pending: { ...member.pending,
    entry: { ...member.pending.entry, idText: fileId } } }
}

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
  return new V2RevisionCapacityBusyError(createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', id(10)),
      protocolOperationId: createFailureIdentity('protocol_operation', id(11))
    }, content: {
      scope: 'revision', code: failure.code, retryable: true, retryAfterMilliseconds
    }
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
  onProgress?: (snapshot: DirectZipPayloadProgressV1) => void,
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
    ...(onProgress === undefined ? {} : { onProgress }),
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
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(artifactPath),
      logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath),
      materializationRelativePath: snapshotMaterializationRootRelativePath(artifactPath),
      parent: Object.freeze({
        kind: 'reference',
        directoryId: 'parent-1',
        generation: 'generation-1',
        sourceAuthenticationPath: snapshotSourceAuthenticationPath(artifactPath.slice(0, -1)),
        logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath.slice(0, -1)),
      }),
      ready: Promise.resolve(),
    }),
  })
}

function coordinatorGate(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void
  const promise = new Promise<void>(complete => { resolve = complete })
  return { promise, resolve }
}

function coordinatorOutput(): DirectZipOrderedOutputV1 {
  return {
    beginTraversal: async () => undefined,
    visit: async (_ordinal, member) => member.kind === 'file' ? 'transfer-file' : 'admitted',
    finishTraversal: async () => undefined,
    materializationSummary: () => ({ entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n }),
  }
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
