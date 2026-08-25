import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_PAUSED,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  checkpointIdentityEqual,
  sameCheckpointLineageSpec,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  type CheckpointLineageDecision,
  type CheckpointNamespaceBinding,
  type CreatedFileCheckpointCommit,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type InitialCheckpointCASResult,
  type OwnedFileRestart,
  type OwnedFileRestartResult,
  type PersistentHandleRecord,
} from '../../persistence/journal'
import { MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER } from '../../materialization-ledger/model'
import {
  INDEXEDDB_BY_OPERATION_FILE_RECORD_INDEX,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_LINEAGE_INDEX,
  INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX,
  INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX,
  INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX,
  INDEXEDDB_BY_OPERATION_RECORD_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_FILE_FINAL_PROOF_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
  requestResult,
} from '../indexeddb-database'
import {
  abortConcurrency,
  abortIntegrity,
  checkpointLineageAuthorityForCandidate,
  checkpointLineageSpec,
  classifyCheckpointInventory,
  readStoredCheckpoint,
  reconcilePhysicalCheckpoints,
  storedCheckpoint,
  validateCheckpointHandle,
  type CheckpointLineageAuthority,
} from './repository-transactions'

export async function classifyIndexedCheckpointLineages(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  authorities: readonly CheckpointLineageAuthority[],
): Promise<readonly CheckpointLineageDecision[]> {
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const lineageRows = await Promise.all(authorities.map(async (authority) => Promise.all([
    requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX).getAll(
      IDBKeyRange.only([binding.operationId, authority.lineageId]),
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
    requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX).getAll(
      IDBKeyRange.only([binding.operationId, authority.lineageId]),
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
  ])))

  return Object.freeze(await Promise.all(lineageRows.map(async (rows, index) => {
    const authority = authorities[index]!
    const lineageRecords = reconcilePhysicalCheckpoints(rows[0], rows[1], binding, true)
      .filter((record) => sameCheckpointLineageSpec(checkpointLineageSpec(record), authority.spec))
    const ownershipRows = await Promise.all([...new Set(
      lineageRecords.map(record => record.ownedObjectId),
    )].flatMap(ownedObjectId => [
      requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
        IDBKeyRange.only([binding.operationId, ownedObjectId]),
        2,
      )),
      requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
        IDBKeyRange.only([binding.operationId, ownedObjectId]),
        2,
      )),
    ]))
    const operationRecords = reconcilePhysicalCheckpoints(
      ownershipRows.filter((_, rowIndex) => rowIndex % 2 === 0).flat(),
      ownershipRows.filter((_, rowIndex) => rowIndex % 2 === 1).flat(),
      binding,
      false,
    )
    const lineageIds = new Set(lineageRecords.map(record => record.recordId))
    return classifyCheckpointInventory(authority, Object.freeze({
      lineageRecords: Object.freeze(lineageRecords),
      operationRecords,
      physicalRecordIds: new Set(operationRecords.map(record => record.recordId)),
      crossLineageOwnershipConflict: operationRecords.some(record => !lineageIds.has(record.recordId)),
    }))
  })))
}

