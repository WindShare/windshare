import type { IncidentLink } from '../incident/reporter'
import { MAX_SAFE_STRING_UTF8_BYTES } from '../incident/policy'
import type {
  DiagnosticsHealthV1,
  BuildIdentityV1,
  IncidentRecordV1,
  RuntimeIdentityV1,
} from './incident-record-v1'
import type {
  TraceCapturedEvent,
  TraceCaptureSnapshot,
  TraceCoreStatus,
  TraceEventObservationV1,
  TraceEventPayloadByNameV1,
  TraceEventRecordV1,
} from '../trace/model'
import {
  decimalUint64,
  deepFreezeJson,
  isDeeplyFrozen,
  utcRfc3339,
} from './json'
import { snapshotTraceEventObservationV1 } from './trace-event-v1'
import type {
  CorrelatedLocalOutputOperationFailureV1,
  LocalOutputOperationFailureV1,
} from '../../output/diagnostics/local-output-failure'
import type { CorrelationV1 } from './correlation-v1'

export const DIAGNOSTIC_BUNDLE_SCHEMA_VERSION = 1 as const

export interface DiagnosticsTraceCapacityV1 {
  readonly default_trace_capture_expiry_ms: number
  readonly max_trace_event_count: number
  readonly max_trace_event_bytes: number
  readonly max_trace_total_bytes: number
  readonly max_trace_pre_failure_event_count: number
  readonly max_trace_pre_failure_bytes: number
  readonly max_trace_post_failure_event_count: number
  readonly max_trace_post_failure_bytes: number
  readonly max_trace_incident_markers: number
  readonly max_trace_post_failure_ms: number
  readonly trace_post_failure_silence_ms: number
  readonly trace_progress_sample_interval_ms: number
  readonly trace_checkpoint_coalesce_interval_ms: number
}

export interface DiagnosticsStatusV1 {
  readonly schema_version: typeof DIAGNOSTIC_BUNDLE_SCHEMA_VERSION
  readonly state: TraceCoreStatus['state']
  readonly enabled: boolean
  readonly capture_generation: string
  readonly expires_at?: string
  readonly seal_reason?: NonNullable<TraceCoreStatus['sealReason']>
  readonly capacity: DiagnosticsTraceCapacityV1
  readonly retained_event_count: string
  readonly retained_event_bytes: string
  readonly incident_marker_count: string
  readonly health: DiagnosticsHealthV1
}

export interface DiagnosticBundleHeaderV1 {
  readonly line_type: 'bundle_header'
  readonly schema_version: typeof DIAGNOSTIC_BUNDLE_SCHEMA_VERSION
  readonly build: BuildIdentityV1
  readonly runtime: RuntimeIdentityV1
  readonly runtime_run_id: string
  readonly time: string
  readonly diagnostics_health_at_export: DiagnosticsHealthV1
}

export interface DiagnosticBundleIncidentLineV1 {
  readonly line_type: 'incident'
  readonly record: IncidentRecordV1
}

export interface DiagnosticBundleLocalOutputFailureLineV1 {
  readonly line_type: 'local_output_operation_failure'
  readonly owning_incident: Readonly<{
    readonly incident_sequence: string
    readonly scope: IncidentRecordV1['payload']['scope']
  }>
  readonly correlation: CorrelationV1
  readonly record: LocalOutputOperationFailureV1
}

export interface DiagnosticBundleTraceCaptureLineV1 {
  readonly line_type: 'trace_capture'
  readonly status: DiagnosticsStatusV1
}

export interface DiagnosticBundleTraceEventLineV1 {
  readonly line_type: 'trace_event'
  readonly record: TraceEventRecordV1
}

export interface DiagnosticBundleV1 {
  readonly header: DiagnosticBundleHeaderV1
  readonly incidents: readonly DiagnosticBundleIncidentLineV1[]
  readonly localOutputFailures: readonly DiagnosticBundleLocalOutputFailureLineV1[]
  readonly traceCapture?: DiagnosticBundleTraceCaptureLineV1
  readonly traceEvents: readonly DiagnosticBundleTraceEventLineV1[]
}

