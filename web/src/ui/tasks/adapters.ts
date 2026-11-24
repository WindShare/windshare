import type { Language } from '../preferences/model'
import type { ReceiveAction, ReceiveAdmission, ReceiveBlockReason, RetainedTaskSnapshot } from '../../application/receive/contracts'
import { canForgetState, retainedTaskReadiness } from '../../application/receive/admission'
import type { ReceiveOperationDisplay, SaveBackendKind } from '../../output/model'
import type { ReceiveLifecycleState } from '../../transfer/state'
import type { ReceiverProgress } from '../model'
import type { OutputPresentationSnapshot } from '../output'
import {
  activeFallbackForgetAction,
  activeTaskAction,
  retainedFallbackForgetAction,
  retainedTaskAction,
} from './actions'
import type {
  TaskAction,
  TaskBlocking,
  TaskCompleteness,
  TaskFacts,
  TaskPublication,
  TaskRecoveryReadiness,
} from './model'

function applyActionAdmission(
  action: TaskAction<ReceiveBlockReason>,
  admission: ReceiveAdmission | undefined,
  fallbackReason: ReceiveBlockReason,
): TaskAction<ReceiveBlockReason> {
  return admission !== undefined && !admission.allowed
    ? Object.freeze({ ...action, disabledReason: admission.reason ?? fallbackReason })
    : action
}

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
  const fallbackForget = !output.actions.includes('forget') && canForgetState(state, backend)
    ? applyActionAdmission(activeFallbackForgetAction(language), input.actionAdmission?.('forget'), 'actionUnavailable')
    : null
  return Object.freeze({
    lifecycle: state,
    backend,
    display: input.display ?? null,
    actions: Object.freeze(output.actions.map(action =>
      applyActionAdmission(activeTaskAction(action, state.kind, backend, language), input.actionAdmission?.(action), 'actionUnavailable'))),
    fallbackForget,
    blocking: activeBlocking(input.blocking, progress),
    readiness: 'matching-share',
    completeness: completenessFromFacts(state, progress),
    publication: publicationFromLifecycle(state, backend),
    progress,
    safeRenamed: output.safeRenamed,
    sourceOmittedEntries: output.sourceOmittedEntries > 0n
      ? output.sourceOmittedEntries
      : progress.sourceOmittedEntries,
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
  const fallbackForget = !task.actions.includes('forget') && canForgetState(task.lifecycle, task.backend)
    ? applyActionAdmission(retainedFallbackForgetAction(task, language), execution.admission?.('forget'), 'busy')
    : null
  return Object.freeze({
    lifecycle: task.lifecycle,
    backend: task.backend,
    display: task.display ?? null,
    actions: Object.freeze(task.actions.map(action =>
      applyActionAdmission(retainedTaskAction(task, action, readiness, language), execution.admission?.(action), 'busy'))),
    fallbackForget,
    blocking: execution.blocking ?? null,
    readiness,
    completeness: completenessFromFacts(task.lifecycle, null),
    publication: publicationFromLifecycle(task.lifecycle, task.backend),
    progress: null,
    safeRenamed: task.safeRenamed ?? null,
    sourceOmittedEntries: task.sourceOmittedEntries ?? 0n,
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
  if (state.kind === 'paused' || state.kind === 'failed') return 'incomplete'
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
