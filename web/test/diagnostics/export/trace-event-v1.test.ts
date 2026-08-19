import { describe, expect, it } from 'vitest'

import {
  createDiagnosticBundleV1,
  projectDiagnosticsStatusV1,
} from '../../../src/diagnostics/export/diagnostic-bundle-v1'
import { isDeeplyFrozen } from '../../../src/diagnostics/export/json'
import { encodeDiagnosticBundleNdjson } from '../../../src/diagnostics/export/ndjson'
import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../../src/diagnostics/export/trace-event-v1'
import type { IncidentLink } from '../../../src/diagnostics/incident/reporter'
import type { IncidentScopeIdentity } from '../../../src/diagnostics/incident/scope'
import { BoundedTraceRecorder } from '../../../src/diagnostics/trace/recorder'
import type {
  TraceDomainEventNameV1,
  TraceEventObservationV1,
  TraceEventPayloadByNameV1,
} from '../../../src/diagnostics/trace/model'
import {
  TEST_BUNDLE_IDENTITY,
  diagnosticsHealthV1,
  traceStatus,
} from './test-support'

const SESSION_ID = 'AQAAAAAAAAAAAAAAAAAAAA'
const OPERATION_ID = 'AgAAAAAAAAAAAAAAAAAAAA'
const PATH_ID = 'AwAAAAAAAAAAAAAAAAAAAA'
const ATTEMPT_ID = 'BAAAAAAAAAAAAAAAAAAAAA'
const PRIVATE_TEXT = 'C:/private/provider-message.txt'

const CORRELATION = Object.freeze({
  protocol_session_id: SESSION_ID,
  protocol_operation_id: OPERATION_ID,
  peer_path_id: PATH_ID,
  peer_attempt_id: ATTEMPT_ID,
  lane_id: 0,
  lane_epoch: 0,
})

