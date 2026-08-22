import type {
  ArtifactChoice,
  ArtifactChoiceInvalidationReason,
  MaterializationRouteIdentity,
  ProjectionEpoch,
  ResolvedArtifactAction,
  RetryableDiscoveryReason,
} from '../../output/planning'
import type { ArtifactChoiceID } from '../../transfer/intent'

export interface V2ActivationObservation {
  /**
   * Revision, rather than session or epoch identity, fences asynchronous planner
   * completions because both observations may be replaced without revoking local authority.
   */
  readonly revision: number
  readonly protocolSessionId: string
  readonly projectionEpoch: ProjectionEpoch
}

export type V2ActivationInvalidationReason =
  | ArtifactChoiceInvalidationReason
  | 'authenticated-share-instance-changed'
  | 'installed-route-changed'
  | 'caller-cancelled'

export interface V2LiveAuthorityActivationSnapshot {
  readonly activationId: string
  readonly authenticatedShareInstanceId: string
  readonly selectionDigest: string
  readonly choice: ArtifactChoice
  /** The initial route identity is retained so a refresh cannot silently switch authority. */
  readonly installedRoute: MaterializationRouteIdentity
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly observation: V2ActivationObservation
}

export type V2WaitingAuthorityResolution =
  | Readonly<{ readonly kind: 'waiting' }>
  | Readonly<{
      readonly kind: 'resolved'
      readonly action: ResolvedArtifactAction
    }>

export type V2AuthorityActivationTerminalOutcome =
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
      readonly reason: V2ActivationInvalidationReason
    }>
  | Readonly<{ readonly kind: 'failed' }>

export type V2ActivationCleanupOwnerKind = 'owned-effects' | 'bound-operation'
export type V2ActivationCleanupStage = 'settlement' | 'detach'

export type V2AuthorityActivationSnapshot =
  | Readonly<{ readonly kind: 'inactive' }>
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'waiting-authority'
      /** Resolution may win the race while the non-cancellable picker remains pending. */
      readonly resolution: V2WaitingAuthorityResolution
    }>)
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'waiting-resolution'
    }>)
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'retry-required'
      readonly authorityReady: boolean
      readonly reason: RetryableDiscoveryReason
    }>)
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'committing'
      readonly action: ResolvedArtifactAction
    }>)
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      /** Durable ownership keeps admission closed until both cleanup stages are proven. */
      readonly kind: 'cleanup-required'
      readonly operationId: string
      readonly ownerKind: V2ActivationCleanupOwnerKind
      readonly failedStage: V2ActivationCleanupStage
      readonly settlementComplete: boolean
      readonly detachComplete: boolean
    }>)
  | (V2LiveAuthorityActivationSnapshot & Readonly<{
      readonly kind: 'terminal'
      readonly outcome: V2AuthorityActivationTerminalOutcome
    }>)

/**
 * Ordinary invalidation is modeled in snapshots; this error is reserved for an
 * impossible coordinator transition so diagnostics can classify it as Contract.
 */
export class V2ActivationStateContractError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2ActivationStateContractError'
  }
}
