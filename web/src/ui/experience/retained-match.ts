import type { RetainedReceiveAction, RetainedReceiveOperation } from '../../application/receive/contracts'
import { retainedContinuationReadiness } from '../../application/receive/retained/readiness'
import type { TaskSnapshot } from '../controller/projections'
import type { TaskAction, TaskPresentation } from '../tasks'
import type { TaskComposition } from './task-composition'

export type WorkspaceTaskSnapshot = TaskSnapshot & TaskComposition

export interface WorkspaceRetainedBanner {
  readonly kind: 'continue' | 'ready-to-save'
  readonly task: TaskPresentation
}

export interface WorkspaceTaskLayout {
  readonly primaryTask: TaskPresentation | null
  readonly collapseRemoteSaving: boolean
  readonly repeatRemoteLabel: boolean
  readonly banner: WorkspaceRetainedBanner | null
}

function isCompleteLocalRetained(
  operation: RetainedReceiveOperation,
  task: TaskPresentation,
): boolean {
  // Transient action busy state disables the button in place; it does not invalidate the local artifact.
  return (operation.continuation === 'retry-download' || operation.continuation === 'save-artifact') &&
    task.completeness === 'complete' &&
    task.primaryAction !== null &&
    task.primaryAction.target.kind === 'retained'
}

function retainedAction(
  task: TaskPresentation,
  action: RetainedReceiveAction,
): TaskAction | null {
  if (task.primaryAction?.target.kind === 'retained' && task.primaryAction.target.action === action) {
    return task.primaryAction
  }
  return task.secondaryActions.find(
    candidate => candidate.target.kind === 'retained' && candidate.target.action === action,
  ) ?? null
}

function isTransientlyBusyContinuation(
  operation: RetainedReceiveOperation,
  task: TaskPresentation,
  snapshot: WorkspaceTaskSnapshot,
): boolean {
  const { pending } = snapshot.retained
  if (pending === null) return false
  if (pending.operationId === task.operationId && pending.action === 'continue') return true
  if (operation.unavailableReason !== undefined) return false
  const needsRemote = retainedContinuationReadiness(operation, snapshot.share?.shareInstance ?? null) !== 'local'
  return !needsRemote || (snapshot.connection.kind !== 'ended' && snapshot.connection.kind !== 'unavailable')
}

function isContinuableRetained(
  operation: RetainedReceiveOperation,
  task: TaskPresentation,
  snapshot: WorkspaceTaskSnapshot,
): boolean {
  if (task.stage === 'handed-to-browser') return false
  // Check the explicit continue action rather than any enabled primaryAction; when
  // remote continuation is disabled, presentation promotes local fallbacks like
  // save-partial that must not displace a complete local copy.
  const continuation = retainedAction(task, 'continue')
  if (continuation === null) return false
  return continuation.disabledReason === null || isTransientlyBusyContinuation(operation, task, snapshot)
}

function findCompleteLocalMatch(snapshot: WorkspaceTaskSnapshot): TaskPresentation | null {
  const { share, current, tasks, retained, output } = snapshot
  const currentSelectionDigest = output.offers?.selectionDigest
  if (current !== null || share === null || currentSelectionDigest === undefined) return null
  for (const task of tasks) {
    const operation = retained.operations.find(candidate => candidate.operationId === task.operationId)
    if (operation === undefined ||
        operation.shareInstance !== share.shareInstance ||
        operation.selectionDigest !== currentSelectionDigest) {
      continue
    }
    if (isCompleteLocalRetained(operation, task)) return task
    // A newer continuable attempt for the same selection supersedes older local copies so reopen surfaces Continue.
    if (isContinuableRetained(operation, task, snapshot)) return null
  }
  return null
}

function currentMatchesSelection(snapshot: WorkspaceTaskSnapshot): boolean {
  const { share, current, output, retained } = snapshot
  if (share === null || current === null) return false
  const currentSelectionDigest = output.offers?.selectionDigest
  const taskSelectionDigest = output.receiveIntent?.selection.digest ??
    retained.operations.find(operation => operation.operationId === current.operationId)?.selectionDigest
  if (taskSelectionDigest !== undefined && currentSelectionDigest !== undefined) {
    return taskSelectionDigest === currentSelectionDigest
  }
  return share.kind !== 'browser'
}

function isSettlingWorkspaceHandoff(snapshot: WorkspaceTaskSnapshot): boolean {
  const { current, output, retained, activeReceiveOperationId } = snapshot
  const lifecycle = output.lifecycle
  return current !== null &&
    lifecycle?.kind === 'download-started' &&
    lifecycle.attemptKind === 'workspace' &&
    current.operationId === lifecycle.operationId &&
    (activeReceiveOperationId === current.operationId || retained.kind === 'loading' || retained.kind === 'unloaded')
}

function retainedBannerKind(task: TaskPresentation): WorkspaceRetainedBanner['kind'] | null {
  if (task.stage === 'handed-to-browser' ||
      task.primaryAction === null ||
      task.primaryAction.disabledReason !== null ||
      task.primaryAction.target.kind !== 'retained') {
    return null
  }
  const { action } = task.primaryAction.target
  if (task.stage === 'ready-to-save') {
    return action === 'save' || action === 'save-staged-files' ? 'ready-to-save' : null
  }
  return action === 'continue' ? 'continue' : null
}

function findRetainedBanner(snapshot: WorkspaceTaskSnapshot): WorkspaceRetainedBanner | null {
  const { share, tasks, retained } = snapshot
  if (share === null) return null
  for (const task of tasks) {
    const kind = retainedBannerKind(task)
    if (kind === null) continue
    const matchesShare = retained.operations.some(operation =>
      operation.operationId === task.operationId && operation.shareInstance === share.shareInstance)
    if (!matchesShare) continue
    return Object.freeze({ kind, task })
  }
  return null
}

/**
 * Keeps local browser-retained re-export orthogonal to starting a fresh transfer from the sender.
 * When the current selection already has a complete local artifact—whether on the same page
 * or after reload/reopen—the workspace surfaces that task directly and folds remote redownload
 * behind a secondary disclosure instead of creating duplicate browser copies.
 */
export function resolveWorkspaceTaskLayout(snapshot: WorkspaceTaskSnapshot): WorkspaceTaskLayout {
  const { share, current, output } = snapshot
  const completedLocalMatch = findCompleteLocalMatch(snapshot)
  const primaryTask = current ?? completedLocalMatch
  const matchesSelection = completedLocalMatch !== null || currentMatchesSelection(snapshot)
  const hasActionableLocalResult = primaryTask !== null && matchesSelection &&
    primaryTask.completeness === 'complete' && (
      (primaryTask.stage === 'handed-to-browser' &&
        (primaryTask.primaryAction !== null || isSettlingWorkspaceHandoff(snapshot))) ||
      (primaryTask.stage === 'ready-to-save' &&
        primaryTask.primaryAction !== null &&
        primaryTask.primaryAction.target.kind === 'retained')
    )
  const noPendingActivation = output.activationPresentation === null
  const collapseRemoteSaving = share !== null && noPendingActivation && hasActionableLocalResult
  const singleFileDelivered = share !== null && share.kind !== 'browser' && noPendingActivation &&
    (primaryTask?.stage === 'saved' || primaryTask?.stage === 'handed-to-browser')
  const banner = primaryTask === null ? findRetainedBanner(snapshot) : null
  return Object.freeze({
    primaryTask,
    collapseRemoteSaving,
    repeatRemoteLabel: collapseRemoteSaving || singleFileDelivered,
    banner,
  })
}
