import type {
  OutputDiagnosticsPorts,
  PerformanceFilePipelineObservation,
} from '../diagnostics'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
  PersistentHandleRecord,
  SemanticFileCheckpointJournal,
} from '../persistence/journal'
import type { MaterializationLedgerJournal } from '../materialization-ledger/journal'
import type {
  MaterializationDirectoryAdmittedEntryV1,
  MaterializationDirectoryFinalization,
  MaterializationDirectoryFinalizedEntryV1,
  StableDirectoryCoordinates,
  StableParentDirectoryCoordinates,
} from '../materialization-ledger/model'
import type { FileCheckpointRecoveryRepository } from './recovery'
import type { AuthenticatedLogicalSiblingMembership } from '../../transfer/job/contract'
import type {
  AutomaticCheckpointTrigger,
  OutputSessionIdentity,
  VerifiedFinalOutputFile,
} from '../../transfer/output-session'
import type {
  PersistentOutputStageAuthority,
  PersistentOutputStageScope,
} from './stage-diagnostics'

export interface OpenedFileRevision {
  readonly fileId: string
  readonly fileRevision: string
  readonly exactSize: bigint
}

export interface PersistentFileRequest {
  readonly materializationRelativePath: readonly string[]
  readonly shareInstance?: string
  readonly outputSession?: OutputSessionIdentity
  readonly recovery?: PersistentFileRecoveryPolicy
  readonly automaticCheckpointAdmission?: AutomaticCheckpointFileAdmission
  readonly preservingWriterCapacity?: PreservingWriterCapacityAuthority
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly openRevision: () => Promise<OpenedFileRevision>
}

export type PersistentPausedFileRecovery = 'preserve' | 'restart-owned-file'

export type PersistentFileRecoveryPolicy = Readonly<{
  readonly pausedFile: PersistentPausedFileRecovery
}>

export type PersistentWriterOpenMode = 'preserve' | 'truncate'

/** Incremental cost of one preserving open; attempt authorities alone own cumulative totals. */
export interface PreservingWriterCost {
  readonly prefixCopyBytes: bigint
  readonly writeAmplificationBytes: bigint
  readonly temporaryBytes: bigint
}

export interface CheckpointAttemptIdentity {
  readonly receiveOperationId: string
  readonly transferJobId: string
  readonly outputSessionId: string
}

export type CheckpointResourceReleaseReason =
  | 'unused'
  | 'capacity-unavailable'
  | 'replacement-open-failed'
  | 'writer-closed'
  | 'writer-aborted'
  | 'file-committed'
  | 'file-paused'
  | 'file-retired'
  | 'cancelled'
  | 'terminal-drain'
  | 'automatic-handoff'

export type CheckpointAuthorityObservation = Readonly<{
  readonly authority: 'automatic-admission' | 'preserving-capacity'
  readonly receiveOperationId: string
  readonly transferJobId: string
  readonly outputSessionId: string
  readonly materializationRelativePath: readonly string[]
  readonly trigger: AutomaticCheckpointTrigger | 'paused-file-recovery'
  readonly checkpointOrdinal?: number
  readonly cost: PreservingWriterCost
  readonly remainingAutomaticWriteAmplificationBytes?: bigint
  readonly decision:
    | 'admitted'
    | 'checkpoint-priority'
    | 'prefix-copy-budget'
    | 'cumulative-write-amplification-budget'
    | 'capacity-unavailable'
    | 'paused-recovery-queued'
    | 'paused-recovery-admitted'
    | 'committed'
    | 'released'
  readonly releaseReason?: CheckpointResourceReleaseReason
}>

export type CheckpointAuthorityObserver = (observation: CheckpointAuthorityObservation) => void

export interface AutomaticCheckpointBudgetHold {
  readonly checkpointOrdinal: number
  readonly cost: PreservingWriterCost
  commit(): void
  release(reason: CheckpointResourceReleaseReason): void
}

export type AutomaticCheckpointAdmissionDecision =
  | Readonly<{
      readonly kind: 'admitted'
      readonly hold: AutomaticCheckpointBudgetHold
      readonly remainingWriteAmplificationBytes: bigint
    }>
  | Readonly<{
      readonly kind: 'deferred'
      readonly reason: 'checkpoint-priority'
      readonly estimate: PreservingWriterCost
      readonly remainingWriteAmplificationBytes: bigint
    }>
  | Readonly<{
      readonly kind: 'finished'
      readonly reason: 'prefix-copy-budget' | 'cumulative-write-amplification-budget'
      readonly estimate: PreservingWriterCost
      readonly remainingWriteAmplificationBytes: bigint
    }>

