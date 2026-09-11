import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../../src/catalog/v2-selection'
import { FileGeometry } from '../../../src/content/geometry'
import type { V2BlockRangeReader } from '../../../src/content/v2-broker'
import type { V2RevisionReader } from '../../../src/content/v2-session-services'
import { OutputCapacityBlockedError } from '../../../src/transfer/capacity-pressure/drain'
import { createProgressiveWorkspaceExecution } from '../../../src/transfer/settlement/progressive-workspace-execution'
import { ProgressiveZipArchive, type ZipObjectCapacity } from '../../../src/output/progressive-zip/archive'
import { ZipCapacityWindow } from '../../../src/output/progressive-zip/capacity-window'
import { ObjectCheckpointCoordinator } from '../../../src/output/origin-private/native-object/coordinator'
import type { NativeObjectIO } from '../../../src/output/origin-private/native-object/contracts'
import type { TaskCheckpoint, TaskEntry, TaskDirectoryPin } from '../../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/store'
import { catalogFixture, fileEntry, identity, planAuthorityFixture, readerFixture,
  receiveIntentFixture, transferJobFixture } from '../../transfer/v2-job-fixture'

const BLOCK_BYTES = 4 * 1024 * 1024
const BLOCK_SIZE = BigInt(BLOCK_BYTES)

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

class Checkpoints implements TaskCheckpointStore {
  checkpoint: TaskCheckpoint | undefined
  readonly entries = new Map<string, TaskEntry>()
  readonly pins = new Map<string, TaskDirectoryPin>()
  async readCheckpoint() { return this.checkpoint }
  async readEntry(id: string) { return this.entries.get(id) }
  async readPath(path: readonly string[]) {
    return [...this.entries.values()].find(entry => entry.path.join('/') === path.join('/'))
  }
  async readDirectoryPin(id: string) { return this.pins.get(id) }
  async readEntries(input: { afterSequence?: bigint; limit: number }) {
    return [...this.entries.values()].filter(entry => input.afterSequence === undefined ||
      entry.zipLayout!.sequence > input.afterSequence).sort((a, b) =>
      Number(a.zipLayout!.sequence - b.zipLayout!.sequence)).slice(0, input.limit)
  }
  async commit(input: Parameters<TaskCheckpointStore['commit']>[0]) {
    expect(input.expectedGeneration).toBe(this.checkpoint?.generation)
    this.checkpoint = structuredClone(input.checkpoint)
    for (const entry of input.entries) this.entries.set(entry.entryId, structuredClone(entry))
    for (const pin of input.directoryPins ?? []) this.pins.set(pin.directoryId, structuredClone(pin))
  }
  close() {}
}

class NativeIO implements NativeObjectIO {
  length = 0n
  closed = false
  readonly writes: { offset: bigint; length: bigint }[] = []
  async writeAt(offset: bigint, bytes: Uint8Array) {
    const end = offset + BigInt(bytes.byteLength)
    if (end > this.length) this.length = end
    this.writes.push({ offset, length: BigInt(bytes.byteLength) })
  }
  async size() { return this.length }
  async truncate(length: bigint) { this.length = length }
  async flush() {}
  async close() { this.closed = true }
}