const VALID_OBSERVATIONS: readonly TraceEventObservationV1[] = [
  observation('join_transition', { transition: 'joined' }),
  observation('browse_transition', { transition: 'started' }),
  observation('browse_transition', { transition: 'page_loaded', entry_count: '2' }),
  observation('preview_transition', { attempt: 'media', transition: 'completed' }),
  observation('projection_transition', { transition: 'started', projection_epoch: '1' }),
  observation('projection_transition', {
    transition: 'refined',
    projection_epoch: '2',
    shape_proof: 'tree',
    discovery_state: 'discovering',
    file_count_lower_bound: '3',
    directory_count_lower_bound: '4',
    byte_count_lower_bound: '5',
    unsettled_target_count: '6',
  }),
  observation('projection_transition', {
    transition: 'proven',
    projection_epoch: '3',
    shape_proof: 'tree',
    layout_basis_class: 'complete_directory',
  }),
  observation('projection_transition', {
    transition: 'retryable_failure',
    projection_epoch: '4',
    shape_proof: 'unknown',
    retryable_discovery_reason: 'receiver_reconnecting',
  }),
  observation('projection_transition', {
    transition: 'retry_started',
    projection_epoch: '5',
    retained_shape_proof: 'single_file',
  }),
  observation('projection_transition', {
    transition: 'stale_event_dropped',
    current_projection_epoch: '6',
    stale_projection_epoch: '5',
    event_class: 'catalog_evidence',
  }),
  observation('authority_transition', {
    transition: 'offers_computed',
    projection_epoch: '7',
    shape_proof: 'tree',
    offered_artifact_kinds: ['directory_tree', 'zip_archive'],
    offered_plan_kinds: ['direct_tree', 'portable_handoff'],
    primary_artifact_kind: 'directory_tree',
  }),
  observation('authority_transition', {
    transition: 'offers_disabled',
    projection_epoch: '8',
    shape_proof: 'unknown',
    reason: 'workspace_limit_exceeded',
    hard_limit_class: 'workspace_job',
  }),
  observation('authority_transition', {
    transition: 'stale_event_dropped',
    current_projection_epoch: '9',
    stale_projection_epoch: '8',
    event_class: 'authority_result',
  }),
  observation('authority_transition', {
    transition: 'authority_acquired',
    artifact_kind: 'original_file',
    plan_kind: 'direct_atomic',
  }),
  correlated('protocol_operation', {
    transition: 'request_sent',
    request_kind: 'request_blocks',
  }),
  correlated('protocol_operation', {
    transition: 'response_received',
    request_kind: 'request_blocks',
    response_kind: 'block_fragment',
  }),
  correlated('protocol_operation', {
    transition: 'authenticated_failure',
    request_kind: 'renew_lease',
    protocol_failure: protocolFailure({ retryable: true, retry_after_ms: 250 }),
  }),
  correlated('protocol_operation', {
    transition: 'authenticated_failure',
    request_kind: 'renew_lease',
    protocol_failure: protocolFailure({ retryable: false }),
  }),
  correlated('protocol_operation', {
    transition: 'authenticated_failure',
    request_kind: 'renew_lease',
    protocol_failure: protocolFailure({
      retryable: true,
      settlement: {
        kind: 'response_send',
        admitted: true,
        settled: false,
        outcome: 'unknown',
      },
    }),
  }),
  correlated('protocol_operation', {
    transition: 'settled',
    request_kind: 'request_blocks',
    settlement: 'remote_final',
  }),
  correlated('peer_attempt', {
    stage: 'started',
    wave_ordinal: '1',
    wave_attempt_ordinal: '2',
    session_attempt_ordinal: '3',
  }),
  correlated('peer_attempt', {
    stage: 'negotiation_deadline_armed',
    deadline_budget_ms: 1_000,
  }),
  correlated('peer_attempt', {
    stage: 'admission_deadline_expired',
    deadline_budget_ms: 1_000,
  }),
  correlated('peer_attempt', {
    stage: 'offer_created',
    local_candidates_emitted: '1',
    remote_candidates_accepted: '0',
  }),
  correlated('peer_attempt', {
    stage: 'grant_requested',
    requested_lane_id: 0xffff_ffff,
  }),
  correlated('peer_attempt', { stage: 'lane_attached' }),
  correlated('peer_attempt', {
    stage: 'admission_response_settled',
    settlement: { disposition: 'accepted' },
  }),
  correlated('peer_attempt', {
    stage: 'admission_response_settled',
    settlement: { disposition: 'rejected', rejection_code: 3, retry_after_ms: 100 },
  }),
  correlated('peer_attempt', {
    stage: 'failed',
    failed_at_stage: 'offer_sent',
    failure_scope: 'attempt',
    code: 'peer_negotiation',
    retryable: true,
  }),
  correlated('peer_recovery', {
    stage: 'wave_started',
    wave_ordinal: '1',
    trigger: 'activation',
  }),
  correlated('peer_recovery', {
    stage: 'retry_decided',
    wave_ordinal: '2',
    decision: 'retry_attempt',
    reason: 'local_transient',
    authenticated_retry_after_ms: 100,
  }),
  correlated('peer_recovery', {
    stage: 'backoff_scheduled',
    wave_ordinal: '3',
    retry_ordinal: '1',
    local_delay_ms: 50,
    authenticated_retry_after_ms: 100,
    effective_delay_ms: 100,
  }),
  correlated('peer_recovery', { stage: 'attempt_replaced', wave_ordinal: '4' }),
  correlated('peer_recovery', {
    stage: 'wave_quiesced',
    wave_ordinal: '5',
    reason: 'wave_attempt_budget',
  }),
  correlated('peer_recovery', { stage: 'peer_detached' }),
  correlated('peer_recovery', {
    stage: 'session_budget_exhausted',
    reason: 'session_elapsed_budget',
  }),
  correlated('peer_recovery', { stage: 'path_stopped', reason: 'local_policy' }),
  correlated('peer_recovery', { stage: 'session_stopped', reason: 'runtime_closed' }),
  correlated('lane_transition', { transition: 'attached' }),
  correlated('lane_transition', {
    transition: 'admission_rejected',
    rejection_code: 2,
    retry_after_ms: 0,
  }),
  correlated('lane_transition', {
    transition: 'detached',
    detachment_class: 'physical_failure',
  }),
  observation('receive_transition', {
    transition: 'intent_frozen',
    artifact_kind: 'directory_tree',
    layout_class: 'directory_tree_result_root',
    plan_kind: 'direct_tree',
  }),
  observation('receive_transition', {
    transition: 'directory_admitted',
    admitted_directory_count: '3',
    layout_class: 'directory_tree_catalog_root',
  }),
  observation('receive_transition', {
    transition: 'materialization_started',
    plan_kind: 'workspace_then_publish',
  }),
  observation('receive_transition', {
    transition: 'materialization_failed',
    plan_kind: 'direct_atomic',
    failure_reason: 'output_commit_failed',
    completed_file_count: '2',
    completed_bytes: '1024',
  }),
  observation('receive_transition', {
    transition: 'materialization_completed',
    entry_count: '4',
    file_count: '2',
    directory_count: '2',
    raw_bytes: '1024',
  }),
  observation('receive_transition', {
    transition: 'tree_finalized',
    outcome: 'partial_directory',
    success_count: '3',
    failure_count: '1',
  }),
  observation('lifecycle_action_transition', {
    transition: 'completed',
    action: 'continue',
    lifecycle_state: 'receiving',
  }),
  observation('transfer_progress', {
    discovered_files: '2',
    discovered_bytes: '1024',
    written_bytes: '512',
    completed_files: '1',
    completed_bytes: '512',
    file_errors: '0',
    selection_errors: '0',
    failed_directories: '0',
    content_lanes: 2,
    discovery: 'open',
    partial: false,
  }),
  observation('output_reservation', { backend: 'file_system_access', transition: 'acquired' }),
  observation('output_write', { backend: 'origin_private', transition: 'transaction_committed' }),
  observation('checkpoint', { backend: 'origin_private', transition: 'persisted' }),
  observation('settlement', {
    backend: 'file_system_access',
    transition: 'completed',
    outcome: 'published',
  }),
  observation('publication', { backend: 'portable', transition: 'committed' }),
  observation('continuation', { backend: 'origin_private', transition: 'resumed' }),
  observation('reopen', { backend: 'file_system_access', transition: 'authorized' }),
  observation('cleanup', { backend: 'portable', transition: 'retryable_failure' }),
  observation('retained_inventory', { transition: 'load_started' }),
  observation('retained_inventory', { transition: 'load_completed', operation_count: '3' }),
  observation('retained_action', {
    transition: 'completed',
    action: 'save',
    continuation: 'save_artifact',
  }),
]

