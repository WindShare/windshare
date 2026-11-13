import { isTerminalLifecycleState, type ReceiveLifecycleState } from '../../transfer/lifecycle/state'
import type { BrowserDeliverySummary } from './browser-delivery'

export type BrowserDeliveryLocalAction = 'save-staged-files' | 'cleanup-staging' | 'discard-incomplete-staging'

export function isBrowserDeliveryLocalLifecycle(lifecycle: ReceiveLifecycleState): boolean {
  return (lifecycle.kind === 'resumable-receive' && lifecycle.payloadKind === 'file-set') ||
    isTerminalLifecycleState(lifecycle)
}

/** Child storage survives source settlement; its actions do not reopen source reception. */
export function browserDeliveryLocalActions(
  lifecycle: ReceiveLifecycleState,
  summary: BrowserDeliverySummary | null | undefined,
): readonly BrowserDeliveryLocalAction[] {
  if (!isBrowserDeliveryLocalLifecycle(lifecycle) || summary == null ||
      summary.operationId !== lifecycle.operationId ||
      summary.receiveIntentDigest !== lifecycle.receiveIntentDigest) return Object.freeze([])
  const actions: BrowserDeliveryLocalAction[] = []
  if (summary.stagedCompleteFiles + summary.copyingFiles > 0) actions.push('save-staged-files')
  if (summary.cleanupPendingFiles > 0) actions.push('cleanup-staging')
  if (isTerminalLifecycleState(lifecycle) && summary.incompleteStagedFiles > 0) actions.push('discard-incomplete-staging')
  return Object.freeze(actions)
}
