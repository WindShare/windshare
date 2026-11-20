import { describe, expect, it } from 'vitest'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { SelectionPolicy } from '../../../src/catalog/selection'
import { RevisionReopenedDuringRecoveryError } from '../../../src/content/session-services'
import { createProgressiveWorkspaceExecution } from '../../../src/output/execution/progressive-workspace-execution'
import type { NativeObjectIO } from '../../../src/output/origin-private/native-object/contracts'
import { ObjectCheckpointCoordinator } from '../../../src/output/origin-private/native-object/coordinator'
import type { TaskCheckpoint, TaskDirectoryPin, TaskEntry } from '../../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/store'
import { ProgressiveZipArchive, type ZipObjectCapacity } from '../../../src/output/progressive-zip/archive'
import { projectSourceRevisionFailures } from '../../../src/output/resume/source-revision-failures'
import { BoundaryFaultError, FaultScope, SourceFaultCode, sourceFault } from '../../../src/transfer/failure/fault'
import {
  catalogFixture, digestIdentity, fileEntry, identity, identityText, planAuthorityFixture,
  readerFixture, receiveIntentFixture, transferJobFixture,
} from '../../transfer/v2-job-fixture'

class InMemoryIO implements NativeObjectIO {
  data = new Uint8Array(0)
  async writeAt(offset: bigint, value: Uint8Array) {
    const end = Number(offset) + value.byteLength
    if (end > this.data.byteLength) {
      const expanded = new Uint8Array(end)
      expanded.set(this.data)
      this.data = expanded
    }
    this.data.set(value, Number(offset))
  }
  async truncate(length: bigint) { this.data = this.data.slice(0, Number(length)) }
  async size() { return BigInt(this.data.length) }
  async flush() {}
  async close() {}
}

class InMemoryStore implements TaskCheckpointStore {
  checkpoint: TaskCheckpoint | undefined
  entries = new Map<string, TaskEntry>()
  pins = new Map<string, TaskDirectoryPin>()
  async readCheckpoint() { return this.checkpoint }
  async readEntry(id: string) { return this.entries.get(id) }
  async readDirectoryPin(id: string) { return this.pins.get(id) }
  async readPath(path: readonly string[]) { return [...this.entries.values()].find(e => e.path.join('/') === path.join('/')) }
  async readEntries(input: { afterSequence?: bigint; limit: number }) {
    return [...this.entries.values()]
      .filter(entry => input.afterSequence === undefined || entry.zipLayout!.sequence > input.afterSequence)
      .sort((left, right) => Number(left.zipLayout!.sequence - right.zipLayout!.sequence))
      .slice(0, input.limit)
  }
  async commit(input: Parameters<TaskCheckpointStore['commit']>[0]) {
    this.checkpoint = input.checkpoint
    for (const entry of input.entries) this.entries.set(entry.entryId, entry)
    for (const pin of input.directoryPins ?? []) this.pins.set(pin.directoryId, pin)
  }
  close() {}
}

function createArchiveFixture(operationId: string) {
  const store = new InMemoryStore()
  const io = new InMemoryIO()
  const object = { operationId, objectId: 'archive', kind: 'zip-archive' as const, handleId: 'handle' }
  const capacity: ZipObjectCapacity = {
    reserveGrowth: async () => ({ settle: async () => undefined, release: async () => undefined }),
  }
  const open = () => ProgressiveZipArchive.open({
    object, store, selectedPaths: [], capacity,
    coordinator: new ObjectCheckpointCoordinator({ io, ...object }),
  })
  return { store, io, open }
}

