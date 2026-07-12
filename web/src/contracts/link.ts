declare const shareIdBrand: unique symbol
declare const readSecretBrand: unique symbol
declare const relayHintBrand: unique symbol

export const CIPHER_SUITE_V1 = 0x01 as const
export const SHARE_ID_BYTES = 9
export const SHARE_ID_BASE64URL_CHARACTERS = 12
export const READ_SECRET_BYTES = 16

export type CipherSuite = typeof CIPHER_SUITE_V1

/** A base64url identifier whose 12 characters decode to exactly nine bytes. */
export type ShareId = string & { readonly [shareIdBrand]: 'ShareId' }

/**
 * A validated 16-byte read secret.
 *
 * The brand records validation, not immutability: consumers retaining these
 * bytes across an asynchronous boundary must take their own snapshot.
 */
export type ReadSecret = Uint8Array & { readonly [readSecretBrand]: 'ReadSecret' }

/** A normalized relay URL accepted by the capability-link parser. */
export type RelayHint = string & { readonly [relayHintBrand]: 'RelayHint' }

export interface CapabilityLink {
  readonly suite: CipherSuite
  readonly shareId: ShareId
  readonly readSecret: ReadSecret
  /** Order is authoritative because it is the caller's connection preference. */
  readonly relayHints: readonly RelayHint[]
}