export interface DiagnosticBundleIdentityV1 {
  readonly build: BuildIdentityV1
  readonly runtime: RuntimeIdentityV1
  readonly runtimeRunId: string
}

export interface DiagnosticBundleSnapshotInput {
  readonly identity: DiagnosticBundleIdentityV1
  readonly time: string
  readonly incidents: readonly IncidentRecordV1[]
  readonly localOutputFailures?: readonly CorrelatedLocalOutputOperationFailureV1[]
  readonly status: DiagnosticsStatusV1
  readonly healthAtExport: DiagnosticsHealthV1
  readonly traceCapture?: TraceCaptureSnapshot<TraceEventObservationV1, IncidentLink>
}

export function projectDiagnosticsStatusV1(
  status: TraceCoreStatus,
  health: DiagnosticsHealthV1,
): DiagnosticsStatusV1 {
  if (status.enabled !== isRecording(status.state)) {
    throw new TypeError('trace enabled flag contradicts capture state')
  }
  if (status.state === 'sealed' && status.sealReason === undefined) {
    throw new TypeError('sealed trace status requires a seal reason')
  }
  if (status.state !== 'sealed' && status.sealReason !== undefined) {
    throw new TypeError('only a sealed trace status may carry a seal reason')
  }
  if (status.enabled !== (status.expiresAtMilliseconds !== undefined)) {
    throw new TypeError('only an enabled trace status may carry expiry')
  }

  return deepFreezeJson({
    schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
    state: status.state,
    enabled: status.enabled,
    capture_generation: decimalUint64(
      status.captureGeneration,
      'trace capture generation',
    ),
    ...(status.expiresAtMilliseconds === undefined
      ? {}
      : { expires_at: utcTimeFromMilliseconds(status.expiresAtMilliseconds) }),
    ...(status.sealReason === undefined ? {} : { seal_reason: status.sealReason }),
    capacity: projectCapacity(status.capacity),
    retained_event_count: decimalUint64(
      status.retainedEventCount,
      'retained trace event count',
    ),
    retained_event_bytes: decimalUint64(
      status.retainedEventBytes,
      'retained trace event bytes',
    ),
    incident_marker_count: decimalUint64(
      status.incidentMarkerCount,
      'trace incident marker count',
    ),
    health: copyHealth(health),
  })
}

export function createDiagnosticBundleV1(
  input: DiagnosticBundleSnapshotInput,
): DiagnosticBundleV1 {
  const identity = copyIdentity(input.identity)
  const healthAtExport = copyHealth(input.healthAtExport)
  const status = copyStatus(input.status)
  if (!sameHealth(status.health, healthAtExport)) {
    throw new TypeError('trace status and export header must share one health cut')
  }

  const incidentRecords = [...input.incidents]
    .map((record) => validateIncident(record, identity))
    .sort((left, right) => compareDecimal(left.sequence, right.sequence))
  requireUniqueDecimalSequences(incidentRecords.map((record) => record.sequence), 'incident')
  const incidents = Object.freeze(incidentRecords.map((record) =>
    deepFreezeJson({ line_type: 'incident' as const, record })))
  const localOutputFailures = Object.freeze(
    [...(input.localOutputFailures ?? [])]
      .map((record) => projectLocalOutputFailureLine(record, incidentRecords))
      .filter((line): line is DiagnosticBundleLocalOutputFailureLineV1 => line !== undefined)
      .sort(compareLocalOutputFailures),
  )

  const capture = input.traceCapture
  if ((capture === undefined) !== (status.state === 'idle')) {
    throw new TypeError('trace capture presence contradicts exported status')
  }
  if (capture !== undefined) validateCaptureCut(capture, status)

  const capturedEvents = (capture?.events ?? [])
    .slice()
    .sort((left, right) => compareBigInt(left.sequence, right.sequence))
  requireUniqueBigIntSequences(
    capturedEvents.map((captured) => captured.sequence),
    'trace event',
  )
  const traceEvents = Object.freeze(capturedEvents.map((captured) => deepFreezeJson({
    line_type: 'trace_event' as const,
    record: projectCapturedEvent(captured, identity.runtimeRunId),
  })))
  const traceCapture = capture === undefined
    ? undefined
    : deepFreezeJson({ line_type: 'trace_capture' as const, status })

  return Object.freeze({
    header: deepFreezeJson({
      line_type: 'bundle_header' as const,
      schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
      build: identity.build,
      runtime: identity.runtime,
      runtime_run_id: identity.runtimeRunId,
      time: utcRfc3339(input.time, 'diagnostic bundle time'),
      diagnostics_health_at_export: healthAtExport,
    }),
    incidents,
    localOutputFailures,
    ...(traceCapture === undefined ? {} : { traceCapture }),
    traceEvents,
  })
}