function createWorkspaceExecution(archive: ProgressiveZipArchive, intent: Awaited<ReturnType<typeof receiveIntentFixture>>) {
  return createProgressiveWorkspaceExecution({
    archive, intent, outputIdentity: { backend: 'native-zip', outputSessionId: 'failure-semantics-session' },
    settlement: {
      pause: async (request, { checkpoint }) => ({
        operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n,
        kind: 'resumable-receive', payloadKind: 'workspace-archive',
        objectId: checkpoint.object.objectId, checkpointGeneration: checkpoint.generation,
        occupiedBytes: checkpoint.physicalLength, discoveryComplete: checkpoint.discoveryComplete,
        completedFileCount: request.materialization.fileCount, completedBytes: request.materialization.rawBytes,
        ...(checkpoint.contentWarning === undefined ? {} : { contentWarning: checkpoint.contentWarning }),
      }),
      settle: async () => { throw new Error('Incomplete ZIP must pause instead of sealing') },
    },
  })
}

describe('progressive ZIP failure semantics', () => {
  it('keeps retry-limit readmissions untainted and reports file-failed instead of source-unavailable', async () => {
    const stalled = fileEntry(identity(21), 'stalled.bin', 6n)
    const healthy = fileEntry(identity(22), 'healthy.bin', 2n)
    const selection = new SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = createArchiveFixture(intent.operationId)
    const archive = await f.open()
    const execution = await createWorkspaceExecution(archive, intent)
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const readers = readerFixture([stalled, healthy])
    const versions = [71, 72, 73, 74].map(seed => ({
      fileRevision: identity(seed), fileRevisionText: identityText(seed), sourceMetadata: digestIdentity(75),
    }))
    let stalledOpens = 0
    const result = await transferJobFixture({
      catalog: catalogFixture([{ id: identity(2), entries: [stalled, healthy] }]).catalog,
      selection, intent, plans, maximumConcurrentFiles: 1,
      revisions: {
        open: async (id, signal) => {
          const opened = await readers.revisions.open(id, signal)
          if (opened.descriptor.fileIdText !== stalled.idText) return opened
          const attempt = stalledOpens++
          return { ...opened, descriptor: { ...opened.descriptor, ...versions[attempt]! }, leaseId: identity(81 + attempt) }
        },
      },
      broker: {
        readRange: async function* (descriptor, lease, range, request) {
          if (descriptor.fileIdText !== stalled.idText) {
            yield* readers.broker.readRange(descriptor, lease, range, request)
            return
          }
          if (range.start === 0n) yield { offset: 0n, data: Uint8Array.of(1, 2) }
          throw new RevisionReopenedDuringRecoveryError(descriptor, { ...descriptor, ...versions[stalledOpens]! })
        },
      },
    }).run()

    expect(stalledOpens).toBe(3)
    expect(result.worker.status).toBe('Paused')
    expect(archive.state.contentWarning).toEqual({
      kind: 'partial-zip', completedFileCount: 1n, selectedFileCount: 2n,
      missingFiles: [{ path: ['windshare', stalled.name], reason: 'file-failed' }],
    })
    const retained = await f.store.readEntry(`file:${stalled.idText}`)
    expect(retained?.revisionFailure).toBeUndefined()
    expect(retained?.ranges).toMatchObject([{ start: 0n, end: 2n }])
    expect(await projectSourceRevisionFailures(f.store, intent.shareInstance)).toBeUndefined()
  })

  it('keeps mid-transfer evidence-unavailable reopens untainted and reports file-failed', async () => {
    const unverified = fileEntry(identity(23), 'unverified.bin', 6n)
    const healthy = fileEntry(identity(24), 'healthy.bin', 2n)
    const selection = new SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = createArchiveFixture(intent.operationId)
    const archive = await f.open()
    const execution = await createWorkspaceExecution(archive, intent)
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const readers = readerFixture([unverified, healthy])
    const result = await transferJobFixture({
      catalog: catalogFixture([{ id: identity(2), entries: [unverified, healthy] }]).catalog,
      selection, intent, plans, maximumConcurrentFiles: 1,
      revisions: {
        open: async (id, signal) => {
          const opened = await readers.revisions.open(id, signal)
          return opened.descriptor.fileIdText === unverified.idText
            ? { ...opened, descriptor: { ...opened.descriptor, sourceMetadata: digestIdentity(76) } }
            : opened
        },
      },
      broker: {
        readRange: async function* (descriptor, lease, range, request) {
          if (descriptor.fileIdText !== unverified.idText) {
            yield* readers.broker.readRange(descriptor, lease, range, request)
            return
          }
          yield { offset: 0n, data: Uint8Array.of(3, 4) }
          const withoutMetadata = { ...descriptor }
          Reflect.deleteProperty(withoutMetadata, 'sourceMetadata')
          throw new RevisionReopenedDuringRecoveryError(descriptor, {
            ...withoutMetadata, fileRevision: identity(77), fileRevisionText: identityText(77),
          })
        },
      },
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(archive.state.contentWarning).toEqual({
      kind: 'partial-zip', completedFileCount: 1n, selectedFileCount: 2n,
      missingFiles: [{ path: ['windshare', unverified.name], reason: 'file-failed' }],
    })
    const retained = await f.store.readEntry(`file:${unverified.idText}`)
    expect(retained?.revisionFailure).toBeUndefined()
    expect(retained?.ranges).toMatchObject([{ start: 0n, end: 2n }])
    expect(await projectSourceRevisionFailures(f.store, intent.shareInstance)).toBeUndefined()
  })

  it.each([
    { code: SourceFaultCode.Unavailable, expectedReason: 'file-failed', expectedRevisionFailure: undefined },
    { code: SourceFaultCode.Permanent, expectedReason: 'source-unavailable', expectedRevisionFailure: SourceFaultCode.Permanent },
  ] as const)('distinguishes $code on allocated resume open failures', async testCase => {
    const root = identity(2)
    const generation = identity(3)
    const target = fileEntry(identity(25), 'target.bin', 4n)
    const healthy = fileEntry(identity(26), 'healthy.bin', 2n)
    const selection = new SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = createArchiveFixture(intent.operationId)
    const first = await f.open()
    const parent = { shareInstance: intent.shareInstance, directoryId: encodeBase64Url(root), generation: encodeBase64Url(generation) }
    await first.admitDirectory({ entryId: `directory:${parent.directoryId}`, path: ['windshare'], source: { ...parent, sourcePath: [] } })
    const targetEntryId = `file:${target.idText}`
    await first.admitFile({
      entryId: targetEntryId, path: ['windshare', target.name],
      source: { ...parent, sourcePath: [target.name] },
      revision: { fileId: target.idText, fileRevision: identityText(78), exactSize: target.expectedSize },
    })
    await first.writeRange(targetEntryId, 0n, Uint8Array.of(9, 8))
    await first.checkpoint('initial-partial')
    await first.close()

    const resumed = await f.open()
    const execution = await createWorkspaceExecution(resumed, intent)
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const readers = readerFixture([target, healthy], [], {
      beforeOpen: async id => {
        if (id === target.idText) throw new BoundaryFaultError(sourceFault(FaultScope.FileLocal, testCase.code))
      },
    })
    await transferJobFixture({
      catalog: catalogFixture([{ id: root, generation, entries: [target, healthy] }]).catalog,
      selection, intent, plans, maximumConcurrentFiles: 1,
      revisions: readers.revisions, broker: readers.broker,
    }).run()

    expect(resumed.state.contentWarning).toEqual({
      kind: 'partial-zip', completedFileCount: 1n, selectedFileCount: 2n,
      missingFiles: [{ path: ['windshare', target.name], reason: testCase.expectedReason }],
    })
    expect((await f.store.readEntry(targetEntryId))?.revisionFailure).toBe(testCase.expectedRevisionFailure)
    const projected = await projectSourceRevisionFailures(f.store, intent.shareInstance)
    if (testCase.expectedRevisionFailure === undefined) {
      expect(projected).toBeUndefined()
    } else {
      expect(projected?.count).toBe(1n)
      expect(projected?.files[0]?.entryId).toBe(targetEntryId)
    }
  })
})
