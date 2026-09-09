import { describe, expect, it, vi } from 'vitest'
import { deferred, manualSettlementDeadline } from '../../transfer/settlement-deadline'
import { BoundaryFaultError, FaultScope, SourceFaultCode, sourceFault } from '../../../src/transfer/fault'
import { V2SelectionPolicy } from '../../../src/catalog/v2-selection'
import { createProgressiveWorkspaceExecution } from '../../../src/transfer/settlement/progressive-workspace-execution'
import { createProgressiveZipOutput } from '../../../src/output/progressive-zip/output'
import { catalogFixture, directoryEntry, fileEntry, identity, planAuthorityFixture, readerFixture,
  receiveIntentFixture, transferJobFixture } from '../../transfer/v2-job-fixture'
import { ZipReader, Uint8ArrayReader, Uint8ArrayWriter } from '@zip.js/zip.js'
import { ProgressiveZipArchive, type ZipObjectCapacity } from '../../../src/output/progressive-zip/archive'
import { ObjectCheckpointCoordinator } from '../../../src/output/origin-private/native-object/coordinator'
import type { NativeObjectIO } from '../../../src/output/origin-private/native-object/contracts'
import type { TaskCheckpoint, TaskEntry, TaskDirectoryPin } from '../../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/store'

const object = { operationId: 'task', objectId: 'archive', kind: 'zip-archive' as const, handleId: 'handle' }
const source = { shareInstance: 'share', directoryId: 'parent', generation: 'generation', sourcePath: ['file'] }
const bytes = (text: string) => new TextEncoder().encode(text)

class MemoryIO implements NativeObjectIO {
  data = new Uint8Array(0)
  writes: { offset: bigint; length: bigint }[] = []
  flushes = 0
  failFlush = false
  closed = false
  async writeAt(offset: bigint, value: Uint8Array) {
    const end = Number(offset) + value.byteLength
    if (end > this.data.byteLength) {
      const expanded = new Uint8Array(end)
      expanded.set(this.data)
      this.data = expanded
    }
    this.data.set(value, Number(offset))
    this.writes.push({ offset, length: BigInt(value.length) })
  }
  async truncate(length: bigint) {
    const next = new Uint8Array(Number(length))
    next.set(this.data.subarray(0, Number(length)))
    this.data = next
  }
  async size() { return BigInt(this.data.length) }
  async flush() {
    this.flushes++
    if (this.failFlush) throw new DOMException('native flush failed', 'UnknownError')
  }
  async close() { this.closed = true }
}

class MemoryStore implements TaskCheckpointStore {
  checkpoint: TaskCheckpoint | undefined
  entries = new Map<string, TaskEntry>()
  pins = new Map<string, TaskDirectoryPin>()
  async readDirectoryPin(id: string) { return this.pins.get(id) }
  failCommit = false
  failFinalizationCursor: bigint | undefined
  async readCheckpoint() { return this.checkpoint }
  async readEntry(id: string) { return this.entries.get(id) }
  async readPath(path: readonly string[]) { return [...this.entries.values()].find(e => e.path.join('/') === path.join('/')) }
  async readEntries(input: { afterSequence?: bigint; limit: number }) {
    return [...this.entries.values()].filter(e =>
      input.afterSequence === undefined || e.zipLayout!.sequence > input.afterSequence,
    ).sort((a, b) => Number(a.zipLayout!.sequence - b.zipLayout!.sequence)).slice(0, input.limit)
  }
  async commit(input: Parameters<TaskCheckpointStore['commit']>[0]) {
    if (this.failCommit || (this.failFinalizationCursor !== undefined &&
        input.checkpoint.finalization?.nextEntry === this.failFinalizationCursor)) {
      throw new Error('metadata commit failed')
    }
    expect(input.expectedGeneration).toBe(this.checkpoint?.generation)
    this.checkpoint = input.checkpoint
    for (const entry of input.entries) this.entries.set(entry.entryId, entry)
    for (const pin of input.directoryPins ?? []) this.pins.set(pin.directoryId, pin)
  }
  close() {}
}

