const UINT64_MAX = 0xffff_ffff_ffff_ffffn
const UINT32_MAX = 0xffff_ffff
const UTC_RFC3339_PATTERN =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/

export type JsonPrimitive = string | number | boolean | null
export type JsonValue =
  | JsonPrimitive
  | { readonly [key: string]: JsonValue | undefined }
  | readonly JsonValue[]

export function decimalUint64(value: bigint, field: string): string {
  if (typeof value !== 'bigint' || value < 0n || value > UINT64_MAX) {
    throw new RangeError(`${field} must be a uint64`)
  }
  return value.toString(10)
}

export function uint32(value: number, field: string): number {
  if (!Number.isInteger(value) || value < 0 || value > UINT32_MAX) {
    throw new RangeError(`${field} must be a uint32`)
  }
  return value
}

export function boundedUtf8String(
  value: string,
  field: string,
  maximumBytes: number,
): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${field} must be a non-empty string`)
  }
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes <= 0) {
    throw new RangeError('string byte capacity must be a positive safe integer')
  }
  if (utf8ByteLength(value) > maximumBytes) {
    throw new RangeError(`${field} exceeds its UTF-8 byte capacity`)
  }
  return value
}

export function utcRfc3339(value: string, field: string): string {
  if (
    typeof value !== 'string' ||
    !UTC_RFC3339_PATTERN.test(value) ||
    Number.isNaN(Date.parse(value))
  ) {
    throw new TypeError(`${field} must be a UTC RFC3339 timestamp`)
  }
  return value
}

export function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).byteLength
}

export function jsonUtf8ByteLength(value: unknown): number {
  const encoded = JSON.stringify(value)
  if (encoded === undefined) throw new TypeError('Diagnostic value is not JSON encodable')
  return utf8ByteLength(encoded)
}

export function snakeCaseClosedValue<Value extends string>(value: Value): string {
  return value.replaceAll('-', '_')
}

export function deepFreezeJson<Value>(value: Value): Value {
  if (typeof value !== 'object' || value === null) return value
  for (const nested of Object.values(value)) deepFreezeJson(nested)
  return Object.freeze(value)
}

export function isDeeplyFrozen(value: unknown): boolean {
  if (typeof value !== 'object' || value === null) return true
  if (!Object.isFrozen(value)) return false
  return Object.values(value).every(isDeeplyFrozen)
}
