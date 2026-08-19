import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  checkpointIdentityEqual,
  classifyCheckpointLineage,
  decodeFileCheckpointV2,
  deriveCheckpointLineageID,
  encodeFileCheckpointV2,
  sameCheckpointLineageSpec,
  validateFileCheckpointTransition,
  type CheckpointLineageID,
  type CheckpointLineageSpec,
  type FileCheckpointV2,
} from '../../persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type CheckpointNamespaceBinding,
  type PersistentHandleRecord,
} from '../../persistence/journal'
import {
  operationRecordId,
  RECEIVE_RECORD_LIFECYCLE_STATE,
  validatePersistedReceiveRecord,
  validateReceiveOperationLeaseRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationLeaseRecord,
} from '../../workspace/records'
import { equalCanonicalBytes, snapshotIdentity } from '../../workspace/canonical'
import type { ReceiveLifecycleState } from '../../workspace/state'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import type { PreparedReceiveOperationTransition } from '../../workspace/repository'
import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_FILE_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  isIndexedDbRecord,
  requestResult,
} from '../indexeddb-database'

interface StoredFileCheckpoint {
  readonly id: string
  readonly operationId: string
  readonly fileId: string
  readonly envelope: Uint8Array<ArrayBuffer>
}

export interface CheckpointLineageAuthority {
  readonly lineageId: CheckpointLineageID
  readonly spec: CheckpointLineageSpec
  readonly fileId: string
  readonly canonicalPath: readonly string[]
  readonly fileRevision: string
  readonly exactSize: bigint
}

export interface IndexedDbCheckpointInventory {
  readonly lineageRecords: readonly FileCheckpointV2[]
  readonly operationRecords: readonly FileCheckpointV2[]
  readonly physicalRecordIds: ReadonlySet<string>
  readonly crossLineageOwnershipConflict: boolean
}

export class IndexedDbOperationConcurrencyError extends DOMException {
  constructor(message = 'Receive operation generation or lease changed') {
    super(message, 'InvalidStateError')
  }
}

export class IndexedDbCheckpointRecoveryRequiredError extends DOMException {
  constructor(message = 'Checkpoint candidate recovery is required before lineage lookup') {
    super(message, 'InvalidStateError')
  }
}

export function storedCheckpoint(record: FileCheckpointV2): StoredFileCheckpoint {
  return Object.freeze({
    id: record.recordId,
    operationId: record.operationId,
    fileId: record.fileId,
    envelope: encodeFileCheckpointV2(record),
  })
}

export function readStoredCheckpoint(value: unknown): FileCheckpointV2 {
  if (!isIndexedDbRecord(value) || typeof value.id !== 'string' ||
      typeof value.operationId !== 'string' || typeof value.fileId !== 'string' ||
      !(value.envelope instanceof Uint8Array)) {
    throw new TypeError('IndexedDB file checkpoint row is invalid')
  }
  const record = decodeFileCheckpointV2(value.envelope)
  if (record.recordId !== value.id || record.operationId !== value.operationId ||
      record.fileId !== value.fileId) {
    throw new TypeError('IndexedDB checkpoint projections disagree with canonical bytes')
  }
  return record
}

export async function stageStoredCheckpointUpdate(
  transaction: IDBTransaction,
  expectedCommitted: FileCheckpointV2,
  candidate: FileCheckpointV2,
): Promise<void> {
  if (expectedCommitted.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      !checkpointIdentityEqual(expectedCommitted, candidate)) {
    abortIntegrity(transaction, 'checkpoint update changed physical identity or predecessor state')
  }
  validateFileCheckpointTransition(expectedCommitted, candidate)
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const [candidateValue, committedValue] = await Promise.all([
    requestResult<unknown>(candidates.get(candidate.recordId)),
    requestResult<unknown>(committed.get(candidate.recordId)),
  ])
  if (committedValue === undefined) {
    abortConcurrency(transaction, 'checkpoint update has no committed physical predecessor')
  }
  const currentCommitted = readStoredCheckpoint(committedValue)
  if (currentCommitted.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      currentCommitted.checksum !== expectedCommitted.checksum) {
    abortConcurrency(transaction, 'checkpoint committed predecessor changed before update staging')
  }
  if (candidateValue !== undefined) {
    const currentCandidate = readStoredCheckpoint(candidateValue)
    if (currentCandidate.checksum === candidate.checksum) return
    abortConcurrency(transaction, 'another checkpoint update candidate is already staged')
  }
  candidates.put(storedCheckpoint(candidate))
}