describe('closed TraceEventObservationV1 boundary', () => {
  it('detaches, freezes, sizes, records, and deterministically exports every union variant', () => {
    for (const candidate of VALID_OBSERVATIONS) {
      const snapshot = snapshotTraceEventObservationV1(candidate)
      expect(traceEventObservationNameV1(snapshot)).toBe(candidate.eventName)
      expect(traceEventObservationBytesV1(snapshot)).toBeGreaterThan(0)
      expect(snapshot).not.toBe(candidate)
      expect(isDeeplyFrozen(snapshot)).toBe(true)
    }

    let now = Date.parse('2026-08-19T01:00:00Z')
    const recorder = new BoundedTraceRecorder<
      TraceEventObservationV1,
      IncidentLink,
      IncidentScopeIdentity
    >({
      captureGeneration: 1n,
      clock: { nowMilliseconds: () => now++ },
      scheduler: {
        schedule: () => Object.freeze({ cancel: () => undefined }),
      },
      eventName: traceEventObservationNameV1,
      snapshotEvent: snapshotTraceEventObservationV1,
      eventBytes: traceEventObservationBytesV1,
      snapshotIncident: (incident) => incident,
      incidentMarkerBytes: () => 1,
      incidentScope: (incident) => incident.scope,
      sameScope: (left, right) =>
        left.scopeKind === right.scopeKind && left.scopeSequence === right.scopeSequence,
    })
    for (const candidate of VALID_OBSERVATIONS) recorder.record(candidate)
    recorder.signal({
      kind: 'incident_sealed',
      incident: Object.freeze({
        incidentSequence: 1n,
        scope: Object.freeze({ scopeKind: 'receive', scopeSequence: 1n }),
      }),
      elapsedMs: 1n,
    })
    const capture = recorder.snapshot()
    const health = diagnosticsHealthV1()
    const status = projectDiagnosticsStatusV1(traceStatus({
      state: capture.state,
      enabled: true,
      captureGeneration: capture.captureGeneration,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
      retainedEventCount: capture.retainedEventCount,
      retainedEventBytes: capture.retainedEventBytes,
      incidentMarkerCount: capture.incidentMarkerCount,
      health: capture.health,
    }), health)
    const bundle = createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents: [],
      status,
      healthAtExport: health,
      traceCapture: capture,
    })
    const first = encodeDiagnosticBundleNdjson(bundle)
    const second = encodeDiagnosticBundleNdjson(bundle)

    expect(capture.health.droppedCount).toBe(0n)
    expect(bundle.traceEvents).toHaveLength(VALID_OBSERVATIONS.length + 1)
    expect(bundle.traceEvents.at(-1)?.record.event).toBe('incident_marker')
    expect(first).toBe(second)
    expect(first.split('\n')).toHaveLength(VALID_OBSERVATIONS.length + 4)
    expect(isDeeplyFrozen(bundle)).toBe(true)
  })

  it('rejects missing, extra, and invalid discriminant fields in every event family', () => {
    const representatives = uniqueByEventName(VALID_OBSERVATIONS)
    for (const candidate of representatives) {
      const missing = clone(candidate)
      delete missing.payload[Object.keys(missing.payload)[0]!]
      expectRejected(missing)

      const extra = clone(candidate)
      extra.payload.unexpected = 'closed_vocab_escape'
      expectRejected(extra)

      let discriminant: 'transition' | 'stage' | undefined
      if (Object.hasOwn(candidate.payload, 'transition')) discriminant = 'transition'
      else if (Object.hasOwn(candidate.payload, 'stage')) discriminant = 'stage'
      if (discriminant !== undefined) {
        const invalid = clone(candidate)
        invalid.payload[discriminant] = 'invalid_variant'
        expectRejected(invalid)
      }
    }

    for (const candidate of crossVariantFields()) expectRejected(candidate)
  })

  it('rejects open text at every represented string and array-item position', () => {
    for (const candidate of VALID_OBSERVATIONS) {
      for (const path of stringLeafPaths(candidate)) {
        const mutated = clone(candidate)
        setAtPath(mutated, path, PRIVATE_TEXT)
        expectRejected(mutated)
      }
    }

    const unknownArtifact = clone(event('authority_transition', 'offers_computed'))
    ;(unknownArtifact.payload.offered_artifact_kinds as unknown[])[0] = PRIVATE_TEXT
    expectRejected(unknownArtifact)

    const unknownPlan = clone(event('authority_transition', 'offers_computed'))
    ;(unknownPlan.payload.offered_plan_kinds as unknown[])[0] = PRIVATE_TEXT
    expectRejected(unknownPlan)
  })

  it('rejects open or contradictory protocol, peer, lifecycle, and correlation shapes', () => {
    const authenticated = clone(event('protocol_operation', 'authenticated_failure'))
    ;(authenticated.payload.protocol_failure as UnknownRecord).detail = PRIVATE_TEXT
    expectRejected(authenticated)

    const missingProtocolField = clone(event('protocol_operation', 'authenticated_failure'))
    delete (missingProtocolField.payload.protocol_failure as UnknownRecord).wire_code
    expectRejected(missingProtocolField)

    const responseAsRequest = clone(event('protocol_operation', 'authenticated_failure'))
    responseAsRequest.payload.request_kind = 'operation_error'
    ;(responseAsRequest.payload.protocol_failure as UnknownRecord).request_kind = 'operation_error'
    expectRejected(responseAsRequest)

    const openSettlement = clone(event('protocol_operation', 'authenticated_failure'))
    const protocolSettlement = (openSettlement.payload.protocol_failure as UnknownRecord)
      .settlement as UnknownRecord
    protocolSettlement.message = PRIVATE_TEXT
    expectRejected(openSettlement)

    const peer = clone(event('peer_attempt', 'admission_response_settled'))
    ;(peer.payload.settlement as UnknownRecord).provider_message = PRIVATE_TEXT
    expectRejected(peer)

    const missingPeerField = clone(event('peer_attempt', 'admission_response_settled'))
    delete (missingPeerField.payload.settlement as UnknownRecord).disposition
    expectRejected(missingPeerField)

    const lifecycle = clone(event('lifecycle_action_transition', 'completed'))
    lifecycle.payload.lifecycle_state = PRIVATE_TEXT
    expectRejected(lifecycle)

    const nestedLifecycle = clone(event('lifecycle_action_transition', 'completed'))
    nestedLifecycle.payload.lifecycle_state = { state: 'receiving', message: PRIVATE_TEXT }
    expectRejected(nestedLifecycle)

    const noCorrelation = clone(event('peer_recovery', 'peer_detached'))
    delete noCorrelation.correlation
    expectRejected(noCorrelation)
    for (const correlation of [
      {},
      { protocol_operation_id: OPERATION_ID },
      { peer_attempt_id: ATTEMPT_ID },
      { lane_id: 1 },
      { lane_epoch: 1 },
      { protocol_session_id: ZERO_IDENTITY },
      { protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAB' },
    ]) {
      const invalid = clone(event('peer_recovery', 'peer_detached'))
      invalid.correlation = correlation
      expectRejected(invalid)
    }

    const mismatch = clone(event('protocol_operation', 'authenticated_failure'))
    ;((mismatch.payload.protocol_failure as UnknownRecord).correlation as UnknownRecord)
      .protocol_operation_id = 'BQAAAAAAAAAAAAAAAAAAAA'
    expectRejected(mismatch)
  })

  it('enforces canonical decimal, fixed-width number, and retry bounds', () => {
    const leadingZero = clone(event('browse_transition', 'page_loaded'))
    leadingZero.payload.entry_count = '01'
    expectRejected(leadingZero)

    const uint64Overflow = clone(event('browse_transition', 'page_loaded'))
    uint64Overflow.payload.entry_count = '18446744073709551616'
    expectRejected(uint64Overflow)

    for (const deadline of [-1, 1.5, 0x1_0000_0000]) {
      const invalid = clone(event('peer_attempt', 'negotiation_deadline_armed'))
      invalid.payload.deadline_budget_ms = deadline
      expectRejected(invalid)
    }

    const wireCodeOverflow = clone(event('protocol_operation', 'authenticated_failure'))
    ;(wireCodeOverflow.payload.protocol_failure as UnknownRecord).wire_code = 0x1_0000
    expectRejected(wireCodeOverflow)

    const retryOverflow = clone(event('protocol_operation', 'authenticated_failure'))
    ;(retryOverflow.payload.protocol_failure as UnknownRecord).retry_after_ms = 30_001
    expectRejected(retryOverflow)
  })

  it('rejects the three production probes from the adversarial review', () => {
    expectRejected({
      eventName: 'join_transition',
      payload: { transition: PRIVATE_TEXT },
    })
    expectRejected({ eventName: 'cleanup', payload: {} })
    const protocolProbe = clone(event('protocol_operation', 'authenticated_failure'))
    ;(protocolProbe.payload.protocol_failure as UnknownRecord).detail = 'secret-capability'
    expectRejected(protocolProbe)
  })
})

