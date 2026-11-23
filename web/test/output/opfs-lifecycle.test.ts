import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import type { StartBackendTaskInput } from '../../src/output/backends/contract'
import {
  deleteOPFSTask, exportPartialOPFSZip, openNewOPFSSession, reopenOPFSSession, saveRetainedOPFSTask,
} from '../../src/output/backends/opfs'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import type { ReceiveStore } from '../../src/output/store/database'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState } from '../../src/transfer/state'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from './opfs-fixture'

const taskId = 'reliable-output'
const fileRequest = { fileId: 'f', path: ['f'], sourcePath: ['f'], revision: 'r', exactSize: 4n }
const bytes = Uint8Array.of(7, 8, 9, 10)
const completion: SettledTransferJobResult = {
  status: 'completed', completedFiles: 1n, completedBytes: 4n, discoveredFiles: 1n, discoveredBytes: 4n,
  discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
}

function fixture(artifact: StartBackendTaskInput['artifact'], wrapIO?: (io: NativeObjectIO) => NativeObjectIO) {
  const { env: native, root } = createMemoryOPFSEnv()
  const { windowPort, downloads } = createMockDownloadWindow()
  const memory = new MemoryReceiveStore()
  const store = memory as unknown as ReceiveStore
  const events: string[] = []
  let held = false
  const locks = { request: async (_name: string, _options: unknown, fn: (lock: object | null) => Promise<void>) => {
    if (held) return fn(null)
    held = true
    try { await fn({}) } finally { held = false; events.push('lock.release') }
  } } as unknown as LockManager
  const env = { ...native, locks, openNativeObject: async (handle: FileSystemFileHandle) => {
    const io = await native.openNativeObject!(handle)
    const traced = { ...io, close: async () => { await io.close(); events.push('native.close') } }
    return wrapIO?.(traced) ?? traced
  } }
  const ports = { env, store, windowPort }
  const task = {
    taskId, artifact, selectionKey: 'selection', lifecycle: initialReceiveState(taskId),
    selection: { shareInstance: 's', syntheticRoot: 'root', defaultSelected: true, rules: [] },
  }
  const directory = () => root.dirs.get('windshare-tasks')!
  const part = () => directory().files.get(taskId + '.part')
  return { ...ports, open: () => openNewOPFSSession({ ...ports, task }), directory, part, memory, events, downloads }
}

const zip = { kind: 'zip-archive' as const, name: 'bundle.zip' }
const single = { kind: 'original-file' as const, name: 'f', singleFileId: 'f' }

