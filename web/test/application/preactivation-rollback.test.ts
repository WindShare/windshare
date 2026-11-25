import { afterEach, describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator, type ReceiveWindowPort } from '../../src/application/receive/coordinator'
import type { ReceiveRuntimeSnapshot } from '../../src/application/receive/contracts'
import * as sessionOpener from '../../src/application/receive/session-opener'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { byteRange } from '../../src/content/geometry'
import { encodeBase64Url } from '../../src/crypto/bytes'
import * as outputContract from '../../src/output/backends/contract'
import { openNewFSASession } from '../../src/output/backends/fsa'
import { openNewOPFSSession, type OPFSEnvironment } from '../../src/output/backends/opfs'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { ReceiveStore } from '../../src/output/store/database'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, FakeFileHandle, MemoryReceiveStore } from '../output/fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow } from '../output/opfs-fixture'

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function makeFileEntry(seed: number, name: string, size: bigint): Extract<CatalogEntry, { kind: 'file' }> {
  const id = new Uint8Array(16).fill(seed)
  return Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize: size })
}

function waitForSnapshot(
  coordinator: ReceiveCoordinator,
  predicate: (snapshot: ReceiveRuntimeSnapshot) => boolean,
): Promise<ReceiveRuntimeSnapshot> {
  const current = coordinator.getSnapshot()
  if (predicate(current)) return Promise.resolve(current)
  return new Promise(resolve => {
    const unsubscribe = coordinator.subscribe(() => {
      const next = coordinator.getSnapshot()
      if (predicate(next)) {
        unsubscribe()
        resolve(next)
      }
    })
  })
}

function makeTrackingLockManager() {
  const held = new Set<string>()
  const locks = {
    request: async (
      name: string,
      options: LockOptions,
      callback: (lock: Lock | null) => Promise<unknown>,
    ) => {
      if (options.ifAvailable && held.has(name)) return callback(null)
      held.add(name)
      try {
        return await callback({ name, mode: options.mode ?? 'exclusive' })
      } finally {
        held.delete(name)
      }
    },
  } as unknown as LockManager
  return { locks, held }
}

function createWindowWithBackends(options: Readonly<{
  parent?: FakeDirectoryHandle
  locks?: LockManager
}> = {}): ReceiveWindowPort {
  const { windowPort: baseWin } = createMockDownloadWindow()
  const { locks } = options.locks === undefined ? makeTrackingLockManager() : { locks: options.locks }
  return {
    ...baseWin,
    indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    navigator: { locks },
    ...(options.parent === undefined
      ? {}
      : { showDirectoryPicker: vi.fn(async () => options.parent as unknown as FileSystemDirectoryHandle) }),
  }
}

function makePausedResult(overrides: Partial<SettledTransferJobResult> = {}): SettledTransferJobResult {
  return {
    status: 'paused',
    pauseReason: 'user',
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 1n,
    discoveredBytes: 8n,
    discoveryComplete: true,
    failedFiles: 0n,
    failedDirectories: 0n,
    ...overrides,
  }
}

function makeCompletedResult(size: bigint): SettledTransferJobResult {
  return {
    status: 'completed',
    completedFiles: 1n,
    completedBytes: size,
    discoveredFiles: 1n,
    discoveredBytes: size,
    discoveryComplete: true,
    failedFiles: 0n,
    failedDirectories: 0n,
  }
}

