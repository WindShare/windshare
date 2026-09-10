import { describe, expect, it } from 'vitest'
import {
  decideDirectZipAutomaticCheckpointV1,
  decideDirectZipCandidateRecoveryV1,
  directZipEpochPolicyDigestV2,
} from '../../../../src/output/direct-zip/writer'
import { DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1 } from '../../../../src/output/direct-zip/session/runtime-admission'
import { candidateObservation } from './fault-model'

const MEBIBYTE = 1024n * 1024n
const GIBIBYTE = 1024n * MEBIBYTE
const AUTOMATIC_POLICY = DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy

describe('DirectZip epoch policy', () => {
  it('declines automatic close when policy is absent', () => {
    expect(decideDirectZipAutomaticCheckpointV1({
      archiveOffset: 80n,
      committedLength: 40n,
    })).toEqual({
      kind: 'decline',
      reason: 'policy-unavailable',
      additionalTemporaryBytesUpperBound: 80n,
    })
  })

  it('requires the minimum archive advance including ZIP metadata', () => {
    for (const archiveOffset of [0n, 1n, 64n * MEBIBYTE - 1n]) {
      expect(decideDirectZipAutomaticCheckpointV1({
        archiveOffset,
        committedLength: 0n,
        policy: AUTOMATIC_POLICY,
      })).toEqual({
        kind: 'decline',
        reason: 'insufficient-progress',
        additionalTemporaryBytesUpperBound: archiveOffset,
      })
    }
    expect(decideDirectZipAutomaticCheckpointV1({
      archiveOffset: 64n * MEBIBYTE,
      committedLength: 0n,
      policy: AUTOMATIC_POLICY,
    })).toEqual({
      kind: 'admit',
      additionalTemporaryBytesUpperBound: 64n * MEBIBYTE,
    })
  })

  it('spaces checkpoints by the whole durable archive across member boundaries and resumes', () => {
    const committedLength = 768n * MEBIBYTE
    // This decision needs only persisted checkpoint authority, with no volatile
    // lifetime counter that resets whenever the writer is reconstructed.
    for (const pending of [0n, 64n * MEBIBYTE, committedLength - 1n]) {
      expect(decideDirectZipAutomaticCheckpointV1({
        archiveOffset: committedLength + pending,
        committedLength,
        policy: AUTOMATIC_POLICY,
      }).kind).toBe('decline')
    }
    expect(decideDirectZipAutomaticCheckpointV1({
      archiveOffset: 2n * committedLength,
      committedLength,
      policy: AUTOMATIC_POLICY,
    })).toEqual({
      kind: 'admit',
      additionalTemporaryBytesUpperBound: 2n * committedLength,
    })
  })

  it('keeps multi-gigabyte checkpoints progressing with amortized prefix copies below twice progress', () => {
    let committedLength = 0n
    let totalPrefixCopyBytes = 0n
    const cuts: bigint[] = []
    // Numeric archive accounting covers a 32 GiB transfer without allocating or
    // copying payload-sized buffers in the local test suite.
    for (let archiveOffset = 16n * MEBIBYTE; archiveOffset <= 32n * GIBIBYTE;
      archiveOffset += 16n * MEBIBYTE) {
      const decision = decideDirectZipAutomaticCheckpointV1({
        archiveOffset,
        committedLength,
        policy: AUTOMATIC_POLICY,
      })
      if (decision.kind === 'decline') continue
      totalPrefixCopyBytes += archiveOffset
      committedLength = archiveOffset
      cuts.push(committedLength)
      expect(totalPrefixCopyBytes).toBeLessThan(2n * archiveOffset)
    }
    expect(cuts).toEqual([
      64n * MEBIBYTE, 128n * MEBIBYTE, 256n * MEBIBYTE, 512n * MEBIBYTE,
      GIBIBYTE, 2n * GIBIBYTE, 4n * GIBIBYTE, 8n * GIBIBYTE,
      16n * GIBIBYTE, 32n * GIBIBYTE,
    ])
  })

  it('binds geometric scheduling and the advance floor into a new canonical digest', async () => {
    await expect(directZipEpochPolicyDigestV2(AUTOMATIC_POLICY))
      .resolves.toBe('cWXRxG365A-doi8JI-H2YlsavP6MppN_O9OpjACii8s')
    await expect(directZipEpochPolicyDigestV2({ minimumAdvanceBytes: 1n }))
      .resolves.not.toBe('cWXRxG365A-doi8JI-H2YlsavP6MppN_O9OpjACii8s')
  })

  it('compares positioned archive bounds without doubling beyond the target limit', () => {
    const maximum = BigInt(Number.MAX_SAFE_INTEGER)
    expect(decideDirectZipAutomaticCheckpointV1({
      archiveOffset: maximum,
      committedLength: maximum / 2n,
      policy: AUTOMATIC_POLICY,
    }).kind).toBe('admit')
    expect(decideDirectZipAutomaticCheckpointV1({
      archiveOffset: maximum,
      committedLength: maximum / 2n + 1n,
      policy: AUTOMATIC_POLICY,
    }).kind).toBe('decline')
  })

  it.each([
    { archiveOffset: -1n, committedLength: 0n },
    { archiveOffset: BigInt(Number.MAX_SAFE_INTEGER) + 1n, committedLength: 0n },
    { archiveOffset: 1n, committedLength: -1n },
    { archiveOffset: 1n, committedLength: 2n },
  ])('rejects invalid archive authority: %s', (input) => {
    expect(() => decideDirectZipAutomaticCheckpointV1({
      ...input, policy: AUTOMATIC_POLICY,
    })).toThrow(RangeError)
  })

  it.each([0n, -1n, BigInt(Number.MAX_SAFE_INTEGER) + 1n])(
    'rejects an invalid minimum advance: %s',
    async (minimumAdvanceBytes) => {
      const policy = { minimumAdvanceBytes }
      expect(() => decideDirectZipAutomaticCheckpointV1({
        archiveOffset: 1n, committedLength: 0n, policy,
      })).toThrow(RangeError)
      await expect(directZipEpochPolicyDigestV2(policy)).rejects.toThrow(RangeError)
    },
  )
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
