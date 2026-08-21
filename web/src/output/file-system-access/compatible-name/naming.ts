import {
  isPortableCatalogName,
  snapshotPortableCatalogPath,
  V2_CATALOG_NAME_BYTES,
} from '../../../catalog/path-policy'
import { sha256 } from '../../../crypto/digest'
import type { CryptoRuntime } from '../../../crypto/webcrypto'
import {
  canonicalFrame,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU32,
  snapshotIdentity,
} from '../../workspace/canonical'
import type { CompatibleNameEntryKind } from './model'

const TEXT_ENCODER = new TextEncoder()
const RFC_4648_BASE32_BITS_PER_CHARACTER = 5
const BITS_PER_BYTE = 8
const BYTE_VALUE_COUNT = 2 ** BITS_PER_BYTE
const UINT32_BITS = 32
const UINT32_BYTES = UINT32_BITS / BITS_PER_BYTE

export const COMPATIBLE_NAME_PRIMARY_TOKEN_BITS = 30
export const COMPATIBLE_NAME_TOKEN_CHARACTERS =
  COMPATIBLE_NAME_PRIMARY_TOKEN_BITS / RFC_4648_BASE32_BITS_PER_CHARACTER
export const COMPATIBLE_NAME_SUFFIX = '.windshare-'
export const COMPATIBLE_NAME_COLLISION_RETRY_LIMIT = 7
export const COMPATIBLE_NAME_FIXED_SUFFIX_UTF8_BYTES =
  TEXT_ENCODER.encode(COMPATIBLE_NAME_SUFFIX).byteLength + COMPATIBLE_NAME_TOKEN_CHARACTERS
export const COMPATIBLE_NAME_READABLE_PREFIX_MAX_UTF8_BYTES =
  V2_CATALOG_NAME_BYTES - COMPATIBLE_NAME_FIXED_SUFFIX_UTF8_BYTES
export const COMPATIBLE_NAME_READABLE_PREFIX_FALLBACK = 'entry'
export const COMPATIBLE_NAME_FALLBACK_TOKEN_DOMAIN =
  'windshare/compatible-name-fallback/v1'
export const COMPATIBLE_NAME_FALLBACK_TOKEN_VERSION = 1

const OPERATION_ID_BYTES = 16
const MAX_PRIMARY_TOKEN_VALUE = 2 ** COMPATIBLE_NAME_PRIMARY_TOKEN_BITS - 1
const PRIMARY_TOKEN_VALUE_COUNT = MAX_PRIMARY_TOKEN_VALUE + 1
const RFC_4648_BASE32_LOWERCASE_ALPHABET = 'abcdefghijklmnopqrstuvwxyz234567'
const TOKEN_PATTERN = /^[a-z2-7]{6}$/u
const PORTABILITY_PROBE_TOKEN = 'aaaaaa'
const READABLE_BASE_SCALAR_PATTERN = /^[\p{L}\p{N}]$/u
const READABLE_MARK_SCALAR_PATTERN = /^\p{M}$/u
const READABLE_SEPARATOR = '-'

export type CompatibleNameRandomBitsSource = () => number

export interface CompatibleNameCandidateInput {
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly primaryToken: string
  readonly attempt: number
}

export interface CompatibleNameCandidate {
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly physicalComponent: string
  readonly attempt: number
  readonly token: string
}

/**
 * The source returns one uniformly sampled value in the complete 30-bit token domain. Keeping the
 * discarded WebCrypto bits below this boundary makes the token's entropy contract explicit and
 * lets tests prove that operation bootstrap performs exactly one sampling call.
 */
export function generateCompatibleNamePrimaryToken(
  randomBits: CompatibleNameRandomBitsSource = randomCompatibleNameBits,
): string {
  return encodeCompatibleNameToken(assertThirtyBitValue(randomBits()))
}

