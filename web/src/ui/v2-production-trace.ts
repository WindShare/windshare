import type {
  V2ConnectivityTraceEvent,
  V2ConnectivityTraceSource,
} from '../connectivity/diagnostics'
import { projectCorrelationV1 } from '../diagnostics/export/correlation-v1'
import type {
  ProtocolFailureV1,
} from '../diagnostics/export/incident-record-v1'
import type { ProtocolFailure } from '../diagnostics/incident'
import type {
  TraceEventObservationV1,
  TraceEventPayloadByNameV1,
} from '../diagnostics/trace/model'
import type { DomainTraceSource } from '../diagnostics/trace/ports'
import type {
  OutputTraceEvent,
  OutputTraceSource,
} from '../output/diagnostics'
import type { ProjectionTraceEvent } from '../transfer/projection'
import type { TransferTraceEvent } from '../transfer/v2-job'
import type {
  V2ProtocolTraceEvent,
  V2ProtocolTraceSource,
} from '../session/v2-diagnostics'
import type { V2ReceiverTraceEvent } from './v2-controller'

type BrowserTraceSource = DomainTraceSource<TraceEventObservationV1>

export function createV2ReceiverTraceSource(
  trace: BrowserTraceSource,
): DomainTraceSource<V2ReceiverTraceEvent> {
  return adaptTraceSource(trace, projectV2ReceiverTraceEvent)
}

export function createOutputTraceSource(
  trace: BrowserTraceSource,
): OutputTraceSource {
  return adaptTraceSource(trace, projectOutputTraceEvent)
}

export function createProtocolTraceSource(
  trace: BrowserTraceSource,
): V2ProtocolTraceSource {
  return adaptTraceSource(trace, projectProtocolTraceEvent)
}

export function createConnectivityTraceSource(
  trace: BrowserTraceSource,
): V2ConnectivityTraceSource {
  return adaptTraceSource(trace, projectConnectivityTraceEvent)
}

export function projectV2ReceiverTraceEvent(
  event: V2ReceiverTraceEvent,
): TraceEventObservationV1 {
  switch (event.name) {
    case 'join_transition':
      return observation('join_transition', { transition: event.transition })
    case 'projection_transition':
      return projectProjectionTraceEvent(event)
    case 'authority_transition':
      return projectAuthorityTraceEvent(event)
    case 'receive_transition':
    case 'transfer_progress':
      return projectTransferTraceEvent(event)
    case 'lifecycle_action_transition':
      return observation('lifecycle_action_transition', {
        transition: event.transition,
        action: snake(event.action),
        ...(event.lifecycleKind === undefined
          ? {}
          : { lifecycle_state: snake(event.lifecycleKind) }),
      })
    case 'receive.inventory.load.started':
      return observation('retained_inventory', { transition: 'load_started' })
    case 'receive.inventory.load.completed':
      return observation('retained_inventory', {
        transition: 'load_completed',
        operation_count: decimal(event.operation_count),
      })
    case 'receive.inventory.load.failed':
      return observation('retained_inventory', { transition: 'load_failed' })
    case 'receive.inventory.action.started':
    case 'receive.inventory.action.completed':
    case 'receive.inventory.action.failed':
      return observation('retained_action', {
        transition: retainedActionTransition(event.name),
        action: event.retained_action,
        continuation: snake(event.continuation),
      })
  }
}

export function projectOutputTraceEvent(
  event: OutputTraceEvent,
): TraceEventObservationV1 {
  // Output's diagnostic port already owns the frozen V1 field vocabulary. This
  // boundary detaches it while the recorder performs the shared exact-key check.
  return Object.freeze({
    eventName: event.eventName,
    payload: Object.freeze({ ...event.payload }),
  }) as TraceEventObservationV1
}

export function projectProtocolTraceEvent(
  event: V2ProtocolTraceEvent,
): TraceEventObservationV1 {
  const correlation = requiredCorrelation(event.correlation)
  if (event.eventName === 'lane_transition') {
    switch (event.transition) {
      case 'admission_rejected':
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
          rejection_code: event.rejectionCode,
          retry_after_ms: event.retryAfterMilliseconds,
        })
      case 'detached':
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
          detachment_class: event.detachmentClass,
        })
      default:
        return correlatedObservation(event.eventName, correlation, {
          transition: event.transition,
        })
    }
  }

  switch (event.transition) {
    case 'response_received':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        response_kind: event.responseKind,
      })
    case 'authenticated_failure':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        protocol_failure: projectProtocolFailure(event.protocolFailure),
      })
    case 'settled':
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
        settlement: event.settlement,
      })
    default:
      return correlatedObservation(event.eventName, correlation, {
        transition: event.transition,
        request_kind: event.requestKind,
      })
  }
}

