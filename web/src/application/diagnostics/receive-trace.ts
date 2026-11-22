import type { FailureEvidenceHistoryPort } from '../../diagnostics/failure/history'
import { projectDiagnosticException } from '../../diagnostics/exception'
import { unclassifiedFailureFact, type FailureCorrelation } from '../../diagnostics/incident/fact'
import { CONNECTION_TRACE_TEXT_MAX_CHARACTERS } from '../../diagnostics/trace/connection-payload'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'
import { projectProtocolTraceEvent } from './protocol-trace'
export { projectProtocolTraceEvent } from './protocol-trace'
import { correlatedObservation, decimal, observation, requiredCorrelation, snake } from '../../diagnostics/export/trace-observation'
import type { CatalogStorageObservation } from '../browser-share/gateway'
import type { ConnectivityTraceEvent, ConnectivityTraceSource } from '../../connectivity/diagnostics'
import type { TraceEventObservationV2, TraceEventPayloadByNameV2 } from '../../diagnostics/trace/model'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { OutputTraceEvent, OutputTraceSource } from '../../output/diagnostics'
import type { ProtocolTraceSource } from '../../session/diagnostics'
import type { ReceiverTraceEvent } from '../receive/contracts'

type BrowserTraceSource = DomainTraceSource<TraceEventObservationV2>

export function createReceiverTraceSource(
  trace: BrowserTraceSource,
): DomainTraceSource<ReceiverTraceEvent> {
  return adaptTraceSource(trace, projectReceiverTraceEvent)
}

export function createOutputTraceSource(
  trace: BrowserTraceSource,
): OutputTraceSource {
  return adaptTraceSource(trace, projectOutputTraceEvent)
}

export function createProtocolTraceSource(
  trace: BrowserTraceSource,
  failures?: FailureEvidenceHistoryPort,
): ProtocolTraceSource {
  const detailed = adaptTraceSource(trace, projectProtocolTraceEvent)
  return Object.freeze({
    get current() { return detailed.current },
    failure: (error: unknown, operation: string, correlation: FailureCorrelation, relation?: 'consequence') => failures?.record(unclassifiedFailureFact({
      error, operation, correlation, stage: 'protocol_operation', recoveryDisposition: 'none',
    }), relation),
  })
}

export function createConnectivityTraceSource(
  trace: BrowserTraceSource,
): ConnectivityTraceSource {
  return adaptTraceSource(trace, projectConnectivityTraceEvent)
}

export function createCatalogStorageObserver(
  trace: BrowserTraceSource,
): (event: CatalogStorageObservation) => void {
  return event => {
    trace.current?.(observation('catalog_storage', {
      share_instance_id: event.shareInstanceId.slice(0, CONNECTION_TRACE_TEXT_MAX_CHARACTERS),
      transition: event.transition,
      reason: snake(event.reason),
      ...(event.failure === undefined ? {} : {
        failure_detail: formatDiagnosticText(event.failure).slice(0, CONNECTION_TRACE_TEXT_MAX_CHARACTERS),
      }),
    }))
  }
}

export function projectReceiverTraceEvent(event: ReceiverTraceEvent): TraceEventObservationV2 {
  switch (event.name) {
    case 'receiver_experience':
      if (event.transition === 'task') {
        return observation('receiver_experience', {
          transition: 'task',
          operation_id: event.operationId,
          generation: decimal(event.generation),
          stage: event.stage,
          reason: event.reason,
          attention: event.attention,
          completeness: event.completeness,
          publication: event.publication,
          elapsed_ms: event.elapsedMilliseconds === null ? null : String(event.elapsedMilliseconds),
        })
      }
      if (event.transition === 'saving') {
        return observation('receiver_experience', {
          transition: 'saving',
          projection_epoch: event.projectionEpoch === null ? null : decimal(event.projectionEpoch),
          choice_id: event.choiceId,
          outcome: event.outcome,
          reason: event.reason,
        })
      }
      if (event.transition === 'ownership') {
        return observation('receiver_experience', {
          transition: 'ownership',
          state: event.state,
          operation_id: event.operationId,
          generation: decimal(event.generation),
          lifecycle_state: snake(event.lifecycleKind),
        })
      }
      return observation('receiver_experience', {
        transition: 'intent',
        action: event.action,
        operation_id: event.operationId,
        generation: decimal(event.generation),
      })
    case 'preview_transition':
      return observation('preview_transition', {
        attempt: 'open',
        transition: event.transition,
        intent: 'automatic_photo',
        preview_id: decimal(event.previewId),
        share_instance_id: event.shareInstanceId,
        file_id: event.fileId,
        exception: projectDiagnosticException(event.error),
      })
    case 'join_transition':
      return observation('join_transition', { transition: event.transition })
  }
}

