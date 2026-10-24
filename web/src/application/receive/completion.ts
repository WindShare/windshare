import type { ReceiveLifecycleState } from '../../output/workspace'

/** Delivery ends receive ownership; retained results can still have independent local actions. */
export function isReceiveOutputDelivered(state: ReceiveLifecycleState | null): boolean {
  return state?.kind === 'published' || state?.kind === 'partial-directory' || state?.kind === 'download-started'
}
