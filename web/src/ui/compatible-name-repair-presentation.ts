import {
  compatibleNameRepairSummary,
  type CompatibleNameRepairSummary,
} from '../output/file-system-access/compatible-name/model'
import type { ReceiveLifecycleState } from '../output/workspace'

export type CompatibleNameRepairActionMode =
  | 'abnormal-stop-recovery'
  | 'catch-up-required'
  | 'routine-restoration'

export type CompatibleNameRepairPresentationContext =
  | 'receive-lifecycle'
  | 'pending-catch-up'

export interface CompatibleNameRepairPresentation {
  readonly noticeTitle: string
  readonly noticeDescription: string
  readonly replacementCount: number
  readonly replacementCountLabel: string
  readonly logicalPathSample: readonly string[]
  readonly omittedLogicalPathCount: number
  readonly scriptName: string
  readonly sidecarName: string
  readonly placementLabel: string
  readonly runCommand: string | null
  readonly actionMode: CompatibleNameRepairActionMode
  readonly actionTitle: string
  readonly actionDescription: string
}

export function presentCompatibleNameRepair(input: Readonly<{
  state: ReceiveLifecycleState | null
  summary: CompatibleNameRepairSummary
  context?: CompatibleNameRepairPresentationContext
}>): CompatibleNameRepairPresentation {
  const summary = compatibleNameRepairSummary(input.summary)
  const actionMode = compatibleNameRepairActionMode(
    input.state,
    summary,
    input.context ?? 'receive-lifecycle',
  )
  const logicalPathSample = Object.freeze(summary.logicalPathSample.map(path => path.join('/')))
  return Object.freeze({
    noticeTitle: 'Compatible names are in use',
    noticeDescription: 'The browser rejected an original name. WindShare saved the affected entry under a compatible name and prepared a restoration tool. The saved names remain compatible until that tool runs.',
    replacementCount: summary.committedCount,
    replacementCountLabel: `${summary.committedCount} verified/committed name ${
      summary.committedCount === 1 ? 'replacement' : 'replacements'}`,
    logicalPathSample,
    omittedLogicalPathCount: summary.committedCount - logicalPathSample.length,
    scriptName: summary.pairDisplayNames.script,
    sidecarName: summary.pairDisplayNames.sidecar,
    placementLabel: summary.placement === 'inside-logical-root'
      ? 'Inside the received folder'
      : 'Beside the received result',
    // Catch-up still needs the compatible physical namespace, so exposing a runnable
    // restoration command at that boundary would invite an irreversible ordering error.
    runCommand: actionMode === 'catch-up-required' ? null : summary.runCommand,
    actionMode,
    actionTitle: repairActionTitle(actionMode),
    actionDescription: repairActionDescription(actionMode),
  })
}

export function hasValidatedTerminalCompatibleNameRepair(
  summary: CompatibleNameRepairSummary,
): boolean {
  const footer = summary.latestObservedFooter
  return summary.committedCount > 0 && !summary.pendingCatchUp &&
    footer !== undefined && footer.state !== 'active' &&
    footer.committedCount === summary.committedCount
}

function compatibleNameRepairActionMode(
  state: ReceiveLifecycleState | null,
  summary: CompatibleNameRepairSummary,
  context: CompatibleNameRepairPresentationContext,
): CompatibleNameRepairActionMode {
  if (context === 'pending-catch-up') return 'catch-up-required'
  if (state === null || !terminalRepairLifecycle(state)) {
    return 'abnormal-stop-recovery'
  }
  return hasValidatedTerminalCompatibleNameRepair(summary)
    ? 'routine-restoration'
    : 'catch-up-required'
}

function terminalRepairLifecycle(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'expired' || state.kind === 'needs-attention' ||
    state.kind === 'download-started'
}

function repairActionTitle(mode: CompatibleNameRepairActionMode): string {
  switch (mode) {
    case 'abnormal-stop-recovery': return 'Abnormal-stop recovery only'
    case 'catch-up-required': return 'Restoration tool catch-up required'
    case 'routine-restoration': return 'Restore the original names'
  }
}

function repairActionDescription(mode: CompatibleNameRepairActionMode): string {
  switch (mode) {
    case 'abnormal-stop-recovery':
      return 'Do not run this command while WindShare is receiving or while this task remains resumable. Use it only after an abnormal stop and after deciding not to resume.'
    case 'catch-up-required':
      return 'Do not run the restoration tool yet. WindShare must finish the local sidecar checkpoint before restoration becomes the routine action.'
    case 'routine-restoration':
      return 'Receiving has ended and the terminal sidecar checkpoint is complete. Run this command when you are ready to restore the logical names.'
  }
}
