import { ByteRangeSet, type ByteRange } from '../content/geometry'
import {
  V2_CATALOG_NAME_BYTES,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import type { JobOutcome } from './outcome'

export type DurabilityLevel = 'None' | 'ProcessRestart' | 'PowerLoss'
export type FileAbortDisposition = 'FileIsolated' | 'JobOutputCompromised'

export const MAXIMUM_OUTPUT_PATH_SEGMENTS = V2_CATALOG_PATH_DEPTH
export const MAXIMUM_OUTPUT_SEGMENT_BYTES = V2_CATALOG_NAME_BYTES
export const MAXIMUM_OUTPUT_PATH_BYTES = V2_CATALOG_PATH_BYTES
export const MAXIMUM_OPEN_OUTPUT_FILES = 32

export interface OutputCapabilities {
  readonly durability: DurabilityLevel
  readonly randomWrite: boolean
  readonly fileFailureIsolation: boolean
  readonly modificationTime: boolean
}

export interface OutputSourceIdentity {
  readonly shareInstance: string
  readonly fileId: string
  readonly fileRevision: string
}

export interface OutputSessionIdentity {
  readonly backend: string
  readonly outputSessionId: string
}

export interface OutputFileOwnership extends OutputSessionIdentity {
  readonly canonicalPath: readonly string[]
  /** Prevents a journal-owned path from matching a pre-existing file at the same path. */
  readonly ownedFileIdentity: string
}

export interface OutputDirectory {
  readonly path: readonly string[]
  readonly modifiedTimeMilliseconds?: bigint
}

export interface OutputFile {
  readonly source: OutputSourceIdentity
  readonly path: readonly string[]
  readonly exactSize: bigint
  readonly modifiedTimeMilliseconds?: bigint
}

/** Only a backend may return this value after reopening and validating output. */
export class VerifiedDurableRanges {
  readonly ownership: OutputFileOwnership
  readonly source: OutputSourceIdentity
  readonly #ranges: ByteRangeSet

  constructor(
    ownership: OutputFileOwnership,
    source: OutputSourceIdentity,
    fileSize: bigint,
    ranges: readonly ByteRange[],
  ) {
    this.ownership = snapshotOutputOwnership(ownership)
    this.source = snapshotOutputSource(source)
    this.#ranges = new ByteRangeSet(fileSize, ranges)
  }

  get fileSize(): bigint {
    return this.#ranges.fileSize
  }

  get ranges(): readonly ByteRange[] {
    return this.#ranges.ranges
  }

  covers(range: ByteRange): boolean {
    return this.#ranges.covers(range)
  }

  asRangeSet(): ByteRangeSet {
    return new ByteRangeSet(this.fileSize, this.ranges)
  }
}

export interface OutputFileTransaction {
  writeRange(offset: bigint, data: Uint8Array): Promise<void>
  /** Data durability must be established before the journal result is returned. */
  checkpoint(): Promise<VerifiedDurableRanges>
  commit(): Promise<void>
  /** A streaming backend may still isolate a failure when no bytes were emitted. */
  abort(reason: unknown): Promise<FileAbortDisposition>
}

export interface BeginOutputFileResult {
  readonly transaction: OutputFileTransaction
  readonly durableRanges: VerifiedDurableRanges
}

/**
 * Transfer orchestration consumes this narrow interface and cannot infer that a
 * completed write is durable. Backend implementations own validation and journals.
 */
export interface OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  ensureDirectory(directory: OutputDirectory): Promise<void>
  finalizeDirectory(directory: OutputDirectory, signal: AbortSignal): Promise<void>
  /** Rejection must leave no unowned partial output because no transaction is returned. */
  beginFile(file: OutputFile): Promise<BeginOutputFileResult>
  finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void>
  abortJob(reason: unknown): Promise<void>
  /** Durable backends retain verified ranges while releasing live browser resources. */
  suspendJob?(reason: unknown): Promise<void>
}

