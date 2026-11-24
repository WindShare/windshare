import { afterEach, describe, expect, it, vi } from 'vitest'
import type { BrowseDirectory, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import { retainedSnapshot } from '../../src/application/receive/retained'
import { ReceiveBlockedError } from '../../src/application/receive/single-file'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import * as outputContract from '../../src/output/backends/contract'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { ReceiveStore } from '../../src/output/store/database'
import { taskLockName } from '../../src/output/store/tab-lock'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'
import {
  deferred,
  makeFileEntry,
  makeJobResult,
  makeSubDirectory as makeDir,
  singleFileSelectionKey as singleFileSpec,
  waitForSnapshot,
} from './receive-coordinator-fixture'

function makeFakeLockManager() {
  const held = new Set<string>()
  const locks = {
    request: async (
      name: string,
      options: LockOptions,
      callback: (lock: Lock | null) => Promise<unknown>,
    ) => {
      if (held.has(name)) {
        if (options.ifAvailable) return callback(null)
        throw new Error(`Unexpected blocking lock wait for ${name}`)
      }
      held.add(name)
      try {
        return await callback({ name, mode: options.mode ?? 'exclusive' })
      } finally {
        held.delete(name)
      }
    },
  } as unknown as LockManager
  return {
    locks,
    holdExternal: (taskId: string) => { held.add(taskLockName(taskId)) },
    releaseExternal: (taskId: string) => { held.delete(taskLockName(taskId)) },
    isHeld: (taskId: string) => held.has(taskLockName(taskId)),
  }
}

function makeLifecycle(
  taskId: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  lifecycleKind: 'ready-to-save' | 'paused' | 'downloading',
  createdAt: number,
) {
  const base = initialReceiveState(taskId, createdAt)
  switch (lifecycleKind) {
    case 'ready-to-save':
      return transitionReceiveState(base, { kind: 'ready-to-save', size: entry.expectedSize, fileName: entry.name }, createdAt)
    case 'paused':
      return transitionReceiveState(base, {
        kind: 'paused', reason: 'user',
        completedFiles: 0n, completedBytes: 0n, retainedBytes: 1n,
        discoveredFiles: 1n, discoveredBytes: entry.expectedSize, discoveryComplete: true,
      }, createdAt)
    case 'downloading':
      return transitionReceiveState(base, { kind: 'downloading', phase: 'receiving' }, createdAt)
  }
}

function makeOpfsRecord(
  taskId: string,
  shareInstance: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  lifecycleKind: 'ready-to-save' | 'paused' | 'downloading',
  createdAt = 1_000,
): ReceiveTaskRecord {
  const { spec, key } = singleFileSpec(shareInstance, 'root-1', entry, directory)
  const lifecycle = makeLifecycle(taskId, entry, lifecycleKind, createdAt)
  return {
    taskId,
    shareInstance,
    selectionKey: key,
    selection: spec,
    artifact: { kind: 'original-file', name: entry.name, singleFileId: entry.idText, singleFileSize: entry.expectedSize },
    backend: 'opfs',
    lifecycle,
    target: {
      kind: 'opfs',
      objectName: `${taskId}.part`,
      fileName: entry.name,
      isZip: false,
      allocatedLength: entry.expectedSize,
      ...(lifecycleKind === 'ready-to-save' ? { sealedSize: entry.expectedSize } : {}),
    },
    display: { objectLabel: entry.name, createdAtMilliseconds: createdAt },
    discoveryComplete: true,
    updatedAt: createdAt,
  }
}

function makeJoinedShare(
  shareInstanceId: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  overrides: Partial<JoinedBrowserShare> = {},
): JoinedBrowserShare {
  return {
    descriptor: { shareInstanceId, syntheticRootId: 'root-1' },
    selection: new SelectionPolicy(true),
    inspectSelection: async () => ({
      kind: 'single-file' as const,
      file: entry,
      sourcePath: [entry.name],
      exactBytes: entry.expectedSize,
    }),
    beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
    subscribeConnection: () => () => undefined,
    createTransferJob: () => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: async () => ({
        status: 'completed' as const,
        completedFiles: 1n,
        completedBytes: entry.expectedSize,
        discoveredFiles: 1n,
        discoveredBytes: entry.expectedSize,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      }),
    }),
    ...overrides,
  } as unknown as JoinedBrowserShare
}

