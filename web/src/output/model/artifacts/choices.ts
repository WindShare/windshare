import type { ArtifactChoiceID, ArtifactChoiceIdentity, ArtifactSpec } from '../../../transfer/intent'
import type { OfferedMaterializationPlanSemantics, OfferedMaterializationRoute } from './destinations'
import type { ArtifactSizeProjection } from './projections'

export type ArtifactOperation =
  | 'download-original'
  | 'save-single-to-folder'
  | 'save-directory-tree'
  | 'download-zip'
  | 'check-then-download'

export type RecoverySemantics =
  | 'checkpoint-resumable'
  | 'workspace-resumable'
  | 'restart-required'
  | 'none'

export interface PreparationRequirement {
  readonly manifest: 'none' | 'exact-zip' | 'exact-artifact'
  readonly hardAdmission: 'none' | 'workspace-budget' | 'portable-artifact'
}

export type ArtifactChoiceSemantics = Readonly<{
  kind: 'artifact-choice'
  operation: ArtifactOperation
  artifactKind: ArtifactSpec['kind']
  recovery: RecoverySemantics
  preparation: PreparationRequirement
  plan: OfferedMaterializationPlanSemantics
}>

export interface StableArtifactChoiceIdentity {
  readonly choiceIdentity: ArtifactChoiceIdentity
  readonly choiceId: ArtifactChoiceID
}

export interface ArtifactChoice extends ArtifactChoiceSemantics, StableArtifactChoiceIdentity {}

export interface OfferedArtifactChoice {
  readonly kind: 'offered-artifact-choice'
  readonly choice: ArtifactChoice
  readonly route: OfferedMaterializationRoute
  readonly suggestedName: string | null
  readonly importance: 'primary' | 'secondary'
  readonly sizeProjection: ArtifactSizeProjection
}