export async function installIndexedInitialClaims(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  candidatesInput: readonly FileCheckpointV2[],
): Promise<readonly InitialCheckpointCASResult[]> {
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const authorities = candidatesInput.map(candidate =>
    checkpointLineageAuthorityForCandidate(binding, candidate))
  const duplicateLineages = new Set(authorities.map(authority => authority.lineageId)).size !==
    authorities.length
  const duplicateObjects = new Set(candidatesInput.map(candidate => candidate.ownedObjectId)).size !==
    candidatesInput.length
  if (duplicateLineages || duplicateObjects) {
    abortIntegrity(transaction, 'initial checkpoint batch repeats a lineage or owned object')
  }

  // Candidate-specific indexes make every classification request independent of
  // operation size while preserving claim-before-create in one transaction.
  const [operationCandidateCount, operationCommittedCount, observations] = await Promise.all([
    requestResult<number>(candidates.index(INDEXEDDB_BY_OPERATION_INDEX).count(
      IDBKeyRange.only(binding.operationId),
    )),
    requestResult<number>(committed.index(INDEXEDDB_BY_OPERATION_INDEX).count(
      IDBKeyRange.only(binding.operationId),
    )),
    Promise.all(authorities.map((authority, index) => {
      const candidate = candidatesInput[index]!
      return Promise.all([
        requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX).getAll(
          IDBKeyRange.only([binding.operationId, authority.lineageId]),
          MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
        )),
        requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX).getAll(
          IDBKeyRange.only([binding.operationId, authority.lineageId]),
          MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
        )),
        requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
          IDBKeyRange.only([binding.operationId, candidate.ownedObjectId]),
          2,
        )),
        requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
          IDBKeyRange.only([binding.operationId, candidate.ownedObjectId]),
          2,
        )),
        requestResult<unknown>(candidates.get(candidate.recordId)),
        requestResult<unknown>(committed.get(candidate.recordId)),
      ])
    })),
  ])
  if (Math.max(operationCandidateCount, operationCommittedCount) + candidatesInput.length >
      MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
    abortConcurrency(transaction, 'checkpoint operation record bound exceeded')
  }

  const results = observations.map((rows, index): InitialCheckpointCASResult => {
    const candidate = candidatesInput[index]!
    const authority = authorities[index]!
    const lineageRecords = reconcilePhysicalCheckpoints(rows[0], rows[1], binding, true)
    const ownershipRecords = reconcilePhysicalCheckpoints(rows[2], rows[3], binding, false)
    const recordCollision = [rows[4], rows[5]].some(value => value !== undefined) &&
      !lineageRecords.some(record => record.recordId === candidate.recordId)
    if (recordCollision) {
      abortIntegrity(transaction, 'initial checkpoint RecordID collides outside its lineage')
    }
    const lineageIds = new Set(lineageRecords.map(record => record.recordId))
    const decision = classifyCheckpointInventory(authority, Object.freeze({
      lineageRecords,
      operationRecords: ownershipRecords,
      physicalRecordIds: new Set(ownershipRecords.map(record => record.recordId)),
      crossLineageOwnershipConflict: ownershipRecords.some(record => !lineageIds.has(record.recordId)),
    }))
    if (decision.kind !== 'absent') return decision
    candidates.put(storedCheckpoint(candidate))
    return Object.freeze({ kind: 'installed', lineageId: authority.lineageId, record: candidate })
  })
  return Object.freeze(results)
}

export async function commitCreatedFileTransaction(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  input: CreatedFileCheckpointCommit,
): Promise<void> {
  const candidate = input.candidate
  const committedCheckpoint = input.committed
  const handle = validateCheckpointHandle(input.handle)
  if (!checkpointMatchesNamespace(candidate, binding) ||
      !checkpointMatchesNamespace(committedCheckpoint, binding) ||
      candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      committedCheckpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      handle.operationId !== binding.operationId || handle.authorityRef !== binding.authorityRef ||
      handle.ownedObjectId !== committedCheckpoint.ownedObjectId) {
    abortIntegrity(transaction, 'created-file commit escaped its checkpoint or handle authority')
  }
  validateFileCheckpointTransition(candidate, committedCheckpoint)
  const candidateStore = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committedStore = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const handles = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
  const [candidateValue, committedValue, handleValue] = await Promise.all([
    requestResult<unknown>(candidateStore.get(candidate.recordId)),
    requestResult<unknown>(committedStore.get(candidate.recordId)),
    requestResult<unknown>(handles.get(handle.id)),
  ])
  if (candidateValue === undefined && committedValue !== undefined && handleValue !== undefined) {
    const current = readStoredCheckpoint(committedValue)
    const currentHandle = validateCheckpointHandle(handleValue as PersistentHandleRecord)
    if (current.checksum === committedCheckpoint.checksum && sameHandleAuthority(currentHandle, handle)) return
  }
  if (candidateValue === undefined || committedValue !== undefined || handleValue !== undefined ||
      readStoredCheckpoint(candidateValue).checksum !== candidate.checksum) {
    abortConcurrency(transaction, 'created-file checkpoint/handle predecessor changed')
  }
  handles.add(handle)
  committedStore.add(storedCheckpoint(committedCheckpoint))
  candidateStore.delete(candidate.recordId)
}

