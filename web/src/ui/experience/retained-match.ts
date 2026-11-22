import type { RetainedReceiveOperation } from '../../application/receive/contracts'
import type { TaskSnapshot } from '../controller/projections'
import type { TaskPresentation } from '../tasks'
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
  return (operation.continuation === 'retry-download' || operation.continuation === 'save-artifact') &&
    task.completeness === 'complete' &&
    task.primaryAction !== null &&
    task.primaryAction.disabledReason === null
}

function findCompleteLocalMatch(snapshot: WorkspaceTaskSnapshot): TaskPresentation | null {
  const { share, current, tasks, retained, output } = snapshot
  const currentSelectionDigest = output.offers?.selectionDigest
  if (current !== null || share === null || currentSelectionDigest === undefined) return null
  for (const task of tasks) {
    const operation = retained.operations.find(candidate => candidate.operationId === task.operationId)
    if (operation !== undefined &&
        operation.shareInstance === share.shareInstance &&
        operation.selectionDigest === currentSelectionDigest &&
        isCompleteLocalRetained(operation, task)) {
      return task
    }
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

function findRetainedBanner(snapshot: WorkspaceTaskSnapshot): WorkspaceRetainedBanner | null {
  const { share, tasks, retained } = snapshot
  if (share === null) return null
  for (const task of tasks) {
    if (task.stage === 'handed-to-browser' || task.primaryAction === null || task.primaryAction.disabledReason !== null) {
      continue
    }
    const matchesShare = retained.operations.some(operation =>
      operation.operationId === task.operationId && operation.shareInstance === share.shareInstance)
    if (!matchesShare) continue
    return Object.freeze({
      kind: task.stage === 'ready-to-save' ? 'ready-to-save' : 'continue',
      task,
    })
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
