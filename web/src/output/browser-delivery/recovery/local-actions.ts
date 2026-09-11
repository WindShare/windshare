import { isTerminalLifecycleState, type ReceiveLifecycleState } from '../../workspace/state'
import type { BrowserDeliveryResumeSummary } from '../retained'

export type BrowserDeliveryLocalAction = 'save-staged-files' | 'cleanup-staging' | 'discard-incomplete-staging'

export function isBrowserDeliveryLocalLifecycle(lifecycle: ReceiveLifecycleState): boolean {
  return (lifecycle.kind === 'resumable-receive' && lifecycle.payloadKind === 'file-set') ||
    isTerminalLifecycleState(lifecycle)
}

/** Child storage survives source settlement; its actions do not reopen source reception. */
export function browserDeliveryLocalActions(
  lifecycle: ReceiveLifecycleState,
  summary: BrowserDeliveryResumeSummary | null | undefined,
): readonly BrowserDeliveryLocalAction[] {
  if (!isBrowserDeliveryLocalLifecycle(lifecycle) || summary == null ||
      summary.policy.operationId !== lifecycle.operationId ||
      summary.policy.receiveIntentDigest !== lifecycle.receiveIntentDigest) return Object.freeze([])
  const actions: BrowserDeliveryLocalAction[] = []
  if (summary.stagedCompleteFiles + summary.copyingFiles > 0) actions.push('save-staged-files')
  if (summary.cleanupPendingFiles > 0) actions.push('cleanup-staging')
  if (isTerminalLifecycleState(lifecycle) && summary.incompleteStagedFiles > 0) actions.push('discard-incomplete-staging')
  return Object.freeze(actions)
}
