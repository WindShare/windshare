import {
  CIPHER_SUITE_V1,
  MANIFEST_FINGERPRINT_BYTES,
  MAX_SEALED_MANIFEST_BYTES,
  type CapabilityLink,
  type ManifestFingerprint,
  type ValidatedManifestV1,
} from '../contracts'
import { copyBytes } from '../crypto/bytes'
import { deriveManifestKey } from '../crypto/key-derivation'
import {
  type CryptoRuntime,
  defaultCryptoRuntime,
  importAesGcmKey,
} from '../crypto/webcrypto'
import { decodeCanonicalManifest } from './cbor'
import { ManifestError } from './errors'

export const MANIFEST_NONCE_BYTES = 12

export interface OpenedManifest {
  readonly manifest: ValidatedManifestV1
  readonly fingerprint: ManifestFingerprint
}

export async function openSealedManifest(
  suite: number,
  readSecret: Uint8Array,
  sealedManifest: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<OpenedManifest> {
  if (suite !== CIPHER_SUITE_V1) {
    throw new ManifestError(
      'unsupported-suite',
      'Manifest uses an unsupported cipher suite; upgrade required',
    )
  }
  if (sealedManifest.byteLength > MAX_SEALED_MANIFEST_BYTES) {
    throw new ManifestError(
      'manifest-too-large',
      `Sealed manifest exceeds the ${MAX_SEALED_MANIFEST_BYTES}-byte ceiling`,
    )
  }
  const minimumBytes = MANIFEST_NONCE_BYTES + MANIFEST_FINGERPRINT_BYTES
  if (sealedManifest.byteLength < minimumBytes) {
    throw new ManifestError(
      'sealed-manifest-too-short',
      `Sealed manifest must be at least ${minimumBytes} bytes`,
    )
  }

  const snapshot = copyBytes(sealedManifest)
  const manifestKey = await deriveManifestKey(readSecret, runtime)
  const key = await importAesGcmKey(manifestKey, runtime)
  const nonce = snapshot.subarray(0, MANIFEST_NONCE_BYTES)
  const ciphertext = snapshot.subarray(MANIFEST_NONCE_BYTES)
  let plaintext: ArrayBuffer
  try {
    plaintext = await runtime.subtle.decrypt(
      {
        name: 'AES-GCM',
        iv: nonce,
        additionalData: Uint8Array.of(suite),
        tagLength: MANIFEST_FINGERPRINT_BYTES * 8,
      },
      key,
      ciphertext,
    )
  } catch (cause) {
    throw new ManifestError(
      'manifest-authentication-failed',
      'Manifest failed AES-GCM authentication',
      { cause },
    )
  }

  const manifest = decodeCanonicalManifest(new Uint8Array(plaintext))
  const fingerprint = snapshot.slice(
    snapshot.byteLength - MANIFEST_FINGERPRINT_BYTES,
  ) as ManifestFingerprint
  return Object.freeze({ manifest, fingerprint })
}

export async function openCapabilityManifest(
  link: CapabilityLink,
  sealedManifest: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<OpenedManifest> {
  return openSealedManifest(link.suite, link.readSecret, sealedManifest, runtime)
}