function validateLocalOutputFailure(
  projection: CorrelatedLocalOutputOperationFailureV1,
): void {
  const { failure: record, correlation, owningScope } = projection
  if (record.schemaVersion !== 1 ||
      record.recordKind !== 'local_output_operation_failure' ||
      record.stageFailure.schemaVersion !== 1 ||
      !Number.isSafeInteger(record.stageFailure.sequence) ||
      record.stageFailure.sequence <= 0 ||
      owningScope.scopeKind.length === 0 ||
      BigInt(canonicalDecimal(owningScope.scopeSequence, 'local output incident scope')) <= 0n ||
      !isDeeplyFrozen(projection)) {
    throw new TypeError('local output failure is not a frozen bounded stage projection')
  }
  const keys = Object.keys(correlation)
  if (!keys.every((key) => [
    'protocol_session_id',
    'receive_operation_id',
    'transfer_job_id',
    'output_session_id',
    'protocol_generation',
  ].includes(key)) ||
      correlation.receive_operation_id !== record.stageFailure.correlation.operationId ||
      correlation.output_session_id !== record.stageFailure.correlation.outputSessionId ||
      !nonEmptyText(correlation.transfer_job_id) ||
      !nonEmptyText(correlation.receive_operation_id) ||
      !nonEmptyText(correlation.output_session_id) ||
      ((correlation.protocol_session_id === undefined) !==
        (correlation.protocol_generation === undefined)) ||
      (correlation.protocol_session_id !== undefined &&
        !/^[A-Za-z0-9_-]{22}$/.test(correlation.protocol_session_id)) ||
      (correlation.protocol_generation !== undefined &&
        (!Number.isSafeInteger(correlation.protocol_generation) ||
          correlation.protocol_generation <= 0 ||
          correlation.protocol_generation > 0xffff_ffff))) {
    throw new TypeError('local output failure correlation is invalid')
  }
}

function projectLocalOutputFailureLine(
  projection: CorrelatedLocalOutputOperationFailureV1,
  incidents: readonly IncidentRecordV1[],
): DiagnosticBundleLocalOutputFailureLineV1 | undefined {
  validateLocalOutputFailure(projection)
  const owner = incidents.filter((incident) =>
    incident.payload.root_incident_sequence === undefined &&
    incident.payload.scope.scope_kind === projection.owningScope.scopeKind &&
    incident.payload.scope.scope_sequence === projection.owningScope.scopeSequence &&
    incidentContainsNativeOutputFailure(incident))
  if (owner.length !== 1) return undefined
  const incident = owner[0]!
  return deepFreezeJson({
    line_type: 'local_output_operation_failure' as const,
    owning_incident: {
      incident_sequence: incident.sequence,
      scope: incident.payload.scope,
    },
    correlation: projection.correlation,
    record: projection.failure,
  })
}

