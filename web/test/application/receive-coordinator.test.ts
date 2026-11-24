import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  actionsForState,
  admitRetainedAction,
  canForgetState,
  canSavePartialState,
  retainedTaskReadiness,
} from '../../src/application/receive/admission'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { mapReceiverFailure } from '../../src/ui/controller/failures'
import type { RetainedTaskSnapshot } from '../../src/application/receive/contracts'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import { executeRetainedTaskAction } from '../../src/application/receive/retained'
import * as sessionOpener from '../../src/application/receive/session-opener'
import {
  ReceiveBlockedError,
  SaveUnavailableError,
  planSingleFileDownload,
} from '../../src/application/receive/single-file'
import type { BrowseDirectory, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { SelectionPolicy, type FrozenSelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserOutputWindow, OutputSessionHandle } from '../../src/output/backends/contract'
import * as outputContract from '../../src/output/backends/contract'
import type { NativeSupportWorker } from '../../src/output/native-object/support'
import { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { isPermissionDeniedError } from '../../src/transfer/fault'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, MemoryReceiveStore as FSAMemoryReceiveStore } from '../output/fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'
import {
  deferred,
  makeFileEntry,
  makeJobResult,
  makeSubDirectory,
  singleFileSelectionKey,
  waitForSnapshot,
} from './receive-coordinator-fixture'

function createDeferredProbeWorker() {
  const workers: Array<NativeSupportWorker & { terminated: boolean }> = []
  class FakeSupportWorker implements NativeSupportWorker {
    onmessage: ((event: MessageEvent<unknown>) => void) | null = null
    onerror: ((event: ErrorEvent) => void) | null = null
    onmessageerror: ((event: MessageEvent<unknown>) => void) | null = null
    terminated = false
    constructor() {
      workers.push(this)
    }
    terminate(): void {
      this.terminated = true
    }
  }
  return { Worker: FakeSupportWorker as unknown as NonNullable<BrowserOutputWindow['Worker']>, workers }
}

function createSingleFileShare(name = 'report.pdf'): JoinedBrowserShare {
  return {
    descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
    selection: new SelectionPolicy(true),
    inspectSelection: async (_policy: unknown, signal?: AbortSignal) => {
      signal?.throwIfAborted()
      return {
        kind: 'single-file' as const,
        file: { kind: 'file' as const, id: new Uint8Array(16).fill(1), idText: 'file-1', name, expectedSize: 2_048n },
        sourcePath: [name],
        exactBytes: 2_048n,
      }
    },
  } as unknown as JoinedBrowserShare
}

describe('receive admission and coordinator', () => {
  it('computes accurate actions and readiness across backends and share contexts', () => {
    const base = initialReceiveState('t1')
    const receiving = transitionReceiveState(base, { kind: 'downloading', phase: 'receiving' })
    const finalizing = transitionReceiveState(base, { kind: 'downloading', phase: 'finalizing' })
    expect(actionsForState(receiving, 'fsa')).toEqual(['pause', 'stop'])
    expect(actionsForState(receiving, 'opfs')).toEqual(['pause'])
    expect(actionsForState(receiving, 'portable')).toEqual(['stop'])
    expect(actionsForState(finalizing, 'opfs')).toEqual([])
    expect(actionsForState(finalizing, 'fsa')).toEqual([])

    const pausedOpfs = transitionReceiveState(base, {
      kind: 'paused', reason: 'user',
      completedFiles: 2n, completedBytes: 20n, retainedBytes: 20n,
      discoveredFiles: 5n, discoveredBytes: 50n, discoveryComplete: true,
    })
    expect(actionsForState(pausedOpfs, 'opfs', true)).toEqual(['continue', 'save-partial', 'discard'])

    const readyOpfs = transitionReceiveState(base, {
      kind: 'ready-to-save', size: 50n, fileName: 'archive.zip',
    })
    expect(actionsForState(readyOpfs, 'opfs')).toEqual(['save', 'discard'])

    const retainedStarted = transitionReceiveState(base, {
      kind: 'download-started', retained: true, fileName: 'archive.zip',
    })
    expect(actionsForState(retainedStarted, 'opfs')).toEqual(['save', 'discard'])

    const portableStarted = transitionReceiveState(base, {
      kind: 'download-started', retained: false, fileName: 'photo.jpg',
    })
    expect(actionsForState(portableStarted, 'portable')).toEqual(['forget'])

    const partialState = transitionReceiveState(base, {
      kind: 'partial', reason: 'failures', savedFiles: 1n, failedFiles: 1n,
    })
    expect(canSavePartialState(partialState, 'opfs', true)).toBe(true)
    expect(canSavePartialState(partialState, 'opfs', false)).toBe(false)
    expect(canSavePartialState(partialState, 'fsa', true)).toBe(false)
    expect(actionsForState(partialState, 'opfs', true)).toEqual(['save-partial', 'discard'])
    expect(actionsForState(partialState, 'opfs', false)).toEqual(['discard'])
    expect(actionsForState(partialState, 'fsa', false)).toEqual(['forget'])

    const partialOpfsTask: RetainedTaskSnapshot = {
      taskId: 't-partial-opfs',
      shareInstance: 'share-a',
      selectionKey: 'sel-a',
      backend: 'opfs',
      lifecycle: partialState,
      actions: ['save-partial', 'discard'],
      activeElsewhere: false,
      canSavePartial: true,
    }
    expect(retainedTaskReadiness(partialOpfsTask, null)).toBe('local')
    expect(admitRetainedAction(partialOpfsTask, 'save-partial', {
      currentShareInstance: null,
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionUnavailable: true,
      shareEnded: true,
    })).toEqual({ allowed: true, reason: null })
    const presentedOpfsPartial = presentTask(retainedTaskFacts(partialOpfsTask))
    expect(presentedOpfsPartial.stage).toBe('ready-to-save')
    expect(presentedOpfsPartial.publication).toBe('unpublished')
    expect(presentedOpfsPartial.completeness).toBe('partial')
    expect(presentedOpfsPartial.primaryAction?.id).toBe('save-partial')

    const partialFsaTask: RetainedTaskSnapshot = {
      ...partialOpfsTask,
      taskId: 't-partial-fsa',
      backend: 'fsa',
      actions: ['forget'],
      canSavePartial: false,
    }
    const presentedFsaPartial = presentTask(retainedTaskFacts(partialFsaTask))
    expect(presentedFsaPartial.stage).toBe('saved')
    expect(presentedFsaPartial.publication).toBe('saved')
    expect(presentedFsaPartial.completeness).toBe('partial')
    expect(presentedFsaPartial.removal).toBeNull()

    const partialFsaPendingLifecycle = transitionReceiveState(base, {
      kind: 'partial', reason: 'stopped', savedFiles: 1n, failedFiles: 0n, cleanupPending: true,
    })
    expect(actionsForState(partialFsaPendingLifecycle, 'fsa')).toEqual(['discard'])
    expect(canForgetState(partialFsaPendingLifecycle, 'fsa')).toBe(true)
    const partialFsaPendingTask: RetainedTaskSnapshot = {
      ...partialFsaTask,
      taskId: 't-partial-fsa-pending',
      lifecycle: partialFsaPendingLifecycle,
      actions: ['discard'],
    }
    const presentedFsaPending = presentTask(retainedTaskFacts(partialFsaPendingTask))
    expect(presentedFsaPending.stage).toBe('saved')
    expect(presentedFsaPending.destructiveActions.map(action => action.id)).toEqual(['discard'])
    expect(presentedFsaPending.removal?.fallbackForget?.id).toBe('forget')
    expect(presentedFsaPending.details.some(line => line.includes('unfinished files'))).toBe(true)

    const task: RetainedTaskSnapshot = {
      taskId: 't1',
      shareInstance: 'share-a',
      selectionKey: 'sel-a',
      backend: 'opfs',
      lifecycle: pausedOpfs,
      actions: ['continue', 'save-partial', 'discard'],
      activeElsewhere: false,
      canSavePartial: true,
    }
    expect(retainedTaskReadiness(task, 'share-a')).toBe('matching-share')
    expect(retainedTaskReadiness(task, 'share-b')).toBe('different-share')
    expect(retainedTaskReadiness(task, null)).toBe('original-link-required')

    expect(admitRetainedAction(task, 'continue', {
      currentShareInstance: 'share-b',
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionUnavailable: false,
      shareEnded: false,
    })).toEqual({ allowed: false, reason: 'matchingShare' })

    expect(admitRetainedAction(task, 'save-partial', {
      currentShareInstance: null,
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionUnavailable: true,
      shareEnded: true,
    })).toEqual({ allowed: true, reason: null })

    expect(admitRetainedAction({ ...task, activeElsewhere: true }, 'discard', {
      currentShareInstance: 'share-a',
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionUnavailable: false,
      shareEnded: false,
    })).toEqual({ allowed: false, reason: 'activeInAnotherTab' })
  })

  it('invokes showDirectoryPicker synchronously inside startDownload before returning', async () => {
    let pickerCalledSynchronously = false
    let resolvePicker!: (handle: FileSystemDirectoryHandle) => void
    const windowPort: BrowserOutputWindow = {
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
      showDirectoryPicker: vi.fn(() => {
        pickerCalledSynchronously = true
        return new Promise<FileSystemDirectoryHandle>(resolve => { resolvePicker = resolve })
      }),
    }
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({
        kind: 'tree' as const,
        suggestedName: 'Album',
        rootPath: ['Album'],
        knownFileCount: 2,
        knownBytes: 100n,
        complete: true,
      }),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
    })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    expect(offers.primary.backend).toBe('fsa')

    const accepted = coordinator.startDownload(offers.primary.choiceId)

    expect(accepted).toBe(true)
    expect(pickerCalledSynchronously).toBe(true)
    expect(coordinator.getSnapshot().activation?.kind).toBe('waiting-picker')
    // Duplicate click while picker is open must be rejected immediately.
    expect(coordinator.startDownload(offers.primary.choiceId)).toBe(false)

    coordinator.cancelActivation()
    resolvePicker({} as FileSystemDirectoryHandle)
    await coordinator.dispose()
  })

  it('preserves draft ancestry hints on the selection policy passed into a live transfer job', async () => {
    const fileId = new Uint8Array(16).fill(7)
    const fileIdText = encodeBase64Url(fileId)
    const selection = new SelectionPolicy(false)
    selection.set(
      { kind: 'file', id: fileId, idText: fileIdText, name: 'invoice.pdf', expectedSize: 16n },
      ['root-1', 'sub-1'],
      true,
    )

    const jobSelection = deferred<FrozenSelectionPolicy | undefined>()
    const windowPort: BrowserOutputWindow = {
      URL: { createObjectURL: vi.fn(() => 'blob:test'), revokeObjectURL: vi.fn() },
      document: {
        createElement: vi.fn(() => ({ click: vi.fn(), remove: vi.fn(), style: {} })),
        body: { appendChild: vi.fn() },
      } as unknown as Document,
    }
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file: { kind: 'file' as const, id: fileId, idText: fileIdText, name: 'invoice.pdf', expectedSize: 16n },
        sourcePath: ['sub-1', 'invoice.pdf'],
        exactBytes: 16n,
      }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: (_output: unknown, _connectivity: unknown, options?: { readonly selection?: FrozenSelectionPolicy }) => {
        jobSelection.resolve(options?.selection)
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          detach: vi.fn(),
          run: async () => ({
            status: 'completed' as const,
            completedFiles: 1n,
            completedBytes: 16n,
            discoveredFiles: 1n,
            discoveredBytes: 16n,
            discoveryComplete: true,
            failedFiles: 0n,
            failedDirectories: 0n,
          }),
        }
      },
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
    })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    expect(coordinator.startDownload(offers.primary.choiceId)).toBe(true)
    const capturedSelection = await jobSelection.promise
    expect(capturedSelection?.draftRules?.[0]?.ancestry).toEqual(['root-1', 'sub-1'])

    await coordinator.dispose()
  })

  it('keeps the shared capability probe alive when selection changes mid-probe', async () => {
    const { Worker, workers } = createDeferredProbeWorker()
    const onActionError = vi.fn()
    const windowPort: BrowserOutputWindow = {
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
      navigator: {
        storage: { getDirectory: vi.fn() } as unknown as StorageManager,
      },
      Worker,
    }
    const shareA = createSingleFileShare('a.pdf')
    const shareB = createSingleFileShare('b.pdf')
    let activeShare = shareA

    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => activeShare,
      onActionError,
    })

    coordinator.refreshOffers(shareA)
    expect(workers).toHaveLength(1)
    expect(coordinator.getSnapshot().offers?.kind).toBe('confirming')

    // Switch selection and share while the initial Worker capability probe is still pending.
    coordinator.refreshOffers(shareA)
    coordinator.suspendForNewShare()
    activeShare = shareB
    coordinator.refreshOffers(shareB)
    expect(workers).toHaveLength(1)
    expect(workers[0]!.terminated).toBe(false)

    workers[0]!.onmessage?.({ data: true } as MessageEvent<unknown>)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    expect(offers.primary.backend).toBe('portable')
    expect(offers.alternatives.some(choice => choice.backend === 'opfs')).toBe(true)
    expect(offers.primary.suggestedName).toBe('b.pdf')
    expect(workers[0]!.terminated).toBe(true)
    expect(onActionError).not.toHaveBeenCalled()

    await coordinator.dispose()
  })

  it('clears a failed capability probe so retryOffers can re-probe and recover', async () => {
    let storageReads = 0
    const windowPort: BrowserOutputWindow = {
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
      get navigator() {
        storageReads += 1
        if (storageReads === 1) throw new Error('transient capability read failure')
        return {}
      },
    }
    const joined = createSingleFileShare('notes.txt')
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
    })

    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'retry-required')

    coordinator.retryOffers()
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')
    expect(storageReads).toBe(2)

    await coordinator.dispose()
  })

  it('terminates an in-flight capability probe worker on coordinator dispose', async () => {
    const { Worker, workers } = createDeferredProbeWorker()
    const onActionError = vi.fn()
    const windowPort: BrowserOutputWindow = {
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
      navigator: {
        storage: { getDirectory: vi.fn() } as unknown as StorageManager,
      },
      Worker,
    }
    const joined = createSingleFileShare('video.mp4')
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError,
    })

    coordinator.refreshOffers(joined)
    expect(workers).toHaveLength(1)
    expect(workers[0]!.terminated).toBe(false)

    await coordinator.dispose()
    expect(workers[0]!.terminated).toBe(true)
    expect(onActionError).not.toHaveBeenCalled()
  })

  it('clears a rejected store open promise so subsequent loads can retry', async () => {
    const open = vi.fn(() => {
      const target = new EventTarget()
      const request = Object.assign(target, {
        error: new DOMException('Database upgrade blocked', 'InvalidStateError'),
      })
      queueMicrotask(() => target.dispatchEvent(new Event('error')))
      return request as unknown as IDBOpenDBRequest
    })
    const windowPort: BrowserOutputWindow = {
      indexedDB: { open } as unknown as IDBFactory,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn() } as unknown as Document,
    }
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => undefined,
    })

    await coordinator.loadRetained()
    await coordinator.loadRetained()
    expect(open).toHaveBeenCalledTimes(2)

    await coordinator.dispose()
  })
})