function observation<Name extends TraceDomainEventNameV1>(
  eventName: Name,
  payload: TraceEventPayloadByNameV1[Name],
): TraceEventObservationV1 {
  return { eventName, payload } as TraceEventObservationV1
}

function correlated<
  Name extends 'protocol_operation' | 'peer_attempt' | 'peer_recovery' | 'lane_transition',
>(
  eventName: Name,
  payload: TraceEventPayloadByNameV1[Name],
): TraceEventObservationV1 {
  return { eventName, correlation: CORRELATION, payload } as TraceEventObservationV1
}

function protocolFailure(input: {
  readonly retryable: boolean
  readonly retry_after_ms?: number
  readonly settlement?:
    | Readonly<{ kind: 'received_authenticated' }>
    | Readonly<{
        kind: 'response_send'
        admitted: boolean
        settled: boolean
        outcome: 'unknown' | 'delivered' | 'dropped'
      }>
}): Extract<
  TraceEventPayloadByNameV1['protocol_operation'],
  { readonly transition: 'authenticated_failure' }
>['protocol_failure'] {
  return {
    request_kind: 'renew_lease',
    wire_scope: 'revision',
    wire_code: 7,
    retryable: input.retryable,
    ...(input.retry_after_ms === undefined ? {} : { retry_after_ms: input.retry_after_ms }),
    settlement: input.settlement ?? { kind: 'received_authenticated' },
    correlation: CORRELATION,
  }
}

