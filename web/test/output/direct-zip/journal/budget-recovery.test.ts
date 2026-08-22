import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../../src/crypto/bytes'
import {
  DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE,
  DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES,
  DIRECT_ZIP_MAXIMUM_MEMBER_COUNT,
  DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_AUTHORITY_TRANSACTION,
  DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_STAGING_TRANSACTION,
  DIRECT_ZIP_MAXIMUM_RECORDS_PER_PAGE,
  admitDirectZipJournalPageV1,
} from '../../../../src/output/direct-zip/journal/budget'
import { decideDirectZipRecoveryV1 } from '../../../../src/output/direct-zip/journal/recovery'
import { createDirectZipImmutablePageV1 } from '../../../../src/output/direct-zip/journal/records'

describe('Direct ZIP journal admission and recovery cuts', () => {
  it('enforces every named budget before admitting a page body', () => {
    expect(DIRECT_ZIP_MAXIMUM_MEMBER_COUNT).toBe(1_000_000n)
    expect(DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES).toBe(256n * 1024n * 1024n)
    expect(DIRECT_ZIP_MAXIMUM_RECORDS_PER_PAGE).toBe(256)
    expect(DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE).toBe(256 * 1024)
    expect(DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_STAGING_TRANSACTION).toBe(1)
    expect(DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_AUTHORITY_TRANSACTION).toBe(0)

    expect(() => admitDirectZipJournalPageV1({
      usage: {
        memberCount: DIRECT_ZIP_MAXIMUM_MEMBER_COUNT,
        canonicalMetadataBytes: 0n,
      },
      pageCanonicalBytes: 1,
      pageRecordCount: 1,
      memberCountDelta: 1n,
    })).toThrow(expect.objectContaining({ name: 'QuotaExceededError' }))
    expect(() => admitDirectZipJournalPageV1({
      usage: {
        memberCount: 0n,
        canonicalMetadataBytes: DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES,
      },
      pageCanonicalBytes: 1,
      pageRecordCount: 1,
      memberCountDelta: 0n,
    })).toThrow(expect.objectContaining({ name: 'QuotaExceededError' }))
  })

  it('counts the canonical page envelope, not caller-reported payload bytes', async () => {
    await expect(createDirectZipImmutablePageV1({
      operationId: identity(16, 1),
      pageKind: 'layout',
      chainId: identity(16, 2),
      pageOrdinal: 0,
      predecessorRootDigest: identity(32, 0),
      canonicalEntries: [new Uint8Array(DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE)],
      accountingPredecessor: {
        kind: 'checkpoint',
        checkpointGeneration: 1n,
        checkpointDigest: identity(32, 3),
      },
      previousBudgetUsage: { memberCount: 0n, canonicalMetadataBytes: 0n },
      previousChainRecordCount: 0n,
      previousChainCanonicalMetadataBytes: 0n,
    })).rejects.toEqual(expect.objectContaining({ name: 'QuotaExceededError' }))
  })

  it('uses conservative evidence precedence for every recovery cut', () => {
    const base = {
      permission: 'granted' as const,
      target: 'predecessor' as const,
      markerMatches: true,
      predecessorEpochsVerified: true,
      candidateRangeVerified: false,
      destinationSpaceAvailable: true,
      candidateAlreadyResolved: false,
    }
    expect(decideDirectZipRecoveryV1(base)).toEqual({ kind: 'retire-and-replay' })
    expect(decideDirectZipRecoveryV1({ ...base, permission: 'unavailable' })).toEqual({
      kind: 'authorization-required',
    })
    expect(decideDirectZipRecoveryV1({ ...base, target: 'absent' })).toEqual({
      kind: 'restart-required',
      reason: 'target-deleted',
    })
    expect(decideDirectZipRecoveryV1({ ...base, target: 'foreign' })).toEqual({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
    })
    expect(decideDirectZipRecoveryV1({
      ...base,
      target: 'candidate',
      candidateRangeVerified: true,
    })).toEqual({ kind: 'promote-candidate' })
    expect(decideDirectZipRecoveryV1({
      ...base,
      target: 'unknown-tail',
    })).toEqual({ kind: 'truncate-to-predecessor-and-replay' })
    expect(decideDirectZipRecoveryV1({
      ...base,
      candidateAlreadyResolved: true,
      destinationSpaceAvailable: false,
    })).toEqual({ kind: 'destination-space-required' })
    expect(decideDirectZipRecoveryV1({ ...base, target: 'ambiguous' })).toEqual({
      kind: 'target-verification-required',
    })
  })
})

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
