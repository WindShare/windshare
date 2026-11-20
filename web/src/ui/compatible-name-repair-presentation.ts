import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import { receiveRepairMode, terminalRepairLifecycle, type CompatibleNameRepairActionMode } from '../application/receive/repair'
import { compatibleNameRepairSummary } from '../transfer/lifecycle/name-repair'
import {
  type CompatibleNameRepairSummary,
} from '../transfer/lifecycle/name-repair'
import type { ReceiveLifecycleState } from '../transfer/lifecycle/state'

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
  readonly scriptName: string | null
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
}>, language: Language = 'en'): CompatibleNameRepairPresentation {
  const t = receiverCopy[language].restoration
  const summary = compatibleNameRepairSummary(input.summary)
  const live = input.context !== 'retained-operation' &&
    (input.state === null || input.state.kind === 'receiving' ||
     input.state.kind === 'finalizing-tree' || input.state.kind === 'committing-atomic' ||
     input.state.kind === 'preparing' || input.state.kind === 'intent-frozen')
  const terminal = input.state !== null && terminalRepairLifecycle(input.state)
  const actionMode = receiveRepairMode(input.state, summary,
    input.context === 'retained-operation' ? 'retained-operation' : 'live-operation')
  const logicalPathSample = Object.freeze(summary.logicalPathSample.map(path => path.join('/')))
  const scriptName = summary.pairDisplayNames.script ?? null
  const commandAvailable = scriptName !== null && summary.committedCount > 0 &&
    (actionMode === 'abnormal-stop-recovery' || actionMode === 'routine-restoration')
  const shortCommand = commandAvailable ? `.\\${summary.pairDisplayNames.script}` : null
  const restorationVisibility = terminal ? 'primary' : 'secondary'
  return Object.freeze({
    noticeTitle: t.compatibleNames,
    noticeDescription: t.compatibleHint,
    replacementCount: summary.committedCount,
    replacementCountLabel: t.replacementCount(summary.committedCount),
    logicalPathSample,
    omittedLogicalPathCount: summary.committedCount - logicalPathSample.length,
    scriptName,
    sidecarName: summary.pairDisplayNames.sidecar,
    placementLabel: summary.placement === 'inside-logical-root'
      ? t.insideFolder
      : t.besideResult,
    // A stopped receiver may remain resumable. Only observed sidecar convergence,
    // independently of continuation availability, permits the local restore command.
    runCommand: shortCommand === null ? null
      : `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "${shortCommand}"`,
    shortCommand,
    visibility: live || summary.committedCount === 0 ? 'notice' : restorationVisibility,
    actionMode,
    actionTitle: scriptName === null ? t.namesRecorded : repairActionTitle(actionMode, language),
    actionDescription: scriptName === null
      ? t.recordedHint
      : repairActionDescription(actionMode, language),
  })
}

function repairActionTitle(mode: CompatibleNameRepairActionMode, language: Language): string {
  const t = receiverCopy[language].restoration
  switch (mode) {
    case 'receiving-notice': return t.namesAdjusted
    case 'abnormal-stop-recovery': return t.restoreAfterStop
    case 'catch-up-required': return t.setupNeeded
    case 'routine-restoration': return t.restoreOriginal
  }
}

function repairActionDescription(mode: CompatibleNameRepairActionMode, language: Language): string {
  const t = receiverCopy[language].restoration
  switch (mode) {
    case 'receiving-notice':
      return t.keepProgress
    case 'abnormal-stop-recovery':
      return t.restoreAfterStopHint
    case 'catch-up-required':
      return t.setupHint
    case 'routine-restoration':
      return t.finished
  }
}
