import { concatBytes, copyBytes } from './bytes'
import { CryptoError } from './errors'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export const SUITE02_DERIVED_KEY_BYTES = 32
export const SUITE02_READ_SECRET_BYTES = 16
export const SUITE02_IDENTITY_BYTES = 16
export const SUITE02_PK_HASH_BYTES = 16

export const SUITE02_DESCRIPTOR_LABEL = 'windshare/v2 descriptor' as const
export const SUITE02_SESSION_AUTH_LABEL = 'windshare/v2 session-auth' as const

const TEXT_ENCODER = new TextEncoder()
const EMPTY_SALT = new Uint8Array(0)

function requireLength(value: Uint8Array, length: number, label: string): void {
  if (value.byteLength !== length) {
    throw new CryptoError(
      'invalid-key-material',
      `${label} must be exactly ${length} bytes`,
    )
  }
}

async function derive(
  secret: Uint8Array,
  label: string,
  context: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const info = concatBytes([TEXT_ENCODER.encode(label), Uint8Array.of(0), context])
  try {
    const material = await runtime.subtle.importKey('raw', copyBytes(secret), 'HKDF', false, [
      'deriveBits',
    ])
    const bits = await runtime.subtle.deriveBits(
      {
        name: 'HKDF',
        hash: 'SHA-256',
        salt: EMPTY_SALT,
        info,
      },
      material,
      SUITE02_DERIVED_KEY_BYTES * 8,
    )
    return new Uint8Array(bits)
  } catch (cause) {
    throw new CryptoError('key-derivation-failed', 'Suite-02 HKDF-SHA256 derivation failed', {
      cause,
    })
  }
}

export async function deriveSuite02DescriptorKey(
  readSecret: Uint8Array,
  pkHash: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, SUITE02_READ_SECRET_BYTES, 'read secret')
  requireLength(pkHash, SUITE02_PK_HASH_BYTES, 'sender public-key hash')
  return derive(readSecret, SUITE02_DESCRIPTOR_LABEL, pkHash, runtime)
}

export async function deriveSuite02SessionAuthKey(
  readSecret: Uint8Array,
  shareInstance: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, SUITE02_READ_SECRET_BYTES, 'read secret')
  requireLength(shareInstance, SUITE02_IDENTITY_BYTES, 'share instance')
  return derive(readSecret, SUITE02_SESSION_AUTH_LABEL, shareInstance, runtime)
}