export async function commitDurableCheckpointCas(
  transaction: IDBTransaction,
  expected: FileCheckpointV2,
  next: FileCheckpointV2,
): Promise<void> {
  if (expected.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      next.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
    abortIntegrity(transaction, 'durable checkpoint CAS requires verified records')
  }
  validateFileCheckpointTransition(expected, next)
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const [candidateValue, committedValue] = await Promise.all([
    requestResult<unknown>(candidates.get(expected.recordId)),
    requestResult<unknown>(committed.get(expected.recordId)),
  ])
  if (candidateValue !== undefined || committedValue === undefined) {
    abortConcurrency(transaction, 'durable checkpoint CAS has an unresolved or missing predecessor')
  }
  const current = readStoredCheckpoint(committedValue)
  if (current.checksum === next.checksum) return
  if (current.checksum !== expected.checksum) {
    abortConcurrency(transaction, 'durable checkpoint predecessor changed')
  }
  committed.put(storedCheckpoint(next))
}

export async function restartOwnedFileTransaction(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  input: OwnedFileRestart,
  pathKey: string,
): Promise<OwnedFileRestartResult> {
  const expectedHandle = validateOwnedFileRestart(binding, input)
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const handles = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
  const proofs = transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE)
  const entries = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE)
  const [candidateValue, committedValue, handleValues, proofValue, entryValue] = await Promise.all([
    requestResult<unknown>(candidates.get(input.previous.recordId)),
    requestResult<unknown>(committed.get(input.previous.recordId)),
    requestResult<unknown[]>(handles.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
      IDBKeyRange.only([binding.operationId, input.previous.ownedObjectId]),
      2,
    )),
    requestResult<unknown>(proofs.index(INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX).get(
      [binding.operationId, input.previous.recordId],
    )),
    requestResult<unknown>(entries.index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX).get([
      binding.operationId,
      pathKey,
      MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    ])),
  ])
  if (candidateValue !== undefined || proofValue !== undefined || entryValue !== undefined) {
    abortConcurrency(transaction, 'owned-file restart conflicts with candidate or final authority')
  }
  const storedHandle = exactOwnedHandle(transaction, handleValues)
  if (!sameHandleAuthority(storedHandle, expectedHandle)) {
    abortConcurrency(transaction, 'owned-file restart handle authority changed')
  }
  if (committedValue === undefined) {
    abortConcurrency(transaction, 'owned-file restart has no durable predecessor')
  }
  const current = readStoredCheckpoint(committedValue)
  if (current.checksum === input.reset.checksum) return 'idempotent'
  if (current.checksum !== input.previous.checksum) {
    abortConcurrency(transaction, 'owned-file restart durable predecessor changed')
  }
  committed.put(storedCheckpoint(input.reset))
  return 'restart'
}

