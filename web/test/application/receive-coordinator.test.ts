import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  actionsForState,
  admitRetainedAction,
  canSavePartialState,
  retainedTaskReadiness,
} from '../../src/application/receive/admission'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import type { RetainedTaskSnapshot } from '../../src/application/receive/contracts'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import { retainedSnapshot } from '../../src/application/receive/retained'
import * as sessionOpener from '../../src/application/receive/session-opener'
import {
  ReceiveBlockedError,
  SaveUnavailableError,
  decideSingleFileReuse,
  planSingleFileDownload,
} from '../../src/application/receive/single-file'
import type { BrowseDirectory, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy, type FrozenSelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserOutputWindow, OutputSessionHandle } from '../../src/output/backends/contract'
import * as outputContract from '../../src/output/backends/contract'
import type { NativeSupportWorker } from '../../src/output/native-object/support'
import { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle } from '../output/fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'

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
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))

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

    let jobSelection: FrozenSelectionPolicy | undefined
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
        jobSelection = options?.selection
        return {
          pause: vi.fn(),
          stop: vi.fn(),
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
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    expect(coordinator.startDownload(offers.primary.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobSelection).toBeDefined())
    expect(jobSelection?.draftRules?.[0]?.ancestry).toEqual(['root-1', 'sub-1'])

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
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))

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
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('retry-required'))

    coordinator.retryOffers()
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))
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

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function makeFileEntry(seed: number, name: string, expectedSize: bigint): Extract<CatalogEntry, { kind: 'file' }> {
  const id = new Uint8Array(16).fill(seed)
  return Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize })
}

function makeSubDirectory(rootId: string, dirSeed: number, name: string): BrowseDirectory {
  const id = new Uint8Array(16).fill(dirSeed)
  const idText = encodeBase64Url(id)
  return Object.freeze({
    id,
    idText,
    path: Object.freeze([name]),
    ancestry: Object.freeze([rootId, idText]),
  })
}

function singleFileSelectionKey(
  shareInstance: string,
  syntheticRoot: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
) {
  const policy = new SelectionPolicy(false)
  policy.set(entry, directory.ancestry, true)
  const spec = selectionSpecFromPolicy(shareInstance, syntheticRoot, policy.snapshot())
  return { spec, key: selectionKey(spec) }
}

function makeJobResult(status: 'completed' | 'paused' = 'completed', size = 16n) {
  return status === 'paused'
    ? {
        status: 'paused' as const,
        pauseReason: 'user' as const,
        completedFiles: 0n,
        completedBytes: 0n,
        discoveredFiles: 1n,
        discoveredBytes: size,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      }
    : {
        status: 'completed' as const,
        completedFiles: 1n,
        completedBytes: size,
        discoveredFiles: 1n,
        discoveredBytes: size,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      }
}

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
    await vi.waitFor(() => expect(coordinator.getSnapshot().capabilitiesState).toBe('ready'))

    let publishedActiveBeforeCallback = false
    const outcomeSmall = coordinator.startSingleFileDownload(entrySmall, dir, {
      onActivated: () => {
        publishedActiveBeforeCallback =
          coordinator.getSnapshot().lifecycle?.kind === 'downloading' &&
          coordinator.getSnapshot().activeSingleFileId === entrySmall.idText
        events.push('activated')
      },
    })
    expect(outcomeSmall).toEqual({ kind: 'started' })
    expect(draftSelection.snapshot().canonicalRules).toHaveLength(1)
    expect(draftSelection.selected(entrySmall, dir.ancestry)).toBe(false)

    await vi.waitFor(() => expect(events).toEqual(['activated']))
    expect(publishedActiveBeforeCallback).toBe(true)
    expect(coordinator.getSnapshot().backend).toBe('portable')

    jobGate.resolve(makeJobResult('completed', entrySmall.expectedSize))
    await vi.waitFor(() => expect(coordinator.getSnapshot().activeSingleFileId).toBeNull())

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
        return { pause: vi.fn(), stop: vi.fn(), run: () => gate.promise }
      },
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({ windowPort, currentJoinedShare: () => joined, onActionError })
    coordinator.refreshOffers(joined)
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))

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

    // Complete B with a pause, then resume B and verify activeSingleFileId tracks resume too.
    const events: string[] = []
    pickers[1]!.resolve(new FakeDirectoryHandle('Downloads', events) as unknown as FileSystemDirectoryHandle)
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('downloading'))
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)

    jobGates[0]!.resolve(makeJobResult('paused', fileB.expectedSize))
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('paused'))
    expect(coordinator.getSnapshot().activeSingleFileId).toBeNull()

    // Resume via quick download (also exercises reusing just-paused task immediately).
    expect(coordinator.startSingleFileDownload(fileB, dir)).toEqual({ kind: 'resumed' })
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('downloading'))
    expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText)

    jobGates[1]!.resolve(makeJobResult('completed', fileB.expectedSize))
    await vi.waitFor(() => expect(coordinator.getSnapshot().activeSingleFileId).toBeNull())
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
    const staleClose = vi.fn(async () => undefined)
    const secondJobGate = deferred<ReturnType<typeof makeJobResult>>()
    let openCalls = 0
    vi.spyOn(sessionOpener, 'openNewOutputSession').mockImplementation(async input => {
      openCalls += 1
      if (openCalls === 1) return firstSessionGate.promise
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
    coordinator.preloadShareTasks('share-1')
    await vi.waitFor(() => expect(coordinator.getSnapshot().capabilitiesState).toBe('ready'))

    expect(coordinator.startSingleFileDownload(fileA, dir)).toEqual({ kind: 'started' })
    await vi.waitFor(() => expect(openCalls).toBe(1))
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
    await vi.waitFor(() => expect(staleClose).toHaveBeenCalledTimes(1))
    await vi.waitFor(() => expect(coordinator.getSnapshot().activeSingleFileId).toBe(fileB.idText))

    secondJobGate.resolve(makeJobResult('completed', fileB.expectedSize))
    await vi.waitFor(() => expect(coordinator.getSnapshot().activeSingleFileId).toBeNull())
    await coordinator.dispose()
  })
})

