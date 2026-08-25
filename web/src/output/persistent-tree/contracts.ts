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
  OutputCheckpointCost,
  OutputCheckpointCostBudget,
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
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly openRevision: () => Promise<OpenedFileRevision>
}

export type PersistentFileRecoveryPolicy =
  | Readonly<{
      readonly kind: 'preserve'
      readonly costBudget?: OutputCheckpointCostBudget
      readonly confirmTemporarySpace?: (
        preflight: PersistentWriterPreflight,
      ) => boolean | Promise<boolean>
    }>
  | Readonly<{
      readonly kind: 'restart-owned-file'
      readonly expectedOwnedObjectId: string
    }>

export type PersistentWriterOpenMode = 'preserve' | 'truncate'

export interface PersistentWriterPreflight {
  readonly cost: OutputCheckpointCost
  readonly space: 'within-modeled-budget' | 'requires-user-confirmation'
}

export interface PersistentTreeFile {
  readonly ownedObjectId: string
  readonly persistedHandle?: PersistentHandleRecord<unknown>
  openWriter?(mode: PersistentWriterOpenMode): Promise<void>
  checkpointPreflight?(
    durablePrefixBytes: bigint,
    cumulativeWriteAmplificationBytes: bigint,
  ): PersistentWriterPreflight
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
    budget: OutputCheckpointCostBudget,
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
      readonly cost: OutputCheckpointCost
    }>
  | Readonly<{
      readonly kind: 'declined'
      readonly reason:
        | 'prefix-copy-budget'
        | 'cumulative-write-amplification-budget'
        | 'peak-temporary-space-budget'
        | 'cost-evidence-unavailable'
        | 'temporary-space-confirmation-required'
      readonly estimate: OutputCheckpointCost
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
