import type {
  DirectZipCandidateTargetExpectation,
  DirectZipCheckpointTargetExpectation,
  DirectZipLifecycleDecision,
  DirectZipProofStatus,
  DirectZipRecoveryResolution,
  DirectZipTargetObservationV1,
} from './model'
import { sameDirectZipTargetObservation } from './model'

export interface DirectZipRecoveryEvidence {
  readonly target: 'absent' | DirectZipTargetObservationV1
  readonly predecessor: DirectZipCheckpointTargetExpectation
  readonly candidate?: DirectZipCandidateTargetExpectation
  readonly predecessorProof: DirectZipProofStatus
  readonly candidateProof: DirectZipProofStatus
}

export function decideDirectZipRecovery(
  evidence: DirectZipRecoveryEvidence,
): DirectZipRecoveryResolution | DirectZipLifecycleDecision {
  if (evidence.target === 'absent') {
    return Object.freeze({
      kind: 'restart-required',
      stage: 'snapshot',
      reason: 'target-deleted',
    })
  }
  const target = evidence.target
  if (target.parentLocator !== 'same') {
    return verification('parent-binding-changed', 'fresh-observation')
  }
  const markerDecision = decisionForMarker(target)
  if (markerDecision !== undefined) return markerDecision
  if (target.size < evidence.predecessor.committedLength) {
    return attention('committed-prefix-lost')
  }
  const candidateDecision = decisionForCandidate(evidence)
  if (candidateDecision !== undefined) return candidateDecision
  if (target.size === evidence.predecessor.committedLength) {
    return decisionAtPredecessor({ ...evidence, target })
  }
  return decisionForUnknownTail(evidence.predecessorProof)
}

function decisionForMarker(
  target: DirectZipTargetObservationV1,
): DirectZipLifecycleDecision | undefined {
  switch (target.marker.kind) {
    case 'matching':
      return undefined
    case 'foreign':
      return attention('foreign-replacement')
    case 'partial':
      return verification('ownership-marker-incomplete', 'ownership-marker')
    case 'malformed':
      return verification('ownership-marker-malformed', 'ownership-marker')
  }
}

function decisionForCandidate(
  evidence: DirectZipRecoveryEvidence,
): DirectZipRecoveryResolution | DirectZipLifecycleDecision | undefined {
  const candidate = evidence.candidate
  if (candidate === undefined || evidence.target === 'absent' ||
      evidence.target.size !== candidate.stagedEnd) return undefined
  const candidateObserved = sameDirectZipTargetObservation(evidence.target, candidate.observation)
  if (evidence.candidateProof === 'verified' &&
      (candidateObserved || evidence.predecessorProof === 'verified')) {
    return Object.freeze({ kind: 'promote-candidate' })
  }
  if (evidence.candidateProof === 'unchecked') {
    return verification('candidate-ambiguous', 'candidate-range')
  }
  if (evidence.candidateProof === 'mismatch') {
    return predecessorFallback(evidence.predecessorProof)
  }
  return evidence.predecessorProof === 'mismatch'
    ? attention('committed-prefix-mismatch')
    : verification('observation-changed', 'predecessor-epochs')
}

function decisionAtPredecessor(
  evidence: DirectZipRecoveryEvidence & { readonly target: DirectZipTargetObservationV1 },
): DirectZipRecoveryResolution | DirectZipLifecycleDecision {
  if (sameDirectZipTargetObservation(evidence.target, evidence.predecessor.observation) ||
      evidence.predecessorProof === 'verified') {
    return Object.freeze({ kind: 'replay-predecessor' })
  }
  return evidence.predecessorProof === 'mismatch'
    ? attention('committed-prefix-mismatch')
    : verification('observation-changed', 'predecessor-epochs')
}

function decisionForUnknownTail(
  proof: DirectZipProofStatus,
): DirectZipRecoveryResolution | DirectZipLifecycleDecision {
  if (proof === 'verified') return Object.freeze({ kind: 'truncate-to-predecessor' })
  return proof === 'mismatch'
    ? attention('committed-prefix-mismatch')
    : verification('unknown-tail', 'predecessor-epochs')
}

function predecessorFallback(
  proof: DirectZipProofStatus,
): DirectZipRecoveryResolution | DirectZipLifecycleDecision {
  if (proof === 'verified') return Object.freeze({ kind: 'truncate-to-predecessor' })
  if (proof === 'mismatch') return attention('committed-prefix-mismatch')
  return verification('candidate-ambiguous', 'predecessor-epochs')
}

function verification(
  reason: Extract<DirectZipLifecycleDecision, { readonly kind: 'target-verification-required' }>['reason'],
  proof: Extract<DirectZipLifecycleDecision, { readonly kind: 'target-verification-required' }>['proof'],
): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'target-verification-required',
    stage: 'snapshot',
    reason,
    proof,
  })
}

function attention(
  reason: Extract<DirectZipLifecycleDecision, { readonly kind: 'needs-attention' }>['reason'],
): DirectZipLifecycleDecision {
  return Object.freeze({ kind: 'needs-attention', stage: 'snapshot', reason })
}