function fixture(store = new MemoryStore(), io = new MemoryIO(), maximumLength?: bigint,
  operationId = object.operationId) {
  const growth: bigint[] = []
  let occupiedLength = BigInt(io.data.length)
  let retainedHeadroom = 0n
  const pressure = { exhausted: false }
  const capacity: ZipObjectCapacity = {
    reserveGrowth: async request => {
      expect(request.currentLength).toBe(occupiedLength)
      if (pressure.exhausted && (request.targetLength > request.currentLength || request.metadataHeadroom > 0n)) {
        throw new DOMException('Growth admission declined', 'QuotaExceededError')
      }
      if (maximumLength !== undefined && request.targetLength > maximumLength &&
          request.targetLength > request.currentLength) {
        throw new DOMException('Growth admission declined', 'QuotaExceededError')
      }
      growth.push(request.targetLength > request.currentLength ? request.targetLength - request.currentLength : 0n)
      retainedHeadroom += request.metadataHeadroom
      let settled = false
      const release = async () => {
        if (!settled) { retainedHeadroom -= request.metadataHeadroom; settled = true }
      }
      return {
        settle: async actualLength => { occupiedLength = actualLength; await release() }, release,
      }
    },
  }
  const open = async () => {
    occupiedLength = await io.size()
    return ProgressiveZipArchive.open({
      object: { ...object, operationId }, store, selectedPaths: [['z'], ['a']],
      coordinator: new ObjectCheckpointCoordinator({ io, ...object, operationId }),
      capacity,
    })
  }
  return { store, io, growth, open, pressure, headroom: () => retainedHeadroom }
}

const admit = (archive: ProgressiveZipArchive, id: string, size: bigint) => archive.admitFile({
  entryId: id, path: [id], source, revision: { fileId: id, fileRevision: 'revision', exactSize: size },
})