describe('single-file quick download planning and activation ownership', () => {
  afterEach(() => vi.restoreAllMocks())

  it('preserves joined.selection, falls back to portable at 32 MiB on low OPFS quota, opens FSA picker synchronously, and fires onActivated after first active publish', async () => {
    const mib = 1024n * 1024n
    const entrySmall = makeFileEntry(10, 'clip.bin', 32n * mib)
    const entryLarge = makeFileEntry(11, 'huge.bin', 80n * mib)
    const dir = makeSubDirectory('root-1', 2, 'Docs')
    const draftSelection = new SelectionPolicy(false)
    draftSelection.set(makeFileEntry(99, 'draft-only.txt', 12n), ['root-1'], true)

    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true,
      fsa: true,
      opfs: true,
      portable: true,
      availableStorageBytes: 8n * mib,
    }))

    const { windowPort: baseWin } = createMockDownloadWindow()
    let pickerSync = false
    const picker = deferred<FileSystemDirectoryHandle>()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      showDirectoryPicker: vi.fn(() => {
        pickerSync = true
        return picker.promise
      }),
    }

    const jobGate = deferred<ReturnType<typeof makeJobResult>>()
    const events: string[] = []
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: draftSelection,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file: entrySmall,
        sourcePath: ['Docs', entrySmall.name],
        exactBytes: entrySmall.expectedSize,
      }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        run: () => jobGate.promise,
      }),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    let publishedActiveBeforeCallback = false
    const activated = deferred<void>()
    const outcomeSmall = coordinator.startSingleFileDownload(entrySmall, dir, {
      onActivated: () => {
        publishedActiveBeforeCallback =
          coordinator.getSnapshot().lifecycle?.kind === 'downloading' &&
          coordinator.getSnapshot().activeSingleFileId === entrySmall.idText
        events.push('activated')
        activated.resolve()
      },
    })
    expect(outcomeSmall).toEqual({ kind: 'started' })
    expect(draftSelection.snapshot().canonicalRules).toHaveLength(1)
    expect(draftSelection.selected(entrySmall, dir.ancestry)).toBe(false)

    await activated.promise
    expect(events).toEqual(['activated'])
    expect(publishedActiveBeforeCallback).toBe(true)
    expect(coordinator.getSnapshot().backend).toBe('portable')

    jobGate.resolve(makeJobResult('completed', entrySmall.expectedSize))
    await waitForSnapshot(coordinator, s => s.activeSingleFileId === null)

    // 80 MiB exceeds both 64 MiB portable cap and 8 MiB OPFS quota, leaving only FSA.
    const outcomeLarge = coordinator.startSingleFileDownload(entryLarge, dir)
    expect(outcomeLarge).toEqual({ kind: 'waiting-picker' })
    expect(pickerSync).toBe(true)
    expect(coordinator.getSnapshot().activation).toMatchObject({
      kind: 'waiting-picker',
      singleFileId: entryLarge.idText,
    })
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(entryLarge.idText)

    coordinator.cancelActivation()
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()
    picker.resolve({} as FileSystemDirectoryHandle)
    await coordinator.dispose()
  })

  it('tracks activeSingleFileId across top selection, list shortcut, and resume while isolating late cancelled activations', async () => {
    const fileA = makeFileEntry(21, 'a.bin', 100n * 1024n * 1024n)
    const fileB = makeFileEntry(22, 'b.bin', 100n * 1024n * 1024n)
    const dir = makeSubDirectory('root-1', 3, 'Videos')
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null,
    }))

    const pickers = [deferred<FileSystemDirectoryHandle>(), deferred<FileSystemDirectoryHandle>()]
    let pickCall = 0
    const { windowPort: baseWin } = createMockDownloadWindow()
    const onActionError = vi.fn()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => pickers[pickCall++]!.promise),
    }

    const jobGates = [deferred<ReturnType<typeof makeJobResult>>(), deferred<ReturnType<typeof makeJobResult>>()]
    let jobCall = 0
    const selection = new SelectionPolicy(false)
    selection.set(fileA, dir.ancestry, true)
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection,
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file: fileA,
        sourcePath: ['Videos', fileA.name],
        exactBytes: fileA.expectedSize,
      }),
      subscribeConnection: () => () => undefined,
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => {
        const gate = jobGates[jobCall++]!
        return { pause: vi.fn(), stop: vi.fn(), detach: vi.fn(), run: () => gate.promise }
      },
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined, onActionError })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    // Top-level single-file selection exposes activeSingleFileId during waiting-picker.
    expect(coordinator.startDownload('fsa:file')).toBe(true)
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileA.idText)

    // Cancel A and immediately start B via quick download.
    coordinator.cancelActivation()
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()
    expect(coordinator.startSingleFileDownload(fileB, dir)).toEqual({ kind: 'waiting-picker' })
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)

    // Late rejection from cancelled picker A must not disturb B or report an error.
    pickers[0]!.reject(new Error('stale picker A failed'))
    await Promise.resolve()
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)
    expect(onActionError).not.toHaveBeenCalled()

    // Complete B with a pause: quick download starts a fresh download, while explicit continue resumes B and tracks activeSingleFileId.
    const events: string[] = []
    pickers[1]!.resolve(new FakeDirectoryHandle('Downloads', events) as unknown as FileSystemDirectoryHandle)
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'downloading')
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)

    jobGates[0]!.resolve(makeJobResult('paused', fileB.expectedSize))
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'paused')
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()

    pickers.push(deferred<FileSystemDirectoryHandle>())
    expect(coordinator.startSingleFileDownload(fileB, dir)).toEqual({ kind: 'waiting-picker' })
    coordinator.cancelActivation()
    pickers[2]!.resolve({} as FileSystemDirectoryHandle)

    coordinator.performActiveAction('continue')
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'downloading')
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)

    jobGates[1]!.resolve(makeJobResult('completed', fileB.expectedSize))
    await waitForSnapshot(coordinator, s => s.activeSingleFileId === null)
    expect(onActionError).not.toHaveBeenCalled()
    await coordinator.dispose()
  })

  it('closes a late session opened for a replaced activation without disturbing the active download', async () => {
    const fileA = makeFileEntry(23, 'first.bin', 128n)
    const fileB = makeFileEntry(24, 'second.bin', 128n)
    const dir = makeSubDirectory('root-1', 3, 'Docs')
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const firstSessionGate = deferred<OutputSessionHandle>()
    const firstOpenCalled = deferred<void>()
    const staleClosed = deferred<void>()
    const staleClose = vi.fn(async () => { staleClosed.resolve() })
    const secondJobGate = deferred<ReturnType<typeof makeJobResult>>()
    let openCalls = 0
    vi.spyOn(sessionOpener, 'openNewOutputSession').mockImplementation(async input => {
      openCalls += 1
      if (openCalls === 1) {
        firstOpenCalled.resolve()
        return firstSessionGate.promise
      }
      return {
        taskId: input.task.taskId,
        backend: 'portable',
        transferOutput: { capabilities: { resumable: false } } as unknown as OutputSessionHandle['transferOutput'],
        settle: async () => ({
          taskId: input.task.taskId,
          shareInstance: input.task.selection.shareInstance,
          selectionKey: input.task.selectionKey,
          selection: input.task.selection,
          artifact: input.task.artifact,
          backend: 'portable',
          lifecycle: transitionReceiveState(input.task.lifecycle, { kind: 'download-started', retained: false, fileName: fileB.name }),
          target: { kind: 'portable', fileName: fileB.name },
          discoveryComplete: true,
          updatedAt: Date.now(),
        }),
        close: vi.fn(async () => undefined),
      }
    })
    const { windowPort } = createMockDownloadWindow()
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => ({ pause: vi.fn(), stop: vi.fn(), run: () => secondJobGate.promise }),
    } as unknown as JoinedBrowserShare
    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    coordinator.warmCapabilities()
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')

    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'started' })
    await firstOpenCalled.promise
    expect(openCalls).toBe(1)
    // Supersede A (e.g. share switch/cancel) and immediately start B.
    coordinator.suspendForNewShare()
    expect(coordinator.startSingleFileDownload(fileB, dir)).toEqual({ kind: 'started' })

    // Resolve stale session A late — it must close immediately and never interfere with B.
    firstSessionGate.resolve({
      taskId: 'stale-a',
      backend: 'portable',
      transferOutput: { capabilities: { resumable: false } } as unknown as OutputSessionHandle['transferOutput'],
      settle: vi.fn(),
      close: staleClose,
    })
    await staleClosed.promise
    expect(staleClose).toHaveBeenCalledTimes(1)
    await waitForSnapshot(coordinator, s => s.activeSingleFileId === fileB.idText)

    secondJobGate.resolve(makeJobResult('completed', fileB.expectedSize))
    await waitForSnapshot(coordinator, s => s.activeSingleFileId === null)
    await coordinator.dispose()
  })
})