describe('pre-activation output session rollback', () => {
  afterEach(() => vi.restoreAllMocks())

  it.each([
    {
      label: 'FSA single-file',
      choiceId: 'fsa:file',
      shape: (file: Extract<CatalogEntry, { kind: 'file' }>) =>
        ({ kind: 'single-file' as const, file, sourcePath: [file.name], exactBytes: file.expectedSize }),
      reservedName: 'report.pdf',
    },
    {
      label: 'FSA folder',
      choiceId: 'fsa:tree',
      shape: () => ({
        kind: 'tree' as const,
        suggestedName: 'Photos',
        rootPath: Object.freeze([]),
        knownBytes: 16n,
        knownFiles: 1n,
        complete: true,
      }),
      reservedName: 'Photos',
    },
  ])('rolls back new $label reservation and IDB row when share ends before activation', async ({
    choiceId,
    shape,
    reservedName,
  }) => {
    const file = makeFileEntry(1, 'report.pdf', 16n)
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const traces: OutputTraceEvent[] = []
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null,
    }))

    const { locks, held } = makeTrackingLockManager()
    let failConnectivity = true
    const startupError = new Error('share ended while opening output session')
    const onError = deferred<unknown>()
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      subscribeConnection: () => () => undefined,
      inspectSelection: async () => shape(file),
      beginDownloadConnectivity: () => {
        if (failConnectivity) throw startupError
        return { routes: [], close: vi.fn() }
      },
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: async () => makeCompletedResult(file.expectedSize),
      }),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort: createWindowWithBackends({ parent, locks }),
      outputTrace: { current: event => traces.push(event) },
      currentJoinedShare: () => joined,
      onActionError: error => onError.resolve(error),
    })

    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    expect(coordinator.startDownload(choiceId)).toBe(true)
    expect(await onError.promise).toBe(startupError)
    await vi.waitFor(() => expect(coordinator.startBlockedReason()).toBeNull(), { interval: 1 })
    await coordinator.refreshRetained()

    expect([...parent.children.keys()]).toEqual([])
    expect([...store.tasks.values()]).toEqual([])
    expect(coordinator.getSnapshot().retained.tasks).toEqual([])
    expect(coordinator.getSnapshot().lifecycle).toBeNull()
    expect(held.size).toBe(0)
    expect(traces).toContainEqual({
      eventName: 'output_reservation',
      payload: expect.objectContaining({
        backend: 'file_system_access',
        transition: 'rolled_back',
        removed_placeholder: true,
      }),
    })

    // Rolling back frees the top-level name so a subsequent download acquires collisionIndex 0.
    failConnectivity = false
    expect(coordinator.startDownload(choiceId)).toBe(true)
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'saved')
    const saved = [...store.tasks.values()][0]!
    expect(saved.target.kind === 'fsa' ? saved.target.reservedName : '').toBe(reservedName)
    expect(saved.target.kind === 'fsa' ? saved.target.collisionIndex : -1).toBe(0)
    await coordinator.dispose()
  })

  it('rolls back new OPFS reservation, closes SyncAccessHandle before unlinking, and leaves no ghost task', async () => {
    const file = makeFileEntry(2, 'video.bin', 80n * 1024n * 1024n)
    const store = new MemoryReceiveStore()
    const { env: baseEnv, root } = createMemoryOPFSEnv()
    const { locks, held } = makeTrackingLockManager()
    const opfsEnv: OPFSEnvironment = Object.freeze({ ...baseEnv, locks })
    const traces: OutputTraceEvent[] = []
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 512n * 1024n * 1024n,
    }))

    const startupError = new Error('share ended before OPFS activation')
    const onError = deferred<unknown>()
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      subscribeConnection: () => () => undefined,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file,
        sourcePath: [file.name],
        exactBytes: file.expectedSize,
      }),
      beginDownloadConnectivity: () => { throw startupError },
      createTransferJob: vi.fn(),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort: createWindowWithBackends({ locks }),
      opfsEnv,
      outputTrace: { current: event => traces.push(event) },
      currentJoinedShare: () => joined,
      onActionError: error => onError.resolve(error),
    })

    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    expect(coordinator.startDownload('opfs:file')).toBe(true)
    expect(await onError.promise).toBe(startupError)
    await vi.waitFor(() => expect(coordinator.startBlockedReason()).toBeNull(), { interval: 1 })
    await coordinator.refreshRetained()

    const tasksDir = root.dirs.get('windshare-tasks')
    expect(tasksDir?.files.size ?? 0).toBe(0)
    expect([...store.tasks.values()]).toEqual([])
    expect(coordinator.getSnapshot().retained.tasks).toEqual([])
    expect(held.size).toBe(0)
    expect(traces).toContainEqual({
      eventName: 'output_reservation',
      payload: expect.objectContaining({
        backend: 'origin_private',
        transition: 'rolled_back',
        removed_placeholder: true,
      }),
    })
    await coordinator.dispose()
  })

  it('preserves externally modified FSA placeholder bytes during pre-activation rollback while dropping the unactivated task', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const traces: OutputTraceEvent[] = []
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-external-edit',
        selection: { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-1',
        artifact: { kind: 'directory-tree', name: 'notes.txt', singleFileId: 'f1' },
        lifecycle: initialReceiveState('fsa-external-edit'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      diagnostics: { backend: 'file_system_access', trace: { current: event => traces.push(event) } },
    })

    const userBytes = new Uint8Array([9, 8, 7, 6])
    const reserved = parent.children.get('notes.txt') as FakeFileHandle
    reserved.overwriteExternally(userBytes)

    await session.rollbackUnactivated()
    await session.close()

    expect([...(parent.children.get('notes.txt') as FakeFileHandle).bytes]).toEqual([...userBytes])
    expect(await store.getTask('fsa-external-edit')).toBeUndefined()
    expect(traces).toContainEqual({
      eventName: 'output_reservation',
      payload: expect.objectContaining({
        backend: 'file_system_access',
        task_id: 'fsa-external-edit',
        transition: 'rolled_back',
        removed_placeholder: false,
      }),
    })
  })
})