export function projectConnectivityTraceEvent(
  event: V2ConnectivityTraceEvent,
): TraceEventObservationV1 {
  const correlation = requiredCorrelation(event.correlation)
  if (event.eventName === 'peer_attempt') {
    return correlatedObservation(
      event.eventName,
      correlation,
      projectPeerAttemptPayload(event),
    )
  }
  return correlatedObservation(
    event.eventName,
    correlation,
    projectPeerRecoveryPayload(event),
  )
}

function projectProjectionTraceEvent(
  event: ProjectionTraceEvent,
): TraceEventObservationV1 {
  switch (event.transition) {
    case 'started':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
      })
    case 'refined':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        shape_proof: snake(event.shapeProof),
        discovery_state: snake(event.discoveryState),
        file_count_lower_bound: decimal(event.fileCountLowerBound),
        directory_count_lower_bound: decimal(event.directoryCountLowerBound),
        byte_count_lower_bound: decimal(event.byteCountLowerBound),
        unsettled_target_count: decimal(event.unsettledTargetCount),
      })
    case 'proven':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        shape_proof: snake(event.shapeProof),
        layout_basis_class: snake(event.layoutBasisClass),
      })
    case 'retryable_failure':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        shape_proof: snake(event.shapeProof),
        retryable_discovery_reason: snake(event.retryableDiscoveryReason),
      })
    case 'retry_started':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        retained_shape_proof: snake(event.retainedShapeProof),
      })
    case 'stale_event_dropped':
      return observation(event.name, {
        transition: event.transition,
        current_projection_epoch: decimal(event.currentProjectionEpoch),
        stale_projection_epoch: decimal(event.staleProjectionEpoch),
        event_class: event.eventClass,
      })
  }
}

function projectAuthorityTraceEvent(
  event: Extract<V2ReceiverTraceEvent, { readonly name: 'authority_transition' }>,
): TraceEventObservationV1 {
  switch (event.transition) {
    case 'offers_computed':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        shape_proof: snake(event.shapeProof),
        offered_artifact_kinds: event.offeredArtifactKinds.map(snake),
        offered_plan_kinds: event.offeredPlanKinds.map(snake),
        primary_artifact_kind: snake(event.primaryArtifactKind),
      })
    case 'offers_disabled':
      return observation(event.name, {
        transition: event.transition,
        projection_epoch: decimal(event.projectionEpoch),
        shape_proof: snake(event.shapeProof),
        reason: snake(event.reason),
        ...(event.hardLimitClass === undefined
          ? {}
          : { hard_limit_class: snake(event.hardLimitClass) }),
      })
    case 'stale_event_dropped':
      return observation(event.name, {
        transition: event.transition,
        current_projection_epoch: decimal(event.currentProjectionEpoch),
        stale_projection_epoch: decimal(event.staleProjectionEpoch),
        event_class: snake(event.eventClass),
      })
    case 'activation_started':
    case 'artifact_resolved':
    case 'commit_started':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
      })
    case 'commit_pre_cut_retry':
    case 'cleanup_completed':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        ...(event.receiverOperationId === undefined
          ? {}
          : { receiver_operation_id: event.receiverOperationId }),
      })
    case 'prerequisite_waiting':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        waiting_for: snake(event.waitingFor),
      })
    case 'retry_required':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        retryable_discovery_reason: snake(event.retryableDiscoveryReason),
      })
    case 'semantic_invalidated':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        invalidation_reason: snake(event.invalidationReason),
      })
    case 'commit_bound_operation':
    case 'commit_owned_effects':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        receiver_operation_id: event.receiverOperationId,
      })
    case 'cleanup_failed':
      return observation(event.name, {
        transition: event.transition,
        ...projectAuthorityActivationContext(event),
        receiver_operation_id: event.receiverOperationId,
        failed_stage: event.failedStage,
      })
  }
}

function projectAuthorityActivationContext(
  event: Extract<
    V2ReceiverTraceEvent,
    { readonly name: 'authority_transition'; readonly activationId: string }
  >,
) {
  return {
    activation_id: event.activationId,
    authenticated_share_instance_id: event.authenticatedShareInstanceId,
    selection_digest: event.selectionDigest,
    observed_protocol_session_id: event.observedProtocolSessionId,
    projection_epoch: decimal(event.projectionEpoch),
    observation_revision: decimal(event.observationRevision),
    artifact_kind: snake(event.artifactKind),
    plan_kind: snake(event.planKind),
  } as const
}