/**
 * Retry is intentionally caller-directed: only proven occupation may advance the attempt. The
 * frozen return value can therefore be persisted as an immutable namespace choice before creation.
 */
export async function compatibleNameCandidate(
  input: CompatibleNameCandidateInput,
  cryptoRuntime?: CryptoRuntime,
): Promise<CompatibleNameCandidate> {
  const operationId = snapshotIdentity(input.operationId, OPERATION_ID_BYTES, 'operation ID')
  const logicalPath = snapshotPortableCatalogPath(input.logicalPath)
  const entryKind = compatibleNameEntryKind(input.entryKind)
  const attempt = compatibleNameAttempt(input.attempt)
  const primaryToken = compatibleNameToken(input.primaryToken, 'primary compatible-name token')
  const token = attempt === 0
    ? primaryToken
    : await deriveCompatibleNameFallbackToken(
      { operationId, logicalPath, entryKind, attempt },
      cryptoRuntime,
    )
  const readablePrefix = compatibleNameReadablePrefix(logicalPath.at(-1) ?? '')
  const physicalComponent = `${readablePrefix}${COMPATIBLE_NAME_SUFFIX}${token}`
  if (!isPortableCatalogName(physicalComponent)) {
    throw new TypeError('compatible physical component violates the canonical path policy')
  }
  return Object.freeze({
    operationId,
    logicalPath,
    entryKind,
    physicalComponent,
    attempt,
    token,
  })
}

export function compatibleNameReadablePrefix(logicalComponent: string): string {
  if (!isPortableCatalogName(logicalComponent)) {
    throw new TypeError('compatible-name prefix source violates the canonical path policy')
  }
  const sanitizedPrefix = sanitizeCompatibleNameReadablePrefix(logicalComponent)
  const prefix = truncateCompatibleNameReadablePrefix(sanitizedPrefix)
  // Removing punctuation can expose a Windows-reserved stem (for example, `COM1!` -> `COM1`).
  // Prefixing that rare result keeps candidate generation total for every portable source name.
  if (!isPortableCatalogName(`${prefix}${COMPATIBLE_NAME_SUFFIX}${PORTABILITY_PROBE_TOKEN}`)) {
    return truncateCompatibleNameReadablePrefix(
      `${COMPATIBLE_NAME_READABLE_PREFIX_FALLBACK}${READABLE_SEPARATOR}${prefix}`,
    )
  }
  return prefix
}

/**
 * This is deliberately a candidate policy, not a browser-rejection oracle. It runs only after the
 * native lookup has rejected the logical component, and reduces syntax diversity without erasing
 * readable Unicode text or relying on browser-, platform-, or error-specific rules.
 */
function sanitizeCompatibleNameReadablePrefix(logicalComponent: string): string {
  let sanitized = ''
  let separatorPending = false
  let hasReadableBase = false
  for (const scalar of logicalComponent) {
    if (READABLE_BASE_SCALAR_PATTERN.test(scalar)) {
      if (separatorPending) sanitized += READABLE_SEPARATOR
      sanitized += scalar
      separatorPending = false
      hasReadableBase = true
      continue
    }
    if (hasReadableBase && !separatorPending && READABLE_MARK_SCALAR_PATTERN.test(scalar)) {
      sanitized += scalar
      continue
    }
    separatorPending = true
    hasReadableBase = false
  }
  return sanitized.length === 0 ? COMPATIBLE_NAME_READABLE_PREFIX_FALLBACK : sanitized
}

function truncateCompatibleNameReadablePrefix(readablePrefix: string): string {
  let prefix = ''
  let retainedBytes = 0
  for (const scalar of readablePrefix) {
    const scalarBytes = TEXT_ENCODER.encode(scalar).byteLength
    if (retainedBytes + scalarBytes > COMPATIBLE_NAME_READABLE_PREFIX_MAX_UTF8_BYTES) break
    prefix += scalar
    retainedBytes += scalarBytes
  }
  return prefix
}