export async function resolveStoredCheckpointCandidate(
  transaction: IDBTransaction,
  expectedCandidate: FileCheckpointV2,
  resolved: FileCheckpointV2,
): Promise<void> {
  if (expectedCandidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      resolved.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      !checkpointIdentityEqual(expectedCandidate, resolved)) {
    abortIntegrity(transaction, 'candidate resolution changed physical checkpoint identity')
  }
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const [candidateValue, committedValue] = await Promise.all([
    requestResult<unknown>(candidates.get(expectedCandidate.recordId)),
    requestResult<unknown>(committed.get(expectedCandidate.recordId)),
  ])

  if (candidateValue === undefined) {
    if (committedValue === undefined) {
      abortConcurrency(transaction, 'checkpoint candidate disappeared before resolution')
    }
    const current = readStoredCheckpoint(committedValue)
    if (current.checksum === resolved.checksum) return
    try {
      validateFileCheckpointTransition(resolved, current)
      return
    } catch {
      abortConcurrency(transaction, 'checkpoint candidate was resolved by another authority')
    }
  }

  const currentCandidate = readStoredCheckpoint(candidateValue)
  if (currentCandidate.checksum !== expectedCandidate.checksum) {
    abortConcurrency(transaction, 'checkpoint candidate changed after its materializer probe')
  }
  if (committedValue !== undefined) {
    const currentCommitted = readStoredCheckpoint(committedValue)
    validateFileCheckpointTransition(currentCommitted, currentCandidate)
    validateFileCheckpointTransition(currentCommitted, resolved)
  }
  validateFileCheckpointTransition(currentCandidate, resolved)
  committed.put(storedCheckpoint(resolved))
  candidates.delete(expectedCandidate.recordId)
}

export function checkpointLineageAuthority(
  binding: CheckpointNamespaceBinding,
  request: CheckpointLineageLookupRequest,
): CheckpointLineageAuthority {
  const spec = Object.freeze({
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    fileId: request.fileId,
    canonicalPath: request.canonicalPath,
    materializerKind: binding.materializerKind,
    authorityRef: binding.authorityRef,
  })
  const lineageId = deriveCheckpointLineageID(spec)
  if (lineageId !== request.lineageId) {
    throw new TypeError('checkpoint lineage ID disagrees with its repository-bound coordinates')
  }
  return Object.freeze({
    lineageId,
    spec,
    fileId: request.fileId,
    canonicalPath: Object.freeze([...request.canonicalPath]),
    fileRevision: request.fileRevision,
    exactSize: request.exactSize,
  })
}

export function checkpointLineageAuthorityForCandidate(
  binding: CheckpointNamespaceBinding,
  candidate: FileCheckpointV2,
): CheckpointLineageAuthority {
  if (!checkpointMatchesNamespace(candidate, binding)) {
    throw new TypeError('initial checkpoint escaped its repository binding')
  }
  assertPristineInitialCheckpoint(candidate)
  const spec = checkpointLineageSpec(candidate)
  return Object.freeze({
    lineageId: deriveCheckpointLineageID(spec),
    spec,
    fileId: candidate.fileId,
    canonicalPath: candidate.canonicalPath,
    fileRevision: candidate.fileRevision,
    exactSize: candidate.exactSize,
  })
}

