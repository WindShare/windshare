import { DONATE_API_ORIGIN } from './config'
import type { CryptoIntentRequest, CryptoIntentResponse, PublicWallResponse } from './types'

const WALL_TIMEOUT_MS = 6_000
const INTENT_TIMEOUT_MS = 10_000
const INVALID_RESPONSE_ERROR_CODE = 'invalid_response'

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
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
  readonly retryAfterSeconds: number | null

  constructor(status: number, code: string | null, retryAfterSeconds: number | null) {
    super(code ?? `Request failed with status ${status}`)
    this.name = 'DonateApiError'
    this.status = status
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
  }
}

async function readErrorDetails(response: Response): Promise<{ code: string | null; retryAfterSeconds: number | null }> {
  const retryHeader = response.headers.get('Retry-After')
  const retryFromHeader = retryHeader !== null && Number.isFinite(Number(retryHeader))
    ? Number(retryHeader)
    : null
  try {
    const body = await response.json() as { error?: unknown; retryAfterSeconds?: unknown }
    const code = typeof body.error === 'string' ? body.error : null
    const retryFromBody = typeof body.retryAfterSeconds === 'number' && Number.isFinite(body.retryAfterSeconds)
      ? body.retryAfterSeconds
      : null
    return { code, retryAfterSeconds: retryFromBody ?? retryFromHeader }
  } catch {
    return { code: null, retryAfterSeconds: retryFromHeader }
  }
}

// Compose caller cancellation with the transport timeout so passing a lifecycle
// AbortSignal never disables the bounded request deadline.
function composeRequestSignal(timeoutMs: number, signal?: AbortSignal): AbortSignal {
  const timeoutSignal = AbortSignal.timeout(timeoutMs)
  return signal === undefined ? timeoutSignal : AbortSignal.any([signal, timeoutSignal])
}

export async function fetchDonationWall(
  options: WallFetchOptions = {},
): Promise<PublicWallResponse> {
  const response = await fetch(`${DONATE_API_ORIGIN}/v1/donations/wall`, {
    method: 'GET',
    ...(options.cache === undefined ? {} : { cache: options.cache }),
    signal: composeRequestSignal(WALL_TIMEOUT_MS, options.signal),
  })
  if (!response.ok) {
    const details = await readErrorDetails(response)
    throw new DonateApiError(response.status, details.code, details.retryAfterSeconds)
  }
  return await response.json() as PublicWallResponse
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
    const details = await readErrorDetails(response)
    throw new DonateApiError(response.status, details.code, details.retryAfterSeconds)
  }
  let body: unknown
  try {
    body = await response.json()
  } catch (error) {
    if (requestSignal.aborted) throw error
    throw new DonateApiError(response.status, INVALID_RESPONSE_ERROR_CODE, null)
  }
  const parsed = parseCryptoIntentResponse(body)
  if (parsed === null) {
    throw new DonateApiError(response.status, INVALID_RESPONSE_ERROR_CODE, null)
  }
  return parsed
}
