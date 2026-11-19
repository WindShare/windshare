import { concatBytes, copyBytes, equalBytes } from './bytes'
import { sha256 } from './digest'
import type { Ed25519Verifier } from './ed25519'
import { SUITE02_DERIVED_KEY_BYTES, SUITE02_PK_HASH_BYTES } from './suite02-key-derivation'
import {
  SUITE02,
  SUITE02_SHARE_ID_BYTES,
  suite02SenderKeyHash,
  suite02ShareIdRaw,
  type Suite02CapabilityKey,
} from './suite02-link'
import { type CryptoRuntime, defaultCryptoRuntime, importAesGcmKey } from './webcrypto'

export const DESCRIPTOR_OBJECT_BYTES = 16 << 10

// Descriptor signature revisions are independent of the session wire and suite-02 keys.
const WIRE_VERSION = 3
const HEADER_BYTES = 8
const NONCE_BYTES = 12
const SIGNATURE_BYTES = 64
const TAG_BYTES = 16
const PREFIX_BYTES = HEADER_BYTES + NONCE_BYTES
const FIXED_BYTES = PREFIX_BYTES + SIGNATURE_BYTES
const DESCRIPTOR_DOMAIN = new TextEncoder().encode('windshare/v2 object/descriptor\0')

export type SenderObjectErrorKind =
  | 'authentication'
  | 'binding'
  | 'key'
  | 'malformed'
  | 'signature'
  | 'too-large'

export class SenderObjectError extends Error {
  readonly kind: SenderObjectErrorKind

  constructor(kind: SenderObjectErrorKind, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'SenderObjectError'
    this.kind = kind
  }
}

interface DescriptorEnvelope {
  readonly header: Uint8Array<ArrayBuffer>
  readonly nonce: Uint8Array<ArrayBuffer>
  readonly ciphertextAndTag: Uint8Array<ArrayBuffer>
  readonly prefix: Uint8Array<ArrayBuffer>
  readonly signature: Uint8Array<ArrayBuffer>
}

function parseDescriptorObject(input: Uint8Array): DescriptorEnvelope {
  if (input.byteLength > DESCRIPTOR_OBJECT_BYTES) {
    throw new SenderObjectError('too-large', 'descriptor object exceeds its byte limit')
  }
  const object = copyBytes(input)
  if (object.byteLength < FIXED_BYTES + TAG_BYTES) {
    throw new SenderObjectError('malformed', 'descriptor object is truncated')
  }
  if (object[0] !== WIRE_VERSION || object[1] !== 0 || object[2] !== 0 || object[3] !== 0) {
    throw new SenderObjectError('malformed', 'descriptor object header is invalid')
  }

  const ciphertextLength = new DataView(object.buffer).getUint32(4, false)
  const prefixEnd = PREFIX_BYTES + ciphertextLength
  if (ciphertextLength < TAG_BYTES || prefixEnd + SIGNATURE_BYTES !== object.byteLength) {
    throw new SenderObjectError('malformed', 'descriptor object length is invalid')
  }

  return {
    header: object.subarray(0, HEADER_BYTES),
    nonce: object.subarray(HEADER_BYTES, PREFIX_BYTES),
    ciphertextAndTag: object.subarray(PREFIX_BYTES, prefixEnd),
    prefix: object.subarray(0, prefixEnd),
    signature: object.subarray(prefixEnd),
  }
}

async function decryptDescriptor(
  envelope: DescriptorEnvelope,
  keyBytes: Uint8Array,
  aad: Uint8Array<ArrayBuffer>,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  let plaintext: ArrayBuffer
  try {
    const key = await importAesGcmKey(keyBytes, runtime)
    plaintext = await runtime.subtle.decrypt(
      {
        name: 'AES-GCM',
        iv: envelope.nonce,
        additionalData: aad,
        tagLength: TAG_BYTES * 8,
      },
      key,
      envelope.ciphertextAndTag,
    )
  } catch (cause) {
    throw new SenderObjectError('authentication', 'descriptor ciphertext is invalid', { cause })
  }
  if (plaintext.byteLength === 0) {
    throw new SenderObjectError('malformed', 'descriptor plaintext is empty')
  }
  return new Uint8Array(plaintext)
}

export async function openDescriptorObjectBootstrap(
  route: Pick<Suite02CapabilityKey, 'pkHash' | 'shareIdRaw'>,
  key: Uint8Array,
  object: Uint8Array,
  senderIdentity: (plaintext: Uint8Array) => Ed25519Verifier,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (route.pkHash.byteLength !== SUITE02_PK_HASH_BYTES ||
      route.shareIdRaw.byteLength !== SUITE02_SHARE_ID_BYTES) {
    throw new SenderObjectError('binding', 'descriptor route identity has an invalid width')
  }
  if (key.byteLength !== SUITE02_DERIVED_KEY_BYTES) {
    throw new SenderObjectError('key', 'descriptor key must be 32 bytes')
  }

  // Decryption and verification must use the same snapshot across WebCrypto awaits.
  const envelope = parseDescriptorObject(object)
  const pkHash = copyBytes(route.pkHash)
  const shareIdRaw = copyBytes(route.shareIdRaw)
  const ownedKey = copyBytes(key)
  try {
    const context = concatBytes([Uint8Array.of(SUITE02), pkHash, shareIdRaw])
    const [expectedShareId, bindingHash, objectHash] = await Promise.all([
      suite02ShareIdRaw(pkHash, runtime),
      sha256(context, runtime),
      sha256(envelope.prefix, runtime),
    ])
    if (!equalBytes(expectedShareId, shareIdRaw)) {
      throw new SenderObjectError('binding', 'descriptor share ID does not match pkHash')
    }

    const domain = concatBytes([DESCRIPTOR_DOMAIN, bindingHash])
    const aad = concatBytes([domain, envelope.header])
    // The descriptor carries the public key, so decryption precedes verification.
    const plaintext = await decryptDescriptor(envelope, ownedKey, aad, runtime)
    const sender = senderIdentity(plaintext)
    if (!equalBytes(await suite02SenderKeyHash(sender.publicKey, runtime), pkHash)) {
      throw new SenderObjectError('signature', 'descriptor sender key does not match link pkHash')
    }

    let valid: boolean
    try {
      valid = await sender.verify(concatBytes([domain, objectHash]), envelope.signature)
    } catch (cause) {
      throw new SenderObjectError('key', 'Unable to verify Ed25519 sender signature', { cause })
    }
    if (!valid) throw new SenderObjectError('signature', 'descriptor sender signature is invalid')
    return plaintext
  } finally {
    ownedKey.fill(0)
  }
}
