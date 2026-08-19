import { MAX_RECORD_LIST_ITEMS } from '../incident/policy'
import type { CorrelationV1 } from './correlation-v1'

const UINT16_MAX = 0xffff
const UINT32_MAX = 0xffff_ffff
const UINT64_MAX = 0xffff_ffff_ffff_ffffn
const CANONICAL_IDENTITY = /^[A-Za-z0-9_-]{21}[AQgw]$/
const ZERO_IDENTITY = 'AAAAAAAAAAAAAAAAAAAAAA'

export type UnknownRecord = Record<string, unknown>

export function validateCorrelationV1(
  value: unknown,
  field = 'trace correlation',
): CorrelationV1 {
  const correlation = recordValue(value, field)
  exactKeys(correlation, [], [
    'protocol_session_id',
    'protocol_operation_id',
    'peer_path_id',
    'peer_attempt_id',
    'lane_id',
    'lane_epoch',
  ], field)
  if (Object.keys(correlation).length === 0) {
    throw new TypeError(`${field} must contain an identity or lane pair`)
  }
  for (const key of [
    'protocol_session_id',
    'protocol_operation_id',
    'peer_path_id',
    'peer_attempt_id',
  ] as const) {
    if (correlation[key] !== undefined) canonicalIdentity(correlation[key], `${field} ${key}`)
  }
  if (correlation.protocol_operation_id !== undefined &&
      correlation.protocol_session_id === undefined) {
    throw new TypeError(`${field} operation identity requires session identity`)
  }
  if (correlation.peer_attempt_id !== undefined && correlation.peer_path_id === undefined) {
    throw new TypeError(`${field} attempt identity requires path identity`)
  }
  const hasLaneId = correlation.lane_id !== undefined
  const hasLaneEpoch = correlation.lane_epoch !== undefined
  if (hasLaneId !== hasLaneEpoch) {
    throw new TypeError(`${field} lane identity requires id and epoch together`)
  }
  if (hasLaneId) {
    uint32(correlation.lane_id, `${field} lane id`)
    uint32(correlation.lane_epoch, `${field} lane epoch`)
  }
  return correlation as CorrelationV1
}

export function recordValue(value: unknown, field: string): UnknownRecord {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError(`${field} must be an object`)
  }
  return value as UnknownRecord
}

export function exactKeys(
  value: UnknownRecord,
  required: readonly string[],
  optional: readonly string[],
  field: string,
): void {
  for (const key of required) {
    if (!Object.hasOwn(value, key)) throw new TypeError(`${field} is missing required field ${key}`)
  }
  const allowed = new Set([...required, ...optional])
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) throw new TypeError(`${field} contains unexpected field ${key}`)
  }
}

export function member<const Value extends string>(
  value: unknown,
  values: readonly Value[],
  field: string,
): asserts value is Value {
  if (typeof value !== 'string' || !values.includes(value as Value)) {
    throw new TypeError(`${field} is outside its closed vocabulary`)
  }
}

export function memberList<const Value extends string>(
  value: unknown,
  values: readonly Value[],
  field: string,
): void {
  if (!Array.isArray(value)) throw new TypeError(`${field} must be an array`)
  if (value.length > MAX_RECORD_LIST_ITEMS) {
    throw new RangeError(`${field} exceeds its reviewed item bound`)
  }
  for (const item of value) member(item, values, `${field} item`)
}

export function booleanValue(value: unknown, field: string): asserts value is boolean {
  if (typeof value !== 'boolean') throw new TypeError(`${field} must be boolean`)
}

export function integerBetween(
  value: unknown,
  minimum: number,
  maximum: number,
  field: string,
): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < minimum ||
      value > maximum) {
    throw new RangeError(`${field} is outside its reviewed integer bound`)
  }
  return value
}

export function uint16(value: unknown, field: string): number {
  return integerBetween(value, 0, UINT16_MAX, field)
}

export function uint32(value: unknown, field: string): number {
  return integerBetween(value, 0, UINT32_MAX, field)
}

export function decimalUint64(value: unknown, field: string): string {
  if (typeof value !== 'string' || !/^(?:0|[1-9]\d*)$/.test(value)) {
    throw new TypeError(`${field} must be canonical non-negative decimal text`)
  }
  if (BigInt(value) > UINT64_MAX) throw new RangeError(`${field} exceeds uint64`)
  return value
}

export function decimalFields(
  payload: UnknownRecord,
  keys: readonly string[],
  field: string,
): void {
  for (const key of keys) decimalUint64(payload[key], `${field} ${key}`)
}

function canonicalIdentity(value: unknown, field: string): string {
  if (typeof value !== 'string' || !CANONICAL_IDENTITY.test(value) || value === ZERO_IDENTITY) {
    throw new TypeError(`${field} must be a canonical non-zero 16-byte base64url identity`)
  }
  return value
}