describe('OPFS object ownership and recovery', () => {
  it('closes the worker before releasing a failed reservation and preserves the paused owner when reopen fails', async () => {
    const f = fixture(zip)
    const error = new Error('database unavailable')
    vi.spyOn(f.memory, 'putTask').mockRejectedValueOnce(error)
    await expect(f.open()).rejects.toBe(error)
    expect(f.events).toEqual(['native.close', 'lock.release'])
    expect(f.part()).toBeUndefined()
    expect(await f.store.getTask(taskId)).toBeUndefined()

    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    const paused = await session.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session.close()
    vi.spyOn(f.memory, 'listTaskFiles').mockRejectedValueOnce(error)
    await expect(reopenOPFSSession({ ...f, task: paused })).rejects.toBe(error)
    expect(f.events.slice(-2)).toEqual(['native.close', 'lock.release'])
    expect(f.part()?.nativeOpen).toBe(false)
    expect(await f.store.getTask(taskId)).toEqual(paused)

    const reopened = await reopenOPFSSession({ ...f, task: paused })
    expect(await reopened.transferOutput.checkCompleted!('f', new AbortController().signal)).toEqual({ size: 4n })
    await reopened.close()
  })

  it('exports a committed single file even if later native flush and size calls would fail', async () => {
    let committed = false
    const f = fixture(single, io => ({
      ...io,
      flush: async () => {
        if (committed) throw new Error('seal flush failed')
        await io.flush()
      },
      size: async () => {
        if (committed) throw new Error('seal size failed')
        return io.size()
      },
    }))
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    committed = true
    const saved = await session.settle(completion)
    await session.close()
    expect(saved.lifecycle.kind).toBe('download-started')
    expect(new Uint8Array(await f.downloads[0]!.blob.arrayBuffer())).toEqual(bytes)
    const controller = new AbortController()
    const part = f.part()!
    const snapshot = await part.getFile()
    vi.spyOn(part, 'getFile').mockImplementationOnce(async () => { controller.abort(); return snapshot })
    await expect(saveRetainedOPFSTask(saved, f.store, f.env, f.windowPort, controller.signal))
      .rejects.toMatchObject({ name: 'AbortError' })
    expect(f.downloads).toHaveLength(1)
    part.bytes = bytes.slice(0, 1)
    await expect(saveRetainedOPFSTask(saved, f.store, f.env, f.windowPort)).rejects.toMatchObject({ name: 'NotFoundError' })
    expect(f.downloads).toHaveLength(1)
  })

  it('checks the closed file before handoff and resumes only physically present bytes', async () => {
    const f = fixture(single)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    f.part()!.bytes = bytes.slice(0, 2)
    const paused = await session.settle(completion)
    await session.close()
    expect(f.downloads).toHaveLength(0)
    expect(paused.lifecycle).toMatchObject({ kind: 'paused', reason: 'interrupted', completedFiles: 0n, retainedBytes: 2n })
    const resumed = await reopenOPFSSession({ ...f, task: paused })
    const fileAgain = await resumed.transferOutput.openFile(fileRequest, new AbortController().signal)
    expect(fileAgain.completedRanges).toEqual([byteRange(0n, 2n)])
    await fileAgain.writeAt(2n, bytes.slice(2))
    await fileAgain.commit([byteRange(0n, 4n)])
    await resumed.settle(completion)
    await resumed.close()
    expect(new Uint8Array(await f.downloads[0]!.blob.arrayBuffer())).toEqual(bytes)
  })

  it.each(['stopped', 'failed', 'source-changed'] as const)('reclaims unfinished private bytes after %s is durable', async status => {
    const f = fixture(single)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes.slice(0, 2))
    await file.checkpoint([byteRange(0n, 2n)], 'disconnect')
    const remove = f.directory().removeEntry.bind(f.directory())
    vi.spyOn(f.directory(), 'removeEntry').mockImplementation(async name => {
      expect((await f.store.getTask(taskId))?.lifecycle.kind).toBe('failed')
      expect(f.part()?.nativeOpen).toBe(false)
      await remove(name)
    })
    const record = await session.settle({ ...completion, status, completedFiles: 0n, completedBytes: 0n })
    await session.close()
    expect(f.part()).toBeUndefined()
    vi.mocked(f.directory().removeEntry).mockRestore()
    await deleteOPFSTask(record, f.store, f.env)
    expect(await f.store.getTask(taskId)).toBeUndefined()
  })

  it('preserves the record on a locked discard and retries a metadata failure after the object is gone', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    const paused = await session.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session.close()
    const part = f.part()!
    const independent = part.createNativeIO()
    await expect(deleteOPFSTask(paused, f.store, f.env)).rejects.toMatchObject({ name: 'NoModificationAllowedError' })
    expect(await f.store.getTask(taskId)).toEqual(paused)
    await independent.close()
    const controller = new AbortController()
    vi.spyOn(f.memory, 'listTaskFiles').mockImplementationOnce(async () => {
      controller.abort()
      return [...f.memory.files.values()]
    })
    await expect(exportPartialOPFSZip(paused, f.store, f.env, f.windowPort, controller.signal))
      .rejects.toMatchObject({ name: 'AbortError' })
    expect(f.downloads).toHaveLength(0)
    await expect(deleteOPFSTask(paused, f.store, f.env, controller.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect(f.part()).toBe(part)
    vi.spyOn(f.memory, 'deleteTask').mockRejectedValueOnce(new Error('transaction aborted'))
    await expect(deleteOPFSTask(paused, f.store, f.env)).rejects.toThrow('transaction aborted')
    expect(f.part()).toBeUndefined()
    expect(await f.store.getTask(taskId)).toEqual(paused)
    await deleteOPFSTask(paused, f.store, f.env)
    expect(await f.store.getTask(taskId)).toBeUndefined()
  })
})
