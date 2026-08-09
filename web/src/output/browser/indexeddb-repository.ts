import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  decodeFileCheckpointV2,
  encodeFileCheckpointV2,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type FileCheckpointJournal,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type PersistentHandleRecord,
  type PersistentHandleInventoryRepository,
  type PersistentHandleRepository,
} from '../persistence/journal'
import {
  operationRecordId,
  RECEIVE_RECORD_LIFECYCLE_STATE,
  validateManifestPageRecord,
  validatePersistedReceiveRecord,
  validateReceiveOperationHandleRecord,
  validateReceiveOperationLeaseRecord,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
} from '../workspace/records'
import {
  equalCanonicalBytes,
  snapshotIdentity,
} from '../workspace/canonical'
import type { ReceiveLifecycleState } from '../workspace/state'
import { decodeStoredReceiveLifecycleState } from '../workspace/state-codec'
import {
  prepareReceiveOperationTransition,
  type PreparedReceiveOperationTransition,
  type ReceiveOperationRepository,
  type ReceiveOperationHandleInventoryRepository,
  type ReceiveOperationTransition,
} from '../workspace/repository'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_OPERATION_FILE_INDEX,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_KIND_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  isIndexedDbRecord,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'

const RECEIVE_OPERATION_RECORD_BOUND = 1_048_576
const RECEIVE_OPERATION_PAGE_BOUND = 1_048_576
const RECEIVE_OPERATION_HANDLE_BOUND = 1_048_576

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

