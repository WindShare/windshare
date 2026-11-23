import { afterEach, assert, describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator, type ReceiveCoordinatorOptions } from '../../src/application/receive/coordinator'
import { DISCONNECT_CHECKPOINT_DELAY_MS } from '../../src/application/receive/disconnect-checkpoint-guard'
import { ReceiveStorage, type ReceiveStoreAccess } from '../../src/application/receive/receive-storage'
import * as outputContract from '../../src/output/backends/contract'
import { createMemoryOPFSEnv } from '../output/opfs-fixture'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { ReceiverConnectionState } from '../../src/receiver/connection-state'
import { ReceiveStore } from '../../src/output/store/database'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { TransferJob, type TransferJobOptions } from '../../src/transfer/job'
import { FakeDirectoryHandle, FakeFileHandle, MemoryReceiveStore } from '../output/fsa-fixture'
import { makeShareFixture } from '../transfer/share-fixture'

function gate() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

function fixture(picker?: () => Promise<FileSystemDirectoryHandle>) {
  const memory = new MemoryReceiveStore()
  const close = vi.fn(() => { memory.events.push('db.close') })
  const store = Object.assign(memory, { close }) as unknown as ReceiveStore
  vi.spyOn(ReceiveStore, 'open').mockResolvedValue(store)
  const source = makeShareFixture([{ seed: 42, name: 'data.bin', bytes: Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8) }])
  const parent = new FakeDirectoryHandle('Downloads', memory.events)
  const connection = new ReceiverConnectionState()
  const waiting = gate()
  let interrupted = true
  const held = new Set<string>()
  let releasing: ReturnType<typeof gate> | undefined
  const locks = { request: async (name: string, options: LockOptions, cb: (lock: object | null) => Promise<void>) => {
    if (held.has(name)) {
      if (options.ifAvailable) return cb(null)
      throw new Error(`Unexpected blocking lock wait for ${name}`)
    }
    held.add(name)
    try { await cb({}) } finally {
      const isTaskLock = name.startsWith('windshare-receive:')
      if (isTaskLock && releasing !== undefined) await releasing.promise
      held.delete(name)
      if (isTaskLock) memory.events.push('lock.release')
    }
  } } as unknown as LockManager
  const broker: BlockRangeReader = {
    async *readRange(desc, lease, span, options) {
      for await (const slice of source.broker.readRange(desc, lease, span, options)) {
        yield slice
        if (interrupted) {
          waiting.resolve()
          const signal = options!.signal!
          await new Promise<void>((_, reject) => {
            if (signal.aborted) reject(signal.reason)
            else signal.addEventListener('abort', () => reject(signal.reason), { once: true })
          })
        }
      }
    },
  }
  const subscribeConnection = vi.fn((cb: Parameters<JoinedBrowserShare['subscribeConnection']>[0]) => connection.subscribe(cb))
  const startConnectivity = vi.fn(() => ({ close: () => memory.events.push('connection.close') }))
  const share = {
    descriptor: source.descriptor, selection: new SelectionPolicy(true),
    inspectSelection: async () => ({
      kind: 'single-file', file: { name: 'data.bin', idText: 'data' }, sourcePath: ['data.bin'], exactBytes: 8n,
    }),
    beginDownloadConnectivity: startConnectivity, subscribeConnection,
    createTransferJob: (output: TransferOutputSession, _connectivity: unknown, options: Partial<TransferJobOptions>) =>
      new TransferJob({ ...source, broker, selection: new SelectionPolicy(true).snapshot(), ...options, output }),
  } as unknown as JoinedBrowserShare
  const trace = vi.fn()
  const coordinator = (overrides: Partial<ReceiveCoordinatorOptions> = {}) => new ReceiveCoordinator({
    windowPort: { navigator: { locks }, indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker ?? (async () => parent as unknown as FileSystemDirectoryHandle),
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() }, document: { createElement: vi.fn() } as unknown as Document },
    currentJoinedShare: () => share, outputTrace: { current: trace }, ...overrides,
  })
  const start = async (receive: ReceiveCoordinator, backend = 'fsa') => {
    receive.refreshOffers(share)
    await vi.waitFor(() => expect(receive.getSnapshot().offers?.kind).toBe('ready'), { interval: 1 })
    expect(receive.startDownload(backend + ':file')).toBe(true)
  }
  return { memory, close, store, locks, share, source, parent, connection, subscribeConnection, startConnectivity, coordinator, waiting, start, trace,
    isLocked: () => [...held].some(name => name.startsWith('windshare-receive:')), reconnect: () => { interrupted = false }, holdRelease: () => (releasing = gate()) }
}