export async function readCheckpointInventory(
  transaction: IDBTransaction,
  binding: CheckpointNamespaceBinding,
  authority: CheckpointLineageAuthority,
): Promise<IndexedDbCheckpointInventory> {
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const operationFile = IDBKeyRange.only([binding.operationId, authority.fileId])
  const operation = IDBKeyRange.only(binding.operationId)

  // Queue every read before yielding. IndexedDB may auto-commit as soon as its
  // request queue drains, so transaction-owned classification cannot introduce
  // an unrelated asynchronous boundary.
  const [fileCandidateValues, fileCommittedValues, operationCandidateValues,
    operationCommittedValues] = await Promise.all([
    requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_FILE_INDEX).getAll(
      operationFile,
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
    requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_FILE_INDEX).getAll(
      operationFile,
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
    requestResult<unknown[]>(candidates.index(INDEXEDDB_BY_OPERATION_INDEX).getAll(
      operation,
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
    requestResult<unknown[]>(committed.index(INDEXEDDB_BY_OPERATION_INDEX).getAll(
      operation,
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    )),
  ])

  assertBoundedCheckpointRows([
    fileCandidateValues,
    fileCommittedValues,
    operationCandidateValues,
    operationCommittedValues,
  ])
  const fileRecords = reconcilePhysicalCheckpoints(
    fileCandidateValues,
    fileCommittedValues,
    binding,
    true,
  )
  const operationRecords = reconcilePhysicalCheckpoints(
    operationCandidateValues,
    operationCommittedValues,
    binding,
    false,
  )
  const physicalRecordIds = new Set(operationRecords.map((record) => record.recordId))
  if (physicalRecordIds.size > MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
    throw new DOMException('Checkpoint operation record bound exceeded', 'QuotaExceededError')
  }

  const lineageRecords = Object.freeze(fileRecords
    .filter((record) => {
      const spec = checkpointLineageSpec(record)
      return deriveCheckpointLineageID(spec) === authority.lineageId &&
        sameCheckpointLineageSpec(spec, authority.spec)
    })
    .sort((left, right) => compareRecordIds(left.recordId, right.recordId)))
  const lineageRecordIds = new Set(lineageRecords.map((record) => record.recordId))
  const lineageObjects = new Set(lineageRecords.map((record) => record.ownedObjectId))
  const crossLineageOwnershipConflict = operationRecords.some((record) =>
    !lineageRecordIds.has(record.recordId) && lineageObjects.has(record.ownedObjectId))

  return Object.freeze({
    lineageRecords,
    operationRecords,
    physicalRecordIds,
    crossLineageOwnershipConflict,
  })
}

export function classifyCheckpointInventory(
  authority: CheckpointLineageAuthority,
  inventory: IndexedDbCheckpointInventory,
): CheckpointLineageDecision {
  const kind = classifyCheckpointLineage(
    { fileRevision: authority.fileRevision, exactSize: authority.exactSize },
    inventory.lineageRecords,
    inventory.crossLineageOwnershipConflict,
  )
  if (kind === 'absent') return Object.freeze({ kind, lineageId: authority.lineageId })
  if (kind === 'exact') {
    const record = inventory.lineageRecords[0]
    if (record === undefined || inventory.lineageRecords.length !== 1) {
      throw new TypeError('exact checkpoint lineage did not resolve to one physical authority')
    }
    return Object.freeze({ kind, lineageId: authority.lineageId, record })
  }
  return Object.freeze({
    kind,
    lineageId: authority.lineageId,
    records: inventory.lineageRecords,
  })
}

export function assertCheckpointInstallCapacity(
  inventory: IndexedDbCheckpointInventory,
  candidate: FileCheckpointV2,
): void {
  if (inventory.physicalRecordIds.has(candidate.recordId)) {
    throw new TypeError('initial checkpoint RecordID collides outside its authenticated lineage')
  }
  if (inventory.physicalRecordIds.size >= MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
    throw new DOMException('Checkpoint operation record bound exceeded', 'QuotaExceededError')
  }
  if (inventory.operationRecords.some((record) =>
    record.recordId !== candidate.recordId && record.ownedObjectId === candidate.ownedObjectId)) {
    throw new DOMException(
      'Initial checkpoint object is already claimed by another physical record',
      'InvalidStateError',
    )
  }
}

export function assertPristineInitialCheckpoint(candidate: FileCheckpointV2): void {
  if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
      candidate.phase !== FILE_CHECKPOINT_PHASE_ACTIVE ||
      candidate.stateGeneration !== 1n || candidate.checkpointGeneration !== 0n ||
      candidate.verifiedRanges.length !== 0 || candidate.quarantineReason !== 0 ||
      candidate.quarantineOrigin !== 0 || candidate.retirementReason !== 0) {
    throw new TypeError('initial checkpoint must be a pristine active candidate reservation')
  }
}

export async function assertAuxiliaryCapacity(
  store: IDBObjectStore,
  operationId: string,
): Promise<void> {
  const count = await requestResult<number>(
    store.index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(operationId)),
  )
  if (count >= MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION) {
    throw new DOMException('Checkpoint auxiliary record bound exceeded', 'QuotaExceededError')
  }
}