export interface CompatibleNameFallbackTokenInput {
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly attempt: number
}

export async function deriveCompatibleNameFallbackToken(
  input: CompatibleNameFallbackTokenInput,
  cryptoRuntime?: CryptoRuntime,
): Promise<string> {
  const attempt = compatibleNameFallbackAttempt(input.attempt)
  const preimage = canonicalRecord(
    COMPATIBLE_NAME_FALLBACK_TOKEN_DOMAIN,
    COMPATIBLE_NAME_FALLBACK_TOKEN_VERSION,
    [
      canonicalFrame(canonicalIdentity(input.operationId, OPERATION_ID_BYTES, 'operation ID')),
      canonicalFrame(canonicalPath(input.logicalPath)),
      canonicalFrame(canonicalText(compatibleNameEntryKind(input.entryKind))),
      canonicalFrame(canonicalU32(attempt)),
    ],
  )
  const digest = cryptoRuntime === undefined
    ? await sha256(preimage)
    : await sha256(preimage, cryptoRuntime)
  return encodeCompatibleNameToken(thirtyBitPrefix(digest))
}

function randomCompatibleNameBits(): number {
  const crypto = globalThis.crypto
  if (crypto?.getRandomValues === undefined) {
    throw new TypeError('WebCrypto random generation is unavailable')
  }
  const sample = new Uint32Array(1)
  crypto.getRandomValues(sample)
  return (sample[0] ?? 0) % PRIMARY_TOKEN_VALUE_COUNT
}

function thirtyBitPrefix(bytes: Uint8Array): number {
  if (bytes.byteLength < UINT32_BYTES) {
    throw new TypeError('compatible-name token source is too short')
  }
  let prefix = 0
  for (let index = 0; index < UINT32_BYTES; index += 1) {
    prefix = (prefix * BYTE_VALUE_COUNT) + (bytes[index] ?? 0)
  }
  return Math.floor(prefix / (2 ** (UINT32_BITS - COMPATIBLE_NAME_PRIMARY_TOKEN_BITS)))
}

function encodeCompatibleNameToken(value: number): string {
  let token = ''
  for (
    let shift = COMPATIBLE_NAME_PRIMARY_TOKEN_BITS - RFC_4648_BASE32_BITS_PER_CHARACTER;
    shift >= 0;
    shift -= RFC_4648_BASE32_BITS_PER_CHARACTER
  ) {
    const alphabetIndex = Math.floor(value / (2 ** shift)) % RFC_4648_BASE32_LOWERCASE_ALPHABET.length
    token += RFC_4648_BASE32_LOWERCASE_ALPHABET[alphabetIndex] ?? ''
  }
  return token
}

function assertThirtyBitValue(value: number): number {
  if (!Number.isInteger(value) || value < 0 || value > MAX_PRIMARY_TOKEN_VALUE) {
    throw new TypeError('compatible-name random source must return exactly one 30-bit value')
  }
  return value
}

function compatibleNameAttempt(value: number): number {
  if (!Number.isInteger(value) || value < 0 || value > COMPATIBLE_NAME_COLLISION_RETRY_LIMIT) {
    throw new TypeError('compatible-name candidate attempt exceeds the collision retry limit')
  }
  return value
}

function compatibleNameFallbackAttempt(value: number): number {
  const attempt = compatibleNameAttempt(value)
  if (attempt === 0) throw new TypeError('compatible-name fallback attempt must follow the primary')
  return attempt
}

function compatibleNameToken(value: string, label: string): string {
  if (typeof value !== 'string' || !TOKEN_PATTERN.test(value)) {
    throw new TypeError(`${label} must be six lowercase RFC 4648 Base32 characters`)
  }
  return value
}

function compatibleNameEntryKind(value: CompatibleNameEntryKind): CompatibleNameEntryKind {
  if (value !== 'file' && value !== 'directory') {
    throw new TypeError('compatible-name entry kind is invalid')
  }
  return value
}
