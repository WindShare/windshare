import type { NetworkMatrixBrowser } from '../../vocabulary.ts'

export const MAXIMUM_CONTAINED_BROWSER_SECRET_BYTES = 1_048_576
export const SHA256_PATTERN = /^[a-f0-9]{64}$/u
export const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
export const PROCESS_INSTANCE_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const MINIMUM_CONTROL_CREDENTIAL_BYTES = 32
const MAXIMUM_CONTROL_CREDENTIAL_BYTES = 512
const BROWSERS = Object.freeze(['chromium', 'firefox', 'webkit'] as const)
const CANONICAL_UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u

export function exactRecord(
  value: unknown,
  keys: readonly string[],
  invalid: () => never = invalidSecret,
): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalid()
  const record = value as Record<string, unknown>
  const actual = Object.keys(record)
  if (actual.length !== keys.length || actual.some((key, index) => key !== keys[index])) {
    invalid()
  }
  return record
}

export function requireBrowser(value: unknown): NetworkMatrixBrowser {
  if (typeof value !== 'string' || !BROWSERS.includes(value as NetworkMatrixBrowser)) {
    invalidOutput()
  }
  return value as NetworkMatrixBrowser
}

export function requireOpaqueId(value: unknown): string {
  if (typeof value !== 'string' || !OPAQUE_ID_PATTERN.test(value)) invalidOutput()
  return value
}

export function requireProcessInstanceId(value: unknown): string {
  if (typeof value !== 'string' || !PROCESS_INSTANCE_PATTERN.test(value)) invalidOutput()
  return value
}

export function requireCanonicalId(
  value: unknown,
  invalid: () => never = invalidSecret,
): string {
  if (typeof value !== 'string' || !PROCESS_INSTANCE_PATTERN.test(value)) invalid()
  return value
}

export function canonicalTimestamp(value: unknown): number {
  if (typeof value !== 'string' || !CANONICAL_UTC_TIMESTAMP_PATTERN.test(value)) invalidSecret()
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) invalidSecret()
  return timestamp
}

export function isCanonicalTimestamp(value: string): boolean {
  if (!CANONICAL_UTC_TIMESTAMP_PATTERN.test(value)) return false
  const timestamp = Date.parse(value)
  return !Number.isNaN(timestamp) && new Date(timestamp).toISOString() === value
}

export function isControlCredentialBytes(value: unknown): value is Uint8Array {
  if (
    !(value instanceof Uint8Array) ||
    value.byteLength < MINIMUM_CONTROL_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CONTROL_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}

export function invalidSecret(): never {
  throw new Error('contained browser secret authority is invalid')
}

export function invalidOutput(): never {
  throw new Error('contained browser sample output is invalid')
}
