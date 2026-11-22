import { describe, expect, it, vi } from 'vitest'
import { actionsForState, admitRetainedAction, retainedTaskReadiness } from '../../src/application/receive/admission'
import type { RetainedTaskSnapshot } from '../../src/application/receive/contracts'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { SelectionPolicy } from '../../src/catalog/selection'
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
})
