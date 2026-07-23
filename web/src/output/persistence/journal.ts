import { ByteRangeSet, type ByteRange } from '../../content/geometry'
import type {
  OutputFile,
  OutputFileOwnership,
  OutputSessionIdentity,
  OutputSourceIdentity,
} from '../../transfer/output-session'
import { snapshotOutputPath } from '../../transfer/output-session'

export interface PersistedDirectoryRecord extends OutputSessionIdentity {
  readonly journalSchema: typeof OUTPUT_JOURNAL_SCHEMA
  readonly generation: bigint
  readonly checksum: string
  readonly kind: 'directory'
  readonly canonicalPath: readonly string[]
  readonly ownedDirectoryIdentity: string
  readonly createdBySession: boolean
  readonly modifiedTimeMilliseconds?: bigint
  readonly finalized: boolean
}

export interface PersistedFileRecord extends OutputFileOwnership {
  readonly journalSchema: typeof OUTPUT_JOURNAL_SCHEMA
  readonly generation: bigint
  readonly checksum: string
  readonly kind: 'file'
  readonly source: OutputSourceIdentity
  readonly exactSize: bigint
  readonly durableRanges: readonly ByteRange[]
  readonly committed: boolean
  readonly modifiedTimeMilliseconds?: bigint
}

export type PersistedOutputRecord = PersistedDirectoryRecord | PersistedFileRecord

export const OUTPUT_JOURNAL_SCHEMA = 1

export const OUTPUT_JOURNAL_PAGE_RECORD_LIMIT = 128

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
 * storage adapters may use different physical mechanisms, but a completed call
 * must survive a fresh adapter instance before the next phase begins.
 */
export interface OutputCheckpointJournal {
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
  identity: OutputSessionIdentity,
): OutputJournalPage {
  if (page.records.length > OUTPUT_JOURNAL_PAGE_RECORD_LIMIT) {
    throw new TypeError('Output journal page exceeds its fixed record limit')
  }
  let previous = scan.cursor
  const records = page.records.map((candidate) => {
    const record = snapshotOutputRecord(candidate)
    if (!recordBelongsToSession(record, identity) ||
        (scan.kind !== undefined && record.kind !== scan.kind)) {
      throw new TypeError('Output journal page escaped its session or kind boundary')
    }
    const key = outputRecordKey(record)
    if (previous !== undefined) {
      const order = compareRecordKeys(key, previous)
      if ((scan.direction === 'ascending' && order <= 0) ||
          (scan.direction === 'descending' && order >= 0)) {
        throw new TypeError('Output journal page cursor did not advance monotonically')
      }
    }
    previous = key
    return record
  })
  const last = records.at(-1)
  if (records.length === OUTPUT_JOURNAL_PAGE_RECORD_LIMIT && page.nextCursor === undefined) {
    throw new TypeError('Output journal full page omitted its continuation cursor')
  }
  if (page.nextCursor !== undefined &&
      (records.length !== OUTPUT_JOURNAL_PAGE_RECORD_LIMIT ||
        last === undefined || page.nextCursor !== outputRecordKey(last))) {
    throw new TypeError('Output journal next cursor does not identify the bounded page tail')
  }
  return Object.freeze({
    records: Object.freeze(records),
    ...(page.nextCursor === undefined ? {} : { nextCursor: page.nextCursor }),
  })
}

export function outputRecordKey(record: PersistedOutputRecord): string {
  return `${record.kind}:${outputPathKey(record.canonicalPath)}`
}

export function outputPathKey(path: readonly string[]): string {
  return path.map((segment) => encodeURIComponent(segment)).join('/')
}

export function fileRecord(
  identity: OutputSessionIdentity,
  ownership: OutputFileOwnership,
  file: OutputFile,
  ranges: readonly ByteRange[],
  committed: boolean,
  generation: bigint,
): PersistedFileRecord {
  requireSessionBinding(identity, ownership)
  requireGeneration(generation)
  const durableRanges = new ByteRangeSet(file.exactSize, ranges).ranges
  const record = {
    journalSchema: OUTPUT_JOURNAL_SCHEMA,
    generation,
    kind: 'file',
    backend: identity.backend,
    outputSessionId: identity.outputSessionId,
    canonicalPath: snapshotPath(ownership.canonicalPath),
    ownedFileIdentity: requirePart(ownership.ownedFileIdentity, 'owned file'),
    source: snapshotSource(file.source),
    exactSize: file.exactSize,
    durableRanges,
    committed,
    ...(file.modifiedTimeMilliseconds === undefined
      ? {}
      : { modifiedTimeMilliseconds: file.modifiedTimeMilliseconds }),
  } as const
  return Object.freeze({ ...record, checksum: recordChecksum(record) })
}