export async function scanStoredCheckpointPage(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  storeName: string,
  scan: FileCheckpointScan,
  limit: number,
): Promise<FileCheckpointPage> {
  const store = transaction.objectStore(storeName)
  const prefix: readonly IDBValidKey[] = scan.fileId === undefined
    ? [binding.operationId]
    : [binding.operationId, scan.fileId]
  const indexName = scan.fileId === undefined
    ? INDEXEDDB_BY_OPERATION_RECORD_INDEX
    : INDEXEDDB_BY_OPERATION_FILE_RECORD_INDEX
  const records = await cursorValues(
    store.index(indexName),
    compoundPrefixRange(prefix, scan.cursor, scan.direction),
    scan.direction === 'ascending' ? 'next' : 'prev',
    limit,
  )
  const decoded = records.map(readStoredCheckpoint)
  for (const record of decoded) {
    if (!checkpointMatchesNamespace(record, binding)) {
      throw new TypeError('checkpoint cursor escaped its repository binding')
    }
  }
  const nextCursor = decoded.length === limit ? decoded.at(-1)?.recordId : undefined
  return Object.freeze({
    records: Object.freeze(decoded),
    ...(nextCursor === undefined ? {} : { nextCursor }),
  })
}

function sameHandleAuthority(
  left: PersistentHandleRecord,
  right: PersistentHandleRecord,
): boolean {
  return left.id === right.id && left.operationId === right.operationId &&
    left.kind === right.kind && left.authorityRef === right.authorityRef &&
    left.ownedObjectId === right.ownedObjectId
}

function validateOwnedFileRestart(
  binding: CheckpointNamespaceBinding,
  input: OwnedFileRestart,
): PersistentHandleRecord {
  validateFileCheckpoint(input.previous)
  validateFileCheckpoint(input.reset)
  const expectedHandle = validateCheckpointHandle(input.expectedHandle)
  if (!checkpointMatchesNamespace(input.previous, binding) ||
      !checkpointMatchesNamespace(input.reset, binding) ||
      !checkpointIdentityEqual(input.previous, input.reset)) {
    throw new TypeError('owned-file restart escaped its immutable checkpoint authority')
  }
  if (input.previous.phase !== FILE_CHECKPOINT_PHASE_PAUSED ||
      input.previous.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      input.reset.phase !== FILE_CHECKPOINT_PHASE_PAUSED ||
      input.reset.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      input.reset.verifiedRanges.length !== 0 ||
      input.reset.stateGeneration !== input.previous.stateGeneration + 1n ||
      input.reset.checkpointGeneration !== input.previous.checkpointGeneration + 1n) {
    throw new TypeError('owned-file restart requires an exact paused-to-zero generation cut')
  }
  if (expectedHandle.operationId !== binding.operationId ||
      expectedHandle.authorityRef !== binding.authorityRef ||
      expectedHandle.ownedObjectId !== input.previous.ownedObjectId) {
    throw new TypeError('owned-file restart expected handle escaped its authority')
  }
  return expectedHandle
}

function exactOwnedHandle(
  transaction: IDBTransaction,
  values: readonly unknown[],
): PersistentHandleRecord {
  if (values.length !== 1) {
    abortConcurrency(transaction, 'owned-file restart requires one exact persisted handle')
  }
  return validateCheckpointHandle(values[0] as PersistentHandleRecord)
}

function compoundPrefixRange(
  prefix: readonly IDBValidKey[],
  cursor: string | undefined,
  direction: FileCheckpointScan['direction'],
): IDBKeyRange {
  const lower: IDBValidKey = [...prefix]
  const upper: IDBValidKey = [...prefix, []]
  if (cursor === undefined) return IDBKeyRange.bound(lower, upper)
  const continuation: IDBValidKey = [...prefix, cursor]
  return direction === 'ascending'
    ? IDBKeyRange.bound(continuation, upper, true, false)
    : IDBKeyRange.bound(lower, continuation, false, true)
}

function cursorValues(
  index: IDBIndex,
  range: IDBKeyRange,
  direction: IDBCursorDirection,
  limit: number,
): Promise<readonly unknown[]> {
  return new Promise((resolve, reject) => {
    const values: unknown[] = []
    const request = index.openCursor(range, direction)
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || values.length === limit) {
        resolve(Object.freeze(values))
        return
      }
      values.push(cursor.value)
      if (values.length === limit) resolve(Object.freeze(values))
      else cursor.continue()
    })
  })
}
