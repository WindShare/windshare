import { concatBytes, copyBytes } from './bytes'
import { CryptoError } from './errors'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export const SUITE02_DERIVED_KEY_BYTES = 32
export const SUITE02_READ_SECRET_BYTES = 16
export const SUITE02_IDENTITY_BYTES = 16
export const SUITE02_PK_HASH_BYTES = 16

export const SUITE02_DESCRIPTOR_LABEL = 'windshare/v2 descriptor' as const
export const SUITE02_CATALOG_LABEL = 'windshare/v2 catalog' as const
export const SUITE02_FILE_OBJECT_LABEL = 'windshare/v2 file-object' as const
export const SUITE02_REVISION_LABEL = 'windshare/v2 file-revision' as const
export const SUITE02_FILE_SEGMENT_LABEL = 'windshare/v2 file-segment' as const
export const SUITE02_SESSION_AUTH_LABEL = 'windshare/v2 session-auth' as const

const TEXT_ENCODER = new TextEncoder()
const EMPTY_SALT = new Uint8Array(0)
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

function requireLength(value: Uint8Array, length: number, label: string): void {
  if (value.byteLength !== length) {
    throw new CryptoError(
      'invalid-key-material',
      `${label} must be exactly ${length} bytes`,
    )
  }
}

function encodeUint64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < 0n || value > MAX_UINT64) {
    throw new CryptoError('invalid-key-material', 'segment must be an unsigned 64-bit integer')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, value, false)
  return encoded
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

export async function deriveSuite02CatalogKey(
  readSecret: Uint8Array,
  shareInstance: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, SUITE02_READ_SECRET_BYTES, 'read secret')
  requireLength(shareInstance, SUITE02_IDENTITY_BYTES, 'share instance')
  return derive(readSecret, SUITE02_CATALOG_LABEL, shareInstance, runtime)
}

export async function deriveSuite02FileObjectKey(
  readSecret: Uint8Array,
  shareInstance: Uint8Array,
  fileId: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(readSecret, SUITE02_READ_SECRET_BYTES, 'read secret')
  requireLength(shareInstance, SUITE02_IDENTITY_BYTES, 'share instance')
  requireLength(fileId, SUITE02_IDENTITY_BYTES, 'file ID')
  return derive(
    readSecret,
    SUITE02_FILE_OBJECT_LABEL,
    concatBytes([shareInstance, fileId]),
    runtime,
  )
}

export async function deriveSuite02RevisionKey(
  fileObjectKey: Uint8Array,
  fileRevision: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(fileObjectKey, SUITE02_DERIVED_KEY_BYTES, 'file-object key')
  requireLength(fileRevision, SUITE02_IDENTITY_BYTES, 'file revision')
  return derive(fileObjectKey, SUITE02_REVISION_LABEL, fileRevision, runtime)
}

export async function deriveSuite02FileSegmentKey(
  revisionKey: Uint8Array,
  segment: bigint,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  requireLength(revisionKey, SUITE02_DERIVED_KEY_BYTES, 'revision key')
  return derive(revisionKey, SUITE02_FILE_SEGMENT_LABEL, encodeUint64(segment), runtime)
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
