import type { ArtifactChoiceID, ArtifactSpec, MaterializationPlan } from '../../../transfer/intent'
import type { ProjectionEpoch, SelectionProjectionV1 } from '../../../transfer/projection/contracts'
import type { OfferedArtifactChoice } from './choices'

export type ZipRouteRecommendation =
  | Readonly<{
      kind: 'recommended'
      choiceId: ArtifactChoiceID
      reason: 'workspace-within-policy-budget' | 'workspace-within-discovered-budget' |
        'direct-unknown-or-over-budget'
    }>
  | Readonly<{
      kind: 'no-recommendation'
      reason:
        | 'only-one-route-available'
        | 'no-browser-zip-route'
        | 'discovery-incomplete'
        | 'workspace-cost-unavailable'
        | 'recommendation-policy-unavailable'
    }>

export interface ZipRouteGroup {
  readonly kind: 'zip-route-group'
  readonly primary: OfferedArtifactChoice
  readonly secondary: OfferedArtifactChoice | null
  readonly recommendation: ZipRouteRecommendation
  readonly fallback: Readonly<{
    kind: 'native-recommended' | 'none'
    reason?: 'no-supported-browser-zip-route'
  }>
}

export type OfferUnavailableReason =
  | 'shape-unsettled'
  | 'selection-empty'
  | 'discovery-retry-required'
  | 'no-safe-destination'
  | 'permission-denied'
  | 'capability-changed'
  | 'portable-limit-exceeded'
  | 'workspace-limit-exceeded'

export interface OfferComputedDecision {
  readonly name: 'receive.offer.computed'
  readonly projection_epoch: ProjectionEpoch
  readonly shape_proof: SelectionProjectionV1['proof']['kind']
  readonly offered_artifact_kinds: readonly ArtifactSpec['kind'][]
  readonly offered_plan_kinds: readonly MaterializationPlan['kind'][]
  readonly primary_artifact_kind: ArtifactSpec['kind']
}

export interface OfferDisabledDecision {
  readonly name: 'receive.offer.disabled'
  readonly projection_epoch: ProjectionEpoch
  readonly shape_proof: SelectionProjectionV1['proof']['kind']
  readonly offer_unavailable_reason: OfferUnavailableReason
  readonly hard_limit_class?: 'portable-artifact' | 'workspace-job' | 'workspace-process'
}

export interface ConfirmingSelectedContentOffer {
  readonly kind: 'confirming-selected-content'
  readonly interactive: false
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly reason: 'shape-unsettled'
  readonly decision: OfferDisabledDecision
}

export interface RetryConfirmationOffer {
  readonly kind: 'retry-confirmation'
  readonly interactive: true
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly reason: 'discovery-retry-required'
  readonly decision: OfferDisabledDecision
}

export interface SelectionEmptyOffer {
  readonly kind: 'selection-empty'
  readonly interactive: false
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly reason: 'selection-empty'
  readonly decision: OfferDisabledDecision
}

export interface NoSafeDestinationOffer {
  readonly kind: 'no-safe-destination'
  readonly interactive: false
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly reason:
    | 'no-safe-destination'
    | 'portable-limit-exceeded'
    | 'workspace-limit-exceeded'
  readonly decision: OfferDisabledDecision
  readonly fallback: Readonly<{
    kind: 'native-recommended'
    reason: 'no-supported-browser-zip-route'
  }>
}

export interface ArtifactActionsOffer {
  readonly kind: 'artifact-actions'
  readonly interactive: true
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly primary: OfferedArtifactChoice
  readonly alternatives: readonly OfferedArtifactChoice[]
  readonly zip: ZipRouteGroup | null
  readonly decision: OfferComputedDecision
}

export type ArtifactOffers =
  | ConfirmingSelectedContentOffer
  | RetryConfirmationOffer
  | SelectionEmptyOffer
  | NoSafeDestinationOffer
  | ArtifactActionsOffer