export function projectOutputTraceEvent(event: OutputTraceEvent): TraceEventObservationV2 {
  return Object.freeze({
    eventName: event.eventName,
    payload: Object.freeze({ ...event.payload }),
  }) as TraceEventObservationV2
}

export function projectConnectivityTraceEvent(event: ConnectivityTraceEvent): TraceEventObservationV2 {
  const correlation = requiredCorrelation(event.correlation)
  if (event.eventName === 'peer_attempt') {
    return correlatedObservation(event.eventName, correlation, projectPeerAttemptPayload(event))
  }
  return correlatedObservation(event.eventName, correlation, projectPeerRecoveryPayload(event))
}

function projectPeerAttemptPayload(
  event: Extract<ConnectivityTraceEvent, { readonly eventName: 'peer_attempt' }>,
): TraceEventPayloadByNameV2['peer_attempt'] {
  const payload = projectPeerAttemptMilestone(event)
  if (event.stage !== 'failed' && event.stage !== 'admitted') return payload
  return {
    ...payload,
    ...(event.stage === 'failed' ? { failure: event.failure } : {}),
    ...(event.summary === undefined ? {} : {
      summary: {
        last_completed_stage: snake(event.summary.lastCompletedStage),
        attempt_elapsed_ms: event.summary.attemptElapsedMilliseconds,
        stage_elapsed_ms: event.summary.stageElapsedMilliseconds,
        deadline_expired: event.summary.deadlineExpired,
      },
    }),
  }
}

function projectPeerAttemptMilestone(
  event: Extract<ConnectivityTraceEvent, { readonly eventName: 'peer_attempt' }>,
): TraceEventPayloadByNameV2['peer_attempt'] {
  const ordinals = {
    wave_ordinal: decimal(event.waveOrdinal),
    wave_attempt_ordinal: decimal(event.waveAttemptOrdinal),
    session_attempt_ordinal: decimal(event.sessionAttemptOrdinal),
  }
  switch (event.stage) {
    case 'provider-fact':
      return { stage: 'provider_fact', fact: event.fact }
    case 'started':
      return { stage: event.stage, ...ordinals }
    case 'negotiation-deadline-armed':
    case 'negotiation-deadline-expired':
    case 'admission-deadline-armed':
    case 'admission-deadline-expired':
      return { stage: snake(event.stage), deadline_budget_ms: event.deadlineBudgetMilliseconds }
    case 'offer-created':
    case 'offer-sent':
    case 'answer-received':
    case 'datachannel-open':
      return {
        stage: snake(event.stage),
        local_candidates_emitted: decimal(event.candidateCounts.localEmitted),
        remote_candidates_accepted: decimal(event.candidateCounts.remoteAccepted),
      }
    case 'grant-requested':
      return { stage: 'grant_requested', requested_lane_id: event.requestedLaneId }
    case 'admission-response-settled':
      return {
        stage: 'admission_response_settled',
        settlement: event.settlement.disposition === 'accepted'
          ? { disposition: 'accepted' }
          : {
              disposition: 'rejected',
              rejection_code: event.settlement.rejectionCode,
              retry_after_ms: event.settlement.retryAfterMilliseconds,
            },
      }
    case 'failed':
      return {
        stage: event.stage,
        failed_at_stage: snake(event.failedAtStage),
        failure_scope: event.failureScope,
        code: snake(event.typedErrorCode),
        retryable: event.retryable,
      }
    default:
      return { stage: snake(event.stage) }
  }
}

