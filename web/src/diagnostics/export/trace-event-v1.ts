import {
  TRACE_EVENT_NAMES_V1,
  type TraceDomainEventNameV1,
  type TraceEventObservationV1,
} from '../trace/model'
import { deepFreezeJson } from './json'
import {
  validateCorrelationV1,
  validateTraceEventPayloadV1,
} from './trace-event-payload-v1'

const CORRELATED_TRACE_EVENT_NAMES: ReadonlySet<TraceDomainEventNameV1> = new Set([
  'protocol_operation',
  'peer_attempt',
  'peer_recovery',
  'lane_transition',
])

/**
 * Detachment alone is not a privacy boundary: the parsed value is still unknown
 * until the event-specific validator proves the complete closed payload variant.
 */
export function snapshotTraceEventObservationV1(
  observation: TraceEventObservationV1,
): TraceEventObservationV1 {
  const encoded = JSON.stringify(observation)
  if (encoded === undefined) throw new TypeError('trace observation is not standard JSON')
  const detached: unknown = JSON.parse(encoded)
  const record = recordValue(detached, 'trace observation')
  exactKeys(
    record,
    record.correlation === undefined
      ? ['eventName', 'payload']
      : ['eventName', 'correlation', 'payload'],
    'trace observation',
  )
  if (!isDomainEventName(record.eventName)) {
    throw new TypeError('trace observation event name is invalid')
  }
  validateTraceEventPayloadV1(record.eventName, record.payload)
  const correlation = record.correlation === undefined
    ? undefined
    : validateCorrelationV1(record.correlation)
  if (CORRELATED_TRACE_EVENT_NAMES.has(record.eventName) && correlation === undefined) {
    throw new TypeError(`${record.eventName} requires correlation`)
  }
  if (record.eventName === 'protocol_operation' &&
      'transition' in record.payload &&
      record.payload.transition === 'authenticated_failure' &&
      'protocol_failure' in record.payload &&
      correlation !== undefined &&
      !sameCorrelation(correlation, record.payload.protocol_failure.correlation)) {
    throw new TypeError('protocol failure correlation contradicts its trace observation')
  }
  return deepFreezeJson(detached) as TraceEventObservationV1
}

export function traceEventObservationNameV1(
  observation: TraceEventObservationV1,
): TraceDomainEventNameV1 {
  if (!isDomainEventName(observation.eventName)) {
    throw new TypeError('trace observation event name is invalid')
  }
  return observation.eventName
}

export function traceEventObservationBytesV1(
  observation: TraceEventObservationV1,
): number {
  const encoded = JSON.stringify(observation)
  if (encoded === undefined) throw new TypeError('trace observation is not standard JSON')
  return new TextEncoder().encode(encoded).byteLength
}

function recordValue(value: unknown, field: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError(`${field} must be an object`)
  }
  return value as Record<string, unknown>
}

function exactKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  field: string,
): void {
  for (const key of required) {
    if (!Object.hasOwn(value, key)) throw new TypeError(`${field} is missing required field ${key}`)
  }
  const allowed = new Set(required)
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) throw new TypeError(`${field} contains unexpected field ${key}`)
  }
}

function isDomainEventName(value: unknown): value is TraceDomainEventNameV1 {
  return typeof value === 'string' &&
    value !== 'incident_marker' &&
    TRACE_EVENT_NAMES_V1.includes(value as (typeof TRACE_EVENT_NAMES_V1)[number])
}

function sameCorrelation(
  left: NonNullable<TraceEventObservationV1['correlation']>,
  right: NonNullable<TraceEventObservationV1['correlation']>,
): boolean {
  return left.protocol_session_id === right.protocol_session_id &&
    left.protocol_operation_id === right.protocol_operation_id &&
    left.peer_path_id === right.peer_path_id &&
    left.peer_attempt_id === right.peer_attempt_id &&
    left.lane_id === right.lane_id &&
    left.lane_epoch === right.lane_epoch
}