export class IndexedDbFileCheckpointRepository
implements FileCheckpointJournal, PersistentHandleRepository, PersistentHandleInventoryRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase, binding: CheckpointNamespaceBinding) {
    this.#database = database
    this.binding = binding
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    binding: CheckpointNamespaceBinding,
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  ): Promise<IndexedDbFileCheckpointRepository> {
    return new IndexedDbFileCheckpointRepository(
      await openIndexedDbCheckpointDatabase(databaseName),
      binding,
    )
  }

  async putCandidate(record: FileCheckpointV2): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(record)
    if (record.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('candidate store accepts only candidate checkpoints')
    }
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
    const existing = await requestResult<unknown>(store.get(record.recordId))
    if (existing === undefined) await assertCheckpointCapacity(store, record.operationId)
    else validateFileCheckpointTransition(readStoredCheckpoint(existing), record)
    store.put(storedCheckpoint(record))
    await transactionCompletion(transaction)
  }

  async commit(record: FileCheckpointV2): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(record)
    if (record.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('committed checkpoint must have verified reducer state')
    }
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
    const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
    const [candidateValue, committedValue] = await Promise.all([
      requestResult<unknown>(candidates.get(record.recordId)),
      requestResult<unknown>(committed.get(record.recordId)),
    ])
    if (candidateValue === undefined && committedValue === undefined) {
      abortConcurrency(transaction, 'checkpoint commit has no candidate authority')
    }
    if (candidateValue !== undefined) {
      validateFileCheckpointTransition(readStoredCheckpoint(candidateValue), record)
    }
    if (committedValue !== undefined) {
      validateFileCheckpointTransition(readStoredCheckpoint(committedValue), record)
    } else {
      await assertCheckpointCapacity(committed, record.operationId)
    }
    committed.put(storedCheckpoint(record))
    candidates.delete(record.recordId)
    await transactionCompletion(transaction)
  }

  async readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      'readonly',
    )
    const value = await requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).get(recordId),
    )
    await transactionCompletion(transaction)
    if (value === undefined) return undefined
    const record = readStoredCheckpoint(value)
    this.#assertBinding(record)
    return record
  }

  scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, scan)
  }

  scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE, scan)
  }

  async finalCheckpointProof(
    recordId: string,
    generation: bigint,
  ): Promise<FinalFileCheckpointProof> {
    const record = await this.readCommitted(recordId)
    if (record === undefined || record.checkpointGeneration !== generation) {
      throw new DOMException('Final checkpoint generation is unavailable', 'NotFoundError')
    }
    return finalFileCheckpointProof(record)
  }

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.#assertOpen()
    const validated = validateCheckpointHandle(record)
    if (validated.operationId !== this.binding.operationId) {
      throw new TypeError('checkpoint handle escaped its operation')
    }
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
    const existing = await requestResult<unknown>(store.get(validated.id))
    if (existing === undefined) {
      await assertAuxiliaryCapacity(store, validated.operationId)
    } else {
      try {
        const authority = validateCheckpointHandle(existing as PersistentHandleRecord)
        if (authority.operationId !== this.binding.operationId) {
          throw new TypeError('handle operation mismatch')
        }
      } catch {
        abortIntegrity(transaction, 'checkpoint handle overwrite escaped its operation')
      }
    }
    store.put(validated)
    await transactionCompletion(transaction)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      'readonly',
    )
    const value = await requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE).get(id),
    )
    await transactionCompletion(transaction)
    if (value === undefined) return undefined
    const record = validateCheckpointHandle(value as PersistentHandleRecord)
    if (record.operationId !== this.binding.operationId) {
      throw new TypeError('checkpoint handle escaped its operation')
    }
    return record
  }

  async listHandles(): Promise<readonly PersistentHandleRecord[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      'readonly',
    )
    const values = await requestResult<unknown[]>(
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
        .index(INDEXEDDB_BY_OPERATION_INDEX)
        .getAll(
          IDBKeyRange.only(this.binding.operationId),
          MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION + 1,
        ),
    )
    await transactionCompletion(transaction)
    if (values.length > MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION) {
      throw new DOMException('Checkpoint handle inventory exceeds its bound', 'QuotaExceededError')
    }
    const handles = values.map((value) => validateCheckpointHandle(
      value as PersistentHandleRecord,
    ))
    if (handles.some((handle) => handle.operationId !== this.binding.operationId)) {
      throw new TypeError('checkpoint handle inventory escaped its operation')
    }
    return Object.freeze(handles.sort((left, right) => compareRecordIds(left.id, right.id)))
  }

  async deleteHandle(id: string): Promise<void> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
    const value = await requestResult<unknown>(store.get(id))
    if (value !== undefined) {
      const record = validateCheckpointHandle(value as PersistentHandleRecord)
      if (record.operationId !== this.binding.operationId) {
        abortIntegrity(transaction, 'checkpoint handle escaped its operation')
      }
      store.delete(id)
    }
    await transactionCompletion(transaction)
  }

  async retireOperation(): Promise<void> {
    this.#assertOpen()
    const stores = [
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    ] as const
    const transaction = this.#database.transaction(stores, 'readwrite')
    for (const name of stores) {
      const store = transaction.objectStore(name)
      const values = await requestResult<unknown[]>(
        store.index(INDEXEDDB_BY_OPERATION_INDEX).getAll(
          IDBKeyRange.only(this.binding.operationId),
          MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION + 1,
        ),
      )
      if (values.length > MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION) {
        abortConcurrency(transaction, 'checkpoint cleanup inventory exceeds its bound')
      }
      for (const value of values) {
        let id: string
        try {
          id = this.#retiredRecordId(name, value)
        } catch {
          abortIntegrity(transaction, 'checkpoint cleanup escaped its operation')
        }
        store.delete(id)
      }
    }
    await transactionCompletion(transaction)
  }

  close(): void {
    if (!this.#closed) {
      this.#closed = true
      this.#database.close()
    }
  }

  async #scan(
    storeName: string,
    scan: FileCheckpointScan,
  ): Promise<FileCheckpointPage> {
    this.#assertOpen()
    const transaction = this.#database.transaction(storeName, 'readonly')
    const store = transaction.objectStore(storeName)
    const query = scan.fileId === undefined
      ? IDBKeyRange.only(this.binding.operationId)
      : IDBKeyRange.only([this.binding.operationId, scan.fileId])
    const indexName = scan.fileId === undefined
      ? INDEXEDDB_BY_OPERATION_INDEX
      : INDEXEDDB_BY_OPERATION_FILE_INDEX
    const values = await requestResult<unknown[]>(store.index(indexName).getAll(
      query,
      MAX_CHECKPOINT_RECORDS_PER_OPERATION + 1,
    ))
    await transactionCompletion(transaction)
    if (values.length > MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
      throw new DOMException('Checkpoint scan exceeds its operation bound', 'QuotaExceededError')
    }
    const records = values.map(readStoredCheckpoint)
      .sort((left, right) => compareRecordIds(left.recordId, right.recordId))
    if (scan.direction === 'descending') records.reverse()
    const afterCursor = scan.cursor === undefined
      ? records
      : records.filter((record) => scan.direction === 'ascending'
        ? record.recordId > scan.cursor!
        : record.recordId < scan.cursor!)
    const limit = scan.limit ?? 128
    const pageRecords = afterCursor.slice(0, limit)
    const nextCursor = afterCursor.length >= limit ? pageRecords.at(-1)?.recordId : undefined
    return validateFileCheckpointPage({
      records: pageRecords,
      ...(nextCursor === undefined ? {} : { nextCursor }),
    }, scan, this.binding)
  }

  #retiredRecordId(storeName: string, value: unknown): string {
    if (storeName === INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE) {
      const handle = validateCheckpointHandle(value as PersistentHandleRecord)
      if (handle.operationId !== this.binding.operationId) {
        throw new TypeError('handle operation mismatch')
      }
      return handle.id
    }
    const checkpoint = readStoredCheckpoint(value)
    this.#assertBinding(checkpoint)
    return checkpoint.recordId
  }

  #assertBinding(record: FileCheckpointV2): void {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding)) {
      throw new TypeError('file checkpoint escaped its repository binding')
    }
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('File checkpoint repository is closed', 'InvalidStateError')
    }
  }
}

