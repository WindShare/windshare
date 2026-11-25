import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { describe, expect, it, vi } from 'vitest'
import { admitRetainedAction, retainedTaskReadiness } from '../../src/application/receive/admission'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { executeRetainedTaskAction, loadRetainedTasks, reopenRetainedTask } from '../../src/application/receive/retained'
import { DirectoryFailureError } from '../../src/catalog/client'
import { CatalogPageStoreError } from '../../src/catalog/page-store'
import { DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT } from '../../src/catalog/records'
import { byteRange } from '../../src/content/geometry'
import type { BrowserOutputWindow, StartBackendTaskInput } from '../../src/output/backends/contract'
import {
  deleteOPFSTask, exportPartialOPFSZip, openNewOPFSSession, reopenOPFSSession, saveRetainedOPFSTask,
} from '../../src/output/backends/opfs'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import type { ReceiveStore } from '../../src/output/store/database'
import { buildPartialZipBlob } from '../../src/output/zip/progressive-zip'
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
  const traces: OutputTraceEvent[] = []
  const diagnostics = { backend: 'origin_private' as const, trace: { current: (event: OutputTraceEvent) => { traces.push(event) } } }
  const ports = { env, store, windowPort, diagnostics }
  const task = {
    taskId, artifact, selectionKey: 'selection', lifecycle: initialReceiveState(taskId),
    selection: { shareInstance: 's', syntheticRoot: 'root', defaultSelected: true, rules: [] },
  }
  const directory = () => root.dirs.get('windshare-tasks')!
  const part = () => directory().files.get(taskId + '.part')
  return { ...ports, open: () => openNewOPFSSession({ ...ports, task }), directory, part, memory, events, downloads, traces }
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
    expect(paused.target.kind === 'opfs' ? paused.target.sealedSize : null).toBeUndefined()
    const resumed = await reopenOPFSSession({ ...f, task: paused })
    const fileAgain = await resumed.transferOutput.openFile(fileRequest, new AbortController().signal)
    expect(fileAgain.completedRanges).toEqual([byteRange(0n, 2n)])
    await fileAgain.writeAt(2n, bytes.slice(2))
    await fileAgain.commit([byteRange(0n, 4n)])
    await resumed.settle(completion)
    await resumed.close()
    expect(new Uint8Array(await f.downloads[0]!.blob.arrayBuffer())).toEqual(bytes)
  })

  it.each(['stopped', 'failed', 'source-changed', 'source-busy'] as const)('reclaims unfinished private bytes after %s is durable', async status => {
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

  it('detaches small partial ZIP payloads before releasing the lock while keeping large exports zero-copy', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    const paused = await session.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session.close()

    const part = f.part()!
    const snapshot = await part.getFile()
    let sliceArrayBufferCalls = 0
    const spyFile = (source: File) => {
      const slice = source.slice.bind(source)
      vi.spyOn(source, 'slice').mockImplementation((...args) => {
        const sliced = slice(...args)
        const originalArrayBuffer = sliced.arrayBuffer.bind(sliced)
        vi.spyOn(sliced, 'arrayBuffer').mockImplementation(() => {
          sliceArrayBufferCalls += 1
          f.events.push('slice.arrayBuffer')
          return originalArrayBuffer()
        })
        return sliced
      })
      return source
    }
    vi.spyOn(part, 'getFile').mockResolvedValueOnce(spyFile(snapshot))
    f.events.length = 0

    const exported = await exportPartialOPFSZip(paused, f.store, f.env, f.windowPort)
    expect(sliceArrayBufferCalls).toBe(1)
    expect(f.events).toEqual(['slice.arrayBuffer', 'lock.release'])

    // Immediate deletion after partial export leaves the materialized ZIP Blob intact.
    await deleteOPFSTask(paused, f.store, f.env)
    const reader = new ZipReader(new BlobReader(exported), { checkSignature: true, useWebWorkers: false })
    try {
      const [entry] = await reader.getEntries()
      expect(entry?.filename).toBe('f')
      expect(await (entry as FileEntry).getData(new Uint8ArrayWriter())).toEqual(bytes)
    } finally {
      await reader.close()
    }

    // Payloads above the detach cap stay zero-copy without eagerly materializing slices.
    sliceArrayBufferCalls = 0
    const files = await f.store.listTaskFiles(taskId)
    expect(files).toHaveLength(0)
    const session2 = await f.open()
    const file2 = await session2.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file2.writeAt(0n, bytes)
    await file2.commit([byteRange(0n, 4n)])
    await session2.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session2.close()
    const largeSource = spyFile(await f.part()!.getFile())
    const largeFiles = await f.store.listTaskFiles(taskId)
    const zeroCopy = await buildPartialZipBlob(largeSource, largeFiles, { detachMaxBytes: 2n })
    expect(sliceArrayBufferCalls).toBe(0)
    expect(zeroCopy.size).toBe(exported.size)

    const aborted = new AbortController()
    aborted.abort()
    await expect(buildPartialZipBlob(largeSource, largeFiles, { signal: aborted.signal }))
      .rejects.toMatchObject({ name: 'AbortError' })
  })

  it('settles permanent catalog capacity errors into distinct partial and failed lifecycle reasons', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    const partial = await session.settle({
      ...completion,
      status: 'partial',
      discoveryComplete: false,
      failedDirectories: 1n,
      error: new CatalogPageStoreError('resource-limit', 'cache full'),
    })
    await session.close()
    expect(partial.lifecycle).toMatchObject({
      kind: 'partial',
      reason: 'browser-catalog-limit',
      savedFiles: 1n,
    })

    const f2 = fixture(zip)
    const session2 = await f2.open()
    const failed = await session2.settle({
      ...completion,
      status: 'failed',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 0n,
      discoveredBytes: 0n,
      discoveryComplete: false,
      failedDirectories: 1n,
      error: new DirectoryFailureError(Object.freeze({
        shareInstance: new Uint8Array(16).fill(1),
        directoryId: new Uint8Array(16).fill(2),
        directoryIdText: 'dir',
        attemptId: new Uint8Array(16).fill(3),
        attemptIdText: 'attempt',
        code: DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
        kind: 'resource-limit',
        retryable: false,
      })),
    })
    await session2.close()
    expect(failed.lifecycle).toMatchObject({
      kind: 'failed',
      reason: 'share-catalog-limit',
    })
    if (failed.lifecycle.kind === 'failed') expect(failed.lifecycle.detail).toBeUndefined()
  })

  it('seals single-file OPFS atomically on commit so abandoning before settle still exports offline', async () => {
    const f = fixture(single)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    // Simulate closing the tab after file commit and before settle()/publication runs.
    await session.close()

    const retainedEnv = {
      store: f.store,
      windowPort: f.windowPort as unknown as BrowserOutputWindow,
      opfsEnv: f.env,
      locks: f.env.locks,
    }
    const retained = await loadRetainedTasks(retainedEnv)
    expect(retained).toHaveLength(1)
    const snapshot = retained[0]!
    expect(snapshot.lifecycle).toMatchObject({ kind: 'ready-to-save', size: 4n, fileName: 'f' })
    expect(snapshot.actions).toEqual(['save', 'discard'])
    expect(retainedTaskReadiness(snapshot, null)).toBe('local')
    expect(admitRetainedAction(snapshot, 'save', {
      currentShareInstance: null,
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionBlock: 'shareEnded',
    })).toEqual({ allowed: true, reason: null })

    const outcome = await executeRetainedTaskAction(retainedEnv, taskId, 'save')
    expect(outcome.updated?.lifecycle.kind).toBe('download-started')
    expect(f.downloads).toHaveLength(1)
    expect(f.downloads[0]?.fileName).toBe('f')
    expect(new Uint8Array(await f.downloads[0]!.blob.arrayBuffer())).toEqual(bytes)
  })

  it('keeps committed single-file OPFS ready-to-save across late pause and concurrent discovery completion', async () => {
    const f = fixture(single)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)

    const putTaskSpy = vi.spyOn(f.memory, 'putTask')
    let atomicCommitStarted!: () => void
    const commitStarted = new Promise<void>(resolve => { atomicCommitStarted = resolve })
    let releaseAtomicCommit!: () => void
    const atomicCommitGate = new Promise<void>(resolve => { releaseAtomicCommit = resolve })
    const originalPutTaskAndFile = f.memory.putTaskAndFile.bind(f.memory)
    vi.spyOn(f.memory, 'putTaskAndFile').mockImplementationOnce(async (task, row) => {
      atomicCommitStarted()
      await atomicCommitGate
      await originalPutTaskAndFile(task, row)
    })

    const committing = file.commit([byteRange(0n, 4n)])
    await commitStarted
    const marking = session.transferOutput.markDiscoveryComplete?.()
    releaseAtomicCommit()
    await Promise.all([committing, marking])

    // Concurrent markDiscoveryComplete must serialize after commit and skip redundant writes.
    expect(putTaskSpy).not.toHaveBeenCalled()
    expect((await f.store.getTask(taskId))?.lifecycle.kind).toBe('ready-to-save')

    const settled = await session.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session.close()
    expect(settled.lifecycle).toMatchObject({ kind: 'ready-to-save', size: 4n, fileName: 'f' })
    expect(f.downloads).toHaveLength(0)

    await saveRetainedOPFSTask(settled, f.store, f.env, f.windowPort)
    expect(f.downloads).toHaveLength(1)
    expect(new Uint8Array(await f.downloads[0]!.blob.arrayBuffer())).toEqual(bytes)

    // Reverse race: markDiscoveryComplete starts first and stays in-flight while commit runs.
    const f2 = fixture(single)
    const session2 = await f2.open()
    const file2 = await session2.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file2.writeAt(0n, bytes)
    let discoveryStarted!: () => void
    const discoveryRunning = new Promise<void>(resolve => { discoveryStarted = resolve })
    let releaseDiscovery!: () => void
    const discoveryGate = new Promise<void>(resolve => { releaseDiscovery = resolve })
    const originalPutTask = f2.memory.putTask.bind(f2.memory)
    vi.spyOn(f2.memory, 'putTask').mockImplementationOnce(async record => {
      discoveryStarted()
      await discoveryGate
      await originalPutTask(record)
    })
    const marking2 = session2.transferOutput.markDiscoveryComplete?.()
    await discoveryRunning
    const committing2 = file2.commit([byteRange(0n, 4n)])
    releaseDiscovery()
    await Promise.all([marking2, committing2])
    await session2.close()
    expect((await f2.store.getTask(taskId))?.lifecycle.kind).toBe('ready-to-save')
  })
})

