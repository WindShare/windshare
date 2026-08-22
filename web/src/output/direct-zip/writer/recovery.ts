import type { DirectZipCandidateObservationV1 } from './ports'

export type DirectZipCandidateRecoveryDecisionV1 =
  | Readonly<{ kind: 'replay-predecessor' }>
  | Readonly<{ kind: 'verify-candidate-range' }>
  | Readonly<{ kind: 'promote-candidate' }>
  | Readonly<{ kind: 'verify-predecessor-epochs' }>
  | Readonly<{ kind: 'truncate-and-replay' }>
  | Readonly<{ kind: 'authorization-required' }>
  | Readonly<{ kind: 'destination-space-required' }>
  | Readonly<{ kind: 'restart-required'; reason: 'target-deleted' }>
  | Readonly<{ kind: 'target-verification-required' }>
  | Readonly<{ kind: 'needs-attention'; reason: 'foreign-target' }>

export function decideDirectZipCandidateRecoveryV1(
  observation: DirectZipCandidateObservationV1,
): DirectZipCandidateRecoveryDecisionV1 {
  const authorityDecision = decideAuthorityGate(observation)
  if (authorityDecision !== undefined) return authorityDecision
  if (observation.length === 'predecessor' &&
      observation.observationMatch === 'predecessor') {
    return Object.freeze({ kind: 'replay-predecessor' })
  }
  if (observation.length === 'candidate' && observation.observationMatch === 'candidate') {
    return decideCandidateIntegrity(observation)
  }
  if (observation.length === 'unknown-tail') {
    return decideUnknownTail(observation)
  }
  return Object.freeze({ kind: 'target-verification-required' })
}

function decideAuthorityGate(
  observation: DirectZipCandidateObservationV1,
): DirectZipCandidateRecoveryDecisionV1 | undefined {
  if (observation.permission === 'unavailable') {
    return Object.freeze({ kind: 'authorization-required' })
  }
  if (observation.presence === 'deleted') {
    return Object.freeze({ kind: 'restart-required', reason: 'target-deleted' })
  }
  if (observation.ownership === 'foreign') {
    return Object.freeze({ kind: 'needs-attention', reason: 'foreign-target' })
  }
  if (observation.ownership === 'ambiguous') {
    return Object.freeze({ kind: 'target-verification-required' })
  }
  if (observation.destinationSpaceFailure === true &&
      observation.candidateResolvedBeforeSpaceFailure === true) {
    return Object.freeze({ kind: 'destination-space-required' })
  }
  return undefined
}

function decideCandidateIntegrity(
  observation: DirectZipCandidateObservationV1,
): DirectZipCandidateRecoveryDecisionV1 {
  if (observation.candidateIntegrity === 'writer-bounded-proof' ||
      observation.candidateIntegrity === 'verified') {
    return Object.freeze({ kind: 'promote-candidate' })
  }
  if (observation.candidateIntegrity === 'not-read') {
    return Object.freeze({ kind: 'verify-candidate-range' })
  }
  return Object.freeze({ kind: 'target-verification-required' })
}

function decideUnknownTail(
  observation: DirectZipCandidateObservationV1,
): DirectZipCandidateRecoveryDecisionV1 {
  if (observation.predecessorIntegrity === 'verified') {
    return Object.freeze({ kind: 'truncate-and-replay' })
  }
  if (observation.predecessorIntegrity === 'not-read') {
    return Object.freeze({ kind: 'verify-predecessor-epochs' })
  }
  return Object.freeze({ kind: 'target-verification-required' })
}
