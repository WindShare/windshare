import type {
  ReceiveOperationHandleRecord,
  ReceiveOperationLeaseRecord,
} from '../../workspace/records'
import type { ReceiveOperationTransition } from '../../workspace/repository'
import type {
  DirectZipBootstrapCandidateBatchV1,
  DirectZipBootstrapCandidateScanV1,
  DirectZipBootstrapCandidateV1,
  DirectZipBootstrapCommitV1,
  DirectZipCandidatePromotionV1,
  DirectZipCandidateRetirementV1,
  DirectZipCandidateV1,
  DirectZipCommitCandidateV1,
  DirectZipImmutablePageV1,
  DirectZipJournalFenceV1,
  DirectZipPageBatchV1,
  DirectZipPageScanV1,
  DirectZipRecoveryLifecycleCommitV1,
  DirectZipStateRowV1,
} from './model'

export interface DirectZipBootstrapCandidateCutV1 {
  readonly candidate: DirectZipBootstrapCandidateV1
  readonly provisionalParentHandle: ReceiveOperationHandleRecord
  readonly lease: ReceiveOperationLeaseRecord
}

export interface DirectZipBootstrapLeaseReplacementV1 {
  readonly expectedCandidate: DirectZipBootstrapCandidateV1
  readonly candidate: DirectZipBootstrapCandidateV1
  readonly lease: ReceiveOperationLeaseRecord
}

export interface DirectZipOrphanCollectionV1 {
  readonly scannedPageCount: bigint
  readonly deletedPageCount: bigint
}

/**
 * The archive engine sees cuts, not IndexedDB. This keeps filesystem observation
 * independent from transaction failure and makes every authority transition injectable.
 */
export interface DirectZipJournalRepository {
  readState(operationId: string): Promise<DirectZipStateRowV1 | undefined>
  readCandidate(operationId: string, candidateId: string): Promise<DirectZipCandidateV1 | undefined>
  readOperationCandidate(operationId: string): Promise<DirectZipCandidateV1 | undefined>
  /** Replaces the live lease row and journal fence as one durable cut. */
  commitLeaseAcquisition(transition: ReceiveOperationTransition): Promise<void>
  createBootstrapCandidate(cut: DirectZipBootstrapCandidateCutV1): Promise<void>
  replaceBootstrapLease(cut: DirectZipBootstrapLeaseReplacementV1): Promise<void>
  stagePage(fence: DirectZipJournalFenceV1, page: DirectZipImmutablePageV1): Promise<void>
  bindCandidate(
    fence: DirectZipJournalFenceV1,
    candidate: DirectZipCommitCandidateV1,
  ): Promise<void>
  commitBootstrap(cut: DirectZipBootstrapCommitV1): Promise<void>
  promoteCandidate(cut: DirectZipCandidatePromotionV1): Promise<void>
  commitRecoveryLifecycle(cut: DirectZipRecoveryLifecycleCommitV1): Promise<void>
  retireCandidate(cut: DirectZipCandidateRetirementV1): Promise<void>
  readPageBatch(scan: DirectZipPageScanV1): Promise<DirectZipPageBatchV1>
  streamPages(scan: Omit<DirectZipPageScanV1, 'afterPageOrdinal'>): AsyncIterable<DirectZipImmutablePageV1>
  readBootstrapCandidateBatch(
    scan?: DirectZipBootstrapCandidateScanV1,
  ): Promise<DirectZipBootstrapCandidateBatchV1>
  streamBootstrapCandidates(): AsyncIterable<DirectZipBootstrapCandidateV1>
  collectOrphanPages(fence: DirectZipJournalFenceV1): Promise<DirectZipOrphanCollectionV1>
  close(): void
}

export interface DirectZipBootstrapResumeDescriptorV1 {
  readonly kind: 'direct-zip-bootstrap'
  readonly operationId: string
  readonly candidateId: string
  readonly leaseId: string
  readonly leaseGeneration: bigint
  readonly choiceId: string
  readonly stablePhysicalName: string
  readonly candidateDigest: string
}

export function directZipBootstrapResumeDescriptorV1(
  candidate: DirectZipBootstrapCandidateV1,
): DirectZipBootstrapResumeDescriptorV1 {
  return Object.freeze({
    kind: 'direct-zip-bootstrap',
    operationId: candidate.operationId,
    candidateId: candidate.candidateId,
    leaseId: candidate.leaseId,
    leaseGeneration: candidate.leaseGeneration,
    choiceId: candidate.choiceId,
    stablePhysicalName: candidate.stablePhysicalName,
    candidateDigest: candidate.digest,
  })
}
