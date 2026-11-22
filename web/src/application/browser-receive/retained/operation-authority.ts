import type { ReceiveBlockReason } from '../../receive/admission'
import type {
  RetainedReceiveAction,
  RetainedReceiveOperation,
} from '../../receive/contracts'

const RESUME_AUTHORITY_UNAVAILABLE =
  'noSavedAuthority'

function retainedActions(
  ...actions: RetainedReceiveAction[]
): readonly RetainedReceiveAction[] {
  return Object.freeze(actions)
}

export function retainedOperationAuthority(
  continuation: RetainedReceiveOperation['continuation'],
  hasMutationAuthority: boolean,
  hasDirectTreeRecoverySummary: boolean,
): Readonly<{
  actions: readonly RetainedReceiveAction[]
  unavailableReason?: ReceiveBlockReason
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
    case 'cleanup-only':
      return Object.freeze({ actions: retainedActions('discard') })
    case 'resume-receive':
      return hasDirectTreeRecoverySummary
        ? Object.freeze({ actions: retainedActions('continue', 'redownload') })
        : Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'resume-package':
    case 'resume-local-finalization':
      return Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'save-artifact':
      return Object.freeze({ actions: retainedActions('save', 'discard') })
    case 'retry-download':
      return Object.freeze({ actions: retainedActions('redownload', 'delete') })
    case 'retry-cleanup':
      return Object.freeze({ actions: retainedActions('delete') })
    case 'needs-attention':
      return Object.freeze({
        actions: retainedActions(),
        unavailableReason: 'ownershipUnknown',
      })
  }
}
