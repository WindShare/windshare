import { ed25519 } from '@noble/curves/ed25519.js'

import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import { concatBytes, copyBytes, encodeBase64Url } from '../../src/crypto/bytes'
import {
  createCatalogPageObjectBinding,
  SENDER_OBJECT_HEADER_BYTES,
  SENDER_OBJECT_NONCE_BYTES,
  SENDER_OBJECT_TAG_BYTES,
  SENDER_OBJECT_WIRE_VERSION,
  senderObjectAuthenticationData,
  senderObjectSignaturePreimage,
} from '../../src/crypto/sender-object'
import { deriveSuite02CatalogKey } from '../../src/crypto/suite02-key-derivation'
import { defaultCryptoRuntime } from '../../src/crypto/webcrypto'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

const IDENTITY_BYTES = 16
const SIGNING_SEED_BYTES = 32
const READ_SECRET_BYTES = 16
const COMMITMENT_BYTES = 32
const CATALOG_SCHEMA = 1n
const DIRECTORY_ENTRY_KIND = 1n
const ROOT_COLLISION_NONCE_PREFIX = 0x73

export interface SignedRootCollisionFixture {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  readonly catalogObject: Uint8Array<ArrayBuffer>
  close(): void
}

export async function createSignedRootCollisionFixture(): Promise<SignedRootCollisionFixture> {
  const shareInstance = fixedIdentity(0x11)
  const syntheticRoot = fixedIdentity(0x22)
  const generation = fixedIdentity(0x33)
  const readSecret = new Uint8Array(READ_SECRET_BYTES).fill(0x44)
  const signingSeed = new Uint8Array(SIGNING_SEED_BYTES).fill(0x55)
  const signing = ed25519.keygen(signingSeed)
  const signingSecret = signing.secretKey.slice()
  const senderPublicKey = signing.publicKey.slice()
  signing.secretKey.fill(0)
  signingSeed.fill(0)
  const descriptor: V2ShareDescriptor = Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot,
    syntheticRootId: encodeBase64Url(syntheticRoot),
    chunkSize: 1 << 20,
    capabilities: 0n,
    senderPublicKey,
    createdAtSeconds: 1n,
    pathPolicy: V2_PATH_POLICY,
  })
  const plaintext = encodeCanonicalCbor(new Map<number, unknown>([
    [0, CATALOG_SCHEMA],
    [1, shareInstance],
    [2, syntheticRoot],
    [3, generation],
    [4, 0n],
    [5, true],
    [6, new Uint8Array(COMMITMENT_BYTES)],
    [7, [[
      DIRECTORY_ENTRY_KIND,
      syntheticRoot,
      'root-loop',
      null,
      null,
      0n,
      0n,
    ]]],
    [8, 0n],
  ]))
  const catalogKeyBytes = await deriveSuite02CatalogKey(readSecret, shareInstance)
  try {
    const subtle = defaultCryptoRuntime().subtle
    const catalogKey = await subtle.importKey('raw', catalogKeyBytes, 'AES-GCM', false, ['encrypt'])
    const binding = createCatalogPageObjectBinding(shareInstance, syntheticRoot, 0)
    const header = new Uint8Array(SENDER_OBJECT_HEADER_BYTES)
    header[0] = SENDER_OBJECT_WIRE_VERSION
    new DataView(header.buffer).setUint32(4, plaintext.byteLength + SENDER_OBJECT_TAG_BYTES, false)
    const nonce = new Uint8Array(SENDER_OBJECT_NONCE_BYTES)
    nonce[0] = ROOT_COLLISION_NONCE_PREFIX
    const ciphertext = new Uint8Array(await subtle.encrypt({
      name: 'AES-GCM',
      iv: nonce,
      additionalData: await senderObjectAuthenticationData(binding, header),
      tagLength: SENDER_OBJECT_TAG_BYTES * 8,
    }, catalogKey, copyBytes(plaintext)))
    const prefix = concatBytes([header, nonce, ciphertext])
    const signature = ed25519.sign(
      await senderObjectSignaturePreimage(binding, prefix),
      signingSecret,
    )
    const catalogObject = concatBytes([prefix, signature])
    return Object.freeze({
      descriptor,
      readSecret,
      catalogObject,
      close: () => readSecret.fill(0),
    })
  } finally {
    signingSecret.fill(0)
    catalogKeyBytes.fill(0)
  }
}

function fixedIdentity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(IDENTITY_BYTES)
  value[0] = first
  return value
}
