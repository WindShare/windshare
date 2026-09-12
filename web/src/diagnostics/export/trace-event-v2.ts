import {
  TRACE_EVENT_NAMES_V2,
  type TraceDomainEventNameV2,
  type TraceEventObservationV2,
} from '../trace/model'
import { deepFreezeJson } from './json'
import {
  validateCorrelationV1,
  validateTraceEventPayloadV2,
} from './trace-event-payload-v2'

const CORRELATED_TRACE_EVENT_NAMES: ReadonlySet<TraceDomainEventNameV2> = new Set([
  'protocol_operation',
  'operation_recovery',
  'content_scheduling',
  'request_scheduling',
  'peer_attempt',
  'peer_recovery',
  'lane_transition',
])

/**
 * Detachment alone is not a privacy boundary: the parsed value is still unknown
 * until the event-specific validator proves the complete closed payload variant.
 */
export function snapshotTraceEventObservationV2(
  observation: TraceEventObservationV2,
): TraceEventObservationV2 {
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
  validateTraceEventPayloadV2(record.eventName, record.payload)
  const correlation = record.correlation === undefined
    ? undefined
    : validateCorrelationV1(record.correlation)
  if (CORRELATED_TRACE_EVENT_NAMES.has(record.eventName) && correlation === undefined) {
    throw new TypeError(`${record.eventName} requires correlation`)
  }
  if (record.eventName === 'protocol_operation' &&
      'protocol_error' in record.payload &&
      (correlation?.protocol_session_id === undefined || correlation.protocol_operation_id === undefined)) {
    throw new TypeError('received protocol error requires session and operation correlation')
  }
  return deepFreezeJson(detached) as TraceEventObservationV2
}

export function traceEventObservationNameV2(
  observation: TraceEventObservationV2,
): TraceDomainEventNameV2 {
  if (!isDomainEventName(observation.eventName)) {
    throw new TypeError('trace observation event name is invalid')
  }
  return observation.eventName
}

export function traceEventObservationBytesV2(
  observation: TraceEventObservationV2,
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

function isDomainEventName(value: unknown): value is TraceDomainEventNameV2 {
  return typeof value === 'string' &&
    value !== 'incident_marker' &&
    TRACE_EVENT_NAMES_V2.includes(value as (typeof TRACE_EVENT_NAMES_V2)[number])
}
