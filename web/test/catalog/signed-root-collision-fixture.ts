import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { ed25519 } from '@noble/curves/ed25519.js'

import { PATH_POLICY, type ShareDescriptor } from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

const IDENTITY_BYTES = 16
const SIGNING_SEED_BYTES = 32
const READ_SECRET_BYTES = 16
const COMMITMENT_BYTES = 32
const CATALOG_SCHEMA = 1n
const DIRECTORY_ENTRY_KIND = 1n

export interface SignedRootCollisionFixture {
  readonly descriptor: ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  readonly catalogObject: Uint8Array<ArrayBuffer>
  close(): void
}

export function createSignedRootCollisionFixture(): SignedRootCollisionFixture {
  const shareInstance = fixedIdentity(0x11)
  const syntheticRoot = fixedIdentity(0x22)
  const generation = fixedIdentity(0x33)
  const readSecret = new Uint8Array(READ_SECRET_BYTES).fill(0x44)
  const signingSeed = new Uint8Array(SIGNING_SEED_BYTES).fill(0x55)
  const signing = ed25519.keygen(signingSeed)
  const senderPublicKey = signing.publicKey.slice()
  signing.secretKey.fill(0)
  signingSeed.fill(0)
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
  return Object.freeze({ descriptor, readSecret, catalogObject: plaintext, close: () => readSecret.fill(0) })
}

function fixedIdentity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(IDENTITY_BYTES)
  value[0] = first
  return value
}
