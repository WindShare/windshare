import {
  compatibleNameRepairSummary,
  type CompatibleNameRepairSummary,
} from '../output/file-system-access/compatible-name/model'
import type { ReceiveLifecycleState } from '../output/workspace'

export type CompatibleNameRepairActionMode =
  | 'receiving-notice'
  | 'abnormal-stop-recovery'
  | 'catch-up-required'
  | 'routine-restoration'

export type CompatibleNameRepairPresentationContext =
  | 'receive-lifecycle'
  | 'retained-operation'

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
  readonly shortCommand: string | null
  readonly visibility: 'notice' | 'secondary' | 'primary'
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
  const live = input.context !== 'retained-operation' &&
    (input.state === null || input.state.kind === 'receiving' ||
     input.state.kind === 'finalizing-tree' || input.state.kind === 'committing-atomic' ||
     input.state.kind === 'preparing' || input.state.kind === 'intent-frozen')
  const terminal = input.state !== null && terminalRepairLifecycle(input.state)
  const actionMode = compatibleNameRepairActionMode(live, terminal, summary)
  const logicalPathSample = Object.freeze(summary.logicalPathSample.map(path => path.join('/')))
  const commandAvailable = summary.committedCount > 0 &&
    (actionMode === 'abnormal-stop-recovery' || actionMode === 'routine-restoration')
  const shortCommand = commandAvailable ? `.\\${summary.pairDisplayNames.script}` : null
  const restorationVisibility = terminal ? 'primary' : 'secondary'
  return Object.freeze({
    noticeTitle: 'Compatible names are in use',
    noticeDescription: 'The browser rejected an original name. Affected entries use compatible names until you restore the originals.',
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
    // A stopped receiver may remain resumable. Only observed sidecar convergence,
    // independently of continuation availability, permits the local restore command.
    runCommand: shortCommand === null ? null
      : `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "${shortCommand}"`,
    shortCommand,
    visibility: live || summary.committedCount === 0 ? 'notice' : restorationVisibility,
    actionMode,
    actionTitle: repairActionTitle(actionMode),
    actionDescription: repairActionDescription(actionMode),
  })
}

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

function terminalRepairLifecycle(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'expired' || state.kind === 'needs-attention' ||
    state.kind === 'download-started'
}

function repairActionTitle(mode: CompatibleNameRepairActionMode): string {
  switch (mode) {
    case 'receiving-notice': return 'Names adjusted while receiving'
    case 'abnormal-stop-recovery': return 'Restore names after stopping'
    case 'catch-up-required': return 'Restoration tool catch-up required'
    case 'routine-restoration': return 'Restore the original names'
  }
}

function repairActionDescription(mode: CompatibleNameRepairActionMode): string {
  switch (mode) {
    case 'receiving-notice':
      return 'Continue receiving to preserve your download progress.'
    case 'abnormal-stop-recovery':
      return 'Continue downloading to preserve progress. After restoring original names, this output cannot resume in the browser.'
    case 'catch-up-required':
      return 'Do not run the restoration tool yet. Finish local catch-up to update its checkpoint; the sender does not need to be online.'
    case 'routine-restoration':
      return 'Receiving has ended and the terminal sidecar checkpoint is complete.'
  }
}
