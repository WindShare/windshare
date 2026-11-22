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

/**
 * Storage-neutral file writer consumed by `transfer/file-transfer.ts`.
 * Backends guarantee physical bytes are flushed before `checkpoint()` persists
 * `completedRanges` to IndexedDB.
 */
export interface TransferOutputFile {
  /** Durable ranges already verified on disk from a prior run. */
  readonly completedRanges: readonly ByteRange[]
  /** True when the file was already completed and committed in an earlier run. */
  readonly alreadyCompleted: boolean
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

export interface TransferOutputCapabilities {
  readonly maxConcurrentFiles: number
  readonly supportsResume: boolean
  readonly checkpointIntervalBytes: bigint
  readonly checkpointIntervalMilliseconds: number
}

export interface TransferOutputSession {
  readonly sessionId: string
  readonly capabilities: TransferOutputCapabilities
  ensureDirectory?(request: TransferDirectoryRequest, signal: AbortSignal): Promise<void>
  openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile>
  markDiscoveryComplete?(): Promise<void>
  safeRenamedSummary?(): SafeRenamedSummary | undefined
}
