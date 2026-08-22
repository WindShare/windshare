import type { V2CatalogModifiedTime } from '../../catalog/v2-records'
import type { DirectZipCompletionProofV1, DirectZipWriterCheckpointV1 } from '../../output/direct-zip/writer'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { DirectResumableZipPlan, ReceiveIntent, ZipArchiveArtifact } from '../intent'
import type { PendingFile } from '../job/contract'
import type {
  OutputCapabilities,
  OutputSessionIdentity,
  MaterializationSummary,
  PlanPauseRequest,
  PlanSettlementRequest,
} from '../output-session'
import type { SuccessfulTransferWorkerSettlement } from '../outcome'

export type DirectZipIntent = ReceiveIntent & Readonly<{
  plan: DirectResumableZipPlan
  artifact: ZipArchiveArtifact
}>

export interface DirectZipAuthenticatedRootV1 {
  readonly directoryId: string
  readonly generation: string
  readonly discoveryEvidence: Uint8Array
}

interface DirectZipOrderedMemberBaseV1 {
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly layoutEvidence: Uint8Array
  readonly discoveryEvidence: Uint8Array
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface DirectZipOrderedDirectoryV1 extends DirectZipOrderedMemberBaseV1 {
  readonly kind: 'directory'
  readonly directoryId: string
  readonly generation: string
}

export interface DirectZipOrderedFileV1 extends DirectZipOrderedMemberBaseV1 {
  readonly kind: 'file'
  readonly fileId: string
  readonly expectedSize: bigint
  readonly pending: PendingFile
}

export type DirectZipOrderedMemberV1 = DirectZipOrderedDirectoryV1 | DirectZipOrderedFileV1

export type DirectZipOrderedVisitV1 = 'replayed' | 'admitted' | 'transfer-file'

/** The execution adapter is the sole bridge from authenticated order into writer admission. */
export interface DirectZipOrderedOutputV1 {
  beginTraversal(root: DirectZipAuthenticatedRootV1, signal: AbortSignal): Promise<void>
  visit(
    ordinal: bigint,
    member: DirectZipOrderedMemberV1,
    signal: AbortSignal,
  ): Promise<DirectZipOrderedVisitV1>
  finishTraversal(nextOrdinal: bigint, signal: AbortSignal): Promise<void>
  materializationSummary(): MaterializationSummary
}

export interface DirectZipOpenedSourceV1 {
  readonly fileId: string
  readonly revision: string
  readonly exactSize: bigint
  readonly rangeAuthority: string
}

export interface DirectZipFileTransactionV1 {
  readonly resumeOffset: bigint
  write(offset: bigint, bytes: Uint8Array, signal: AbortSignal): Promise<void>
  /** A content block is an observation; the returned safe offset may remain unchanged. */
  observeCheckpoint(signal: AbortSignal): Promise<bigint>
  commit(signal: AbortSignal): Promise<void>
}

export interface DirectZipOutputSessionV1 {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  beginFile(
    file: DirectZipOrderedFileV1,
    source: DirectZipOpenedSourceV1,
    signal: AbortSignal,
  ): Promise<DirectZipFileTransactionV1>
}

export interface DirectZipOrderedSourceV1 {
  root(signal: AbortSignal): Promise<DirectZipAuthenticatedRootV1>
  members(signal: AbortSignal): AsyncIterable<DirectZipOrderedMemberV1>
}

export interface DirectZipStableEvidenceV1 {
  readonly checkpoint: DirectZipWriterCheckpointV1
  readonly materialization: MaterializationSummary
  readonly additionalTemporaryBytesUpperBound: bigint
}

export interface DirectZipPublishedEvidenceV1 extends DirectZipStableEvidenceV1 {
  readonly completion: DirectZipCompletionProofV1
}

export interface DirectZipSettlementAuthorityV1 {
  pause(
    intent: DirectZipIntent,
    request: PlanPauseRequest,
    evidence: DirectZipStableEvidenceV1,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  settle(
    intent: DirectZipIntent,
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    evidence: DirectZipPublishedEvidenceV1,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}
