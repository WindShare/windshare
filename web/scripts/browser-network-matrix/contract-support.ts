import { parseCanonicalJsonText } from '../browser-evidence/contract/strict-json.ts'

const TEXT_ENCODER = new TextEncoder()
const CANONICAL_UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u
// Maximal valid 45-sample ledgers exceed 64 KiB; this must remain byte-for-byte
// aligned with the Go parser's denial-of-service boundary.
const MAXIMUM_CONTRACT_BYTES = 8 * 1024 * 1024

export type JsonRecord = Record<string, unknown>

export class BrowserNetworkMatrixContractError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(`browser network matrix contract: ${message}`, options)
    this.name = 'BrowserNetworkMatrixContractError'
  }
}

export function networkMatrixError(message: string): never {
  throw new BrowserNetworkMatrixContractError(message)
}

export function parseNetworkMatrixJsonText(encoded: string, label: string): unknown {
  if (TEXT_ENCODER.encode(encoded).byteLength > MAXIMUM_CONTRACT_BYTES) {
    networkMatrixError(`${label} exceeds ${MAXIMUM_CONTRACT_BYTES} UTF-8 bytes`)
  }
  try {
    return parseCanonicalJsonText(encoded, label)
  } catch (cause) {
    throw new BrowserNetworkMatrixContractError(`${label} is not strict JSON`, { cause })
  }
}

export function requireCanonicalEncoding<T extends object>(
  encoded: string,
  parsed: T,
  label: string,
): T {
  if (encoded !== `${JSON.stringify(parsed)}\n`) {
    networkMatrixError(`${label} bytes differ from the canonical minified JSON plus LF encoding`)
  }
  return parsed
}

export function requireRecord(value: unknown, label: string): JsonRecord {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    networkMatrixError(`${label} must be an object`)
  }
  const prototype = Object.getPrototypeOf(value) as object | null
  if (prototype !== Object.prototype && prototype !== null) {
    networkMatrixError(`${label} must be a plain JSON object`)
  }
  for (const key of Reflect.ownKeys(value)) {
    if (typeof key !== 'string') networkMatrixError(`${label} contains a non-JSON symbol field`)
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (descriptor === undefined || !descriptor.enumerable || !Object.hasOwn(descriptor, 'value')) {
      networkMatrixError(`${label} field ${JSON.stringify(key)} must be an enumerable JSON data field`)
    }
  }
  return value as JsonRecord
}

export function requireExactKeys(
  value: JsonRecord,
  required: readonly string[],
  label: string,
): void {
  if (
    Object.keys(value).length !== required.length ||
    required.some((key) => !Object.hasOwn(value, key))
  ) {
    networkMatrixError(`${label} must contain exactly ${required.join(', ')}`)
  }
}

export function requireArray(value: unknown, label: string): readonly unknown[] {
  if (!Array.isArray(value)) networkMatrixError(`${label} must be an array`)
  for (const key of Reflect.ownKeys(value)) {
    if (key === 'length') continue
    if (typeof key !== 'string' || !/^(0|[1-9]\d*)$/u.test(key) || Number(key) >= value.length) {
      networkMatrixError(`${label} contains a non-JSON array field`)
    }
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (descriptor === undefined || !descriptor.enumerable || !Object.hasOwn(descriptor, 'value')) {
      networkMatrixError(`${label} index ${key} must be an enumerable JSON data field`)
    }
  }
  for (let index = 0; index < value.length; index += 1) {
    if (!Object.hasOwn(value, index)) networkMatrixError(`${label} must not be sparse`)
  }
  return value
}

export function requireLiteral<T extends string | number | boolean>(
  value: unknown,
  expected: T,
  label: string,
): T {
  if (value !== expected) networkMatrixError(`${label} must be ${JSON.stringify(expected)}`)
  return expected
}

export function requireEnum<T extends string>(
  value: unknown,
  allowed: readonly T[],
  label: string,
): T {
  if (typeof value !== 'string' || !allowed.includes(value as T)) {
    networkMatrixError(`${label} is outside the frozen vocabulary`)
  }
  return value as T
}

export function requireString(value: unknown, label: string, maximumBytes: number): string {
  if (
    typeof value !== 'string' || value === '' || !isWellFormedUnicode(value) ||
    value.normalize('NFC') !== value ||
    TEXT_ENCODER.encode(value).byteLength > maximumBytes
  ) {
    networkMatrixError(`${label} must be non-empty NFC text within ${maximumBytes} UTF-8 bytes`)
  }
  return value
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const current = value.charCodeAt(index)
    if (current >= 0xD800 && current <= 0xDBFF) {
      if (index + 1 >= value.length) return false
      const next = value.charCodeAt(index + 1)
      if (next < 0xDC00 || next > 0xDFFF) return false
      index += 1
    } else if (current >= 0xDC00 && current <= 0xDFFF) {
      return false
    }
  }
  return true
}

export function requireSafeInteger(
  value: unknown,
  minimum: number,
  maximum: number,
  label: string,
): number {
  if (!Number.isSafeInteger(value) || (value as number) < minimum || (value as number) > maximum) {
    networkMatrixError(`${label} must be an integer in [${minimum}, ${maximum}]`)
  }
  return value as number
}

export function requireSha256(value: unknown, label: string): string {
  if (typeof value !== 'string' || !/^[0-9a-f]{64}$/u.test(value)) {
    networkMatrixError(`${label} must be lowercase SHA-256`)
  }
  return value
}

export function requireCanonicalUtcTimestamp(value: unknown, label: string): string {
  if (typeof value !== 'string' || !CANONICAL_UTC_TIMESTAMP_PATTERN.test(value)) {
    networkMatrixError(`${label} must be a canonical UTC timestamp with millisecond precision`)
  }
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) {
    networkMatrixError(`${label} must name a real canonical UTC instant`)
  }
  return value
}

export function requireCheckoutSha(value: unknown, label: string): string {
  if (typeof value !== 'string' || !/^[0-9a-f]{40}$/u.test(value)) {
    networkMatrixError(`${label} must be a lowercase 40-hex checkout SHA`)
  }
  return value
}

export function requireRunId(value: unknown, label: string): string {
  const runId = requireString(value, label, 96)
  if (!/^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u.test(runId)) {
    networkMatrixError(`${label} is not a canonical lowercase authority identifier`)
  }
  return runId
}

export function requireCanonicalStringSet<T extends string>(
  value: unknown,
  vocabulary: readonly T[],
  label: string,
): readonly T[] {
  const entries = requireArray(value, label).map((entry) => requireEnum(entry, vocabulary, label))
  if (new Set(entries).size !== entries.length) networkMatrixError(`${label} contains duplicates`)
  const positions = entries.map((entry) => vocabulary.indexOf(entry))
  if (positions.some((position, index) => index > 0 && position <= (positions[index - 1] ?? -1))) {
    networkMatrixError(`${label} is not in frozen vocabulary order`)
  }
  return Object.freeze(entries)
}

export function requireExactArray<T>(
  value: unknown,
  expected: readonly T[],
  label: string,
): readonly T[] {
  const entries = requireArray(value, label)
  if (entries.length !== expected.length || entries.some((entry, index) => entry !== expected[index])) {
    networkMatrixError(`${label} differs from the frozen ordered registry`)
  }
  return Object.freeze([...expected])
}

export function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
