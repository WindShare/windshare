import type { ReceiverConnectionSnapshot } from '../../receiver/connection-state'
import type { SaveBackendKind } from '../../output/model'
import {
  isActiveReceiveState,
  type ReceiveLifecycleState,
} from '../../transfer/state'
import type { ReceiveAction, ReceiveAdmission, RetainedReadiness, RetainedTaskSnapshot, TerminalConnectionBlock } from './contracts'

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
      // When an FSA partial settlement had to defer unfinished-file cleanup, expose `discard`
      // so the user can retry removing unfinished files while keeping completed files intact.
      if (backend === 'opfs') {
        return Object.freeze([
          ...(canSavePartial ? ['save-partial' as const] : []),
          'discard',
        ])
      }
      if (backend === 'fsa' && state.cleanupPending) {
        return Object.freeze(['discard'])
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
  switch (backend) {
    case 'portable':
      return Object.freeze(['stop'])
    case 'opfs':
      // Stopping cannot reclaim unfinished slots inside a shared OPFS archive; pausing keeps
      // progress resumable and exposes explicit Save partial ZIP and Discard controls.
      return Object.freeze(['pause'])
    case 'fsa':
      return Object.freeze(['pause', 'stop'])
  }
}

export function isLocalOnlyAction(action: ReceiveAction): boolean {
  return action === 'save' ||
    action === 'save-partial' ||
    action === 'discard' ||
    action === 'forget'
}

/**
 * Whether removing only the IndexedDB history record (`forget`) is safe for this state/backend.
 *
 * OPFS retains opaque browser-private files that users cannot delete in their OS file manager,
 * so forgetting an OPFS task with retained data would leak browser quota. FSA writes into a
 * user-owned folder whose permission can be revoked across sessions, so record-only removal
 * remains a safe explicit fallback while `actionsForState` keeps `discard` as the primary path.
 */
export function canForgetState(
  state: ReceiveLifecycleState,
  backend: SaveBackendKind,
): boolean {
  return actionsForState(state, backend).includes('forget') ||
    (backend === 'fsa' && (state.kind === 'paused' || state.kind === 'failed' || state.kind === 'partial'))
}

export function retainedTaskReadiness(
  task: Pick<RetainedTaskSnapshot, 'shareInstance' | 'lifecycle'>,
  currentShareInstance: string | null,
): RetainedReadiness {
  if (task.lifecycle.kind !== 'paused') return 'local'
  if (currentShareInstance === null) return 'original-link-required'
  return task.shareInstance === currentShareInstance ? 'matching-share' : 'different-share'
}

export function terminalConnectionBlock(connection: ReceiverConnectionSnapshot | Readonly<{ kind: 'idle' }>): TerminalConnectionBlock | null {
  if (connection.kind === 'ended') return 'shareEnded'
  if (connection.kind === 'unavailable') return connection.reason === 'access-denied' ? 'relayAccessDenied' : 'connectionUnavailable'
  return null
}

export interface RetainedAdmissionContext {
  readonly currentShareInstance: string | null
  readonly hasActiveDownload: boolean
  readonly hasPendingAction: boolean
  readonly connectionBlock: TerminalConnectionBlock | null
}

export function admitRetainedAction(
  task: RetainedTaskSnapshot,
  action: ReceiveAction,
  context: RetainedAdmissionContext,
): ReceiveAdmission {
  const supported = task.actions.includes(action) ||
    (action === 'forget' && canForgetState(task.lifecycle, task.backend))
  if (!supported) return blockedAdmission('actionUnavailable')
  if (task.activeElsewhere) return blockedAdmission('activeInAnotherTab')
  if (isLocalOnlyAction(action)) {
    return context.hasPendingAction ? blockedAdmission('busy') : ALLOWED_ADMISSION
  }
  // Structural share/connection blockers take precedence over transient local-slot
  // contention so offline tasks do not masquerade as transiently busy during local exports.
  const readiness = retainedTaskReadiness(task, context.currentShareInstance)
  if (readiness === 'original-link-required' || readiness === 'different-share') {
    return blockedAdmission('matchingShare')
  }
  if (context.connectionBlock !== null) return blockedAdmission(context.connectionBlock)
  if (context.hasPendingAction) return blockedAdmission('busy')
  if (context.hasActiveDownload) return blockedAdmission('downloading')
  return ALLOWED_ADMISSION
}

export function canStartNewDownload(lifecycle: ReceiveLifecycleState | null): boolean {
  return lifecycle === null || !isActiveReceiveState(lifecycle)
}

export function canReleaseCurrentTask(lifecycle: ReceiveLifecycleState | null): boolean {
  return lifecycle !== null && (lifecycle.kind === 'failed' || lifecycle.kind === 'paused')
}
