import { createHash } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import {
  createBlockRecordObjectBinding,
  senderObjectSignaturePreimage,
} from '../../src/crypto/sender-object'
import type { CryptoRuntime } from '../../src/crypto/webcrypto'

const BLOCK_BYTES = 1 << 20
const BLOCK_SIGNATURE_INPUT_BYTES = 97
const binding = createBlockRecordObjectBinding(
  new Uint8Array(16).fill(1),
  new Uint8Array(16).fill(2),
  new Uint8Array(16).fill(3),
  9n,
  BLOCK_BYTES,
)

describe('sender object signature commitments', () => {
  it('starts both native digests before waiting and authenticates a fixed-size input', async () => {
    const releases: Array<() => void> = []
    const digest = vi.fn(async (algorithm: AlgorithmIdentifier, data: BufferSource) => {
      await new Promise<void>(resolve => { releases.push(resolve) })
      return crypto.subtle.digest(algorithm, data)
    })
    const prefix = new Uint8Array(BLOCK_BYTES).fill(0x5a)
    const pending = senderObjectSignaturePreimage(binding, prefix, runtimeWithDigest(digest))
    // A controlled scheduler catches serial hashing without timing-dependent assertions.
    expect(releases).toHaveLength(2)
    releases.forEach(release => release())
    const preimage = await pending
    expect(preimage.byteLength).toBe(BLOCK_SIGNATURE_INPUT_BYTES)
    expect(preimage).toEqual(new Uint8Array(Buffer.concat([
      Buffer.from(binding.domain + '\0'),
      createHash('sha256').update(binding.context).digest(),
      createHash('sha256').update(prefix).digest(),
    ])))
    expect(digest.mock.calls.map(([algorithm]) => algorithm)).toEqual(['SHA-256', 'SHA-256'])
  })

  it.each([0, 1])('fails closed when commitment digest %i fails', async failedDigest => {
    const failure = new DOMException('hash engine failed', 'OperationError')
    let calls = 0
    const digest: SubtleCrypto['digest'] = async (algorithm, data) => {
      if (calls++ === failedDigest) throw failure
      return crypto.subtle.digest(algorithm, data)
    }
    await expect(senderObjectSignaturePreimage(
      binding, new Uint8Array(128), runtimeWithDigest(digest),
    )).rejects.toMatchObject({ code: 'digest-failed', cause: failure })
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
