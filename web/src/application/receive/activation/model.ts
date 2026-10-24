import type {
  ArtifactChoice,
  ArtifactChoiceInvalidationReason,
  MaterializationRouteIdentity,
  ProjectionEpoch,
  ResolvedArtifactAction,
  RetryableDiscoveryReason,
} from '../../../output/planning'
import type { ArtifactChoiceID } from '../../../transfer/intent'

export interface ActivationObservation {
  /**
   * Revision, rather than session or epoch identity, fences asynchronous planner
   * completions because both observations may be replaced without revoking local authority.
   */
  readonly revision: number
  readonly protocolSessionId: string
  readonly projectionEpoch: ProjectionEpoch
}

export type ActivationInvalidationReason =
  | ArtifactChoiceInvalidationReason
  | 'authenticated-share-instance-changed'
  | 'installed-route-changed'
  | 'caller-cancelled'

export interface LiveAuthorityActivationSnapshot {
  readonly activationId: string
  readonly authenticatedShareInstanceId: string
  readonly selectionDigest: string
  readonly choice: ArtifactChoice
  /** The initial route identity is retained so a refresh cannot silently switch authority. */
  readonly installedRoute: MaterializationRouteIdentity
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly observation: ActivationObservation
}

export type WaitingAuthorityResolution =
  | Readonly<{ readonly kind: 'waiting' }>
  | Readonly<{
      readonly kind: 'resolved'
      readonly action: ResolvedArtifactAction
    }>

export type AuthorityActivationTerminalOutcome =
  | Readonly<{
      readonly kind: 'bound-operation'
      readonly operationId: string
    }>
  | Readonly<{
      readonly kind: 'owned-effects-settled'
      readonly operationId: string
    }>
  | Readonly<{ readonly kind: 'picker-refused' }>
  | Readonly<{
      readonly kind: 'invalidated'
      readonly reason: ActivationInvalidationReason
    }>
  | Readonly<{ readonly kind: 'failed' }>

export type ActivationCleanupOwnerKind = 'owned-effects' | 'bound-operation'
export type ActivationCleanupStage = 'settlement' | 'detach'

export type AuthorityActivationSnapshot =
  | Readonly<{ readonly kind: 'inactive' }>
  | (LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'waiting-authority'
      /** Resolution may win the race while the non-cancellable picker remains pending. */
      readonly resolution: WaitingAuthorityResolution
    }>)
  | (LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'waiting-resolution'
    }>)
  | (LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'retry-required'
      readonly authorityReady: boolean
      readonly reason: RetryableDiscoveryReason
    }>)
  | (LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'committing'
      readonly action: ResolvedArtifactAction
    }>)
  | (LiveAuthorityActivationSnapshot & Readonly<{
      /** Durable ownership keeps admission closed until both cleanup stages are proven. */
      readonly kind: 'cleanup-required'
      readonly operationId: string
      readonly ownerKind: ActivationCleanupOwnerKind
      readonly failedStage: ActivationCleanupStage
      readonly settlementComplete: boolean
      readonly detachComplete: boolean
    }>)
  | (LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'terminal'
      readonly outcome: AuthorityActivationTerminalOutcome
    }>)

/**
 * Ordinary invalidation is modeled in snapshots; this error is reserved for an
 * impossible coordinator transition so diagnostics can classify it as Contract.
 */
export class ActivationStateContractError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'ActivationStateContractError'
  }
}

export function activationOwnsChoice(
  activation: AuthorityActivationSnapshot,
): activation is Extract<AuthorityActivationSnapshot, {
  readonly kind: 'waiting-authority' | 'waiting-resolution' | 'retry-required' | 'committing' |
    'cleanup-required'
}> {
  switch (activation.kind) {
    case 'waiting-authority':
    case 'waiting-resolution':
    case 'retry-required':
    case 'committing':
    case 'cleanup-required':
      return true
    case 'inactive':
    case 'terminal':
      return false
  }
}
