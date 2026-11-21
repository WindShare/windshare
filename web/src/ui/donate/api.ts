import { DONATE_API_ORIGIN } from './config'
import type { CryptoIntentRequest, CryptoIntentResponse, PublicWallResponse } from './types'

const WALL_TIMEOUT_MS = 6_000
const INTENT_TIMEOUT_MS = 10_000

export interface WallFetchOptions {
  readonly cache?: RequestCache
  readonly signal?: AbortSignal
}

export async function fetchDonationWall(
  options: WallFetchOptions = {},
): Promise<PublicWallResponse> {
  const signal = options.signal ?? AbortSignal.timeout(WALL_TIMEOUT_MS)
  const response = await fetch(`${DONATE_API_ORIGIN}/v1/donations/wall`, {
    method: 'GET',
    ...(options.cache === undefined ? {} : { cache: options.cache }),
    signal,
  })
  if (!response.ok) {
    throw new Error(`Donation wall request failed with status ${response.status}`)
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
    signal: signal ?? AbortSignal.timeout(INTENT_TIMEOUT_MS),
  })
  if (!response.ok) {
    throw new Error(`Crypto checkout request failed with status ${response.status}`)
  }
  return await response.json() as CryptoIntentResponse
}