function compareLocalOutputFailures(
  left: DiagnosticBundleLocalOutputFailureLineV1,
  right: DiagnosticBundleLocalOutputFailureLineV1,
): number {
  return compareDecimal(
    left.owning_incident.incident_sequence,
    right.owning_incident.incident_sequence,
  ) ||
    (left.correlation.protocol_generation ?? 0) -
      (right.correlation.protocol_generation ?? 0) ||
    compareText(left.correlation.receive_operation_id!, right.correlation.receive_operation_id!) ||
    compareText(left.correlation.transfer_job_id!, right.correlation.transfer_job_id!) ||
    compareText(left.correlation.output_session_id!, right.correlation.output_session_id!) ||
    compareText(
      left.record.stageFailure.correlation.artifactId,
      right.record.stageFailure.correlation.artifactId,
    ) ||
    left.record.stageFailure.sequence - right.record.stageFailure.sequence
}

function incidentContainsNativeOutputFailure(incident: IncidentRecordV1): boolean {
  return incident.payload.trigger.kind === 'native_output_failure' ||
    [...incident.payload.contributors, ...incident.payload.consequences]
      .some((bucket) => bucket.representative.kind === 'native_output_failure')
}

function projectCapturedEvent(
  captured: TraceCapturedEvent<TraceEventObservationV1, IncidentLink>,
  runtimeRunId: string,
): TraceEventRecordV1 {
  const sequence = decimalUint64(captured.sequence, 'trace event sequence')
  const elapsedMs = decimalUint64(captured.elapsedMs, 'trace event elapsed milliseconds')
  const time = utcTimeFromMilliseconds(captured.observedAtMilliseconds)
  if (captured.value.kind === 'incident_marker') {
    const incident = captured.value.incident
    if (incident.rootIncidentSequence !== undefined &&
        incident.rootIncidentSequence >= incident.incidentSequence) {
      throw new RangeError('trace root incident marker must precede its linked incident')
    }
    const payload: TraceEventPayloadByNameV1['incident_marker'] = {
      incident_sequence: decimalUint64(
        incident.incidentSequence,
        'trace incident marker sequence',
      ),
      ...(incident.rootIncidentSequence === undefined
        ? {}
        : {
            root_incident_sequence: decimalUint64(
              incident.rootIncidentSequence,
              'trace root incident marker sequence',
            ),
          }),
      scope: {
        scope_kind: incident.scope.scopeKind,
        scope_sequence: decimalUint64(
          incident.scope.scopeSequence,
          'trace incident marker scope sequence',
        ),
      },
    }
    return deepFreezeJson({
      schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
      sequence,
      time,
      elapsed_ms: elapsedMs,
      level: 'debug',
      event: 'incident_marker',
      runtime_run_id: runtimeRunId,
      payload,
    })
  }

  const observation = snapshotTraceEventObservationV1(captured.value.event)
  if (captured.value.eventName !== observation.eventName) {
    throw new TypeError('captured trace event name contradicts its immutable observation')
  }
  return deepFreezeJson({
    schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
    sequence,
    time,
    elapsed_ms: elapsedMs,
    level: 'debug',
    event: observation.eventName,
    runtime_run_id: runtimeRunId,
    ...(observation.correlation === undefined
      ? {}
      : { correlation: observation.correlation }),
    payload: observation.payload,
  }) as TraceEventRecordV1
}

