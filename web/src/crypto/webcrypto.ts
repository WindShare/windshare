import { copyBytes } from './bytes'
import { CryptoError } from './errors'

export const AES_256_KEY_BYTES = 32

export interface CryptoRuntime {
  readonly subtle: SubtleCrypto
}

export function defaultCryptoRuntime(): CryptoRuntime {
  const subtle = globalThis.crypto?.subtle
  if (subtle === undefined) {
    throw new CryptoError('webcrypto-unavailable', 'WebCrypto is not available in this runtime')
  }
  return { subtle }
}

export async function importAesGcmKey(
  rawKey: Uint8Array,
  runtime: CryptoRuntime,
): Promise<CryptoKey> {
  if (rawKey.byteLength !== AES_256_KEY_BYTES) {
    throw new CryptoError(
      'invalid-key-material',
      `AES-256 key must be ${AES_256_KEY_BYTES} bytes`,
    )
  }
  try {
    return await runtime.subtle.importKey('raw', copyBytes(rawKey), 'AES-GCM', false, [
      'decrypt',
    ])
  } catch (cause) {
    throw new CryptoError('invalid-key-material', 'Unable to import AES-256 key', { cause })
  }
}