export interface AutomaticCheckpointFileAdmission {
  readonly materializationRelativePath: readonly string[]
  request(
    trigger: AutomaticCheckpointTrigger,
    cost: PreservingWriterCost,
  ): AutomaticCheckpointAdmissionDecision
  retire(reason?: CheckpointResourceReleaseReason): void
}

export interface AutomaticCheckpointAdmissionSnapshot {
  readonly accepting: boolean
  readonly enrolledFiles: number
  readonly committedWriteAmplificationBytes: bigint
  readonly remainingWriteAmplificationBytes: bigint
  readonly tentativeHolds: number
  readonly cumulativelyExhausted: boolean
}

export interface AutomaticCheckpointAdmissionAuthority {
  enrollFile(materializationRelativePath: readonly string[]): AutomaticCheckpointFileAdmission
  close(reason?: CheckpointResourceReleaseReason): void
  snapshot(): AutomaticCheckpointAdmissionSnapshot
}

export type PreservingWriterCapacityPurpose = 'automatic-checkpoint' | 'paused-file-recovery'

export interface PreservingWriterCapacityToken {
  readonly purpose: PreservingWriterCapacityPurpose
  readonly reservedTemporaryBytes: bigint
  commit(): void
  release(reason: CheckpointResourceReleaseReason): void
}

export interface PreservingWriterCapacityRequest {
  readonly materializationRelativePath: readonly string[]
  readonly trigger: AutomaticCheckpointTrigger | 'paused-file-recovery'
  readonly checkpointOrdinal?: number
  readonly cost: PreservingWriterCost
  readonly remainingAutomaticWriteAmplificationBytes?: bigint
}

export type AutomaticCapacityHandoffResult =
  | Readonly<{ readonly kind: 'reserved'; readonly token: PreservingWriterCapacityToken }>
  | Readonly<{ readonly kind: 'unavailable'; readonly reason: 'capacity-unavailable' }>

export interface PreservingWriterCapacitySnapshot {
  readonly accepting: boolean
  readonly heldTemporaryBytes: bigint
  readonly heldTokens: number
  readonly queuedPausedRecoveries: number
  readonly oversizedExclusive: boolean
}

export interface PreservingWriterCapacityAuthority {
  tryHandoff(
    request: PreservingWriterCapacityRequest,
    current?: PreservingWriterCapacityToken,
  ): AutomaticCapacityHandoffResult
  reservePaused(
    request: PreservingWriterCapacityRequest,
    signal?: AbortSignal,
  ): Promise<PreservingWriterCapacityToken>
  close(reason?: CheckpointResourceReleaseReason): void
  snapshot(): PreservingWriterCapacitySnapshot
}

export interface PersistentTreeFile {
  readonly ownedObjectId: string
  readonly persistedHandle?: PersistentHandleRecord<unknown>
  openWriter?(mode: PersistentWriterOpenMode): Promise<void>
  preservingWriterCost?(durablePrefixBytes: bigint): PreservingWriterCost
  writeAt(offset: bigint, data: Uint8Array): Promise<void>
  flush(): Promise<void>
  size(): Promise<bigint>
  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void>
  abort?(reason?: unknown): Promise<void>
  close(): Promise<void>
  read(): Promise<Blob>
}

export interface PersistentDirectoryMaterialization {
  readonly ownedObjectId: string
  readonly created: boolean
}

/** Paths locate output objects relative to the materialization root selected by the immutable plan. */
export interface PersistentOutputTree {
  authorize(): Promise<void>
  prepareRoot(): Promise<void>
  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization>
  validateDirectory(path: readonly string[], ownedObjectId: string): Promise<boolean>
  proposeFileOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string>
  inspectFileDestination(
    path: readonly string[],
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<'absent' | 'occupied'>
  createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
    stageScope?: PersistentOutputStageScope,
    commitCreatedFile?: (handle: PersistentHandleRecord<unknown>) => Promise<void>,
  ): Promise<PersistentTreeFile>
  openFile(
    path: readonly string[],
    ownedObjectId: string,
    stageScope?: PersistentOutputStageScope,
  ): Promise<PersistentTreeFile | undefined>
  removeFile(path: readonly string[], ownedObjectId: string): Promise<void>
  removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void>
}

export interface PersistentMaterializationPort {
  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort>
  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization>
  materializeDirectory?(request: PersistentDirectoryLedgerRequest): Promise<
    PersistentDirectoryLedgerMaterialization
  >
  finalizeDirectory?(
    admission: MaterializationDirectoryAdmittedEntryV1,
    outcome: MaterializationDirectoryFinalization,
  ): Promise<MaterializationDirectoryFinalizedEntryV1>
  closeForTerminalSettlement?(): Promise<void>
  close(): Promise<void>
}