function projectTransferTraceEvent(
  event: TransferTraceEvent,
): TraceEventObservationV1 {
  if (event.name === 'transfer_progress') {
    return observation(event.name, {
      discovered_files: decimal(event.discoveredFiles),
      discovered_bytes: decimal(event.discoveredBytes),
      written_bytes: decimal(event.writtenBytes),
      completed_files: decimal(event.completedFiles),
      completed_bytes: decimal(event.completedBytes),
      file_errors: decimal(event.fileErrors),
      selection_errors: decimal(event.selectionErrors),
      failed_directories: decimal(event.failedDirectories),
      content_lanes: event.contentLanes,
      discovery: event.discovery,
      partial: event.partial,
    })
  }

  switch (event.transition) {
    case 'intent_frozen':
      return observation(event.name, {
        transition: event.transition,
        artifact_kind: snake(event.artifactKind),
        layout_class: snake(event.layoutClass),
        plan_kind: snake(event.planKind),
      })
    case 'directory_admitted':
      return observation(event.name, {
        transition: event.transition,
        admitted_directory_count: decimal(event.admittedDirectoryCount),
        layout_class: snake(event.layoutClass),
      })
    case 'materialization_started':
      return observation(event.name, {
        transition: event.transition,
        plan_kind: snake(event.planKind),
      })
    case 'materialization_failed':
      return observation(event.name, {
        transition: event.transition,
        plan_kind: snake(event.planKind),
        failure_reason: snake(event.materializationFailureReason),
        completed_file_count: decimal(event.completedFileCount),
        completed_bytes: decimal(event.completedBytes),
      })
    case 'materialization_completed':
      return observation(event.name, {
        transition: event.transition,
        entry_count: decimal(event.entryCount),
        file_count: decimal(event.fileCount),
        directory_count: decimal(event.directoryCount),
        raw_bytes: decimal(event.rawBytes),
      })
    case 'tree_finalized':
      return observation(event.name, {
        transition: event.transition,
        outcome: event.outcome,
        success_count: decimal(event.successCount),
        failure_count: decimal(event.failureCount),
      })
    case 'worker_consequence_observed': {
      const source = event.failureSource
      const sourceIndex = workerConsequenceSourceIndex(source)
      return observation(event.name, {
        transition: event.transition,
        worker_family: snake(event.workerFamily),
        failure_source: snake(source.kind),
        ...(sourceIndex === undefined ? {} : { failure_source_index: sourceIndex }),
        operation_id: event.operationId,
        transfer_job_id: event.transferJobId,
        ...(event.protocolSessionId === undefined || event.protocolGeneration === undefined
          ? {}
          : {
              protocol_session_id: event.protocolSessionId,
              protocol_generation: event.protocolGeneration,
            }),
        ...(event.outputSessionId === undefined
          ? {}
          : { output_session_id: event.outputSessionId }),
      })
    }
  }
}

function workerConsequenceSourceIndex(
  source: Extract<
    TransferTraceEvent,
    { readonly transition: 'worker_consequence_observed' }
  >['failureSource'],
): number | undefined {
  if (source.kind === 'worker') return source.workerIndex
  if (source.kind === 'queue-close' || source.kind === 'queue-abort') return source.queueIndex
  return undefined
}

