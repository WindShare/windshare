import type { ReceiveAction, RetainedTaskSnapshot } from '../../application/receive/contracts'
import { retainedTaskReadiness } from '../../application/receive/admission'
import type { TaskSnapshot } from '../controller/projections'
import type { TaskAction, TaskPresentation } from '../tasks'
import type { TaskComposition } from './task-composition'

export type WorkspaceTaskSnapshot = TaskSnapshot & TaskComposition

export interface WorkspaceRetainedBanner {
  readonly kind: 'continue' | 'ready-to-save'
  readonly task: TaskPresentation
  readonly action: TaskAction
}

export interface WorkspaceTaskLayout {
  readonly primaryTask: TaskPresentation | null
  readonly collapseRemoteSaving: boolean
  readonly repeatRemoteLabel: boolean
  readonly banner: WorkspaceRetainedBanner | null
}

type SelectionRetainedMatch =
  | { readonly kind: 'local-complete'; readonly task: TaskPresentation }
  | { readonly kind: 'banner'; readonly banner: WorkspaceRetainedBanner }
  | { readonly kind: 'pending-continuation' }
  | { readonly kind: 'none' }

function isCompleteLocalRetained(
  retained: RetainedTaskSnapshot,
  task: TaskPresentation,
): boolean {
  const hasLocalArtifact = retained.lifecycle.kind === 'ready-to-save' ||
    (retained.lifecycle.kind === 'download-started' && retained.lifecycle.retained)
  return hasLocalArtifact &&
    task.completeness === 'complete' &&
    task.primaryAction !== null &&
    task.primaryAction.target.kind === 'retained'
}

function retainedAction(
  task: TaskPresentation,
  action: ReceiveAction,
): TaskAction | null {
  if (task.primaryAction?.target.kind === 'retained' && task.primaryAction.target.action === action) {
    return task.primaryAction
  }
  return task.secondaryActions.find(
    candidate => candidate.target.kind === 'retained' && candidate.target.action === action,
  ) ?? null
}

function isTransientlyBusyContinuation(
  retained: RetainedTaskSnapshot,
  task: TaskPresentation,
  snapshot: WorkspaceTaskSnapshot,
): boolean {
  const { pending } = snapshot.retained
  if (pending === null) return false
  if (pending.taskId === task.operationId && pending.action === 'continue') return true
  if (retained.activeElsewhere) return false
  const needsRemote = retainedTaskReadiness(retained, snapshot.share?.shareInstance ?? null) !== 'local'
  return !needsRemote || (snapshot.connection.kind !== 'ended' && snapshot.connection.kind !== 'unavailable')
}

function isContinuableRetained(
  retained: RetainedTaskSnapshot,
  task: TaskPresentation,
  snapshot: WorkspaceTaskSnapshot,
): boolean {
  if (task.stage === 'handed-to-browser') return false
  const continuation = retainedAction(task, 'continue')
  if (continuation === null) return false
  return continuation.disabledReason === null || isTransientlyBusyContinuation(retained, task, snapshot)
}

function retainedBannerFor(task: TaskPresentation): WorkspaceRetainedBanner | null {
  if (task.stage === 'handed-to-browser') return null
  const primary = task.primaryAction
  if (
    task.stage === 'ready-to-save' &&
    primary !== null &&
    primary.disabledReason === null &&
    primary.target.kind === 'retained' &&
    primary.target.action === 'save'
  ) {
    return Object.freeze({ kind: 'ready-to-save', task, action: primary })
  }
  const continuation = retainedAction(task, 'continue')
  if (continuation !== null && continuation.disabledReason === null) {
    return Object.freeze({ kind: 'continue', task, action: continuation })
  }
  return null
}

function selectionTask(
  task: TaskPresentation,
  snapshot: WorkspaceTaskSnapshot,
  key: string,
): RetainedTaskSnapshot | null {
  const shareInstance = snapshot.share?.shareInstance
  if (shareInstance === undefined) return null
  return snapshot.retained.tasks.find(candidate =>
    candidate.taskId === task.operationId &&
    candidate.shareInstance === shareInstance &&
    candidate.selectionKey === key) ?? null
}

export function isDismissibleWorkspaceTask(task: TaskPresentation): boolean {
  return task.stage === 'handed-to-browser' || task.stage === 'saved'
}

function isDismissedWorkspaceTask(
  task: TaskPresentation,
  dismissedTaskIds: ReadonlySet<string> | undefined,
): boolean {
  return dismissedTaskIds !== undefined &&
    isDismissibleWorkspaceTask(task) &&
    dismissedTaskIds.has(task.operationId)
}

