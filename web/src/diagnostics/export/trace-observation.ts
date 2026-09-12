import { projectCorrelationV1 } from './correlation-v1'
import type { TraceEventObservationV2, TraceEventPayloadByNameV2 } from '../trace/model'

export function observation<Name extends Exclude<keyof TraceEventPayloadByNameV2, 'incident_marker'>>(
  eventName: Name,
  payload: TraceEventPayloadByNameV2[Name],
): TraceEventObservationV2 {
  return Object.freeze({
    eventName,
    payload: Object.freeze(payload),
  }) as TraceEventObservationV2
}

export function correlatedObservation<
  Name extends 'lease_retirement' | 'request_scheduling' | 'content_scheduling' | 'protocol_operation' | 'operation_recovery' | 'peer_attempt' | 'peer_recovery' | 'lane_transition',
>(
  eventName: Name,
  correlation: NonNullable<TraceEventObservationV2['correlation']>,
  payload: TraceEventPayloadByNameV2[Name],
): TraceEventObservationV2 {
  return Object.freeze({
    eventName,
    correlation,
    payload: Object.freeze(payload),
  }) as TraceEventObservationV2
}

export function requiredCorrelation(
  correlation: Parameters<typeof projectCorrelationV1>[0],
): NonNullable<TraceEventObservationV2['correlation']> {
  const projected = projectCorrelationV1(correlation)
  if (projected === undefined) {
    throw new TypeError('Correlated trace event omitted its typed correlation')
  }
  return projected
}

export function decimal(value: number | bigint): string {
  const candidate = typeof value === 'bigint' ? value : BigInt(value)
  if (candidate < 0n) throw new RangeError('Trace counter must be non-negative')
  return candidate.toString(10)
}

export function snake<Value extends string>(value: Value): SnakeCase<Value> {
  return value.replaceAll('-', '_') as SnakeCase<Value>
}

type SnakeCase<Value extends string> =
  Value extends `${infer Head}-${infer Tail}`
    ? `${Head}_${SnakeCase<Tail>}`
    : Value
