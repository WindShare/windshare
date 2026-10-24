import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import type { ReceiveLifecycleState } from '../../output/workspace'

export type CompatibleNameRepairActionMode =
  | 'receiving-notice'
  | 'abnormal-stop-recovery'
  | 'catch-up-required'
  | 'routine-restoration'

export function hasValidatedTerminalCompatibleNameRepair(
  summary: CompatibleNameRepairSummary,
): boolean {
  const footer = summary.latestObservedFooter
  return summary.committedCount > 0 && summary.sidecarSync === 'current' &&
    summary.terminalSettlement === 'complete' &&
    footer !== undefined && footer.state !== 'active' &&
    footer.committedCount === summary.committedCount
}

function compatibleNameRepairActionMode(
  live: boolean,
  terminal: boolean,
  summary: CompatibleNameRepairSummary,
): CompatibleNameRepairActionMode {
  if (live) return 'receiving-notice'
  if (summary.sidecarSync === 'pending' || summary.terminalSettlement === 'pending') {
    return 'catch-up-required'
  }
  if (!terminal) return 'abnormal-stop-recovery'
  return hasValidatedTerminalCompatibleNameRepair(summary)
    ? 'routine-restoration'
    : 'catch-up-required'
}

export function terminalRepairLifecycle(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'needs-attention' ||
    state.kind === 'download-started'
}


export function receiveRepairMode(
  state: ReceiveLifecycleState | null,
  summary: CompatibleNameRepairSummary,
  context: 'live-operation' | 'retained-operation' = 'live-operation',
): CompatibleNameRepairActionMode {
  const live = context === 'live-operation' &&
    (state === null || state.kind === 'receiving' || state.kind === 'finalizing-tree' ||
     state.kind === 'committing-atomic' || state.kind === 'preparing' || state.kind === 'intent-frozen')
  return compatibleNameRepairActionMode(live, state !== null && terminalRepairLifecycle(state), summary)
}

export function canCatchUpReceiveRepair(state: ReceiveLifecycleState | null, summary: CompatibleNameRepairSummary | null): boolean {
  return summary !== null && summary.committedCount > 0 && receiveRepairMode(state, summary) === 'catch-up-required'
}
