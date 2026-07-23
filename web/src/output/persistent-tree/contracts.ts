export interface PersistentTreeFile {
  readonly identity: string
  writeAt(offset: bigint, data: Uint8Array): Promise<void>
  flush(): Promise<void>
  size(): Promise<bigint>
  close(): Promise<void>
  read(): Promise<Blob>
}

export interface PersistentDirectoryMaterialization {
  readonly identity: string
  readonly created: boolean
}

/** All paths are capability-rooted segments; implementations never accept OS paths. */
export interface PersistentOutputTree {
  authorize(): Promise<void>
  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization>
  validateDirectory(path: readonly string[], identity: string): Promise<boolean>
  createFileExclusive(path: readonly string[]): Promise<PersistentTreeFile>
  openFile(path: readonly string[], identity: string): Promise<PersistentTreeFile | undefined>
  removeFile(path: readonly string[], identity: string): Promise<void>
  removeDirectory(path: readonly string[], identity: string): Promise<void>
  forgetIdentity?(identity: string): Promise<void>
  setFileModificationTime?(
    path: readonly string[],
    identity: string,
    milliseconds: bigint,
  ): Promise<void>
  setDirectoryModificationTime?(
    path: readonly string[],
    identity: string,
    milliseconds: bigint,
  ): Promise<void>
}

export type CheckpointCrashPhase =
  | 'DataWritten'
  | 'DataFlushed'
  | 'JournalWritten'
  | 'JournalFlushed'
  | 'CheckpointCommitted'
  | 'CheckpointVerified'

export type CheckpointCrashHook = (
  phase: CheckpointCrashPhase,
  fileKey: string,
) => void | Promise<void>
