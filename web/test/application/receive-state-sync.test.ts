import { afterEach, describe, expect, it, vi } from 'vitest'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import type { ReceiverTraceEvent } from '../../src/application/receive/contracts'
import { ReceiveCoordinator, type ReceiveWindowPort } from '../../src/application/receive/coordinator'
import * as sessionOpener from '../../src/application/receive/session-opener'
import { ReceiveBlockedError } from '../../src/application/receive/single-file'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { byteRange } from '../../src/content/geometry'
import * as outputContract from '../../src/output/backends/contract'
import { openNewFSASession } from '../../src/output/backends/fsa'
import { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { ReceiverController } from '../../src/ui/controller'
import { TaskComposer } from '../../src/ui/experience/task-composition'
import { FakeDirectoryHandle, MemoryReceiveStore as FSAMemoryReceiveStore } from '../output/fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'
import {
  commitMockOutputFile,
  deferred,
  makeFileEntry,
  makeJobResult,
  makeSubDirectory,
  singleFileSelectionKey,
  waitForSnapshot,
} from './receive-coordinator-fixture'

function makeTrackingLocks() {
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

function makeWindowPort(options: Readonly<{
  parent?: FakeDirectoryHandle
  locks?: LockManager
}> = {}): ReceiveWindowPort {
  const { windowPort } = createMockDownloadWindow()
  const locks = options.locks ?? makeTrackingLocks().locks
  return {
    ...windowPort,
    indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    navigator: { locks },
    ...(options.parent === undefined
      ? {}
      : { showDirectoryPicker: vi.fn(async () => options.parent as unknown as FileSystemDirectoryHandle) }),
  }
}

function makeOpfsRecord(
  taskId: string,
  shareInstance: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  lifecycleKind: 'paused' | 'downloading',
  updatedAt: number,
): ReceiveTaskRecord {
  const { spec, key } = singleFileSelectionKey(shareInstance, 'root-1', entry, directory)
  const base = initialReceiveState(taskId, updatedAt)
  const lifecycle = lifecycleKind === 'downloading'
    ? transitionReceiveState(base, { kind: 'downloading' }, updatedAt)
    : transitionReceiveState(base, {
        kind: 'paused', reason: 'user', completedFiles: 0n, completedBytes: 0n, retainedBytes: 1n,
      }, updatedAt)
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
    },
    display: { objectLabel: entry.name, createdAtMilliseconds: updatedAt },
    updatedAt,
  }
}

function makeSingleFileShare(
  shareInstanceId: string,
  file: Extract<CatalogEntry, { kind: 'file' }>,
  overrides: Partial<JoinedBrowserShare> = {},
): JoinedBrowserShare {
  return {
    descriptor: {
      shareInstance: new Uint8Array(16).fill(1),
      shareInstanceId,
      syntheticRoot: new Uint8Array(16).fill(2),
      syntheticRootId: 'root-1',
    },
    recoveryIdentity: `recovery-${shareInstanceId}`,
    protocolSessionId: 'session-1',
    protocolSessionIdentity: 'session-1',
    selection: new SelectionPolicy(true),
    replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
    rootDirectory: () => ({ id: new Uint8Array(16).fill(2), idText: 'root-1', path: [], ancestry: ['root-1'] }),
    page: async (directory: BrowseDirectory) => ({
      directory, pageIndex: 0, pageCount: 1, entryCount: 1, omittedCount: 0n, entries: [file],
    }),
    inspectSelection: async () => ({
      kind: 'single-file' as const,
      file,
      sourcePath: [file.name],
      exactBytes: file.expectedSize,
    }),
    subscribeCatalogScanProgress: () => () => undefined,
    subscribePathActivity: () => () => undefined,
    subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => {
      listener({ kind: 'connected' })
      return () => undefined
    },
    subscribeProtocolGeneration: () => () => undefined,
    beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
    beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
    createTransferJob: () => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: async () => makeJobResult('completed', file.expectedSize),
    }),
    close: async () => undefined,
    ...overrides,
  } as unknown as JoinedBrowserShare
}