describe('native ZIP capacity scheduling', () => {
  it('drains allocated regions through V2Job after two rejected blocks release the entire write budget', async () => {
    const files = [fileEntry(identity(71), 'a.bin', BLOCK_SIZE * 2n),
      fileEntry(identity(72), 'b.bin', BLOCK_SIZE * 2n), fileEntry(identity(73), 'c.bin', BLOCK_SIZE),
      fileEntry(identity(74), 'd-empty.bin', 0n), fileEntry(identity(75), 'e-not-admitted.bin', 0n)]
    const [a, b, c, d, e] = files
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const object = { operationId: intent.operationId, objectId: 'archive', handleId: 'handle', kind: 'zip-archive' as const }
    const io = new NativeIO()
    const store = new Checkpoints()
    const firstA = deferred()
    const thirdActive = deferred()
    const fourthOpening = deferred()
    const pressureStarted = deferred()
    const windowPressured = deferred()
    const bothDeclined = deferred()
    let pressure = false
    let refusals = 0
    let accountedLength = 0n
    const traces: string[] = []
    const capacity: ZipObjectCapacity = {
      reserveGrowth: async request => {
        expect(request.currentLength).toBe(accountedLength)
        if (pressure && (request.targetLength > request.currentLength || request.metadataHeadroom > 0n)) {
          refusals++
          if (refusals === 2) bothDeclined.resolve()
          throw new DOMException('No growth capacity', 'QuotaExceededError')
        }
        return { settle: async length => { accountedLength = length }, release: async () => undefined }
      },
    }
    const archive = await ProgressiveZipArchive.open({ object, store, capacity, selectedPaths: [],
      coordinator: new ObjectCheckpointCoordinator({ io, ...object }),
      trace: event => {
        traces.push(event)
        if (event === 'capacity-pressure') windowPressured.resolve()
      },
    })
    const execution = await createProgressiveWorkspaceExecution({ archive, intent,
      outputIdentity: { backend: 'native-zip', outputSessionId: 'capacity-test' },
      settlement: {
        pause: async (_request, evidence) => {
          expect(evidence.kind).toBe('checkpoint-committed')
          expect(evidence.checkpoint).toEqual(await store.readCheckpoint())
          return { operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n,
            kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: object.objectId,
            checkpointGeneration: evidence.checkpoint.generation, occupiedBytes: evidence.checkpoint.physicalLength,
            checkpointRecovery: 'current-cut', completedFileCount: 2n, completedBytes: BLOCK_SIZE * 2n,
            discoveryComplete: evidence.checkpoint.discoveryComplete }
        },
        settle: async () => { throw new Error('Capacity refusal cannot seal the archive') },
      },
    })
    expect(execution.output.executionProfile).not.toHaveProperty('automaticCheckpoint')
    const readers = readerFixture(files, [], { beforeOpen: async id => {
      if (id === d!.idText) {
        fourthOpening.resolve()
        await windowPressured.promise
      }
    } })
    const revisions: V2RevisionReader = { open: async (id, signal) => {
      const opened = await readers.revisions.open(id, signal)
      return { ...opened, descriptor: { ...opened.descriptor, geometry: new FileGeometry(opened.descriptor.exactSize, BLOCK_SIZE) } }
    } }
    const broker: V2BlockRangeReader = { readRange: async function* (descriptor, _lease, range, request) {
      expect(range.start).toBe(0n)
      if (descriptor.fileIdText === a!.idText) {
        yield { offset: 0n, data: new Uint8Array(BLOCK_BYTES).fill(1) }
        firstA.resolve()
        await bothDeclined.promise
        request?.signal?.throwIfAborted()
        yield { offset: BLOCK_SIZE, data: new Uint8Array(BLOCK_BYTES).fill(2) }
      } else if (descriptor.fileIdText === b!.idText) {
        await firstA.promise
        yield { offset: 0n, data: new Uint8Array(BLOCK_BYTES).fill(3) }
        await Promise.all([thirdActive.promise, fourthOpening.promise])
        pressure = true
        pressureStarted.resolve()
        yield { offset: BLOCK_SIZE, data: new Uint8Array(BLOCK_BYTES).fill(4) }
      } else {
        expect(descriptor.fileIdText).toBe(c!.idText)
        thirdActive.resolve()
        await pressureStarted.promise
        yield { offset: 0n, data: new Uint8Array(BLOCK_BYTES).fill(5) }
      }
    } }
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const catalog = catalogFixture([{ id: identity(2), entries: files }])
    const result = await transferJobFixture({ catalog: catalog.catalog, selection, intent, plans, revisions, broker, chunkSize: BLOCK_BYTES }).run()
    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip' })
    expect(result.failureTrigger?.fault).toMatchObject({ domain: 'output', code: 'resource-budget' })
    expect(refusals).toBe(2)
    expect(readers.releases).toHaveLength(4)
    expect(readers.revisionRequests).not.toContain(e!.idText)
    expect(await archive.findCommittedEntry(`file:${e!.idText}`)).toBeUndefined()
    expect(archive.failed).toBe(false)
    expect(io.closed).toBe(true)
    expect((await archive.committedEntry(`file:${a!.idText}`)).ranges).toMatchObject([{ start: 0n, end: BLOCK_SIZE * 2n }])
    expect((await archive.committedEntry(`file:${b!.idText}`)).ranges).toMatchObject([{ start: 0n, end: BLOCK_SIZE }])
    expect((await archive.committedEntry(`file:${c!.idText}`)).ranges).toEqual([])
    expect(traces).toContain('capacity-pressure')
    expect(traces).toContain('capacity-drained')
    expect(io.writes).toHaveLength(3)
  })

  it('stops new entry admissions during pressure and aborts waits without a retry loop', async () => {
    const window = new ZipCapacityWindow(2)
    const a = window.enter('a')
    const b = window.enter('b')
    expect(() => window.enter('a')).toThrow('active transaction')
    expect(() => window.enter('c')).toThrow('window exceeded')
    const blocked = b.blocked(new DOMException('No capacity', 'QuotaExceededError'))
    expect(() => window.enter('c')).toThrow(OutputCapacityBlockedError)
    const controller = new AbortController()
    const waiting = blocked.waitForDrain(controller.signal)
    controller.abort(new Error('user pause'))
    await expect(waiting).rejects.toThrow('user pause')
    b.finish()
    let drained = false
    const resumed = blocked.waitForDrain(new AbortController().signal).then(() => { drained = true })
    await Promise.resolve()
    expect(drained).toBe(false)
    a.finish()
    await resumed
    expect(drained).toBe(true)
    await expect(window.beforeDirectory(new AbortController().signal)).rejects.toBeInstanceOf(OutputCapacityBlockedError)
  })
})
