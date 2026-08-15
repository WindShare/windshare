import { describe, expect, it } from 'vitest'

import {
  decodeV2OpenResults,
  V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS,
  V2_FRAGMENT_PAYLOAD_BYTES,
  V2FragmentAssembler,
  V2_LEASE_RENEW_AFTER_MILLISECONDS,
  V2_LEASE_TTL_MILLISECONDS,
  V2_REVISION_RETRY_MAXIMUM_MILLISECONDS,
} from '../../src/content/v2-flow'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { fragmentRecord } from './v2-fragment-fixture'

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

describe('v2 fragment inactivity accounting', () => {
  it('allows assemblies longer than 15 seconds while unique fragments keep arriving', async () => {
    let now = 0
    const operationId = identity(11)
    const object = new Uint8Array(V2_FRAGMENT_PAYLOAD_BYTES * 2 + 1).fill(0x31)
    const fragments = fragmentRecord(operationId, object)
    const assembler = new V2FragmentAssembler(operationId, () => now)

    await expect(assembler.accept(fragments[0]!)).resolves.toMatchObject({ status: 'accepted' })
    now += V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[1]!)).resolves.toMatchObject({ status: 'accepted' })
    now += V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[2]!)).resolves.toMatchObject({
      status: 'complete',
      object,
    })
  })

  it('does not let authenticated duplicates extend the inactivity deadline', async () => {
    let now = 0
    const operationId = identity(12)
    const fragments = fragmentRecord(
      operationId,
      new Uint8Array(V2_FRAGMENT_PAYLOAD_BYTES + 1).fill(0x32),
    )
    const assembler = new V2FragmentAssembler(operationId, () => now)

    await assembler.accept(fragments[0]!)
    now += V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[0]!)).resolves.toMatchObject({ status: 'duplicate' })
    now += 1_000
    await expect(assembler.accept(fragments[1]!)).rejects.toMatchObject({
      name: 'V2FragmentInactivityError',
    })
  })
})