describe('receive state sync invariants', () => {
  afterEach(() => vi.restoreAllMocks())

  it.each([
    { backend: 'fsa' as const, choiceId: 'fsa:file', expectedDestination: 'Projects' },
    { backend: 'opfs' as const, choiceId: 'opfs:file', expectedDestination: { kind: 'browser-downloads' } },
    { backend: 'portable' as const, choiceId: 'portable:file', expectedDestination: { kind: 'browser-downloads' } },
  ])('exposes backend-enriched destinationLabel immediately during active $backend downloads', async ({
    backend,
    choiceId,
    expectedDestination,
  }) => {
    const file = makeFileEntry(1, 'archive.bin', 16n)
    const store = new FSAMemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Projects', store.events)
    const { env } = createMemoryOPFSEnv()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))

    const jobGate = deferred<ReturnType<typeof makeJobResult>>()
    const joined = makeSingleFileShare('share-1', file, {
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: () => jobGate.promise,
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })
    const coordinator = new ReceiveCoordinator({
      windowPort: makeWindowPort({ parent }),
      opfsEnv: env,
      currentJoinedShare: () => joined,
    })

    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const started = coordinator.startDownload(choiceId, {
      objectLabel: file.name,
      createdAtMilliseconds: 1_234,
    })
    expect(started).toBe(true)

    const active = await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'downloading')
    expect(active.backend).toBe(backend)
    expect(active.display).toEqual({
      objectLabel: file.name,
      createdAtMilliseconds: 1_234,
      destinationLabel: expectedDestination,
    })

    jobGate.resolve(makeJobResult('completed', file.expectedSize))
    await waitForSnapshot(coordinator, s => s.activeTaskId === null)
    await coordinator.dispose()
  })

  it('re-checks terminal connectionBlock before and after async FSA permission prompt on continue', async () => {
    const store = new FSAMemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const { locks, held } = makeTrackingLocks()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )

    const file = makeFileEntry(2, 'movie.bin', 8n)
    const initialSession = await openNewFSASession({
      task: {
        taskId: 'paused-fsa-prompt',
        selection: { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] },
        selectionKey: 'sel-paused',
        artifact: { kind: 'directory-tree', name: file.name, singleFileId: file.idText },
        lifecycle: initialReceiveState('paused-fsa-prompt'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      locks,
    })
    const outFile = await initialSession.transferOutput.openFile({
      fileId: file.idText,
      revision: 'rev-1',
      path: [file.name],
      sourcePath: [file.name],
      exactSize: 8n,
    }, new AbortController().signal)
    await outFile.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await outFile.pause([byteRange(0n, 4n)])
    await initialSession.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 1n,
      discoveredBytes: 8n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await initialSession.close()

    const promptStarted = deferred<void>()
    const promptGate = deferred<PermissionState>()
    vi.spyOn(parent, 'queryPermission').mockResolvedValue('prompt')
    vi.spyOn(parent, 'requestPermission').mockImplementation(() => {
      promptStarted.resolve()
      return promptGate.promise
    })

    const beginConnectivity = vi.fn(() => ({ routes: [], close: vi.fn() }))
    const joined = makeSingleFileShare('share-1', file, {
      beginDownloadConnectivity: beginConnectivity as unknown as JoinedBrowserShare['beginDownloadConnectivity'],
    })
    const actionError = deferred<unknown>()
    const coordinator = new ReceiveCoordinator({
      windowPort: makeWindowPort({ parent, locks }),
      currentJoinedShare: () => joined,
      onActionError: error => actionError.resolve(error),
    })
    await coordinator.loadRetained()
    const retained = coordinator.getSnapshot().retained.tasks[0]!

    coordinator.performRetainedAction(retained, 'continue')
    await promptStarted.promise
    // Share ends while the browser folder-permission prompt is still awaiting user confirmation.
    coordinator.observeConnection('shareEnded')
    promptGate.resolve('granted')

    const error = await actionError.promise
    expect(error).toBeInstanceOf(ReceiveBlockedError)
    expect((error as ReceiveBlockedError).reason).toBe('shareEnded')
    expect(beginConnectivity).not.toHaveBeenCalled()
    expect(held.size).toBe(0)
    expect((await store.getTask('paused-fsa-prompt'))?.lifecycle).toMatchObject({
      kind: 'paused',
      reason: 'user',
      retainedBytes: 4n,
    })
    await coordinator.dispose()
  })

  it('indexes post-normalization updatedAt so a follow-up silent refresh does not republish unchanged tasks', async () => {
    const file = makeFileEntry(3, 'abandoned.bin', 16n)
    const dir = makeSubDirectory('root-1', 4, 'Docs')
    const store = new MemoryReceiveStore()
    await store.putTask(makeOpfsRecord('crashed-task', 'share-1', file, dir, 'downloading', 1_000))
    const { env } = createMemoryOPFSEnv()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )

    const coordinator = new ReceiveCoordinator({
      windowPort: makeWindowPort(),
      opfsEnv: env,
      currentJoinedShare: () => makeSingleFileShare('share-1', file),
    })

    await coordinator.loadRetained()
    const afterLoad = coordinator.getSnapshot().retained.tasks
    expect(afterLoad).toHaveLength(1)
    expect(afterLoad[0]?.lifecycle).toMatchObject({ kind: 'paused', reason: 'interrupted' })
    expect((await store.getTask('crashed-task'))?.updatedAt).toBeGreaterThan(1_000)

    let silentPublishes = 0
    const unsubscribe = coordinator.subscribe(() => { silentPublishes += 1 })
    await coordinator.refreshRetained()
    unsubscribe()

    expect(silentPublishes).toBe(0)
    expect(coordinator.getSnapshot().retained.tasks).toBe(afterLoad)
    await coordinator.dispose()
  })

  it('reuses ReceiverProjections TaskComposer for observability without duplicate trace rows on admission flips', async () => {
    const fileA = makeFileEntry(5, 'fresh.bin', 16n)
    const fileB = makeFileEntry(6, 'paused.bin', 16n)
    const dir = makeSubDirectory('root-1', 7, 'Docs')
    const store = new MemoryReceiveStore()
    await store.putTask(makeOpfsRecord('paused-history', 'share-1', fileB, dir, 'paused', 2_000))
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))

    const composeSpy = vi.spyOn(TaskComposer.prototype, 'compose')
    const events: ReceiverTraceEvent[] = []
    const jobGate = deferred<ReturnType<typeof makeJobResult>>()
    const joined = makeSingleFileShare('share-1', fileA, {
      createTransferJob: output => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: async () => {
          const result = await jobGate.promise
          if (result.status === 'completed') await commitMockOutputFile(output, fileA)
          return result
        },
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      {
        windowPort: makeWindowPort(),
        trace: { current: event => events.push(event) },
      },
    )
    let controllerPublishes = 0
    const unsubscribe = controller.subscribe(() => { controllerPublishes += 1 })

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await vi.waitFor(() => {
      const snapshot = controller.getSnapshot()
      expect(snapshot.retained.tasks).toHaveLength(1)
      expect(snapshot.output.offers?.kind).toBe('ready')
    })

    // Every controller publish runs TaskComposer.compose exactly once (shared by projections and observability),
    // plus the initial compose in ReceiverProjections constructor.
    expect(composeSpy).toHaveBeenCalledTimes(controllerPublishes + 1)

    const historyEventsBefore = events.filter(
      event => event.name === 'receiver_experience' && event.transition === 'task' && event.operationId === 'paused-history',
    )
    expect(historyEventsBefore).toHaveLength(1)

    // Starting and finishing another download toggles the retained task's `continue` disabledReason
    // from null -> 'downloading' -> null, which must not emit duplicate-payload task trace entries.
    controller.chooseArtifact('portable:file')
    await vi.waitFor(() => expect(controller.getSnapshot().output.lifecycle?.kind).toBe('downloading'))
    jobGate.resolve(makeJobResult('completed', fileA.expectedSize))
    await vi.waitFor(() => expect(controller.getSnapshot().output.lifecycle?.kind).toBe('download-started'))
    await vi.waitFor(() => expect(controller.getSnapshot().activeReceiveOperationId).toBeNull())

    expect(composeSpy).toHaveBeenCalledTimes(controllerPublishes + 1)
    const historyEventsAfter = events.filter(
      event => event.name === 'receiver_experience' && event.transition === 'task' && event.operationId === 'paused-history',
    )
    expect(historyEventsAfter).toHaveLength(1)

    unsubscribe()
    await controller.dispose()
  })

  it('clears a discarded workspace task and publishes pending release during waiting-picker while gating pre-activation release/retain/continue', async () => {
    const file = makeFileEntry(8, 'report.bin', 16n)
    const store = new FSAMemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const { locks } = makeTrackingLocks()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))

    const pickers = [deferred<FileSystemDirectoryHandle>(), deferred<FileSystemDirectoryHandle>()]
    let pickerIndex = 0
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: ReceiveWindowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      navigator: { locks },
      showDirectoryPicker: vi.fn(() => pickers[pickerIndex++]!.promise),
    }

    const jobGate = deferred<ReturnType<typeof makeJobResult>>()
    const joined = makeSingleFileShare('share-1', file, {
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: () => jobGate.promise,
      }) as unknown as ReturnType<JoinedBrowserShare['createTransferJob']>,
    })

    const onActionError = vi.fn()
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError,
    })
    coordinator.refreshOffers(joined)
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    // Complete an initial download into a paused workspace task.
    expect(coordinator.startDownload('fsa:file')).toBe(true)
    pickers[0]!.resolve(parent as unknown as FileSystemDirectoryHandle)
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'downloading')
    jobGate.resolve(makeJobResult('paused', 0n))
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'paused' && s.retained.tasks.length === 1)

    const pausedTaskId = coordinator.activeTaskId!
    expect(coordinator.canReleaseCurrent).toBe(true)
    expect(coordinator.canRetainCurrent).toBe(true)
    expect(coordinator.activeActionAdmission('continue')).toEqual({ allowed: true, reason: null })

    // While resuming awaits folder permission, workspace release/retain must remain blocked.
    const promptStarted = deferred<void>()
    const promptGate = deferred<PermissionState>()
    const querySpy = vi.spyOn(parent, 'queryPermission').mockResolvedValueOnce('prompt')
    const requestSpy = vi.spyOn(parent, 'requestPermission').mockImplementationOnce(() => {
      promptStarted.resolve()
      return promptGate.promise
    })
    coordinator.performActiveAction('continue')
    await promptStarted.promise
    expect(coordinator.canReleaseCurrent).toBe(false)
    expect(coordinator.canRetainCurrent).toBe(false)
    expect(coordinator.activeActionAdmission('continue')).toEqual({ allowed: false, reason: 'busy' })

    promptGate.resolve('denied')
    await waitForSnapshot(coordinator, s => s.retained.pending === null)
    expect(querySpy).toHaveBeenCalledOnce()
    expect(requestSpy).toHaveBeenCalledOnce()
    expect(coordinator.canReleaseCurrent).toBe(true)
    expect(coordinator.canRetainCurrent).toBe(true)

    // Launch a new download into waiting-picker: prior paused task stays in workspace, but release/retain/continue are blocked.
    expect(coordinator.startDownload('fsa:file')).toBe(true)
    expect(coordinator.getSnapshot().activation?.kind).toBe('waiting-picker')
    expect(coordinator.activeTaskId).toBe(pausedTaskId)
    expect(coordinator.canReleaseCurrent).toBe(false)
    expect(coordinator.canRetainCurrent).toBe(false)
    expect(coordinator.activeActionAdmission('continue')).toEqual({ allowed: false, reason: 'downloading' })
    expect(coordinator.activeActionAdmission('discard')).toEqual({ allowed: true, reason: null })

    // Discarding the pinned paused task during waiting-picker clears workspace and publishes pending=null without disturbing activation.
    coordinator.performActiveAction('discard')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.lifecycle === null)
    expect(coordinator.activeTaskId).toBeNull()
    expect(coordinator.getSnapshot().retained.tasks).toEqual([])
    expect(coordinator.getSnapshot().activation?.kind).toBe('waiting-picker')
    expect(store.tasks.has(pausedTaskId)).toBe(false)

    // Cancelling the picker leaves the workspace cleanly empty rather than pinning the deleted task.
    coordinator.cancelActivation()
    expect(coordinator.getSnapshot().activation).toBeNull()
    expect(coordinator.getSnapshot().lifecycle).toBeNull()
    pickers[1]!.reject(new DOMException('Cancelled', 'AbortError'))
    await coordinator.dispose()
  })

  it('clears a forgotten ephemeral portable workspace task and publishes pending release while another download is opening', async () => {
    const file = makeFileEntry(9, 'photo.png', 4n)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const openStarted = deferred<void>()
    const openGate = deferred<never>()
    vi.spyOn(sessionOpener, 'openNewOutputSession').mockImplementation(() => {
      openStarted.resolve()
      return openGate.promise
    })

    const { windowPort } = createMockDownloadWindow()
    const onActionError = vi.fn()
    const joined = makeSingleFileShare('share-1', file)
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError,
    })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const blob = new Blob([Uint8Array.of(137, 80, 78, 71)], { type: 'image/png' })
    expect(coordinator.completeLocalBlobDownload('portable:file', blob)).toBe(true)
    expect(coordinator.getSnapshot().lifecycle?.kind).toBe('download-started')

    // Start a new download held in the pre-activation 'opening' state.
    expect(coordinator.startDownload('portable:file')).toBe(true)
    await openStarted.promise
    expect(coordinator.getSnapshot().activation?.kind).toBe('committing')

    // Forgetting the ephemeral portable workspace task during 'opening' clears workspace and releases pending.
    coordinator.performActiveAction('forget')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.lifecycle === null)
    expect(coordinator.activeTaskId).toBeNull()
    expect(coordinator.getSnapshot().activation?.kind).toBe('committing')

    openGate.reject(new DOMException('Cancelled', 'AbortError'))
    await waitForSnapshot(coordinator, s => s.activation === null)
    expect(coordinator.getSnapshot().lifecycle).toBeNull()
    await coordinator.dispose()
  })
})