describe('single-file fresh download, errors, and local blob handoff', () => {
  afterEach(() => vi.restoreAllMocks())

  it('starts a fresh transfer even when retained OPFS or paused tasks exist, while explicit retained actions save and resume', async () => {
    const file = makeFileEntry(30, 'report.pdf', 4n)
    const dir = makeSubDirectory('root-1', 4, 'Reports')
    const { spec, key } = singleFileSelectionKey('share-1', 'root-1', file, dir)

    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)

    const { env, root } = createMemoryOPFSEnv()
    const tasksDir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    const opfsFile = await tasksDir.getFileHandle('opfs-ready.part', { create: true })
    opfsFile.bytes = new Uint8Array([1, 2, 3, 4])

    const readyRecord: ReceiveTaskRecord = {
      taskId: 'opfs-ready',
      shareInstance: 'share-1',
      selectionKey: key,
      selection: spec,
      artifact: { kind: 'original-file', name: file.name, singleFileId: file.idText, singleFileSize: 4n },
      backend: 'opfs',
      lifecycle: transitionReceiveState(initialReceiveState('opfs-ready'), {
        kind: 'ready-to-save', size: 4n, fileName: file.name,
      }),
      target: { kind: 'opfs', objectName: 'opfs-ready.part', fileName: file.name, isZip: false, allocatedLength: 4n, sealedSize: 4n },
      display: { objectLabel: file.name, createdAtMilliseconds: 5_000 },
      discoveryComplete: true,
      updatedAt: 5_000,
    }
    const pausedRecord: ReceiveTaskRecord = {
      taskId: 'paused-task',
      shareInstance: 'share-1',
      selectionKey: key,
      selection: spec,
      artifact: { kind: 'original-file', name: file.name, singleFileId: file.idText, singleFileSize: 4n },
      backend: 'opfs',
      lifecycle: transitionReceiveState(initialReceiveState('paused-task'), {
        kind: 'paused', reason: 'user',
        completedFiles: 0n, completedBytes: 0n, retainedBytes: 2n,
        discoveredFiles: 1n, discoveredBytes: 4n, discoveryComplete: true,
      }),
      target: { kind: 'opfs', objectName: 'paused.part', fileName: file.name, isZip: false, allocatedLength: 4n },
      display: { objectLabel: file.name, createdAtMilliseconds: 9_000 },
      discoveryComplete: true,
      updatedAt: 9_000,
    }
    await store.putTask(readyRecord)
    await store.putTask(pausedRecord)

    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const createTransferJob = vi.fn((output: Parameters<JoinedBrowserShare['createTransferJob']>[0]) => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: async () => {
        const writer = await output.openFile({
          fileId: file.idText,
          path: [file.name],
          sourcePath: ['Reports', file.name],
          revision: 'rev-fresh',
          exactSize: 4n,
        }, new AbortController().signal)
        await writer.writeAt(0n, new Uint8Array([8, 8, 8, 8]))
        await writer.commit([{ start: 0n, end: 4n }])
        return makeJobResult('completed', 4n)
      },
    }))
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({ kind: 'single-file' as const, file, sourcePath: ['Reports', file.name], exactBytes: 4n }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob,
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => joined,
    })
    coordinator.warmCapabilities()
    await coordinator.loadRetained()
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready' && s.retained.tasks.length === 2)

    // Quick download always fetches freshly from the sender instead of silently exporting or resuming retained tasks.
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({ kind: 'started' })
    await waitForSnapshot(coordinator, s => s.activeTaskId === null && s.lifecycle?.kind === 'download-started')
    expect(createTransferJob).toHaveBeenCalledTimes(1)
    expect(downloads).toHaveLength(1)
    expect(new Uint8Array(await downloads[0]!.blob.arrayBuffer())).toEqual(new Uint8Array([8, 8, 8, 8]))

    // Explicit retained save exports the stored OPFS copy locally without starting a transfer.
    const savedReady = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'opfs-ready')!
    const exported = deferred<void>()
    coordinator.performRetainedAction(savedReady, 'save', { onCompleted: () => exported.resolve() })
    await exported.promise
    expect(createTransferJob).toHaveBeenCalledTimes(1)
    expect(downloads).toHaveLength(2)
    expect(new Uint8Array(await downloads[1]!.blob.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3, 4]))

    // Explicit retained continue still honors shareEnded admission checks.
    const pausedTask = coordinator.getSnapshot().retained.tasks.find(t => t.taskId === 'paused-task')!
    coordinator.observeConnection('ended')
    expect(coordinator.retainedActionAdmission(pausedTask, 'continue')).toEqual({
      allowed: false,
      reason: 'shareEnded',
    })
    await coordinator.dispose()
  })

  it('retries failed capability probe, reports SaveUnavailableError and path limit synchronously, and separates startup vs transfer errors', async () => {
    const file = makeFileEntry(40, 'archive.bin', 128n * 1024n * 1024n)
    const smallFile = makeFileEntry(41, 'note.txt', 32n)
    const dir = makeSubDirectory('root-1', 5, 'Sub')

    let probeCalls = 0
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockImplementation(async () => {
      probeCalls += 1
      if (probeCalls === 1) throw new Error('probe failed')
      return Object.freeze({
        indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
      })
    })
    const { windowPort: baseWin } = createMockDownloadWindow()
    const onActionError = vi.fn()
    let failConnectivity = true
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({ kind: 'single-file' as const, file: smallFile, sourcePath: ['Sub', smallFile.name], exactBytes: smallFile.expectedSize }),
      beginDownloadConnectivity: () => {
        if (failConnectivity) throw new Error('startup connectivity failed')
        return { routes: [], close: vi.fn() }
      },
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        run: async () => { throw new Error('mid-transfer failure') },
      }),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort: baseWin,
      currentJoinedShare: () => joined,
      onActionError,
    })
    coordinator.warmCapabilities()
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'failed')

    // Clicking while failed switches back to probing and recovers.
    expect(coordinator.startSingleFileDownload(smallFile, dir)).toEqual({ kind: 'probing' })
    await waitForSnapshot(coordinator, s => s.capabilitiesState === 'ready')
    expect(probeCalls).toBe(2)

    // Path exceeding 256 depth fails synchronously without calling onActionError.
    const deepDir: BrowseDirectory = Object.freeze({
      id: new Uint8Array(16).fill(6),
      idText: 'deep',
      path: Object.freeze(Array.from({ length: 256 }, (_, i) => `d${i}`)),
      ancestry: Object.freeze(['root-1', 'deep']),
    })
    const pathFailure = coordinator.startSingleFileDownload(smallFile, deepDir)
    expect(pathFailure.kind).toBe('failed')
    expect(onActionError).not.toHaveBeenCalled()

    // 128 MiB with only portable returns structured SaveUnavailableError.
    const unavailable = coordinator.startSingleFileDownload(file, dir)
    expect(unavailable.kind).toBe('unavailable')
    if (unavailable.kind === 'unavailable') {
      expect(unavailable.error).toBeInstanceOf(SaveUnavailableError)
      expect(unavailable.error.reason).toBe('portable-limit-exceeded')
    }
    expect(onActionError).not.toHaveBeenCalled()

    // Startup failure before first active publish routes to onAsyncError, not onActionError.
    const asyncErrors: unknown[] = []
    const startupError = deferred<void>()
    expect(coordinator.startSingleFileDownload(smallFile, dir, {
      onAsyncError: error => {
        asyncErrors.push(error)
        startupError.resolve()
      },
    })).toEqual({ kind: 'started' })
    await startupError.promise
    expect(asyncErrors).toHaveLength(1)
    expect(onActionError).not.toHaveBeenCalled()
    expect(coordinator.getSnapshot().lifecycle).toBeNull()

    // Post-activation transfer error routes to normal lifecycle failure + onActionError.
    failConnectivity = false
    const asyncErrors2: unknown[] = []
    expect(coordinator.startSingleFileDownload(smallFile, dir, {
      onAsyncError: error => asyncErrors2.push(error),
    })).toEqual({ kind: 'started' })
    await waitForSnapshot(coordinator, s => s.lifecycle?.kind === 'failed')
    expect(asyncErrors2).toHaveLength(0)
    expect(onActionError).toHaveBeenCalledTimes(1)

    // Also sanity-check pure plan helper leaves ReceiveBlockedError type intact.
    expect(new ReceiveBlockedError('shareEnded').reason).toBe('shareEnded')
    expect(planSingleFileDownload(joined, smallFile, dir, {
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }).offers.kind).toBe('ready')
    await coordinator.dispose()
  })

  it('saveLocalBlob saves synchronously without claiming transfer slot and completeLocalBlobDownload persists portable history', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const joined = createSingleFileShare('photo.png')
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
    })
    await coordinator.loadRetained()
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const blob = new Blob([Uint8Array.of(137, 80, 78, 71)], { type: 'image/png' })
    expect(coordinator.saveLocalBlob(blob, 'photo.png')).toBe(true)
    expect(downloads).toHaveLength(1)
    expect(downloads[0]!.fileName).toBe('photo.png')
    expect(coordinator.active).toBe(false)
    expect(coordinator.getSnapshot().lifecycle).toBeNull()
    expect(store.tasks.size).toBe(0)

    expect(coordinator.completeLocalBlobDownload('portable:file', blob, {
      objectLabel: 'photo.png',
      createdAtMilliseconds: 1234,
    })).toBe(true)
    expect(downloads).toHaveLength(2)
    const taskId = coordinator.getSnapshot().lifecycle?.taskId
    expect(taskId).toBeDefined()
    expect(coordinator.getSnapshot().lifecycle).toMatchObject({
      kind: 'download-started',
      retained: false,
      fileName: 'photo.png',
    })
    expect(coordinator.getSnapshot().backend).toBe('portable')
    expect(coordinator.active).toBe(false)
    expect(coordinator.getSnapshot().retained.tasks.map(task => task.taskId)).toEqual([taskId])

    // Immediate dispose still flushes the admitted background history write.
    await coordinator.dispose()
    const persisted = store.tasks.get(taskId!)
    expect(persisted).toMatchObject({
      taskId,
      shareInstance: 'share-1',
      backend: 'portable',
      discoveryComplete: true,
      target: { kind: 'portable', fileName: 'photo.png' },
      display: {
        objectLabel: 'photo.png',
        createdAtMilliseconds: 1234,
        destinationLabel: { kind: 'browser-downloads' },
      },
      lifecycle: { kind: 'download-started', retained: false, fileName: 'photo.png' },
    })

    // A reloaded coordinator loads the persisted terminal history entry.
    const reloaded = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined })
    await reloaded.loadRetained()
    expect(reloaded.getSnapshot().retained.tasks.map(task => task.taskId)).toEqual([taskId])
    await reloaded.dispose()
  })

  it('serializes local blob history persistence against concurrent scans, immediate forget, and storage failure', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const onActionError = vi.fn()
    const joined = createSingleFileShare('photo.png')
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError,
    })
    coordinator.refreshOffers(joined)
    await waitForSnapshot(coordinator, s => s.offers?.kind === 'ready')

    const blob = new Blob([Uint8Array.of(137, 80, 78, 71)], { type: 'image/png' })
    const realPut = store.putTask.bind(store)

    // 1. A loadRetained() started before a slow putTask settles waits for it instead of dropping the record.
    const scanGate = deferred<void>()
    const scanWriteStarted = deferred<void>()
    vi.spyOn(store, 'putTask').mockImplementationOnce(async record => {
      scanWriteStarted.resolve()
      await scanGate.promise
      await realPut(record)
    })
    expect(coordinator.completeLocalBlobDownload('portable:file', blob)).toBe(true)
    const firstTaskId = coordinator.getSnapshot().lifecycle!.taskId
    await scanWriteStarted.promise
    const scanPromise = coordinator.loadRetained()
    scanGate.resolve()
    await scanPromise
    expect(coordinator.getSnapshot().retained.tasks.map(task => task.taskId)).toEqual([firstTaskId])

    // 2. Immediate forget waits for an in-flight putTask so it deletes cleanly without resurrecting a zombie record.
    const forgetGate = deferred<void>()
    const forgetWriteStarted = deferred<void>()
    vi.spyOn(store, 'putTask').mockImplementationOnce(async record => {
      forgetWriteStarted.resolve()
      await forgetGate.promise
      await realPut(record)
    })
    expect(coordinator.completeLocalBlobDownload('portable:file', blob)).toBe(true)
    const secondTaskId = coordinator.getSnapshot().lifecycle!.taskId
    await forgetWriteStarted.promise
    coordinator.performActiveAction('forget')
    expect(coordinator.getSnapshot().retained.pending).toEqual({ taskId: secondTaskId, action: 'forget' })
    forgetGate.resolve()
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.lifecycle === null)
    expect(store.tasks.has(secondTaskId)).toBe(false)
    expect(coordinator.getSnapshot().retained.tasks.map(task => task.taskId)).toEqual([firstTaskId])

    // 3. Best-effort persistence failures never fail the blob save and still allow forgetting the in-memory entry.
    vi.spyOn(store, 'putTask').mockRejectedValueOnce(new DOMException('Quota exceeded', 'QuotaExceededError'))
    expect(coordinator.completeLocalBlobDownload('portable:file', blob)).toBe(true)
    const failedPersistId = coordinator.getSnapshot().lifecycle!.taskId
    coordinator.performActiveAction('forget')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && s.lifecycle === null)
    expect(downloads).toHaveLength(3)
    expect(onActionError).not.toHaveBeenCalled()
    expect(store.tasks.has(failedPersistId)).toBe(false)
    expect(coordinator.getSnapshot().retained.tasks.map(task => task.taskId)).toEqual([firstTaskId])

    await coordinator.dispose()
  })
})