describe('progressive ZIP settlement and cancellation', () => {
  it('drains a timed-out finalization before pausing and resumes locally without losing payload', async () => {
    const file = fileEntry(identity(19), 'file.bin', 6n)
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    const archive = await f.open()
    const entered = deferred()
    const release = deferred()
    const checkpoint = archive.checkpoint.bind(archive)
    vi.spyOn(archive, 'checkpoint').mockImplementation(async reason => {
      const state = await checkpoint(reason)
      if (reason === 'zip-before-finalization') { entered.resolve(); await release.promise }
      return state
    })
    const close = vi.spyOn(archive, 'close')
    const pause = vi.fn(async (_request, evidence) => ({
      kind: 'resumable-receive' as const, payloadKind: 'opfs-zip' as const,
      operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n,
      objectId: identity(33).toString(), checkpointGeneration: evidence.checkpoint.generation,
      occupiedBytes: evidence.checkpoint.physicalLength, completedFileCount: 1n, completedBytes: 6n,
      discoveryComplete: true,
    }))
    const publish = vi.fn(async () => { throw new Error('Canceled finalization must not publish') })
    const execution = await createProgressiveWorkspaceExecution({
      archive, intent, outputIdentity: { backend: 'native-zip', outputSessionId: 'timeout-session' },
      settlement: { pause, settle: publish },
    })
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const readers = readerFixture([file])
    const deadline = manualSettlementDeadline()
    const running = transferJobFixture({
      catalog: catalogFixture([{ id: identity(2), entries: [file] }]).catalog,
      selection, intent, plans, revisions: readers.revisions, broker: readers.broker,
      outputSettlementDeadline: deadline,
    }).run()
    let exposed = false
    const observation = running.then(() => { exposed = true })
    await entered.promise
    deadline.expire()
    // The deadline callback and its promise observers run before this turn yields.
    await Promise.resolve()
    await Promise.resolve()
    expect(close).not.toHaveBeenCalled()
    expect(pause).not.toHaveBeenCalled()
    expect(exposed).toBe(false)
    release.resolve()
    const result = await running
    await observation
    expect(result.lifecycle).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip' })
    expect(pause).toHaveBeenCalledOnce()
    expect(close).toHaveBeenCalledOnce()
    expect(publish).not.toHaveBeenCalled()
    expect(plans.unknownSettlements).toEqual([])
    const reopened = await f.open()
    await reopened.finalize()
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    const entries = (await reader.getEntries()).filter(entry => !entry.directory)
    expect(entries).toHaveLength(1)
    expect(entries[0]!.uncompressedSize).toBe(6)
    expect(await entries[0]!.getData!(new Uint8ArrayWriter())).toHaveLength(6)
    await reader.close()
  })

  it('retains a committed result when publication drains after the deadline', async () => {
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    const archive = await f.open()
    const entered = deferred()
    const release = deferred()
    const pause = vi.fn(async () => { throw new Error('Completed ZIP must not pause') })
    const execution = await createProgressiveWorkspaceExecution({
      archive, intent, outputIdentity: { backend: 'native-zip', outputSessionId: 'published-session' },
      settlement: {
        pause,
        settle: async () => {
          entered.resolve()
          await release.promise
          return { kind: 'waiting-to-save', operationId: intent.operationId,
            receiveIntentDigest: intent.digest, generation: 2n, packageDigest: 'package' }
        },
      },
    })
    const plans = planAuthorityFixture()
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution })
    const readers = readerFixture([])
    const deadline = manualSettlementDeadline()
    const running = transferJobFixture({
      catalog: catalogFixture([{ id: identity(2), entries: [] }]).catalog,
      selection, intent, plans, revisions: readers.revisions, broker: readers.broker,
      outputSettlementDeadline: deadline,
    }).run()
    await entered.promise
    deadline.expire()
    await Promise.resolve()
    expect(pause).not.toHaveBeenCalled()
    release.resolve()
    expect((await running).lifecycle.kind).toBe('waiting-to-save')
    expect(pause).not.toHaveBeenCalled()
    expect(plans.unknownSettlements).toEqual([])
  })

  it('stops finalization between committed batches and preserves the cursor for local continuation', async () => {
    const f = fixture()
    const archive = await f.open()
    await admit(archive, 'a', 3n)
    await archive.writeRange('a', 0n, bytes('abc'))
    await archive.markDiscoveryComplete()
    const controller = new AbortController()
    const commit = f.store.commit.bind(f.store)
    vi.spyOn(f.store, 'commit').mockImplementation(async input => {
      await commit(input)
      if (input.checkpoint.finalization?.nextEntry === 1n) controller.abort(new Error('deadline'))
    })
    await expect(archive.finalize(controller.signal)).rejects.toThrow('deadline')
    expect(archive.state.artifactState).toBe('finalizing')
    expect(archive.state.finalization?.nextEntry).toBe(1n)
    await archive.close()
    vi.mocked(f.store.commit).mockRestore()
    const reopened = await f.open()
    await reopened.finalize()
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    const entry = (await reader.getEntries())[0]!
    if (entry.directory) throw new Error('Expected retained file')
    expect(await entry.getData!(new Uint8ArrayWriter())).toEqual(bytes('abc'))
    await reader.close()
  })

})