function validateCaptureCut(
  capture: TraceCaptureSnapshot<TraceEventObservationV1, IncidentLink>,
  status: DiagnosticsStatusV1,
): void {
  if (
    status.state !== capture.state ||
    status.capture_generation !== capture.captureGeneration.toString(10) ||
    status.retained_event_count !== capture.retainedEventCount.toString(10) ||
    status.retained_event_bytes !== capture.retainedEventBytes.toString(10) ||
    status.incident_marker_count !== capture.incidentMarkerCount.toString(10) ||
    status.seal_reason !== capture.sealReason
  ) {
    throw new TypeError('trace status and capture are not from the same cut')
  }
  if (capture.events.length !== Number(capture.retainedEventCount)) {
    throw new TypeError('trace capture retained count contradicts its events')
  }
  const retainedBytes = capture.events.reduce((total, event) => {
    if (!Number.isSafeInteger(event.encodedBytes) || event.encodedBytes <= 0) {
      throw new RangeError('captured trace event bytes must be positive safe integers')
    }
    if (event.encodedBytes > status.capacity.max_trace_event_bytes) {
      throw new RangeError('captured trace event exceeds the exported per-event capacity')
    }
    return total + BigInt(event.encodedBytes)
  }, 0n)
  const markerCount = BigInt(capture.events.filter(
    (event) => event.value.kind === 'incident_marker',
  ).length)
  if (retainedBytes !== capture.retainedEventBytes || markerCount !== capture.incidentMarkerCount) {
    throw new TypeError('trace capture capacity totals contradict its events')
  }
  if (
    status.health.trace_dropped_count !== capture.health.droppedCount.toString(10) ||
    status.health.trace_overwritten_count !== capture.health.overwrittenCount.toString(10) ||
    status.health.trace_sampled_count !== capture.health.sampledCount.toString(10) ||
    status.health.trace_coalesced_count !== capture.health.coalescedCount.toString(10)
  ) {
    throw new TypeError('trace capture and export health are not from the same cut')
  }
}

function validateIncident(
  record: IncidentRecordV1,
  identity: DiagnosticBundleIdentityV1,
): IncidentRecordV1 {
  if (
    record.schema_version !== DIAGNOSTIC_BUNDLE_SCHEMA_VERSION ||
    record.event !== 'failure_incident' ||
    record.runtime_run_id !== identity.runtimeRunId ||
    !isDeeplyFrozen(record)
  ) {
    throw new TypeError('diagnostic bundle accepts only immutable incidents from its runtime')
  }
  canonicalDecimal(record.sequence, 'incident sequence')
  if (
    JSON.stringify(record.payload.build) !== JSON.stringify(identity.build) ||
    JSON.stringify(record.payload.runtime) !== JSON.stringify(identity.runtime)
  ) {
    throw new TypeError('incident build/runtime identity contradicts bundle identity')
  }
  return record
}

function copyIdentity(identity: DiagnosticBundleIdentityV1): DiagnosticBundleIdentityV1 {
  if (!/^[A-Za-z0-9_-]{22}$/.test(identity.runtimeRunId) ||
      identity.runtimeRunId === 'AAAAAAAAAAAAAAAAAAAAAA') {
    throw new TypeError('diagnostic runtime run identity must be non-zero base64url')
  }
  if (
    identity.build.application !== 'windshare_web' ||
    typeof identity.build.version !== 'string' ||
    identity.build.version.length === 0 ||
    new TextEncoder().encode(identity.build.version).byteLength > MAX_SAFE_STRING_UTF8_BYTES ||
    (identity.build.mode !== 'development' &&
      identity.build.mode !== 'production' &&
      identity.build.mode !== 'test')
  ) {
    throw new TypeError('diagnostic build identity is invalid')
  }
  if (
    identity.build.revision !== undefined &&
    !/^[0-9a-f]{7,64}$/.test(identity.build.revision)
  ) {
    throw new TypeError('diagnostic build revision is invalid')
  }
  if (
    identity.runtime.kind !== 'browser' ||
    typeof identity.runtime.secure_context !== 'boolean'
  ) {
    throw new TypeError('diagnostic runtime identity is invalid')
  }
  return deepFreezeJson({
    build: {
      application: 'windshare_web' as const,
      version: identity.build.version,
      ...(identity.build.revision === undefined
        ? {}
        : { revision: identity.build.revision }),
      mode: identity.build.mode,
    },
    runtime: {
      kind: 'browser' as const,
      secure_context: identity.runtime.secure_context,
    },
    runtimeRunId: identity.runtimeRunId,
  })
}

