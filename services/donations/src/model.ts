export const MIN_PUBLIC_CNY_MINOR = 1000
export const MIN_PUBLIC_USD_MINOR = 500
export const MIN_CRYPTO_USD_MINOR = 500
export const MAX_CRYPTO_USD_MINOR = 10_000_000
export const PUBLIC_WALL_LIMIT = 50
export const TOP_SUPPORTERS_LIMIT = 3
// Align with NOWPayments' 7-day payment tracking window so slow confirmations and IPN retries retain donor notes.
export const CRYPTO_INTENT_TTL_SECONDS = 7 * 24 * 60 * 60
export const ADMIN_SESSION_TTL_SECONDS = 12 * 60 * 60
export const MAX_DISPLAY_NAME_CODEPOINTS = 32
export const MAX_MESSAGE_CODEPOINTS = 140
export const MAX_HIDE_REASON_CODEPOINTS = 140
export const MAX_ADMIN_SEARCH_CODEPOINTS = 64
export const MAX_MANUAL_EXTERNAL_ID_CODEPOINTS = 96
export const MAX_MANUAL_IDEMPOTENCY_KEY_LENGTH = 128
export const MAX_FUTURE_SKEW_SECONDS = 60

export const DONATION_PROVIDERS = ['afdian', 'kofi', 'nowpayments', 'direct'] as const
export type DonationProvider = (typeof DONATION_PROVIDERS)[number]

export const VISIBILITY_KINDS = [
  'published',
  'pending_review',
  'below_threshold',
  'hidden',
  'private_donor',
] as const
export type VisibilityKind = (typeof VISIBILITY_KINDS)[number]

export const KNOWN_CURRENCY_DECIMALS: Readonly<Record<string, number>> = Object.freeze({
  CNY: 2,
  USD: 2,
  EUR: 2,
  GBP: 2,
  JPY: 0,
})

export type PendingReviewReason =
  | 'moderation_mode'
  | 'url_detected'
  | 'pii_detected'
  | 'unsupported_currency'

export type RestorableVisibility =
  | { readonly kind: 'published' }
  | { readonly kind: 'below_threshold' }
  | { readonly kind: 'pending_review'; readonly reason: PendingReviewReason }

export type WallVisibility =
  | RestorableVisibility
  | { readonly kind: 'private_donor' }
  | {
    readonly kind: 'hidden'
    readonly moderatedAt: number
    readonly reason: string
    readonly previous: RestorableVisibility
  }

export interface CryptoCheckoutIntent {
  readonly intentId: string
  readonly displayName: string | null
  readonly message: string | null
  readonly requestedUsdMinor: number
  readonly returnOrigin: string
  readonly createdAt: number
  readonly expiresAt: number
}

export interface DonationRecord {
  readonly id: string
  readonly provider: DonationProvider
  readonly externalId: string
  readonly displayName: string | null
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly message: string | null
  readonly visibility: WallVisibility
  readonly donatedAt: number
}

export interface PublicWallEntry {
  readonly id: string
  readonly provider: DonationProvider
  readonly displayName: string | null
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly message: string | null
  readonly donatedAt: number
}

export interface PublicWallResponse {
  readonly version: number
  readonly entries: readonly PublicWallEntry[]
  readonly topSupporters: readonly PublicWallEntry[]
}

export type PublicWallReadResult =
  | { readonly notModified: true; readonly etag: string }
  | { readonly notModified: false; readonly etag: string; readonly json: string }

export interface CreateCryptoIntentInput {
  readonly clientIp: string
  readonly displayName: string | null
  readonly message: string | null
  readonly requestedUsdMinor: number
  readonly returnOrigin: string
  readonly nowSec: number
}

export interface CreateAdminSessionInput {
  readonly sessionHash: string
  readonly csrfToken: string
  readonly nowSec: number
}

export interface DonationConfig {
  readonly minPublicCnyMinor: number
  readonly minPublicUsdMinor: number
  readonly moderationEnabled: boolean
  readonly cryptoCheckoutPaused: boolean
}

export interface AdminConfigPatch {
  readonly minPublicCnyMinor?: number
  readonly minPublicUsdMinor?: number
  readonly moderationEnabled?: boolean
  readonly cryptoCheckoutPaused?: boolean
}

export type AdminVisibilityCommand =
  | { readonly action: 'approve' }
  | { readonly action: 'hide'; readonly reason: string }
  | { readonly action: 'restore' }

export type VisibilityTransitionResult =
  | { readonly ok: true; readonly next: WallVisibility }
  | { readonly ok: false; readonly code: 'private_donor_immutable' | 'invalid_transition' }

export interface CommitDonationInput {
  readonly provider: DonationProvider
  readonly externalId: string
  readonly displayName: string | null
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly message: string | null
  readonly isPublic: boolean
  readonly donatedAt: number | null
}

