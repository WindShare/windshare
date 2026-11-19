import { describe, expect, it } from 'vitest'
import { decodeCapacityWait } from '../../src/transport/relay/capacity-wait-codec'
import { BandwidthWaitError } from '../../src/contracts/capacity'
import { recoveryRetry } from '../../src/receiver/recovery-failure'

describe('relay bandwidth waits', () => {
  const bytes = Uint8Array.from(Buffer.from('5753325a02010000010203040506070800003a98', 'hex'))
  it('uses the Go and Rust wire format and rejects unbounded waits', () => {
    expect(decodeCapacityWait(bytes)).toEqual({
      relaySessionId: Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8), direction: 1, milliseconds: 15_000,
    })
    const malformed = bytes.slice()
    new DataView(malformed.buffer).setUint32(16, 60_001, false)
    expect(() => decodeCapacityWait(malformed)).toThrow()
    for (const offset of [4, 5, 6, 7]) {
      const corrupt = bytes.slice(); corrupt[offset] = 3
      expect(() => decodeCapacityWait(corrupt)).toThrow()
    }
  })
  it('keeps deadline evidence and applies bounded recovery delay through wrappers', () => {
    const cause = new DOMException('slow', 'TimeoutError')
    const error = new BandwidthWaitError(15_000, cause)
    expect(error.cause).toBe(cause)
    expect(recoveryRetry(new AggregateError([new Error('lane', { cause: error })]), 10)).toEqual({
      retryAt: 30_010, reason: 'bandwidth',
    })
    expect(recoveryRetry(new Error('connection failed'), 10)).toBeUndefined()
  })
})
