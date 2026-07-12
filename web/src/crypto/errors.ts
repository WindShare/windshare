export type CryptoErrorCode =
  | 'authentication-failed'
  | 'block-index-out-of-range'
  | 'block-too-long'
  | 'block-too-short'
  | 'digest-failed'
  | 'invalid-chunk-size'
  | 'invalid-key-material'
  | 'key-conflict'
  | 'key-derivation-failed'
  | 'malformed-key'
  | 'malformed-link'
  | 'malformed-share-id'
  | 'missing-key'
  | 'unsupported-suite'
  | 'webcrypto-unavailable'

export class CryptoError extends Error {
  readonly code: CryptoErrorCode

  constructor(code: CryptoErrorCode, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'CryptoError'
    this.code = code
  }
}
