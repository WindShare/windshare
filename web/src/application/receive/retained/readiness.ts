import type { RetainedReceiveOperation } from '../contracts'

export type RetainedContinuationReadiness =
  | 'local'
  | 'matching-share'
  | 'original-link-required'
  | 'different-share'

/** This projects prerequisites; only the inventory's opaque action token grants authority. */
export function retainedContinuationReadiness(
  operation: RetainedReceiveOperation,
  currentShareInstance: string | null,
): RetainedContinuationReadiness {
  const remote = operation.continuation === 'resume-start' || operation.continuation === 'resume-receive'
  if (!remote) return 'local'
  if (currentShareInstance === null || operation.shareInstance === undefined) return 'original-link-required'
  if (operation.shareInstance !== currentShareInstance) return 'different-share'
  return 'matching-share'
}