function resolveSelectionRetained(
  snapshot: WorkspaceTaskSnapshot,
  visibleCurrent: TaskPresentation | null,
  dismissedTaskIds?: ReadonlySet<string>,
): SelectionRetainedMatch {
  const key = snapshot.output.offers?.selectionKey
  if (visibleCurrent !== null || snapshot.share === null || key === undefined) {
    return { kind: 'none' }
  }
  let fallbackBanner: WorkspaceRetainedBanner | null = null
  for (const task of snapshot.tasks) {
    if (isDismissedWorkspaceTask(task, dismissedTaskIds)) continue
    const retained = selectionTask(task, snapshot, key)
    if (retained === null) continue
    if (isCompleteLocalRetained(retained, task)) return { kind: 'local-complete', task }
    if (isContinuableRetained(retained, task, snapshot)) {
      const banner = fallbackBanner ?? retainedBannerFor(task)
      return banner === null ? { kind: 'pending-continuation' } : { kind: 'banner', banner }
    }
    fallbackBanner ??= retainedBannerFor(task)
  }
  return fallbackBanner === null ? { kind: 'none' } : { kind: 'banner', banner: fallbackBanner }
}

function currentMatchesSelection(snapshot: WorkspaceTaskSnapshot): boolean {
  const { share, current, output, retained } = snapshot
  if (share === null || current === null) return false
  const currentKey = output.offers?.selectionKey
  const taskKey = output.activeSelectionKey ??
    retained.tasks.find(candidate => candidate.taskId === current.operationId)?.selectionKey
  if (taskKey !== undefined && taskKey !== null && currentKey !== undefined) {
    return taskKey === currentKey
  }
  return share.kind !== 'browser'
}

function isSettlingWorkspaceHandoff(snapshot: WorkspaceTaskSnapshot): boolean {
  const { current, output, retained, activeReceiveOperationId } = snapshot
  const lifecycle = output.lifecycle
  return current !== null &&
    lifecycle?.kind === 'download-started' &&
    lifecycle.retained &&
    current.operationId === lifecycle.taskId &&
    (activeReceiveOperationId === current.operationId || retained.kind === 'loading' || retained.kind === 'unloaded')
}

function findShareRetainedBanner(snapshot: WorkspaceTaskSnapshot): WorkspaceRetainedBanner | null {
  const { share, tasks, retained } = snapshot
  if (share === null) return null
  for (const task of tasks) {
    const banner = retainedBannerFor(task)
    if (banner === null) continue
    const matchesShare = retained.tasks.some(candidate =>
      candidate.taskId === task.operationId && candidate.shareInstance === share.shareInstance)
    if (matchesShare) return banner
  }
  return null
}

function resolveRetainedBanner(
  snapshot: WorkspaceTaskSnapshot,
  selectionMatch: SelectionRetainedMatch,
  primaryTask: TaskPresentation | null,
): WorkspaceRetainedBanner | null {
  if (primaryTask !== null) return null
  if (selectionMatch.kind === 'banner') return selectionMatch.banner
  if (selectionMatch.kind === 'pending-continuation') return null
  return findShareRetainedBanner(snapshot)
}

/**
 * Keeps local browser-retained re-export orthogonal to starting a fresh transfer from the sender.
 * When the current selection already has a complete local artifact—whether on the same page
 * or after reload/reopen—the workspace surfaces that task directly and folds remote redownload
 * behind a secondary disclosure instead of creating duplicate browser copies.
 */
export function resolveWorkspaceTaskLayout(
  snapshot: WorkspaceTaskSnapshot,
  dismissedTaskIds?: ReadonlySet<string>,
): WorkspaceTaskLayout {
  const { share, current, output } = snapshot
  const visibleCurrent = current !== null && !isDismissedWorkspaceTask(current, dismissedTaskIds) ? current : null
  const selectionMatch = resolveSelectionRetained(snapshot, visibleCurrent, dismissedTaskIds)
  const completedLocalMatch = selectionMatch.kind === 'local-complete' ? selectionMatch.task : null
  const primaryTask = visibleCurrent ?? completedLocalMatch
  const matchesSelection = completedLocalMatch !== null || (visibleCurrent !== null && currentMatchesSelection(snapshot))
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
  const banner = resolveRetainedBanner(snapshot, selectionMatch, primaryTask)
  return Object.freeze({
    primaryTask,
    collapseRemoteSaving,
    repeatRemoteLabel: collapseRemoteSaving || singleFileDelivered,
    banner,
  })
}
