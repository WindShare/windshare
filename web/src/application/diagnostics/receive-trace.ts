import type { ConnectivityTraceEvent, ConnectivityTraceSource } from '../../connectivity/diagnostics'
import type { DiagnosticsRuntime, TraceEntry, TraceSource } from '../../diagnostics/runtime'
import type { OutputTraceEvent, OutputTraceSource } from '../../output/diagnostics'
import { formatDiagnosticValue } from '../../security/diagnostic-formatter'
import type { ProtocolTraceSource } from '../../session/diagnostics'
import type { ProtocolCorrelation } from '../../session/identities'
import type { ReceiverTraceEvent } from '../receive/contracts'
import { projectCorrelation, projectProtocolTraceEvent } from './protocol-trace'

export { projectCorrelation, projectProtocolTraceEvent } from './protocol-trace'

export interface FailureReportEvent {
  readonly stage: string
  readonly error?: unknown
  readonly retryable?: boolean
  readonly operation?: string
}

export type FailureReporter = (event: FailureReportEvent) => void

export function createFailureReporter(runtime: DiagnosticsRuntime): FailureReporter {
  return event => {
    runtime.recordFailure({
      stage: event.stage,
      ...(event.operation === undefined ? {} : { operation: event.operation }),
      ...(event.error === undefined ? {} : { error: event.error }),
      ...(event.retryable === undefined ? {} : { context: { retryable: event.retryable } }),
      consoleReport: true,
    })
  }
}

export function createReceiverTraceSource(
  trace: TraceSource<TraceEntry>,
): TraceSource<ReceiverTraceEvent> {
  return adaptTraceSource(trace, projectReceiverTraceEvent)
}

export function createOutputTraceSource(
  trace: TraceSource<TraceEntry>,
): OutputTraceSource {
  return adaptTraceSource(trace, projectOutputTraceEvent)
}

export function createProtocolTraceSource(
  runtime: Pick<DiagnosticsRuntime, 'trace' | 'recordFailure'>,
): ProtocolTraceSource {
  const detailed = adaptTraceSource(runtime.trace, projectProtocolTraceEvent)
  return Object.freeze({
    get current() { return detailed.current },
    failure: (
      error: unknown,
      operation: string,
      correlation: ProtocolCorrelation,
      context?: Readonly<Record<string, unknown>>,
    ) => {
      const projected = projectCorrelation(correlation)
      runtime.recordFailure({
        stage: 'protocol_operation',
        operation,
        error,
        ...(projected === undefined ? {} : { correlation: { ...projected } }),
        ...(context === undefined ? {} : { context }),
      })
    },
  })
}

export function createConnectivityTraceSource(
  trace: TraceSource<TraceEntry>,
): ConnectivityTraceSource {
  return adaptTraceSource(trace, projectConnectivityTraceEvent)
}

export function projectReceiverTraceEvent(event: ReceiverTraceEvent): TraceEntry {
  switch (event.name) {
    case 'receiver_experience':
      return { event: 'receiver_experience', payload: projectExperiencePayload(event) }
    case 'preview_transition':
      return {
        event: 'preview_transition',
        payload: {
          attempt: 'open',
          transition: event.transition,
          intent: 'automatic_photo',
          preview_id: String(event.previewId),
          share_instance_id: event.shareInstanceId,
          file_id: event.fileId,
          error: formatDiagnosticValue(event.error, { maxDepth: 4, maxEntries: 8, maxStringCharacters: 256 }),
        },
      }
    case 'join_transition':
      return { event: 'join_transition', payload: { transition: event.transition } }
  }
}

export function projectOutputTraceEvent(event: OutputTraceEvent): TraceEntry {
  return { event: event.eventName, payload: { ...event.payload } }
}

export function projectConnectivityTraceEvent(event: ConnectivityTraceEvent): TraceEntry {
  const correlation = projectCorrelation(event.correlation)
  const envelope = correlation === undefined ? {} : { correlation }
  if (event.eventName === 'peer_attempt') {
    return { event: 'peer_attempt', ...envelope, payload: projectPeerAttemptPayload(event) }
  }
  return { event: 'peer_recovery', ...envelope, payload: projectPeerRecoveryPayload(event) }
}

function projectExperiencePayload(
  event: Extract<ReceiverTraceEvent, { name: 'receiver_experience' }>,
): Readonly<Record<string, unknown>> {
  if (event.transition === 'task') {
    return {
      transition: 'task',
      operation_id: event.operationId,
      generation: String(event.generation),
      stage: event.stage,
      reason: event.reason,
      attention: event.attention,
      completeness: event.completeness,
      publication: event.publication,
      elapsed_ms: event.elapsedMilliseconds === null ? null : String(event.elapsedMilliseconds),
    }
  }
  if (event.transition === 'saving') {
    return {
      transition: 'saving',
      projection_epoch: event.projectionEpoch === null ? null : String(event.projectionEpoch),
      choice_id: event.choiceId,
      outcome: event.outcome,
      reason: event.reason,
    }
  }
  return {
    transition: 'intent',
    action: event.action,
    operation_id: event.operationId,
    generation: String(event.generation),
  }
}