function copyStatus(status: DiagnosticsStatusV1): DiagnosticsStatusV1 {
  if (status.schema_version !== DIAGNOSTIC_BUNDLE_SCHEMA_VERSION) {
    throw new TypeError('diagnostics status schema version is invalid')
  }
  if (status.enabled !== isRecording(status.state) ||
      status.enabled !== (status.expires_at !== undefined)) {
    throw new TypeError('diagnostics status lifecycle fields contradict its state')
  }
  if ((status.state === 'sealed') !== (status.seal_reason !== undefined)) {
    throw new TypeError('diagnostics status seal reason contradicts its state')
  }
  const copy = deepFreezeJson({
    schema_version: status.schema_version,
    state: status.state,
    enabled: status.enabled,
    capture_generation: canonicalDecimal(
      status.capture_generation,
      'status capture generation',
    ),
    ...(status.expires_at === undefined
      ? {}
      : { expires_at: utcRfc3339(status.expires_at, 'status expiry') }),
    ...(status.seal_reason === undefined ? {} : { seal_reason: status.seal_reason }),
    capacity: projectCapacity({
      captureExpiryMs: status.capacity.default_trace_capture_expiry_ms,
      maxEventCount: status.capacity.max_trace_event_count,
      maxEventBytes: status.capacity.max_trace_event_bytes,
      maxTotalBytes: status.capacity.max_trace_total_bytes,
      maxPreFailureEventCount: status.capacity.max_trace_pre_failure_event_count,
      maxPreFailureBytes: status.capacity.max_trace_pre_failure_bytes,
      maxPostFailureEventCount: status.capacity.max_trace_post_failure_event_count,
      maxPostFailureBytes: status.capacity.max_trace_post_failure_bytes,
      maxIncidentMarkers: status.capacity.max_trace_incident_markers,
      maxPostFailureMs: status.capacity.max_trace_post_failure_ms,
      postFailureSilenceMs: status.capacity.trace_post_failure_silence_ms,
      progressSampleIntervalMs: status.capacity.trace_progress_sample_interval_ms,
      checkpointCoalesceIntervalMs: status.capacity.trace_checkpoint_coalesce_interval_ms,
    }),
    retained_event_count: canonicalDecimal(
      status.retained_event_count,
      'status retained event count',
    ),
    retained_event_bytes: canonicalDecimal(
      status.retained_event_bytes,
      'status retained event bytes',
    ),
    incident_marker_count: canonicalDecimal(
      status.incident_marker_count,
      'status incident marker count',
    ),
    health: copyHealth(status.health),
  })
  validateStatusCapacity(copy)
  return copy
}

function validateStatusCapacity(status: DiagnosticsStatusV1): void {
  const retainedEvents = BigInt(status.retained_event_count)
  const retainedBytes = BigInt(status.retained_event_bytes)
  const incidentMarkers = BigInt(status.incident_marker_count)
  if (
    retainedEvents > BigInt(status.capacity.max_trace_event_count) ||
    retainedBytes > BigInt(status.capacity.max_trace_total_bytes) ||
    incidentMarkers > BigInt(status.capacity.max_trace_incident_markers) ||
    incidentMarkers > retainedEvents
  ) {
    throw new RangeError('diagnostics status exceeds its named trace capacity')
  }
  if (status.state === 'idle' &&
      (retainedEvents !== 0n || retainedBytes !== 0n || incidentMarkers !== 0n)) {
    throw new TypeError('idle diagnostics status cannot retain trace data')
  }
}

