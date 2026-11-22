import type { Language } from '../preferences/model'
import type { ReceiveAdmission } from '../../application/receive/admission'
import { presentReceiveReason } from '../experience/receive-reason'
import type { ReceiveLifecycleState } from '../../transfer/lifecycle/state'
import type { ReceiveOperationDisplay } from '../../output/model/operation-display'
import type { ReceiverProgress } from '../model'
import type { RetainedReceiveOperationSnapshot, PendingRetainedReceiveActionSnapshot } from '../../application/receive/retained/model'
import type { RetainedReceiveAction } from '../../application/receive/contracts'
import type { OutputPresentationSnapshot } from '../output'
import { activeTaskAction, retainedTaskAction } from './actions'
import { recoverySummaryDescription } from '../resumable-file-set-presentation'
import { retainedContinuationReadiness } from '../../application/receive/retained/readiness'
import type { TaskBlocking, TaskCompleteness, TaskFacts, TaskPublication, TaskRecoveryReadiness } from './model'

export function activeTaskFacts(input: Readonly<{
  output: OutputPresentationSnapshot
  progress: ReceiverProgress
  display?: ReceiveOperationDisplay | null
  blocking?: TaskBlocking | null
  actionAdmission?: (action: import('../../application/receive/actions').LifecycleUserAction) => ReceiveAdmission
}>, language: Language = 'en'): TaskFacts | null {
  const { output, progress } = input
  const state = output.lifecycle
  if (state === null) return null
  const lifecycle = output.lifecyclePresentation
  const details = [
    ...(output.transferResultPresentation?.lines ?? []),
    ...(output.recoverySummary === null ? [] : [recoverySummaryDescription(output.recoverySummary, language)]),
    ...(lifecycle?.writerOpenPause === null || lifecycle?.writerOpenPause === undefined
      ? [] : [lifecycle.writerOpenPause.description]),
    ...(lifecycle?.usage === null || lifecycle?.usage === undefined ? [] : [lifecycle.usage.label]),
  ]
  return Object.freeze({
    lifecycle: state,
    display: input.display ?? null,
    actions: Object.freeze((lifecycle?.actions ?? []).map(action => {
      const presented = activeTaskAction(action, output.plan?.kind ?? '', language)
      const admission = input.actionAdmission?.(action.kind)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'actionUnavailable' as const })
        : presented
    })),
    blocking: activeBlocking(input.blocking, progress),
    readiness: 'matching-share',
    completeness: completenessFromFacts(state, progress),
    publication: publicationFromLifecycle(state),
    progress,
    browserDelivery: output.browserFolderProgress?.summary ?? null,
    safeRenamed: output.safeRenamed,
    details: Object.freeze(details),
    interruption: output.receiveInterruption?.operation ?? null,
    execution: Object.freeze({ kind: 'active' }),
  })
}

export function retainedTaskFacts(
  operation: RetainedReceiveOperationSnapshot,
  readiness: TaskRecoveryReadiness = retainedContinuationReadiness(operation, null),
  execution: Readonly<{
    pending?: PendingRetainedReceiveActionSnapshot | null
    blocking?: TaskBlocking | null
    admission?: (action: RetainedReceiveAction) => ReceiveAdmission
  }> = {},
  language: Language = 'en',
): TaskFacts {
  const details: string[] = []
  const localWork = execution.pending?.operationId === operation.operationId &&
    (execution.pending.action === 'save-staged-files' || execution.pending.action === 'cleanup-staging' ||
      execution.pending.action === 'discard-incomplete-staging' ||
      (execution.pending.action === 'continue' &&
        (operation.continuation === 'resume-package' || operation.continuation === 'resume-local-finalization')))
    ? 'finalizing' : 'idle'
  if (operation.recoverySummary !== undefined) details.push(recoverySummaryDescription(operation.recoverySummary, language))
  if (operation.unavailableReason !== undefined) details.push(presentReceiveReason(operation.unavailableReason, language))
  return Object.freeze({
    lifecycle: operation.lifecycle,
    display: operation.display ?? null,
    actions: Object.freeze(operation.actions.map(action => {
      const presented = retainedTaskAction(operation, action, readiness, language)
      const admission = execution.admission?.(action)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'destinationBusy' as const })
        : presented
    })),
    blocking: execution.blocking ?? null,
    readiness,
    completeness: completenessFromFacts(operation.lifecycle, null),
    publication: publicationFromLifecycle(operation.lifecycle),
    progress: null,
    browserDelivery: operation.browserDelivery ?? null,
    safeRenamed: null,
    details: Object.freeze(details),
    interruption: null,
    execution: localWork === 'finalizing'
      ? Object.freeze({ kind: 'local-finalization' })
      : Object.freeze({ kind: 'retained', continuation: operation.continuation }),
  })
}

const SENDER_CAPACITY_BLOCKING: TaskBlocking = Object.freeze({ kind: 'sender-capacity' })
const RELAY_BANDWIDTH_BLOCKING: TaskBlocking = Object.freeze({ kind: 'relay-bandwidth' })

function activeBlocking(blocking: TaskBlocking | null | undefined, progress: ReceiverProgress): TaskBlocking | null {
  if (blocking != null) return blocking
  if (progress.capacityWaitVisible && progress.capacityWaitingFiles > 0) return SENDER_CAPACITY_BLOCKING
  return progress.bandwidthWaitVisible ? RELAY_BANDWIDTH_BLOCKING : null
}

function completenessFromFacts(state: ReceiveLifecycleState, progress: ReceiverProgress | null): TaskCompleteness {
  // Paused receives remain resumable rather than finalized partial archives.
  if (state.kind === 'resumable-start' || state.kind === 'resumable-receive') return 'incomplete'
  if (state.contentWarning !== undefined || state.kind === 'partial-directory') return 'partial'
  if (progress !== null && (progress.fileErrors > 0 || progress.selectionErrors > 0 ||
      progress.failedDirectories > 0 || progress.discovery === 'failed')) return 'partial'
  // Publication proves that an artifact is ready. Its retained warning separately records selection omissions.
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