function projectPeerAttemptPayload(
  event: Extract<ConnectivityTraceEvent, { readonly eventName: 'peer_attempt' }>,
): Readonly<Record<string, unknown>> {
  const base = projectPeerAttemptMilestone(event)
  if (event.stage !== 'failed' && event.stage !== 'admitted') return base
  return {
    ...base,
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
): Readonly<Record<string, unknown>> {
  switch (event.stage) {
    case 'provider-fact':
      return { stage: 'provider_fact', fact: event.fact }
    case 'started':
      return {
        stage: event.stage,
        wave_ordinal: String(event.waveOrdinal),
        wave_attempt_ordinal: String(event.waveAttemptOrdinal),
        session_attempt_ordinal: String(event.sessionAttemptOrdinal),
      }
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
        local_candidates_emitted: String(event.candidateCounts.localEmitted),
        remote_candidates_accepted: String(event.candidateCounts.remoteAccepted),
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
): Readonly<Record<string, unknown>> {
  switch (event.stage) {
    case 'turn_demand_sent': case 'turn_refresh_requested': case 'turn_grants_received':
    case 'turn_path_admitted': case 'turn_path_closed':
      return Object.fromEntries(Object.entries({
        stage: event.stage,
        service_id: event.serviceId,
        grant_id: event.grantId,
        mode: event.mode,
        status: event.status,
        reason: event.reason,
      }).filter(([, value]) => value !== undefined))
    case 'parallelism':
      return {
        stage: 'parallelism',
        action: event.action,
        reason: event.reason,
        paths: event.paths,
        network_waiting_bytes: String(event.networkWaitingBytes),
        in_flight_bytes: String(event.inFlightBytes),
        remaining_bytes: String(event.remainingBytes),
        remaining_known: event.remainingKnown,
        queued_bytes: String(event.queuedBytes),
        unfinished_bytes: String(event.unfinishedBytes),
        capacity_waiting_bytes: String(event.capacityWaitingBytes),
        useful_rate: event.usefulRate,
        output_rate: event.outputRate,
        baseline_useful_rate: event.baselineUsefulRate,
        baseline_output_rate: event.baselineOutputRate,
        ...(event.retryAfterMilliseconds === undefined ? {} : { retry_after_ms: event.retryAfterMilliseconds }),
        ...(event.trialPath === undefined ? {} : {
          trial_path: { path_id: event.trialPath.pathId, lane_id: event.trialPath.laneId, lane_epoch: event.trialPath.laneEpoch },
        }),
        ...(event.trialWork === undefined ? {} : {
          trial_work: {
            offered_bytes: String(event.trialWork.offeredBytes),
            settled_bytes: String(event.trialWork.settledBytes),
            completed_bytes: String(event.trialWork.completedBytes),
            completed_attempts: String(event.trialWork.completedAttempts),
            failed_attempts: String(event.trialWork.failedAttempts),
            superseded_attempts: String(event.trialWork.supersededAttempts),
            cancelled_attempts: String(event.trialWork.cancelledAttempts),
            busy_ms: String(event.trialWork.busyMilliseconds),
          },
        }),
      }
    case 'wave-started':
    case 'wave-rearmed':
      return { stage: snake(event.stage), wave_ordinal: String(event.waveOrdinal), trigger: snake(event.trigger) }
    case 'retry-decided':
      return {
        stage: 'retry_decided',
        wave_ordinal: String(event.waveOrdinal),
        decision: snake(event.decision),
        reason: snake(event.reason),
        authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
      }
    case 'backoff-scheduled':
      return {
        stage: 'backoff_scheduled',
        wave_ordinal: String(event.waveOrdinal),
        retry_ordinal: String(event.retryOrdinal),
        local_delay_ms: event.localDelayMilliseconds,
        authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
        effective_delay_ms: event.effectiveDelayMilliseconds,
      }
    case 'attempt-replaced':
      return { stage: 'attempt_replaced', wave_ordinal: String(event.waveOrdinal) }
    case 'wave-quiesced':
      return { stage: 'wave_quiesced', wave_ordinal: String(event.waveOrdinal), reason: snake(event.reason) }
    case 'peer-detached':
      return { stage: 'peer_detached' }
    case 'session-budget-exhausted':
    case 'path-stopped':
    case 'session-stopped':
      return { stage: snake(event.stage), reason: snake(event.reason) }
  }
}

function adaptTraceSource<Input>(
  trace: TraceSource<TraceEntry>,
  project: (event: Input) => TraceEntry,
): TraceSource<Input> {
  return Object.freeze({
    get current() {
      const observer = trace.current
      if (observer === undefined) return undefined
      return (event: Input) => observer(project(event))
    },
  })
}

function snake(value: string): string {
  return value.replaceAll('-', '_')
}