function projectCapacity(capacity: TraceCoreStatus['capacity']): DiagnosticsTraceCapacityV1 {
  return Object.freeze({
    default_trace_capture_expiry_ms: positiveSafeInteger(capacity.captureExpiryMs, 'trace expiry'),
    max_trace_event_count: positiveSafeInteger(capacity.maxEventCount, 'trace event count'),
    max_trace_event_bytes: positiveSafeInteger(capacity.maxEventBytes, 'trace event bytes'),
    max_trace_total_bytes: positiveSafeInteger(capacity.maxTotalBytes, 'trace total bytes'),
    max_trace_pre_failure_event_count: positiveSafeInteger(
      capacity.maxPreFailureEventCount,
      'trace pre-failure event count',
    ),
    max_trace_pre_failure_bytes: positiveSafeInteger(
      capacity.maxPreFailureBytes,
      'trace pre-failure bytes',
    ),
    max_trace_post_failure_event_count: positiveSafeInteger(
      capacity.maxPostFailureEventCount,
      'trace post-failure event count',
    ),
    max_trace_post_failure_bytes: positiveSafeInteger(
      capacity.maxPostFailureBytes,
      'trace post-failure bytes',
    ),
    max_trace_incident_markers: positiveSafeInteger(
      capacity.maxIncidentMarkers,
      'trace incident marker count',
    ),
    max_trace_post_failure_ms: positiveSafeInteger(
      capacity.maxPostFailureMs,
      'trace post-failure time',
    ),
    trace_post_failure_silence_ms: positiveSafeInteger(
      capacity.postFailureSilenceMs,
      'trace silence time',
    ),
    trace_progress_sample_interval_ms: positiveSafeInteger(
      capacity.progressSampleIntervalMs,
      'trace progress sample time',
    ),
    trace_checkpoint_coalesce_interval_ms: positiveSafeInteger(
      capacity.checkpointCoalesceIntervalMs,
      'trace checkpoint coalesce time',
    ),
  })
}

function copyHealth(health: DiagnosticsHealthV1): DiagnosticsHealthV1 {
  return Object.freeze({
    fact_overflow_count: canonicalDecimal(health.fact_overflow_count, 'fact overflow health'),
    incident_history_eviction_count: canonicalDecimal(
      health.incident_history_eviction_count,
      'incident history eviction health',
    ),
    console_suppression_count: canonicalDecimal(
      health.console_suppression_count,
      'console suppression health',
    ),
    late_link_eviction_count: canonicalDecimal(
      health.late_link_eviction_count,
      'late link eviction health',
    ),
    trace_dropped_count: canonicalDecimal(health.trace_dropped_count, 'trace dropped health'),
    trace_overwritten_count: canonicalDecimal(
      health.trace_overwritten_count,
      'trace overwritten health',
    ),
    trace_sampled_count: canonicalDecimal(health.trace_sampled_count, 'trace sampled health'),
    trace_coalesced_count: canonicalDecimal(
      health.trace_coalesced_count,
      'trace coalesced health',
    ),
  })
}

function canonicalDecimal(value: string, field: string): string {
  if (!/^(?:0|[1-9]\d*)$/.test(value)) {
    throw new TypeError(`${field} must be canonical non-negative decimal text`)
  }
  if (BigInt(value) > 0xffff_ffff_ffff_ffffn) {
    throw new RangeError(`${field} exceeds uint64`)
  }
  return value
}

function positiveSafeInteger(value: number, field: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${field} must be a positive safe integer`)
  }
  return value
}

function utcTimeFromMilliseconds(milliseconds: number): string {
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 0) {
    throw new RangeError('diagnostic wall time must be non-negative safe integer milliseconds')
  }
  try {
    return new Date(milliseconds).toISOString()
  } catch {
    throw new RangeError('diagnostic wall time is outside the UTC timestamp domain')
  }
}

function compareDecimal(left: string, right: string): number {
  return compareBigInt(
    BigInt(canonicalDecimal(left, 'sequence')),
    BigInt(canonicalDecimal(right, 'sequence')),
  )
}

function compareBigInt(left: bigint, right: bigint): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

function compareText(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

function nonEmptyText(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function requireUniqueDecimalSequences(
  sequences: readonly string[],
  field: string,
): void {
  requireUniqueBigIntSequences(sequences.map((sequence) => BigInt(sequence)), field)
}

function requireUniqueBigIntSequences(
  sequences: readonly bigint[],
  field: string,
): void {
  for (let index = 1; index < sequences.length; index += 1) {
    if (sequences[index] === sequences[index - 1]) {
      throw new TypeError(`${field} sequences must be unique`)
    }
  }
}

function isRecording(state: TraceCoreStatus['state']): boolean {
  return state === 'recording_pre_failure' || state === 'recording_post_failure'
}

function sameHealth(left: DiagnosticsHealthV1, right: DiagnosticsHealthV1): boolean {
  return JSON.stringify(left) === JSON.stringify(right)
}