describe('receive inventory lookup and concurrency invariants', () => {
  afterEach(() => vi.restoreAllMocks())

  it('degrades cleanly on share index preload failure without FSA probing loop (#1)', async () => {
    const file = makeFileEntry(1, 'movie.mkv', 100n * 1024n * 1024n)
    const dir = makeDir('root-1', 2, 'Media')
    const store = new MemoryReceiveStore()
    const listSpy = vi.spyOn(store, 'listTasksByShare').mockRejectedValue(new Error('IDB transaction failed'))
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))

    let pickerOpenedSync = false
    const picker = deferred<FileSystemDirectoryHandle>()
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => {
        pickerOpenedSync = true
        return picker.promise
      }),
    }
    const joined = makeJoinedShare('share-1', file)
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready' && s.singleFilePickerPendingFromBytes === null)
    expect(listSpy).toHaveBeenCalledTimes(1)

    // Clicking FSA single-file must NOT reset failed index back to loading; picker opens synchronously.
    const outcome = coordinator.startSingleFileDownload(file, dir)
    expect(outcome).toEqual({ kind: 'waiting-picker' })
    expect(pickerOpenedSync).toBe(true)
    expect(listSpy).toHaveBeenCalledTimes(1)

    coordinator.cancelActivation()
    picker.resolve({} as FileSystemDirectoryHandle)
    await coordinator.dispose()
  })

  it('prunes vanished or terminal stale downloading records on lookup so FSA next click opens picker (#2)', async () => {
    const fileDeleted = makeFileEntry(3, 'iso.img', 100n * 1024n * 1024n)
    const fileSettled = makeFileEntry(24, 'settled.img', 100n * 1024n * 1024n)
    const dir = makeDir('root-1', 4, 'ISOs')
    const store = new MemoryReceiveStore()
    const staleDeleted = makeOpfsRecord('stale-dl', 'share-1', fileDeleted, dir, 'downloading', 1_000)
    const staleSettled = makeOpfsRecord('stale-terminal', 'share-1', fileSettled, dir, 'downloading', 2_000)
    await store.putTask(staleDeleted)
    await store.putTask(staleSettled)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))

    const pickers = [deferred<FileSystemDirectoryHandle>(), deferred<FileSystemDirectoryHandle>()]
    let pickerIndex = 0
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => pickers[pickerIndex++]!.promise),
    }
    const joined = makeJoinedShare('share-1', fileDeleted)
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready' && s.singleFilePickerPendingFromBytes === null)

    // Case A: another tab deleted the stale downloading task before click.
    await store.deleteTask('stale-dl')
    const deletedErrors: unknown[] = []
    const deletedErrorDone = deferred<void>()
    expect(coordinator.startSingleFileDownload(fileDeleted, dir, {
      onAsyncError: err => { deletedErrors.push(err); deletedErrorDone.resolve() },
    })).toEqual({ kind: 'started' })
    await deletedErrorDone.promise
    expect(deletedErrors[0]).toBeInstanceOf(ReceiveBlockedError)
    expect((deletedErrors[0] as ReceiveBlockedError).reason).toBe('actionUnavailable')

    expect(coordinator.startSingleFileDownload(fileDeleted, dir)).toEqual({ kind: 'waiting-picker' })
    coordinator.cancelActivation()

    // Case B: another tab settled the stale downloading task into terminal `saved` state before click.
    await store.putTask({
      ...staleSettled,
      lifecycle: transitionReceiveState(staleSettled.lifecycle, {
        kind: 'saved',
        savedFiles: 1n,
        savedBytes: fileSettled.expectedSize,
      }, 3_000),
      updatedAt: 3_000,
    })
    const terminalErrors: unknown[] = []
    const terminalErrorDone = deferred<void>()
    expect(coordinator.startSingleFileDownload(fileSettled, dir, {
      onAsyncError: err => { terminalErrors.push(err); terminalErrorDone.resolve() },
    })).toEqual({ kind: 'started' })
    await terminalErrorDone.promise
    expect(terminalErrors[0]).toBeInstanceOf(ReceiveBlockedError)
    expect((terminalErrors[0] as ReceiveBlockedError).reason).toBe('actionUnavailable')

    // Resolved terminal state is written back so the second click synchronously launches the FSA picker.
    expect(coordinator.startSingleFileDownload(fileSettled, dir)).toEqual({ kind: 'waiting-picker' })
    coordinator.cancelActivation()
    for (const picker of pickers) picker.resolve({} as FileSystemDirectoryHandle)
    await coordinator.dispose()
  })

  it('recovers without deadlocking pending when openStoreOptional fails once and requireStore succeeds (#3)', async () => {
    const file = makeFileEntry(5, 'doc.pdf', 8n)
    const dir = makeDir('root-1', 6, 'Docs')
    const store = new MemoryReceiveStore()
    const record = makeOpfsRecord('task-retry-store', 'share-1', file, dir, 'ready-to-save', 1_000)
    await store.putTask(record)

    let opens = 0
    vi.spyOn(ReceiveStore, 'open').mockImplementation(async () => {
      opens += 1
      if (opens === 1) throw new Error('transient first open failure')
      return Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore
    })

    const { env } = createMemoryOPFSEnv()
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => makeJoinedShare('share-1', file),
    })

    // First open inside openStoreOptional fails; requireStore retries and completes discard cleanly.
    coordinator.performRetainedAction(retainedSnapshot(record, false), 'discard')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.retained.kind === 'empty')
    expect(opens).toBeGreaterThanOrEqual(2)
    expect(coordinator.startBlockedReason()).toBeNull()
    expect(store.tasks.size).toBe(0)

    await coordinator.dispose()
  })

  it('prevents stale out-of-order loads from resurrecting tasks, preserves in-flight mutations, and allows local actions during active downloads (#4, #15)', async () => {
    const fileA = makeFileEntry(7, 'active.bin', 16n)
    const fileB = makeFileEntry(8, 'other.bin', 8n)
    const dir = makeDir('root-1', 9, 'Files')
    const store = new MemoryReceiveStore()
    const otherRecord = makeOpfsRecord('other-retained', 'share-1', fileB, dir, 'ready-to-save', 1_000)
    await store.putTask(otherRecord)

    const { env, root } = createMemoryOPFSEnv()
    const tasksDir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    const opfsFile = await tasksDir.getFileHandle('other-retained.part', { create: true })
    opfsFile.bytes = new Uint8Array(8)

    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))

    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }

    const jobGate = deferred<ReturnType<typeof makeJobResult>>()
    const joined = makeJoinedShare('share-1', fileA, {
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: () => jobGate.promise,
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })

    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => joined,
    })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')
    await coordinator.loadRetained()
    expect(coordinator.getSnapshot().retained.tasks.map(t => t.taskId)).toEqual(['other-retained'])

    // Start an active download on fileA.
    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'started' })
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'downloading')

    // Start a slow loadAll() before discarding other-retained; it must NOT resurrect other-retained or clear pending.
    const slowLoadGate = deferred<readonly ReceiveTaskRecord[]>()
    const realListTasks = store.listTasks.bind(store)
    vi.spyOn(store, 'listTasks').mockImplementationOnce(() => slowLoadGate.promise)
    const slowLoad = coordinator.loadRetained()

    // Orthogonal local action (`discard`) on a DIFFERENT retained task succeeds WHILE fileA is running!
    const otherTask = coordinator.getSnapshot().retained.tasks[0]!
    expect(coordinator.retainedActionAdmission(otherTask, 'discard').allowed).toBe(true)
    coordinator.performRetainedAction(otherTask, 'discard')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.retained.tasks.length === 0)
    expect(coordinator.getSnapshot().lifecycle?.kind).toBe('downloading')

    // Resolve the stale slow load with the deleted record — loadEpoch discards it cleanly.
    vi.spyOn(store, 'listTasks').mockImplementation(realListTasks)
    slowLoadGate.resolve([otherRecord])
    await slowLoad
    expect(coordinator.getSnapshot().retained.tasks).toHaveLength(0)

    jobGate.resolve(makeJobResult('completed', 16n))
    await waitForSnapshot(coordinator, s => s.activeTaskId === null)
    await coordinator.dispose()
  })

  it('keeps pending local and resuming actions intact when concurrent loadAll fails (#4)', async () => {
    const fileA = makeFileEntry(25, 'local.bin', 8n)
    const fileB = makeFileEntry(26, 'paused.bin', 16n)
    const dir = makeDir('root-1', 27, 'Work')
    const store = new MemoryReceiveStore()
    const localRecord = makeOpfsRecord('local-task', 'share-1', fileA, dir, 'ready-to-save', 1_000)
    const pausedRecord = makeOpfsRecord('paused-task', 'share-1', fileB, dir, 'paused', 2_000)
    await store.putTask(localRecord)
    await store.putTask(pausedRecord)

    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { env } = createMemoryOPFSEnv()
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const joined = makeJoinedShare('share-1', fileA)
    const coordinator = new ReceiveCoordinator({ windowPort, opfsEnv: env, currentJoinedShare: () => joined })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')
    await coordinator.loadRetained()

    // Part A: in-flight local action (`discard`) survives a failing concurrent loadAll() without dropping pending.
    const localReadStarted = deferred<void>()
    const localReadGate = deferred<ReceiveTaskRecord | undefined>()
    const getSpy = vi.spyOn(store, 'getTask').mockImplementationOnce(async () => {
      localReadStarted.resolve()
      return localReadGate.promise
    })
    const localTask = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'local-task')!
    coordinator.performRetainedAction(localTask, 'discard')
    await localReadStarted.promise
    expect(coordinator.getSnapshot().retained.pending).toEqual({ taskId: 'local-task', action: 'discard' })

    vi.spyOn(store, 'listTasks').mockRejectedValueOnce(new Error('concurrent scan failed'))
    await coordinator.loadRetained()
    expect(coordinator.getSnapshot().retained.kind).toBe('failed')
    expect(coordinator.getSnapshot().retained.pending).toEqual({ taskId: 'local-task', action: 'discard' })
    expect(coordinator.startBlockedReason()).toBe('busy')
    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'blocked', reason: 'busy' })

    localReadGate.resolve(localRecord)
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.retained.kind === 'available')
    expect(coordinator.getSnapshot().retained.tasks.map(t => t.taskId)).toEqual(['paused-task'])

    // Part B: in-flight transfer resume (`continue`) survives a failing concurrent loadAll() and still blocks new downloads.
    const resumeReadStarted = deferred<void>()
    const resumeReadGate = deferred<ReceiveTaskRecord | undefined>()
    getSpy.mockImplementationOnce(async () => {
      resumeReadStarted.resolve()
      return resumeReadGate.promise
    })
    const resumeErr = deferred<unknown>()
    const pausedTask = coordinator.getSnapshot().retained.tasks[0]!
    coordinator.performRetainedAction(pausedTask, 'continue', { onError: err => resumeErr.resolve(err) })
    await resumeReadStarted.promise
    expect(coordinator.getSnapshot().retained.pending).toEqual({ taskId: 'paused-task', action: 'continue' })

    vi.spyOn(store, 'listTasks').mockRejectedValueOnce(new Error('concurrent scan failed during resume'))
    await coordinator.loadRetained()
    expect(coordinator.getSnapshot().retained.kind).toBe('failed')
    expect(coordinator.getSnapshot().retained.pending).toEqual({ taskId: 'paused-task', action: 'continue' })
    expect(coordinator.startBlockedReason()).toBe('busy')
    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'blocked', reason: 'busy' })

    resumeReadGate.reject(new DOMException('removed elsewhere', 'NotFoundError'))
    await resumeErr.promise
    expect(coordinator.getSnapshot().retained.pending).toBeNull()
    expect(coordinator.startBlockedReason()).toBeNull()
    await coordinator.dispose()
  })
})

