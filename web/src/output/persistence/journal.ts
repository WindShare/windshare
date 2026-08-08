import { ByteRangeSet, type ByteRange } from '../../content/geometry'
import { encodeBase64Url } from '../../crypto/bytes'
import type {
  OutputFile,
  OutputFileOwnership,
  OutputSessionIdentity,
  OutputSourceIdentity,
} from '../../transfer/output-session'
import { snapshotOutputPath } from '../../transfer/output-session'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PUBLISHED,
  FILE_CHECKPOINT_V1_SCHEMA_VERSION,
  canonicalFileCheckpointBackend,
  type FileCheckpointRange,
  type FileCheckpointV1,
  deriveCheckpointIdentity,
  identityBytes,
  newFileCheckpointV1,
  validateFileCheckpoint,
} from './checkpoint'
import { durableCheckpointNamespaceIdentity } from './namespace'

/** The physical store may use this bounded page size without changing semantics. */
export const OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT = 128
export const OUTPUT_JOURNAL_PAGE_RECORD_LIMIT = OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT

export interface CheckpointNamespaceBinding {
  readonly backend: string
  /** Stable TransferIntentDigest; never a run/session identifier. */
  readonly transferIntentDigest: string
  /** Stable receiver root identity, usually derived from a picker capability. */
  readonly rootIdentity: string
}

export type CheckpointOwner = OutputSessionIdentity & CheckpointNamespaceBinding

interface PersistedRecordBinding {
  readonly schemaVersion: typeof FILE_CHECKPOINT_V1_SCHEMA_VERSION
  readonly ownershipMarker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly recordId: string
  readonly backend: string
  readonly rootIdentity: string
  readonly transferIntentDigest: string
  readonly stateGeneration: bigint
  readonly checkpointGeneration: bigint
  readonly phase: number
  readonly commitState: number
  readonly checksum: string
}

export interface PersistedDirectoryRecord extends PersistedRecordBinding {
  readonly kind: 'directory'
  readonly generation: bigint
  readonly canonicalPath: readonly string[]
  readonly ownedDirectoryIdentity: string
  readonly createdBySession: boolean
  readonly modifiedTimeMilliseconds?: bigint
  readonly finalized: boolean
}

export interface PersistedFileRecord extends PersistedRecordBinding {
  readonly kind: 'file'
  readonly generation: bigint
  readonly canonicalPath: readonly string[]
  readonly ownedFileIdentity: string
  readonly source: OutputSourceIdentity
  readonly exactSize: bigint
  readonly durableRanges: readonly ByteRange[]
  readonly committed: boolean
  readonly fileCheckpoint: FileCheckpointV1
  readonly quarantineReason: FileCheckpointV1['quarantineReason']
  readonly quarantineOrigin: FileCheckpointV1['quarantineOrigin']
  readonly retirementReason: FileCheckpointV1['retirementReason']
  readonly modifiedTimeMilliseconds?: bigint
}

export type PersistedOutputRecord = PersistedDirectoryRecord | PersistedFileRecord

export interface OutputJournalScan {
  readonly kind?: PersistedOutputRecord['kind']
  readonly direction: 'ascending' | 'descending'
  /** Exclusive durable record key returned by the previous page. */
  readonly cursor?: string
}

export interface OutputJournalPage {
  readonly records: readonly PersistedOutputRecord[]
  /** Present only when another bounded query may contain more records. */
  readonly nextCursor?: string
}

/**
 * A candidate and its committed publication are deliberately separate. Browser
 * adapters may use different physical mechanisms, but every phase is reopened
 * through FileCheckpointV1 before it grants durable ranges to a transfer.
 */
export interface OutputCheckpointJournal {
  readonly binding: CheckpointNamespaceBinding
  scanCommitted(scan: OutputJournalScan): Promise<OutputJournalPage>
  scanCandidates(scan: OutputJournalScan): Promise<OutputJournalPage>
  writeCandidate(record: PersistedOutputRecord): Promise<void>
  flushCandidate(key: string): Promise<void>
  commitCandidate(key: string): Promise<void>
  /** A separate read transaction is the journal half of reopen verification. */
  readCommitted(key: string): Promise<PersistedOutputRecord | undefined>
  discardCandidate(key: string): Promise<void>
  deleteCommitted(key: string): Promise<void>
}

