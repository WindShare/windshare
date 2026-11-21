export const MIN_PUBLIC_CNY_MINOR = 1000
export const MIN_PUBLIC_USD_MINOR = 500
export const PUBLIC_WALL_LIMIT = 50
export const CRYPTO_INTENT_TTL_SECONDS = 7 * 24 * 60 * 60
export const MAX_DISPLAY_NAME_CODEPOINTS = 32
export const MAX_MESSAGE_CODEPOINTS = 140

export type DonationProvider = 'afdian' | 'kofi' | 'nowpayments'

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
  readonly moderationEnabled: boolean
  readonly entries: readonly PublicWallEntry[]
}

export interface DonationConfig {
  readonly minPublicCnyMinor: number
  readonly minPublicUsdMinor: number
  readonly moderationEnabled: boolean
  readonly cryptoCheckoutPaused: boolean
}

export type AdminVisibilityCommand =
  | { readonly action: 'approve' }
  | { readonly action: 'hide'; readonly reason: string }
  | { readonly action: 'restore' }

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