describe('receive slot lifecycle, cross-tab locks, and disposal invariants', () => {
  afterEach(() => vi.restoreAllMocks())

  it('handles resolveSingleFileAsync errors, share change, and dispose cleanly without leaking slot (#5, #14)', async () => {
    const file = makeFileEntry(10, 'clip.mp4', 16n)
    const dir = makeDir('root-1', 11, 'Clips')
    const store = new MemoryReceiveStore()
    const preloadGate = deferred<readonly ReceiveTaskRecord[]>()
    vi.spyOn(store, 'listTasksByShare').mockReturnValue(preloadGate.promise)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))

    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const shareA = makeJoinedShare('share-a', file)
    const shareB = makeJoinedShare('share-b', file)
    let activeShare: JoinedBrowserShare | undefined = shareA
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => activeShare })
    coordinator.preloadShareTasks('share-a')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    // Start async lookup, then switch currentJoinedShare without calling suspendForNewShare.
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'started' })
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(file.idText)
    activeShare = shareB
    preloadGate.resolve([])
    await waitForSnapshot(coordinator, s => s.activation === null && s.activeSingleFileId === null)
    expect(coordinator.startBlockedReason()).toBeNull()

    // Non-abort error inside resolveSingleFileAsync releases the slot before reporting to onAsyncError.
    const staleRec = makeOpfsRecord('stale-2', 'share-b', file, dir, 'downloading', 1_000)
    await store.putTask(staleRec)
    vi.spyOn(store, 'listTasksByShare').mockResolvedValue([staleRec])
    const getSpy = vi.spyOn(store, 'getTask').mockRejectedValueOnce(new Error('IDB read failed'))
    coordinator.suspendForNewShare()
    coordinator.preloadShareTasks('share-b')
    await waitForSnapshot(coordinator, s => s.singleFilePickerPendingFromBytes === null)
    await Promise.resolve()

    const lookupErr = deferred<unknown>()
    let blockedDuringError: string | null = 'unset'
    let activationDuringError: unknown = 'unset'
    expect(coordinator.startSingleFileDownload(file, dir, {
      onAsyncError: err => {
        blockedDuringError = coordinator.startBlockedReason()
        activationDuringError = coordinator.getSnapshot().activation
        lookupErr.resolve(err)
      },
    })).toEqual({ kind: 'started' })
    expect((await lookupErr.promise as Error).message).toBe('IDB read failed')
    expect(blockedDuringError).toBeNull()
    expect(activationDuringError).toBeNull()
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()

    // Dispose while a second lookup is in flight: activeSingleFileId clears and no unhandled rejection occurs.
    const getGate = deferred<ReceiveTaskRecord | undefined>()
    getSpy.mockReturnValueOnce(getGate.promise)
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'started' })
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(file.idText)

    const disposing = coordinator.dispose()
    expect(coordinator.getSnapshot().activeTaskId).toBeNull()
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()
    getGate.resolve(staleRec)
    await disposing

    // Dispose while a transfer is actively running: BOTH activeTaskId and activeSingleFileId transition from non-null to null.
    const runningGate = deferred<Awaited<ReturnType<ReturnType<JoinedBrowserShare['createTransferJob']>['run']>>>()
    const detachJob = vi.fn(() => {
      runningGate.resolve({
        status: 'detached',
        completedFiles: 0n,
        completedBytes: 0n,
        discoveredFiles: 1n,
        discoveredBytes: file.expectedSize,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      })
    })
    const runningShare = makeJoinedShare('share-c', file, {
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: detachJob,
        run: () => runningGate.promise,
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })
    const runningCoordinator = new ReceiveCoordinator({
      windowPort: baseWin,
      currentJoinedShare: () => runningShare,
    })
    runningCoordinator.preloadShareTasks('share-c')
    await waitForSnapshot(runningCoordinator, s => s.capabilitiesState === 'ready')
    expect(runningCoordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'started' })
    await waitForSnapshot(runningCoordinator, s => s.lifecycle?.kind === 'downloading')
    expect(runningCoordinator.getSnapshot().activeTaskId).not.toBeNull()
    expect(runningCoordinator.getSnapshot().activeSingleFileId).toBe(file.idText)

    await runningCoordinator.dispose()
    expect(detachJob).toHaveBeenCalledTimes(1)
    expect(runningCoordinator.getSnapshot().activeTaskId).toBeNull()
    expect(runningCoordinator.getSnapshot().activeSingleFileId).toBeNull()
    expect(runningCoordinator.getSnapshot().lifecycle).toBeNull()
  })

  it('checks cross-tab Web Locks on raw candidates, falls back to unlocked OPFS copy, and rechecks after lock release (#6, #14)', async () => {
    const file = makeFileEntry(12, 'deck.pdf', 4n)
    const dir = makeDir('root-1', 13, 'Decks')
    const store = new MemoryReceiveStore()
    const older = makeOpfsRecord('opfs-older', 'share-1', file, dir, 'ready-to-save', 1_000)
    const newer = makeOpfsRecord('opfs-newer', 'share-1', file, dir, 'ready-to-save', 5_000)
    await store.putTask(older)
    await store.putTask(newer)

    const fakeLocks = makeFakeLockManager()
    const { env: baseEnv, root } = createMemoryOPFSEnv()
    const env = { ...baseEnv, locks: fakeLocks.locks }
    const tasksDir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    const olderFile = await tasksDir.getFileHandle('opfs-older.part', { create: true })
    olderFile.bytes = new Uint8Array([7, 7, 7, 7])
    const newerFile = await tasksDir.getFileHandle('opfs-newer.part', { create: true })
    newerFile.bytes = new Uint8Array([9, 9, 9, 9])

    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))

    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      navigator: { locks: fakeLocks.locks },
    }
    const joined = makeJoinedShare('share-1', file)
    const coordinator = new ReceiveCoordinator({ windowPort, opfsEnv: env, currentJoinedShare: () => joined })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')
    await Promise.resolve()

    // Hold lock on newer copy: quick download checks locks and seamlessly exports older unlocked copy without flickering activeSingleFileId!
    fakeLocks.holdExternal('opfs-newer')
    const observedFileIds: Array<string | null> = []
    const unsub = coordinator.subscribe(() => { observedFileIds.push(coordinator.getSnapshot().activeSingleFileId) })
    const exportedDone = deferred<void>()
    let pendingDuringExportCallback: unknown = 'unset'
    expect(coordinator.startSingleFileDownload(file, dir, {
      onExported: () => {
        pendingDuringExportCallback = coordinator.getSnapshot().retained.pending
        exportedDone.resolve()
      },
    })).toEqual({ kind: 'started' })
    await exportedDone.promise
    unsub()

    // #12a: pending is already null when onExported runs.
    expect(pendingDuringExportCallback).toBeNull()
    // #14: activeSingleFileId never flickered fileId -> null -> fileId mid-handoff.
    const firstNullIdx = observedFileIds.indexOf(null)
    if (firstNullIdx !== -1) {
      expect(observedFileIds.slice(firstNullIdx).every(id => id === null)).toBe(true)
    }
    expect(downloads).toHaveLength(1)
    expect(new Uint8Array(await downloads[0]!.blob.arrayBuffer())).toEqual(new Uint8Array([7, 7, 7, 7]))

    // Lock BOTH copies: returns activeInAnotherTab; then release lock on newer -> re-click succeeds!
    fakeLocks.holdExternal('opfs-older')
    await coordinator.loadRetained()
    const blockedErr = deferred<unknown>()
    expect(coordinator.startSingleFileDownload(file, dir, {
      onAsyncError: err => blockedErr.resolve(err),
    })).toEqual({ kind: 'started' })
    const err = await blockedErr.promise
    expect(err).toBeInstanceOf(ReceiveBlockedError)
    expect((err as ReceiveBlockedError).reason).toBe('activeInAnotherTab')

    fakeLocks.releaseExternal('opfs-newer')
    const exported2 = deferred<void>()
    expect(coordinator.startSingleFileDownload(file, dir, {
      onExported: () => exported2.resolve(),
    })).toEqual({ kind: 'started' })
    await exported2.promise
    expect(downloads).toHaveLength(2)
    expect(new Uint8Array(await downloads[1]!.blob.arrayBuffer())).toEqual(new Uint8Array([9, 9, 9, 9]))
    await coordinator.dispose()
  })
})

