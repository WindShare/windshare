import type {
  ArtifactSpec,
  DeliveryMode,
  GuaranteeProfile,
  MaterializationPlan,
  NameAuthority,
  ReplacementGuarantee,
  RollbackGuarantee,
  CommitVisibility,
} from '../../transfer/intent'
import type {
  DiscoveryState,
  ProjectionEpoch,
  SelectionProjectionV1,
} from '../../transfer/projection'

export type { DiscoveryState, ProjectionEpoch, SelectionProjectionV1 }

export interface DestinationGuaranteeFacts {
  readonly nameAuthority: NameAuthority
  readonly replacement: ReplacementGuarantee
  readonly delivery: DeliveryMode
  readonly visibility: CommitVisibility
  readonly rollback: RollbackGuarantee
}

interface EnvironmentTargetOfferBase<
  Kind extends EnvironmentTargetKind,
  Persistence extends TargetAuthorityPersistence,
  Profile extends GuaranteeProfile | null,
> {
  readonly id: string
  readonly kind: Kind
  readonly guarantees: DestinationGuaranteeFacts
  readonly persistence: Persistence
  readonly hardMaximumOutputBytes: bigint | null
  readonly legalProfile: Profile
}

export type NativeDirectoryContainerOffer = EnvironmentTargetOfferBase<
  'native-directory-container',
  'durable-authority',
  'native-tree'
>

export type FSADirectoryContainerOffer = EnvironmentTargetOfferBase<
  'fsa-parent-directory',
  'durable-after-repository-commit',
  'fsa-tree'
>

export type ManagedAtomicTargetOffer = EnvironmentTargetOfferBase<
  'managed-atomic-file-target',
  'operation-scoped',
  'managed-atomic'
>

export type BrowserHandoffTargetOffer = EnvironmentTargetOfferBase<
  'browser-handoff',
  'none',
  'browser-handoff'
> & Readonly<{
  readonly objectUrlLeaseMilliseconds: bigint
  readonly supportsWorkspacePackage: boolean
  readonly supportsPortableArtifact: boolean
}>

export type PrecreatedBrowserFileOffer = EnvironmentTargetOfferBase<
  'precreated-browser-file',
  'operation-scoped',
  null
>

export type EnvironmentTargetOffer =
  | NativeDirectoryContainerOffer
  | FSADirectoryContainerOffer
  | ManagedAtomicTargetOffer
  | BrowserHandoffTargetOffer
  | PrecreatedBrowserFileOffer

export type EnvironmentTargetKind =
  | 'native-directory-container'
  | 'fsa-parent-directory'
  | 'managed-atomic-file-target'
  | 'browser-handoff'
  | 'precreated-browser-file'

export type TargetAuthorityPersistence =
  | 'durable-authority'
  | 'durable-after-repository-commit'
  | 'operation-scoped'
  | 'none'

type WithoutLegalProfile<T> = T extends EnvironmentTargetOffer
  ? Omit<T, 'legalProfile'>
  : never

export type EnvironmentTargetOfferInput = WithoutLegalProfile<EnvironmentTargetOffer>

export interface WorkspaceEnvironmentOffer {
  readonly id: string
  readonly kind: 'origin-private-workspace'
  readonly persistence: 'durable-owned-repository'
  readonly jobHardLimitBytes: bigint
  readonly processHardLimitBytes: bigint
  readonly minimumQuotaReserveBytes: bigint
  readonly quotaAvailabilityEstimateBytes: bigint | null
}

export interface PortableEnvironmentOffer {
  readonly id: string
  readonly kind: 'portable-memory'
  readonly persistence: 'none'
  readonly maximumArtifactBytes: bigint
  readonly assemblyPartBytes: bigint
  readonly maximumParts: bigint
  readonly objectUrlLeaseMilliseconds: bigint
}

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

export interface DirectTreePlanOffer {
  readonly kind: 'direct-tree'
  readonly target: NativeDirectoryContainerOffer | FSADirectoryContainerOffer
}

export interface DirectAtomicPlanOffer {
  readonly kind: 'direct-atomic'
  readonly target: ManagedAtomicTargetOffer
}

export interface WorkspaceThenPublishPlanOffer {
  readonly kind: 'workspace-then-publish'
  readonly workspace: WorkspaceEnvironmentOffer
  readonly publicationTarget: ManagedAtomicTargetOffer | BrowserHandoffTargetOffer
}

export interface PortableHandoffPlanOffer {
  readonly kind: 'portable-handoff'
  readonly portable: PortableEnvironmentOffer
  readonly handoffTarget: BrowserHandoffTargetOffer
}

export type OfferedMaterializationPlan =
  | DirectTreePlanOffer
  | DirectAtomicPlanOffer
  | WorkspaceThenPublishPlanOffer
  | PortableHandoffPlanOffer

export interface ArtifactAction {
  readonly kind: 'artifact-action'
  readonly projectionEpoch: ProjectionEpoch
  readonly operation: ArtifactOperation
  readonly artifactKind: ArtifactSpec['kind']
  readonly artifact: ArtifactSpec | null
  readonly suggestedName: string | null
  readonly importance: 'primary' | 'secondary'
  readonly recovery: RecoverySemantics
  readonly preparation: PreparationRequirement
  readonly plan: OfferedMaterializationPlan
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
  readonly reason: 'shape-unsettled'
  readonly decision: OfferDisabledDecision
}

export interface RetryConfirmationOffer {
  readonly kind: 'retry-confirmation'
  readonly interactive: true
  readonly projectionEpoch: ProjectionEpoch
  readonly reason: 'discovery-retry-required'
  readonly decision: OfferDisabledDecision
}

export interface SelectionEmptyOffer {
  readonly kind: 'selection-empty'
  readonly interactive: false
  readonly projectionEpoch: ProjectionEpoch
  readonly reason: 'selection-empty'
  readonly decision: OfferDisabledDecision
}

export interface NoSafeDestinationOffer {
  readonly kind: 'no-safe-destination'
  readonly interactive: false
  readonly projectionEpoch: ProjectionEpoch
  readonly reason:
    | 'no-safe-destination'
    | 'portable-limit-exceeded'
    | 'workspace-limit-exceeded'
  readonly decision: OfferDisabledDecision
}

export interface ArtifactActionsOffer {
  readonly kind: 'artifact-actions'
  readonly interactive: true
  readonly projectionEpoch: ProjectionEpoch
  readonly primary: ArtifactAction
  readonly alternatives: readonly ArtifactAction[]
  readonly decision: OfferComputedDecision
}

export type ArtifactOffers =
  | ConfirmingSelectedContentOffer
  | RetryConfirmationOffer
  | SelectionEmptyOffer
  | NoSafeDestinationOffer
  | ArtifactActionsOffer
