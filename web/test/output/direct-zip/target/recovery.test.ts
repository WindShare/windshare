import { describe, expect, it } from 'vitest'

import {
  decideDirectZipRecovery,
  type DirectZipProofStatus,
  type DirectZipTargetObservationV1,
} from '../../../../src/output/direct-zip/target'
import { bytes } from './staged-fsa-model'

const PREDECESSOR_LENGTH = 100n
const CANDIDATE_LENGTH = 140n
const EPOCH = Object.freeze({
  start: 100n,
  end: 140n,
  predecessorRoot: bytes(32, 1),
  epochRoot: bytes(32, 2),
})

describe('DirectZip predecessor/candidate ambiguity decisions', () => {
  it.each([
    {
      label: 'unchanged predecessor',
      target: observation(PREDECESSOR_LENGTH),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: { kind: 'replay-predecessor' },
    },
    {
      label: 'verified candidate',
      target: observation(CANDIDATE_LENGTH, 200),
      predecessorProof: 'unchecked',
      candidateProof: 'verified',
      expected: { kind: 'promote-candidate' },
    },
    {
      label: 'unknown tail with verified predecessor',
      target: observation(130n, 300),
      predecessorProof: 'verified',
      candidateProof: 'unchecked',
      expected: { kind: 'truncate-to-predecessor' },
    },
    {
      label: 'bad candidate with verified predecessor',
      target: observation(CANDIDATE_LENGTH, 300),
      predecessorProof: 'verified',
      candidateProof: 'mismatch',
      expected: { kind: 'truncate-to-predecessor' },
    },
    {
      label: 'candidate requiring range proof',
      target: observation(CANDIDATE_LENGTH, 200),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: {
        kind: 'target-verification-required',
        reason: 'candidate-ambiguous',
        proof: 'candidate-range',
      },
    },
    {
      label: 'changed predecessor requiring epoch proof',
      target: observation(PREDECESSOR_LENGTH, 400),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: {
        kind: 'target-verification-required',
        reason: 'observation-changed',
        proof: 'predecessor-epochs',
      },
    },
    {
      label: 'mismatching committed prefix',
      target: observation(PREDECESSOR_LENGTH, 400),
      predecessorProof: 'mismatch',
      candidateProof: 'unchecked',
      expected: { kind: 'needs-attention', reason: 'committed-prefix-mismatch' },
    },
    {
      label: 'short committed prefix',
      target: observation(99n),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: { kind: 'needs-attention', reason: 'committed-prefix-lost' },
    },
    {
      label: 'valid foreign marker',
      target: observation(PREDECESSOR_LENGTH, 100, 'foreign'),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: { kind: 'needs-attention', reason: 'foreign-replacement' },
    },
    {
      label: 'partial marker',
      target: observation(12n, 100, 'partial'),
      predecessorProof: 'unchecked',
      candidateProof: 'unchecked',
      expected: {
        kind: 'target-verification-required',
        reason: 'ownership-marker-incomplete',
      },
      predecessorLength: 0n,
    },
  ] as const)(
    'chooses $expected.kind for $label',
    ({ target, predecessorProof, candidateProof, expected, predecessorLength }) => {
      const committedLength = predecessorLength ?? PREDECESSOR_LENGTH
      const predecessorObservation = observation(committedLength)
      const decision = decideDirectZipRecovery({
        target,
        predecessor: {
          committedLength,
          observation: predecessorObservation,
          committedEpochs: [],
        },
        candidate: {
          stagedEnd: CANDIDATE_LENGTH,
          observation: observation(CANDIDATE_LENGTH, 200),
          epoch: EPOCH,
        },
        predecessorProof: predecessorProof as DirectZipProofStatus,
        candidateProof: candidateProof as DirectZipProofStatus,
      })
      expect(decision).toMatchObject(expected)
    },
  )

  it('never reuses a target under a changed parent binding without fresh authority evidence', () => {
    const target = Object.freeze({ ...observation(PREDECESSOR_LENGTH), parentLocator: 'different' as const })
    expect(decideDirectZipRecovery({
      target,
      predecessor: {
        committedLength: PREDECESSOR_LENGTH,
        observation: observation(PREDECESSOR_LENGTH),
        committedEpochs: [],
      },
      predecessorProof: 'verified',
      candidateProof: 'unchecked',
    })).toMatchObject({
      kind: 'target-verification-required',
      reason: 'parent-binding-changed',
    })
  })
})

function observation(
  size: bigint,
  lastModified = 100,
  markerKind: 'matching' | 'foreign' | 'partial' | 'malformed' = 'matching',
): DirectZipTargetObservationV1 {
  const marker = markerKind === 'matching'
    ? Object.freeze({ kind: 'matching' as const, prefixLength: 80n })
    : Object.freeze({ kind: markerKind })
  return Object.freeze({
    size,
    lastModified,
    marker,
    parentLocator: 'same',
    fileLocator: 'same',
  })
}
