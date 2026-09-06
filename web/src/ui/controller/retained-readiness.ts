import type { V2RetainedReceiveOperation } from '../v2-receive-runtime'

export type RetainedContinuationReadiness =
  | 'local'
  | 'matching-share'
  | 'original-link-required'
  | 'different-share'
  | 'destination-authorization-required'

/** This projects prerequisites; only the inventory's opaque action token grants authority. */
export function retainedContinuationReadiness(
  operation: V2RetainedReceiveOperation,
  currentShareInstance: string | null,
): RetainedContinuationReadiness {
  const remote = operation.continuation === 'resume-receive' ||
    operation.continuation === 'resume-direct-zip' ||
    operation.continuation === 'reauthorize-direct-zip' ||
    operation.continuation === 'verify-direct-zip-target' ||
    operation.continuation === 'retry-direct-zip-space'
  if (!remote) return 'local'
  if (currentShareInstance === null || operation.shareInstance === undefined) return 'original-link-required'
  if (operation.shareInstance !== currentShareInstance) return 'different-share'
  if (operation.continuation === 'reauthorize-direct-zip') return 'destination-authorization-required'
  return 'matching-share'
}
