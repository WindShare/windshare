import type {
  RetainedReceiveAction,
  RetainedReceiveOperation,
} from '../../receive/contracts'

const RESUME_AUTHORITY_UNAVAILABLE =
  'Saved actions are unavailable because this browser has no persisted-operation authority.'

function retainedActions(
  ...actions: RetainedReceiveAction[]
): readonly RetainedReceiveAction[] {
  return Object.freeze(actions)
}

export function retainedOperationAuthority(
  continuation: RetainedReceiveOperation['continuation'],
  hasMutationAuthority: boolean,
  hasDirectZipAuthority: boolean,
  hasDirectTreeRecoverySummary: boolean,
): Readonly<{
  actions: readonly RetainedReceiveAction[]
  unavailableReason?: string
}> {
  if (!hasMutationAuthority) {
    return Object.freeze({
      actions: retainedActions(),
      unavailableReason: RESUME_AUTHORITY_UNAVAILABLE,
    })
  }
  switch (continuation) {
    case 'resume-start':
      return Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'history-only':
      return Object.freeze({ actions: retainedActions() })
    case 'cleanup-incompatible':
    case 'cleanup-only':
      return Object.freeze({ actions: retainedActions('discard') })
    case 'pending-catch-up':
    case 'restoration-available':
      return Object.freeze({ actions: retainedActions('catch-up') })
    case 'resume-receive':
      return hasDirectTreeRecoverySummary
        ? Object.freeze({ actions: retainedActions('continue', 'redownload', 'catch-up') })
        : Object.freeze({ actions: retainedActions('continue', 'discard', 'catch-up') })
    case 'resume-package':
    case 'resume-local-finalization':
      return Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'resume-direct-zip':
    case 'reauthorize-direct-zip':
    case 'verify-direct-zip-target':
    case 'verify-direct-zip-completion':
    case 'retry-direct-zip-space':
      return hasDirectZipAuthority
        ? Object.freeze({ actions: retainedActions('continue', 'delete') })
        : Object.freeze({
            actions: retainedActions(),
            unavailableReason: 'Direct ZIP recovery authority is not installed.',
          })
    case 'save-artifact':
      return Object.freeze({ actions: retainedActions('save', 'discard') })
    case 'retry-download':
      return Object.freeze({ actions: retainedActions('redownload', 'delete') })
    case 'retry-cleanup':
      return Object.freeze({ actions: retainedActions('catch-up', 'delete') })
    case 'needs-attention':
      return Object.freeze({
        actions: retainedActions(),
        unavailableReason: 'Ownership needs attention; no automatic action is safe.',
      })
  }
}
