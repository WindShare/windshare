const TEXT_ENCODER = new TextEncoder()

export type JsonRecord = Record<string, unknown>

export class BrowserEvidenceContractError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(`browser evidence contract: ${message}`, options)
    this.name = 'BrowserEvidenceContractError'
  }
}

export function contractError(message: string): never {
  throw new BrowserEvidenceContractError(message)
}

export function requireRecord(value: unknown, label: string): JsonRecord {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    contractError(`${label} must be an object`)
  }
  const prototype = Object.getPrototypeOf(value) as object | null
  if (prototype !== Object.prototype && prototype !== null) {
    contractError(`${label} must be a plain JSON object`)
  }
  for (const key of Reflect.ownKeys(value)) {
    if (typeof key !== 'string') contractError(`${label} contains a non-JSON symbol field`)
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (descriptor === undefined || !descriptor.enumerable || !Object.hasOwn(descriptor, 'value')) {
      contractError(`${label} field ${JSON.stringify(key)} must be an enumerable JSON data field`)
    }
  }
  return value as JsonRecord
}

export function requireExactKeys(
  value: JsonRecord,
  required: readonly string[],
  optional: readonly string[],
  label: string,
): void {
  const allowed = new Set([...required, ...optional])
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) contractError(`${label} contains unknown field ${JSON.stringify(key)}`)
  }
  for (const key of required) {
    if (!Object.hasOwn(value, key)) contractError(`${label} is missing field ${JSON.stringify(key)}`)
  }
}

export function requireLiteral<T extends string | number>(
  value: unknown,
  expected: T,
  label: string,
): T {
  if (value !== expected) contractError(`${label} must be ${JSON.stringify(expected)}`)
  return expected
}

export function requireEnum<T extends string>(
  value: unknown,
  allowed: readonly T[],
  label: string,
): T {
  if (typeof value !== 'string' || !allowed.includes(value as T)) {
    contractError(`${label} is outside the frozen vocabulary`)
  }
  return value as T
}

export function requireString(
  value: unknown,
  label: string,
  maximumUtf8Bytes: number,
): string {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    value.normalize('NFC') !== value ||
    TEXT_ENCODER.encode(value).byteLength > maximumUtf8Bytes
  ) {
    contractError(`${label} must be non-empty NFC text within ${maximumUtf8Bytes} UTF-8 bytes`)
  }
  return value
}

export function requireSafeInteger(
  value: unknown,
  minimum: number,
  maximum: number,
  label: string,
): number {
  if (!Number.isSafeInteger(value) || (value as number) < minimum || (value as number) > maximum) {
    contractError(`${label} must be an integer in [${minimum}, ${maximum}]`)
  }
  return value as number
}

export function requireBoolean(value: unknown, label: string): boolean {
  if (typeof value !== 'boolean') contractError(`${label} must be boolean`)
  return value
}

export function requireArray(value: unknown, label: string): readonly unknown[] {
  if (!Array.isArray(value)) contractError(`${label} must be an array`)
  for (const key of Reflect.ownKeys(value)) {
    if (key === 'length') continue
    if (typeof key !== 'string' || !/^(0|[1-9]\d*)$/u.test(key) || Number(key) >= value.length) {
      contractError(`${label} contains a non-JSON array field`)
    }
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (descriptor === undefined || !descriptor.enumerable || !Object.hasOwn(descriptor, 'value')) {
      contractError(`${label} index ${key} must be an enumerable JSON data field`)
    }
  }
  for (let index = 0; index < value.length; index += 1) {
    if (!Object.hasOwn(value, index)) contractError(`${label} must not contain sparse entries`)
  }
  return value
}

export function requireCanonicalIdentity(value: unknown, label: string): string {
  if (
    typeof value !== 'string' ||
    !/^[A-Za-z0-9_-]{21}[AQgw]$/u.test(value) ||
    value === 'AAAAAAAAAAAAAAAAAAAAAA'
  ) {
    contractError(`${label} must be canonical unpadded base64url for a nonzero 16-byte identity`)
  }
  return value
}

export function requireSha256(value: unknown, label: string): string {
  if (typeof value !== 'string' || !/^[0-9a-f]{64}$/u.test(value)) {
    contractError(`${label} must be a lowercase SHA-256 digest`)
  }
  return value
}

export function requireCheckoutSha(value: unknown, label: string): string {
  if (typeof value !== 'string' || !/^[0-9a-f]{40}$/u.test(value)) {
    contractError(`${label} must be a lowercase 40-hex checkout SHA`)
  }
  return value
}

export function requireDecimalUint64(value: unknown, label: string): string {
  if (typeof value !== 'string' || !/^[1-9]\d{0,19}$/u.test(value)) {
    contractError(`${label} must be a positive canonical decimal uint64 string`)
  }
  const parsed = BigInt(value)
  if (parsed > 0xffff_ffff_ffff_ffffn) contractError(`${label} exceeds uint64`)
  return value
}

export function optionalField(value: JsonRecord, key: string): unknown | undefined {
  if (!Object.hasOwn(value, key)) return undefined
  const field = value[key]
  // JSON has no undefined value. Treating an own undefined field as absent lets
  // in-memory producers bypass required pairings such as failure code/message.
  if (field === undefined) contractError(`field ${JSON.stringify(key)} must not be undefined`)
  return field
}

export function freezeRecord<T extends object>(value: T): Readonly<T> {
  for (const item of Object.values(value)) {
    if (typeof item === 'object' && item !== null && !Object.isFrozen(item)) Object.freeze(item)
  }
  return Object.freeze(value)
}
