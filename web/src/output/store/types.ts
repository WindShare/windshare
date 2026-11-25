import type { ByteRange } from '../../content/geometry'
import type { ReceiveOperationDisplay } from '../model/operation-display'
import type { SaveBackendKind } from '../model/offers'
import type { ResolvedArtifactSpec, SelectionSpec } from '../../transfer/selection'
import type {
  ReceiveLifecycleState,
  SafeRenamedSummary,
  SourceRevisionFailures,
} from '../../transfer/state'
import type { ZipCrcRange } from '../zip/crc-ranges'

export type CompletedRangeTuple = readonly [bigint, bigint]

/**
 * Local OS file metadata captured immediately after reserving a single-file
 * placeholder or flushing an unfinished FSA write, used to verify ownership.
 */
export interface FSADiskFingerprint {
  readonly size: number
  readonly lastModified: number
}

export interface FSAReservedFile {
  readonly handle: FileSystemFileHandle
  readonly fingerprint: FSADiskFingerprint
}

export type FSATargetRecord =
  | Readonly<{
      kind: 'fsa'
      parentHandle: FileSystemDirectoryHandle
      reservedName: string
      collisionIndex: number
      singleFile: false
      /** Timestamp before reserving the destination so resume can recognize uncommitted empty placeholders. */
      reservedAtMs: number
    }>
  | Readonly<{
      kind: 'fsa'
      parentHandle: FileSystemDirectoryHandle
      reservedName: string
      collisionIndex: number
      singleFile: true
      reservedFile: FSAReservedFile
    }>

export interface OPFSTargetRecord {
  readonly kind: 'opfs'
  readonly objectName: string
  readonly fileName: string
  readonly isZip: boolean
  readonly allocatedLength: bigint
  readonly sealedSize?: bigint
}

export interface PortableTargetRecord {
  readonly kind: 'portable'
  readonly fileName: string
}

export type TaskTargetRecord = FSATargetRecord | OPFSTargetRecord | PortableTargetRecord

export interface SafeRenamedDirectoryRecord {
  readonly logicalPath: readonly string[]
  readonly physicalPath: readonly string[]
}

/** Plain JSON task row stored in the `tasks` IndexedDB object store. */
export interface ReceiveTaskRecord {
  readonly taskId: string
  readonly shareInstance: string
  readonly selectionKey: string
  readonly selection: SelectionSpec
  readonly artifact: ResolvedArtifactSpec
  readonly backend: SaveBackendKind
  readonly lifecycle: ReceiveLifecycleState
  readonly target: TaskTargetRecord
  readonly display?: ReceiveOperationDisplay
  readonly safeRenamed?: SafeRenamedSummary
  readonly safeRenamedDirectories?: readonly SafeRenamedDirectoryRecord[]
  readonly sourceOmittedEntries?: bigint
  readonly sourceRevisionFailures?: SourceRevisionFailures
  readonly discoveryComplete: boolean
  readonly updatedAt: number
}

export interface ZipEntryLayoutRecord {
  readonly sequence: number
  readonly kind: 'file' | 'directory'
  readonly localHeaderOffset: bigint
  readonly payloadOffset: bigint
  readonly descriptorOffset: bigint
  readonly endOffset: bigint
  readonly crcRanges: readonly ZipCrcRange[]
  readonly crc32?: number
  readonly modifiedTimeMilliseconds?: bigint
}

/**
 * Plain JSON per-file row stored in the `files` IndexedDB object store.
 * Unifies both FSA direct-write progress and OPFS single-file / progressive-ZIP progress.
 */
export interface ReceiveFileRecord {
  readonly id: string
  readonly taskId: string
  readonly fileId: string
  readonly revision: string
  readonly sourceMetadata?: string
  readonly path: readonly string[]
  /** Physical components under the reserved root, including OS-safe names. */
  readonly fsaPath?: readonly string[]
  /** While close and IndexedDB disagree, only the previous on-disk version has a known fingerprint. */
  readonly fsaPendingPublish?: Readonly<{ fingerprint: FSADiskFingerprint; ranges: readonly CompletedRangeTuple[] }>
  readonly size: bigint
  readonly completedRanges: readonly CompletedRangeTuple[]
  readonly status: 'receiving' | 'completed' | 'failed'
  readonly handle?: FileSystemFileHandle
  readonly fsaDiskFingerprint?: FSADiskFingerprint
  readonly zipEntry?: ZipEntryLayoutRecord
}

export function fileRecordId(taskId: string, fileId: string): string {
  return `${taskId}:${fileId}`
}

export function toRangeTuples(ranges: readonly ByteRange[]): readonly CompletedRangeTuple[] {
  return Object.freeze(ranges.map(range => Object.freeze([range.start, range.end] as const)))
}

export function fromRangeTuples(tuples: readonly CompletedRangeTuple[]): readonly ByteRange[] {
  return Object.freeze(tuples.map(([start, end]) => Object.freeze({ start, end })))
}

export function completedTuplesBytes(tuples: readonly CompletedRangeTuple[]): bigint {
  let total = 0n
  for (const [start, end] of tuples) {
    if (end > start) total += end - start
  }
  return total
}

export function maxRangeEnd(tuples: readonly CompletedRangeTuple[]): bigint {
  let max = 0n
  for (const [, end] of tuples) {
    if (end > max) max = end
  }
  return max
}
