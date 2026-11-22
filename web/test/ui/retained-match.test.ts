import { describe, expect, it } from 'vitest'
import type { ReceiveCompositionPort, RetainedReceiveActionResult, RetainedReceiveOperation } from '../../src/application/receive/contracts'
import { RetainedInventoryCoordinator } from '../../src/application/receive/retained/coordinator'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { ArtifactOffers } from '../../src/output/model/artifacts/offers'
import type { ProjectionEpoch } from '../../src/transfer/projection/contracts'
import { composeTasks, type RetainedAdmission } from '../../src/ui/experience/task-composition'
import { resolveWorkspaceTaskLayout, type WorkspaceTaskSnapshot } from '../../src/ui/experience/retained-match'
import { EMPTY_OUTPUT_PRESENTATION } from '../../src/ui/output'
import { EMPTY_RETAINED_INVENTORY } from '../../src/application/receive/retained/model'
import { experienceSnapshot } from './receiver-experience-fixture'
import { identityText } from './v2-receiver-orchestration-fixture'

const SHARE_ID = identityText(1)
const OTHER_SHARE_ID = identityText(2)
const SELECTION_A = identityText(3, 32)
const SELECTION_B = identityText(4, 32)

function workspaceHandoff(operationId: string, partial = false): ReceiveLifecycleState {
  return Object.freeze({
    operationId,
    receiveIntentDigest: identityText(10, 32),
    generation: 3n,
    kind: 'download-started' as const,
    attemptKind: 'workspace' as const,
    attemptId: identityText(11),
    packageDigest: identityText(12, 32),
    ...(partial ? {
      contentWarning: {
        kind: 'partial-zip' as const,
        selectedFileCount: 2n,
        completedFileCount: 1n,
        missingFiles: [{ path: ['b.txt'], reason: 'source-changed' as const }],
      },
    } : {}),
  })
}

function retainedOp(overrides: Partial<RetainedReceiveOperation> & {
  readonly operationId: string
  readonly lifecycle: ReceiveLifecycleState
  readonly continuation: RetainedReceiveOperation['continuation']
  readonly actions: RetainedReceiveOperation['actions']
}): RetainedReceiveOperation {
  return Object.freeze({
    receiveIntentDigest: overrides.lifecycle.receiveIntentDigest,
    lifecycleGeneration: overrides.lifecycle.generation,
    shareInstance: SHARE_ID,
    selectionDigest: SELECTION_A,
    display: Object.freeze({ objectLabel: 'photo.jpg', createdAtMilliseconds: 1_000 }),
    ...overrides,
  })
}

const EPOCH_ONE = 1n as ProjectionEpoch

function offersFor(selectionDigest: string): ArtifactOffers {
  return Object.freeze({
    kind: 'confirming-selected-content',
    interactive: false,
    projectionEpoch: EPOCH_ONE,
    selectionDigest,
    reason: 'shape-unsettled',
    decision: Object.freeze({
      name: 'receive.offer.disabled',
      projection_epoch: EPOCH_ONE,
      shape_proof: 'unknown',
      offer_unavailable_reason: 'shape-unsettled',
    }),
  })
}

function workspaceSnapshot(
  patch: Partial<Parameters<typeof experienceSnapshot>[0]> = {},
  admission?: RetainedAdmission,
): WorkspaceTaskSnapshot {
  const base = experienceSnapshot({
    share: {
      kind: 'photo',
      shareInstance: SHARE_ID,
      name: 'photo.jpg',
      file: { id: 'f1', kind: 'file', name: 'photo.jpg', expectedSize: 1024n, selection: 'selected' },
    },
    output: { ...EMPTY_OUTPUT_PRESENTATION, offers: offersFor(SELECTION_A) },
    retained: { ...EMPTY_RETAINED_INVENTORY, kind: 'available' },
    ...patch,
  })
  const composed = composeTasks(base, admission)
  return Object.freeze({
    output: base.output,
    progress: base.progress,
    taskDisplay: base.taskDisplay,
    connection: base.connection,
    share: base.share,
    retained: base.retained,
    activeReceiveOperationId: base.activeReceiveOperationId,
    startAdmission: base.startAdmission,
    draft: base.draft,
    hasContent: true,
    ...composed,
  })
}

