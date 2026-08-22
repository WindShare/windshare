import { V2_CATALOG_NAME_BYTES } from '../../catalog/path-policy'
import { V2_MAXIMUM_SELECTION_RULE_OVERRIDES } from '../../catalog/v2-selection'

export type CanonicalBytes = Uint8Array<ArrayBuffer>

export const RECEIVE_INTENT_VERSION = 3 as const
export const SELECTION_SPEC_VERSION = 1 as const
export const ARTIFACT_SPEC_VERSION = 1 as const
export const MATERIALIZATION_PLAN_VERSION = 3 as const
export const DESTINATION_RESERVATION_VERSION = 3 as const
export const FSA_OWNED_FILE_BINDING_VERSION = 1 as const
export const ARTIFACT_CHOICE_IDENTITY_VERSION = 1 as const
export const WORKSPACE_BINDING_VERSION = 1 as const
export const PORTABLE_BINDING_VERSION = 1 as const

export const STABLE_IDENTITY_BYTES = 16
export const AUTHORITY_REFERENCE_BYTES = 32
export const RECEIVE_INTENT_DIGEST_BYTES = 32
export const ARTIFACT_CHOICE_ID_BYTES = 32
export const MAX_SELECTION_RULES = V2_MAXIMUM_SELECTION_RULE_OVERRIDES
export const MAX_SELECTION_TARGET_UTF8_BYTES = 1 << 20
export const DEFAULT_RESULT_ROOT_NAME = 'windshare'
export const DEFAULT_ARCHIVE_NAME = 'windshare.zip'
export const PARTIAL_SELECTION_SUFFIX = '-selection'
export const ARCHIVE_EXTENSION = '.zip'
export const DIRECT_ZIP_STABLE_NAME_INFIX = '.windshare-'
export const DIRECT_ZIP_CANDIDATE_TOKEN_LENGTH = 22
export const RESULT_NAME_POLICY = 'windshare/result-name/v1-unicode-15.0.0'
export const MAX_RESULT_COMPONENT_BYTES = V2_CATALOG_NAME_BYTES
export const COLLISION_SUFFIX_HEX_CHARS = 10
export const DEFAULT_PORTABLE_ARTIFACT_LIMIT = 67_108_864n
export const DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES = 1_048_576n
export const DEFAULT_PORTABLE_MAXIMUM_PARTS = 64n
export const BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS = 60_000n

export interface CanonicalValue {
  readonly canonicalBytes: CanonicalBytes
}

export interface CanonicalDigestValue extends CanonicalValue {
  readonly digest: string
}

export type SelectionMode = 'node-id' | 'catalog-path'
export type SelectionRuleKind = 'directory' | 'file'

export interface NodeSelectionRule {
  readonly kind: SelectionRuleKind
  readonly id: string
  readonly selected: boolean
}

export interface NodeIDSelectionRules {
  readonly mode: 'node-id'
  readonly defaultSelected: boolean
  readonly rules: readonly NodeSelectionRule[]
}

export interface CatalogPathSelectionRules {
  readonly mode: 'catalog-path'
  readonly defaultSelected: false
  readonly paths: readonly string[]
}

export type SelectionRulesSpec = NodeIDSelectionRules | CatalogPathSelectionRules

export interface SelectionSpec extends CanonicalDigestValue {
  readonly version: typeof SELECTION_SPEC_VERSION
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly rules: SelectionRulesSpec
}

export type ResultRootClass =
  | 'complete-directory'
  | 'directory-selection'
  | 'synthetic-selection'

export interface DirectoryResultRootLayout extends CanonicalValue {
  readonly class: 'complete-directory' | 'directory-selection'
  readonly anchor: Readonly<{
    kind: 'directory'
    directoryId: string
    sourcePath: string
  }>
  readonly name: string
}

export interface SyntheticResultRootLayout extends CanonicalValue {
  readonly class: 'synthetic-selection'
  readonly anchor: Readonly<{ kind: 'synthetic-root' }>
  readonly name: typeof DEFAULT_RESULT_ROOT_NAME
}

export type ResultRootLayout = DirectoryResultRootLayout | SyntheticResultRootLayout

