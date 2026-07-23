import { concatBytes, encodeUint32, equalBytes } from './bytes'
import { verifyEd25519Signature } from './curve25519'
import { sha256 } from './digest'
import { suite02SenderKeyHash } from './suite02-link'
import { SUITE02_DERIVED_KEY_BYTES } from './suite02-key-derivation'
import { type CryptoRuntime, defaultCryptoRuntime, importAesGcmKey } from './webcrypto'

export const SENDER_OBJECT_WIRE_VERSION = 2
export const SENDER_OBJECT_HEADER_BYTES = 8
export const SENDER_OBJECT_NONCE_BYTES = 12
export const SENDER_OBJECT_SIGNATURE_BYTES = 64
export const SENDER_OBJECT_TAG_BYTES = 16
export const SENDER_OBJECT_FIXED_BYTES =
  SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES + SENDER_OBJECT_SIGNATURE_BYTES

export const SENDER_OBJECT_DOMAIN = Object.freeze({
  descriptor: 'windshare/v2 object/descriptor',
  catalogPage: 'windshare/v2 object/catalog-page',
  directoryError: 'windshare/v2 object/directory-error',
  revision: 'windshare/v2 object/file-revision',
  blockRecord: 'windshare/v2 object/block-record',
  offlineCommit: 'windshare/v2 object/offline-commit',
} as const)

export type SenderObjectDomain = (typeof SENDER_OBJECT_DOMAIN)[keyof typeof SENDER_OBJECT_DOMAIN]

const OBJECT_LIMITS: Readonly<Record<SenderObjectDomain, number>> = Object.freeze({
  [SENDER_OBJECT_DOMAIN.descriptor]: 16 << 10,
  [SENDER_OBJECT_DOMAIN.catalogPage]: 60 << 10,
  [SENDER_OBJECT_DOMAIN.directoryError]: 16 << 10,
  [SENDER_OBJECT_DOMAIN.revision]: 16 << 10,
  [SENDER_OBJECT_DOMAIN.blockRecord]: (4 << 20) + 512,
  [SENDER_OBJECT_DOMAIN.offlineCommit]: 16 << 10,
})

const TEXT_ENCODER = new TextEncoder()
const SHARE_ID_DOMAIN = TEXT_ENCODER.encode('windshare/v2 share-id\0')
const IDENTITY_BYTES = 16
const PK_HASH_BYTES = 16
const SHARE_ID_BYTES = 12
const MAX_UINT32 = 0xffff_ffff
const MAX_UINT64 = 0xffff_ffff_ffff_ffffn

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

const BINDING_CONSTRUCTOR = Symbol('sender-object-binding')

export class SenderObjectBinding {
  readonly domain: SenderObjectDomain
  readonly maxBytes: number
  readonly #context: Uint8Array<ArrayBuffer>

  constructor(
    constructorAuthority: symbol,
    domain: SenderObjectDomain,
    context: Uint8Array,
    maxBytes: number,
  ) {
    if (constructorAuthority !== BINDING_CONSTRUCTOR) {
      throw new SenderObjectError('binding', 'sender object bindings must use a typed factory')
    }
    this.domain = domain
    this.maxBytes = maxBytes
    this.#context = context.slice()
    Object.freeze(this)
  }

  get context(): Uint8Array<ArrayBuffer> {
    return this.#context.slice()
  }
}

interface ParsedSenderObject {
  readonly header: Uint8Array<ArrayBuffer>
  readonly nonce: Uint8Array<ArrayBuffer>
  readonly ciphertextAndTag: Uint8Array<ArrayBuffer>
  readonly prefix: Uint8Array<ArrayBuffer>
  readonly signature: Uint8Array<ArrayBuffer>
}

function nonzeroIdentity(value: Uint8Array, label: string): Uint8Array<ArrayBuffer> {
  if (value.byteLength !== IDENTITY_BYTES || !value.some((item) => item !== 0)) {
    throw new SenderObjectError('binding', `${label} must be a nonzero 16-byte identity`)
  }
  return value.slice()
}

function boundedUint32(value: number, label: string): number {
  if (!Number.isInteger(value) || value < 0 || value > MAX_UINT32) {
    throw new SenderObjectError('binding', `${label} must be an unsigned 32-bit integer`)
  }
  return value
}

function encodeUint64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < 0n || value > MAX_UINT64) {
    throw new SenderObjectError('binding', 'local block index must be an unsigned 64-bit integer')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, value, false)
  return encoded
}

function binding(domain: SenderObjectDomain, context: Uint8Array): SenderObjectBinding {
  return new SenderObjectBinding(BINDING_CONSTRUCTOR, domain, context, OBJECT_LIMITS[domain])
}