describe('reopen preservation, superseded launches, and rollback failure safety', () => {
  afterEach(() => vi.restoreAllMocks())

  it.each(['fsa', 'opfs'] as const)(
    'preserves retained bytes and restores prior paused reason when resuming a %s task fails before activation',
    async backend => {
      const store = new MemoryReceiveStore()
      const parent = new FakeDirectoryHandle('Downloads', store.events)
      const { env: baseEnv, root } = createMemoryOPFSEnv()
      const { locks, held } = makeTrackingLockManager()
      const opfsEnv: OPFSEnvironment = Object.freeze({ ...baseEnv, locks })
      const traces: OutputTraceEvent[] = []
      vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
        Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
      )
      vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
        indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 512n * 1024n * 1024n,
      }))

      const file = makeFileEntry(5, 'movie.bin', 8n)
      const partialBytes = new Uint8Array([1, 2, 3, 4])
      const taskInput = {
        taskId: `paused-${backend}`,
        selection: { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-paused',
        artifact: backend === 'fsa'
          ? { kind: 'directory-tree' as const, name: file.name, singleFileId: file.idText }
          : { kind: 'original-file' as const, name: file.name, singleFileId: file.idText },
        lifecycle: initialReceiveState(`paused-${backend}`),
      }
      const initialSession = backend === 'fsa'
        ? await openNewFSASession({
            task: taskInput,
            parentHandle: parent as unknown as FileSystemDirectoryHandle,
            store: store as unknown as ReceiveStore,
            locks,
          })
        : await openNewOPFSSession({
            task: taskInput,
            store: store as unknown as ReceiveStore,
            env: opfsEnv,
            windowPort: createMockDownloadWindow().windowPort,
          })

      const outFile = await initialSession.transferOutput.openFile({
        fileId: file.idText,
        revision: 'rev-1',
        path: [file.name],
        sourcePath: [file.name],
        exactSize: 8n,
      }, new AbortController().signal)
      await outFile.writeAt(0n, partialBytes)
      await outFile.pause([byteRange(0n, 4n)])
      await initialSession.settle(makePausedResult({ pauseReason: 'user' }))
      await initialSession.close()

      const resumeError = new Error('share ended while resuming')
      const onError = deferred<unknown>()
      const joined = {
        descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
        selection: new SelectionPolicy(true),
        subscribeConnection: () => () => undefined,
        beginDownloadConnectivity: () => { throw resumeError },
        createTransferJob: vi.fn(),
      } as unknown as JoinedBrowserShare

      const coordinator = new ReceiveCoordinator({
        windowPort: createWindowWithBackends({ parent, locks }),
        opfsEnv,
        outputTrace: { current: event => traces.push(event) },
        currentJoinedShare: () => joined,
        onActionError: error => onError.resolve(error),
      })
      await coordinator.loadRetained()
      const retained = coordinator.getSnapshot().retained.tasks[0]!
      expect(retained.lifecycle).toMatchObject({ kind: 'paused', reason: 'user', retainedBytes: 4n })

      coordinator.performRetainedAction(retained, 'continue')
      expect(await onError.promise).toBe(resumeError)
      await vi.waitFor(() => expect(coordinator.startBlockedReason()).toBeNull(), { interval: 1 })
      await coordinator.refreshRetained()

      const persisted = await store.getTask(taskInput.taskId)
      expect(persisted?.lifecycle).toMatchObject({ kind: 'paused', reason: 'user', retainedBytes: 4n })
      expect(coordinator.getSnapshot().retained.tasks[0]?.lifecycle).toMatchObject({
        kind: 'paused',
        reason: 'user',
        retainedBytes: 4n,
      })
      if (backend === 'fsa') {
        expect([...(parent.children.get(file.name) as FakeFileHandle).bytes]).toEqual([...partialBytes])
      } else {
        const opfsFile = root.dirs.get('windshare-tasks')?.files.get(`${taskInput.taskId}.part`)
        expect([...(opfsFile?.bytes ?? [])]).toEqual([...partialBytes])
      }
      expect(held.size).toBe(0)
      expect(traces).toContainEqual({
        eventName: 'reopen',
        payload: expect.objectContaining({
          task_id: taskInput.taskId,
          transition: 'rolled_back',
        }),
      })
      await coordinator.dispose()
    },
  )

  it('rolls back a superseded FSA session opened while suspendForNewShare runs', async () => {
    const file = makeFileEntry(6, 'draft.pdf', 16n)
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null,
    }))

    const realOpen = sessionOpener.openNewOutputSession
    const openStarted = deferred<void>()
    const allowOpenReturn = deferred<void>()
    const openFinished = deferred<void>()
    vi.spyOn(sessionOpener, 'openNewOutputSession').mockImplementation(async input => {
      const session = await realOpen(input)
      openStarted.resolve()
      await allowOpenReturn.promise
      queueMicrotask(() => openFinished.resolve())
      return session
    })

    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      subscribeConnection: () => () => undefined,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file,
        sourcePath: [file.name],
        exactBytes: file.expectedSize,
      }),
      beginDownloadConnectivity: vi.fn(),
      createTransferJob: vi.fn(),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort: createWindowWithBackends({ parent }),
      currentJoinedShare: () => joined,
    })
    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    expect(coordinator.startDownload('fsa:file')).toBe(true)
    await openStarted.promise
    expect(parent.children.has('draft.pdf')).toBe(true)
    expect(store.tasks.size).toBe(1)

    coordinator.suspendForNewShare()
    allowOpenReturn.resolve()
    await openFinished.promise
    await vi.waitFor(() => expect(store.tasks.size).toBe(0), { interval: 1 })

    expect([...parent.children.keys()]).toEqual([])
    expect(coordinator.getSnapshot().retained.tasks).toEqual([])
    await coordinator.dispose()
  })

  it('releases lock and reports original error when rollbackUnactivated throws, and ignores rollback after output starts', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const { locks, held } = makeTrackingLockManager()
    const traces: OutputTraceEvent[] = []

    const opened = await openNewFSASession({
      task: {
        taskId: 'post-touch-safe',
        selection: { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-touch',
        artifact: { kind: 'directory-tree', name: 'keep.bin', singleFileId: 'f1' },
        lifecycle: initialReceiveState('post-touch-safe'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      locks,
    })
    const writer = await opened.transferOutput.openFile({
      fileId: 'f1', revision: 'r1', path: ['keep.bin'], sourcePath: ['keep.bin'], exactSize: 4n,
    }, new AbortController().signal)
    await writer.writeAt(0n, new Uint8Array([4, 5, 6, 7]))
    await writer.commit([byteRange(0n, 4n)])
    // Once output has been touched, rollbackUnactivated must be a strict no-op.
    await opened.rollbackUnactivated()
    expect(parent.children.has('keep.bin')).toBe(true)
    expect(await store.getTask('post-touch-safe')).toBeDefined()
    await opened.close()
    expect(held.size).toBe(0)

    // Rollback failure must trace rollback_failed, still release the lock, and never mask the original error.
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const failParent = new FakeDirectoryHandle('Protected', store.events)
    vi.spyOn(failParent, 'removeEntry').mockRejectedValue(new DOMException('Permission revoked', 'NotAllowedError'))
    const startupError = new Error('original share-ended error')
    const onError = deferred<unknown>()
    const file = makeFileEntry(7, 'locked.bin', 16n)
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      subscribeConnection: () => () => undefined,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file,
        sourcePath: [file.name],
        exactBytes: file.expectedSize,
      }),
      beginDownloadConnectivity: () => { throw startupError },
      createTransferJob: vi.fn(),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort: createWindowWithBackends({ parent: failParent, locks }),
      outputTrace: { current: event => traces.push(event) },
      currentJoinedShare: () => joined,
      onActionError: error => onError.resolve(error),
    })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    expect(coordinator.startDownload('fsa:file')).toBe(true)
    expect(await onError.promise).toBe(startupError)
    await vi.waitFor(() => expect(coordinator.startBlockedReason()).toBeNull(), { interval: 1 })
    expect(held.size).toBe(0)
    expect(traces).toContainEqual({
      eventName: 'settlement',
      payload: expect.objectContaining({
        backend: 'file_system_access',
        transition: 'rollback_failed',
        error_class: 'not_allowed',
      }),
    })
    await coordinator.dispose()
  })
})