export interface SingleFileDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'single-file'
  readonly fileId: string
  readonly sourcePath: string
  readonly outputName: string
}

export interface ResultRootDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'result-root'
  readonly root: ResultRootLayout
}

export interface CatalogRootDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'catalog-root'
}

export type DirectoryTreeLayout =
  | SingleFileDirectoryTreeLayout
  | ResultRootDirectoryTreeLayout
  | CatalogRootDirectoryTreeLayout

interface ArtifactSpecBase extends CanonicalDigestValue {
  readonly version: typeof ARTIFACT_SPEC_VERSION
}

export interface OriginalFileArtifact extends ArtifactSpecBase {
  readonly kind: 'original-file'
  readonly fileId: string
  readonly sourcePath: string
  readonly suggestedName: string
}

export interface DirectoryTreeArtifact extends ArtifactSpecBase {
  readonly kind: 'directory-tree'
  readonly layout: DirectoryTreeLayout
}

export interface ZipArchiveArtifact extends ArtifactSpecBase {
  readonly kind: 'zip-archive'
  readonly layout: ResultRootLayout
  readonly suggestedName: string
  readonly encoding: 'store'
  readonly completeness: 'complete-only'
}

export type ArtifactSpec =
  | OriginalFileArtifact
  | DirectoryTreeArtifact
  | ZipArchiveArtifact

export type ArtifactKind = ArtifactSpec['kind']
export type MaterializationKind = MaterializationPlan['kind']
export type PreparationPolicy = 'none' | 'exact-zip' | 'exact-artifact'

export type NameAuthority = 'application-chosen' | 'user-chosen' | 'browser-chosen'
export type ReplacementGuarantee =
  | 'atomic-no-replace'
  | 'coordinated-no-replace'
  | 'user-authorized-replace'
  | 'unknown'
export type DeliveryMode = 'managed-target' | 'browser-handoff'
export type TargetVisibility =
  | 'hidden-until-verified-publication'
  | 'committed-objects-visible'
  | 'unobservable'
  | 'operation-owned-incomplete-file-visible'
export type ArtifactAvailability =
  | 'verified-complete-only'
  | 'committed-objects-usable'
  | 'handoff-only'
export type CleanupAuthority =
  | 'rollback-to-absent-before-publication'
  | 'no-whole-target-rollback'
  | 'ownership-proof-required'
  | 'no-managed-cleanup'
export type GuaranteeProfile =
  | 'native-tree'
  | 'fsa-tree'
  | 'managed-atomic'
  | 'browser-handoff'
  | 'fsa-owned-file'

export interface GuaranteeSet {
  readonly profile: GuaranteeProfile
  readonly nameAuthority: NameAuthority
  readonly replacement: ReplacementGuarantee
  readonly delivery: DeliveryMode
  readonly targetVisibility: TargetVisibility
  readonly artifactAvailability: ArtifactAvailability
  readonly cleanupAuthority: CleanupAuthority
}

interface DestinationReservationBase extends CanonicalDigestValue {
  readonly version: typeof DESTINATION_RESERVATION_VERSION
  readonly operationId: string
  readonly reservationId: string
  readonly artifactDigest: string
  readonly authorityKind: 'native-container' | 'fsa-container' | 'managed-atomic-target'
  readonly authorityRef: string
  readonly guarantees: GuaranteeSet
}

export interface ContainerRootReservation extends DestinationReservationBase {
  readonly kind: 'container-root'
  readonly authorityKind: 'native-container'
}

export interface NamedContainerEntryReservation extends DestinationReservationBase {
  readonly kind: 'named-container-entry'
  readonly authorityKind: 'native-container' | 'fsa-container'
  readonly entryKind: 'single-file' | 'result-root'
  readonly requestedName: string
  readonly logicalReservedName: string
  readonly physicalName: string
  readonly collisionIndex: number
}

export interface AtomicTargetReservation extends DestinationReservationBase {
  readonly kind: 'atomic-target'
  readonly authorityKind: 'managed-atomic-target'
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
}

export type DestinationReservation =
  | ContainerRootReservation
  | NamedContainerEntryReservation
  | AtomicTargetReservation