export type CommitDonationOutcome = 'inserted' | 'deduplicated' | 'redacted_private'

export type NonPrivateVisibilityKind = Exclude<VisibilityKind, 'private_donor'>

export type CommitDonationResult =
  | {
    readonly outcome: 'inserted' | 'deduplicated'
    readonly record: DonationRecord
  }
  | {
    readonly outcome: 'redacted_private'
    readonly record: DonationRecord
    readonly previousVisibility: NonPrivateVisibilityKind
  }

export interface CreateManualDonationInput {
  readonly provider: DonationProvider
  readonly externalId: string | null
  readonly displayName: string | null
  readonly amountMinor: number
  readonly currency: string
  readonly minorDigits: number
  readonly message: string | null
  readonly donatedAt: number | null
  readonly idempotencyKey: string | null
}

export type CreateManualDonationResult =
  | {
    readonly ok: true
    readonly record: DonationRecord
    readonly counts: AdminVisibilityCounts
    readonly replayed: boolean
  }
  | { readonly ok: false; readonly code: 'duplicate_external_id' }
  | {
    readonly ok: false
    readonly code: 'idempotency_key_conflict'
    readonly existingDonationId: string
  }

export interface AdminVisibilityCounts {
  readonly total: number
  readonly published: number
  readonly pending_review: number
  readonly below_threshold: number
  readonly hidden: number
  readonly private_donor: number
}

export interface AdminDonationsFilter {
  readonly visibility?: VisibilityKind
  readonly provider?: DonationProvider
  readonly query?: string
}

export interface AdminDonationsCursor {
  readonly donatedAt: number
  readonly id: string
}

export interface AdminDonationsPage {
  readonly items: readonly DonationRecord[]
  readonly nextCursor: string | null
  readonly config: DonationConfig
  readonly counts: AdminVisibilityCounts
}

const ADMIN_CURSOR_PATTERN = /^(\d+):([A-Za-z0-9_-]+)$/

export function encodeAdminCursor(cursor: AdminDonationsCursor): string {
  return `${cursor.donatedAt}:${cursor.id}`
}

export function decodeAdminCursor(raw: string | null): AdminDonationsCursor | null {
  if (raw === null) return null
  const match = ADMIN_CURSOR_PATTERN.exec(raw.trim())
  if (match === null) return null
  const donatedAt = Number(match[1])
  const id = match[2] ?? ''
  if (!Number.isSafeInteger(donatedAt) || id.length === 0) return null
  return { donatedAt, id }
}

export function emptyVisibilityCounts(): AdminVisibilityCounts {
  return {
    total: 0,
    published: 0,
    pending_review: 0,
    below_threshold: 0,
    hidden: 0,
    private_donor: 0,
  }
}

export interface AdminSessionRecord {
  readonly csrfToken: string
  readonly expiresAt: number
}

export function defaultDonationConfig(): DonationConfig {
  return {
    minPublicCnyMinor: MIN_PUBLIC_CNY_MINOR,
    minPublicUsdMinor: MIN_PUBLIC_USD_MINOR,
    moderationEnabled: false,
    cryptoCheckoutPaused: false,
  }
}

export function clampDonatedAt(candidateSec: number | null, nowSec: number): number {
  if (candidateSec === null || !Number.isFinite(candidateSec) || candidateSec <= 0) {
    return nowSec
  }
  const normalized = Math.floor(candidateSec)
  const maxAllowed = nowSec + MAX_FUTURE_SKEW_SECONDS
  return normalized > maxAllowed ? maxAllowed : normalized
}

export function applyVisibilityCommand(
  current: WallVisibility,
  command: AdminVisibilityCommand,
  nowSec: number,
): VisibilityTransitionResult {
  if (current.kind === 'private_donor') {
    // Private donor preference is an immutable terminal state and can never become public.
    return { ok: false, code: 'private_donor_immutable' }
  }

  switch (command.action) {
    case 'approve':
      if (current.kind !== 'pending_review' && current.kind !== 'below_threshold') {
        return { ok: false, code: 'invalid_transition' }
      }
      return { ok: true, next: { kind: 'published' } }
    case 'hide':
      if (current.kind === 'hidden') {
        return { ok: false, code: 'invalid_transition' }
      }
      return {
        ok: true,
        next: {
          kind: 'hidden',
          moderatedAt: nowSec,
          reason: command.reason,
          previous: current,
        },
      }
    case 'restore':
      if (current.kind !== 'hidden') {
        return { ok: false, code: 'invalid_transition' }
      }
      return { ok: true, next: current.previous }
  }
}

export function projectPublicWallEntry(record: DonationRecord): PublicWallEntry {
  return {
    id: record.id,
    provider: record.provider,
    displayName: record.displayName,
    amountMinor: record.amountMinor,
    currency: record.currency,
    minorDigits: record.minorDigits,
    message: record.message,
    donatedAt: record.donatedAt,
  }
}
