import type { ArtifactChoiceID } from '../../../transfer/intent'
import type { CanonicalBytes } from '../../workspace/canonical'
import type { DirectZipEntryPlanV2 } from '../format'
import type {
  PersistedReceiveRecord,
  ReceiveOperationHandleRecord,
  ReceiveOperationLeaseRecord,
  ReceiveOperationV2,
} from '../../workspace/records'
import type { ReceiveLifecycleState } from '../../workspace/state'

export const DIRECT_ZIP_JOURNAL_SCHEMA_VERSION = 1 as const
export const DIRECT_ZIP_PAGE_LAYOUT = 1 as const
export const DIRECT_ZIP_PAGE_CENTRAL = 2 as const
export const DIRECT_ZIP_PAGE_EPOCH = 3 as const
export const DIRECT_ZIP_CANDIDATE_BOOTSTRAP = 1 as const
export const DIRECT_ZIP_CANDIDATE_EPOCH = 2 as const
export const DIRECT_ZIP_CANDIDATE_CLOSING = 3 as const

export type DirectZipPageKind = 'layout' | 'central' | 'epoch'
export type DirectZipCandidateKind = 'bootstrap' | 'epoch' | 'closing'
export type DirectZipCheckpointPhase = 'between-members' | 'inside-member' | 'closing'

export interface DirectZipPolicyDigestsV1 {
  readonly encodingPolicyDigest: string
  readonly layoutPolicyDigest: string
  readonly checkpointPolicyDigest: string
  readonly journalBudgetDigest: string
  readonly epochPolicyDigest: string
}

export interface DirectZipJournalBudgetUsageV1 {
  readonly memberCount: bigint
  readonly canonicalMetadataBytes: bigint
}

export interface DirectZipPageChainV1 {
  readonly chainId: string
  readonly rootDigest: string
  readonly pageCount: bigint
  readonly recordCount: bigint
  readonly canonicalMetadataBytes: bigint
}

export interface DirectZipDiscoveryEvidenceV1 {
  readonly cursorCanonicalBytes: CanonicalBytes
  readonly directoryAdmissionDigest: string
  readonly discoveryRootDigest: string
}

