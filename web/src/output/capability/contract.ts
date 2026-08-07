/**
 * Metadata carried by an acquired browser output capability.  The authority
 * authenticates this descriptor against the frozen transfer intent before it
 * gives the capability to an output session.  Keeping the descriptor beside
 * the capability prevents a caller from reconstructing a locator from a
 * backend kind after the picker has returned.
 */
export type OutputCapabilityFormat = 'directory' | 'single-file' | 'zip'

export type OutputCapabilityTargetKind = 1 | 2

/** A 32-byte, unpadded base64url-compatible identity held as bytes in memory. */
export type OutputCapabilityIdentity = Uint8Array<ArrayBuffer>

export interface OutputCapabilityMetadata {
  readonly rootIdentity: OutputCapabilityIdentity
  readonly targetKind: OutputCapabilityTargetKind
  readonly backend: string
  readonly format: OutputCapabilityFormat
}

export const FILE_SYSTEM_ACCESS_BACKEND = 'file-system-access'
export const ORIGIN_PRIVATE_BACKEND = 'origin-private-staging'
export const SINGLE_FILE_STREAM_BACKEND = 'single-file-stream'
export const ZIP_STREAM_BACKEND = 'zip-stream'

export const OUTPUT_CAPABILITY_IDENTITY_BYTES = 32

/**
 * Copy and validate identity bytes at the capability boundary.  A copy is
 * intentional: WebCrypto and test fixtures often reuse their scratch buffer,
 * while the capability must retain the exact bytes selected by the picker.
 */
export function snapshotOutputCapabilityIdentity(
  value: Uint8Array<ArrayBufferLike>,
  label = 'output capability identity',
): OutputCapabilityIdentity {
  if (!(value instanceof Uint8Array)) {
    throw new TypeError(`${label} must be a Uint8Array`)
  }
  if (value.byteLength !== OUTPUT_CAPABILITY_IDENTITY_BYTES) {
    throw new TypeError(`${label} must be exactly 32 bytes`)
  }
  const copy = new Uint8Array(OUTPUT_CAPABILITY_IDENTITY_BYTES)
  copy.set(value)
  if (copy.every((byte) => byte === 0)) {
    throw new TypeError(`${label} must not be all zeroes`)
  }
  return copy
}

/** Generate an identity for a non-durable stream target. */
export function createOutputCapabilityIdentity(): OutputCapabilityIdentity {
  const bytes = new Uint8Array(OUTPUT_CAPABILITY_IDENTITY_BYTES)
  const cryptoSource = globalThis.crypto
  if (cryptoSource?.getRandomValues === undefined) {
    throw new DOMException('Secure output capability identity generation is unavailable', 'NotSupportedError')
  }
  cryptoSource.getRandomValues(bytes)
  return snapshotOutputCapabilityIdentity(bytes)
}

export function sameOutputCapabilityIdentity(
  left: Uint8Array<ArrayBufferLike>,
  right: Uint8Array<ArrayBufferLike>,
): boolean {
  if (left.byteLength !== right.byteLength) return false
  let different = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    different |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return different === 0
}