describe('receive capability cache, pre-activation errors, and stale record cleanup', () => {
  afterEach(() => vi.restoreAllMocks())

  it('preserves prior workspace card on pre-activation error even without onAsyncError (#7.2)', async () => {
    const fileA = makeFileEntry(14, 'done.txt', 4n)
    const fileB = makeFileEntry(15, 'fail.txt', 4n)
    const dir = makeDir('root-1', 16, 'Docs')
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))

    const { windowPort } = createMockDownloadWindow()
    let shouldFail = false
    const joined = makeJoinedShare('share-1', fileA, {
      beginDownloadConnectivity: () => {
        if (shouldFail) throw new Error('connectivity boom')
        return { routes: [], close: vi.fn() } as unknown as ReturnType<JoinedBrowserShare['beginDownloadConnectivity']>
      },
    })
    const onError = deferred<unknown>()
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError: err => onError.resolve(err),
    })
    coordinator.preloadShareTasks('share-1')
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    // Complete fileA first so workspace holds a settled download-started card.
    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'started' })
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'download-started')
    const priorTaskId = coordinator.activeTaskId

    // Start fileB WITHOUT onAsyncError; pre-activation failure must NOT wipe fileA's card or create a fake failed card.
    shouldFail = true
    expect(coordinator.startSingleFileDownload(fileB, dir)).toEqual({ kind: 'started' })
    await onError.promise
    expect(coordinator.activeTaskId).toBe(priorTaskId)
    expect(coordinator.getSnapshot().lifecycle?.kind).toBe('download-started')
    expect(coordinator.startBlockedReason()).toBeNull()
    await coordinator.dispose()
  })

  it('handles restricted indexedDB getter, propagates retry failure, auto-refreshes retry-required offers, and dynamically updates OPFS storage quota (#11, #13)', async () => {
    // #11a: SecurityError from indexedDB getter degrades gracefully to portable.
    const restrictedWindow: BrowserOutputWindow = {
      get indexedDB(): IDBFactory {
        throw new DOMException('Access denied', 'SecurityError')
      },
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
    }
    const caps = await outputContract.probeBrowserOutputCapabilities(restrictedWindow)
    expect(caps).toEqual({
      indexedDb: false,
      fsa: false,
      opfs: false,
      portable: true,
      availableStorageBytes: null,
    })

    // #11b: retry failure is NOT swallowed, and subsequent recovery auto-refreshes retry-required offers.
    const mib = 1024n * 1024n
    const largeFile = makeFileEntry(17, 'big.bin', 80n * mib)
    const dir = makeDir('root-1', 18, 'Big')
    let probeAttempt = 0
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockImplementation(async () => {
      probeAttempt += 1
      if (probeAttempt <= 2) throw new Error(`probe failed #${probeAttempt}`)
      return Object.freeze({
        indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 10n * mib,
      })
    })
    let estimatedFreeBytes = 10n * mib
    vi.spyOn(outputContract, 'estimateAvailableStorageBytes').mockImplementation(async () => estimatedFreeBytes)

    const store = new MemoryReceiveStore()
    const oldOpfs = makeOpfsRecord('big-old', 'share-1', makeFileEntry(19, 'old.bin', 100n * mib), dir, 'ready-to-save', 1_000)
    await store.putTask(oldOpfs)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      navigator: {
        storage: {
          estimate: vi.fn(async () => ({ quota: Number(estimatedFreeBytes), usage: 0 })),
        } as unknown as StorageManager,
      },
    }
    const joined = makeJoinedShare('share-1', largeFile)
    const coordinator = new ReceiveCoordinator({ windowPort, opfsEnv: env, currentJoinedShare: () => joined })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'failed' && s.offers?.kind === 'retry-required')

    // First click retry fails again -> error MUST reach onAsyncError.
    const retryErr = deferred<unknown>()
    expect(coordinator.startSingleFileDownload(largeFile, dir, {
      onAsyncError: err => retryErr.resolve(err),
    })).toEqual({ kind: 'probing' })
    await retryErr.promise

    // Second click retry succeeds -> top-bar retry-required offers automatically refresh!
    expect(coordinator.startSingleFileDownload(largeFile, dir)).toEqual({ kind: 'probing' })
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready' && s.offers?.kind === 'unavailable')
    expect(coordinator.getSnapshot().offers).toMatchObject({ kind: 'unavailable', reason: 'storage-full' })
    expect(coordinator.startSingleFileDownload(largeFile, dir)).toMatchObject({ kind: 'unavailable' })

    // #13: Deleting old OPFS task refreshes availableStorageBytes in background and unblocks BOTH top-bar offers and single-file click!
    await coordinator.loadRetained()
    estimatedFreeBytes = 200n * mib
    coordinator.performRetainedAction(coordinator.getSnapshot().retained.tasks[0]!, 'discard')
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')
    expect(coordinator.getSnapshot().offers).toMatchObject({ kind: 'ready', primary: { backend: 'opfs' } })

    // Returning null from a failed background estimate must preserve the last known 200 MiB quota.
    vi.spyOn(outputContract, 'estimateAvailableStorageBytes').mockResolvedValueOnce(null)
    coordinator.preloadShareTasks('share-1')
    await Promise.resolve()
    expect(coordinator.getSnapshot().offers?.kind).toBe('ready')
    await coordinator.dispose()
  })

  it('prunes stale inventory when save hits NotFoundError (#12b)', async () => {
    const file = makeFileEntry(20, 'gone.bin', 4n)
    const dir = makeDir('root-1', 21, 'Docs')
    const store = new MemoryReceiveStore()
    const record = makeOpfsRecord('gone-task', 'share-1', file, dir, 'ready-to-save', 1_000)
    await store.putTask(record)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const errDone = deferred<unknown>()
    let pendingAtError: unknown = 'unset'
    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => makeJoinedShare('share-1', file),
      onActionError: err => {
        pendingAtError = coordinator.getSnapshot().retained.pending
        errDone.resolve(err)
      },
    })
    await coordinator.loadRetained()
    expect(coordinator.getSnapshot().retained.tasks).toHaveLength(1)

    // Delete from store behind coordinator's back; save fails with NotFoundError, releases pending before error, and prunes inventory.
    await store.deleteTask('gone-task')
    coordinator.performRetainedAction(coordinator.getSnapshot().retained.tasks[0]!, 'save')
    await errDone.promise
    expect(pendingAtError).toBeNull()
    expect(coordinator.getSnapshot().retained.tasks).toHaveLength(0)
    await coordinator.dispose()
  })

  it('preserves local settlement upserts arriving during in-flight share index preload (#15)', async () => {
    const file = makeFileEntry(22, 'fresh.bin', 4n)
    const dir = makeDir('root-1', 23, 'Fresh')
    const store = new MemoryReceiveStore()
    const preloadGate = deferred<readonly ReceiveTaskRecord[]>()
    const fullLoadGate = deferred<readonly ReceiveTaskRecord[]>()
    vi.spyOn(store, 'listTasksByShare').mockReturnValue(preloadGate.promise)
    vi.spyOn(store, 'listTasks').mockReturnValue(fullLoadGate.promise)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: false, availableStorageBytes: 64n * 1024n * 1024n,
    }))

    const { env } = createMemoryOPFSEnv()
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const selection = new SelectionPolicy(false)
    selection.set(file, dir.ancestry, true)
    const joined = makeJoinedShare('share-1', file, {
      selection,
      createTransferJob: (output: Parameters<JoinedBrowserShare['createTransferJob']>[0]) => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: async () => {
          const writer = await output.openFile({
            fileId: file.idText,
            path: [file.name],
            sourcePath: ['Fresh', file.name],
            revision: 'rev-1',
            exactSize: 4n,
          }, new AbortController().signal)
          await writer.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
          await writer.commit([{ start: 0n, end: 4n }])
          return {
            status: 'completed' as const,
            completedFiles: 1n,
            completedBytes: 4n,
            discoveredFiles: 1n,
            discoveredBytes: 4n,
            discoveryComplete: true,
            failedFiles: 0n,
            failedDirectories: 0n,
          }
        },
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })

    const coordinator = new ReceiveCoordinator({ windowPort, opfsEnv: env, currentJoinedShare: () => joined })
    // Start top-bar download while listTasksByShare is still in flight, let it settle into OPFS, then resolve stale empty preload.
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')
    expect(coordinator.startDownload('opfs:file')).toBe(true)
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'download-started' && s.activeTaskId === null)
    expect(downloads).toHaveLength(1)

    // Resolve stale empty scan AFTER settlement; mutation overlay must keep the newly settled OPFS record.
    preloadGate.resolve([])
    await waitForSnapshot(coordinator, s => s.singleFilePickerPendingFromBytes === null)
    await Promise.resolve()

    const exported = deferred<void>()
    const outcome = coordinator.startSingleFileDownload(file, dir, {
      onExported: () => exported.resolve(),
    })
    expect(outcome).toEqual({ kind: 'exported' })
    await exported.promise
    expect(downloads).toHaveLength(2)

    fullLoadGate.resolve([...store.tasks.values()])
    await coordinator.dispose()
  })

  it('keeps workspace lifecycle clean on reopen so retained matching stays selection-scoped and connection-aware', async () => {
    const fileA = makeFileEntry(31, 'alpha.bin', 8n)
    const fileB = makeFileEntry(32, 'beta.bin', 8n)
    const dir = makeDir('root-1', 33, 'Album')
    const store = new MemoryReceiveStore()
    const olderComplete = makeOpfsRecord('complete-a', 'share-1', fileA, dir, 'ready-to-save', 1_000)
    const newerPaused = makeOpfsRecord('paused-a', 'share-1', fileA, dir, 'paused', 2_000)
    await store.putTask(olderComplete)
    await store.putTask(newerPaused)

    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const selectionA = new SelectionPolicy(false)
    selectionA.set(fileA, dir.ancestry, true)
    const joined = makeJoinedShare('share-1', fileA, { selection: selectionA })
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })

    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready' && s.retained.tasks.length === 2)

    // Loading retained inventory must not hijack #workspace with a sticky adopted task.
    expect(coordinator.getSnapshot().lifecycle).toBeNull()
    expect(coordinator.activeTaskId).toBeNull()

    // Switching selection updates offers.selectionKey cleanly without leaving a stale task behind.
    const selectionB = new SelectionPolicy(false)
    selectionB.set(fileB, dir.ancestry, true)
    ;(joined as { selection: SelectionPolicy }).selection = selectionB
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.selectionKey === singleFileSpec('share-1', 'root-1', fileB, dir).key)
    expect(coordinator.getSnapshot().lifecycle).toBeNull()

    await coordinator.dispose()
  })
})