export class IndexedDbReceiveOperationRepository
implements ReceiveOperationRepository, ReceiveOperationHandleInventoryRepository {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  ): Promise<IndexedDbReceiveOperationRepository> {
    return new IndexedDbReceiveOperationRepository(
      await openIndexedDbCheckpointDatabase(databaseName),
    )
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    this.#assertOpen()
    const prepared = await prepareReceiveOperationTransition(transition)
    const transaction = this.#database.transaction([
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    await assertOperationConcurrency(transaction, prepared)
    await assertOperationMutationOwnership(transaction, prepared)
    applyOperationTransition(transaction, prepared)
    await transactionCompletion(transaction)
  }

  async readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    const value = await this.#read<unknown>(INDEXEDDB_RECEIVE_RECORD_STORE, id)
    return value === undefined ? undefined : validateStoredReceiveRecord(value)
  }

  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return this.readRecord(operationRecordId(operationId, RECEIVE_RECORD_LIFECYCLE_STATE))
  }

  async listRecords(
    operationId: string,
    kind?: ReceiveRecordKind,
  ): Promise<readonly PersistedReceiveRecord[]> {
    const canonicalOperationId = snapshotIdentity(operationId, 16, 'operation ID')
    const values = await this.#listByOperation<unknown>(
      INDEXEDDB_RECEIVE_RECORD_STORE,
      canonicalOperationId,
      kind,
      RECEIVE_OPERATION_RECORD_BOUND,
    )
    return Object.freeze(await Promise.all(values.map(validateStoredReceiveRecord)))
  }

  async listManifestPages(
    operationId: string,
    kind?: ReceiveRecordKind,
  ): Promise<readonly ManifestPageRecord[]> {
    const canonicalOperationId = snapshotIdentity(operationId, 16, 'operation ID')
    const values = await this.#listByOperation<unknown>(
      INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
      canonicalOperationId,
      kind,
      RECEIVE_OPERATION_PAGE_BOUND,
    )
    return Object.freeze(await Promise.all(
      values.map((value) => validateManifestPageRecord(value as ManifestPageRecord)),
    ))
  }

  async readHandle<T = unknown>(
    id: string,
  ): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    const value = await this.#read<unknown>(INDEXEDDB_RECEIVE_HANDLE_STORE, id)
    return value === undefined
      ? undefined
      : validateReceiveOperationHandleRecord(value as ReceiveOperationHandleRecord<T>)
  }

  async listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]> {
    const canonicalOperationId = snapshotIdentity(operationId, 16, 'operation ID')
    const values = await this.#listByOperation<unknown>(
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      canonicalOperationId,
      undefined,
      RECEIVE_OPERATION_HANDLE_BOUND,
    )
    const handles = values.map((value) => validateReceiveOperationHandleRecord(
      value as ReceiveOperationHandleRecord,
    ))
    return Object.freeze(handles.sort((left, right) => compareRecordIds(left.id, right.id)))
  }

  async readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    const canonicalOperationId = snapshotIdentity(operationId, 16, 'operation ID')
    const value = await this.#read<unknown>(
      INDEXEDDB_RECEIVE_LEASE_STORE,
      `windshare/receive-operation/v1/${canonicalOperationId}/lease`,
    )
    if (value === undefined) return undefined
    const record = validateReceiveOperationLeaseRecord(value as ReceiveOperationLeaseRecord)
    if (record.operationId !== canonicalOperationId) {
      throw new TypeError('receive lease escaped its operation')
    }
    return record
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async #read<T>(storeName: string, id: string): Promise<T | undefined> {
    this.#assertOpen()
    const transaction = this.#database.transaction(storeName, 'readonly')
    const value = await requestResult<T | undefined>(
      transaction.objectStore(storeName).get(id),
    )
    await transactionCompletion(transaction)
    return value
  }

  async #listByOperation<T>(
    storeName: string,
    operationId: string,
    kind: ReceiveRecordKind | undefined,
    bound: number,
  ): Promise<readonly T[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(storeName, 'readonly')
    const store = transaction.objectStore(storeName)
    const values = kind === undefined
      ? await requestResult<T[]>(store.index(INDEXEDDB_BY_OPERATION_INDEX).getAll(
          IDBKeyRange.only(operationId),
          bound + 1,
        ))
      : await requestResult<T[]>(store.index(INDEXEDDB_BY_OPERATION_KIND_INDEX).getAll(
          IDBKeyRange.only([operationId, kind]),
          bound + 1,
        ))
    await transactionCompletion(transaction)
    if (values.length > bound || values.some((value) =>
      !isIndexedDbRecord(value) || value.operationId !== operationId)) {
      throw new DOMException('Receive operation inventory exceeds its bound', 'QuotaExceededError')
    }
    return Object.freeze(values)
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Receive operation repository is closed', 'InvalidStateError')
    }
  }
}

