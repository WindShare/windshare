import { describe, expect, it, vi } from 'vitest'
import {
  actionsForState,
  admitRetainedAction,
  canSavePartialState,
  retainedTaskReadiness,
} from '../../src/application/receive/admission'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import type { RetainedTaskSnapshot } from '../../src/application/receive/contracts'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { SelectionPolicy, type FrozenSelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'

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
})
