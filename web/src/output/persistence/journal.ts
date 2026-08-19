import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  fileCheckpointDigest,
  fileCheckpointIsComplete,
  validateFileCheckpoint,
  type CheckpointLineageID,
  type FileCheckpointV2,
} from './checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  sameDurableCheckpointNamespace,
  type DurableCheckpointNamespaceIdentity,
} from './namespace'

export const FILE_CHECKPOINT_PAGE_RECORD_LIMIT = 128

export type CheckpointNamespaceBinding = DurableCheckpointNamespaceIdentity

export interface FileCheckpointScan {
  readonly direction: 'ascending' | 'descending'
  readonly fileId?: string
  readonly cursor?: string
  readonly limit?: number
}

export interface FileCheckpointPage {
  readonly records: readonly FileCheckpointV2[]
  readonly nextCursor?: string
}

export interface FinalFileCheckpointProof {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly recordId: string
  readonly recordDigest: string
  readonly checkpointGeneration: bigint
  readonly fileId: string
  readonly fileRevision: string
  readonly canonicalPath: readonly string[]
  readonly exactSize: bigint
  readonly ownedObjectId: string
  readonly complete: true
}

export interface CheckpointLineageLookupRequest {
  readonly lineageId: CheckpointLineageID
  readonly fileId: string
  readonly canonicalPath: readonly string[]
  readonly fileRevision: string
  readonly exactSize: bigint
}

export type CheckpointLineageDecision =
  | Readonly<{ kind: 'absent'; lineageId: CheckpointLineageID }>
  | Readonly<{ kind: 'exact'; lineageId: CheckpointLineageID; record: FileCheckpointV2 }>
  | Readonly<{
    kind: 'revision-conflict'
    lineageId: CheckpointLineageID
    records: readonly FileCheckpointV2[]
  }>
  | Readonly<{
    kind: 'ownership-conflict'
    lineageId: CheckpointLineageID
    records: readonly FileCheckpointV2[]
  }>
  | Readonly<{
    kind: 'invalid'
    lineageId: CheckpointLineageID
    records: readonly FileCheckpointV2[]
  }>

export type InitialCheckpointCASResult =
  | Readonly<{
    kind: 'installed'
    lineageId: CheckpointLineageID
    record: FileCheckpointV2
  }>
  | Exclude<CheckpointLineageDecision, { readonly kind: 'absent' }>

export interface FileCheckpointJournal {
  readonly binding: CheckpointNamespaceBinding
  lookupLineage(request: CheckpointLineageLookupRequest): Promise<CheckpointLineageDecision>
  /** Sole initial authority install; classification and persistence are one repository CAS. */
  createInitialCheckpoint(candidate: FileCheckpointV2): Promise<InitialCheckpointCASResult>
  /** Physical progress may advance only from an exact committed predecessor. */
  stageCheckpointUpdate(previous: FileCheckpointV2, candidate: FileCheckpointV2): Promise<void>
  /** Promotion consumes the exact candidate selected or staged by repository authority. */
  commitCheckpointCandidate(candidate: FileCheckpointV2, committed: FileCheckpointV2): Promise<void>
  readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined>
  scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage>
  scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage>
  finalCheckpointProof(recordId: string, generation: bigint): Promise<FinalFileCheckpointProof>
  retireOperation(): Promise<void>
}

export interface PersistentHandleRecord<T = unknown> {
  readonly id: string
  readonly operationId: string
  readonly kind: number
  readonly authorityRef: string
  readonly ownedObjectId: string
  readonly handle: T
}

export interface PersistentHandleRepository<T = unknown> {
  putHandle(record: PersistentHandleRecord<T>): Promise<void>
  readHandle(id: string): Promise<PersistentHandleRecord<T> | undefined>
  deleteHandle(id: string): Promise<void>
}

/** Cleanup inventory is a stronger capability than point lookup and ordinary mutation. */
export interface PersistentHandleInventoryRepository<T = unknown>
extends PersistentHandleRepository<T> {
  listHandles(): Promise<readonly PersistentHandleRecord<T>[]>
}

export function checkpointRecordKey(record: FileCheckpointV2): string {
  validateFileCheckpoint(record)
  return record.recordId
}

export function checkpointMatchesNamespace(
  record: FileCheckpointV2,
  binding: CheckpointNamespaceBinding,
): boolean {
  return sameDurableCheckpointNamespace(
    durableCheckpointNamespaceIdentity({
      operationId: record.operationId,
      receiveIntentDigest: record.receiveIntentDigest,
      materializationBindingDigest: record.materializationBindingDigest,
      materializerKind: record.materializerKind,
      authorityRef: record.authorityRef,
    }),
    binding,
  )
}

export function finalFileCheckpointProof(record: FileCheckpointV2): FinalFileCheckpointProof {
  validateFileCheckpoint(record)
  if (record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED || !fileCheckpointIsComplete(record)) {
    throw new TypeError('aggregate manifest requires a verified complete file checkpoint')
  }
  return Object.freeze({
    operationId: record.operationId,
    receiveIntentDigest: record.receiveIntentDigest,
    materializationBindingDigest: record.materializationBindingDigest,
    recordId: record.recordId,
    recordDigest: fileCheckpointDigest(record),
    checkpointGeneration: record.checkpointGeneration,
    fileId: record.fileId,
    fileRevision: record.fileRevision,
    canonicalPath: record.canonicalPath,
    exactSize: record.exactSize,
    ownedObjectId: record.ownedObjectId,
    complete: true,
  })
}

export function validateFileCheckpointPage(
  input: FileCheckpointPage,
  scan: FileCheckpointScan,
  binding: CheckpointNamespaceBinding,
): FileCheckpointPage {
  const limit = scan.limit ?? FILE_CHECKPOINT_PAGE_RECORD_LIMIT
  if (!Number.isInteger(limit) || limit < 1 || limit > FILE_CHECKPOINT_PAGE_RECORD_LIMIT ||
      input.records.length > limit) {
    throw new TypeError('checkpoint page exceeds its fixed record limit')
  }
  const records = input.records.map((record) => {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, binding) ||
        (scan.fileId !== undefined && record.fileId !== scan.fileId)) {
      throw new TypeError('checkpoint record escaped its operation namespace')
    }
    return record
  })
  validateCursorOrder(records, scan)
  if (input.nextCursor !== undefined &&
      input.nextCursor !== records.at(-1)?.recordId) {
    throw new TypeError('checkpoint page continuation is not its tail')
  }
  if (records.length === limit && input.nextCursor === undefined) {
    throw new TypeError('full checkpoint page omitted its continuation')
  }
  return Object.freeze({
    records: Object.freeze(records),
    ...(input.nextCursor === undefined ? {} : { nextCursor: input.nextCursor }),
  })
}

function validateCursorOrder(
  records: readonly FileCheckpointV2[],
  scan: FileCheckpointScan,
): void {
  let previous = scan.cursor
  for (const record of records) {
    if (previous !== undefined) {
      const comparison = compareText(record.recordId, previous)
      const advances = scan.direction === 'ascending' ? comparison > 0 : comparison < 0
      if (!advances) throw new TypeError('checkpoint page cursor did not advance')
    }
    previous = record.recordId
  }
}

function compareText(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