type UnknownRecord = Record<string, unknown>
type MutableObservation = {
  eventName: unknown
  correlation?: unknown
  payload: UnknownRecord
}

function clone(value: unknown): MutableObservation {
  return JSON.parse(JSON.stringify(value)) as MutableObservation
}

function expectRejected(value: unknown): void {
  expect(() => snapshotTraceEventObservationV1(
    value as TraceEventObservationV1,
  )).toThrow()
}

function uniqueByEventName(
  observations: readonly TraceEventObservationV1[],
): readonly TraceEventObservationV1[] {
  const unique = new Map<string, TraceEventObservationV1>()
  for (const observation of observations) {
    if (!unique.has(observation.eventName)) unique.set(observation.eventName, observation)
  }
  return [...unique.values()]
}

function event(eventName: string, discriminant: string): TraceEventObservationV1 {
  const found = VALID_OBSERVATIONS.find((candidate) =>
    candidate.eventName === eventName &&
    (candidate.payload as UnknownRecord).transition === discriminant ||
    candidate.eventName === eventName &&
    (candidate.payload as UnknownRecord).stage === discriminant)
  if (found === undefined) throw new Error('test trace fixture does not exist')
  return found
}

function crossVariantFields(): readonly MutableObservation[] {
  const browse = clone(event('browse_transition', 'started'))
  browse.payload.entry_count = '1'
  const projection = clone(event('projection_transition', 'started'))
  projection.payload.shape_proof = 'tree'
  const protocol = clone(event('protocol_operation', 'request_sent'))
  protocol.payload.response_kind = 'block_fragment'
  const authority = clone(event('authority_transition', 'authority_acquired'))
  authority.payload.projection_epoch = '1'
  const peer = clone(event('peer_attempt', 'started'))
  peer.payload.deadline_budget_ms = 10
  const recovery = clone(event('peer_recovery', 'peer_detached'))
  recovery.payload.reason = 'local_policy'
  const lane = clone(event('lane_transition', 'attached'))
  lane.payload.detachment_class = 'closed'
  const receive = clone(event('receive_transition', 'materialization_started'))
  receive.payload.failure_reason = 'output_write_failed'
  const inventory = clone(event('retained_inventory', 'load_started'))
  inventory.payload.operation_count = '1'
  return [browse, projection, authority, protocol, peer, recovery, lane, receive, inventory]
}

