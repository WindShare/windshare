import { describe, expect, it } from 'vitest'

import {
  decodeV2OpenResults,
  V2_LEASE_RENEW_AFTER_MILLISECONDS,
  V2_LEASE_TTL_MILLISECONDS,
  V2_REVISION_RETRY_MAXIMUM_MILLISECONDS,
} from '../../src/content/v2-flow'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 content result frozen bounds', () => {
  const fileId = identity(1)

  it('accepts revision retry delays only from 1 through 30000 milliseconds', () => {
    const failedOpen = (delay: number) => encodeCanonicalCbor(new Map<number, unknown>([
      [0, 1],
      [1, [[fileId, 1, 0x3005, true, delay]]],
    ]))
    expect(() => decodeV2OpenResults(failedOpen(0), fileId)).toThrow(/frozen range/i)
    expect(
      decodeV2OpenResults(failedOpen(V2_REVISION_RETRY_MAXIMUM_MILLISECONDS), fileId)
        .failure?.retryAfterMilliseconds,
    ).toBe(V2_REVISION_RETRY_MAXIMUM_MILLISECONDS)
    expect(() => decodeV2OpenResults(failedOpen(30_001), fileId)).toThrow(/frozen range/i)
  })

  it('requires exact authenticated lease timing', () => {
    const successfulOpen = (ttl: number, renewAfter: number) => encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1],
        [1, [[fileId, 0, Uint8Array.of(1), identity(2), ttl, renewAfter]]],
      ]),
    )
    expect(() => decodeV2OpenResults(successfulOpen(
      V2_LEASE_TTL_MILLISECONDS,
      V2_LEASE_RENEW_AFTER_MILLISECONDS,
    ), fileId)).not.toThrow()
    expect(() => decodeV2OpenResults(successfulOpen(119_999, 60_000), fileId))
      .toThrow(/lease timing/i)
    expect(() => decodeV2OpenResults(successfulOpen(120_000, 59_999), fileId))
      .toThrow(/lease timing/i)
  })
})
