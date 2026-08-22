import type { DirectZipCandidateRecoveryDecisionV1 } from './recovery'
import type { DirectZipTargetVerificationPort } from './ports'

export type DirectZipWriterGateV1 =
  | 'authorization-required'
  | 'destination-space-required'
  | 'target-deleted'
  | 'target-verification-required'
  | 'needs-attention'

export class DirectZipWriterGateError extends Error {
  readonly gate: DirectZipWriterGateV1

  constructor(gate: DirectZipWriterGateV1, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'DirectZipWriterGateError'
    this.gate = gate
  }
}

export function recoveryDecisionLabel(decision: DirectZipCandidateRecoveryDecisionV1): string {
  return decision.kind === 'restart-required' || decision.kind === 'needs-attention'
    ? `${decision.kind}:${decision.reason}`
    : decision.kind
}

export function gateFromPredecessor(
  kind: Exclude<Awaited<ReturnType<DirectZipTargetVerificationPort['verifyPredecessor']>>['kind'],
    'accepted-fast' | 'digest-readback-required'>,
): DirectZipWriterGateError {
  const gate = kind === 'foreign-target' ? 'needs-attention' : kind
  return new DirectZipWriterGateError(gate, `direct ZIP predecessor verification stopped: ${kind}`)
}

export function gateFromOpen(
  kind: Exclude<Awaited<ReturnType<DirectZipTargetVerificationPort['openEpoch']>>['kind'], 'opened'>,
): DirectZipWriterGateError {
  return new DirectZipWriterGateError(kind, `direct ZIP epoch open stopped: ${kind}`)
}

export function gateFromTruncate(
  kind: Exclude<Awaited<ReturnType<DirectZipTargetVerificationPort['truncateToPredecessor']>>['kind'],
    'truncated'>,
): DirectZipWriterGateError {
  const gate = kind === 'refused' ? 'needs-attention' : kind
  return new DirectZipWriterGateError(gate, `direct ZIP truncate stopped: ${kind}`)
}

export function gateFromRecovery(decision: Exclude<DirectZipCandidateRecoveryDecisionV1,
  { readonly kind: 'promote-candidate' | 'replay-predecessor' | 'truncate-and-replay' }>):
  DirectZipWriterGateError {
  if (decision.kind === 'authorization-required' || decision.kind === 'destination-space-required' ||
      decision.kind === 'target-verification-required') {
    return new DirectZipWriterGateError(decision.kind, `direct ZIP recovery stopped: ${decision.kind}`)
  }
  if (decision.kind === 'restart-required') {
    return new DirectZipWriterGateError('target-deleted', 'direct ZIP target was deleted')
  }
  if (decision.kind === 'needs-attention') {
    return new DirectZipWriterGateError('needs-attention', 'direct ZIP target is foreign')
  }
  return new DirectZipWriterGateError(
    'target-verification-required',
    `direct ZIP recovery proof was incomplete: ${decision.kind}`,
  )
}
