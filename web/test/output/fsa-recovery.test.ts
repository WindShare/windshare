import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import type { OutputSessionHandle } from '../../src/output/backends/contract'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import { RetryableFilePublishError } from '../../src/transfer/fault'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, MemoryReceiveStore, openFolderFixture } from './fsa-fixture'

const paused = {
  status: 'paused', completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 8n,
  discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
} satisfies SettledTransferJobResult

const request = { fileId: 'large', path: ['large'], sourcePath: ['large'], revision: 'revision', exactSize: 8n }
const signal = new AbortController().signal
const prefix = [byteRange(0n, 2n)]
const extended = [byteRange(0n, 4n)]

async function reopen(session: OutputSessionHandle, store: ReceiveStore) {
  const task = await session.settle(paused)
  await session.close()
  return reopenFSASession({ task, store, allowPermissionPrompt: false })
}

describe('FSA publication and recovery', () => {
  it.each([
    { prior: false, persistent: false }, { prior: false, persistent: true },
    { prior: true, persistent: false }, { prior: true, persistent: true },
  ])('reconciles a rejected file record ($prior prior checkpoint, $persistent persistent error)', async ({ prior, persistent }) => {
    const { open, root, session, memory } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    if (prior) {
      await file.checkpoint(prefix, 'disconnect')
      await file.writeAt(2n, Uint8Array.of(3, 4))
    }
    const ranges = prior ? extended : prefix
    const bytes = prior ? [1, 2, 3, 4] : [1, 2]
    const put = memory.putFile.bind(memory)
    let failures = 0
    vi.spyOn(memory, 'putFile').mockImplementation(async row => {
      if (!row.fsaPendingPublish && (persistent || failures++ === 0)) throw new DOMException('quota', 'QuotaExceededError')
      await put(row)
    })
    await expect(file.checkpoint(ranges, 'disconnect')).rejects.toMatchObject({
      message: expect.stringContaining('Downloads/Folder/large'), fault: { code: 'quota-exceeded' },
    })
    expect(await memory.getFile(session.taskId, 'large')).toMatchObject({
      completedRanges: [], fsaPendingPublish: { ranges: prior ? [[0n, 2n]] : [] },
    })
    await file.abort()
    if (!persistent) {
      expect([...(await root.getFileHandle('large')).bytes]).toEqual(bytes)
      expect(await memory.getFile(session.taskId, 'large')).toMatchObject({
        completedRanges: [[0n, BigInt(bytes.length)]], fsaDiskFingerprint: { size: bytes.length },
      })
      expect((await memory.getFile(session.taskId, 'large'))?.fsaPendingPublish).toBeUndefined()
    } else if (prior) {
      expect([...(await root.getFileHandle('large')).bytes]).toEqual(bytes)
      expect(await memory.getFile(session.taskId, 'large')).toMatchObject({
        completedRanges: [], fsaPendingPublish: { ranges: [[0n, 2n]] },
      })
    } else {
      expect(root.children.has('large')).toBe(false)
      expect(await memory.getFile(session.taskId, 'large')).toBeUndefined()
    }
    await session.close()
  })

  it.each([false, true])('preserves a fully written resumed file through a metadata failure (persistent=%s)', async persistent => {
    const { open, root, session, memory } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint(prefix, 'disconnect')
    await file.writeAt(2n, Uint8Array.of(3, 4, 5, 6, 7, 8))
    const put = memory.putFile.bind(memory)
    let attempted = false
    vi.spyOn(memory, 'putFile').mockImplementation(async row => {
      if (row.status === 'completed' && (persistent || !attempted)) {
        attempted = true
        throw new Error('write failed')
      }
      await put(row)
    })
    await expect(file.commit([byteRange(0n, 8n)])).rejects.toThrow('Downloads/Folder/large')
    await file.abort()
    const record = await memory.getFile(session.taskId, 'large')
    expect([...(await root.getFileHandle('large')).bytes]).toEqual([1, 2, 3, 4, 5, 6, 7, 8])
    if (persistent) {
      expect(record).toMatchObject({ status: 'receiving', completedRanges: [], fsaPendingPublish: { ranges: [[0n, 2n]] } })
    } else {
      expect(record).toMatchObject({ status: 'completed', completedRanges: [[0n, 8n]] })
    }
    await session.close()
  })

  it('retains a same-size replacement across the publication gap and restarts after the user moves it', async () => {
    const { open, root, session, memory, store } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint(prefix, 'disconnect')
    await file.writeAt(2n, Uint8Array.of(3, 4))
    const put = memory.putFile.bind(memory)
    const rejected = vi.spyOn(memory, 'putFile').mockImplementation(async row => {
      if (!row.fsaPendingPublish) throw new Error('record failed')
      await put(row)
    })
    await expect(file.checkpoint(extended, 'disconnect')).rejects.toThrow('Downloads/Folder/large')
    const replacement = root.putUserFile('large', Uint8Array.of(9, 8, 7, 6))
    await file.abort()
    rejected.mockRestore()
    const resumed = await reopen(session, store)
    await expect(resumed.transferOutput.openFile(request, signal)).rejects.toMatchObject({
      fault: { code: 'target-collision' }, message: expect.stringContaining('move it aside'),
    })
    expect([...replacement.bytes]).toEqual([9, 8, 7, 6])
    expect(replacement.writableCalls).toEqual([])
    expect((await memory.getFile(session.taskId, 'large'))?.completedRanges).toEqual([])
    root.children.delete('large')
    const restarted = await resumed.transferOutput.openFile(request, signal)
    expect(restarted.completedRanges).toEqual([])
    await restarted.writeAt(0n, Uint8Array.of(5, 6))
    await restarted.pause(prefix)
    expect([...(await root.getFileHandle('large')).bytes]).toEqual([5, 6])
    expect((await memory.getFile(session.taskId, 'large'))?.completedRanges).toEqual([[0n, 2n]])
    await resumed.close()
  })

  it('recovers only the fingerprinted prior version after an interrupted close', async () => {
    const { open, root, session, store, memory } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint(prefix, 'disconnect')
    const handle = await root.getFileHandle('large')
    const create = handle.createWritable.bind(handle)
    vi.spyOn(handle, 'createWritable').mockImplementationOnce(async options => ({
      ...await create(options), close: async () => { throw new Error('interrupted close') },
    }))
    await file.writeAt(2n, Uint8Array.of(3, 4))
    await expect(file.checkpoint(extended, 'disconnect')).rejects.toThrow('interrupted close')
    // Closing without the writer's abort callback leaves only IndexedDB and disk evidence for the next session.
    const resumed = await reopen(session, store)
    const recovered = await resumed.transferOutput.openFile(request, signal)
    expect(recovered.completedRanges).toEqual(prefix)
    expect([...handle.bytes]).toEqual([1, 2])
    expect(await memory.getFile(session.taskId, 'large')).toMatchObject({ completedRanges: [[0n, 2n]] })
    expect((await memory.getFile(session.taskId, 'large'))?.fsaPendingPublish).toBeUndefined()
    await resumed.close()
  })

  it('performs one metadata write for a new file and reads the row only when the saved name is rediscovered', async () => {
    const { open, root, session, memory } = await openFolderFixture()
    const lookup = vi.spyOn(memory, 'getFile')
    const persist = vi.spyOn(memory, 'putFile')
    const file = await open('large')
    const ranges = [byteRange(0n, 8n)]
    await file.writeAt(0n, new Uint8Array(8))
    await file.commit(ranges)
    const handle = await root.getFileHandle('large')
    expect(lookup).not.toHaveBeenCalled()
    expect(persist).toHaveBeenCalledOnce()
    expect(handle.getFileCalls).toBe(0)
    const again = await open('large')
    expect(again.completedRanges).toEqual(ranges)
    expect(lookup).toHaveBeenCalledExactlyOnceWith(session.taskId, 'large')
    expect(handle.writableCalls).toEqual([false])
    await session.close()
  })

  it('preserves a complete file whose ordinary commit lost its only IndexedDB write', async () => {
    const { open, root, session, store, memory } = await openFolderFixture()
    const file = await open('large')
    const bytes = Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8)
    await file.writeAt(0n, bytes)
    vi.spyOn(memory, 'putFile').mockRejectedValueOnce(new Error('metadata lost'))
    await expect(file.commit([byteRange(0n, 8n)])).rejects.toThrow('Downloads/Folder/large')
    await file.abort()
    expect(await store.getFile(session.taskId, 'large')).toBeUndefined()
    const resumed = await reopen(session, store)
    await expect(resumed.transferOutput.openFile(request, signal)).rejects.toMatchObject({ fault: { code: 'target-collision' } })
    const physical = await root.getFileHandle('large')
    expect(physical.bytes).toEqual(bytes)
    expect(physical.writableCalls).toEqual([false])
    await resumed.close()
  })

  it.each(['pause', 'commit'] as const)('rechecks a cut before %s without opening a second stream', async action => {
    const { open, root, session, memory } = await openFolderFixture()
    const file = await open('large')
    const bytes = new Uint8Array(8).fill(4)
    const ranges = [byteRange(0n, 8n)]
    await file.writeAt(0n, bytes)
    await file.checkpoint(ranges, 'disconnect')
    const put = vi.spyOn(memory, 'putFile')
    await file[action](ranges)
    expect(put).toHaveBeenCalledTimes(action === 'pause' ? 0 : 1)
    expect((await root.getFileHandle('large')).writableCalls).toEqual([false])
    expect(await memory.getFile(session.taskId, 'large')).toMatchObject({
      status: action === 'pause' ? 'receiving' : 'completed', completedRanges: [[0n, 8n]],
    })
    await session.close()
  })

  it('verifies the named entry again when a handle outlives a same-name replacement', async () => {
    const { open, root, session } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint(prefix, 'disconnect')
    const swapped = root.putUserFile('large', Uint8Array.of(5, 9))
    await expect(file.writeAt(2n, Uint8Array.of(3))).rejects.toMatchObject({ fault: { code: 'target-collision' } })
    expect([...swapped.bytes]).toEqual([5, 9])
    expect(swapped.writableCalls).toEqual([])
    await session.close()
  })

  it('honors cancellation during a slow prefix copy and leaves the verified checkpoint usable', async () => {
    const { session, root, memory } = await openFolderFixture()
    const controller = new AbortController()
    const file = await session.transferOutput.openFile(request, controller.signal)
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint(prefix, 'disconnect')
    const physical = await root.getFileHandle('large')
    const create = physical.createWritable.bind(physical)
    const write = vi.fn(), abort = vi.fn()
    const cancelled = new DOMException('user pause', 'AbortError')
    vi.spyOn(physical, 'createWritable').mockImplementationOnce(async options => {
      const stream = await create(options)
      controller.abort(cancelled)
      return { ...stream, write, abort: async () => { abort() } }
    })
    await expect(file.writeAt(2n, Uint8Array.of(9))).rejects.toBe(cancelled)
    expect(write).not.toHaveBeenCalled()
    expect(abort).toHaveBeenCalledOnce()
    await file.pause(prefix)
    expect([...physical.bytes]).toEqual([1, 2])
    expect((await memory.getFile(session.taskId, 'large'))?.completedRanges).toEqual([[0n, 2n]])
    await session.close()
  })

  it.each(['directory', 'single-file'] as const)('reclaims an uncommitted 0-byte %s placeholder after an abrupt exit during first write', async mode => {
    const memory = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', memory.events)
    const store = memory as unknown as ReceiveStore
    const artifact = mode === 'single-file'
      ? { kind: 'directory-tree' as const, name: 'large', singleFileId: 'large' }
      : { kind: 'directory-tree' as const, name: 'Folder' }
    const session = await openNewFSASession({
      task: {
        taskId: `crash-${mode}`, selectionKey: 'selection',
        selection: { shareInstance: 'share', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        artifact, lifecycle: initialReceiveState(`crash-${mode}`),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle, store,
    })
    const writer = await session.transferOutput.openFile(request, signal)
    await writer.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    // Simulate closing the page before commit or pause: no file row is persisted and disk stays 0 bytes.
    await session.close()
    const container = mode === 'single-file' ? parent : await parent.getDirectoryHandle('Folder')
    expect([...(await container.getFileHandle('large')).bytes]).toEqual([])
    expect(await memory.getFile(session.taskId, 'large')).toBeUndefined()

    const traces: OutputTraceEvent[] = []
    const persisted = (await memory.getTask(session.taskId))!
    const reopened = await reopenFSASession({
      task: persisted, store, allowPermissionPrompt: false,
      diagnostics: { backend: 'file_system_access', trace: { current: event => traces.push(event) } },
    })
    // Concurrent opens colliding onto the same reclaimed placeholder handle must admit only one owner.
    const [firstOutcome, secondOutcome] = await Promise.allSettled([
      reopened.transferOutput.openFile(request, signal),
      reopened.transferOutput.openFile({ ...request, fileId: 'other' }, signal),
    ])
    expect(firstOutcome.status).toBe('fulfilled')
    expect(secondOutcome).toMatchObject({ status: 'rejected', reason: { fault: { code: 'target-collision' } } })
    if (firstOutcome.status !== 'fulfilled') throw new Error('expected first open to reclaim placeholder')
    const resumed = firstOutcome.value
    expect(resumed.completedRanges).toEqual([])
    expect(traces).toContainEqual({
      eventName: 'reopen',
      payload: { backend: 'file_system_access', task_id: session.taskId, file_id: 'large', transition: 'placeholder_reclaimed' },
    })
    const payload = Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8)
    await resumed.writeAt(0n, payload)
    await resumed.commit([byteRange(0n, 8n)])
    expect([...(await container.getFileHandle('large')).bytes]).toEqual([...payload])
    expect(await memory.getFile(session.taskId, 'large')).toMatchObject({ status: 'completed', completedRanges: [[0n, 8n]] })
    await reopened.close()
  })

  it('removes a reclaimed 0-byte placeholder cleanly when the resumed attempt aborts before publication', async () => {
    const { open, root, session, store } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    const physical = await root.getFileHandle('large')
    // Age the task and placeholder together to confirm abort uses the placeholder's original mtime, not reopen time.
    const earlier = Date.now() - 60_000
    const task = (await store.getTask(session.taskId))!
    if (task.target.kind === 'fsa') {
      await store.putTask({ ...task, target: { ...task.target, reservedAtMs: earlier } })
    }
    physical.lastModified = earlier + 500
    await session.close()

    const reopened = await reopenFSASession({ task: (await store.getTask(session.taskId))!, store, allowPermissionPrompt: false })
    const resumed = await reopened.transferOutput.openFile(request, signal)
    await resumed.abort(new DOMException('stopped', 'AbortError'))
    expect(root.children.has('large')).toBe(false)
    await reopened.close()
  })

  it('rejects foreign or post-reopen 0-byte files while still rejecting unexpected files in a fresh session', async () => {
    const { root, session, store } = await openFolderFixture()
    // Fresh session must never reclaim an unexpected 0-byte entry created externally.
    root.putUserFile('fresh', new Uint8Array(0), Date.now())
    await expect(session.transferOutput.openFile({ ...request, fileId: 'fresh', path: ['fresh'] }, signal))
      .rejects.toMatchObject({ fault: { code: 'target-collision' } })
    expect(root.children.has('fresh')).toBe(true)

    const reopened = await reopen(session, store)
    // A 0-byte file created after reopenFSASession acquired the task lock cannot be a prior session's placeholder.
    root.putUserFile('newer', new Uint8Array(0), Date.now() + 10_000)
    await expect(reopened.transferOutput.openFile({ ...request, fileId: 'newer', path: ['newer'] }, signal))
      .rejects.toMatchObject({ fault: { code: 'target-collision' } })
    expect(root.children.has('newer')).toBe(true)
    await reopened.close()
  })
})

