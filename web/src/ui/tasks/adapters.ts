import type { Language } from '../preferences/model'
import type { ReceiveAction, ReceiveAdmission, RetainedTaskSnapshot } from '../../application/receive/contracts'
import { retainedTaskReadiness } from '../../application/receive/admission'
import type { ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { ReceiveLifecycleState } from '../../transfer/state'
import type { ReceiverProgress } from '../model'
import type { OutputPresentationSnapshot } from '../output'
import { activeTaskAction, retainedTaskAction } from './actions'
import type { TaskBlocking, TaskCompleteness, TaskFacts, TaskPublication, TaskRecoveryReadiness } from './model'

export function activeTaskFacts(input: Readonly<{
  output: OutputPresentationSnapshot
  progress: ReceiverProgress
  display?: ReceiveOperationDisplay | null
  blocking?: TaskBlocking | null
  actionAdmission?: (action: ReceiveAction) => ReceiveAdmission
}>, language: Language = 'en'): TaskFacts | null {
  const { output, progress } = input
  const state = output.lifecycle
  if (state === null || output.backend === null) return null

  const backend = output.backend
  return Object.freeze({
    lifecycle: state,
    backend,
    display: input.display ?? null,
    actions: Object.freeze(output.actions.map(action => {
      const presented = activeTaskAction(action, state.kind, backend, language)
      const admission = input.actionAdmission?.(action)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'actionUnavailable' as const })
        : presented
    })),
    blocking: activeBlocking(input.blocking, progress),
    readiness: 'matching-share',
    completeness: completenessFromFacts(state, progress),
    publication: publicationFromLifecycle(state, backend),
    progress,
    safeRenamed: output.safeRenamed,
    details: Object.freeze([]),
    outageCheckpointed: output.outageCheckpointed,
    interruption: output.interruption,
  })
}

export function retainedTaskFacts(
  task: RetainedTaskSnapshot,
  readiness: TaskRecoveryReadiness = retainedTaskReadiness(task, null),
  execution: Readonly<{
    blocking?: TaskBlocking | null
    admission?: (action: ReceiveAction) => ReceiveAdmission
  }> = {},
  language: Language = 'en',
): TaskFacts {
  return Object.freeze({
    lifecycle: task.lifecycle,
    backend: task.backend,
    display: task.display ?? null,
    actions: Object.freeze(task.actions.map(action => {
      const presented = retainedTaskAction(task, action, readiness, language)
      const admission = execution.admission?.(action)
      return admission !== undefined && !admission.allowed
        ? Object.freeze({ ...presented, disabledReason: admission.reason ?? 'busy' as const })
        : presented
    })),
    blocking: execution.blocking ?? null,
    readiness,
    completeness: completenessFromFacts(task.lifecycle, null),
    publication: publicationFromLifecycle(task.lifecycle, task.backend),
    progress: null,
    safeRenamed: task.safeRenamed ?? null,
    details: Object.freeze([]),
    outageCheckpointed: false,
    interruption: null,
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
  if (state.kind === 'paused') return 'incomplete'
  if (state.contentWarning !== undefined || state.kind === 'partial') return 'partial'
  if (progress !== null && (progress.fileErrors > 0 || progress.selectionErrors > 0 ||
      progress.failedDirectories > 0 || progress.discovery === 'failed')) {
    return 'partial'
  }
  if (state.kind === 'saved' || state.kind === 'ready-to-save' || state.kind === 'download-started') {
    return 'complete'
  }
  if (progress !== null && progress.discovery === 'complete' &&
      progress.completedFiles === progress.discoveredFiles &&
      progress.completedBytes === progress.discoveredBytes) {
    return 'complete'
  }
  return 'unknown'
}

function publicationFromLifecycle(state: ReceiveLifecycleState, backend: SaveBackendKind): TaskPublication {
  if (state.kind === 'saved' || (state.kind === 'partial' && backend === 'fsa')) return 'saved'
  return state.kind === 'download-started' ? 'browser-handoff' : 'unpublished'
}
