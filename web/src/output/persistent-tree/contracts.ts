import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
} from '../persistence/journal'

export interface OpenedFileRevision {
  readonly fileId: string
  readonly fileRevision: string
  readonly exactSize: bigint
}

export interface PersistentFileRequest {
  readonly artifactPath: readonly string[]
  /**
   * Namespace mutation is sequenced after this promise resolves. The callback is the
   * authenticated content-session revision boundary, not a catalog-size assertion.
   */
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
  createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<PersistentTreeFile>
  openFile(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined>
  removeFile(path: readonly string[], ownedObjectId: string): Promise<void>
  removeDirectory(path: readonly string[], ownedObjectId: string): Promise<void>
}

export interface PersistentMaterializationPort {
  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort>
  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization>
  close(): Promise<void>
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

export interface PersistentTreeSessionOptions {
  readonly tree: PersistentOutputTree
  readonly checkpoints: FileCheckpointJournal
  readonly trace?: PersistentTreeTrace
}

export type PersistentTreeTraceEvent =
  Readonly<{
    name: 'receive.operation.needs_attention'
    operation_id: string
    prior_state: 'receiving'
    needs_attention_reason: 'target-ownership-unknown'
  }>

export type PersistentTreeTrace = (event: PersistentTreeTraceEvent) => void

export const NOOP_PERSISTENT_TREE_TRACE: PersistentTreeTrace = () => undefined
