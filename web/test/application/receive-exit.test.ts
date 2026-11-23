import { afterEach, assert, describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import { DISCONNECT_CHECKPOINT_DELAY_MS } from '../../src/application/receive/disconnect-checkpoint-guard'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { ReceiverConnectionState } from '../../src/receiver/connection-state'
import { ReceiveStore } from '../../src/output/store/database'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { TransferJob, type TransferJobOptions } from '../../src/transfer/job'
import { FakeDirectoryHandle, MemoryReceiveStore } from '../output/fsa-fixture'
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
  let interrupted = true, held = false
  const locks = { request: async (_name: string, _options: unknown, cb: (lock: object | null) => Promise<void>) => {
    if (held) return cb(null)
    held = true
    try { await cb({}) } finally { held = false; memory.events.push('lock.release') }
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
  const coordinator = () => new ReceiveCoordinator({
    windowPort: { navigator: { locks }, indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker ?? (async () => parent as unknown as FileSystemDirectoryHandle),
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() }, document: { createElement: vi.fn() } as unknown as Document },
    currentJoinedShare: () => share, outputTrace: { current: trace },
  })
  const start = async (receive: ReceiveCoordinator, backend = 'fsa') => {
    receive.refreshOffers(share)
    await vi.waitFor(() => expect(receive.getSnapshot().offers?.kind).toBe('ready'), { interval: 1 })
    expect(receive.startDownload(backend + ':file')).toBe(true)
  }
  return { memory, close, source, parent, connection, subscribeConnection, startConnectivity, coordinator, waiting, start, trace,
    isLocked: () => held, reconnect: () => { interrupted = false } }
}

describe('receive session shutdown', () => {
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

  it.each(['new', 'continue'])('closes a late %s session under its lock before closing IndexedDB', async action => {
    const f = fixture(), receive = f.coordinator(), opening = gate(), finish = gate()
    const task = {
      taskId: 'resuming', selectionKey: 'key', selection: { shareInstance: f.source.descriptor.shareInstanceId,
        syntheticRoot: 'root', defaultSelected: true, rules: [] },
      shareInstance: f.source.descriptor.shareInstanceId, backend: 'fsa' as const,
      artifact: { kind: 'directory-tree' as const, name: 'data.bin', singleFileId: 'data' },
      target: { kind: 'fsa' as const, parentHandle: f.parent as unknown as FileSystemDirectoryHandle,
        reservedName: 'data.bin', collisionIndex: 0, singleFile: true },
      lifecycle: transitionReceiveState(initialReceiveState('resuming'), { kind: 'paused', reason: 'user',
        completedFiles: 0n, completedBytes: 0n, retainedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 8n, discoveryComplete: false }),
      discoveryComplete: false, updatedAt: 0,
    }
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

  it('drains an in-flight disk checkpoint and resumes only the missing bytes after reopening', async () => {
    const f = fixture(), receive = f.coordinator()
    await f.start(receive)
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
    expect(f.memory.events.slice(-3)).toEqual(['lock.release', 'connection.close', 'db.close'])
    expect(f.trace).toHaveBeenCalledWith(expect.objectContaining({ eventName: 'settlement',
      payload: expect.objectContaining({ transition: 'detached' }) }))
    vi.useRealTimers()
    f.connection.connected()
    f.reconnect()
    const reopened = f.coordinator()
    await reopened.loadRetained()
    const task = reopened.getSnapshot().retained.tasks[0]!
    expect(task.lifecycle).toMatchObject({ kind: 'paused', reason: 'interrupted', retainedBytes: 4n })
    reopened.performRetainedAction(task, 'continue')
    await vi.waitFor(() => expect(reopened.getSnapshot().lifecycle?.kind).toBe('saved'), { interval: 1 })
    expect(f.source.readRanges).toEqual([{ start: 0n, end: 8n }, { start: 4n, end: 8n }])
    expect([...(await f.parent.getFileHandle('data.bin')).bytes]).toEqual([1, 2, 3, 4, 5, 6, 7, 8])
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

  it('keeps portable output out of disconnect scheduling', async () => {
    const f = fixture(), receive = f.coordinator()
    await f.start(receive, 'portable')
    await f.waiting.promise
    assert(receive.active)
    expect(f.subscribeConnection).not.toHaveBeenCalled()
    await receive.dispose()
  })
})
