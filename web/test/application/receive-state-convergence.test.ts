import { terminalConnectionBlock } from '../../src/application/receive/admission'
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
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { taskLockName } from '../../src/output/store/tab-lock'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle } from '../output/fsa-fixture'
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

  it('opens FSA directory picker synchronously even while retained inventory loads, fails, or holds stale records', async () => {
    const file = makeFileEntry(1, 'movie.mkv', 100n * 1024n * 1024n)
    const dir = makeDir('root-1', 2, 'Media')
    const store = new MemoryReceiveStore()
    await store.putTask(makeOpfsRecord('stale-dl', 'share-1', file, dir, 'downloading', 1_000))
    await store.putTask(makeOpfsRecord('old-ready', 'share-1', file, dir, 'ready-to-save', 2_000))

    const loadGate = deferred<readonly ReceiveTaskRecord[]>()
    vi.spyOn(store, 'listTasks').mockReturnValueOnce(loadGate.promise)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))

    const pickers = [deferred<FileSystemDirectoryHandle>(), deferred<FileSystemDirectoryHandle>()]
    let pickerIndex = 0
    let pickerOpenedSync = false
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => {
        pickerOpenedSync = true
        return pickers[pickerIndex++]!.promise
      }),
    }
    const joined = makeJoinedShare('share-1', file)
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    coordinator.warmCapabilities()
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    // Start a slow retained inventory load: FSA single-file click must not wait on history loading.
    const pendingLoad = coordinator.loadRetained()
    const outcomeWhileLoading = coordinator.startSingleFileDownload(file, dir)
    expect(outcomeWhileLoading).toEqual({ kind: 'waiting-picker' })
    expect(pickerOpenedSync).toBe(true)
    coordinator.cancelActivation()

    // Even when retained inventory loading fails, FSA single-file click still opens picker synchronously.
    pickerOpenedSync = false
    loadGate.reject(new Error('IDB transaction failed'))
    await pendingLoad
    expect(coordinator.getSnapshot().retained.kind).toBe('failed')
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'waiting-picker' })
    expect(pickerOpenedSync).toBe(true)

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
    coordinator.warmCapabilities()
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
    coordinator.warmCapabilities()
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

  it('clears activeSingleFileId and activeTaskId on dispose across waiting-picker and active running transfers (#14)', async () => {
    const file = makeFileEntry(10, 'clip.mp4', 16n)
    const dir = makeDir('root-1', 11, 'Clips')
    const picker = deferred<FileSystemDirectoryHandle>()
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValueOnce(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))
    const { windowPort: baseWin } = createMockDownloadWindow()
    const pickerWin: BrowserOutputWindow = {
      ...baseWin,
      showDirectoryPicker: vi.fn(() => picker.promise),
    }
    const pickerCoordinator = new ReceiveCoordinator({
      windowPort: pickerWin,
      currentJoinedShare: () => makeJoinedShare('share-a', file),
    })
    pickerCoordinator.warmCapabilities()
    await waitForSnapshot(pickerCoordinator, s => s.capabilitiesState === 'ready')

    expect(pickerCoordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'waiting-picker' })
    expect(pickerCoordinator.getSnapshot().activeSingleFileId).toBe(file.idText)
    const disposingPicker = pickerCoordinator.dispose()
    expect(pickerCoordinator.getSnapshot().activation).toBeNull()
    expect(pickerCoordinator.getSnapshot().activeSingleFileId).toBeNull()
    picker.resolve({} as FileSystemDirectoryHandle)
    await disposingPicker

    // Dispose while a transfer is actively running: BOTH activeTaskId and activeSingleFileId transition from non-null to null.
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValueOnce(Object.freeze({
      indexedDb: true, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
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
    runningCoordinator.warmCapabilities()
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

  it('tracks cross-tab Web Locks on retained tasks without blocking fresh single-file downloads (#6, #12a)', async () => {
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
    coordinator.warmCapabilities()
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    // Hold lock on newer copy: retained admission blocks saving the locked task while older unlocked task saves cleanly.
    fakeLocks.holdExternal('opfs-newer')
    await coordinator.loadRetained()
    const newerTask = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'opfs-newer')!
    const olderTask = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'opfs-older')!
    expect(coordinator.retainedActionAdmission(newerTask, 'save')).toEqual({ allowed: false, reason: 'activeInAnotherTab' })
    expect(coordinator.retainedActionAdmission(olderTask, 'save')).toEqual({ allowed: true, reason: null })

    const exportedDone = deferred<void>()
    let pendingDuringExportCallback: unknown = 'unset'
    coordinator.performRetainedAction(olderTask, 'save', {
      onCompleted: () => {
        pendingDuringExportCallback = coordinator.getSnapshot().retained.pending
        exportedDone.resolve()
      },
    })
    await exportedDone.promise
    // #12a: pending is already null when onCompleted runs.
    expect(pendingDuringExportCallback).toBeNull()
    expect(downloads).toHaveLength(1)
    expect(new Uint8Array(await downloads[0]!.blob.arrayBuffer())).toEqual(new Uint8Array([7, 7, 7, 7]))

    // Lock BOTH copies: explicit retained save is blocked, yet fresh single-file download still starts cleanly.
    fakeLocks.holdExternal('opfs-older')
    await coordinator.loadRetained()
    expect(coordinator.retainedActionAdmission(
      coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'opfs-older')!,
      'save',
    )).toEqual({ allowed: false, reason: 'activeInAnotherTab' })
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'started' })
    await waitForSnapshot(coordinator, s => s.activeTaskId === null && s.lifecycle?.kind === 'download-started')

    // Release external lock on newer -> reload and explicit retained save succeeds.
    fakeLocks.releaseExternal('opfs-newer')
    await coordinator.loadRetained()
    const unlockedNewer = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'opfs-newer')!
    const exported2 = deferred<void>()
    coordinator.performRetainedAction(unlockedNewer, 'save', {
      onCompleted: () => exported2.resolve(),
    })
    await exported2.promise
    expect(new Uint8Array(await downloads.at(-1)!.blob.arrayBuffer())).toEqual(new Uint8Array([9, 9, 9, 9]))
    await coordinator.dispose()
  })

  it('revalidates cross-tab locks and persisted states silently without loading flicker or redundant publishes', async () => {
    const fileA = makeFileEntry(31, 'paused-elsewhere.bin', 16n)
    const fileB = makeFileEntry(32, 'crashed-elsewhere.bin', 16n)
    const fileC = makeFileEntry(33, 'ready-elsewhere.bin', 8n)
    const dir = makeDir('root-1', 34, 'Shared')
    const store = new MemoryReceiveStore()
    await store.putTask(makeOpfsRecord('tab-a-paused', 'share-1', fileA, dir, 'downloading', 1_000))
    await store.putTask(makeOpfsRecord('tab-a-crashed', 'share-1', fileB, dir, 'downloading', 2_000))
    await store.putTask(makeOpfsRecord('tab-a-ready', 'share-1', fileC, dir, 'ready-to-save', 3_000))

    const fakeLocks = makeFakeLockManager()
    fakeLocks.holdExternal('tab-a-paused')
    fakeLocks.holdExternal('tab-a-crashed')
    fakeLocks.holdExternal('tab-a-ready')

    const { env } = createMemoryOPFSEnv()
    const listTasksSpy = vi.spyOn(store, 'listTasks')
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      navigator: { locks: fakeLocks.locks },
    }
    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: { ...env, locks: fakeLocks.locks },
      currentJoinedShare: () => makeJoinedShare('share-1', fileA),
    })

    // First ensure loads once; repeated ensure stays an idempotent no-op for directory navigation.
    await coordinator.ensureRetained()
    await coordinator.ensureRetained()
    expect(listTasksSpy).toHaveBeenCalledTimes(1)
    expect(coordinator.getSnapshot().retained.tasks.every(task => task.activeElsewhere)).toBe(true)
    const lockedReady = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'tab-a-ready')!
    expect(coordinator.retainedActionAdmission(lockedReady, 'save')).toEqual({ allowed: false, reason: 'activeInAnotherTab' })

    // Unchanged silent refresh must neither emit loading nor replace the cached tasks reference.
    const unchangedBefore = coordinator.getSnapshot().retained.tasks
    let unchangedPublishes = 0
    const stopUnchanged = coordinator.subscribe(() => { unchangedPublishes += 1 })
    await coordinator.refreshRetained()
    stopUnchanged()
    expect(unchangedPublishes).toBe(0)
    expect(coordinator.getSnapshot().retained.tasks).toBe(unchangedBefore)

    // Another tab pauses one task, crashes on another, and finishes using the third.
    await store.putTask(makeOpfsRecord('tab-a-paused', 'share-1', fileA, dir, 'paused', 4_000))
    fakeLocks.releaseExternal('tab-a-paused')
    fakeLocks.releaseExternal('tab-a-crashed')
    fakeLocks.releaseExternal('tab-a-ready')

    const observedKinds: string[] = []
    const stopRefresh = coordinator.subscribe(() => {
      observedKinds.push(coordinator.getSnapshot().retained.kind)
    })
    await coordinator.refreshRetained()
    stopRefresh()

    expect(observedKinds).toEqual(['available'])
    const refreshed = coordinator.getSnapshot().retained.tasks
    const pausedTask = refreshed.find(t => t.taskId === 'tab-a-paused')!
    const crashedTask = refreshed.find(t => t.taskId === 'tab-a-crashed')!
    const readyTask = refreshed.find(t => t.taskId === 'tab-a-ready')!
    expect(pausedTask.activeElsewhere).toBe(false)
    expect(pausedTask.lifecycle.kind).toBe('paused')
    expect(coordinator.retainedActionAdmission(pausedTask, 'continue')).toEqual({ allowed: true, reason: null })
    expect(crashedTask.activeElsewhere).toBe(false)
    expect(crashedTask.lifecycle.kind).toBe('paused')
    expect(crashedTask.lifecycle.kind === 'paused' ? crashedTask.lifecycle.reason : null).toBe('interrupted')
    expect(coordinator.retainedActionAdmission(crashedTask, 'continue')).toEqual({ allowed: true, reason: null })
    expect(readyTask.activeElsewhere).toBe(false)
    expect(coordinator.retainedActionAdmission(readyTask, 'save')).toEqual({ allowed: true, reason: null })
    await coordinator.dispose()
  })

  it('preserves settled inventory on silent refresh failure and keeps in-flight local mutations intact', async () => {
    const file = makeFileEntry(35, 'keep.bin', 8n)
    const dir = makeDir('root-1', 36, 'Keep')
    const store = new MemoryReceiveStore()
    const record = makeOpfsRecord('keep-task', 'share-1', file, dir, 'ready-to-save', 1_000)
    await store.putTask(record)

    const { env, root } = createMemoryOPFSEnv()
    const tasksDir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    const opfsFile = await tasksDir.getFileHandle('keep-task.part', { create: true })
    opfsFile.bytes = new Uint8Array(8)

    const traceEvents: OutputTraceEvent[] = []
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      outputTrace: { current: event => { traceEvents.push(event) } },
      currentJoinedShare: () => makeJoinedShare('share-1', file),
    })

    // Initial refresh from unloaded runs in foreground mode (shows loading -> available).
    const initialKinds: string[] = []
    const stopInitial = coordinator.subscribe(() => { initialKinds.push(coordinator.getSnapshot().retained.kind) })
    await coordinator.refreshRetained()
    stopInitial()
    expect(initialKinds).toEqual(['loading', 'available'])

    // Transient IDB error during silent refresh keeps existing list intact and emits a trace event.
    const settledTasks = coordinator.getSnapshot().retained.tasks
    vi.spyOn(store, 'listTasks').mockRejectedValueOnce(new Error('transient refresh error'))
    let failedRefreshPublishes = 0
    const stopFailed = coordinator.subscribe(() => { failedRefreshPublishes += 1 })
    await coordinator.refreshRetained()
    stopFailed()
    expect(failedRefreshPublishes).toBe(0)
    expect(coordinator.getSnapshot().retained.kind).toBe('available')
    expect(coordinator.getSnapshot().retained.tasks).toBe(settledTasks)
    expect(traceEvents).toContainEqual(expect.objectContaining({
      eventName: 'continuation',
      payload: expect.objectContaining({ transition: 'inventory_refresh_failed' }),
    }))

    // Slow silent refresh must not flicker to loading or resurrect a task discarded mid-flight.
    const slowRefreshGate = deferred<readonly ReceiveTaskRecord[]>()
    const realListTasks = store.listTasks.bind(store)
    vi.spyOn(store, 'listTasks').mockImplementationOnce(() => slowRefreshGate.promise)
    const refreshKinds: string[] = []
    const stopSlow = coordinator.subscribe(() => { refreshKinds.push(coordinator.getSnapshot().retained.kind) })
    const slowRefresh = coordinator.refreshRetained()

    coordinator.performRetainedAction(settledTasks[0]!, 'discard')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.retained.tasks.length === 0)

    vi.spyOn(store, 'listTasks').mockImplementation(realListTasks)
    slowRefreshGate.resolve([record])
    await slowRefresh
    stopSlow()

    expect(refreshKinds).not.toContain('loading')
    expect(coordinator.getSnapshot().retained.kind).toBe('empty')
    expect(coordinator.getSnapshot().retained.tasks).toHaveLength(0)
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
    coordinator.warmCapabilities()
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
    coordinator.warmCapabilities()
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

  it('preserves local settlement upserts arriving during an in-flight retained inventory scan (#15)', async () => {
    const file = makeFileEntry(22, 'fresh.bin', 4n)
    const dir = makeDir('root-1', 23, 'Fresh')
    const store = new MemoryReceiveStore()
    const fullLoadGate = deferred<readonly ReceiveTaskRecord[]>()
    const realListTasks = store.listTasks.bind(store)
    vi.spyOn(store, 'listTasks')
      .mockReturnValueOnce(fullLoadGate.promise)
      .mockImplementation(realListTasks)
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
    // Start a slow retained scan, then run a top-bar OPFS download to settlement before the stale empty scan resolves.
    const slowScan = coordinator.loadRetained()
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')
    expect(coordinator.startDownload('opfs:file')).toBe(true)
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'download-started' && s.activeTaskId === null)
    expect(downloads).toHaveLength(1)

    // Resolve the stale empty scan AFTER settlement; load epochs and mutation overlay must keep the settled OPFS record.
    fullLoadGate.resolve([])
    await slowScan
    await waitForSnapshot(coordinator, s => s.retained.tasks.length === 1)

    const settledTask = coordinator.getSnapshot().retained.tasks[0]!
    const exported = deferred<void>()
    coordinator.performRetainedAction(settledTask, 'save', {
      onCompleted: () => exported.resolve(),
    })
    await exported.promise
    expect(downloads).toHaveLength(2)

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

  it('blocks remote starts and FSA pickers on terminal connection states while keeping reconnecting and local saves available', async () => {
    const file = makeFileEntry(41, 'photo.png', 16n)
    const dir = makeDir('root-1', 42, 'Album')
    const store = new MemoryReceiveStore()
    const retainedReady = makeOpfsRecord('ready-copy', 'share-1', file, dir, 'ready-to-save', 1_000)
    await store.putTask(retainedReady)
    const { env } = createMemoryOPFSEnv()

    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const pickerGate = deferred<FileSystemDirectoryHandle>()
    const picker = vi.fn(() => pickerGate.promise)
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker,
    }
    const onActionError = vi.fn()
    const joined = makeJoinedShare('share-1', file)
    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => joined,
      onActionError,
    })

    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready' && s.retained.tasks.length === 1)

    // Reconnecting is transient and must stay admitted for new downloads.
    coordinator.observeConnection(terminalConnectionBlock({ kind: 'reconnecting', activity: { kind: 'connecting' } }))
    expect(coordinator.startBlockedReason()).toBeNull()

    for (const [connection, expectedReason] of [
      [{ kind: 'ended', reason: 'share-stopped' }, 'shareEnded'],
      [{ kind: 'unavailable', reason: 'protocol-failed' }, 'connectionUnavailable'],
      [{ kind: 'unavailable', reason: 'access-denied' }, 'relayAccessDenied'],
    ] as const) {
      coordinator.observeConnection(terminalConnectionBlock(connection))
      expect(coordinator.startBlockedReason()).toBe(expectedReason)
      expect(coordinator.localStartBlockedReason()).toBeNull()
      expect(coordinator.startDownload('fsa:file')).toBe(false)
      expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'blocked', reason: expectedReason })
      expect(picker).not.toHaveBeenCalled()
    }

    // A remote denial must not prevent exporting complete local copies.
    const readyTask = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'ready-copy')!
    expect(coordinator.retainedActionAdmission(readyTask, 'save')).toEqual({ allowed: true, reason: null })
    const blob = new Blob([Uint8Array.of(137, 80, 78, 71)], { type: 'image/png' })
    expect(coordinator.saveLocalBlob(blob, 'photo.png')).toBe(true)
    expect(coordinator.completeLocalBlobDownload('portable:file', blob)).toBe(true)
    expect(downloads).toHaveLength(2)

    // If the share ends while the FSA directory picker is open, settling the picker fails cleanly
    // without reserving entries on disk or writing a failed IndexedDB task.
    coordinator.observeConnection(null)
    expect(coordinator.startDownload('fsa:file')).toBe(true)
    expect(picker).toHaveBeenCalledTimes(1)
    coordinator.observeConnection('shareEnded')

    const pickedDir = new FakeDirectoryHandle('Downloads', [])
    pickerGate.resolve(pickedDir as unknown as FileSystemDirectoryHandle)
    await waitForSnapshot(coordinator, s => s.activation === null)
    expect(onActionError).toHaveBeenCalledTimes(1)
    expect(onActionError.mock.calls[0]?.[0]).toBeInstanceOf(ReceiveBlockedError)
    expect((onActionError.mock.calls[0]?.[0] as ReceiveBlockedError).reason).toBe('shareEnded')
    expect([...pickedDir.children.keys()]).toEqual([])
    expect([...store.tasks.values()].filter(t => t.backend === 'fsa')).toEqual([])

    await coordinator.dispose()
  })
})