export async function createDescriptorObjectBinding(
  pkHash: Uint8Array,
  shareIdRaw: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<SenderObjectBinding> {
  if (pkHash.byteLength !== PK_HASH_BYTES || shareIdRaw.byteLength !== SHARE_ID_BYTES) {
    throw new SenderObjectError('binding', 'descriptor route identity has an invalid width')
  }
  const expected = (await sha256(concatBytes([SHARE_ID_DOMAIN, pkHash]), runtime)).subarray(
    0,
    SHARE_ID_BYTES,
  )
  if (!equalBytes(expected, shareIdRaw)) {
    throw new SenderObjectError('binding', 'descriptor share ID does not match pkHash')
  }
  return binding(
    SENDER_OBJECT_DOMAIN.descriptor,
    concatBytes([Uint8Array.of(2), pkHash, shareIdRaw]),
  )
}

export function createCatalogPageObjectBinding(
  shareInstance: Uint8Array,
  directoryId: Uint8Array,
  pageIndex: number,
): SenderObjectBinding {
  return binding(
    SENDER_OBJECT_DOMAIN.catalogPage,
    concatBytes([
      nonzeroIdentity(shareInstance, 'share instance'),
      nonzeroIdentity(directoryId, 'directory ID'),
      encodeUint32(boundedUint32(pageIndex, 'page index')),
    ]),
  )
}

export function createDirectoryErrorObjectBinding(
  shareInstance: Uint8Array,
  directoryId: Uint8Array,
): SenderObjectBinding {
  return binding(
    SENDER_OBJECT_DOMAIN.directoryError,
    concatBytes([
      nonzeroIdentity(shareInstance, 'share instance'),
      nonzeroIdentity(directoryId, 'directory ID'),
    ]),
  )
}

export function createRevisionObjectBinding(
  shareInstance: Uint8Array,
  fileId: Uint8Array,
): SenderObjectBinding {
  return binding(
    SENDER_OBJECT_DOMAIN.revision,
    concatBytes([
      nonzeroIdentity(shareInstance, 'share instance'),
      nonzeroIdentity(fileId, 'file ID'),
    ]),
  )
}

export function createBlockRecordObjectBinding(
  shareInstance: Uint8Array,
  fileId: Uint8Array,
  fileRevision: Uint8Array,
  localBlockIndex: bigint,
  dataLength: number,
): SenderObjectBinding {
  return binding(
    SENDER_OBJECT_DOMAIN.blockRecord,
    concatBytes([
      nonzeroIdentity(shareInstance, 'share instance'),
      nonzeroIdentity(fileId, 'file ID'),
      nonzeroIdentity(fileRevision, 'file revision'),
      encodeUint64(localBlockIndex),
      encodeUint32(boundedUint32(dataLength, 'data length')),
    ]),
  )
}

export function createOfflineCommitObjectBinding(
  shareInstance: Uint8Array,
): SenderObjectBinding {
  return binding(
    SENDER_OBJECT_DOMAIN.offlineCommit,
    nonzeroIdentity(shareInstance, 'share instance'),
  )
}

function parseSenderObject(
  object: Uint8Array,
  expectedBinding: SenderObjectBinding,
): ParsedSenderObject {
  if (object.byteLength > expectedBinding.maxBytes) {
    throw new SenderObjectError('too-large', 'sender object exceeds its domain limit')
  }
  if (object.byteLength < SENDER_OBJECT_FIXED_BYTES + SENDER_OBJECT_TAG_BYTES) {
    throw new SenderObjectError('malformed', 'sender object is truncated')
  }
  if (
    object[0] !== SENDER_OBJECT_WIRE_VERSION ||
    object[1] !== 0 ||
    object[2] !== 0 ||
    object[3] !== 0
  ) {
    throw new SenderObjectError('malformed', 'sender object header is not canonical')
  }
  const ciphertextLength = new DataView(
    object.buffer,
    object.byteOffset,
    SENDER_OBJECT_HEADER_BYTES,
  ).getUint32(4, false)
  const prefixLength = SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES + ciphertextLength
  if (
    ciphertextLength < SENDER_OBJECT_TAG_BYTES ||
    prefixLength + SENDER_OBJECT_SIGNATURE_BYTES !== object.byteLength
  ) {
    throw new SenderObjectError('malformed', 'sender object length is inconsistent')
  }
  return {
    header: object.slice(0, SENDER_OBJECT_HEADER_BYTES),
    nonce: object.slice(
      SENDER_OBJECT_HEADER_BYTES,
      SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES,
    ),
    ciphertextAndTag: object.slice(
      SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES,
      prefixLength,
    ),
    prefix: object.slice(0, prefixLength),
    signature: object.slice(prefixLength),
  }
}

async function contextHash(
  expectedBinding: SenderObjectBinding,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  if (
    !(expectedBinding instanceof SenderObjectBinding) ||
    expectedBinding.context.byteLength === 0 ||
    OBJECT_LIMITS[expectedBinding.domain] !== expectedBinding.maxBytes
  ) {
    throw new SenderObjectError('binding', 'sender object binding is invalid')
  }
  return sha256(expectedBinding.context, runtime)
}

export async function senderObjectAuthenticationData(
  expectedBinding: SenderObjectBinding,
  header: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (header.byteLength !== SENDER_OBJECT_HEADER_BYTES) {
    throw new SenderObjectError('malformed', 'sender object header has an invalid width')
  }
  return concatBytes([
    TEXT_ENCODER.encode(expectedBinding.domain),
    Uint8Array.of(0),
    await contextHash(expectedBinding, runtime),
    header,
  ])
}

export async function senderObjectSignaturePreimage(
  expectedBinding: SenderObjectBinding,
  prefix: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  return concatBytes([
    TEXT_ENCODER.encode(expectedBinding.domain),
    Uint8Array.of(0),
    await contextHash(expectedBinding, runtime),
    prefix,
  ])
}

async function verifySignature(
  publicKey: Uint8Array,
  preimage: Uint8Array,
  signature: Uint8Array,
  runtime: CryptoRuntime,
): Promise<boolean> {
  if (publicKey.byteLength !== 32 || signature.byteLength !== SENDER_OBJECT_SIGNATURE_BYTES) {
    throw new SenderObjectError('key', 'Ed25519 verification material has an invalid width')
  }
  try {
    return await verifyEd25519Signature(publicKey, preimage, signature, runtime)
  } catch (cause) {
    throw new SenderObjectError('key', 'Unable to verify Ed25519 sender signature', { cause })
  }
}

export async function verifySenderObject(
  expectedBinding: SenderObjectBinding,
  publicKey: Uint8Array,
  object: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<void> {
  const parsed = parseSenderObject(object, expectedBinding)
  const preimage = await senderObjectSignaturePreimage(expectedBinding, parsed.prefix, runtime)
  if (!(await verifySignature(publicKey, preimage, parsed.signature, runtime))) {
    throw new SenderObjectError('signature', 'sender object signature is invalid')
  }
}

async function decryptSenderObject(
  expectedBinding: SenderObjectBinding,
  keyBytes: Uint8Array,
  object: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  if (keyBytes.byteLength !== SUITE02_DERIVED_KEY_BYTES) {
    throw new SenderObjectError('key', 'sender object key must be 32 bytes')
  }
  const parsed = parseSenderObject(object, expectedBinding)
  const aad = await senderObjectAuthenticationData(expectedBinding, parsed.header, runtime)
  try {
    const key = await importAesGcmKey(keyBytes, runtime)
    const plaintext = await runtime.subtle.decrypt(
      {
        name: 'AES-GCM',
        iv: parsed.nonce,
        additionalData: aad,
        tagLength: SENDER_OBJECT_TAG_BYTES * 8,
      },
      key,
      parsed.ciphertextAndTag,
    )
    if (plaintext.byteLength === 0) {
      throw new SenderObjectError('malformed', 'sender object plaintext is empty')
    }
    return new Uint8Array(plaintext)
  } catch (cause) {
    if (cause instanceof SenderObjectError) throw cause
    throw new SenderObjectError('authentication', 'sender object ciphertext is invalid', {
      cause,
    })
  }
}

export async function openSenderObject(
  expectedBinding: SenderObjectBinding,
  key: Uint8Array,
  publicKey: Uint8Array,
  object: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  await verifySenderObject(expectedBinding, publicKey, object, runtime)
  return decryptSenderObject(expectedBinding, key, object, runtime)
}

export async function openDescriptorObjectBootstrap(
  expectedBinding: SenderObjectBinding,
  key: Uint8Array,
  object: Uint8Array,
  senderPublicKey: (plaintext: Uint8Array) => Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  if (expectedBinding.domain !== SENDER_OBJECT_DOMAIN.descriptor) {
    throw new SenderObjectError('binding', 'descriptor bootstrap requires a descriptor binding')
  }
  const plaintext = await decryptSenderObject(expectedBinding, key, object, runtime)
  const publicKey = senderPublicKey(plaintext)
  const expectedPKHash = expectedBinding.context.subarray(1, 1 + PK_HASH_BYTES)
  if (!equalBytes(await suite02SenderKeyHash(publicKey, runtime), expectedPKHash)) {
    throw new SenderObjectError('signature', 'descriptor sender key does not match link pkHash')
  }
  await verifySenderObject(expectedBinding, publicKey, object, runtime)
  return plaintext
}