export function directoryRecord(
  identity: OutputSessionIdentity,
  path: readonly string[],
  ownedDirectoryIdentity: string,
  createdBySession: boolean,
  modifiedTimeMilliseconds: bigint | undefined,
  finalized: boolean,
  generation: bigint,
): PersistedDirectoryRecord {
  requireGeneration(generation)
  const record = {
    journalSchema: OUTPUT_JOURNAL_SCHEMA,
    generation,
    kind: 'directory',
    backend: requirePart(identity.backend, 'backend'),
    outputSessionId: requirePart(identity.outputSessionId, 'output session'),
    canonicalPath: snapshotPath(path),
    ownedDirectoryIdentity: requirePart(ownedDirectoryIdentity, 'owned directory'),
    createdBySession,
    ...(modifiedTimeMilliseconds === undefined
      ? {}
      : { modifiedTimeMilliseconds }),
    finalized,
  } as const
  return Object.freeze({ ...record, checksum: recordChecksum(record) })
}

export function snapshotOutputRecord(record: PersistedOutputRecord): PersistedOutputRecord {
  if (record.journalSchema !== OUTPUT_JOURNAL_SCHEMA) {
    throw new TypeError('persisted output journal schema is unsupported')
  }
  requireGeneration(record.generation)
  const expectedChecksum = recordChecksum(record)
  if (record.checksum !== expectedChecksum) {
    throw new TypeError('persisted output journal checksum is invalid')
  }
  if (record.kind === 'file') {
    return fileRecord(
      record,
      record,
      {
        source: record.source,
        path: record.canonicalPath,
        exactSize: record.exactSize,
        ...(record.modifiedTimeMilliseconds === undefined
          ? {}
          : { modifiedTimeMilliseconds: record.modifiedTimeMilliseconds }),
      },
      record.durableRanges,
      record.committed,
      record.generation,
    )
  }
  return directoryRecord(
    record,
    record.canonicalPath,
    record.ownedDirectoryIdentity,
    record.createdBySession,
    record.modifiedTimeMilliseconds,
    record.finalized,
    record.generation,
  )
}

export function sameOutputRecord(
  left: PersistedOutputRecord,
  right: PersistedOutputRecord,
): boolean {
  return left.checksum === right.checksum &&
    left.kind === right.kind &&
    left.generation === right.generation &&
    outputRecordKey(left) === outputRecordKey(right)
}

export function recordBelongsToSession(
  record: PersistedOutputRecord,
  identity: OutputSessionIdentity,
): boolean {
  return record.backend === identity.backend &&
    record.outputSessionId === identity.outputSessionId
}

function requireSessionBinding(
  identity: OutputSessionIdentity,
  ownership: OutputFileOwnership,
): void {
  if (ownership.backend !== identity.backend ||
      ownership.outputSessionId !== identity.outputSessionId) {
    throw new TypeError('output ownership belongs to another session')
  }
}

function snapshotSource(source: OutputSourceIdentity): OutputSourceIdentity {
  return Object.freeze({
    shareInstance: requirePart(source.shareInstance, 'share instance'),
    fileId: requirePart(source.fileId, 'file'),
    fileRevision: requirePart(source.fileRevision, 'file revision'),
  })
}

function snapshotPath(path: readonly string[]): readonly string[] {
  return snapshotOutputPath(path)
}

function requirePart(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${label} identity must not be empty`)
  }
  return value
}

function requireGeneration(generation: bigint): void {
  if (typeof generation !== 'bigint' || generation <= 0n) {
    throw new TypeError('persisted output generation must be positive')
  }
}

/** The checksum detects accidental local corruption; same-account tampering is out of scope. */
type UnsealedOutputRecord =
  | Omit<PersistedFileRecord, 'checksum'>
  | Omit<PersistedDirectoryRecord, 'checksum'>

function recordChecksum(record: UnsealedOutputRecord | PersistedOutputRecord): string {
  const payload = record.kind === 'file'
    ? [
        'file',
        String(record.journalSchema),
        record.generation.toString(),
        record.backend,
        record.outputSessionId,
        [...record.canonicalPath],
        record.ownedFileIdentity,
        [record.source.shareInstance, record.source.fileId, record.source.fileRevision],
        record.exactSize.toString(),
        record.durableRanges.map((range) => [range.start.toString(), range.end.toString()]),
        record.committed,
        record.modifiedTimeMilliseconds?.toString() ?? null,
      ]
    : [
        'directory',
        String(record.journalSchema),
        record.generation.toString(),
        record.backend,
        record.outputSessionId,
        [...record.canonicalPath],
        record.ownedDirectoryIdentity,
        record.createdBySession,
        record.modifiedTimeMilliseconds?.toString() ?? null,
        record.finalized,
      ]
  return fnv1a64(JSON.stringify(payload))
}

function fnv1a64(value: string): string {
  const offsetBasis = 14_695_981_039_346_656_037n
  const prime = 1_099_511_628_211n
  const mask = (1n << 64n) - 1n
  let hash = offsetBasis
  for (const byte of new TextEncoder().encode(value)) {
    hash ^= BigInt(byte)
    hash = (hash * prime) & mask
  }
  return hash.toString(16).padStart(16, '0')
}

function compareRecordKeys(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
