export type KnownDonationProvider = 'afdian' | 'kofi' | 'nowpayments' | 'direct'
export type DonationProvider = KnownDonationProvider | (string & {})
export type TopSupporterRank = 1 | 2 | 3

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

export interface CryptoIntentRequest {
  readonly displayName: string | null
  readonly message: string | null
  readonly amountUsd: string
}

export interface CryptoIntentResponse {
  readonly intentId: string
  readonly invoiceUrl: string
}
