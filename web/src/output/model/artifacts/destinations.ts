import type {
  ArtifactAvailability,
  CleanupContract,
  DeliveryMode,
  GuaranteeProfile,
  NameChooser,
  ReplacementGuarantee,
  TargetVisibility,
} from '../../../transfer/intent'

export interface DestinationGuaranteeFacts {
  readonly nameChooser: NameChooser
  readonly replacement: ReplacementGuarantee
  readonly delivery: DeliveryMode
  readonly targetVisibility: TargetVisibility
  readonly artifactAvailability: ArtifactAvailability
  readonly cleanupContract: CleanupContract
}

interface EnvironmentTargetOfferBase<
  Kind extends EnvironmentTargetKind,
  Persistence extends TargetAuthorityPersistence,
  Profile extends GuaranteeProfile | null,
> {
  readonly routeId: string
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

export interface WorkspaceEnvironmentOffer {
  readonly routeId: string
  readonly kind: 'origin-private-workspace'
  readonly persistence: 'durable-owned-repository'
  readonly minimumQuotaReserveBytes: bigint
  readonly quotaAvailabilityEstimateBytes: bigint | null
}

export interface PortableEnvironmentOffer {
  readonly routeId: string
  readonly kind: 'portable-memory'
  readonly persistence: 'none'
  readonly maximumArtifactBytes: bigint
  readonly assemblyPartBytes: bigint
  readonly maximumParts: bigint
  readonly objectUrlLeaseMilliseconds: bigint
}

interface TargetSemanticsBase<
  Kind extends EnvironmentTargetKind,
  Persistence extends TargetAuthorityPersistence,
  Profile extends GuaranteeProfile,
> {
  readonly kind: Kind
  readonly guarantees: DestinationGuaranteeFacts
  readonly persistence: Persistence
  readonly hardMaximumOutputBytes: bigint | null
  readonly legalProfile: Profile
}

export type NativeDirectoryTargetSemantics = TargetSemanticsBase<
  'native-directory-container',
  'durable-authority',
  'native-tree'
>

export type FSADirectoryTargetSemantics = TargetSemanticsBase<
  'fsa-parent-directory',
  'durable-after-repository-commit',
  'fsa-tree'
>

export type ManagedAtomicTargetSemantics = TargetSemanticsBase<
  'managed-atomic-file-target',
  'operation-scoped',
  'managed-atomic'
>

export type BrowserHandoffTargetSemantics = TargetSemanticsBase<
  'browser-handoff',
  'none',
  'browser-handoff'
> & Readonly<{
  readonly objectUrlLeaseMilliseconds: bigint
  readonly supportsWorkspacePackage: boolean
  readonly supportsPortableArtifact: boolean
}>

export type MaterializationTargetSemantics =
  | NativeDirectoryTargetSemantics
  | FSADirectoryTargetSemantics
  | ManagedAtomicTargetSemantics
  | BrowserHandoffTargetSemantics

export interface WorkspacePlanSemantics {
  readonly kind: 'origin-private-workspace'
  readonly persistence: 'durable-owned-repository'
  readonly minimumQuotaReserveBytes: bigint
}

export interface PortablePlanSemantics {
  readonly kind: 'portable-memory'
  readonly persistence: 'none'
  readonly maximumArtifactBytes: bigint
  readonly assemblyPartBytes: bigint
  readonly maximumParts: bigint
  readonly objectUrlLeaseMilliseconds: bigint
}

export interface DirectTreePlanSemantics {
  readonly kind: 'direct-tree'
  readonly target: NativeDirectoryTargetSemantics | FSADirectoryTargetSemantics
}

export interface WorkspaceThenPublishPlanSemantics {
  readonly kind: 'workspace-then-publish'
  readonly workspace: WorkspacePlanSemantics
  readonly publicationTarget: ManagedAtomicTargetSemantics | BrowserHandoffTargetSemantics
}

export interface PortableHandoffPlanSemantics {
  readonly kind: 'portable-handoff'
  readonly portable: PortablePlanSemantics
  readonly handoffTarget: BrowserHandoffTargetSemantics
}

export type OfferedMaterializationPlanSemantics =
  | DirectTreePlanSemantics
  | WorkspaceThenPublishPlanSemantics
  | PortableHandoffPlanSemantics

export interface DirectTreeMaterializationRoute {
  readonly kind: 'direct-tree'
  readonly target: NativeDirectoryContainerOffer | FSADirectoryContainerOffer
}

export interface WorkspaceThenPublishMaterializationRoute {
  readonly kind: 'workspace-then-publish'
  readonly workspace: WorkspaceEnvironmentOffer
  readonly publicationTarget: ManagedAtomicTargetOffer | BrowserHandoffTargetOffer
}

export interface PortableHandoffMaterializationRoute {
  readonly kind: 'portable-handoff'
  readonly portable: PortableEnvironmentOffer
  readonly handoffTarget: BrowserHandoffTargetOffer
}

export type OfferedMaterializationRoute =
  | DirectTreeMaterializationRoute
  | WorkspaceThenPublishMaterializationRoute
  | PortableHandoffMaterializationRoute
