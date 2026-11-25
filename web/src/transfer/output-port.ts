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

export type CheckpointReason = 'periodic' | 'disconnect' | 'retry'
export type CheckpointOutcome = 'saved' | 'skipped'

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
  checkpoint(ranges: readonly ByteRange[], reason: CheckpointReason): Promise<CheckpointOutcome>
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
   * True when `checkpoint()` durably persists progress across file reopens
   * (FSA and OPFS), allowing mid-file lease replacement and reopened-revision
   * readmission without discarding written bytes.
   */
  readonly resumable: boolean
  /**
   * True when completed files survive another member's failure or a user stop
   * and can be delivered as a partial result (FSA folder writes and OPFS
   * progressive ZIP). False for all-or-nothing outputs (Portable in-memory
   * downloads), which discard buffered bytes unless every selected entry
   * completes and therefore halt immediately on the first member failure.
   */
  readonly partialOutput: boolean
  /**
   * Optional mid-file periodic checkpoint schedule.
   * Only backends with cheap in-place flushing (e.g. OPFS SyncAccessHandle)
   * should advertise this; copy-on-reopen streams like FSA commit on `pause()`,
   * `commit()`, and explicit retry or long-disconnect checkpoints.
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
