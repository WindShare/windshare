import type { OutputDiagnosticsPorts } from '../diagnostics'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
} from '../persistence/journal'
import type { FileCheckpointRecoveryRepository } from './recovery'
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
  readonly artifactPath: readonly string[]
  readonly openRevision: () => Promise<OpenedFileRevision>
}

export interface PersistentTreeFile {
  readonly ownedObjectId: string
  writeAt(offset: bigint, data: Uint8Array): Promise<void>
  flush(): Promise<void>
  size(): Promise<bigint>
  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void>
  close(): Promise<void>
  read(): Promise<Blob>
}

export interface PersistentDirectoryMaterialization {
  readonly ownedObjectId: string
  readonly created: boolean
}

/** All paths are canonical artifact-path segments rooted in one immutable plan. */
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
  close(): Promise<void>
}

/** Root publication is an explicit post-binding step for PrefixVisible destinations. */
export interface ActivatablePersistentMaterializationPort extends PersistentMaterializationPort {
  activate(): Promise<void>
}

export interface PersistentFileTransactionPort {
  readonly revision: OpenedFileRevision
  readonly ownedObjectId: string
  readonly verifiedRanges: readonly PersistentByteRange[]
  writeRange(offset: bigint, data: Uint8Array, signal?: AbortSignal): Promise<void>
  checkpoint(signal?: AbortSignal): Promise<readonly PersistentByteRange[]>
  commit(signal?: AbortSignal): Promise<FinalFileCheckpointProof>
  close(): Promise<void>
}

export interface PersistentByteRange {
  readonly start: bigint
  readonly end: bigint
}

export type RecoverableFileCheckpointJournal =
  FileCheckpointJournal & Pick<FileCheckpointRecoveryRepository, 'resolveCandidate'>

export interface PersistentTreeSessionOptions {
  readonly tree: PersistentOutputTree
  readonly checkpoints: RecoverableFileCheckpointJournal
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
