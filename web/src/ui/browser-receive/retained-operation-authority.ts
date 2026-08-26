import type {
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

const RESUME_AUTHORITY_UNAVAILABLE =
  'Saved actions are unavailable because this browser has no persisted-operation authority.'

function retainedActions(
  ...actions: V2RetainedReceiveAction[]
): readonly V2RetainedReceiveAction[] {
  return Object.freeze(actions)
}

export function retainedOperationAuthority(
  continuation: V2RetainedReceiveOperation['continuation'],
  hasMutationAuthority: boolean,
  hasDirectZipAuthority: boolean,
  hasDirectTreeRecoverySummary: boolean,
): Readonly<{
  actions: readonly V2RetainedReceiveAction[]
  unavailableReason?: string
}> {
  if (!hasMutationAuthority) {
    return Object.freeze({
      actions: retainedActions(),
      unavailableReason: RESUME_AUTHORITY_UNAVAILABLE,
    })
  }
  switch (continuation) {
    case 'pending-catch-up':
    case 'restoration-available':
      return Object.freeze({ actions: retainedActions('catch-up') })
    case 'resume-receive':
      return hasDirectTreeRecoverySummary
        ? Object.freeze({ actions: retainedActions('continue', 'redownload') })
        : Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'resume-package':
      return Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'resume-direct-zip':
    case 'reauthorize-direct-zip':
    case 'verify-direct-zip-target':
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
    case 'cleanup-expired':
      return Object.freeze({ actions: retainedActions('delete') })
    case 'retry-cleanup':
      return Object.freeze({ actions: retainedActions('catch-up', 'delete') })
    case 'needs-attention':
      return Object.freeze({
        actions: retainedActions(),
        unavailableReason: 'Ownership needs attention; no automatic action is safe.',
      })
  }
}