export async function assertOperationConcurrency(
  transaction: IDBTransaction,
  transition: PreparedReceiveOperationTransition,
): Promise<void> {
  const records = transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
  const leases = transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE)
  const [lifecycleValue, leaseValue] = await Promise.all([
    requestResult<unknown>(records.get(operationRecordId(
      transition.operationId,
      RECEIVE_RECORD_LIFECYCLE_STATE,
    ))),
    requestResult<unknown>(leases.get(
      `windshare/receive-operation/v1/${transition.operationId}/lease`,
    )),
  ])
  const lifecycle = lifecycleValue === undefined
    ? undefined
    : lifecycleAuthority(transaction, lifecycleValue, transition.operationId)
  const lease = leaseValue === undefined
    ? undefined
    : leaseAuthority(transaction, leaseValue, transition.operationId)

  if (transition.expectedLifecycleGeneration !== undefined &&
      lifecycle?.generation !== transition.expectedLifecycleGeneration) {
    abortConcurrency(transaction)
  }
  if (transition.expectedLifecycleGeneration === undefined &&
      transition.records.some((record) => record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) &&
      lifecycle !== undefined) {
    abortConcurrency(transaction, 'initial lifecycle record already exists')
  }
  if (transition.expectedLeaseId !== undefined && lease?.leaseId !== transition.expectedLeaseId) {
    abortConcurrency(transaction)
  }
  if (transition.lease?.kind === 'put' && transition.expectedLeaseId === undefined &&
      lease !== undefined) {
    abortConcurrency(transaction, 'receive operation already has a lease')
  }
}

export async function assertOperationMutationOwnership(
  transaction: IDBTransaction,
  transition: PreparedReceiveOperationTransition,
): Promise<void> {
  const records = transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
  const pages = transaction.objectStore(INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE)
  const handles = transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)

  await assertOwnedDeletions(transaction, records, transition.deleteRecordIds, transition.operationId)
  await assertOwnedDeletions(
    transaction,
    pages,
    transition.deleteManifestPageIds,
    transition.operationId,
  )
  await assertOwnedDeletions(
    transaction,
    handles,
    transition.deleteHandleIds,
    transition.operationId,
  )

  for (const record of transition.records) {
    if (record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) continue
    const existing = await requestResult<unknown>(records.get(record.id))
    if (existing === undefined) continue
    const row = ownedIndexedDbRow(transaction, existing, transition.operationId)
    if (row.digest !== record.digest ||
        !(row.canonicalBytes instanceof Uint8Array) ||
        !equalCanonicalBytes(row.canonicalBytes, record.canonicalBytes) ||
        row.reopenKey !== record.reopenKey) {
      abortIntegrity(transaction, 'immutable receive record overwrite was rejected')
    }
  }

  for (const page of transition.manifestPages) {
    const existing = await requestResult<unknown>(pages.get(page.id))
    if (existing === undefined) continue
    const row = ownedIndexedDbRow(transaction, existing, transition.operationId)
    if (row.digest !== page.digest ||
        !(row.canonicalBytes instanceof Uint8Array) ||
        !equalCanonicalBytes(row.canonicalBytes, page.canonicalBytes)) {
      abortIntegrity(transaction, 'immutable manifest page overwrite was rejected')
    }
  }

  for (const handle of transition.handles) {
    const existing = await requestResult<unknown>(handles.get(handle.id))
    if (existing !== undefined) ownedIndexedDbRow(transaction, existing, transition.operationId)
  }
}