type PropertyPath = readonly (string | number)[]

function stringLeafPaths(value: unknown, prefix: PropertyPath = []): PropertyPath[] {
  if (typeof value === 'string') return [prefix]
  if (Array.isArray(value)) {
    return value.flatMap((entry, index) => stringLeafPaths(entry, [...prefix, index]))
  }
  if (typeof value !== 'object' || value === null) return []
  return Object.entries(value).flatMap(([key, entry]) =>
    stringLeafPaths(entry, [...prefix, key]))
}

function setAtPath(target: unknown, path: PropertyPath, value: unknown): void {
  let parent = target
  for (const segment of path.slice(0, -1)) {
    parent = childAt(parent, segment)
  }
  const key = path.at(-1)
  if (key === undefined) throw new TypeError('test mutation path must not be empty')
  if (Array.isArray(parent) && typeof key === 'number') {
    parent[key] = value
    return
  }
  if (typeof parent === 'object' && parent !== null && !Array.isArray(parent) &&
      typeof key === 'string') {
    ;(parent as UnknownRecord)[key] = value
    return
  }
  throw new TypeError('test mutation path contradicts its container')
}

function childAt(parent: unknown, key: string | number): unknown {
  if (Array.isArray(parent) && typeof key === 'number') return parent[key]
  if (typeof parent === 'object' && parent !== null && !Array.isArray(parent) &&
      typeof key === 'string') {
    return (parent as UnknownRecord)[key]
  }
  throw new TypeError('test mutation path contradicts its container')
}

const ZERO_IDENTITY = 'AAAAAAAAAAAAAAAAAAAAAA'