function pausedTask(f: ReturnType<typeof fixture>) {
  const reservedHandle = new FakeFileHandle('data.bin', '/Downloads/data.bin', f.memory.events)
  return {
    taskId: 'resuming', selectionKey: 'key', selection: { shareInstance: f.source.descriptor.shareInstanceId,
      syntheticRoot: 'root', defaultSelected: true, rules: [] },
    shareInstance: f.source.descriptor.shareInstanceId, backend: 'fsa' as const,
    artifact: { kind: 'directory-tree' as const, name: 'data.bin', singleFileId: 'data' },
    target: {
      kind: 'fsa' as const,
      parentHandle: f.parent as unknown as FileSystemDirectoryHandle,
      reservedName: 'data.bin',
      collisionIndex: 0,
      singleFile: true as const,
      reservedAtMs: 0,
      reservedFile: {
        handle: reservedHandle as unknown as FileSystemFileHandle,
        fingerprint: { size: 0, lastModified: reservedHandle.lastModified },
      },
    },
    lifecycle: transitionReceiveState(initialReceiveState('resuming'), { kind: 'paused', reason: 'user',
      completedFiles: 0n, completedBytes: 0n, retainedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 8n, discoveryComplete: false }),
    discoveryComplete: false, updatedAt: 0,
  }
}