export function applyOperationTransition(
  transaction: IDBTransaction,
  transition: PreparedReceiveOperationTransition,
): void {
  const records = transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
  const pages = transaction.objectStore(INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE)
  const handles = transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)
  const leases = transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE)
  for (const id of transition.deleteRecordIds) records.delete(id)
  for (const id of transition.deleteManifestPageIds) pages.delete(id)
  for (const id of transition.deleteHandleIds) handles.delete(id)
  for (const record of transition.records) records.put(record)
  for (const page of transition.manifestPages) pages.put(page)
  for (const handle of transition.handles) handles.put(handle)
  if (transition.lease?.kind === 'put') leases.put(transition.lease.record)
  else if (transition.lease?.kind === 'delete') {
    leases.delete(`windshare/receive-operation/v1/${transition.operationId}/lease`)
  }
}

export async function validateStoredReceiveRecord(value: unknown): Promise<PersistedReceiveRecord> {
  const record = await validatePersistedReceiveRecord(value as PersistedReceiveRecord)
  if (record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) decodeStoredReceiveLifecycleState(record)
  return record
}

export function validateCheckpointHandle(record: PersistentHandleRecord): PersistentHandleRecord {
  if (typeof record.id !== 'string' || record.id.length === 0 ||
      !Number.isInteger(record.kind) || record.kind < 1 || record.kind > 0xff) {
    throw new TypeError('checkpoint handle identity is invalid')
  }
  return Object.freeze({
    id: record.id,
    operationId: snapshotIdentity(record.operationId, 16, 'operation ID'),
    kind: record.kind,
    authorityRef: snapshotIdentity(record.authorityRef, 32, 'authority reference'),
    ownedObjectId: snapshotIdentity(record.ownedObjectId, 32, 'owned object ID'),
    handle: record.handle,
  })
}

