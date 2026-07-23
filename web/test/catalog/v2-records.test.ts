import { describe, expect, it } from 'vitest'

import { decodeV2DirectoryFailure } from '../../src/catalog/v2-records'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 directory failure retry semantics', () => {
  const failure = (code: number, retryable: boolean, retryAfter: number | null) =>
    encodeCanonicalCbor(new Map<number, unknown>([
      [0, 1],
      [1, identity(1)],
      [2, identity(2)],
      [3, identity(3)],
      [4, code],
      [5, retryable],
      [6, retryAfter],
    ]))

  it('allows retryable budget failures but requires transient I/O to be retryable', () => {
    const budget = decodeV2DirectoryFailure(failure(0x2005, true, 250))
    expect(budget.retryable).toBe(true)
    expect(budget.retryAfterMilliseconds).toBe(250)
    expect(() => decodeV2DirectoryFailure(failure(0x2007, false, null)))
      .toThrow(/must be retryable/i)
  })
})