export function validateOutputJournalPage(
  page: OutputJournalPage,
  scan: OutputJournalScan,
  identity: CheckpointNamespaceBinding,
): OutputJournalPage {
  if (page.records.length > OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT) throw new TypeError('Output checkpoint page exceeds its fixed record limit')
  let previous = scan.cursor
  const records = page.records.map((candidate) => {
    const record = snapshotOutputRecord(candidate)
    if (!recordBelongsToCheckpointNamespace(record, identity) ||
        (scan.kind !== undefined && record.kind !== scan.kind)) {
      throw new TypeError('Output checkpoint page escaped its namespace or kind boundary')
    }
    const key = outputRecordKey(record)
    if (previous !== undefined) {
      const order = compareRecordKeys(key, previous)
      if ((scan.direction === 'ascending' && order <= 0) || (scan.direction === 'descending' && order >= 0)) throw new TypeError('Output checkpoint cursor did not advance monotonically')
    }
    previous = key
    return record
  })
  const last = records.at(-1)
  if (records.length === OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT && page.nextCursor === undefined) throw new TypeError('Output checkpoint full page omitted its continuation cursor')
  if (page.nextCursor !== undefined && (records.length !== OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT || last === undefined || page.nextCursor !== outputRecordKey(last))) throw new TypeError('Output checkpoint next cursor does not identify the bounded page tail')
  return Object.freeze({ records: Object.freeze(records), ...(page.nextCursor === undefined ? {} : { nextCursor: page.nextCursor }) })
}

/** A logical index remains path-addressable; the stable V1 recordId is separate. */
export function outputRecordKey(record: PersistedOutputRecord): string {
  return `${record.kind}:${outputPathKey(record.canonicalPath)}`
}

export function outputPathKey(path: readonly string[]): string {
  return path.map((segment) => encodeURIComponent(segment)).join('/')
}

export function fileRecord(
  identity: CheckpointOwner,
  ownership: OutputFileOwnership,
  file: OutputFile,
  ranges: readonly ByteRange[],
  committed: boolean,
  generation: bigint,
): PersistedFileRecord {
  const binding = namespaceBinding(identity)
  requireSessionBinding(identity, ownership)
  if (generation <= 0n) throw new TypeError('checkpoint generation must be positive')
  const canonicalPath = snapshotOutputPath(ownership.canonicalPath)
  const durableRanges = new ByteRangeSet(file.exactSize, ranges).ranges
  const checkpoint = newFileCheckpointV1({
    transferIntentDigest: binding.transferIntentDigest,
    fileId: file.source.fileId,
    fileRevision: file.source.fileRevision,
    canonicalPath,
    exactSize: file.exactSize,
    backend: binding.backend,
    rootIdentity: binding.rootIdentity,
    ownedOutputObject: ownership.ownedFileIdentity,
    stateGeneration: generation,
    checkpointGeneration: generation,
    verifiedRanges: durableRanges.map(toCheckpointRange),
    phase: committed ? FILE_CHECKPOINT_PHASE_PUBLISHED : FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: committed ? FILE_CHECKPOINT_COMMIT_PUBLISHED : FILE_CHECKPOINT_COMMIT_CANDIDATE,
  })
  const record = {
    ...persistedBinding(checkpoint),
    kind: 'file' as const,
    generation,
    canonicalPath,
    ownedFileIdentity: checkpoint.ownedOutputObject,
    source: snapshotSource(file.source),
    exactSize: file.exactSize,
    durableRanges,
    committed,
    fileCheckpoint: checkpoint,
    quarantineReason: checkpoint.quarantineReason,
    quarantineOrigin: checkpoint.quarantineOrigin,
    retirementReason: checkpoint.retirementReason,
    ...(file.modifiedTime === undefined ? {} : { modifiedTimeMilliseconds: file.modifiedTime.milliseconds }),
  }
  return Object.freeze({ ...record })
}

