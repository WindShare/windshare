import type { ArtifactSpec } from '../../transfer/intent'
import type { ProjectionEpoch } from '../../transfer/projection/contracts'
import type {
  EnvironmentTargetOffer,
  WorkspaceEnvironmentOffer,
  PortableEnvironmentOffer,
  OfferedMaterializationRoute,
} from '../model/artifacts/destinations'
import type { StableArtifactChoiceIdentity, ArtifactChoice } from '../model/artifacts/choices'

type WithoutLegalProfile<T> = T extends EnvironmentTargetOffer
  ? Omit<T, 'legalProfile'>
  : never

export type EnvironmentTargetOfferInput = WithoutLegalProfile<EnvironmentTargetOffer>

export interface EnvironmentOffers {
  readonly targets: readonly EnvironmentTargetOffer[]
  readonly workspace: WorkspaceEnvironmentOffer | null
  readonly portable: PortableEnvironmentOffer | null
}

export interface EnvironmentOffersInput {
  readonly targets: readonly EnvironmentTargetOfferInput[]
  readonly workspace?: WorkspaceEnvironmentOffer | null
  readonly portable?: PortableEnvironmentOffer | null
}

export type MaterializationRouteIdentity =
  | Readonly<{ kind: 'direct'; targetRouteId: string }>
  | Readonly<{
      kind: 'workspace'
      workspaceRouteId: string
      publicationTargetRouteId: string
    }>
  | Readonly<{
      kind: 'portable'
      portableRouteId: string
      handoffTargetRouteId: string
    }>

export interface ResolvedArtifactAction extends StableArtifactChoiceIdentity {
  readonly kind: 'resolved-artifact-action'
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly resolvedArtifactDigest: string
  readonly choice: ArtifactChoice
  readonly route: OfferedMaterializationRoute
  readonly artifact: ArtifactSpec
}

export interface ArtifactResolutionObservation {
  readonly projectionEpoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly resolvedArtifactDigest: string | null
}