export interface PersistentDirectoryLedgerRequest {
  readonly relativePath: readonly string[]
  readonly directoryId: string
  readonly generation: string
  readonly parent?: StableParentDirectoryCoordinates
  readonly modifiedTime?: StableDirectoryCoordinates['modifiedTime']
}

export interface PersistentDirectoryLedgerMaterialization extends PersistentDirectoryMaterialization {
  readonly ledgerAdmission: MaterializationDirectoryAdmittedEntryV1
}

export interface PersistentDirectoryNamespaceClaim {
  readonly materializationRelativePath: readonly string[]
  readonly logicalSiblingMembership: AuthenticatedLogicalSiblingMembership
}

/**
 * Optional DirectTree join for a backend that can activate compatible physical names.
 * Binding is synchronous and must not inspect membership; candidate allocation owns that lazy query.
 */
export interface PersistentOutputNamespaceClaimPort {
  bindDirectoryNamespace(claim: PersistentDirectoryNamespaceClaim): void
}

/** Root publication is an explicit post-binding step for PrefixVisible destinations. */
export interface ActivatablePersistentMaterializationPort extends PersistentMaterializationPort {
  activate(): Promise<void>
}

export interface PersistentFileTransactionPort {
  readonly revision: OpenedFileRevision
  readonly ownedObjectId: string
  readonly initialDurableRanges: readonly PersistentByteRange[]
  /** Transitional low-level observation; generic callers consume initialDurableRanges. */
  readonly verifiedRanges: readonly PersistentByteRange[]
  writeRange(offset: bigint, data: Uint8Array, signal?: AbortSignal): Promise<void>
  automaticCheckpoint(
    trigger: AutomaticCheckpointTrigger,
    signal?: AbortSignal,
  ): Promise<PersistentAutomaticCheckpointResult>
  checkpoint(signal?: AbortSignal): Promise<readonly PersistentByteRange[]>
  commit(signal?: AbortSignal): Promise<PersistentFinalFileCommit>
  pause(reason?: unknown): Promise<readonly PersistentByteRange[]>
  retire(reason?: unknown): Promise<void>
  close(): Promise<void>
}

export type PersistentAutomaticCheckpointResult =
  | Readonly<{
      readonly kind: 'advanced'
      readonly durableRanges: readonly PersistentByteRange[]
      readonly cost: PreservingWriterCost
    }>
  | Readonly<{
      readonly kind: 'deferred'
      readonly reason: 'capacity-unavailable' | 'checkpoint-priority'
      readonly estimate: PreservingWriterCost
    }>
  | Readonly<{
      readonly kind: 'finished'
      readonly reason:
        | 'prefix-copy-budget'
        | 'cumulative-write-amplification-budget'
        | 'cost-evidence-unavailable'
      readonly estimate: PreservingWriterCost
    }>

export interface PersistentFinalFileCommit extends FinalFileCheckpointProof {
  readonly checkpointProof: FinalFileCheckpointProof
  readonly finalOutput: VerifiedFinalOutputFile
}

export interface PersistentByteRange {
  readonly start: bigint
  readonly end: bigint
}

export type RecoverableFileCheckpointJournal =
  FileCheckpointJournal & Pick<FileCheckpointRecoveryRepository, 'resolveCandidate'>

export type SemanticPersistentOutputJournal =
  RecoverableFileCheckpointJournal &
  SemanticFileCheckpointJournal<unknown> &
  Pick<MaterializationLedgerJournal,
    | 'appendDirectoryAdmission'
    | 'appendDirectoryFinalization'
    | 'commitFinalFile'
    | 'readMaterializationFinalProof'>

export interface PersistentTreeSessionOptions {
  readonly tree: PersistentOutputTree
  readonly checkpoints: RecoverableFileCheckpointJournal
  readonly semantic?: SemanticPersistentOutputJournal
  readonly maximumConcurrentInitialClaimInspections?: number
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageAuthority?: PersistentOutputStageAuthority
  readonly trace?: PersistentTreeTrace
}

export type PersistentTreeTraceEvent =
  | Readonly<{
      name: 'receive.operation.needs_attention'
      operation_id: string
      prior_state: 'receiving'
      needs_attention_reason: 'target-ownership-unknown'
    }>
  | Readonly<{
      name: 'receive.checkpoint.decision'
      operation_id: string
      file_id: string
      record_id?: string
      decision: 'absent' | 'installed' | 'exact' | 'revision-conflict' | 'ownership-conflict' | 'invalid'
    }>

export type PersistentTreeTrace = (event: PersistentTreeTraceEvent) => void
