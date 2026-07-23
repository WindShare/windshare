import { concatBytes, decodeBase64Url, encodeBase64Url, equalBytes } from './bytes'
import { CryptoError } from './errors'
import { sha256 } from './digest'
import {
  SUITE02_PK_HASH_BYTES,
  SUITE02_READ_SECRET_BYTES,
} from './suite02-key-derivation'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export const SUITE02 = 0x02
export const SUITE02_SHARE_ID_BYTES = 12
export const SUITE02_SHARE_ID_CHARACTERS = 16
export const SUITE02_KEY_BYTES = 1 + SUITE02_READ_SECRET_BYTES + SUITE02_PK_HASH_BYTES
export const SUITE02_KEY_CHARACTERS = 44

const TEXT_ENCODER = new TextEncoder()
const SENDER_KEY_DOMAIN = TEXT_ENCODER.encode('windshare/v2 sender-key\0')
const SHARE_ID_DOMAIN = TEXT_ENCODER.encode('windshare/v2 share-id\0')

export interface Suite02CapabilityKey {
  readonly suite: typeof SUITE02
  readonly readSecret: Uint8Array<ArrayBuffer>
  readonly pkHash: Uint8Array<ArrayBuffer>
  readonly shareIdRaw: Uint8Array<ArrayBuffer>
  readonly shareId: string
}

export interface Suite02CapabilityLink extends Suite02CapabilityKey {
  readonly relayHints: readonly string[]
}

function keyPayload(input: string): string {
  const trimmed = input.trim()
  const fragment = trimmed.indexOf('#')
  return (fragment === -1 ? trimmed : trimmed.slice(fragment + 1)).trim()
}

async function shareIdentity(
  pkHash: Uint8Array,
  runtime: CryptoRuntime,
): Promise<{ readonly raw: Uint8Array<ArrayBuffer>; readonly text: string }> {
  if (pkHash.byteLength !== SUITE02_PK_HASH_BYTES) {
    throw new CryptoError('malformed-key', 'Suite-02 pkHash must be exactly 16 bytes')
  }
  const digest = await sha256(concatBytes([SHARE_ID_DOMAIN, pkHash]), runtime)
  const raw = digest.slice(0, SUITE02_SHARE_ID_BYTES)
  return { raw, text: encodeBase64Url(raw) }
}

export async function suite02SenderKeyHash(
  senderPublicKey: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (senderPublicKey.byteLength !== 32) {
    throw new CryptoError('invalid-key-material', 'Ed25519 sender public key must be 32 bytes')
  }
  const digest = await sha256(concatBytes([SENDER_KEY_DOMAIN, senderPublicKey]), runtime)
  return digest.slice(0, SUITE02_PK_HASH_BYTES)
}

export async function decodeSuite02CapabilityKey(
  input: string,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Suite02CapabilityKey> {
  const encoded = keyPayload(input)
  const raw = decodeBase64Url(encoded)
  if (
    encoded.length !== SUITE02_KEY_CHARACTERS ||
    raw?.byteLength !== SUITE02_KEY_BYTES ||
    raw[0] !== SUITE02
  ) {
    throw new CryptoError(
      'malformed-key',
      'Suite-02 capability key must be 44 canonical unpadded base64url characters',
    )
  }
  const readSecret = raw.slice(1, 1 + SUITE02_READ_SECRET_BYTES)
  const pkHash = raw.slice(1 + SUITE02_READ_SECRET_BYTES)
  const share = await shareIdentity(pkHash, runtime)
  return Object.freeze({
    suite: SUITE02,
    readSecret,
    pkHash,
    shareIdRaw: share.raw,
    shareId: share.text,
  })
}

export async function encodeSuite02CapabilityKey(
  readSecret: Uint8Array,
  pkHash: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Suite02CapabilityKey & { readonly encoded: string }> {
  if (
    readSecret.byteLength !== SUITE02_READ_SECRET_BYTES ||
    pkHash.byteLength !== SUITE02_PK_HASH_BYTES
  ) {
    throw new CryptoError('invalid-key-material', 'Suite-02 capability key material has invalid width')
  }
  const raw = concatBytes([Uint8Array.of(SUITE02), readSecret, pkHash])
  const share = await shareIdentity(pkHash, runtime)
  return Object.freeze({
    suite: SUITE02,
    readSecret: readSecret.slice(),
    pkHash: pkHash.slice(),
    shareIdRaw: share.raw,
    shareId: share.text,
    encoded: encodeBase64Url(raw),
  })
}

export async function parseSuite02CapabilityLink(
  input: string,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Suite02CapabilityLink> {
  const trimmed = input.trim()
  if (trimmed.includes('\\')) {
    throw new CryptoError('malformed-link', 'Suite-02 capability URL contains a backslash')
  }
  let url: URL
  try {
    url = new URL(trimmed)
  } catch (cause) {
    throw new CryptoError('malformed-link', 'Suite-02 capability URL is invalid', { cause })
  }
  if (url.protocol === '' || url.host === '' || url.hash.length <= 1) {
    throw new CryptoError('malformed-link', 'Suite-02 capability URL is incomplete')
  }
  const capability = await decodeSuite02CapabilityKey(url.hash.slice(1), runtime)
  let path = url.pathname
  while (path.endsWith('/')) path = path.slice(0, -1)
  const lastSlash = path.lastIndexOf('/')
  let shareId: string
  try {
    shareId = decodeURIComponent(path.slice(lastSlash + 1))
  } catch (cause) {
    throw new CryptoError('malformed-share-id', 'Suite-02 share ID is not valid UTF-8', {
      cause,
    })
  }
  const decodedShareId = decodeBase64Url(shareId)
  if (
    shareId.length !== SUITE02_SHARE_ID_CHARACTERS ||
    decodedShareId?.byteLength !== SUITE02_SHARE_ID_BYTES ||
    !equalBytes(decodedShareId, capability.shareIdRaw)
  ) {
    throw new CryptoError(
      'malformed-share-id',
      'Suite-02 URL route is not the deterministic image of its fragment pkHash',
    )
  }
  return Object.freeze({
    ...capability,
    relayHints: Object.freeze(url.searchParams.getAll('r')),
  })
}