describe('FSA discard permission fallback', () => {
  afterEach(() => vi.restoreAllMocks())

  it('keeps a paused FSA task retryable after PermissionDenied discard and allows explicit record-only forget while blocking OPFS forget', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const parent = new FakeDirectoryHandle('Downloads', [])
    const queryPermission = vi.spyOn(parent, 'queryPermission').mockResolvedValue('prompt')
    const requestPermission = vi.spyOn(parent, 'requestPermission').mockResolvedValue('denied')
    const pausedFsaLifecycle = transitionReceiveState(initialReceiveState('fsa-paused'), {
      kind: 'paused',
      reason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
      retainedBytes: 4n,
      discoveredFiles: 1n,
      discoveredBytes: 8n,
      discoveryComplete: true,
    })
    const failedFsaLifecycle = transitionReceiveState(initialReceiveState('fsa-failed'), {
      kind: 'failed',
      reason: 'error',
      detail: 'io failed',
    })
    const pausedOpfsLifecycle = transitionReceiveState(initialReceiveState('opfs-paused'), {
      kind: 'paused',
      reason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
      retainedBytes: 4n,
      discoveredFiles: 1n,
      discoveredBytes: 8n,
      discoveryComplete: true,
    })

    // Default actions still prioritize discard; only FSA paused/failed permit record-only forget fallback.
    expect(actionsForState(pausedFsaLifecycle, 'fsa')).toEqual(['continue', 'discard'])
    expect(actionsForState(failedFsaLifecycle, 'fsa')).toEqual(['discard'])
    expect(canForgetState(pausedFsaLifecycle, 'fsa')).toBe(true)
    expect(canForgetState(failedFsaLifecycle, 'fsa')).toBe(true)
    expect(canForgetState(pausedOpfsLifecycle, 'opfs')).toBe(false)

    const baseSelection = { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] }
    const fsaRecord: ReceiveTaskRecord = {
      taskId: 'fsa-paused',
      shareInstance: 'share-1',
      selectionKey: 'sel-fsa',
      selection: baseSelection,
      artifact: { kind: 'directory-tree', name: 'Album', rootPath: ['Album'] },
      backend: 'fsa',
      lifecycle: pausedFsaLifecycle,
      target: {
        kind: 'fsa',
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        reservedName: 'Album',
        collisionIndex: 0,
        singleFile: false,
        reservedAtMs: 1_000,
      },
      discoveryComplete: true,
      updatedAt: 2_000,
    }
    const opfsRecord: ReceiveTaskRecord = {
      taskId: 'opfs-paused',
      shareInstance: 'share-1',
      selectionKey: 'sel-opfs',
      selection: baseSelection,
      artifact: { kind: 'zip-archive', name: 'Album.zip', rootPath: ['Album'] },
      backend: 'opfs',
      lifecycle: pausedOpfsLifecycle,
      target: { kind: 'opfs', objectName: 'opfs-paused.part', fileName: 'Album.zip', isZip: true, allocatedLength: 8n },
      discoveryComplete: true,
      updatedAt: 1_000,
    }
    await store.putTask(fsaRecord)
    await store.putFile({
      id: 'fsa-paused:f1',
      taskId: 'fsa-paused',
      fileId: 'f1',
      revision: 'rev-1',
      path: ['part.bin'],
      size: 8n,
      completedRanges: [[0n, 4n]],
      status: 'receiving',
    })
    await store.putTask(opfsRecord)

    const traces: OutputTraceEvent[] = []
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => undefined,
      outputTrace: { current: event => { traces.push(event) } },
    })
    await coordinator.loadRetained()

    const fsaTask = coordinator.getSnapshot().retained.tasks.find(task => task.taskId === 'fsa-paused')!
    const opfsTask = coordinator.getSnapshot().retained.tasks.find(task => task.taskId === 'opfs-paused')!
    expect(presentTask(retainedTaskFacts(fsaTask)).removal?.fallbackForget?.target).toEqual({
      kind: 'retained',
      task: fsaTask,
      action: 'forget',
    })
    expect(presentTask(retainedTaskFacts(opfsTask)).removal?.fallbackForget).toBeNull()
    expect(coordinator.retainedActionAdmission(fsaTask, 'forget')).toEqual({ allowed: true, reason: null })
    expect(coordinator.retainedActionAdmission(opfsTask, 'forget')).toEqual({ allowed: false, reason: 'actionUnavailable' })

    const discardFailed = deferred<unknown>()
    coordinator.performRetainedAction(fsaTask, 'discard', { onError: error => { discardFailed.resolve(error) } })
    const discardError = await discardFailed.promise
    expect(coordinator.getSnapshot().retained.pending).toBeNull()
    expect(isPermissionDeniedError(discardError)).toBe(true)
    expect(mapReceiverFailure(discardError, err => String(err))).toMatchObject({ kind: 'permissionDenied' })
    expect(queryPermission).toHaveBeenCalledOnce()
    expect(requestPermission).toHaveBeenCalledOnce()
    expect(store.tasks.has('fsa-paused')).toBe(true)
    expect(await store.listTaskFiles('fsa-paused')).toHaveLength(1)
    expect(traces).toContainEqual({
      eventName: 'cleanup',
      payload: { backend: 'file_system_access', task_id: 'fsa-paused', transition: 'permission_denied' },
    })

    // Explicit fallback forget removes the FSA record without prompting for folder permission again.
    coordinator.performRetainedAction(fsaTask, 'forget')
    await waitForSnapshot(coordinator, s => s.retained.pending === null && !s.retained.tasks.some(t => t.taskId === 'fsa-paused'))
    expect(requestPermission).toHaveBeenCalledOnce()
    expect(store.tasks.has('fsa-paused')).toBe(false)
    expect(await store.listTaskFiles('fsa-paused')).toHaveLength(0)
    expect(traces).toContainEqual({
      eventName: 'cleanup',
      payload: { backend: 'file_system_access', task_id: 'fsa-paused', transition: 'forgotten_without_disk_cleanup' },
    })

    // OPFS retained tasks still reject forget at the storage-command boundary to prevent browser-quota leaks.
    await expect(executeRetainedTaskAction(
      { store: store as unknown as ReceiveStore, windowPort },
      'opfs-paused',
      'forget',
    )).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(store.tasks.has('opfs-paused')).toBe(true)

    await coordinator.dispose()
  })

  it('retries unfinished FSA cleanup on partial(cleanupPending) tasks through ReceiveCoordinator and preserves completed files', async () => {
    const store = new FSAMemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const parent = new FakeDirectoryHandle('Downloads', [])
    const folder = await parent.getDirectoryHandle('Album', { create: true })
    const savedHandle = await folder.getFileHandle('done.txt', { create: true })
    const savedWriter = await savedHandle.createWritable()
    await savedWriter.write({ type: 'write', position: 0, data: Uint8Array.of(1, 2, 3, 4) })
    await savedWriter.close()
    const unfinishedHandle = await folder.getFileHandle('part.bin', { create: true })
    const unfinishedWriter = await unfinishedHandle.createWritable()
    await unfinishedWriter.write({ type: 'write', position: 0, data: Uint8Array.of(9, 8) })
    await unfinishedWriter.close()
    const unfinishedDisk = await unfinishedHandle.getFile()

    const partialPending = transitionReceiveState(initialReceiveState('fsa-partial-pending'), {
      kind: 'partial',
      reason: 'stopped',
      savedFiles: 1n,
      failedFiles: 0n,
      cleanupPending: true,
    })
    await store.putTask({
      taskId: 'fsa-partial-pending',
      shareInstance: 'share-1',
      selectionKey: 'sel-partial',
      selection: { shareInstance: 'share-1', syntheticRoot: 'root-1', defaultSelected: true, rules: [] },
      artifact: { kind: 'directory-tree', name: 'Album', rootPath: ['Album'] },
      backend: 'fsa',
      lifecycle: partialPending,
      target: {
        kind: 'fsa',
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        reservedName: 'Album',
        collisionIndex: 0,
        singleFile: false,
        reservedAtMs: 1_000,
      },
      discoveryComplete: true,
      updatedAt: 2_000,
    })
    await store.putFile({
      id: 'fsa-partial-pending:done',
      taskId: 'fsa-partial-pending',
      fileId: 'done',
      revision: 'rev-1',
      path: ['done.txt'],
      fsaPath: ['done.txt'],
      size: 4n,
      completedRanges: [[0n, 4n]],
      status: 'completed',
      handle: savedHandle as unknown as FileSystemFileHandle,
    })
    await store.putFile({
      id: 'fsa-partial-pending:part',
      taskId: 'fsa-partial-pending',
      fileId: 'part',
      revision: 'rev-2',
      path: ['part.bin'],
      fsaPath: ['part.bin'],
      size: 8n,
      completedRanges: [[0n, 2n]],
      status: 'receiving',
      handle: unfinishedHandle as unknown as FileSystemFileHandle,
      fsaDiskFingerprint: { size: unfinishedDisk.size, lastModified: unfinishedDisk.lastModified },
    })

    const queryPermission = vi.spyOn(parent, 'queryPermission').mockResolvedValueOnce('prompt')
    const requestPermission = vi.spyOn(parent, 'requestPermission').mockResolvedValueOnce('denied')
    const { windowPort: baseWin } = createMockDownloadWindow()
    const coordinator = new ReceiveCoordinator({
      windowPort: { ...baseWin, indexedDB: { open: vi.fn() } as unknown as IDBFactory },
      currentJoinedShare: () => undefined,
    })
    await coordinator.loadRetained()

    const task = coordinator.getSnapshot().retained.tasks.find(entry => entry.taskId === 'fsa-partial-pending')!
    expect(task.actions).toEqual(['discard'])
    expect(coordinator.retainedActionAdmission(task, 'forget')).toEqual({ allowed: true, reason: null })

    // Permission still denied keeps the task retryable and leaves files untouched.
    const denied = deferred<unknown>()
    coordinator.performRetainedAction(task, 'discard', { onError: error => { denied.resolve(error) } })
    expect(isPermissionDeniedError(await denied.promise)).toBe(true)
    expect(queryPermission).toHaveBeenCalledOnce()
    expect(requestPermission).toHaveBeenCalledOnce()
    expect(folder.children.has('part.bin')).toBe(true)
    expect(store.tasks.has('fsa-partial-pending')).toBe(true)

    // Restoring directory permission lets discard clean the unfinished file while keeping done.txt intact.
    let retryError: unknown
    coordinator.performRetainedAction(task, 'discard', { onError: error => { retryError = error } })
    await waitForSnapshot(coordinator, s => s.retained.pending === null && !s.retained.tasks.some(t => t.taskId === 'fsa-partial-pending'))
    expect(retryError).toBeUndefined()
    expect(folder.children.has('part.bin')).toBe(false)
    expect([...(await folder.getFileHandle('done.txt')).bytes]).toEqual([1, 2, 3, 4])
    expect(store.tasks.has('fsa-partial-pending')).toBe(false)
    expect(await store.listTaskFiles('fsa-partial-pending')).toEqual([])

    await coordinator.dispose()
  })
})
