import { describe, expect, it } from 'vitest'

import {
  createX25519KeyAgreement,
  verifyEd25519Signature,
} from '../../src/crypto/curve25519'
import type { CryptoRuntime } from '../../src/crypto/webcrypto'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface IdentityVector extends VectorCase {
  readonly senderPublicKeyB64: string
}

interface TranscriptVector extends VectorCase {
  readonly receiverPrivateB64: string
  readonly receiverPublicB64: string
  readonly senderPublicB64: string
  readonly sharedSecretB64: string
}

interface RegistrationVector extends VectorCase {
  readonly preimageB64: string
  readonly signatureB64: string
}

const identity = loadVectorFile(
  new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
).cases[0] as IdentityVector
const sessionCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases
const transcript = named<TranscriptVector>('sender-authenticated-x25519-transcript')
const registration = named<RegistrationVector>('fresh-relay-registration-proof')

describe('curve25519 browser boundary', () => {
  it('matches the shared X25519 vector through injected portable entropy', async () => {
    const agreement = await createX25519KeyAgreement({
      randomBytes: () => bytes(transcript.receiverPrivateB64),
    })

    expect(agreement.publicKey).toEqual(bytes(transcript.receiverPublicB64))
    await expect(
      agreement.deriveSharedSecret(bytes(transcript.senderPublicB64)),
    ).resolves.toEqual(bytes(transcript.sharedSecretB64))
    await expect(
      agreement.deriveSharedSecret(bytes(transcript.senderPublicB64)),
    ).rejects.toThrow('already consumed')
  })

  it('falls back only for unsupported X25519 and interoperates with native WebCrypto', async () => {
    const portable = await createX25519KeyAgreement({ runtime: unsupportedCurveRuntime() })
    const native = await createX25519KeyAgreement()
    const portablePublic = portable.publicKey
    const nativePublic = native.publicKey

    const portableSecret = await portable.deriveSharedSecret(nativePublic)
    const nativeSecret = await native.deriveSharedSecret(portablePublic)
    expect(portableSecret).toEqual(nativeSecret)
    portableSecret.fill(0)
    nativeSecret.fill(0)
  })

  it('verifies shared Ed25519 vectors with strict portable semantics', async () => {
    const publicKey = bytes(identity.senderPublicKeyB64)
    const preimage = bytes(registration.preimageB64)
    const signature = bytes(registration.signatureB64)

    await expect(
      verifyEd25519Signature(publicKey, preimage, signature, unsupportedCurveRuntime()),
    ).resolves.toBe(true)

    signature[0] = signature[0]! ^ 1
    await expect(
      verifyEd25519Signature(publicKey, preimage, signature, unsupportedCurveRuntime()),
    ).resolves.toBe(false)
  })

  it('does not disguise validation failures as missing-algorithm fallbacks', async () => {
    const failure = new DOMException('invalid key', 'DataError')
    await expect(
      verifyEd25519Signature(
        bytes(identity.senderPublicKeyB64),
        bytes(registration.preimageB64),
        bytes(registration.signatureB64),
        failingEd25519Runtime(failure),
      ),
    ).rejects.toBe(failure)
  })

  it('rejects invalid widths and low-order X25519 peers', async () => {
    await expect(
      verifyEd25519Signature(new Uint8Array(31), new Uint8Array(), new Uint8Array(64)),
    ).rejects.toThrow('Ed25519 public key must be 32 bytes')

    const agreement = await createX25519KeyAgreement({
      randomBytes: () => bytes(transcript.receiverPrivateB64),
    })
    await expect(agreement.deriveSharedSecret(new Uint8Array(32))).rejects.toThrow()
    await expect(
      agreement.deriveSharedSecret(bytes(transcript.senderPublicB64)),
    ).rejects.toThrow('already consumed')
  })
})

function named<T extends VectorCase>(name: string): T {
  const result = sessionCases.find((candidate) => candidate.name === name)
  if (result === undefined) throw new Error(`missing vector ${name}`)
  return result as T
}

function bytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}

function unsupportedCurveRuntime(): CryptoRuntime {
  return runtimeRejectingCurveMethods(() => new DOMException(
    'curve unavailable',
    'NotSupportedError',
  ))
}

function failingEd25519Runtime(failure: DOMException): CryptoRuntime {
  return runtimeRejectingCurveMethods(() => failure)
}

function runtimeRejectingCurveMethods(failure: () => DOMException): CryptoRuntime {
  const subtle = new Proxy(crypto.subtle, {
    get(target, property) {
      if (property === 'generateKey' || property === 'importKey') {
        return async () => { throw failure() }
      }
      const value = Reflect.get(target, property) as unknown
      return typeof value === 'function' ? value.bind(target) : value
    },
  })
  return { subtle }
}
