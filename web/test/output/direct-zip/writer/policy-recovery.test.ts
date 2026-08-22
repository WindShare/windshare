import { describe, expect, it } from 'vitest'
import {
  decideDirectZipAutomaticCheckpointV1,
  decideDirectZipCandidateRecoveryV1,
  directZipEpochPolicyDigestV1,
} from '../../../../src/output/direct-zip/writer'
import { candidateObservation } from './fault-model'

const MEBIBYTE = 1024n * 1024n
const REVIEWED_EPOCH_POLICY = Object.freeze({
  maximumPrefixCopyBytes: 256n * MEBIBYTE,
  maximumCumulativePrefixCopyBytes: 512n * MEBIBYTE,
  maximumModeledPeakTemporaryBytes: 256n * MEBIBYTE,
})

describe('DirectZip epoch policy', () => {
  it('declines automatic close when evidence-derived budgets are absent', () => {
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 80n,
      cumulativePrefixCopyBytes: 40n,
    })).toEqual({
      kind: 'decline',
      reason: 'evidence-unavailable',
      additionalTemporaryBytesUpperBound: 80n,
    })
  })

  it('checks both peak prefix copy and cumulative copy without changing pause space math', () => {
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 101n,
      cumulativePrefixCopyBytes: 0n,
      budget: {
        maximumPrefixCopyBytes: 100n,
        maximumCumulativePrefixCopyBytes: 500n,
        maximumModeledPeakTemporaryBytes: 100n,
      },
    })).toMatchObject({ kind: 'decline', reason: 'prefix-copy-budget' })
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 80n,
      cumulativePrefixCopyBytes: 430n,
      budget: {
        maximumPrefixCopyBytes: 100n,
        maximumCumulativePrefixCopyBytes: 500n,
        maximumModeledPeakTemporaryBytes: 100n,
      },
    })).toEqual({
      kind: 'decline',
      reason: 'cumulative-copy-budget',
      additionalTemporaryBytesUpperBound: 80n,
    })
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 80n,
      cumulativePrefixCopyBytes: 400n,
      budget: {
        maximumPrefixCopyBytes: 100n,
        maximumCumulativePrefixCopyBytes: 500n,
        maximumModeledPeakTemporaryBytes: 100n,
      },
    })).toEqual({
      kind: 'admit',
      nextCumulativePrefixCopyBytes: 480n,
      additionalTemporaryBytesUpperBound: 80n,
    })
  })

  it('binds all reviewed byte limits into the canonical policy digest', async () => {
    await expect(directZipEpochPolicyDigestV1(REVIEWED_EPOCH_POLICY))
      .resolves.toBe('dVc_DFPK_50xrZ7_GK0oQ9noWgHhb-2eZEnl4-0kUOo')
  })

  it('admits the inclusive 256 MiB archive-prefix boundary but declines a 257 MiB prefix', () => {
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 256n * MEBIBYTE,
      cumulativePrefixCopyBytes: 256n * MEBIBYTE,
      budget: REVIEWED_EPOCH_POLICY,
    })).toEqual({
      kind: 'admit',
      nextCumulativePrefixCopyBytes: 512n * MEBIBYTE,
      additionalTemporaryBytesUpperBound: 256n * MEBIBYTE,
    })
    expect(decideDirectZipAutomaticCheckpointV1({
      // The measured 256 MiB predecessor gained a 1 MiB staged ZIP epoch.
      committedLength: 257n * MEBIBYTE,
      cumulativePrefixCopyBytes: 256n * MEBIBYTE,
      budget: REVIEWED_EPOCH_POLICY,
    })).toEqual({
      kind: 'decline',
      reason: 'prefix-copy-budget',
      additionalTemporaryBytesUpperBound: 257n * MEBIBYTE,
    })
  })

  it('checks modeled peak and cumulative admission without overflowing positioned arithmetic', () => {
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 81n,
      cumulativePrefixCopyBytes: 0n,
      budget: {
        maximumPrefixCopyBytes: 100n,
        maximumCumulativePrefixCopyBytes: 500n,
        maximumModeledPeakTemporaryBytes: 80n,
      },
    })).toEqual({
      kind: 'decline',
      reason: 'modeled-peak-temporary-budget',
      additionalTemporaryBytesUpperBound: 81n,
    })
    expect(decideDirectZipAutomaticCheckpointV1({
      committedLength: 2n,
      cumulativePrefixCopyBytes: BigInt(Number.MAX_SAFE_INTEGER),
      budget: {
        maximumPrefixCopyBytes: BigInt(Number.MAX_SAFE_INTEGER),
        maximumCumulativePrefixCopyBytes: BigInt(Number.MAX_SAFE_INTEGER),
        maximumModeledPeakTemporaryBytes: BigInt(Number.MAX_SAFE_INTEGER),
      },
    })).toEqual({
      kind: 'decline',
      reason: 'cumulative-copy-budget',
      additionalTemporaryBytesUpperBound: 2n,
    })
  })
})

describe('DirectZip candidate recovery decisions', () => {
  it('does not accept a candidate from length or observation alone', () => {
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation())).toEqual({
      kind: 'verify-candidate-range',
    })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      observationMatch: 'neither',
      candidateIntegrity: 'verified',
    }))).toEqual({ kind: 'target-verification-required' })
  })

  it('separates replay, promotion, verified truncation, and ambiguity', () => {
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      length: 'predecessor',
      observationMatch: 'predecessor',
    }))).toEqual({ kind: 'replay-predecessor' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      candidateIntegrity: 'verified',
    }))).toEqual({ kind: 'promote-candidate' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      length: 'unknown-tail',
      observationMatch: 'neither',
      predecessorIntegrity: 'not-read',
    }))).toEqual({ kind: 'verify-predecessor-epochs' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      length: 'unknown-tail',
      observationMatch: 'neither',
      predecessorIntegrity: 'verified',
    }))).toEqual({ kind: 'truncate-and-replay' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      ownership: 'ambiguous',
    }))).toEqual({ kind: 'target-verification-required' })
  })

  it('keeps permission, deletion, foreign ownership, and post-resolution space distinct', () => {
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      permission: 'unavailable',
    }))).toEqual({ kind: 'authorization-required' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      presence: 'deleted',
    }))).toEqual({ kind: 'restart-required', reason: 'target-deleted' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      ownership: 'foreign',
    }))).toEqual({ kind: 'needs-attention', reason: 'foreign-target' })
    expect(decideDirectZipCandidateRecoveryV1(candidateObservation({
      destinationSpaceFailure: true,
      candidateResolvedBeforeSpaceFailure: true,
    }))).toEqual({ kind: 'destination-space-required' })
  })
})
