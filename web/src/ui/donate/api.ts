import { DONATE_API_ORIGIN } from './config'
import { isValidEpochSeconds } from './format'
import type {
  CryptoIntentRequest,
  CryptoIntentResponse,
  PublicWallEntry,
  PublicWallResponse,
} from './types'

const WALL_TIMEOUT_MS = 6_000
const INTENT_TIMEOUT_MS = 10_000
const INVALID_RESPONSE_ERROR_CODE = 'invalid_response'
const RATE_LIMITED_STATUS = 429
const RATE_LIMITED_ERROR_CODE = 'rate_limited'
const INVALID_AMOUNT_ERROR_CODE = 'invalid_amount'
const CRYPTO_UNAVAILABLE_ERROR_CODES: ReadonlySet<string> = new Set([
  'crypto_checkout_paused',
  'crypto_not_configured',
])

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
}

function isNullableString(value: unknown): value is string | null {
  return value === null || typeof value === 'string'
}

function isNonNegativeFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

function isValidWallTimestamp(value: unknown): value is number {
  return typeof value === 'number' && isValidEpochSeconds(value)
}

function isValidCheckoutHttpsUrl(raw: unknown): raw is string {
  if (typeof raw !== 'string') return false
  try {
    const parsed = new URL(raw)
    // Require an absolute credential-free https: URL so malformed or compromised API
    // responses cannot execute javascript: on the E2EE receiver origin or navigate to /undefined.
    return (
      parsed.protocol === 'https:'
      && parsed.username === ''
      && parsed.password === ''
    )
  } catch {
    return false
  }
}

function parsePublicWallEntry(raw: unknown): PublicWallEntry | null {
  if (!isRecord(raw)) return null
  const {
    id,
    provider,
    displayName,
    amountMinor,
    currency,
    minorDigits,
    message,
    donatedAt,
  } = raw
  if (
    !isNonEmptyString(id)
    || !isNonEmptyString(provider)
    || !isNullableString(displayName)
    || !isNonNegativeFiniteNumber(amountMinor)
    || !isNonEmptyString(currency)
    || !isNonNegativeFiniteNumber(minorDigits)
    || !isNullableString(message)
    || !isValidWallTimestamp(donatedAt)
  ) {
    return null
  }
  return {
    id,
    provider,
    displayName,
    amountMinor,
    currency,
    minorDigits,
    message,
    donatedAt,
  }
}

export function parsePublicWallResponse(raw: unknown): PublicWallResponse | null {
  if (!isRecord(raw)) return null
  const { version, entries } = raw
  if (typeof version !== 'number' || !Number.isFinite(version)) return null
  if (!Array.isArray(entries)) return null

  const parsedEntries: PublicWallEntry[] = []
  for (const item of entries) {
    const entry = parsePublicWallEntry(item)
    if (entry === null) return null
    parsedEntries.push(entry)
  }
  return { version, entries: parsedEntries }
}

export function parseCryptoIntentResponse(raw: unknown): CryptoIntentResponse | null {
  if (!isRecord(raw)) return null
  const { intentId, invoiceUrl } = raw
  if (typeof intentId !== 'string' || intentId.trim().length === 0) return null
  if (!isValidCheckoutHttpsUrl(invoiceUrl)) return null
  return { intentId, invoiceUrl }
}

export interface WallFetchOptions {
  readonly cache?: RequestCache
  readonly signal?: AbortSignal
}

export class DonateApiError extends Error {
  readonly status: number
  readonly code: string | null

  constructor(status: number, code: string | null) {
    super(code ?? `Request failed with status ${status}`)
    this.name = 'DonateApiError'
    this.status = status
    this.code = code
  }
}

export type CryptoIntentErrorKind =
  | 'unavailable'
  | 'rate_limited'
  | 'invalid_amount'
  | 'failed'

export function classifyCryptoIntentError(error: unknown): CryptoIntentErrorKind {
  if (error instanceof DonateApiError) {
    // Match explicit unavailable codes instead of status 503 so transient edge/proxy
    // 503 responses still surface the retryable failure path.
    if (error.code !== null && CRYPTO_UNAVAILABLE_ERROR_CODES.has(error.code)) {
      return 'unavailable'
    }
    if (error.status === RATE_LIMITED_STATUS || error.code === RATE_LIMITED_ERROR_CODE) {
      return 'rate_limited'
    }
    if (error.code === INVALID_AMOUNT_ERROR_CODE) {
      return 'invalid_amount'
    }
  }
  return 'failed'
}

async function readErrorCode(response: Response): Promise<string | null> {
  try {
    const body: unknown = await response.json()
    return isRecord(body) && typeof body.error === 'string' ? body.error : null
  } catch {
    return null
  }
}

// Compose caller cancellation with the transport timeout so passing a lifecycle
// AbortSignal never disables the bounded request deadline.
function composeRequestSignal(timeoutMs: number, signal?: AbortSignal): AbortSignal {
  const timeoutSignal = AbortSignal.timeout(timeoutMs)
  return signal === undefined ? timeoutSignal : AbortSignal.any([signal, timeoutSignal])
}

async function readJsonPayload(response: Response, requestSignal: AbortSignal): Promise<unknown> {
  try {
    return await response.json()
  } catch (error) {
    if (requestSignal.aborted) throw error
    throw new DonateApiError(response.status, INVALID_RESPONSE_ERROR_CODE)
  }
}

export async function fetchDonationWall(
  options: WallFetchOptions = {},
): Promise<PublicWallResponse> {
  const requestSignal = composeRequestSignal(WALL_TIMEOUT_MS, options.signal)
  const response = await fetch(`${DONATE_API_ORIGIN}/v1/donations/wall`, {
    method: 'GET',
    ...(options.cache === undefined ? {} : { cache: options.cache }),
    signal: requestSignal,
  })
  if (!response.ok) {
    const code = await readErrorCode(response)
    throw new DonateApiError(response.status, code)
  }
  const body = await readJsonPayload(response, requestSignal)
  const parsed = parsePublicWallResponse(body)
  if (parsed === null) {
    throw new DonateApiError(response.status, INVALID_RESPONSE_ERROR_CODE)
  }
  return parsed
}

export async function createCryptoIntent(
  payload: CryptoIntentRequest,
  signal?: AbortSignal,
): Promise<CryptoIntentResponse> {
  const requestSignal = composeRequestSignal(INTENT_TIMEOUT_MS, signal)
  const response = await fetch(`${DONATE_API_ORIGIN}/v1/donations/crypto-intents`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
    signal: requestSignal,
  })
  if (!response.ok) {
    const code = await readErrorCode(response)
    throw new DonateApiError(response.status, code)
  }
  const body = await readJsonPayload(response, requestSignal)
  const parsed = parseCryptoIntentResponse(body)
  if (parsed === null) {
    throw new DonateApiError(response.status, INVALID_RESPONSE_ERROR_CODE)
  }
  return parsed
}