export function directoryRecord(
  identity: CheckpointOwner,
  path: readonly string[],
  ownedDirectoryIdentity: string,
  createdBySession: boolean,
  modifiedTimeMilliseconds: bigint | undefined,
  finalized: boolean,
  generation: bigint,
): PersistedDirectoryRecord {
  const binding = namespaceBinding(identity)
  if (generation <= 0n) throw new TypeError('checkpoint generation must be positive')
  const canonicalPath = snapshotOutputPath(path)
  const ownedIdentity = canonicalIdentity(
    ownedDirectoryIdentity,
    'owned directory',
  )
  const recordId = deriveCheckpointIdentity(stableMetadataString([
    'directory-admission-v1', binding.transferIntentDigest, binding.backend, binding.rootIdentity,
    canonicalPath, ownedIdentity,
  ]))
  const base = {
    schemaVersion: FILE_CHECKPOINT_V1_SCHEMA_VERSION,
    ownershipMarker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespace: FILE_CHECKPOINT_NAMESPACE,
    recordId,
    backend: binding.backend,
    rootIdentity: binding.rootIdentity,
    transferIntentDigest: binding.transferIntentDigest,
    stateGeneration: generation,
    checkpointGeneration: generation,
    phase: finalized ? FILE_CHECKPOINT_PHASE_PUBLISHED : FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: finalized ? FILE_CHECKPOINT_COMMIT_PUBLISHED : FILE_CHECKPOINT_COMMIT_CANDIDATE,
    kind: 'directory' as const,
    generation,
    canonicalPath,
    ownedDirectoryIdentity: ownedIdentity,
    createdBySession,
    ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    finalized,
  }
  return Object.freeze({ ...base, checksum: deriveCheckpointIdentity(directoryChecksumPayload(base)) })
}

export function snapshotOutputRecord(record: PersistedOutputRecord): PersistedOutputRecord {
  if (record.schemaVersion !== FILE_CHECKPOINT_V1_SCHEMA_VERSION ||
      record.ownershipMarker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      record.namespace !== FILE_CHECKPOINT_NAMESPACE) {
    throw new TypeError('persisted output record ownership is invalid')
  }
  if (Object.hasOwn(record, 'outputSessionId')) {
    throw new TypeError('persisted output record contains runtime session identity')
  }
  if (record.kind === 'file') {
    const checkpoint = record.fileCheckpoint
    if (checkpoint === undefined) {
      throw new TypeError('persisted file record omitted its canonical FileCheckpointV1')
    }
    validateFileCheckpoint(checkpoint)
    if (checkpoint.recordId !== record.recordId || checkpoint.checksum !== record.checksum ||
        checkpoint.transferIntentDigest !== record.transferIntentDigest ||
        checkpoint.fileId !== record.source.fileId ||
        checkpoint.fileRevision !== record.source.fileRevision ||
        checkpoint.backend !== record.backend || checkpoint.rootIdentity !== record.rootIdentity ||
        checkpoint.ownedOutputObject !== record.ownedFileIdentity ||
        checkpoint.stateGeneration !== record.stateGeneration ||
        checkpoint.checkpointGeneration !== record.checkpointGeneration ||
        checkpoint.phase !== record.phase || checkpoint.commitState !== record.commitState ||
        checkpoint.quarantineReason !== record.quarantineReason ||
        checkpoint.quarantineOrigin !== record.quarantineOrigin ||
        checkpoint.retirementReason !== record.retirementReason ||
        checkpoint.canonicalPath !== outputPathText(record.canonicalPath) || checkpoint.exactSize !== record.exactSize) {
      throw new TypeError('persisted file checkpoint binding is invalid')
    }
    const durableRanges = new ByteRangeSet(record.exactSize, record.durableRanges).ranges
    if (durableRanges.length !== checkpoint.verifiedRanges.length || durableRanges.some((range, index) => range.start !== checkpoint.verifiedRanges[index]!.start || range.end !== checkpoint.verifiedRanges[index]!.end)) throw new TypeError('persisted file ranges do not match FileCheckpointV1')
    return Object.freeze({
      ...record,
      canonicalPath: Object.freeze([...record.canonicalPath]),
      source: snapshotSource(record.source),
      durableRanges,
      fileCheckpoint: checkpoint,
    })
  }
  if (record.schemaVersion !== FILE_CHECKPOINT_V1_SCHEMA_VERSION || record.ownershipMarker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || record.namespace !== FILE_CHECKPOINT_NAMESPACE) throw new TypeError('persisted directory metadata ownership is invalid')
  canonicalIdentity(record.transferIntentDigest, 'transfer intent digest')
  canonicalFileCheckpointBackend(record.backend)
  canonicalIdentity(record.rootIdentity, 'root identity')
  canonicalIdentity(record.ownedDirectoryIdentity, 'owned directory')
  if (record.checksum !== deriveCheckpointIdentity(directoryChecksumPayload(record))) throw new TypeError('persisted directory metadata checksum is invalid')
  return Object.freeze({ ...record, canonicalPath: Object.freeze([...record.canonicalPath]) })
}

