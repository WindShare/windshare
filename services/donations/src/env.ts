import type {
  AdminConfigPatch,
  AdminDonationsPage,
  AdminSessionRecord,
  AdminVisibilityCommand,
  CommitDonationInput,
  CommitDonationResult,
  CreateAdminSessionInput,
  CreateCryptoIntentInput,
  CryptoCheckoutIntent,
  DonationConfig,
  DonationProvider,
  PublicWallReadResult,
} from './model'
import type {
  CreateIntentResult,
  ModerateVisibilityResult,
  RateLimitDecision,
} from './wall-store'

export interface DonationWallRpc extends Rpc.DurableObjectBranded {
  getPublicWall(ifNoneMatch: string | null): PublicWallReadResult
  createCryptoIntent(input: CreateCryptoIntentInput): CreateIntentResult
  getCryptoIntent(intentId: string, nowSec: number): CryptoCheckoutIntent | null
  hasDonation(provider: DonationProvider, externalId: string): boolean
  findExistingExternalIds(provider: DonationProvider, externalIds: readonly string[]): string[]
  commitDonation(input: CommitDonationInput, nowSec: number): CommitDonationResult
  commitDonationsBatch(inputs: readonly CommitDonationInput[], nowSec: number): CommitDonationResult[]
  consumeAdminLoginRateLimit(clientIp: string, nowSec: number): RateLimitDecision
  createAdminSession(input: CreateAdminSessionInput): AdminSessionRecord
  verifyAdminSession(sessionHash: string, nowSec: number): AdminSessionRecord | null
  revokeAdminSession(sessionHash: string): void
  getAdminConfig(): DonationConfig
  patchAdminConfig(patch: AdminConfigPatch): DonationConfig
  listAdminDonations(afterCursor: string | null, rawLimit: number | null): AdminDonationsPage
  moderateDonationVisibility(
    donationId: string,
    command: AdminVisibilityCommand,
    nowSec: number,
  ): ModerateVisibilityResult
}

export interface DonationsEnv {
  readonly DONATION_WALL: DurableObjectNamespace<DonationWallRpc>
  readonly AFDIAN_USER_ID: string
  readonly ALLOWED_ORIGINS: string
  readonly DONATE_API_ORIGIN: string
  readonly AFDIAN_TOKEN?: string
  readonly AFDIAN_WEBHOOK_SECRET?: string
  readonly KOFI_VERIFICATION_TOKEN?: string
  readonly NOWPAYMENTS_API_KEY?: string
  readonly NOWPAYMENTS_IPN_SECRET?: string
  readonly DONATE_ADMIN_PASSWORD?: string
}

export const MIN_ADMIN_PASSWORD_LENGTH = 24

export type AdminPasswordResolution =
  | { readonly ok: true; readonly password: string }
  | { readonly ok: false; readonly reason: 'missing' | 'too_short' }

export function resolveAdminPassword(env: DonationsEnv): AdminPasswordResolution {
  // Admin login deliberately omits a shared global rate-limit bucket so unauthenticated
  // callers cannot lock out the operator; distributed guessing resistance therefore relies
  // on a high-entropy secret (24 chars matches the lower bound of 128-bit base64).
  const raw = env.DONATE_ADMIN_PASSWORD
  if (!raw) {
    return { ok: false, reason: 'missing' }
  }
  if (raw.trim().length < MIN_ADMIN_PASSWORD_LENGTH) {
    return { ok: false, reason: 'too_short' }
  }
  return { ok: true, password: raw }
}

export function resolveCryptoCheckoutApiKey(env: DonationsEnv): string | null {
  // Require both invoice creation and IPN verification secrets before accepting new
  // checkouts so paid orders cannot stall in unverified 503 webhooks. Webhook settlement
  // stays decoupled from NOWPAYMENTS_API_KEY so in-flight payments can finish settling
  // even when checkout creation is disabled by removing the API key.
  if (!env.NOWPAYMENTS_API_KEY || !env.NOWPAYMENTS_IPN_SECRET) {
    return null
  }
  return env.NOWPAYMENTS_API_KEY
}

export function parseAllowedOrigins(raw: string): ReadonlySet<string> {
  const origins = new Set<string>()
  for (const part of raw.split(',')) {
    const trimmed = part.trim()
    if (trimmed.length === 0) continue
    try {
      const parsed = new URL(trimmed)
      if (parsed.protocol === 'https:' || parsed.protocol === 'http:') {
        origins.add(parsed.origin)
      }
    } catch {
      // Ignore malformed origin entries instead of weakening allowlist matching.
    }
  }
  return origins
}