export function compareRecordIds(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

function reconcilePhysicalCheckpoints(
  candidateValues: readonly unknown[],
  committedValues: readonly unknown[],
  binding: CheckpointNamespaceBinding,
  requireResolvedState: boolean,
): readonly FileCheckpointV2[] {
  const candidates = readPhysicalStore(candidateValues, binding, true)
  const committed = readPhysicalStore(committedValues, binding, false)
  const recordIds = new Set([...candidates.keys(), ...committed.keys()])
  const reconciled = [...recordIds].map((recordId) => reconcilePhysicalCheckpoint(
    candidates.get(recordId),
    committed.get(recordId),
    requireResolvedState,
  ))
  return Object.freeze(reconciled.sort((left, right) =>
    compareRecordIds(left.recordId, right.recordId)))
}

function reconcilePhysicalCheckpoint(
  candidate: FileCheckpointV2 | undefined,
  committed: FileCheckpointV2 | undefined,
  requireResolvedState: boolean,
): FileCheckpointV2 {
  if (candidate === undefined) {
    if (committed === undefined) throw new TypeError('checkpoint physical pair is empty')
    return committed
  }
  if (committed === undefined) {
    if (requireResolvedState && !isPristineInitialCheckpoint(candidate)) {
      throw new IndexedDbCheckpointRecoveryRequiredError()
    }
    return candidate
  }
  if (!checkpointIdentityEqual(candidate, committed)) {
    throw new TypeError('candidate and committed checkpoint identities disagree')
  }
  validateFileCheckpointTransition(committed, candidate)
  if (requireResolvedState) throw new IndexedDbCheckpointRecoveryRequiredError()
  return committed
}

function readPhysicalStore(
  values: readonly unknown[],
  binding: CheckpointNamespaceBinding,
  candidateStore: boolean,
): ReadonlyMap<string, FileCheckpointV2> {
  const records = new Map<string, FileCheckpointV2>()
  for (const value of values) {
    const record = readStoredCheckpoint(value)
    if (!checkpointMatchesNamespace(record, binding)) {
      throw new TypeError('checkpoint physical inventory escaped its repository binding')
    }
    if ((record.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) !== candidateStore) {
      throw new TypeError('checkpoint commit state disagrees with its physical store')
    }
    if (records.has(record.recordId)) {
      throw new TypeError('checkpoint physical inventory repeated a RecordID')
    }
    records.set(record.recordId, record)
  }
  return records
}

function checkpointLineageSpec(record: FileCheckpointV2): CheckpointLineageSpec {
  return Object.freeze({
    operationId: record.operationId,
    receiveIntentDigest: record.receiveIntentDigest,
    materializationBindingDigest: record.materializationBindingDigest,
    fileId: record.fileId,
    canonicalPath: record.canonicalPath,
    materializerKind: record.materializerKind,
    authorityRef: record.authorityRef,
  })
}

function isPristineInitialCheckpoint(record: FileCheckpointV2): boolean {
  return record.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE &&
    record.phase === FILE_CHECKPOINT_PHASE_ACTIVE &&
    record.stateGeneration === 1n && record.checkpointGeneration === 0n &&
    record.verifiedRanges.length === 0 && record.quarantineReason === 0 &&
    record.quarantineOrigin === 0 && record.retirementReason === 0
}

function assertBoundedCheckpointRows(groups: readonly (readonly unknown[])[]): void {
  if (groups.some((values) => values.length > MAX_CHECKPOINT_RECORDS_PER_OPERATION)) {
    throw new DOMException('Checkpoint operation inventory exceeds its bound', 'QuotaExceededError')
  }
}

async function assertOwnedDeletions(
  transaction: IDBTransaction,
  store: IDBObjectStore,
  ids: readonly string[],
  operationId: string,
): Promise<void> {
  for (const id of ids) {
    const existing = await requestResult<unknown>(store.get(id))
    if (existing !== undefined) ownedIndexedDbRow(transaction, existing, operationId)
  }
}

function ownedIndexedDbRow(
  transaction: IDBTransaction,
  value: unknown,
  operationId: string,
): Record<string, unknown> {
  if (!isIndexedDbRecord(value) || value.operationId !== operationId) {
    abortIntegrity(transaction, 'receive operation mutation escaped its ownership boundary')
  }
  return value
}

function lifecycleAuthority(
  transaction: IDBTransaction,
  value: unknown,
  operationId: string,
): ReceiveLifecycleState {
  try {
    if (!isIndexedDbRecord(value) ||
        value.kind !== RECEIVE_RECORD_LIFECYCLE_STATE ||
        !(value.canonicalBytes instanceof Uint8Array)) {
      throw new TypeError('lifecycle row is invalid')
    }
    const state = decodeStoredReceiveLifecycleState(value as unknown as PersistedReceiveRecord)
    if (state.operationId !== operationId ||
        value.operationId !== operationId ||
        value.id !== operationRecordId(operationId, RECEIVE_RECORD_LIFECYCLE_STATE)) {
      throw new TypeError('lifecycle projections disagree with canonical bytes')
    }
    return state
  } catch {
    abortIntegrity(transaction, 'stored lifecycle authority is invalid')
  }
}

function leaseAuthority(
  transaction: IDBTransaction,
  value: unknown,
  operationId: string,
): ReceiveOperationLeaseRecord {
  try {
    const lease = validateReceiveOperationLeaseRecord(value as ReceiveOperationLeaseRecord)
    if (lease.operationId !== operationId) throw new TypeError('lease operation mismatch')
    return lease
  } catch {
    abortIntegrity(transaction, 'stored lease authority is invalid')
  }
}

export function abortIntegrity(transaction: IDBTransaction, message: string): never {
  transaction.abort()
  throw new TypeError(message)
}

export function abortConcurrency(transaction: IDBTransaction, message?: string): never {
  transaction.abort()
  throw new IndexedDbOperationConcurrencyError(message)
}