describe('progressive native ZIP archive', () => {
  it('receives through the production OutputSession adapter with authenticated directory and revision ports', async () => {
    const file = fileEntry(identity(14), 'file.bin', 6n)
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection,
    })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    const archive = await f.open()
    const ports = await createProgressiveZipOutput({
      archive, intent, identity: { backend: 'native-zip', outputSessionId: 'test-session' },
    })
    const plans = planAuthorityFixture()
    const openWorkspaceZip = plans.openWorkspaceZip
    plans.openWorkspaceZip = async (intent, signal) => {
      const accepted = await openWorkspaceZip(intent, signal)
      if (accepted.kind === 'rejected') return accepted
      return { kind: 'accepted', execution: {
        ...accepted.execution, ...ports,
        discoveryGeneration: (id, generation, path) => archive.pinDirectory(id, generation, path),
        discoveryComplete: () => archive.markDiscoveryComplete(),
      } }
    }
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker,
    }).run()
    expect(result.worker.status).toBe('Succeeded')
    expect(archive.state.discoveryComplete).toBe(true)
    expect(archive.state.physicalLength).toBe(BigInt(f.io.data.length))
    expect(readers.revisionRequests).toEqual([file.idText])
    await archive.finalize()
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    const files = (await reader.getEntries()).filter(entry => !entry.directory)
    expect(files).toHaveLength(1)
    expect(files[0]!.uncompressedSize).toBe(6)
    await reader.close()
  })

  it('settles native ZIP directory receipts incrementally beyond the FSA retained-admission budget', async () => {
    const directories = Array.from({ length: 20 }, (_, index) => directoryEntry(identity(30 + index), `empty-${index}`))
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    const archive = await f.open()
    const ports = await createProgressiveZipOutput({ archive, intent,
      identity: { backend: 'native-zip', outputSessionId: 'directory-session' } })
    const plans = planAuthorityFixture()
    const originalOpen = plans.openWorkspaceZip
    plans.openWorkspaceZip = async (intent, signal) => {
      const accepted = await originalOpen(intent, signal)
      if (accepted.kind === 'rejected') return accepted
      return { kind: 'accepted', execution: { ...accepted.execution, ...ports,
        discoveryComplete: () => archive.markDiscoveryComplete() } }
    }
    const catalog = catalogFixture([{ id: identity(2), entries: directories },
      ...directories.map(directory => ({ id: directory.id, entries: [] }))])
    const readers = readerFixture([])
    const result = await transferJobFixture({ catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker, maximumDirectoryAdmissions: 1 }).run()
    expect(result.worker.status).toBe('Succeeded')
    expect(archive.state.entryCount).toBe(21n)
    await archive.finalize()
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    expect(await reader.getEntries()).toHaveLength(21)
    await reader.close()
  })

  it.each(Object.values(SourceFaultCode))('persists %s with partial ranges and resumes complete siblings without reopening their remote revision', async code => {
    const a = fileEntry(identity(16), 'a.bin', 2n)
    const b = fileEntry(identity(17), 'b.bin', 6n)
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    let archive = await f.open()
    const plans = planAuthorityFixture()
    const originalOpen = plans.openWorkspaceZip
    plans.openWorkspaceZip = async (intent, signal) => {
      const accepted = await originalOpen(intent, signal)
      if (accepted.kind === 'rejected') return accepted
      const ports = await createProgressiveZipOutput({ archive, intent,
        identity: { backend: 'native-zip', outputSessionId: 'resume-session' } })
      return { kind: 'accepted', execution: { ...accepted.execution, ...ports,
        discoveryComplete: () => archive.markDiscoveryComplete() } }
    }
    const catalog = catalogFixture([{ id: identity(2), entries: [a, b] }])
    const readers = readerFixture([a, b])
    const first = await transferJobFixture({ catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, maximumConcurrentFiles: 1,
      broker: { readRange: async function* (descriptor, lease, range, request) {
        if (descriptor.fileIdText !== b.idText) {
          yield* readers.broker.readRange(descriptor, lease, range, request)
          return
        }
        yield { offset: range.start, data: new Uint8Array(2).fill(7) }
        throw new BoundaryFaultError(sourceFault(FaultScope.FileLocal, code), 'source revision unavailable')
      } },
    }).run()
    expect(first.worker.status).not.toBe('Succeeded')
    const retainedB = await archive.committedEntry(`file:${b.idText}`)
    expect(retainedB.revisionFailure).toBe(code)
    expect(retainedB.ranges.map(r => [r.start, r.end])).toEqual([[0n, 2n]])
    expect((await archive.committedEntry(`file:${a.idText}`)).revisionFailure).toBeUndefined()
    await archive.close()
    archive = await f.open()
    const resumed = readerFixture([a, b], [], { failRevisionFor: a.idText })
    const result = await transferJobFixture({ catalog: catalog.catalog, selection, intent, plans,
      revisions: resumed.revisions, broker: resumed.broker, maximumConcurrentFiles: 1 }).run()
    expect(result.worker.status).toBe('Succeeded')
    expect(resumed.revisionRequests).toEqual([b.idText])
    const complete = []
    for await (const entry of archive.completeEntries()) complete.push(entry.entry.entryId)
    expect(complete).toContain(`file:${a.idText}`)
    expect(complete).toContain(`file:${b.idText}`)
    await archive.finalize()
  })

  it('retains the committed task cut after a real V2Job native flush failure without claiming a forced file cut', async () => {
    const file = fileEntry(identity(15), 'file.bin', 6n)
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection,
    })
    const f = fixture(new MemoryStore(), new MemoryIO(), undefined, intent.operationId)
    const archive = await f.open()
    const execution = await createProgressiveWorkspaceExecution({
      archive, intent, outputIdentity: { backend: 'native-zip', outputSessionId: 'failure-session' },
      settlement: {
        pause: async (_request, evidence) => {
          expect(evidence.kind).toBe('last-committed-checkpoint')
          const checkpoint = evidence.checkpoint
          expect(checkpoint).toEqual(await f.store.readCheckpoint())
          return {
            operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n,
            kind: 'resumable-receive', payloadKind: 'opfs-zip',
            objectId: checkpoint.object.objectId, checkpointGeneration: checkpoint.generation,
            occupiedBytes: checkpoint.physicalLength, completedFileCount: 0n, completedBytes: 0n,
            discoveryComplete: checkpoint.discoveryComplete,
          }
        },
        settle: async () => { throw new Error('failed reception must pause') },
      },
    })
    const plans = planAuthorityFixture()
    let baselineCheckpointed = false
    plans.openWorkspaceZip = async () => ({ kind: 'accepted', execution: {
      ...execution,
      output: {
        ...execution.output,
        beginFile: async (request, signal) => {
          const begun = await execution.output.beginFile(request, signal)
          return { ...begun, transaction: {
            ...begun.transaction,
            writeRange: async (offset, data, signal) => {
              if (!baselineCheckpointed) {
                await begun.transaction.writeRange(offset, data.subarray(0, 2), signal)
                await archive.checkpoint('known-durable-prefix')
                baselineCheckpointed = true
                if (data.length <= 2) return
                await begun.transaction.writeRange(offset + 2n, data.subarray(2), signal)
              } else {
                await begun.transaction.writeRange(offset, data, signal)
              }
              f.io.failFlush = true
            },
          } }
        },
      },
    } })
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker,
    }).run()
    expect(result.worker.status).not.toBe('Succeeded')
    expect(result.lifecycle).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip' })
    expect((await archive.committedEntry(`file:${file.idText}`)).ranges.map(r => [r.start, r.end])).toEqual([[0n, 2n]])
    expect(archive.failed).toBe(true)
    expect(f.io.closed).toBe(true)
  })

  it('writes concurrent files at final offsets, resumes a committed cut and finalizes without payload reads', async () => {
    const f = fixture()
    let archive = await f.open()
    const z = await admit(archive, 'z', 6n)
    const a = await admit(archive, 'a', 3n)
    await archive.admitDirectory({ entryId: 'empty', path: ['empty'], source })
    expect(z.zipLayout!.sequence).toBe(0n)
    expect(a.zipLayout!.sequence).toBe(1n)
    await Promise.all([archive.writeRange('z', 3n, bytes('456')), archive.writeRange('a', 0n, bytes('abc'))])
    await archive.checkpoint('pause')
    expect((await f.store.readEntry('z'))!.ranges).toHaveLength(1)
    await archive.close()
    archive = await f.open()
    const writesBeforeRetry = f.io.writes.length
    await archive.writeRange('a', 0n, bytes('BAD'))
    expect(f.io.writes).toHaveLength(writesBeforeRetry)
    await archive.writeRange('z', 0n, bytes('123'))
    await archive.markDiscoveryComplete()
    const spans = []
    for await (const span of archive.completeEntries()) spans.push(span)
    expect(spans).toHaveLength(3)
    const result = await archive.finalize()
    expect(result.artifactState).toBe('sealed')
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    const entries = await reader.getEntries()
    expect(entries.map(e => e.filename)).toEqual(['z', 'a', 'empty/'])
    const decoded = []
    for (const entry of entries) {
      if (!entry.directory) decoded.push(new TextDecoder().decode(await entry.getData!(new Uint8ArrayWriter())))
    }
    expect(decoded).toEqual(['123456', 'abc'])
    await reader.close()
    expect(f.growth.some(n => n > 3n)).toBe(true)
    expect(f.io.closed).toBe(true)
  })

  it('trusts only committed CRC coverage when bytes flush but the metadata transaction fails', async () => {
    const f = fixture()
    let archive = await f.open()
    await admit(archive, 'z', 6n)
    await archive.writeRange('z', 0n, bytes('123'))
    await archive.checkpoint('first')
    await archive.writeRange('z', 3n, bytes('456'))
    f.store.failCommit = true
    await expect(archive.checkpoint('failure')).rejects.toThrow('metadata')
    expect(archive.failed).toBe(true)
    expect(f.io.closed).toBe(true)
    f.store.failCommit = false
    archive = await f.open()
    expect((await archive.entry('z')).ranges.map(r => [r.start, r.end])).toEqual([[0n, 3n]])
    await archive.writeRange('z', 3n, bytes('456'))
    await archive.markDiscoveryComplete()
    expect((await archive.finalize()).artifactState).toBe('sealed')
  })

  it('requires discovery completion and every payload before local finalization', async () => {
    const f = fixture()
    const archive = await f.open()
    await admit(archive, 'a', 2n)
    await expect(archive.finalize()).rejects.toThrow('discovery')
    await archive.markDiscoveryComplete()
    await expect(archive.finalize()).rejects.toThrow('a')
    expect(archive.state.artifactState).toBe('receiving')
  })

  it('resumes paged finalization after an uncommitted metadata tail without touching earlier entries', async () => {
    const f = fixture()
    let archive = await f.open()
    for (let index = 0; index < 130; index++) {
      await archive.admitDirectory({ entryId: String(index), path: [String(index)], source })
    }
    await archive.markDiscoveryComplete()
    f.store.failFinalizationCursor = 130n
    await expect(archive.finalize()).rejects.toThrow('metadata')
    expect(archive.state.finalization!.nextEntry).toBe(128n)
    f.store.failFinalizationCursor = undefined
    f.io.writes = []
    archive = await f.open()
    await archive.finalize()
    const firstResumed = (await f.store.readEntry('128'))!.zipLayout!.localHeaderOffset
    expect(f.io.writes.every(write => write.offset >= firstResumed)).toBe(true)
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data))
    expect(await reader.getEntries()).toHaveLength(130)
    await reader.close()
  })

  it('pins discovery independently of payload and rejects changed revisions without stopping unrelated entries', async () => {
    const f = fixture()
    const archive = await f.open()
    await archive.pinDirectory('root', 'first-generation', [])
    await expect(archive.pinDirectory('root', 'replacement-generation', [])).rejects.toThrow('generation changed')
    await admit(archive, 'a', 2n)
    await admit(archive, 'b', 2n)
    await expect(archive.admitFile({
      entryId: 'a', path: ['a'], source,
      revision: { fileId: 'a', fileRevision: 'replacement', exactSize: 2n },
    })).rejects.toThrow('source revision')
    await archive.writeRange('b', 0n, bytes('ok'))
    await archive.checkpoint('unrelated-completion')
    expect((await archive.entry('a')).revisionFailure).toContain('Original revision')
    expect((await archive.entry('b')).ranges).toHaveLength(1)
    expect(f.io.closed).toBe(false)
    await admit(archive, 'a', 2n)
    expect((await archive.entry('a')).revisionFailure).toBeUndefined()
    await expect(archive.admitFile({ entryId: 'a', path: ['a'],
      source: { ...source, generation: 'other-generation' },
      revision: { fileId: 'a', fileRevision: 'revision', exactSize: 2n },
    })).rejects.toThrow('source revision')
  })

  it('rejects recovery when the physical object lost committed payload bytes', async () => {
    const f = fixture()
    const archive = await f.open()
    await admit(archive, 'a', 2n)
    await archive.writeRange('a', 0n, bytes('ok'))
    await archive.checkpoint('durable')
    await archive.close()
    await f.io.truncate(0n)
    await expect(f.open()).rejects.toThrow('shorter')
  })

  it('declines later sparse growth while allowing admitted earlier payloads to keep filling', async () => {
    const f = fixture(new MemoryStore(), new MemoryIO(), 200n)
    const archive = await f.open()
    await admit(archive, 'early', 1000n)
    await admit(archive, 'late', 1n)
    await archive.writeRange('early', 100n, bytes('ok'))
    const protectedHeadroom = f.headroom()
    f.pressure.exhausted = true
    await expect(archive.writeRange('late', 0n, bytes('x'))).rejects.toThrow('Growth admission')
    expect(archive.failed).toBe(false)
    await archive.writeRange('early', 2n, bytes('ok'))
    await archive.writeRange('early', 0n, bytes('go'))
    await archive.checkpoint('allocated-region-progress')
    expect((await archive.entry('early')).ranges.map(r => [r.start, r.end])).toEqual([[0n, 4n], [100n, 102n]])
    expect((await archive.entry('late')).ranges).toEqual([])
    expect(f.headroom()).toBe(protectedHeadroom)
    await archive.close()
    expect(f.headroom()).toBe(0n)
  })

  it('uses ZIP64 layout arithmetic without allocating a multi-gigabyte fixture', async () => {
    const f = fixture()
    const archive = await f.open()
    const huge = await admit(archive, 'huge', 0xffffffffn)
    const later = await admit(archive, 'later', 1n)
    expect(huge.zipPlan!.zip64Size).toBe(true)
    expect(later.zipPlan!.zip64Offset).toBe(true)
    expect(f.io.data).toHaveLength(0)
  })
})

