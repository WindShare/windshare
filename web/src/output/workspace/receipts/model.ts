import type { CanonicalBytes } from '../canonical'
import type { ReceiveLifecycleState } from '../state'

export const RECEIPT_SCHEMA_VERSION = 1 as const
export const RECEIPT_PREPARATION_ADMISSION = 5 as const
export const RECEIPT_WORKSPACE_SEAL = 6 as const
export const RECEIPT_PACKAGE = 7 as const
export const RECEIPT_MANAGED_PUBLICATION = 8 as const
export const RECEIPT_HANDOFF = 9 as const
export const RECEIPT_EXPIRY = 10 as const
export const RECEIPT_CLEANUP = 11 as const
export const RECEIPT_PACKAGE_TEMPORARY_CLEANUP = 12 as const
export const RAW_WORKSPACE_RECEIPT_DOMAIN = 'windshare/raw-workspace-receipt/v1'

export interface ReceiveReceiptBase {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface PreparationAdmissionReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'preparation-admission'
  readonly preparationManifestDigest?: string
  readonly sealedZipLayoutDigest?: string
  readonly workspaceBudgetDigest: string
  readonly contentRequestCountAtAdmission: 0n
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}

export interface OwnedWorkspaceObjectReceipt {
  readonly ownedObjectId: string
  readonly exactBytes: bigint
}

export interface RawWorkspaceReceiptV1 {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly materializedManifestDigest: string
  readonly ownedObjects: readonly OwnedWorkspaceObjectReceipt[]
  readonly uniqueRawBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface WorkspaceSealReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'workspace-seal'
  readonly workspaceBindingDigest: string
  readonly sealedMaterializationDigest: string
  readonly rawWorkspaceReceipt: RawWorkspaceReceiptV1
}

interface ArtifactVerificationReceiptBase {
  readonly schemaVersion: typeof RECEIPT_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly layoutDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface ZipArtifactVerificationReceiptV1 extends ArtifactVerificationReceiptBase {
  readonly kind: 'zip-writer'
  readonly writerCloseVerified: true
}

export interface OriginalFileArtifactVerificationReceiptV1 extends ArtifactVerificationReceiptBase {
  readonly kind: 'original-file-promotion'
  readonly finalCheckpointDigest: string
  readonly finalCheckpointGeneration: bigint
  readonly promotionVerified: true
}

export type ArtifactVerificationReceiptV1 =
  | ZipArtifactVerificationReceiptV1
  | OriginalFileArtifactVerificationReceiptV1

export interface PackageReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'package'
  readonly packagedArtifactDigest: string
  readonly artifactVerification: ArtifactVerificationReceiptV1
}

export interface ManagedPublicationReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'managed-publication'
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly reservationDigest: string
  readonly targetAuthorityRef: string
  readonly exactBytes: bigint
  readonly commitConfirmed: true
}

export interface HandoffReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'handoff'
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly suggestedName: string
  readonly urlLeaseEndsAt: number
  readonly handoffStarted: true
}

export interface ExpiryReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'expiry'
  readonly priorStableState:
    | 'resumable-receive'
    | 'resumable-package'
    | 'waiting-to-save'
    | 'download-started'
  readonly expiresAt: number
  readonly retainedSuccessCount: bigint
  readonly cleanupState: 'clean' | 'cleanup-pending'
}

export interface CleanupReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'cleanup'
  readonly removedObjectIds: readonly string[]
  readonly removedRecordDigests: readonly string[]
  readonly cleanupGeneration: bigint
}

export interface PackageTemporaryCleanupReceiptV1 extends ReceiveReceiptBase {
  readonly kind: 'package-temporary-cleanup'
  readonly sealedMaterializationDigest: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly cleanupResult: 'removed' | 'already-absent'
  readonly cleanupProofDigest: string
}

export type WorkspaceReceiveReceiptV1 =
  | PreparationAdmissionReceiptV1
  | WorkspaceSealReceiptV1
  | PackageReceiptV1
  | ManagedPublicationReceiptV1
  | HandoffReceiptV1
  | ExpiryReceiptV1
  | CleanupReceiptV1
  | PackageTemporaryCleanupReceiptV1

export type WorkspaceStableState = Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive' | 'resumable-package' | 'waiting-to-save' | 'download-started' }
>
