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
const FILE_ENTRY_KIND = 2n
const DIRECTORY_ENTRY_KIND = 1n

export interface SignedCatalogEntryInput {
  readonly kind: 'file' | 'directory'
  readonly id: Uint8Array<ArrayBuffer>
  readonly name: string
  readonly expectedSize?: bigint
}

export interface SignedCatalogPageInput {
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly generation: Uint8Array<ArrayBuffer>
  readonly pageIndex: number
  readonly terminal: boolean
  readonly previousCommitment: Uint8Array<ArrayBuffer>
  readonly entries: readonly SignedCatalogEntryInput[]
}

export interface SealedCatalogPage {
  readonly object: Uint8Array<ArrayBuffer>
  readonly commitment: Uint8Array<ArrayBuffer>
}

export interface SignedCatalogFixture {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  sealPage(input: SignedCatalogPageInput): Promise<SealedCatalogPage>
  close(): void
}

export async function createSignedCatalogFixture(): Promise<SignedCatalogFixture> {
  const shareInstance = fixedIdentity(0x11)
  const syntheticRoot = fixedIdentity(0x22)
  const readSecret = new Uint8Array(16).fill(0x44)
  const signingSeed = new Uint8Array(32).fill(0x55)
  const signing = ed25519.keygen(signingSeed)
  const signingSecret = signing.secretKey.slice()
  const senderPublicKey = signing.publicKey.slice()
  signing.secretKey.fill(0)
  signingSeed.fill(0)
  const catalogKeyBytes = await deriveSuite02CatalogKey(readSecret, shareInstance)
  const subtle = defaultCryptoRuntime().subtle
  const catalogKey = await subtle.importKey('raw', catalogKeyBytes, 'AES-GCM', false, ['encrypt'])
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
  let nonceSequence = 1
  let closed = false

  return Object.freeze({
    descriptor,
    readSecret,
    sealPage: async (input: SignedCatalogPageInput): Promise<SealedCatalogPage> => {
      if (closed) throw new Error('Signed catalog fixture is closed')
      const plaintext = encodeCanonicalCbor(new Map<number, unknown>([
        [0, 1n],
        [1, shareInstance],
        [2, input.directoryId],
        [3, input.generation],
        [4, BigInt(input.pageIndex)],
        [5, input.terminal],
        [6, input.previousCommitment],
        [7, input.entries.map(catalogEntryRecord)],
        [8, 0n],
      ]))
      const binding = createCatalogPageObjectBinding(shareInstance, input.directoryId, input.pageIndex)
      const header = new Uint8Array(SENDER_OBJECT_HEADER_BYTES)
      header[0] = SENDER_OBJECT_WIRE_VERSION
      new DataView(header.buffer).setUint32(4, plaintext.byteLength + SENDER_OBJECT_TAG_BYTES, false)
      const nonce = new Uint8Array(SENDER_OBJECT_NONCE_BYTES)
      nonce[0] = 0x73
      new DataView(nonce.buffer).setUint32(SENDER_OBJECT_NONCE_BYTES - 4, nonceSequence, false)
      nonceSequence += 1
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
      const object = concatBytes([prefix, signature])
      const commitment = new Uint8Array(await subtle.digest('SHA-256', object))
      return Object.freeze({ object, commitment })
    },
    close: () => {
      if (closed) return
      closed = true
      readSecret.fill(0)
      signingSecret.fill(0)
      catalogKeyBytes.fill(0)
    },
  })
}

export function signedCatalogIdentity(first: number): Uint8Array<ArrayBuffer> {
  return fixedIdentity(first)
}

function catalogEntryRecord(entry: SignedCatalogEntryInput): readonly unknown[] {
  return Object.freeze([
    entry.kind === 'directory' ? DIRECTORY_ENTRY_KIND : FILE_ENTRY_KIND,
    entry.id,
    entry.name,
    entry.kind === 'directory' ? null : (entry.expectedSize ?? 1n),
    null,
    0n,
    0n,
  ])
}

function fixedIdentity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(IDENTITY_BYTES)
  value[0] = first
  return value
}
