import type { LifecycleActionPresentation } from '../v2-lifecycle-presentation'
import type { V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../v2-receive-runtime'
import type { TaskAction, TaskRecoveryReadiness } from './model'

export function activeTaskAction(action: LifecycleActionPresentation, planKind: string): TaskAction {
  return Object.freeze({
    id: action.kind,
    label: action.kind === 'pause' ? 'Pause' : action.label,
    destructive: action.destructive,
    disabledReason: null,
    consequence: activeConsequence(action, planKind),
    target: Object.freeze({ kind: 'active', action: action.kind }),
  })
}

function activeConsequence(action: LifecycleActionPresentation, planKind: string): string | null {
  if (action.kind === 'pause') return 'Keeps the progress supported by this saving method; accepted writes finish before pausing.'
  if (action.kind === 'stop') return stopConsequence(planKind)
  if (action.kind === 'delete' || action.kind === 'discard') {
    return 'Deletes task-owned retained data. Files already exported through browser downloads are unaffected.'
  }
  return action.kind === 'redownload' && action.destructive
    ? 'Restarts incomplete files; their existing partial progress will not be reused.' : null
}

export function retainedTaskAction(
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  readiness: TaskRecoveryReadiness,
): TaskAction {
  const remote = action === 'continue' || (action === 'redownload' && operation.recoverySummary !== undefined)
  let disabledReason: string | null = null
  if (remote && (readiness === 'original-link-required' || readiness === 'different-share')) {
    disabledReason = 'Open the original share link to continue this download.'
  } else if (remote) {
    disabledReason = operation.unavailableReason ?? null
  }
  const cleanupOnly = operation.continuation === 'retry-cleanup'
  const destructive = action === 'discard' || action === 'delete' ||
    (action === 'redownload' && operation.recoverySummary !== undefined)
  return Object.freeze({
    id: action,
    label: retainedLabel(operation, action, readiness),
    destructive: destructive && !cleanupOnly,
    disabledReason,
    consequence: retainedConsequence(operation, action),
    target: Object.freeze({ kind: 'retained', operation, action }),
  })
}

function stopConsequence(planKind: string): string {
  switch (planKind) {
    case 'direct-tree': return 'Completed files stay in the chosen folder. Unfinished files follow their verified checkpoint disposition.'
    case 'direct-resumable-zip': return 'Keeps the unfinished ZIP and its verified resume position. It is not a usable archive until finishing succeeds.'
    case 'workspace-then-publish': return 'Stops receiving and settles retained browser data before releasing this task.'
    default: return 'Stops receiving. This saving method cannot resume an interrupted transfer.'
  }
}

function retainedLabel(
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  readiness: TaskRecoveryReadiness,
): string {
  switch (action) {
    case 'save-partial': return 'Save partial ZIP'
    case 'catch-up': return 'Finish filename restoration setup'
    case 'continue': return continuationLabel(operation, readiness)
    case 'save': return 'Save'
    case 'redownload': return operation.recoverySummary === undefined ? 'Download again' : 'Restart incomplete files'
    case 'forget': return 'Remove from Downloads'
    case 'discard': return operation.continuation === 'cleanup-incompatible' ? 'Delete history record' : 'Discard unfinished output'
    case 'delete':
      if (operation.continuation === 'retry-cleanup') return 'Retry cleanup'
      if (['resume-direct-zip', 'reauthorize-direct-zip', 'verify-direct-zip-target', 'retry-direct-zip-space'].includes(operation.continuation)) return 'Delete unfinished ZIP'
      return 'Delete retained result'
  }
}

function continuationLabel(operation: V2RetainedReceiveOperation, readiness: TaskRecoveryReadiness): string {
  if (readiness === 'destination-authorization-required') return 'Authorize destination and continue'
  if (operation.continuation === 'resume-local-finalization' || operation.continuation === 'resume-package') return 'Finish and save'
  if (operation.continuation === 'verify-direct-zip-target') return 'Verify destination and continue'
  if (operation.continuation === 'retry-direct-zip-space') return 'Retry after freeing space'
  return operation.recoverySummary === undefined ? 'Continue' : 'Continue and preserve partial files'
}

function retainedConsequence(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction): string | null {
  switch (action) {
    case 'save-partial':
      return 'Exports only complete files as a separate partial ZIP. Missing and unfinished items are excluded; the retained task remains available for continuation.'
    case 'forget': return 'Removes this history record from Downloads. Files already saved or handed to the browser remain untouched.'
    case 'discard':
      return operation.continuation === 'cleanup-incompatible'
        ? 'Removes only the incompatible saved record; it does not authorize deleting destination files.'
        : 'Deletes task-owned unfinished output and retained data.'
    case 'delete':
      return operation.continuation === 'retry-cleanup'
        ? 'Removes owned temporary data; the published result remains saved.'
        : 'Verifies ownership before deleting retained output. Files already exported through browser downloads are unaffected.'
    case 'redownload':
      return operation.recoverySummary === undefined ? null
        : 'Restarts incomplete files instead of reusing their retained partial progress.'
    default: return null
  }
}
