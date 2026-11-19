import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { ed25519 } from '@noble/curves/ed25519.js'

import { PATH_POLICY, type ShareDescriptor } from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { defaultCryptoRuntime } from '../../src/crypto/webcrypto'
import { encode, rfc8949EncodeOptions } from 'cborg'

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
  readonly descriptor: ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  sealPage(input: SignedCatalogPageInput): Promise<SealedCatalogPage>
  close(): void
}

export function createSignedCatalogFixture(): SignedCatalogFixture {
  const shareInstance = fixedIdentity(0x11)
  const syntheticRoot = fixedIdentity(0x22)
  const readSecret = new Uint8Array(16).fill(0x44)
  const signingSeed = new Uint8Array(32).fill(0x55)
  const signing = ed25519.keygen(signingSeed)
  const senderPublicKey = signing.publicKey.slice()
  signing.secretKey.fill(0)
  signingSeed.fill(0)
  const subtle = defaultCryptoRuntime().subtle
  const descriptor: ShareDescriptor = Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot,
    syntheticRootId: encodeBase64Url(syntheticRoot),
    chunkSize: 1 << 20,
    capabilities: 0n,
    sender: createEd25519Verifier(senderPublicKey),
    createdAtSeconds: 1n,
    pathPolicy: PATH_POLICY,
  })
  let closed = false

  return Object.freeze({
    descriptor,
    readSecret,
    sealPage: async (input: SignedCatalogPageInput): Promise<SealedCatalogPage> => {
      if (closed) throw new Error('Catalog fixture is closed')
      // The Go catalog codec encodes schema-validated names, not generic session text.
      const plaintext = encode(new Map<number, unknown>([
        [0, 1n],
        [1, shareInstance],
        [2, input.directoryId],
        [3, input.generation],
        [4, BigInt(input.pageIndex)],
        [5, input.terminal],
        [6, input.previousCommitment],
        [7, input.entries.map(catalogEntryRecord)],
        [8, 0n],
      ]), rfc8949EncodeOptions)
      const object = Uint8Array.from(plaintext)
      const commitment = new Uint8Array(await subtle.digest('SHA-256', object))
      return Object.freeze({ object, commitment })
    },
    close: () => {
      if (closed) return
      closed = true
      readSecret.fill(0)
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
