import {
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  decodeFileCheckpointV2,
  encodeFileCheckpointV2,
  type FileCheckpointV2,
} from '../../persistence/checkpoint'
import type { PersistentHandleRecord } from '../../persistence/journal'
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

export class IndexedDbOperationConcurrencyError extends DOMException {
  constructor(message = 'Receive operation generation or lease changed') {
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

export async function assertCheckpointCapacity(
  store: IDBObjectStore,
  operationId: string,
): Promise<void> {
  const count = await requestResult<number>(
    store.index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(operationId)),
  )
  if (count >= MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
    throw new DOMException('Checkpoint operation record bound exceeded', 'QuotaExceededError')
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