describe('single-file reuse, errors, and local blob handoff', () => {
  afterEach(() => vi.restoreAllMocks())

  it('reuses completed OPFS over paused during concurrent index preload, fires onExported after export completes, and reports blocked resume reasons', async () => {
    const file = makeFileEntry(30, 'report.pdf', 12n)
    const dir = makeSubDirectory('root-1', 4, 'Reports')
    const { spec, key } = singleFileSelectionKey('share-1', 'root-1', file, dir)

    const store = new MemoryReceiveStore()
    const listGate = deferred<readonly ReceiveTaskRecord[]>()
    vi.spyOn(store, 'listTasksByShare').mockImplementation(() => listGate.promise)
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)

    const { env, root } = createMemoryOPFSEnv()
    const tasksDir = await root.getDirectoryHandle('windshare-tasks', { create: true })
    const opfsFile = await tasksDir.getFileHandle('opfs-new.part', { create: true })
    opfsFile.bytes = new Uint8Array([1, 2, 3, 4])
    const opfsOldFile = await tasksDir.getFileHandle('opfs-old.part', { create: true })
    opfsOldFile.bytes = new Uint8Array([9, 9, 9, 9])

    const makeOpfsReady = (taskId: string, objectName: string, createdAtMilliseconds: number): ReceiveTaskRecord => ({
      taskId,
      shareInstance: 'share-1',
      selectionKey: key,
      selection: spec,
      artifact: { kind: 'original-file', name: file.name, singleFileId: file.idText, singleFileSize: 4n },
      backend: 'opfs',
      lifecycle: transitionReceiveState(initialReceiveState(taskId), {
        kind: 'ready-to-save', size: 4n, fileName: file.name,
      }),
      target: { kind: 'opfs', objectName, fileName: file.name, isZip: false, allocatedLength: 4n, sealedSize: 4n },
      display: { objectLabel: file.name, createdAtMilliseconds },
      discoveryComplete: true,
      updatedAt: createdAtMilliseconds,
    })
    const pausedRecord: ReceiveTaskRecord = {
      taskId: 'paused-newer',
      shareInstance: 'share-1',
      selectionKey: key,
      selection: spec,
      artifact: { kind: 'original-file', name: file.name, singleFileId: file.idText, singleFileSize: 4n },
      backend: 'opfs',
      lifecycle: transitionReceiveState(initialReceiveState('paused-newer'), {
        kind: 'paused', reason: 'user',
        completedFiles: 0n, completedBytes: 0n, retainedBytes: 2n,
        discoveredFiles: 1n, discoveredBytes: 4n, discoveryComplete: true,
      }),
      target: { kind: 'opfs', objectName: 'paused.part', fileName: file.name, isZip: false, allocatedLength: 4n },
      display: { objectLabel: file.name, createdAtMilliseconds: 9_000 },
      discoveryComplete: true,
      updatedAt: 9_000,
    }
    const olderReady = makeOpfsReady('opfs-old', 'opfs-old.part', 1_000)
    const newerReady = makeOpfsReady('opfs-new', 'opfs-new.part', 5_000)
    await store.putTask(olderReady)
    await store.putTask(newerReady)
    await store.putTask(pausedRecord)

    // Verify pure candidate ranking prefers exportable OPFS (newest) over even newer paused task.
    const pureDecision = decideSingleFileReuse(
      [
        retainedSnapshot(pausedRecord, false),
        retainedSnapshot(olderReady, false),
        retainedSnapshot(newerReady, false),
      ],
      'share-1',
      key,
      () => ({ allowed: true, reason: null }),
    )
    expect(pureDecision).toMatchObject({ kind: 'export', task: { taskId: 'opfs-new' } })

    const lockedElsewhereDecision = decideSingleFileReuse(
      [retainedSnapshot(pausedRecord, true)],
      'share-1',
      key,
      (task, action) => admitRetainedAction(task, action, {
        currentShareInstance: 'share-1',
        hasActiveDownload: false,
        hasPendingAction: false,
        connectionUnavailable: false,
        shareEnded: false,
      }),
    )
    expect(lockedElsewhereDecision).toMatchObject({ kind: 'blocked', reason: 'activeInAnotherTab' })

    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort: BrowserOutputWindow = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const joined = {
      descriptor: { shareInstanceId: 'share-1', syntheticRootId: 'root-1' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({ kind: 'single-file' as const, file, sourcePath: ['Reports', file.name], exactBytes: 4n }),
      beginDownloadConnectivity: vi.fn(),
      createTransferJob: vi.fn(),
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      opfsEnv: env,
      currentJoinedShare: () => joined,
    })
    coordinator.preloadShareTasks('share-1')
    await vi.waitFor(() => expect(coordinator.getSnapshot().capabilitiesState).toBe('ready'))

    // First click arrives while listTasksByShare() is still unresolved.
    let exportedCount = 0
    const outcome = coordinator.startSingleFileDownload(file, dir, {
      onExported: () => { exportedCount += 1 },
    })
    expect(outcome).toEqual({ kind: 'started' })
    expect(exportedCount).toBe(0)
    expect(downloads).toHaveLength(0)

    listGate.resolve([pausedRecord, olderReady, newerReady])
    await vi.waitFor(() => expect(exportedCount).toBe(1))
    expect(downloads).toHaveLength(1)
    expect(new Uint8Array(await downloads[0]!.blob.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3, 4]))
    expect(joined.createTransferJob).not.toHaveBeenCalled()

    // Remove OPFS copies so only paused task remains, then verify shareEnded blocks resume explicitly.
    await store.deleteTask('opfs-new')
    await store.deleteTask('opfs-old')
    await coordinator.loadRetained()
    coordinator.observeConnection('ended')
    expect(coordinator.startSingleFileDownload(file, dir)).toEqual({
      kind: 'blocked',
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
    coordinator.preloadShareTasks('share-1')
    await vi.waitFor(() => expect(coordinator.getSnapshot().capabilitiesState).toBe('failed'))

    // Clicking while failed switches back to probing and recovers.
    expect(coordinator.startSingleFileDownload(smallFile, dir)).toEqual({ kind: 'probing' })
    await vi.waitFor(() => expect(coordinator.getSnapshot().capabilitiesState).toBe('ready'))
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
    expect(coordinator.startSingleFileDownload(smallFile, dir, {
      onAsyncError: error => asyncErrors.push(error),
    })).toEqual({ kind: 'started' })
    await vi.waitFor(() => expect(asyncErrors).toHaveLength(1))
    expect(onActionError).not.toHaveBeenCalled()
    expect(coordinator.getSnapshot().lifecycle).toBeNull()

    // Post-activation transfer error routes to normal lifecycle failure + onActionError.
    failConnectivity = false
    const asyncErrors2: unknown[] = []
    expect(coordinator.startSingleFileDownload(smallFile, dir, {
      onAsyncError: error => asyncErrors2.push(error),
    })).toEqual({ kind: 'started' })
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('failed'))
    expect(asyncErrors2).toHaveLength(0)
    expect(onActionError).toHaveBeenCalledTimes(1)

    // Also sanity-check pure plan helper leaves ReceiveBlockedError type intact.
    expect(new ReceiveBlockedError('shareEnded').reason).toBe('shareEnded')
    expect(planSingleFileDownload(joined, smallFile, dir, {
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }).offers.kind).toBe('ready')
    await coordinator.dispose()
  })

  it('saveLocalBlob saves synchronously without claiming transfer slot and completeLocalBlobDownload records non-retained handoff', async () => {
    const { windowPort, downloads } = createMockDownloadWindow()
    const joined = createSingleFileShare('photo.png')
    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
    })

    const blob = new Blob([Uint8Array.of(137, 80, 78, 71)], { type: 'image/png' })
    expect(coordinator.saveLocalBlob(blob, 'photo.png')).toBe(true)
    expect(downloads).toHaveLength(1)
    expect(downloads[0]!.fileName).toBe('photo.png')
    expect(coordinator.active).toBe(false)
    expect(coordinator.getSnapshot().lifecycle).toBeNull()

    expect(coordinator.completeLocalBlobDownload(blob, 'photo.png', {
      objectLabel: 'photo.png',
      createdAtMilliseconds: 1234,
    })).toBe(true)
    expect(downloads).toHaveLength(2)
    expect(coordinator.getSnapshot().lifecycle).toMatchObject({
      kind: 'download-started',
      retained: false,
      fileName: 'photo.png',
    })
    expect(coordinator.getSnapshot().backend).toBe('portable')
    expect(coordinator.active).toBe(false)
    await coordinator.dispose()
  })
})