describe('resolveWorkspaceTaskLayout', () => {
  it('promotes a complete retained OPFS artifact after reload and collapses remote redownload', () => {
    const lifecycle = workspaceHandoff(identityText(20))
    const operation = retainedOp({
      operationId: lifecycle.operationId,
      lifecycle,
      continuation: 'retry-download',
      actions: ['redownload', 'delete'],
    })
    const layout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: { kind: 'available', pending: null, operations: [operation] },
    }))
    expect(layout.primaryTask?.operationId).toBe(operation.operationId)
    expect(layout.primaryTask?.primaryAction?.id).toBe('redownload')
    expect(layout.collapseRemoteSaving).toBe(true)
    expect(layout.repeatRemoteLabel).toBe(true)
    expect(layout.banner).toBeNull()
  })

  it.each([
    {
      label: 'retry-download',
      lifecycle: workspaceHandoff(identityText(29)),
      continuation: 'retry-download' as const,
      actions: ['redownload', 'delete'] as const,
      action: 'redownload' as const,
    },
    {
      label: 'save-artifact',
      lifecycle: Object.freeze({
        operationId: identityText(30),
        receiveIntentDigest: identityText(31, 32),
        generation: 3n,
        kind: 'waiting-to-save' as const,
        packageDigest: identityText(32, 32),
      }),
      continuation: 'save-artifact' as const,
      actions: ['save', 'discard'] as const,
      action: 'save' as const,
    },
  ])('keeps the $label local task visible and remote saving collapsed while re-export is in progress', async ({
    lifecycle,
    continuation,
    actions,
    action,
  }) => {
    const operation = retainedOp({
      operationId: lifecycle.operationId,
      lifecycle,
      continuation,
      actions,
    })
    let finishExport!: (result: RetainedReceiveActionResult) => void
    const exportGate = new Promise<RetainedReceiveActionResult>(resolve => {
      finishExport = resolve
    })
    const coordinator = new RetainedInventoryCoordinator({
      receive: {
        retained: {
          list: async () => Object.freeze({
            operations: Object.freeze([operation]),
            presentationFailures: Object.freeze([]),
            act: () => exportGate,
            close: () => undefined,
          }),
        },
      } as unknown as ReceiveCompositionPort,
      isDisposed: () => false,
      currentJoinedShare: () => undefined,
      continuationBlocked: () => false,
      adoptContinuation: async () => undefined,
      ownsRuntime: () => false,
      publish: () => undefined,
      onActionError: () => undefined,
    })
    const admission: RetainedAdmission = (candidate, candidateAction) =>
      coordinator.actionAdmission(candidate, candidateAction)

    await coordinator.load()
    const before = resolveWorkspaceTaskLayout(
      workspaceSnapshot({ retained: coordinator.getSnapshot() }, admission),
    )
    expect(before.primaryTask?.operationId).toBe(operation.operationId)
    expect(before.primaryTask?.primaryAction?.id).toBe(action)
    expect(before.primaryTask?.primaryAction?.disabledReason).toBeNull()
    expect(before.collapseRemoteSaving).toBe(true)

    coordinator.perform(coordinator.getSnapshot().operations[0]!, action)
    const during = resolveWorkspaceTaskLayout(
      workspaceSnapshot({ retained: coordinator.getSnapshot() }, admission),
    )
    expect(during.primaryTask?.operationId).toBe(operation.operationId)
    expect(during.primaryTask?.primaryAction?.id).toBe(action)
    expect(during.primaryTask?.primaryAction?.disabledReason).not.toBeNull()
    expect(during.collapseRemoteSaving).toBe(true)
    expect(during.repeatRemoteLabel).toBe(true)
    expect(during.banner).toBeNull()

    finishExport(Object.freeze({ kind: 'completed' }))
    await Promise.resolve()
    coordinator.close(new DOMException('test complete', 'AbortError'))
  })

  it('does not promote or collapse remote saving when retained actions are read-only', () => {
    const lifecycle = workspaceHandoff(identityText(33))
    const operation = retainedOp({
      operationId: lifecycle.operationId,
      lifecycle,
      continuation: 'retry-download',
      actions: [],
      unavailableReason: 'noSavedAuthority',
    })
    const layout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: { kind: 'available', pending: null, operations: [operation] },
    }))
    expect(layout.primaryTask).toBeNull()
    expect(layout.collapseRemoteSaving).toBe(false)
    expect(layout.banner).toBeNull()
  })

  it('matches folder shares only when selectionDigest equals the current selection', () => {
    const lifecycle = workspaceHandoff(identityText(21))
    const operation = retainedOp({
      operationId: lifecycle.operationId,
      lifecycle,
      continuation: 'retry-download',
      actions: ['redownload', 'delete'],
      selectionDigest: SELECTION_A,
    })
    const folderShare = {
      kind: 'browser' as const,
      shareInstance: SHARE_ID,
      name: 'Photos',
      homeDirectoryId: 'root',
      singleFolder: true,
    }
    const matching = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: folderShare,
      output: { ...EMPTY_OUTPUT_PRESENTATION, offers: offersFor(SELECTION_A) },
      retained: { kind: 'available', pending: null, operations: [operation] },
    }))
    expect(matching.primaryTask?.operationId).toBe(operation.operationId)
    expect(matching.collapseRemoteSaving).toBe(true)

    const differentSelection = resolveWorkspaceTaskLayout(workspaceSnapshot({
      share: folderShare,
      output: { ...EMPTY_OUTPUT_PRESENTATION, offers: offersFor(SELECTION_B) },
      retained: { kind: 'available', pending: null, operations: [operation] },
    }))
    expect(differentSelection.primaryTask).toBeNull()
    expect(differentSelection.collapseRemoteSaving).toBe(false)
    expect(differentSelection.banner).toBeNull()
  })

  it('does not collapse remote saving for a partial retained ZIP', () => {
    const lifecycle = workspaceHandoff(identityText(22), true)
    const operation = retainedOp({
      operationId: lifecycle.operationId,
      lifecycle,
      continuation: 'retry-download',
      actions: ['redownload', 'delete'],
    })
    const layout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: { kind: 'available', pending: null, operations: [operation] },
    }))
    expect(layout.primaryTask).toBeNull()
    expect(layout.collapseRemoteSaving).toBe(false)
    expect(layout.banner).toBeNull()
  })

  it('keeps remote saving collapsed while a just-completed workspace handoff reloads retained inventory', () => {
    const lifecycle = workspaceHandoff(identityText(23))
    const layout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      activeReceiveOperationId: null,
      output: { ...EMPTY_OUTPUT_PRESENTATION, lifecycle, offers: offersFor(SELECTION_A) },
      retained: { kind: 'loading', pending: null, operations: [] },
    }))
    expect(layout.primaryTask?.operationId).toBe(lifecycle.operationId)
    expect(layout.primaryTask?.primaryAction).toBeNull()
    expect(layout.collapseRemoteSaving).toBe(true)
    expect(layout.repeatRemoteLabel).toBe(true)
  })

  it('distinguishes paused continuation banners from ready-to-save banners and ignores other shares', () => {
    const pausedLifecycle: ReceiveLifecycleState = {
      operationId: identityText(24),
      receiveIntentDigest: identityText(25, 32),
      generation: 2n,
      kind: 'resumable-start',
      reason: 'paused',
    }
    const pausedOp = retainedOp({
      operationId: pausedLifecycle.operationId,
      lifecycle: pausedLifecycle,
      continuation: 'resume-start',
      actions: ['continue', 'discard'],
    })
    const pausedLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: { kind: 'available', pending: null, operations: [pausedOp] },
    }))
    expect(pausedLayout.primaryTask).toBeNull()
    expect(pausedLayout.banner?.kind).toBe('continue')
    expect(pausedLayout.banner?.task.operationId).toBe(pausedOp.operationId)

    const readyLifecycle: ReceiveLifecycleState = {
      operationId: identityText(26),
      receiveIntentDigest: identityText(27, 32),
      generation: 2n,
      kind: 'waiting-to-save',
      packageDigest: identityText(28, 32),
    }
    const readyDifferentSelection = retainedOp({
      operationId: readyLifecycle.operationId,
      lifecycle: readyLifecycle,
      continuation: 'save-artifact',
      actions: ['save', 'discard'],
      selectionDigest: SELECTION_B,
    })
    const readyLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: { kind: 'available', pending: null, operations: [readyDifferentSelection] },
    }))
    expect(readyLayout.primaryTask).toBeNull()
    expect(readyLayout.banner?.kind).toBe('ready-to-save')

    const otherShareLayout = resolveWorkspaceTaskLayout(workspaceSnapshot({
      retained: {
        kind: 'available',
        pending: null,
        operations: [{ ...pausedOp, shareInstance: OTHER_SHARE_ID }],
      },
    }))
    expect(otherShareLayout.banner).toBeNull()
  })
})
