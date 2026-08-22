import type {
  DirectZipRecoveryDecisionV1,
  DirectZipRecoveryEvidenceV1,
} from './model'

export function decideDirectZipRecoveryV1(
  evidence: DirectZipRecoveryEvidenceV1,
): DirectZipRecoveryDecisionV1 {
  if (evidence === null || typeof evidence !== 'object') {
    throw new TypeError('Direct ZIP recovery evidence is invalid')
  }
  if (evidence.permission === 'unavailable') {
    return Object.freeze({ kind: 'authorization-required' })
  }
  if (evidence.permission !== 'granted') {
    throw new TypeError('Direct ZIP recovery permission observation is invalid')
  }
  if (evidence.target === 'absent') {
    return Object.freeze({ kind: 'restart-required', reason: 'target-deleted' })
  }
  if (evidence.target === 'foreign') {
    return Object.freeze({ kind: 'needs-attention', reason: 'target-ownership-unknown' })
  }
  if (evidence.candidateAlreadyResolved && !evidence.destinationSpaceAvailable) {
    return Object.freeze({ kind: 'destination-space-required' })
  }
  if (!evidence.markerMatches) {
    return Object.freeze({ kind: 'target-verification-required' })
  }
  return decideVerifiedDirectZipTargetV1(evidence)
}

function decideVerifiedDirectZipTargetV1(
  evidence: DirectZipRecoveryEvidenceV1,
): DirectZipRecoveryDecisionV1 {
  switch (evidence.target) {
    case 'predecessor':
      return evidence.predecessorEpochsVerified
        ? Object.freeze({ kind: 'retire-and-replay' })
        : Object.freeze({ kind: 'target-verification-required' })
    case 'candidate':
      return evidence.candidateRangeVerified
        ? Object.freeze({ kind: 'promote-candidate' })
        : Object.freeze({ kind: 'target-verification-required' })
    case 'unknown-tail':
      if (!evidence.predecessorEpochsVerified) {
        return Object.freeze({ kind: 'target-verification-required' })
      }
      if (!evidence.destinationSpaceAvailable) {
        return Object.freeze({ kind: 'destination-space-required' })
      }
      return Object.freeze({ kind: 'truncate-to-predecessor-and-replay' })
    case 'ambiguous':
      return Object.freeze({ kind: 'target-verification-required' })
    default:
      throw new TypeError('Direct ZIP recovery target observation is invalid')
  }
}
