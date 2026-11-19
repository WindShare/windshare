import { describe, expect, it, vi } from 'vitest'
import { decodeShareDescriptor } from '../../src/catalog/records'
import { openDescriptorObjectBootstrap } from '../../src/crypto/sender-object'
import type { CryptoRuntime } from '../../src/crypto/webcrypto'
import { descriptorObjectVector } from '../protocol/delivery-vectors'
import { b64ToBytes } from '../vectors'

function fixture() {
  const context = b64ToBytes(descriptorObjectVector.contextB64)
  return {
    route: { pkHash: context.slice(1, 17), shareIdRaw: context.slice(17) },
    key: b64ToBytes(descriptorObjectVector.keyB64),
    object: b64ToBytes(descriptorObjectVector.objectB64),
    plaintext: b64ToBytes(descriptorObjectVector.canonicalCborB64),
  }
}

const identifySender = (candidate: Uint8Array) => decodeShareDescriptor(candidate).sender

describe('descriptor bootstrap authentication', () => {
  it('starts the independent digests together and verifies the frozen signature preimage', async () => {
    const { route, key, object, plaintext } = fixture()
    const releases: Array<() => void> = []
    const digest = vi.fn(async (algorithm: AlgorithmIdentifier, data: BufferSource) => {
      if (releases.length < 3) await new Promise<void>(resolve => { releases.push(resolve) })
      return crypto.subtle.digest(algorithm, data)
    })
    const sender = identifySender(plaintext)
    const verify = vi.fn((message: Uint8Array, signature: Uint8Array) => sender.verify(message, signature))
    const pending = openDescriptorObjectBootstrap(route, key, object, () => ({
      publicKey: sender.publicKey, verify,
    }), runtimeWithDigest(digest))
    // A controlled scheduler detects serial hashing without timing assertions.
    expect(releases).toHaveLength(3)
    releases.forEach(release => release())

    await expect(pending).resolves.toEqual(plaintext)
    expect(verify).toHaveBeenCalledExactlyOnceWith(
      b64ToBytes(descriptorObjectVector.signaturePreimageB64), object.subarray(-64),
    )
  })

  it.each([0, 1, 2])('fails closed when bootstrap digest %i fails', async failedDigest => {
    const { route, key, object } = fixture()
    const failure = new DOMException('hash engine failed', 'OperationError')
    let calls = 0
    const digest: SubtleCrypto['digest'] = async (algorithm, data) => {
      if (calls++ === failedDigest) throw failure
      return crypto.subtle.digest(algorithm, data)
    }
    const identity = vi.fn(identifySender)
    await expect(openDescriptorObjectBootstrap(route, key, object, identity, runtimeWithDigest(digest)))
      .rejects.toMatchObject({ code: 'digest-failed', cause: failure })
    expect(identity).not.toHaveBeenCalled()
  })

  it('keeps the route, key, and descriptor on one snapshot across WebCrypto awaits', async () => {
    const { route, key, object, plaintext } = fixture()
    const pending = openDescriptorObjectBootstrap(route, key, object, identifySender)

    route.pkHash.fill(0)
    route.shareIdRaw.fill(0)
    key.fill(0)
    object.fill(0)

    await expect(pending).resolves.toEqual(plaintext)
  })
})

function runtimeWithDigest(digest: SubtleCrypto['digest']): CryptoRuntime {
  return {
    subtle: new Proxy(crypto.subtle, {
      get(target, property) {
        if (property === 'digest') return digest
        const value = Reflect.get(target, property) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    }),
  }
}
