import { DONATE_API_ORIGIN } from './config'
import type { CryptoIntentRequest, CryptoIntentResponse, PublicWallResponse } from './types'

const WALL_TIMEOUT_MS = 6_000
const INTENT_TIMEOUT_MS = 10_000

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
  const response = await fetch(`${DONATE_API_ORIGIN}/v1/donations/crypto-intents`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
    signal: composeRequestSignal(INTENT_TIMEOUT_MS, signal),
  })
  if (!response.ok) {
    const details = await readErrorDetails(response)
    throw new DonateApiError(response.status, details.code, details.retryAfterSeconds)
  }
  return await response.json() as CryptoIntentResponse
}
