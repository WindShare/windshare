import type { ByteRange } from '../content/geometry'
import type { SafeRenamedSummary } from './state'

export interface TransferFileRequest {
  readonly fileId: string
  readonly path: readonly string[]
  readonly sourcePath: readonly string[]
  readonly revision: string
  readonly sourceMetadata?: string
  readonly exactSize: bigint
  readonly modifiedTimeMilliseconds?: bigint
}

export interface TransferDirectoryRequest {
  readonly directoryId: string
  readonly path: readonly string[]
  readonly modifiedTimeMilliseconds?: bigint
}

export interface CompletedOutputFile {
  readonly size: bigint
}

/**
 * Storage-neutral file writer consumed by `transfer/file-transfer.ts`.
 * Backends guarantee physical bytes are flushed before `checkpoint()` persists
 * `completedRanges` to IndexedDB.
 */
export interface TransferOutputFile {
  /** Durable ranges already verified on disk from a prior run. */
  readonly completedRanges: readonly ByteRange[]
  /** Saved revision from a prior run, if any, used to detect source file changes. */
  readonly savedRevision?: string
  readonly savedSourceMetadata?: string
  readonly savedSize?: bigint
  writeAt(offset: bigint, data: Uint8Array): Promise<void>
  /** Flush physical storage first, then durably record `ranges` in IndexedDB. */
  checkpoint(ranges: readonly ByteRange[]): Promise<void>
  /** Flush, close, and mark this file complete. */
  commit(ranges: readonly ByteRange[]): Promise<void>
  /** Flush accepted bytes and record `ranges` for later resume. */
  pause(ranges: readonly ByteRange[]): Promise<void>
  abort(reason?: unknown): Promise<void>
}

export interface PeriodicCheckpointPolicy {
  readonly intervalBytes: bigint
  readonly intervalMilliseconds: number
}

export interface TransferOutputCapabilities {
  readonly maxConcurrentFiles: number
  /**
   * Optional mid-file periodic checkpoint schedule.
   * Only backends with cheap in-place flushing (e.g. OPFS SyncAccessHandle)
   * should advertise this; copy-on-reopen streams like FSA commit on `pause()`,
   * `commit()`, and explicit lease-replacement `checkpoint()` calls instead.
   */
  readonly periodicCheckpoint?: PeriodicCheckpointPolicy
}

export interface TransferOutputSession {
  readonly sessionId: string
  readonly capabilities: TransferOutputCapabilities
  ensureDirectory?(request: TransferDirectoryRequest, signal: AbortSignal): Promise<void>
  /**
   * Verifies whether `fileId` was already completed and committed locally in a prior run.
   * Checked before acquiring a remote revision lease so resumed transfers skip completed
   * files without remote round-trips or failing if the source file was later removed.
   */
  checkCompleted?(fileId: string, signal: AbortSignal): Promise<CompletedOutputFile | undefined>
  openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile>
  markDiscoveryComplete?(): Promise<void>
  safeRenamedSummary?(): SafeRenamedSummary | undefined
}