describe('progressive ZIP finalization capacity', () => {
  it('finalizes many small entries with page-sized capacity admissions and no payload rewrite', async () => {
    const f = fixture()
    const archive = await f.open()
    const entryCount = 686
    for (let index = 0; index < entryCount; index++) {
      await admit(archive, String(index), 1n)
      await archive.writeRange(String(index), 0n, new Uint8Array([index % 256]))
    }
    await archive.markDiscoveryComplete()
    const admissionsBefore = f.growth.length
    const writesBefore = f.io.writes.length
    const result = await archive.finalize()
    // The regression was thousands of strict capacity transactions for a tiny archive.
    expect(f.growth.length - admissionsBefore).toBeLessThanOrEqual(8)
    const metadataWrites = f.io.writes.slice(writesBefore)
    for (const entry of f.store.entries.values()) {
      const payload = entry.zipLayout!.payloadOffset
      expect(metadataWrites.every(write => write.offset >= payload + 1n ||
        write.offset + write.length <= payload)).toBe(true)
    }
    const reader = new ZipReader(new Uint8ArrayReader(f.io.data), { checkSignature: true })
    const entries = await reader.getEntries()
    expect(entries).toHaveLength(entryCount)
    const lastFile = entries.at(-1)!
    if (lastFile.directory) throw new Error('Expected the last payload file')
    expect(await lastFile.getData!(new Uint8ArrayWriter())).toEqual(new Uint8Array([(entryCount - 1) % 256]))
    expect(result.sealedLength).toBe(BigInt(f.io.data.length))
    await reader.close()
  })
})