export interface DirectZipTargetObservationV1 {
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly operationId: string
  readonly parentBindingDigest: string
  readonly fileBindingDigest: string
  readonly ownershipMarkerDigest: string
  readonly exactLength: bigint
  readonly lastModifiedMilliseconds: number
  readonly epochRootDigest: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface DirectZipRecoveryGateV1 {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly kind: import('../../workspace/state').RecoveryGateKind
  readonly checkpointDigest: string
  readonly candidateDigest?: string
  readonly additionalTemporaryBytesUpperBound?: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface DirectZipMemberResumeV1 {
  readonly fileId: string
  readonly fileRevision: string
  readonly exactSize: bigint
  readonly sourceRangeAuthorityDigest: string
  readonly entryPlan: DirectZipEntryPlanV2
  readonly entryPlanCanonicalBytes: CanonicalBytes
  readonly entryPlanDigest: string
  readonly memberPayloadOffset: bigint
  readonly crc32Accumulator: number
  readonly rollback: DirectZipMemberRollbackV1
}

export interface DirectZipMemberRollbackV1 {
  readonly archiveOffset: bigint
  readonly safeSelectedPayloadBytes: bigint
  readonly entryOrdinal: bigint
  readonly epochStart: bigint
  readonly predecessorEpochRootDigest: string
  readonly epochContentDigest: string
  readonly epochRootDigest: string
  readonly layoutPages: DirectZipPageChainV1
  readonly centralPages: DirectZipPageChainV1
  readonly epochPages: DirectZipPageChainV1
  readonly journalUsage: DirectZipJournalBudgetUsageV1
  readonly accountingTailPageId?: string
}

export interface DirectZipClosingReplayV1 {
  readonly archiveOffset: bigint
  readonly centralRecordRootDigest: string
  readonly completion?: DirectZipCommittedCompletionV1
}

export interface DirectZipCommittedCompletionV1 {
  readonly exactArchiveBytes: bigint
  readonly preClosingEpochRootDigest: string
}

export interface DirectZipCheckpointV1 {
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly targetBindingDigest: string
  readonly policies: DirectZipPolicyDigestsV1
  readonly generation: bigint
  readonly predecessorCheckpointDigest?: string
  readonly candidateLineageDigest?: string
  readonly phase: DirectZipCheckpointPhase
  readonly entryOrdinal: bigint
  readonly currentMember?: DirectZipMemberResumeV1
  readonly discovery: DirectZipDiscoveryEvidenceV1
  readonly archiveOffset: bigint
  readonly committedArchiveLength: bigint
  readonly committedSelectedPayloadBytes: bigint
  readonly parentBindingDigest: string
  readonly fileBindingDigest: string
  readonly targetObservation: DirectZipTargetObservationV1
  readonly epochRootDigest: string
  readonly layoutPages: DirectZipPageChainV1
  readonly centralPages: DirectZipPageChainV1
  readonly epochPages: DirectZipPageChainV1
  readonly journalUsage: DirectZipJournalBudgetUsageV1
  readonly accountingTailPageId?: string
  readonly closingReplay?: DirectZipClosingReplayV1
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

/** Candidate authority intentionally excludes fields that only exist after the writable closes. */
export type DirectZipCheckpointProposalV1 = Omit<
  DirectZipCheckpointV1,
  'candidateLineageDigest' | 'targetObservation' | 'canonicalBytes' | 'digest'
> & Readonly<{
  canonicalBytes: CanonicalBytes
  digest: string
}>

export type DirectZipPageAccountingPredecessorV1 =
  | Readonly<{
      kind: 'checkpoint'
      checkpointGeneration: bigint
      checkpointDigest: string
    }>
  | Readonly<{
      kind: 'page'
      pageKind: DirectZipPageKind
      pageId: string
      pageDigest: string
    }>

export interface DirectZipImmutablePageV1 {
  readonly id: string
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly operationId: string
  readonly pageKind: DirectZipPageKind
  readonly pageKindByte: number
  readonly chainId: string
  readonly pageOrdinal: number
  readonly predecessorRootDigest: string
  readonly canonicalEntries: readonly CanonicalBytes[]
  readonly entryCount: number
  readonly memberCountDelta: bigint
  readonly chainRecordCount: bigint
  readonly chainCanonicalMetadataBytes: bigint
  readonly accountingPredecessor: DirectZipPageAccountingPredecessorV1
  readonly budgetUsage: DirectZipJournalBudgetUsageV1
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
  readonly chainRootDigest: string
}

export interface DirectZipBootstrapCandidateV1 {
  readonly id: string
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly kind: 'bootstrap'
  readonly kindByte: typeof DIRECT_ZIP_CANDIDATE_BOOTSTRAP
  readonly operationId: string
  readonly candidateId: string
  readonly leaseId: string
  readonly leaseGeneration: bigint
  readonly selectionCanonicalBytes: CanonicalBytes
  readonly artifactCanonicalBytes: CanonicalBytes
  readonly choiceIdentityCanonicalBytes: CanonicalBytes
  readonly choiceId: ArtifactChoiceID
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly stablePhysicalName: string
  readonly ownershipNonce: string
  readonly targetBindingDigest: string
  readonly policies: DirectZipPolicyDigestsV1
  readonly parentHandleId: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface DirectZipCommitCandidateV1 {
  readonly id: string
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly kind: 'epoch' | 'closing'
  readonly kindByte: typeof DIRECT_ZIP_CANDIDATE_EPOCH | typeof DIRECT_ZIP_CANDIDATE_CLOSING
  readonly operationId: string
  readonly candidateId: string
  readonly leaseId: string
  readonly predecessorCheckpointGeneration: bigint
  readonly predecessorCheckpointDigest: string
  readonly expectedRangeDigest: string
  readonly predecessorTargetObservation: DirectZipTargetObservationV1
  readonly proposedCheckpoint: DirectZipCheckpointProposalV1
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export type DirectZipCandidateV1 = DirectZipBootstrapCandidateV1 | DirectZipCommitCandidateV1

export interface DirectZipStateRowV1 {
  readonly id: string
  readonly schemaVersion: typeof DIRECT_ZIP_JOURNAL_SCHEMA_VERSION
  readonly operationId: string
  readonly leaseId: string
  readonly checkpointGeneration: string
  readonly checkpointDigest: string
  readonly checkpoint: DirectZipCheckpointV1
  readonly recoveryGate?: DirectZipRecoveryGateV1
}

export interface DirectZipJournalFenceV1 {
  readonly operationId: string
  readonly leaseId: string
  readonly checkpointGeneration: bigint
}

export interface DirectZipBootstrapCommitV1 {
  readonly candidate: DirectZipBootstrapCandidateV1
  readonly operation: ReceiveOperationV2
  readonly operationRecord: PersistedReceiveRecord
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
  readonly handles: readonly ReceiveOperationHandleRecord[]
  readonly lease: ReceiveOperationLeaseRecord
  readonly checkpoint: DirectZipCheckpointV1
}

export interface DirectZipCandidatePromotionV1 {
  readonly fence: DirectZipJournalFenceV1
  readonly candidate: DirectZipCommitCandidateV1
  readonly checkpoint: DirectZipCheckpointV1
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
  readonly handles?: readonly ReceiveOperationHandleRecord[]
}

export interface DirectZipRecoveryLifecycleCommitV1 {
  readonly fence: DirectZipJournalFenceV1
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
  readonly recoveryGate?: DirectZipRecoveryGateV1
  readonly candidate?: DirectZipCommitCandidateV1
}

export interface DirectZipCandidateRetirementV1 {
  readonly fence: DirectZipJournalFenceV1
  readonly candidate: DirectZipCommitCandidateV1
  readonly disposition: 'replay-predecessor' | 'truncate-and-replay'
  readonly checkpoint: DirectZipCheckpointV1
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
}

export interface DirectZipPageScanV1 {
  readonly operationId: string
  readonly pageKind: DirectZipPageKind
  readonly chainId: string
  readonly afterPageOrdinal?: number
  readonly limit?: number
}

export interface DirectZipPageBatchV1 {
  readonly pages: readonly DirectZipImmutablePageV1[]
  readonly nextPageOrdinal?: number
}

export interface DirectZipBootstrapCandidateScanV1 {
  readonly afterCandidateKey?: string
  readonly limit?: number
}

export interface DirectZipBootstrapCandidateBatchV1 {
  readonly candidates: readonly DirectZipBootstrapCandidateV1[]
  readonly nextCandidateKey?: string
}

export type DirectZipRecoveryPermissionObservation = 'granted' | 'unavailable'
export type DirectZipRecoveryTargetObservation =
  | 'predecessor'
  | 'candidate'
  | 'unknown-tail'
  | 'absent'
  | 'foreign'
  | 'ambiguous'

export interface DirectZipRecoveryEvidenceV1 {
  readonly permission: DirectZipRecoveryPermissionObservation
  readonly target: DirectZipRecoveryTargetObservation
  readonly markerMatches: boolean
  readonly predecessorEpochsVerified: boolean
  readonly candidateRangeVerified: boolean
  readonly destinationSpaceAvailable: boolean
  readonly candidateAlreadyResolved: boolean
}

export type DirectZipRecoveryDecisionV1 =
  | Readonly<{ kind: 'retire-and-replay' }>
  | Readonly<{ kind: 'promote-candidate' }>
  | Readonly<{ kind: 'truncate-to-predecessor-and-replay' }>
  | Readonly<{ kind: 'authorization-required' }>
  | Readonly<{ kind: 'target-verification-required' }>
  | Readonly<{ kind: 'destination-space-required' }>
  | Readonly<{ kind: 'restart-required'; reason: 'target-deleted' }>
  | Readonly<{
      kind: 'needs-attention'
      reason: 'target-ownership-unknown' | 'publication-unknown'
    }>

export type DirectZipJournalTraceEvent = Readonly<{
  name:
    | 'direct_zip.journal.bootstrap_stored'
    | 'direct_zip.journal.bootstrap_lease_replaced'
    | 'direct_zip.journal.page_staged'
    | 'direct_zip.journal.candidate_bound'
    | 'direct_zip.journal.bootstrap_committed'
    | 'direct_zip.journal.lease_acquired'
    | 'direct_zip.journal.candidate_promoted'
    | 'direct_zip.journal.candidate_retired'
    | 'direct_zip.journal.recovery_lifecycle_committed'
    | 'direct_zip.journal.orphans_collected'
    | 'direct_zip.journal.transaction_failed'
  operation_id: string
  lease_id?: string
  checkpoint_generation?: bigint
  candidate_id?: string
  page_kind?: DirectZipPageKind
  page_ordinal?: number
  decision?: string
}>

export type DirectZipJournalTrace = (event: DirectZipJournalTraceEvent) => void