describe('receive session shutdown', () => {
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

  it.each(['new', 'continue'])('closes a late %s session under its lock before closing IndexedDB', async action => {
    const f = fixture(), receive = f.coordinator(), opening = gate(), finish = gate()
    const task = pausedTask(f)
    if (action === 'continue') { await f.memory.putTask(task); await receive.loadRetained() }
    const putTask = f.memory.putTask.bind(f.memory)
    vi.spyOn(f.memory, 'putTask').mockImplementationOnce(async record => {
      opening.resolve(); await finish.promise; await putTask(record)
    })
    if (action === 'new') await f.start(receive)
    else receive.performRetainedAction(receive.getSnapshot().retained.tasks[0]!, 'continue')
    await opening.promise
    const closing = receive.dispose()
    expect(f.isLocked()).toBe(true)
    expect(f.close).not.toHaveBeenCalled()
    finish.resolve()
    await closing
    expect(f.startConnectivity).not.toHaveBeenCalled()
    expect(f.memory.events.slice(-2)).toEqual(['lock.release', 'db.close'])
    expect([...f.memory.tasks.values()][0]?.lifecycle.kind).toBe('downloading')
  })

  it('keeps recoverable and healthy tasks actionable when one row is unreadable and IDB is full', async () => {
    const f = fixture(), { env, root } = createMemoryOPFSEnv()
    const receive = f.coordinator({ opfsEnv: env }), base = pausedTask(f)
    const stale = { ...base, taskId: 'stale', backend: 'opfs' as const,
      target: { kind: 'opfs' as const, objectName: 'stale.part', fileName: 'data.bin', isZip: false, allocatedLength: 8n },
      lifecycle: transitionReceiveState(initialReceiveState('stale'), { kind: 'downloading', phase: 'receiving' }) }
    const dir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    await dir.getFileHandle(stale.target.objectName, { create: true })
    await f.memory.putTask(stale)
    await f.memory.putTask({ ...stale, taskId: 'unreadable' })
    await f.memory.putTask({ ...base, lifecycle: transitionReceiveState(base.lifecycle, {
      kind: 'saved', savedFiles: 1n, savedBytes: 8n,
    }) })
    await f.memory.putFile({ id: 'stale:file', taskId: 'stale', fileId: 'file', revision: 'r', path: ['data.bin'],
      size: 8n, completedRanges: [[0n, 4n]], status: 'receiving' })
    const list = f.memory.listTaskFiles.bind(f.memory)
    vi.spyOn(f.memory, 'listTaskFiles').mockImplementation(async id => {
      if (id === 'unreadable') throw new DOMException('unavailable', 'DataError')
      return list(id)
    })
    vi.spyOn(f.memory, 'putTask').mockRejectedValue(new DOMException('full', 'QuotaExceededError'))
    await receive.loadRetained()
    const tasks = receive.getSnapshot().retained.tasks
    expect(tasks.map(task => task.taskId)).toEqual(['stale', base.taskId])
    const paused = tasks[0]!
    expect(paused.lifecycle).toMatchObject({ kind: 'paused', reason: 'storage-full', retainedBytes: 4n })
    expect(f.memory.tasks.get('stale')?.lifecycle.kind).toBe('downloading')
    expect(f.trace).toHaveBeenCalledWith(expect.objectContaining({ payload: expect.objectContaining({
      task_id: 'stale', transition: 'pause_persist_failed', error_class: 'quota_exceeded',
    }) }))
    receive.performRetainedAction(paused, 'discard')
    await vi.waitFor(() => expect(f.memory.tasks.has('stale')).toBe(false), { interval: 1 })
    expect(dir.files.has('stale.part')).toBe(false)
    expect(f.memory.tasks.has(base.taskId)).toBe(true)
    await receive.dispose()
  })

  it('forgets direct-write partial history without touching the destination', async () => {
    const f = fixture(), receive = f.coordinator(), base = pausedTask(f)
    await f.memory.putTask({ ...base, lifecycle: transitionReceiveState(base.lifecycle, {
      kind: 'partial', reason: 'failures', savedFiles: 1n, failedFiles: 1n,
    }) })
    const userFile = f.parent.putUserFile('data.bin', Uint8Array.of(7))
    const query = vi.spyOn(f.parent, 'queryPermission')
    await receive.loadRetained()
    receive.performRetainedAction(receive.getSnapshot().retained.tasks[0]!, 'forget')
    await vi.waitFor(() => expect(f.memory.tasks.size).toBe(0), { interval: 1 })
    expect(query).not.toHaveBeenCalled()
    expect(await f.parent.getFileHandle('data.bin')).toBe(userFile)
    await receive.dispose()
  })

  it.each(['queued', 'reading'] as const)('cancels a %s discard when the receiver closes', async timing => {
    const f = fixture(), receive = f.coordinator(), reading = gate(), finish = gate()
    await f.memory.putTask(pausedTask(f))
    await receive.loadRetained()
    const getTask = f.memory.getTask.bind(f.memory)
    vi.spyOn(f.memory, 'getTask').mockImplementationOnce(async id => {
      reading.resolve(); await finish.promise; return getTask(id)
    })
    const remove = vi.spyOn(f.memory, 'deleteTask')
    receive.performRetainedAction(receive.getSnapshot().retained.tasks[0]!, 'discard')
    if (timing === 'reading') await reading.promise
    const closing = receive.dispose()
    finish.resolve()
    await closing
    expect(remove).not.toHaveBeenCalled()
    expect(f.memory.tasks.size).toBe(1)
  })

  it('waits for a started deletion before closing the database', async () => {
    const f = fixture(), receive = f.coordinator(), deleting = gate(), finish = gate()
    await f.memory.putTask(pausedTask(f))
    await receive.loadRetained()
    const remove = f.memory.deleteTask.bind(f.memory)
    vi.spyOn(f.memory, 'deleteTask').mockImplementationOnce(async id => {
      deleting.resolve(); await finish.promise; await remove(id)
    })
    receive.performRetainedAction(receive.getSnapshot().retained.tasks[0]!, 'discard')
    await deleting.promise
    const closing = receive.dispose()
    expect(f.close).not.toHaveBeenCalled()
    finish.resolve()
    await closing
    expect(f.memory.tasks.size).toBe(0)
    expect(f.memory.events.slice(-2)).toEqual(['lock.release', 'db.close'])
  })

  it('skips inventory normalization if the page closes while IndexedDB opens', async () => {
    const f = fixture(), opening = gate(), finish = gate(), receive = f.coordinator()
    vi.mocked(ReceiveStore.open).mockImplementationOnce(async () => {
      opening.resolve(); await finish.promise; return f.store
    })
    const list = vi.spyOn(f.memory, 'listTasks')
    const loading = receive.loadRetained()
    await opening.promise
    const closing = receive.dispose()
    finish.resolve()
    await Promise.all([loading, closing])
    expect(list).not.toHaveBeenCalled()
    expect(f.close).toHaveBeenCalledOnce()
  })

  it('closes cleanly after a failed database open and refuses further opens', async () => {
    const f = fixture(), opening = gate(), finish = gate()
    const open = vi.mocked(ReceiveStore.open).mockImplementationOnce(async () => {
      opening.resolve(); await finish.promise; throw new DOMException('denied', 'NotAllowedError')
    })
    const storage = new ReceiveStorage({ open: vi.fn() } as unknown as IDBFactory)
    let access!: ReceiveStoreAccess
    const operation = storage.run(async stores => { access = stores; await stores.requireStore() })
    await opening.promise
    const closing = storage.close()
    finish.resolve()
    await expect(operation).rejects.toMatchObject({ name: 'NotAllowedError' })
    await closing
    await expect(access.requireStore()).rejects.toMatchObject({ name: 'AbortError' })
    expect(await access.openStoreOptional()).toBeUndefined()
    const late = vi.fn()
    await storage.run(late)
    expect(late).not.toHaveBeenCalled()
    expect(open).toHaveBeenCalledOnce()
    expect(f.close).not.toHaveBeenCalled()
  })

  it.each(['before-open', 'during-open'] as const)('preserves the paused record when the share changes %s', async timing => {
    const f = fixture(), base = pausedTask(f), error = vi.fn()
    let current: JoinedBrowserShare | undefined = f.share
    const receive = f.coordinator({ currentJoinedShare: () => current, onActionError: error })
    await f.memory.putTask(base)
    await receive.loadRetained()
    const put = f.memory.putTask.bind(f.memory)
    const write = vi.spyOn(f.memory, 'putTask').mockImplementation(async record => {
      if (record.lifecycle.kind === 'downloading') current = undefined
      await put(record)
    })
    const permission = vi.spyOn(f.parent, 'queryPermission')
    receive.performRetainedAction(receive.getSnapshot().retained.tasks[0]!, 'continue')
    if (timing === 'before-open') current = undefined
    await vi.waitFor(() => expect(error).toHaveBeenCalledOnce(), { interval: 1 })
    if (timing === 'before-open') {
      expect(permission).not.toHaveBeenCalled()
      expect(write).not.toHaveBeenCalled()
    }
    expect(f.memory.tasks.get(base.taskId)).toEqual(base)
    expect(f.startConnectivity).not.toHaveBeenCalled()
    expect(f.isLocked()).toBe(false)
    await receive.dispose()
  })

  it.each(['resolve', 'reject'] as const)('ignores a cancelled picker that later settles with %s', async outcome => {
    let grantFirst!: (handle: FileSystemDirectoryHandle) => void, rejectFirst!: (cause: Error) => void
    let grantSecond!: (handle: FileSystemDirectoryHandle) => void
    const first = new Promise<FileSystemDirectoryHandle>((resolve, reject) => { grantFirst = resolve; rejectFirst = reject })
    const second = new Promise<FileSystemDirectoryHandle>(resolve => { grantSecond = resolve })
    const picker = vi.fn<() => Promise<FileSystemDirectoryHandle>>().mockReturnValueOnce(first).mockReturnValueOnce(second)
    const f = fixture(picker), receive = f.coordinator()
    await f.start(receive)
    receive.cancelActivation()
    expect(receive.startDownload('fsa:file')).toBe(true)
    const folder = f.parent as unknown as FileSystemDirectoryHandle
    if (outcome === 'resolve') grantFirst(folder)
    else rejectFirst(new DOMException('Closed', 'AbortError'))
    await first.catch(() => undefined)
    expect(receive.getSnapshot().activation?.kind).toBe('waiting-picker')
    expect(f.memory.tasks.size).toBe(0)
    grantSecond(folder)
    await f.waiting.promise
    await receive.dispose()
    expect(f.startConnectivity).toHaveBeenCalledOnce()
  })

  it.each(['fsa', 'opfs'] as const)('drains an in-flight %s checkpoint and resumes only the missing bytes', async backend => {
    const f = fixture(), { env, root } = createMemoryOPFSEnv()
    const opfsEnv = { ...env, locks: f.locks, openNativeObject: async (handle: FileSystemFileHandle) => {
      const io = await env.openNativeObject!(handle)
      return { ...io, close: async () => { await io.close(); f.memory.events.push('io.close') } }
    } }
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: null,
    })
    const receive = f.coordinator({ opfsEnv })
    await f.start(receive, backend)
    await f.waiting.promise
    vi.useFakeTimers()
    const flushing = gate(), finish = gate()
    const putFile = f.memory.putFile.bind(f.memory)
    vi.spyOn(f.memory, 'putFile').mockImplementationOnce(async row => {
      flushing.resolve(); await finish.promise; await putFile(row)
    })
    f.connection.reconnecting({ kind: 'connecting' })
    await vi.advanceTimersByTimeAsync(DISCONNECT_CHECKPOINT_DELAY_MS)
    await flushing.promise
    const closing = receive.dispose()
    expect(f.isLocked()).toBe(true)
    expect(f.close).not.toHaveBeenCalled()
    finish.resolve()
    await closing
    const closingEvents = ['lock.release', 'connection.close', 'db.close']
    if (backend === 'opfs') closingEvents.unshift('io.close')
    expect(f.memory.events.slice(-closingEvents.length)).toEqual(closingEvents)
    expect(f.trace).toHaveBeenCalledWith(expect.objectContaining({ eventName: 'settlement',
      payload: expect.objectContaining({ transition: 'detached' }) }))
    vi.useRealTimers()
    f.connection.connected()
    f.reconnect()
    const reopened = f.coordinator({ opfsEnv })
    await reopened.loadRetained()
    const task = reopened.getSnapshot().retained.tasks[0]!
    expect(task.lifecycle).toMatchObject({ kind: 'paused', reason: 'interrupted', retainedBytes: 4n })
    reopened.performRetainedAction(task, 'continue')
    await vi.waitFor(() => expect(reopened.getSnapshot().lifecycle?.kind).toBe(backend === 'fsa' ? 'saved' : 'ready-to-save'), { interval: 1 })
    expect(f.source.readRanges).toEqual([{ start: 0n, end: 8n }, { start: 4n, end: 8n }])
    const file = backend === 'fsa' ? await f.parent.getFileHandle('data.bin')
      : await (await root.getDirectoryHandle('windshare-tasks')).getFileHandle(task.taskId + '.part')
    expect([...file.bytes]).toEqual([1, 2, 3, 4, 5, 6, 7, 8])
    await reopened.dispose()
  })

  it.each(['stop', 'pause'] as const)('preserves an accepted user %s on immediate disposal', async action => {
    const f = fixture(), receive = f.coordinator()
    await f.start(receive)
    await f.waiting.promise
    receive.performActiveAction(action)
    await receive.dispose()
    const record = [...f.memory.tasks.values()][0]!
    expect(record.lifecycle).toMatchObject(action === 'stop'
      ? { kind: 'failed', reason: 'cancelled' } : { kind: 'paused', reason: 'user', retainedBytes: 4n })
  })

  it('protects the settling task through lock release while other retained downloads remain actionable', async () => {
    const f = fixture(), receive = f.coordinator(), saving = gate(), finish = gate()
    const old = pausedTask(f)
    await f.memory.putTask(old)
    await receive.loadRetained()
    const other = receive.getSnapshot().retained.tasks[0]!
    const put = f.memory.putTask.bind(f.memory)
    vi.spyOn(f.memory, 'putTask').mockImplementation(async record => {
      if (record.lifecycle.kind === 'saved') { saving.resolve(); await finish.promise }
      await put(record)
    })
    f.reconnect()
    await f.start(receive)
    await saving.promise
    const taskId = receive.activeTaskId!
    expect(receive.getSnapshot().lifecycle).toMatchObject({ kind: 'downloading', phase: 'finalizing' })
    expect(receive.activeActionAdmission('pause').allowed).toBe(false)
    expect(receive.activeActionAdmission('stop').allowed).toBe(false)
    const release = f.holdRelease()
    finish.resolve()
    await vi.waitFor(() => expect(f.memory.tasks.get(taskId)?.lifecycle.kind).toBe('saved'), { interval: 1 })
    const same = { ...other, taskId, actions: ['forget'] as const }
    expect(receive.retainedActionAdmission(same, 'forget').allowed).toBe(false)
    expect(receive.activeActionAdmission('forget').allowed).toBe(false)
    expect(receive.retainedActionAdmission(other, 'discard').allowed).toBe(true)
    release.resolve()
    await vi.waitFor(() => expect(receive.activeActionAdmission('forget').allowed).toBe(true), { interval: 1 })
    await receive.dispose()
  })

  it('holds admission until the output lock is released even if connectivity fails before creating a job', async () => {
    const f = fixture(), connectivityTried = gate(), failed = gate(), release = f.holdRelease()
    let activeDuringErrorCallback = true
    let startBlockedDuringErrorCallback: string | null = 'initial'
    const receive = f.coordinator({
      onActionError: () => {
        activeDuringErrorCallback = receive.active
        startBlockedDuringErrorCallback = receive.startBlockedReason()
        failed.resolve()
      },
    })
    f.startConnectivity.mockImplementationOnce(() => {
      connectivityTried.resolve()
      throw new Error('connectivity unavailable')
    })
    await f.start(receive)
    await connectivityTried.promise
    await Promise.resolve()
    // While lock release is still pending, admission stays blocked and error has not dispatched yet.
    expect(receive.active).toBe(true)
    expect(receive.startBlockedReason()).toBe('downloading')
    expect(receive.activeActionAdmission('discard').allowed).toBe(false)
    release.resolve()
    await failed.promise
    expect(activeDuringErrorCallback).toBe(false)
    expect(startBlockedDuringErrorCallback).toBeNull()
    expect(receive.active).toBe(false)
    expect(receive.getSnapshot().lifecycle).toBeNull()
    await receive.dispose()
  })

  it('keeps portable output out of disconnect scheduling', async () => {
    const f = fixture(), receive = f.coordinator()
    await f.start(receive, 'portable')
    await f.waiting.promise
    assert(receive.active)
    expect(f.subscribeConnection).not.toHaveBeenCalled()
    await receive.dispose()
  })
})
