declare const canonicalPathBrand: unique symbol
declare const byteLengthBrand: unique symbol
declare const chunkSizeBrand: unique symbol
declare const unixMillisecondsBrand: unique symbol
declare const manifestFingerprintBrand: unique symbol
declare const validatedManifestBrand: unique symbol

export const MANIFEST_VERSION = 1 as const
export const PATH_POLICY_VERSION = 'windshare/path/v1-unicode-15.0.0' as const

export const MIN_CHUNK_BYTES = 1_024
export const MAX_CHUNK_BYTES = 4 * 1_024 * 1_024
export const MAX_STREAM_BYTES = 2 ** 48
export const MAX_SEALED_MANIFEST_BYTES = 16 * 1_024 * 1_024
export const MANIFEST_FINGERPRINT_BYTES = 16
export const MAX_MTIME_MILLISECONDS = Number.MAX_SAFE_INTEGER
export const MIN_MTIME_MILLISECONDS = -MAX_MTIME_MILLISECONDS

export type ManifestVersion = typeof MANIFEST_VERSION
export type PathPolicyVersion = typeof PATH_POLICY_VERSION

/** A path already normalized and accepted under PATH_POLICY_VERSION. */
export type CanonicalPath = string & { readonly [canonicalPathBrand]: 'CanonicalPath' }

/** A non-negative integer already checked against the shared 2^48 ceiling. */
export type ByteLength = number & { readonly [byteLengthBrand]: 'ByteLength' }

/** A power-of-two byte length inside the normative chunk-size interval. */
export type ChunkSize = ByteLength & { readonly [chunkSizeBrand]: 'ChunkSize' }

/** Unix epoch milliseconds already checked for exact JavaScript representation. */
export type UnixMilliseconds = number & {
  readonly [unixMillisecondsBrand]: 'UnixMilliseconds'
}

/**
 * The complete 16-byte GCM tag from a structurally valid sealed manifest.
 * Producers must snapshot the tag before branding it.
 */
export type ManifestFingerprint = Uint8Array & {
  readonly [manifestFingerprintBrand]: 'ManifestFingerprint'
}

interface ManifestEntryBase {
  readonly path: CanonicalPath
  readonly mtime: UnixMilliseconds
}

export interface FileManifestEntry extends ManifestEntryBase {
  readonly kind: 'file'
  readonly size: ByteLength
}

export interface DirectoryManifestEntry extends ManifestEntryBase {
  readonly kind: 'directory'
}

export type ManifestEntry = FileManifestEntry | DirectoryManifestEntry

/**
 * A version-one manifest after strict schema checks, path policy, geometry, and
 * byte-for-byte RFC 8949 deterministic re-encoding have all succeeded.
 *
 * The private brand keeps a decoded `unknown` object from becoming a trusted
 * manifest merely because it happens to have similarly named properties.
 */
export interface ValidatedManifestV1 {
  readonly [validatedManifestBrand]: true
  readonly version: ManifestVersion
  readonly chunkSize: ChunkSize
  /** Authenticated array order is the packed-stream order and must be preserved. */
  readonly entries: readonly ManifestEntry[]
}