describe('OPFS completed publication durability', () => {
  it('persists ready-to-save before ZIP handoff and keeps offline save intact if download-started persistence fails', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])

    const order: string[] = []
    const originalPutTask = f.memory.putTask.bind(f.memory)
    vi.spyOn(f.memory, 'putTask').mockImplementation(async record => {
      order.push(`putTask:${record.lifecycle.kind}`)
      await originalPutTask(record)
    })
    const originalCreateObjectURL = f.windowPort.URL!.createObjectURL.bind(f.windowPort.URL)
    vi.spyOn(f.windowPort.URL!, 'createObjectURL').mockImplementation(blob => {
      order.push('handoff')
      return originalCreateObjectURL(blob)
    })

    const settled = await session.settle(completion)
    await session.close()
    expect(settled.lifecycle.kind).toBe('download-started')
    expect(order).toEqual(['putTask:ready-to-save', 'handoff', 'putTask:download-started'])

    // If persisting `download-started` fails after handoff, the durable `ready-to-save` record
    // stays intact so the download does not fail and offline save still exports the complete ZIP.
    const f2 = fixture(zip)
    const session2 = await f2.open()
    const file2 = await session2.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file2.writeAt(0n, bytes)
    await file2.commit([byteRange(0n, 4n)])
    const originalPutTask2 = f2.memory.putTask.bind(f2.memory)
    vi.spyOn(f2.memory, 'putTask').mockImplementation(async record => {
      if (record.lifecycle.kind === 'download-started') {
        throw new DOMException('quota exceeded', 'QuotaExceededError')
      }
      await originalPutTask2(record)
    })

    const settled2 = await session2.settle(completion)
    await session2.close()
    expect(settled2.lifecycle.kind).toBe('ready-to-save')
    expect(f2.downloads).toHaveLength(1)
    expect(f2.traces.filter(event => event.eventName === 'publication').map(event => event.payload.transition))
      .toEqual(['ready_to_save', 'committed', 'handoff_persist_failed'])

    const retainedEnv = {
      store: f2.store,
      windowPort: f2.windowPort as unknown as BrowserOutputWindow,
      opfsEnv: f2.env,
      locks: f2.env.locks,
    }
    const [snapshot] = await loadRetainedTasks(retainedEnv)
    expect(snapshot?.lifecycle.kind).toBe('ready-to-save')
    expect(snapshot?.actions).toEqual(['save', 'discard'])
    expect(retainedTaskReadiness(snapshot!, null)).toBe('local')

    vi.mocked(f2.memory.putTask).mockRestore()
    const exported = await executeRetainedTaskAction(retainedEnv, taskId, 'save')
    expect(exported.updated?.lifecycle.kind).toBe('download-started')
    expect(f2.downloads).toHaveLength(2)
    expect(f2.downloads[1]?.fileName).toBe('bundle.zip')
    const reader = new ZipReader(new BlobReader(f2.downloads[1]!.blob), { checkSignature: true, useWebWorkers: false })
    try {
      const [entry] = await reader.getEntries()
      expect(entry?.filename).toBe('f')
      expect(await (entry as FileEntry).getData(new Uint8ArrayWriter())).toEqual(bytes)
    } finally {
      await reader.close()
    }
  })

  it('still hands off a verified ZIP when ready-to-save persistence fails and skips redundant single-file writes', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])

    const quotaError = new DOMException('quota exceeded', 'QuotaExceededError')
    vi.spyOn(f.memory, 'putTask').mockRejectedValueOnce(quotaError)
    await expect(session.settle(completion)).rejects.toBe(quotaError)
    await session.close()

    expect(f.downloads).toHaveLength(1)
    expect(f.downloads[0]?.fileName).toBe('bundle.zip')
    const reader = new ZipReader(new BlobReader(f.downloads[0]!.blob), { checkSignature: true, useWebWorkers: false })
    try {
      const [entry] = await reader.getEntries()
      expect(entry?.filename).toBe('f')
      expect(await (entry as FileEntry).getData(new Uint8ArrayWriter())).toEqual(bytes)
    } finally {
      await reader.close()
    }
    expect(f.traces.filter(event => event.eventName === 'publication').map(event => event.payload.transition))
      .toEqual(['ready_to_save_persist_failed', 'committed'])

    // Single-file OPFS already seals `ready-to-save` during commit, so settle only persists `download-started`.
    const fSingle = fixture(single)
    const singleSession = await fSingle.open()
    const singleFile = await singleSession.transferOutput.openFile(fileRequest, new AbortController().signal)
    await singleFile.writeAt(0n, bytes)
    await singleFile.commit([byteRange(0n, 4n)])
    const persistedKinds: string[] = []
    const originalSinglePut = fSingle.memory.putTask.bind(fSingle.memory)
    vi.spyOn(fSingle.memory, 'putTask').mockImplementation(async record => {
      persistedKinds.push(record.lifecycle.kind)
      await originalSinglePut(record)
    })

    const settledSingle = await singleSession.settle(completion)
    await singleSession.close()
    expect(settledSingle.lifecycle.kind).toBe('download-started')
    expect(persistedKinds).toEqual(['download-started'])
  })
})