export function sameOutputRecord(left: PersistedOutputRecord, right: PersistedOutputRecord): boolean {
  return left.checksum === right.checksum && left.kind === right.kind && left.recordId === right.recordId && left.generation === right.generation && outputRecordKey(left) === outputRecordKey(right)
}

export function recordBelongsToCheckpointNamespace(
  record: PersistedOutputRecord,
  identity: CheckpointNamespaceBinding,
): boolean {
  const binding = namespaceBinding(identity)
  return record.backend === binding.backend &&
    record.transferIntentDigest === binding.transferIntentDigest &&
    record.rootIdentity === binding.rootIdentity
}

function namespaceBinding(identity: CheckpointNamespaceBinding): CheckpointNamespaceBinding {
  return durableCheckpointNamespaceIdentity(identity)
}

function persistedBinding(checkpoint: FileCheckpointV1): PersistedRecordBinding {
  return {
    schemaVersion: FILE_CHECKPOINT_V1_SCHEMA_VERSION,
    ownershipMarker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespace: FILE_CHECKPOINT_NAMESPACE,
    recordId: checkpoint.recordId,
    backend: checkpoint.backend,
    rootIdentity: checkpoint.rootIdentity,
    transferIntentDigest: checkpoint.transferIntentDigest,
    stateGeneration: checkpoint.stateGeneration,
    checkpointGeneration: checkpoint.checkpointGeneration,
    phase: checkpoint.phase,
    commitState: checkpoint.commitState,
    checksum: checkpoint.checksum,
  }
}

function requireSessionBinding(identity: OutputSessionIdentity, ownership: OutputFileOwnership): void {
  if (ownership.backend !== identity.backend || ownership.outputSessionId !== identity.outputSessionId) throw new TypeError('output ownership belongs to another runtime session')
}

function snapshotSource(source: OutputSourceIdentity): OutputSourceIdentity {
  return Object.freeze({ shareInstance: requirePart(source.shareInstance, 'share instance'), fileId: requirePart(source.fileId, 'file'), fileRevision: requirePart(source.fileRevision, 'file revision') })
}

function requirePart(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new TypeError(`${label} identity must not be empty`)
  return value
}

function canonicalIdentity(value: string, label: string): string {
  return encodeBase64Url(identityBytes(value, 32, label))
}

function toCheckpointRange(range: ByteRange): FileCheckpointRange { return { start: range.start, end: range.end } }
function outputPathText(path: readonly string[]): string { return path.join('/') }
function directoryChecksumPayload(record: Omit<PersistedDirectoryRecord, 'checksum'> | PersistedDirectoryRecord): string {
  const durable = { ...record }
  Reflect.deleteProperty(durable, 'checksum')
  return stableMetadataString(durable)
}
function stableMetadataString(value: unknown): string {
  return JSON.stringify(value, (_key, candidate: unknown) => typeof candidate === 'bigint' ? `${candidate.toString()}n` : candidate)
}
function compareRecordKeys(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