describe('FSA terminal cleanup', () => {
  it('defers physical cleanup until settlement is durable and retries a partially retired inventory', async () => {
    const { session, open, root, parent, memory, store } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.pause(prefix)
    const failure = { ...paused, status: 'failed' } as const
    vi.spyOn(memory, 'putTask').mockRejectedValueOnce(new Error('terminal write failed'))
    await expect(session.settle(failure)).rejects.toThrow('terminal write failed')
    expect((await memory.getTask(session.taskId))?.lifecycle.kind).toBe('downloading')
    expect((await memory.getFile(session.taskId, 'large'))?.completedRanges).toEqual([[0n, 2n]])
    expect([...(await root.getFileHandle('large')).bytes]).toEqual([1, 2])

    vi.spyOn(memory, 'deleteFiles').mockRejectedValueOnce(new Error('retirement interrupted'))
    const record = await session.settle(failure)
    expect(record.lifecycle.kind).toBe('failed')
    expect(parent.children.has('Folder')).toBe(false)
    expect(await memory.getFile(session.taskId, 'large')).toBeDefined()
    await session.close()
    await discardFSATask(record, store)
    expect(await memory.getTask(session.taskId)).toBeUndefined()
    expect(await memory.getFile(session.taskId, 'large')).toBeUndefined()
  })

  it('keeps selected empty directories alongside a partial result and prunes abandoned file parents', async () => {
    const { session, root, open, memory } = await openFolderFixture()
    await session.transferOutput.ensureDirectory?.({ path: ['keep'], directoryId: 'keep' }, signal)
    const complete = await open('saved')
    await complete.writeAt(0n, new Uint8Array(8))
    await complete.commit([byteRange(0n, 8n)])
    const file = await session.transferOutput.openFile({ ...request, path: ['unfinished', 'large'] }, signal)
    await file.abort()
    await session.settle({ ...paused, status: 'partial', completedFiles: 1n, completedBytes: 8n })
    expect([...root.children.keys()]).toEqual(['keep', 'saved'])
    expect((await memory.listTaskFiles(session.taskId)).map(row => row.fileId)).toEqual(['saved'])
    await session.close()
  })

  it('cancels a retained reopen before permission prompting and a discard before its first removal', async () => {
    const { session, open, parent, root, store, memory } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.pause(prefix)
    const record = await session.settle(paused)
    await session.close()
    const reopenSignal = new AbortController()
    vi.spyOn(parent, 'queryPermission').mockImplementationOnce(async () => {
      reopenSignal.abort()
      return 'prompt'
    })
    const prompt = vi.spyOn(parent, 'requestPermission')
    await expect(reopenFSASession({ task: record, store, allowPermissionPrompt: true, signal: reopenSignal.signal }))
      .rejects.toMatchObject({ name: 'AbortError' })
    expect(prompt).not.toHaveBeenCalled()
    expect((await memory.getTask(session.taskId))?.lifecycle.kind).toBe('paused')

    const discardSignal = new AbortController()
    const handle = await root.getFileHandle('large')
    const read = handle.getFile.bind(handle)
    vi.spyOn(handle, 'getFile').mockImplementationOnce(async () => {
      discardSignal.abort()
      return read()
    })
    await expect(discardFSATask(record, store, undefined, undefined, discardSignal.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect([...handle.bytes]).toEqual([1, 2])
    expect(root.children.get('large')).toBe(handle)
    expect(await memory.getFile(session.taskId, 'large')).toMatchObject({ completedRanges: [[0n, 2n]] })
  })

  it('retains explicit discard state after denied permission or I/O failure and skips a discharged task prompt', async () => {
    const { session, open, parent, root, memory, store } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.pause(prefix)
    const record = await session.settle(paused)
    await session.close()
    vi.spyOn(parent, 'queryPermission').mockResolvedValueOnce('prompt')
    const prompt = vi.spyOn(parent, 'requestPermission').mockResolvedValueOnce('denied')
    await expect(discardFSATask(record, store)).rejects.toMatchObject({ fault: { code: 'permission-denied' } })
    expect(prompt).toHaveBeenCalledOnce()
    vi.spyOn(root, 'removeEntry').mockRejectedValueOnce(new DOMException('disk busy', 'NoModificationAllowedError'))
    await expect(discardFSATask(record, store)).rejects.toMatchObject({ name: 'NoModificationAllowedError' })
    expect(await memory.getFile(session.taskId, 'large')).toBeDefined()
    expect([...(await root.getFileHandle('large')).bytes]).toEqual([1, 2])
    await discardFSATask(record, store)
    expect(await memory.getTask(session.taskId)).toBeUndefined()
    expect(parent.children.has('Folder')).toBe(false)
    await discardFSATask(record, store)
    expect(prompt).toHaveBeenCalledOnce()
  })
})

describe('FSA native close recovery', () => {
  const limit = 128 * 1024
  const cases = [
    { size: limit, destination: 'original', retry: true },
    { size: limit + 1, destination: 'original', retry: false },
    { size: 8, destination: 'changed', retry: false },
    { size: 8, destination: 'missing', retry: false },
    { size: 8, destination: 'checkpoint', retry: false },
    { size: 8, destination: 'aborted', retry: false },
    { size: 8, destination: 'cancel-after-removal', retry: true },
    { size: 8, destination: 'different-error', retry: false },
  ] as const
  it.each(cases)('reports safe removal=$retry for a $size-byte file whose destination is $destination', async ({ size, destination, retry }) => {
    const { session, root, store } = await openFolderFixture()
    const controller = new AbortController()
    const file = await session.transferOutput.openFile({ ...request, exactSize: BigInt(size) }, controller.signal)
    const handle = await root.getFileHandle('large')
    if (destination === 'checkpoint') {
      await file.writeAt(0n, Uint8Array.of(1, 2))
      await file.checkpoint(prefix, 'disconnect')
    }
    const create = handle.createWritable.bind(handle)
    if (destination === 'cancel-after-removal') {
      const remove = root.removeEntry.bind(root)
      vi.spyOn(root, 'removeEntry').mockImplementationOnce(async name => {
        await remove(name)
        controller.abort()
      })
    }
    const cause = new DOMException('native close failed', destination === 'different-error' ? 'UnknownError' : 'InvalidStateError')
    vi.spyOn(handle, 'createWritable').mockImplementationOnce(async options => ({
      ...await create(options),
      close: async () => {
        if (destination === 'changed') handle.overwriteExternally(Uint8Array.of(7, 8))
        if (destination === 'missing') await root.removeEntry('large')
        if (destination === 'aborted') controller.abort()
        throw cause
      },
    }))
    await file.writeAt(BigInt(size - 1), Uint8Array.of(3))
    const failure = await file.commit([byteRange(0n, BigInt(size))]).catch((cause: unknown) => cause)
    expect(failure).toMatchObject({ cause })
    expect(failure instanceof RetryableFilePublishError).toBe(retry)
    if (retry) {
      expect(root.children.has('large')).toBe(false)
      expect(await store.getFile(session.taskId, 'large')).toBeUndefined()
    }
    await file.abort()
    if (destination === 'changed') expect([...(await root.getFileHandle('large')).bytes]).toEqual([7, 8])
    if (destination === 'checkpoint') {
      expect([...(await root.getFileHandle('large')).bytes]).toEqual([1, 2])
      expect((await store.getFile(session.taskId, 'large'))?.completedRanges).toEqual([[0n, 2n]])
    }
    await session.close()
  })

  it('propagates an inconclusive root probe without creating a directory or recording a task', async () => {
    const { session, parent, memory, store } = await openFolderFixture()
    const cause = new DOMException('inconclusive lookup', 'InvalidStateError')
    vi.spyOn(parent, 'getFileHandle').mockRejectedValueOnce(cause)
    const id = 'second'
    await expect(openNewFSASession({
      task: {
        taskId: id, selectionKey: 'selection', selection: { shareInstance: 's', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'Second' }, lifecycle: initialReceiveState(id),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle, store,
    })).rejects.toBe(cause)
    expect(parent.children.has('Second')).toBe(false)
    expect(await memory.getTask(id)).toBeUndefined()
    await session.close()
  })
})
