import { describe, expect, it } from 'vitest'

import {
  decodeOpenResults,
  FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS,
  FRAGMENT_PAYLOAD_BYTES,
  FragmentAssembler,
  LEASE_RENEW_AFTER_MILLISECONDS,
  LEASE_TTL_MILLISECONDS,
  REVISION_CODE_QUOTA,
  REVISION_RETRY_MAXIMUM_MILLISECONDS,
} from '../../src/content/flow'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { fragmentRecord } from './v2-fragment-fixture'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 content result frozen bounds', () => {
  const fileId = identity(1)

  it('preserves the exact quota tuple while enforcing the frozen retry bounds', () => {
    const failedOpen = (code: number, delay: number) => encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1],
        [1, [[fileId, 1, code, true, delay]]],
      ]),
    )
    expect(() => decodeOpenResults(
      failedOpen(REVISION_CODE_QUOTA, 0),
      fileId,
    )).toThrow(/frozen range/i)
    expect(decodeOpenResults(
      failedOpen(REVISION_CODE_QUOTA, REVISION_RETRY_MAXIMUM_MILLISECONDS),
      fileId,
    ).failure).toEqual({
      code: REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds: REVISION_RETRY_MAXIMUM_MILLISECONDS,
    })
    expect(() => decodeOpenResults(
      failedOpen(REVISION_CODE_QUOTA, 30_001),
      fileId,
    )).toThrow(/frozen range/i)

    expect(decodeOpenResults(failedOpen(0x3001, 25), fileId).failure).toEqual({
      code: 0x3001,
      retryable: true,
      retryAfterMilliseconds: 25,
    })
  })

  it('requires exact authenticated lease timing', () => {
    const successfulOpen = (ttl: number, renewAfter: number) => encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1],
        [1, [[fileId, 0, Uint8Array.of(1), identity(2), ttl, renewAfter]]],
      ]),
    )
    expect(() => decodeOpenResults(successfulOpen(
      LEASE_TTL_MILLISECONDS,
      LEASE_RENEW_AFTER_MILLISECONDS,
    ), fileId)).not.toThrow()
    expect(() => decodeOpenResults(successfulOpen(119_999, 60_000), fileId))
      .toThrow(/lease timing/i)
    expect(() => decodeOpenResults(successfulOpen(120_000, 59_999), fileId))
      .toThrow(/lease timing/i)
  })
})

describe('v2 fragment inactivity accounting', () => {
  it('allows assemblies longer than 15 seconds while unique fragments keep arriving', async () => {
    let now = 0
    const operationId = identity(11)
    const object = new Uint8Array(FRAGMENT_PAYLOAD_BYTES * 2 + 1).fill(0x31)
    const fragments = fragmentRecord(operationId, object)
    const assembler = new FragmentAssembler(operationId, () => now)

    await expect(assembler.accept(fragments[0]!)).resolves.toMatchObject({ status: 'accepted', receivedBytes: FRAGMENT_PAYLOAD_BYTES })
    now += FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[1]!)).resolves.toMatchObject({ status: 'accepted', receivedBytes: FRAGMENT_PAYLOAD_BYTES })
    now += FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[2]!)).resolves.toMatchObject({
      status: 'complete',
      receivedBytes: 1,
      object,
    })
  })

  it('does not let authenticated duplicates extend the inactivity deadline', async () => {
    let now = 0
    const operationId = identity(12)
    const fragments = fragmentRecord(
      operationId,
      new Uint8Array(FRAGMENT_PAYLOAD_BYTES + 1).fill(0x32),
    )
    const assembler = new FragmentAssembler(operationId, () => now)

    await assembler.accept(fragments[0]!)
    now += FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS - 1_000
    await expect(assembler.accept(fragments[0]!)).resolves.toMatchObject({ status: 'duplicate' })
    now += 1_000
    await expect(assembler.accept(fragments[1]!)).rejects.toMatchObject({
      name: 'FragmentInactivityError',
    })
  })
})