function projectPeerAttemptPayload(
  event: Extract<V2ConnectivityTraceEvent, { readonly eventName: 'peer_attempt' }>,
): TraceEventPayloadByNameV1['peer_attempt'] {
  const ordinals = {
    wave_ordinal: decimal(event.waveOrdinal),
    wave_attempt_ordinal: decimal(event.waveAttemptOrdinal),
    session_attempt_ordinal: decimal(event.sessionAttemptOrdinal),
  }
  switch (event.stage) {
    case 'started':
      return { stage: event.stage, ...ordinals }
    case 'negotiation-deadline-armed':
    case 'negotiation-deadline-expired':
    case 'admission-deadline-armed':
    case 'admission-deadline-expired':
      return {
        stage: snake(event.stage),
        deadline_budget_ms: event.deadlineBudgetMilliseconds,
      }
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
      return {
        stage: 'grant_requested',
        requested_lane_id: event.requestedLaneId,
      }
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
  event: Extract<V2ConnectivityTraceEvent, { readonly eventName: 'peer_recovery' }>,
): TraceEventPayloadByNameV1['peer_recovery'] {
  switch (event.stage) {
    case 'wave-started':
    case 'wave-rearmed':
      return {
        stage: snake(event.stage),
        wave_ordinal: decimal(event.waveOrdinal),
        trigger: snake(event.trigger),
      }
    case 'retry-decided':
      return {
        stage: 'retry_decided',
        wave_ordinal: decimal(event.waveOrdinal),
        decision: snake(event.decision),
        reason: snake(event.reason),
        authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
      }
    case 'backoff-scheduled':
      return {
        stage: 'backoff_scheduled',
        wave_ordinal: decimal(event.waveOrdinal),
        retry_ordinal: decimal(event.retryOrdinal),
        local_delay_ms: event.localDelayMilliseconds,
        authenticated_retry_after_ms: event.authenticatedRetryAfterMilliseconds,
        effective_delay_ms: event.effectiveDelayMilliseconds,
      }
    case 'attempt-replaced':
      return {
        stage: 'attempt_replaced',
        wave_ordinal: decimal(event.waveOrdinal),
      }
    case 'wave-quiesced':
      return {
        stage: 'wave_quiesced',
        wave_ordinal: decimal(event.waveOrdinal),
        reason: snake(event.reason),
      }
    case 'peer-detached':
      return { stage: 'peer_detached' }
    case 'session-budget-exhausted':
      return {
        stage: 'session_budget_exhausted',
        reason: snake(event.reason),
      }
    case 'path-stopped':
      return {
        stage: 'path_stopped',
        reason: snake(event.reason),
      }
    case 'session-stopped':
      return {
        stage: 'session_stopped',
        reason: snake(event.reason),
      }
  }
}

function projectProtocolFailure(failure: ProtocolFailure): ProtocolFailureV1 {
  return {
    request_kind: failure.requestKind,
    wire_scope: failure.wireScope,
    wire_code: failure.wireCode,
    retryable: failure.retryable,
    ...(failure.retryAfterMilliseconds === undefined
      ? {}
      : { retry_after_ms: failure.retryAfterMilliseconds }),
    settlement: failure.settlement.kind === 'received_authenticated'
      ? { kind: failure.settlement.kind }
      : {
          kind: failure.settlement.kind,
          admitted: failure.settlement.admitted,
          settled: failure.settlement.settled,
          outcome: failure.settlement.outcome,
        },
    correlation: requiredCorrelation(failure.correlation),
  }
}

function adaptTraceSource<Input>(
  trace: BrowserTraceSource,
  project: (event: Input) => TraceEventObservationV1,
): DomainTraceSource<Input> {
  return Object.freeze({
    get current() {
      const observer = trace.current
      if (observer === undefined) return undefined
      return (event: Input) => observer(project(event))
    },
  })
}

function observation<Name extends Exclude<keyof TraceEventPayloadByNameV1, 'incident_marker'>>(
  eventName: Name,
  payload: TraceEventPayloadByNameV1[Name],
): TraceEventObservationV1 {
  return Object.freeze({
    eventName,
    payload: Object.freeze(payload),
  }) as TraceEventObservationV1
}

function correlatedObservation<
  Name extends 'protocol_operation' | 'peer_attempt' | 'peer_recovery' | 'lane_transition',
>(
  eventName: Name,
  correlation: NonNullable<TraceEventObservationV1['correlation']>,
  payload: TraceEventPayloadByNameV1[Name],
): TraceEventObservationV1 {
  return Object.freeze({
    eventName,
    correlation,
    payload: Object.freeze(payload),
  }) as TraceEventObservationV1
}

function requiredCorrelation(
  correlation: Parameters<typeof projectCorrelationV1>[0],
): NonNullable<TraceEventObservationV1['correlation']> {
  const projected = projectCorrelationV1(correlation)
  if (projected === undefined) {
    throw new TypeError('Correlated trace event omitted its typed correlation')
  }
  return projected
}

function retainedActionTransition(
  name:
    | 'receive.inventory.action.started'
    | 'receive.inventory.action.completed'
    | 'receive.inventory.action.failed',
): 'started' | 'completed' | 'failed' {
  if (name === 'receive.inventory.action.started') return 'started'
  if (name === 'receive.inventory.action.completed') return 'completed'
  return 'failed'
}

function decimal(value: number | bigint): string {
  const candidate = typeof value === 'bigint' ? value : BigInt(value)
  if (candidate < 0n) throw new RangeError('Trace counter must be non-negative')
  return candidate.toString(10)
}

function snake<Value extends string>(value: Value): SnakeCase<Value> {
  return value.replaceAll('-', '_') as SnakeCase<Value>
}

type SnakeCase<Value extends string> =
  Value extends `${infer Head}-${infer Tail}`
    ? `${Head}_${SnakeCase<Tail>}`
    : Value
