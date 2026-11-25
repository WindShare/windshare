import type { ReceiveAction } from '../../application/receive/contracts'
import { terminalConnectionBlock, type RetainedAdmissionContext } from '../../application/receive/admission'
import { resolveWorkspaceRetainedMatch } from '../../application/receive/retained'
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

export function dismissedTaskKey(operationId: string, generation: bigint): string {
  return `${operationId}:${generation}`
}

export function isDismissibleWorkspaceTask(task: TaskPresentation): boolean {
  return task.stage === 'handed-to-browser' || task.stage === 'saved'
}

function isDismissedWorkspaceTask(
  task: TaskPresentation,
  dismissedTaskKeys: ReadonlySet<string> | undefined,
): boolean {
  return dismissedTaskKeys !== undefined &&
    isDismissibleWorkspaceTask(task) &&
    dismissedTaskKeys.has(dismissedTaskKey(task.operationId, task.generation))
}

function admissionContextFor(snapshot: WorkspaceTaskSnapshot): RetainedAdmissionContext {
  const { share, output, retained, connection, activeReceiveOperationId } = snapshot
  return {
    currentShareInstance: share?.shareInstance ?? null,
    hasActiveDownload: activeReceiveOperationId !== null ||
      output.activation !== null ||
      retained.pending?.action === 'continue',
    hasPendingAction: retained.pending !== null,
    connectionBlock: terminalConnectionBlock(connection),
  }
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

function hasActionableLocalCopy(task: TaskPresentation): boolean {
  return task.completeness === 'complete' &&
    (task.stage === 'handed-to-browser' || task.stage === 'ready-to-save') &&
    task.primaryAction !== null &&
    task.primaryAction.target.action === 'save'
}

function resolveRetainedPresentation(
  snapshot: WorkspaceTaskSnapshot,
  shareInstance: string,
  dismissedTaskKeys: ReadonlySet<string> | undefined,
): Readonly<{ primaryTask: TaskPresentation | null; banner: WorkspaceRetainedBanner | null }> {
  const presentedById = new Map(snapshot.tasks.map(task => [task.operationId, task]))
  const match = resolveWorkspaceRetainedMatch({
    tasks: snapshot.retained.tasks,
    shareInstance,
    selectionKey: snapshot.output.offers?.selectionKey ?? null,
    admission: admissionContextFor(snapshot),
    isIgnored: retained => {
      const presented = presentedById.get(retained.taskId)
      return presented === undefined || isDismissedWorkspaceTask(presented, dismissedTaskKeys)
    },
  })
  if (match.kind === 'local-complete') {
    return { primaryTask: presentedById.get(match.task.taskId) ?? null, banner: null }
  }
  if (match.kind === 'banner') {
    const presented = presentedById.get(match.task.taskId)
    const action = presented === undefined ? null : retainedAction(presented, match.action)
    if (presented !== undefined && action !== null && action.disabledReason === null) {
      return { primaryTask: null, banner: Object.freeze({ kind: match.bannerKind, task: presented, action }) }
    }
  }
  return { primaryTask: null, banner: null }
}

/**
 * Projects active workspace state and domain retained-task matching into workspace layout.
 * When the current selection already has a complete local OPFS copy—whether from this session
 * or after reload/reopen—the workspace surfaces that task directly and folds remote redownload
 * behind a secondary disclosure instead of creating duplicate browser copies.
 */
export function resolveWorkspaceTaskLayout(
  snapshot: WorkspaceTaskSnapshot,
  dismissedTaskKeys?: ReadonlySet<string>,
): WorkspaceTaskLayout {
  const { share, current, output } = snapshot
  const visibleCurrent = current !== null && !isDismissedWorkspaceTask(current, dismissedTaskKeys) ? current : null
  const retainedMatch = visibleCurrent === null && share !== null
    ? resolveRetainedPresentation(snapshot, share.shareInstance, dismissedTaskKeys)
    : null
  const primaryTask = visibleCurrent ?? retainedMatch?.primaryTask ?? null
  const banner = retainedMatch?.banner ?? null
  const matchedLocalRetained = retainedMatch !== null && retainedMatch.primaryTask !== null
  const matchesSelection = matchedLocalRetained || (visibleCurrent !== null && currentMatchesSelection(snapshot))
  const noPendingActivation = output.activationPresentation === null
  const collapseRemoteSaving = share !== null && noPendingActivation &&
    primaryTask !== null && matchesSelection && hasActionableLocalCopy(primaryTask)
  const singleFileDelivered = share !== null && share.kind !== 'browser' && noPendingActivation &&
    (primaryTask?.stage === 'saved' || primaryTask?.stage === 'handed-to-browser')

  return Object.freeze({
    primaryTask,
    collapseRemoteSaving,
    repeatRemoteLabel: collapseRemoteSaving || singleFileDelivered,
    banner,
  })
}