function storedCheckpoint(record: FileCheckpointV2): StoredFileCheckpoint {
  return Object.freeze({
    id: record.recordId,
    operationId: record.operationId,
    fileId: record.fileId,
    envelope: encodeFileCheckpointV2(record),
  })
}

function readStoredCheckpoint(value: unknown): FileCheckpointV2 {
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

async function assertCheckpointCapacity(store: IDBObjectStore, operationId: string): Promise<void> {
  const count = await requestResult<number>(
    store.index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(operationId)),
  )
  if (count >= MAX_CHECKPOINT_RECORDS_PER_OPERATION) {
    throw new DOMException('Checkpoint operation record bound exceeded', 'QuotaExceededError')
  }
}

async function assertAuxiliaryCapacity(store: IDBObjectStore, operationId: string): Promise<void> {
  const count = await requestResult<number>(
    store.index(INDEXEDDB_BY_OPERATION_INDEX).count(IDBKeyRange.only(operationId)),
  )
  if (count >= MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION) {
    throw new DOMException('Checkpoint auxiliary record bound exceeded', 'QuotaExceededError')
  }
}

async function assertOperationConcurrency(
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

async function assertOperationMutationOwnership(
  transaction: IDBTransaction,
  transition: PreparedReceiveOperationTransition,
): Promise<void> {
  const records = transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
  const pages = transaction.objectStore(INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE)
  const handles = transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)

  await assertOwnedDeletions(
    transaction,
    records,
    transition.deleteRecordIds,
    transition.operationId,
  )
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
    if (existing === undefined) continue
    ownedIndexedDbRow(transaction, existing, transition.operationId)
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

function applyOperationTransition(
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

async function validateStoredReceiveRecord(value: unknown): Promise<PersistedReceiveRecord> {
  const record = await validatePersistedReceiveRecord(value as PersistedReceiveRecord)
  if (record.kind === RECEIVE_RECORD_LIFECYCLE_STATE) {
    decodeStoredReceiveLifecycleState(record)
  }
  return record
}

function validateCheckpointHandle(
  record: PersistentHandleRecord,
): PersistentHandleRecord {
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

function abortIntegrity(transaction: IDBTransaction, message: string): never {
  transaction.abort()
  throw new TypeError(message)
}

function abortConcurrency(transaction: IDBTransaction, message?: string): never {
  transaction.abort()
  throw new IndexedDbOperationConcurrencyError(message)
}

function compareRecordIds(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

export type { PersistentHandleRecord }
