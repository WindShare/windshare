import { READ_SECRET_BYTES } from '../contracts'
import { concatBytes, copyBytes, encodeUint32 } from './bytes'
import { CryptoError } from './errors'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export const DERIVED_KEY_BYTES = 32
export const MANIFEST_KEY_LABEL = 'windshare/v1 manifest' as const
export const STREAM_KEY_LABEL = 'windshare/v1 stream' as const
export const SEGMENT_KEY_LABEL = 'windshare/v1 seg' as const

const encoder = new TextEncoder()
const EMPTY_SALT = new Uint8Array(0)
const MAX_SEGMENT_NUMBER = 0xffff_ffff

function requireLength(bytes: Uint8Array, length: number, label: string): void {
  if (bytes.byteLength !== length) {
    throw new CryptoError(
      'invalid-key-material',
      `${label} must be exactly ${length} bytes`,
    )
  }
}

async function deriveKey(
  secret: Uint8Array,
  info: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const secretSnapshot = copyBytes(secret)
  try {
    const key = await runtime.subtle.importKey('raw', secretSnapshot, 'HKDF', false, [
      'deriveBits',
    ])
    const bits = await runtime.subtle.deriveBits(
      {
        name: 'HKDF',
        hash: 'SHA-256',
        salt: EMPTY_SALT,
        info: copyBytes(info),
      },
      key,
      DERIVED_KEY_BYTES * 8,
    )
    return new Uint8Array(bits)
  } catch (cause) {
    throw new CryptoError('key-derivation-failed', 'HKDF-SHA256 derivation failed', {
      cause,
    })
  }
}

export async function deriveManifestKey(
  readSecret: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, READ_SECRET_BYTES, 'read secret')
  return deriveKey(readSecret, encoder.encode(MANIFEST_KEY_LABEL), runtime)
}

export async function deriveStreamKey(
  readSecret: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, READ_SECRET_BYTES, 'read secret')
  return deriveKey(readSecret, encoder.encode(STREAM_KEY_LABEL), runtime)
}

export async function deriveSegmentKey(
  streamKey: Uint8Array,
  segment: number,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(streamKey, DERIVED_KEY_BYTES, 'stream key')
  if (!Number.isSafeInteger(segment) || segment < 0 || segment > MAX_SEGMENT_NUMBER) {
    throw new CryptoError(
      'invalid-key-material',
      `segment number must be an integer in [0, ${MAX_SEGMENT_NUMBER}]`,
    )
  }
  const info = concatBytes([encoder.encode(SEGMENT_KEY_LABEL), encodeUint32(segment)])
  return deriveKey(streamKey, info, runtime)
}
