import type { SaveBackendKind } from '../../output/model'
import {
  isActiveReceiveState,
  type ReceiveLifecycleState,
} from '../../transfer/state'
import type { ReceiveAction, ReceiveAdmission, RetainedReadiness, RetainedTaskSnapshot } from './contracts'

export const ALLOWED_ADMISSION: ReceiveAdmission = Object.freeze({ allowed: true, reason: null })

export function blockedAdmission(reason: NonNullable<ReceiveAdmission['reason']>): ReceiveAdmission {
  return Object.freeze({ allowed: false, reason })
}

export function canSavePartialState(
  state: ReceiveLifecycleState | null,
  backend: SaveBackendKind | null,
  isZipArchive: boolean,
): boolean {
  if (state === null || backend !== 'opfs' || !isZipArchive) return false
  if (state.kind === 'paused') return state.completedFiles > 0n
  if (state.kind === 'partial') return state.savedFiles > 0n
  return false
}

export function actionsForState(
  state: ReceiveLifecycleState,
  backend: SaveBackendKind,
  canSavePartial = false,
): readonly ReceiveAction[] {
  switch (state.kind) {
    case 'preparing':
      return Object.freeze([])
    case 'downloading':
      return interruptionActions(state.phase, backend)
    case 'paused':
      return Object.freeze([
        'continue',
        ...(canSavePartial ? ['save-partial' as const] : []),
        'discard',
      ])
    case 'ready-to-save':
      return Object.freeze(['save', 'discard'])
    case 'download-started':
      return state.retained
        ? Object.freeze(['save', 'discard'])
        : Object.freeze(['forget'])
    case 'saved':
      return Object.freeze(['forget'])
    case 'partial':
      // OPFS partial members stay inside browser storage until exported or explicitly discarded,
      // unlike FSA partial results whose completed files already live in the user's folder.
      if (backend === 'opfs') {
        return Object.freeze([
          ...(canSavePartial ? ['save-partial' as const] : []),
          'discard',
        ])
      }
      return Object.freeze(['forget'])
    case 'failed':
      return state.reason === 'source-changed' || backend !== 'portable'
        ? Object.freeze(['discard'])
        : Object.freeze(['forget'])
  }
}

function interruptionActions(phase: 'receiving' | 'finalizing', backend: SaveBackendKind): readonly ReceiveAction[] {
  if (phase === 'finalizing') return Object.freeze([])
  return backend === 'portable' ? Object.freeze(['stop']) : Object.freeze(['pause', 'stop'])
}

export function isLocalOnlyAction(action: ReceiveAction): boolean {
  return action === 'save' ||
    action === 'save-partial' ||
    action === 'discard' ||
    action === 'forget'
}

export function retainedTaskReadiness(
  task: Pick<RetainedTaskSnapshot, 'shareInstance' | 'lifecycle'>,
  currentShareInstance: string | null,
): RetainedReadiness {
  if (task.lifecycle.kind !== 'paused') return 'local'
  if (currentShareInstance === null) return 'original-link-required'
  return task.shareInstance === currentShareInstance ? 'matching-share' : 'different-share'
}

export function admitRetainedAction(
  task: RetainedTaskSnapshot,
  action: ReceiveAction,
  context: Readonly<{
    currentShareInstance: string | null
    hasActiveDownload: boolean
    hasPendingAction: boolean
    connectionUnavailable: boolean
    shareEnded: boolean
  }>,
): ReceiveAdmission {
  if (!task.actions.includes(action)) return blockedAdmission('actionUnavailable')
  if (task.activeElsewhere) return blockedAdmission('activeInAnotherTab')
  if (context.hasPendingAction) return blockedAdmission('busy')
  if (isLocalOnlyAction(action)) return ALLOWED_ADMISSION
  if (context.hasActiveDownload) return blockedAdmission('downloading')
  const readiness = retainedTaskReadiness(task, context.currentShareInstance)
  if (readiness === 'original-link-required' || readiness === 'different-share') {
    return blockedAdmission('matchingShare')
  }
  if (context.shareEnded) return blockedAdmission('shareEnded')
  if (context.connectionUnavailable) return blockedAdmission('connectionUnavailable')
  return ALLOWED_ADMISSION
}

export function canStartNewDownload(lifecycle: ReceiveLifecycleState | null): boolean {
  return lifecycle === null || !isActiveReceiveState(lifecycle)
}

export function canReleaseCurrentTask(lifecycle: ReceiveLifecycleState | null): boolean {
  return lifecycle !== null && (lifecycle.kind === 'failed' || lifecycle.kind === 'paused')
}