export interface WorkspaceBinding extends CanonicalDigestValue {
  readonly version: typeof WORKSPACE_BINDING_VERSION
  readonly operationId: string
  readonly workspaceId: string
  readonly artifactDigest: string
  readonly repositoryRef: string
  readonly workspaceKind: 'origin-private'
  readonly budgetPolicy: 'workspace-v1'
  readonly retentionPolicy: 'stable-24h-v1'
}

export interface PortableBinding extends CanonicalDigestValue {
  readonly version: typeof PORTABLE_BINDING_VERSION
  readonly operationId: string
  readonly portablePlanId: string
  readonly artifactDigest: string
  readonly maximumArtifactBytes: typeof DEFAULT_PORTABLE_ARTIFACT_LIMIT
  readonly assemblyPartBytes: typeof DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES
  readonly maximumParts: typeof DEFAULT_PORTABLE_MAXIMUM_PARTS
  readonly objectUrlLeaseMilliseconds: typeof BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS
  readonly preparation: 'exact-artifact'
}

export interface DirectZipPolicyDigests {
  readonly zipEncoding: string | null
  readonly layout: string | null
  readonly checkpoint: string | null
  readonly journalBudget: string | null
  readonly epoch: string | null
}

export interface AvailableDirectZipPolicyDigests extends DirectZipPolicyDigests {
  readonly zipEncoding: string
  readonly layout: string
  readonly checkpoint: string
  readonly journalBudget: string
  readonly epoch: string
}

export interface FSAOwnedFileBinding extends CanonicalDigestValue {
  readonly version: typeof FSA_OWNED_FILE_BINDING_VERSION
  readonly operationId: string
  readonly artifactDigest: string
  readonly stableName: string
  readonly targetRef: string
  readonly guarantees: GuaranteeSet & Readonly<{ profile: 'fsa-owned-file' }>
  readonly policies: AvailableDirectZipPolicyDigests
}

interface MaterializationPlanBase extends CanonicalValue {
  readonly version: typeof MATERIALIZATION_PLAN_VERSION
}

export interface DirectTreePlan extends MaterializationPlanBase {
  readonly kind: 'direct-tree'
  readonly reservation: DestinationReservation
  readonly preparation: 'none'
}

export interface DirectAtomicPlan extends MaterializationPlanBase {
  readonly kind: 'direct-atomic'
  readonly reservation: AtomicTargetReservation
  readonly preparation: 'none'
}

export interface WorkspaceThenPublishPlan extends MaterializationPlanBase {
  readonly kind: 'workspace-then-publish'
  readonly workspace: WorkspaceBinding
  readonly publicationGuarantee: 'managed-atomic' | 'browser-handoff'
  readonly preparation: 'none' | 'exact-zip'
}

export interface PortableHandoffPlan extends MaterializationPlanBase {
  readonly kind: 'portable-handoff'
  readonly portable: PortableBinding
  readonly publicationGuarantee: 'browser-handoff'
  readonly preparation: 'exact-artifact'
}

export interface DirectResumableZipPlan extends MaterializationPlanBase {
  readonly kind: 'direct-resumable-zip'
  readonly binding: FSAOwnedFileBinding
  readonly preparation: 'none'
}

export type MaterializationPlan =
  | DirectTreePlan
  | DirectAtomicPlan
  | WorkspaceThenPublishPlan
  | PortableHandoffPlan
  | DirectResumableZipPlan

declare const ARTIFACT_CHOICE_ID_BRAND: unique symbol
export type ArtifactChoiceID = string & Readonly<{ [ARTIFACT_CHOICE_ID_BRAND]: true }>

export interface ArtifactChoiceIdentity extends CanonicalValue {
  readonly version: typeof ARTIFACT_CHOICE_IDENTITY_VERSION
  readonly artifactKind: ArtifactKind
  readonly materializationKind: MaterializationKind
  readonly guaranteeProfile: GuaranteeProfile
  readonly preparation: PreparationPolicy
  readonly id: ArtifactChoiceID
}

export interface ReceiveIntent extends CanonicalDigestValue {
  readonly version: typeof RECEIVE_INTENT_VERSION
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly operationId: string
  readonly bindingDigest: string
}
