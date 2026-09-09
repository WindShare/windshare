import type { ReceiveLifecycleState } from '../../output/workspace'
import type { ReceiveOperationDisplay } from '../../output/workspace/operation-display'
import { presentCompatibleNameRepair } from '../compatible-name-repair-presentation'
import type { V2ReceiverProgress, V2RetainedReceivePresentationOperation, V2PendingRetainedReceiveActionSnapshot } from '../v2-model'
import type { V2RetainedReceiveAction } from '../v2-receive-runtime'
import type { V2OutputPresentationSnapshot } from '../v2-output'
import { activeTaskAction, retainedTaskAction } from './actions'
import { recoverySummaryDescription } from '../resumable-file-set-presentation'
import { retainedContinuationReadiness } from '../controller/retained-readiness'
import type { TaskBlocking, TaskCompleteness, TaskFacts, TaskPublication, TaskRecoveryReadiness } from './model'

export function activeTaskFacts(input: Readonly<{
  output: V2OutputPresentationSnapshot
  progress: V2ReceiverProgress
  display?: ReceiveOperationDisplay | null
  blocking?: TaskBlocking | null
  actionAdmission?: (action: import('../v2-lifecycle-presentation').LifecycleUserAction) => Readonly<{ allowed: boolean; reason: string | null }>
}>): TaskFacts | null {
  const { output, progress } = input
  const state = output.lifecycle
  if (state === null) return null
  const lifecycle = output.lifecyclePresentation
  const details = [
    ...(output.transferResultPresentation?.lines ?? []),
    ...(output.recoverySummary === null ? [] : [recoverySummaryDescription(output.recoverySummary)]),
    ...(lifecycle?.writerOpenPause === null || lifecycle?.writerOpenPause === undefined
      ? [] : [lifecycle.writerOpenPause.description]),
    ...(lifecycle?.usage === null || lifecycle?.usage === undefined ? [] : [lifecycle.usage.label]),
  ]
  return Object.freeze({
    lifecycle: state,
    display: input.display ?? null,
    actions: Object.freeze((lifecycle?.actions ?? []).map(action => {
      const presented = activeTaskAction(action, output.plan?.kind ?? '')
      const admission = input.actionAdmission?.(action.kind)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'This action is currently unavailable.' })
        : presented
    })),
    blocking: input.blocking ?? (progress.capacityWaitVisible && progress.capacityWaitingFiles > 0
      ? Object.freeze({ kind: 'sender-capacity' }) : null),
    readiness: 'matching-share',
    completeness: completenessFromFacts(state, progress),
    publication: publicationFromLifecycle(state),
    progress,
    directZipProgress: output.directZipProgress,
    fidelity: lifecycle?.compatibleNameRepair ?? null,
    details: Object.freeze(details),
    interruption: output.receiveInterruption?.operation ?? null,
    execution: Object.freeze({ kind: 'active' }),
  })
}

export function retainedTaskFacts(
  operation: V2RetainedReceivePresentationOperation,
  readiness: TaskRecoveryReadiness = retainedContinuationReadiness(operation, null),
  execution: Readonly<{
    pending?: V2PendingRetainedReceiveActionSnapshot | null
    blocking?: TaskBlocking | null
    admission?: (action: V2RetainedReceiveAction) => Readonly<{ allowed: boolean; reason: string | null }>
  }> = {},
): TaskFacts {
  const details: string[] = []
  const localWork = execution.pending?.operationId === operation.operationId &&
    execution.pending.action === 'continue' &&
    (operation.continuation === 'resume-package' || operation.continuation === 'resume-local-finalization')
    ? 'finalizing' : 'idle'
  if (operation.recoverySummary !== undefined) details.push(recoverySummaryDescription(operation.recoverySummary))
  if (operation.unavailableReason !== undefined) details.push(operation.unavailableReason)
  return Object.freeze({
    lifecycle: operation.lifecycle,
    display: operation.display ?? null,
    actions: Object.freeze(operation.actions.map(action => {
      const presented = retainedTaskAction(operation, action, readiness)
      const admission = execution.admission?.(action)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'Another operation is using this destination.' })
        : presented
    })),
    blocking: execution.blocking ?? null,
    readiness,
    completeness: completenessFromFacts(operation.lifecycle, null),
    publication: publicationFromLifecycle(operation.lifecycle),
    progress: null,
    directZipProgress: null,
    fidelity: operation.repairSummary === undefined ? null : presentCompatibleNameRepair({
      state: operation.lifecycle, summary: operation.repairSummary, context: 'retained-operation',
    }),
    details: Object.freeze(details),
    interruption: null,
    execution: localWork === 'finalizing'
      ? Object.freeze({ kind: 'local-finalization' })
      : Object.freeze({ kind: 'retained', continuation: operation.continuation }),
  })
}

function completenessFromFacts(state: ReceiveLifecycleState, progress: V2ReceiverProgress | null): TaskCompleteness {
  if (state.kind === 'partial-directory') return 'partial'
  if (state.kind === 'resumable-receive') return 'incomplete'
  if (progress !== null && (progress.fileErrors > 0 || progress.selectionErrors > 0 ||
      progress.failedDirectories > 0 || progress.discovery === 'failed')) return 'partial'
  // Native sealing requires complete selected content. Partial ZIP export writes
  // a separate artifact and intentionally leaves this operation resumable.
  if (['published', 'artifact-sealed', 'waiting-to-save', 'download-started',
    'publishing-managed', 'handing-off', 'materialization-sealed', 'packaging',
    'resumable-package'].includes(state.kind)) return 'complete'
  if (progress !== null && progress.discovery === 'complete' &&
      progress.completedFiles === progress.discoveredFiles &&
      progress.completedBytes === progress.discoveredBytes) return 'complete'
  return 'unknown'
}

function publicationFromLifecycle(state: ReceiveLifecycleState): TaskPublication {
  if (state.kind === 'published' || state.kind === 'partial-directory') return 'saved'
  return state.kind === 'download-started' ? 'browser-handoff' : 'unpublished'
}