describe('OPFS cross-tab reopen and export authority', () => {
  it('prevents cross-tab reopen or export from resurrecting a deleted or settled OPFS task', async () => {
    const f = fixture(zip)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 4n)])
    const stalePaused = await session.settle({ ...completion, status: 'paused', pauseReason: 'user' })
    await session.close()

    // Race 1: Tab A reads the paused task via reopenRetainedTask; Tab B deletes it before Tab A takes the lock.
    const joined = { descriptor: { shareInstanceId: 's' } } as unknown as JoinedBrowserShare
    const retainedEnv = {
      store: f.store,
      windowPort: f.windowPort as unknown as BrowserOutputWindow,
      opfsEnv: f.env,
      locks: f.env.locks,
    }
    const originalGetTask = f.memory.getTask.bind(f.memory)
    let deletedInterleaved = false
    vi.spyOn(f.memory, 'getTask').mockImplementation(async id => {
      const record = await originalGetTask(id)
      if (!deletedInterleaved) {
        deletedInterleaved = true
        await deleteOPFSTask(stalePaused, f.store, f.env)
      }
      return record
    })
    f.events.length = 0
    await expect(reopenRetainedTask(retainedEnv, taskId, () => joined)).rejects.toMatchObject({ name: 'NotFoundError' })
    expect(f.part()).toBeUndefined()
    expect(await originalGetTask(taskId)).toBeUndefined()
    expect(f.events).toEqual(['lock.release', 'lock.release'])
    vi.mocked(f.memory.getTask).mockRestore()

    // Race 2: Tab B settles the task to ready-to-save/download-started before Tab A acquires the lock.
    const f2 = fixture(single)
    const s2 = await f2.open()
    const w2 = await s2.transferOutput.openFile(fileRequest, new AbortController().signal)
    await w2.writeAt(0n, bytes.slice(0, 2))
    await w2.pause([byteRange(0n, 2n)])
    const staleSingle = await s2.settle({ ...completion, status: 'paused', pauseReason: 'user', completedFiles: 0n, completedBytes: 0n })
    await s2.close()

    const tabB = await reopenOPFSSession({ ...f2, task: staleSingle })
    const wb = await tabB.transferOutput.openFile(fileRequest, new AbortController().signal)
    await wb.writeAt(2n, bytes.slice(2))
    await wb.commit([byteRange(0n, 4n)])
    const settled = await tabB.settle(completion)
    await tabB.close()
    expect(settled.lifecycle.kind).toBe('download-started')

    await expect(reopenOPFSSession({ ...f2, task: staleSingle })).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(await f2.store.getTask(taskId)).toEqual(settled)

    // Race 3: Tab B deletes a sealed task before saveRetainedOPFSTask acquires the lock.
    await deleteOPFSTask(settled, f2.store, f2.env)
    const downloadsBefore = f2.downloads.length
    await expect(saveRetainedOPFSTask(settled, f2.store, f2.env, f2.windowPort)).rejects.toMatchObject({ name: 'NotFoundError' })
    expect(f2.downloads).toHaveLength(downloadsBefore)
    expect(await f2.store.getTask(taskId)).toBeUndefined()
  })

  it('reuses the authoritative locked record on cross-tab update and still recovers when OPFS storage was evicted', async () => {
    const f = fixture(single)
    const session = await f.open()
    const file = await session.transferOutput.openFile(fileRequest, new AbortController().signal)
    await file.writeAt(0n, bytes.slice(0, 2))
    await file.pause([byteRange(0n, 2n)])
    const olderPaused = await session.settle({
      ...completion, status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveryComplete: false,
    })
    await session.close()

    const newerPaused = Object.freeze({
      ...olderPaused,
      discoveryComplete: true,
      lifecycle: Object.freeze({ ...olderPaused.lifecycle, generation: olderPaused.lifecycle.generation + 2n }),
    })
    const joined = { descriptor: { shareInstanceId: 's' } } as unknown as JoinedBrowserShare
    const retainedEnv = {
      store: f.store,
      windowPort: f.windowPort as unknown as BrowserOutputWindow,
      opfsEnv: f.env,
      locks: f.env.locks,
    }
    const originalGetTask = f.memory.getTask.bind(f.memory)
    let updatedInterleaved = false
    vi.spyOn(f.memory, 'getTask').mockImplementation(async id => {
      const record = await originalGetTask(id)
      if (!updatedInterleaved) {
        updatedInterleaved = true
        await f.store.putTask(newerPaused)
      }
      return record
    })
    const reopened = await reopenRetainedTask(retainedEnv, taskId, () => joined)
    expect(reopened.task.discoveryComplete).toBe(true)
    expect(reopened.task.lifecycle.generation).toBe(newerPaused.lifecycle.generation + 1n)
    expect((await originalGetTask(taskId))?.discoveryComplete).toBe(true)
    await reopened.session.close()
    vi.mocked(f.memory.getTask).mockRestore()

    // Browser storage eviction removes the .part object while the IDB task row remains paused; reopen must still recover.
    const pausedAfter = await f.store.getTask(taskId)
    await f.directory().removeEntry(`${taskId}.part`)
    expect(f.part()).toBeUndefined()
    const recovered = await reopenOPFSSession({ ...f, task: pausedAfter! })
    const recoveredFile = await recovered.transferOutput.openFile(fileRequest, new AbortController().signal)
    expect(recoveredFile.completedRanges).toEqual([])
    expect(f.part()).toBeDefined()
    await recovered.close()
  })
})