function projectPeerRecoveryPayload(
  event: Extract<ConnectivityTraceEvent, { readonly eventName: 'peer_recovery' }>,
): TraceEventPayloadByNameV2['peer_recovery'] {
  switch (event.stage) {
    case 'parallelism':
      return {
        stage: 'parallelism', action: event.action, reason: event.reason, paths: event.paths,
        network_waiting_bytes: decimal(event.networkWaitingBytes), in_flight_bytes: decimal(event.inFlightBytes),
        remaining_bytes: decimal(event.remainingBytes), remaining_known: event.remainingKnown,
        queued_bytes: decimal(event.queuedBytes), unfinished_bytes: decimal(event.unfinishedBytes),
        capacity_waiting_bytes: decimal(event.capacityWaitingBytes),
        useful_rate: event.usefulRate, output_rate: event.outputRate,
        baseline_useful_rate: event.baselineUsefulRate, baseline_output_rate: event.baselineOutputRate,
        ...(event.retryAfterMilliseconds === undefined ? {} : { retry_after_ms: event.retryAfterMilliseconds }),
        ...(event.trialPath === undefined ? {} : {
          trial_path: { path_id: event.trialPath.pathId, lane_id: event.trialPath.laneId, lane_epoch: event.trialPath.laneEpoch },
        }),
        ...(event.trialWork === undefined ? {} : {
          trial_work: {
            offered_bytes: decimal(event.trialWork.offeredBytes), settled_bytes: decimal(event.trialWork.settledBytes),
            completed_bytes: decimal(event.trialWork.completedBytes), completed_attempts: decimal(event.trialWork.completedAttempts),
            failed_attempts: decimal(event.trialWork.failedAttempts), superseded_attempts: decimal(event.trialWork.supersededAttempts),
            cancelled_attempts: decimal(event.trialWork.cancelledAttempts), busy_ms: decimal(event.trialWork.busyMilliseconds),
          },
        }),
      }
    case 'wave-started':
    case 'wave-rearmed':
      return { stage: snake(event.stage), wave_ordinal: decimal(event.waveOrdinal), trigger: snake(event.trigger) }
    case 'retry-decided':
      return {
        stage: 'retry_decided', wave_ordinal: decimal(event.waveOrdinal), decision: snake(event.decision),
        reason: snake(event.reason), authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
      }
    case 'backoff-scheduled':
      return {
        stage: 'backoff_scheduled', wave_ordinal: decimal(event.waveOrdinal), retry_ordinal: decimal(event.retryOrdinal),
        local_delay_ms: event.localDelayMilliseconds, authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
        effective_delay_ms: event.effectiveDelayMilliseconds,
      }
    case 'attempt-replaced':
      return { stage: 'attempt_replaced', wave_ordinal: decimal(event.waveOrdinal) }
    case 'wave-quiesced':
      return { stage: 'wave_quiesced', wave_ordinal: decimal(event.waveOrdinal), reason: snake(event.reason) }
    case 'peer-detached':
      return { stage: 'peer_detached' }
    case 'session-budget-exhausted':
      return { stage: 'session_budget_exhausted', reason: snake(event.reason) }
    case 'path-stopped':
      return { stage: 'path_stopped', reason: snake(event.reason) }
    case 'session-stopped':
      return { stage: 'session_stopped', reason: snake(event.reason) }
  }
}

function adaptTraceSource<Input>(
  trace: BrowserTraceSource,
  project: (event: Input) => TraceEventObservationV2,
): DomainTraceSource<Input> {
  return Object.freeze({
    get current() {
      const observer = trace.current
      if (observer === undefined) return undefined
      return (event: Input) => observer(project(event))
    },
  })
}