export class OutputSessionSuspendedError extends Error {
  constructor() {
    super('Output session suspended for receiver restart')
    this.name = 'OutputSessionSuspendedError'
  }
}

export function outputCapabilities(
  capabilities: OutputCapabilities,
): OutputCapabilities {
  return Object.freeze({ ...capabilities })
}

export function outputSessionIdentity(identity: OutputSessionIdentity): OutputSessionIdentity {
  return Object.freeze({
    backend: requireIdentityPart(identity.backend, 'output backend'),
    outputSessionId: requireIdentityPart(identity.outputSessionId, 'output session'),
  })
}

export function snapshotOutputDirectory(directory: OutputDirectory): OutputDirectory {
  return Object.freeze({
    path: snapshotOutputPath(directory.path),
    ...(directory.modifiedTimeMilliseconds === undefined
      ? {}
      : { modifiedTimeMilliseconds: directory.modifiedTimeMilliseconds }),
  })
}

export function snapshotOutputFile(file: OutputFile): OutputFile {
  if (file.exactSize < 0n) {
    throw new RangeError('output file size must not be negative')
  }
  return Object.freeze({
    source: snapshotOutputSource(file.source),
    path: snapshotOutputPath(file.path),
    exactSize: file.exactSize,
    ...(file.modifiedTimeMilliseconds === undefined
      ? {}
      : { modifiedTimeMilliseconds: file.modifiedTimeMilliseconds }),
  })
}

function snapshotOutputSource(source: OutputSourceIdentity): OutputSourceIdentity {
  const values = [
    ['shareInstance', source.shareInstance],
    ['fileId', source.fileId],
    ['fileRevision', source.fileRevision],
  ] as const
  for (const [label, value] of values) {
    if (typeof value !== 'string' || value.length === 0) {
      throw new TypeError(`output source ${label} must not be empty`)
    }
  }
  return Object.freeze({
    shareInstance: source.shareInstance,
    fileId: source.fileId,
    fileRevision: source.fileRevision,
  })
}

function snapshotOutputOwnership(ownership: OutputFileOwnership): OutputFileOwnership {
  const identity = outputSessionIdentity(ownership)
  return Object.freeze({
    ...identity,
    canonicalPath: snapshotOutputPath(ownership.canonicalPath),
    ownedFileIdentity: requireIdentityPart(
      ownership.ownedFileIdentity,
      'owned output file',
    ),
  })
}

function requireIdentityPart(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${label} identity must not be empty`)
  }
  return value
}

export function snapshotOutputPath(path: readonly string[]): readonly string[] {
  if (path.length === 0) {
    throw new TypeError('output path must contain at least one segment')
  }
  if (path.length > MAXIMUM_OUTPUT_PATH_SEGMENTS) {
    throw new TypeError('output path exceeds its segment limit')
  }
  let pathBytes = path.length - 1
  for (const segment of path) {
    if (
      segment.length === 0 ||
      segment === '.' ||
      segment === '..' ||
      segment.includes('/') ||
      segment.includes('\\') ||
      segment.includes('\0') ||
      !hasWellFormedUtf16(segment)
    ) {
      throw new TypeError('output path contains an unsafe structural segment')
    }
    const segmentBytes = new TextEncoder().encode(segment).byteLength
    if (segmentBytes > MAXIMUM_OUTPUT_SEGMENT_BYTES) {
      throw new TypeError('output path segment exceeds its byte limit')
    }
    pathBytes += segmentBytes
  }
  if (pathBytes > MAXIMUM_OUTPUT_PATH_BYTES) {
    throw new TypeError('output path exceeds its byte limit')
  }
  return Object.freeze([...path])
}

function hasWellFormedUtf16(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code >= 0xd800 && code <= 0xdbff) {
      if (index + 1 >= value.length) return false
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false
    }
  }
  return true
}
