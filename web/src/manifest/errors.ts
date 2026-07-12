export type ManifestErrorCode =
  | 'duplicate-path'
  | 'invalid-cbor'
  | 'invalid-path'
  | 'chunk-size-not-power-of-two'
  | 'chunk-size-too-large'
  | 'chunk-size-too-small'
  | 'manifest-authentication-failed'
  | 'manifest-too-large'
  | 'mtime-out-of-range'
  | 'negative-stream'
  | 'negative-size'
  | 'non-canonical'
  | 'path-collision'
  | 'path-type-conflict'
  | 'schema-mismatch'
  | 'sealed-manifest-too-short'
  | 'stream-too-large'
  | 'too-many-chunks'
  | 'unknown-selector'
  | 'unsupported-suite'
  | 'unsupported-version'

export class ManifestError extends Error {
  readonly code: ManifestErrorCode

  constructor(code: ManifestErrorCode, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'ManifestError'
    this.code = code
  }
}
