import { decode, encode, rfc8949EncodeOptions, type DecodeOptions } from 'cborg'

import { equalBytes } from '../crypto/bytes'
import { isWellFormedUnicode } from './text'

const STRICT_DECODE_OPTIONS: DecodeOptions = Object.freeze({
  allowBigInt: true,
  allowIndefinite: false,
  allowInfinity: false,
  allowNaN: false,
  allowUndefined: false,
  rejectDuplicateMapKeys: true,
  strict: true,
  useMaps: true,
})

export const MAXIMUM_CBOR_DEPTH = 16
export const MAXIMUM_CBOR_ARRAY_ITEMS = 2_048
export const MAXIMUM_CBOR_MAP_FIELDS = 256

export class V2CborError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2CborError'
  }
}

export function encodeCanonicalCbor(value: unknown): Uint8Array<ArrayBuffer> {
  validateValue(value, 0)
  try {
    return encode(value, rfc8949EncodeOptions)
  } catch (cause) {
    throw new V2CborError('Unable to encode deterministic CBOR', { cause })
  }
}

export function decodeCanonicalCbor(
  encoded: Uint8Array,
  maximumBytes: number,
  label: string,
): unknown {
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes <= 0) {
    throw new TypeError('CBOR byte limit must be a positive safe integer')
  }
  if (encoded.byteLength === 0 || encoded.byteLength > maximumBytes) {
    throw new V2CborError(`${label} exceeds its authenticated byte boundary`)
  }
  let decoded: unknown
  try {
    decoded = decode(encoded, STRICT_DECODE_OPTIONS) as unknown
  } catch (cause) {
    throw new V2CborError(`${label} is not strict CBOR`, { cause })
  }
  validateValue(decoded, 0)
  let canonical: Uint8Array
  try {
    canonical = encode(decoded, rfc8949EncodeOptions)
  } catch (cause) {
    throw new V2CborError(`${label} cannot be deterministically encoded`, { cause })
  }
  if (!equalBytes(canonical, encoded)) {
    throw new V2CborError(`${label} is not deterministic CBOR`)
  }
  return decoded
}

export function requireNumericMap(
  value: unknown,
  keys: readonly number[],
  label: string,
): ReadonlyMap<number, unknown> {
  if (!(value instanceof Map) || value.size !== keys.length) {
    throw new V2CborError(`${label} has missing or unknown fields`)
  }
  const allowed = new Set(keys)
  for (const key of value.keys()) {
    if (typeof key !== 'number' || !Number.isSafeInteger(key) || !allowed.has(key)) {
      throw new V2CborError(`${label} has a non-canonical field key`)
    }
  }
  return value as ReadonlyMap<number, unknown>
}

export function requireArray(
  value: unknown,
  maximumItems: number,
  label: string,
): readonly unknown[] {
  if (!Array.isArray(value) || value.length > maximumItems) {
    throw new V2CborError(`${label} is not a bounded array`)
  }
  return value
}

export function requireBytes(
  value: unknown,
  exactBytes: number | undefined,
  label: string,
  nonzero = false,
): Uint8Array<ArrayBuffer> {
  if (
    !(value instanceof Uint8Array) ||
    (exactBytes !== undefined && value.byteLength !== exactBytes) ||
    (nonzero && !value.some((item) => item !== 0))
  ) {
    throw new V2CborError(`${label} has an invalid byte width or value`)
  }
  return value.slice()
}

export function requireUnsigned(value: unknown, label: string): bigint {
  const integer = integerValue(value)
  if (integer === undefined || integer < 0n) {
    throw new V2CborError(`${label} must be an unsigned integer`)
  }
  return integer
}

export function requireSigned(value: unknown, label: string): bigint {
  const integer = integerValue(value)
  if (integer === undefined) {
    throw new V2CborError(`${label} must be an integer`)
  }
  return integer
}

export function requireText(value: unknown, label: string): string {
  if (
    typeof value !== 'string' ||
    !isWellFormedUnicode(value) ||
    value.normalize('NFC') !== value
  ) {
    throw new V2CborError(`${label} must be well-formed NFC text`)
  }
  return value
}

export function requireBoolean(value: unknown, label: string): boolean {
  if (typeof value !== 'boolean') {
    throw new V2CborError(`${label} must be boolean`)
  }
  return value
}

function validateValue(value: unknown, depth: number): void {
  if (depth > MAXIMUM_CBOR_DEPTH) {
    throw new V2CborError('CBOR nesting exceeds the protocol limit')
  }
  if (
    value === null ||
    typeof value === 'boolean' ||
    value instanceof Uint8Array
  ) {
    return
  }
  if (typeof value === 'number') return validateNumber(value)
  if (typeof value === 'bigint') return
  if (typeof value === 'string') return validateText(value)
  if (Array.isArray(value)) return validateArray(value, depth)
  if (value instanceof Map) return validateMap(value, depth)
  throw new V2CborError('CBOR contains an unsupported semantic value')
}

function integerValue(value: unknown): bigint | undefined {
  if (typeof value === 'bigint') return value
  if (typeof value === 'number' && Number.isSafeInteger(value)) return BigInt(value)
  return undefined
}

function validateNumber(value: number): void {
  if (!Number.isSafeInteger(value)) throw new V2CborError('CBOR numbers must be safe integers')
}

function validateText(value: string): void {
  if (!isWellFormedUnicode(value) || value.normalize('NFC') !== value) {
    throw new V2CborError('CBOR text must be well-formed NFC')
  }
}

function validateArray(value: readonly unknown[], depth: number): void {
  if (value.length > MAXIMUM_CBOR_ARRAY_ITEMS) {
    throw new V2CborError('CBOR array exceeds the protocol limit')
  }
  for (const item of value) validateValue(item, depth + 1)
}

function validateMap(value: ReadonlyMap<unknown, unknown>, depth: number): void {
  if (value.size > MAXIMUM_CBOR_MAP_FIELDS) {
    throw new V2CborError('CBOR map exceeds the protocol limit')
  }
  for (const [key, item] of value) {
    if (typeof key !== 'number' || !Number.isSafeInteger(key) || key < 0) {
      throw new V2CborError('CBOR schema keys must be nonnegative integers')
    }
    validateValue(item, depth + 1)
  }
}
