import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type FileCheckpointJournal,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type InitialCheckpointCASResult,
  type PersistentHandleRecord,
  type PersistentHandleInventoryRepository,
  type PersistentHandleRepository,
} from '../persistence/journal'
import {
  operationRecordId,
  receiveOperationLeaseId,
  decodeStoredWorkspaceActivationCandidate,
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_WORKSPACE_ACTIVATION,
  validateManifestPageRecord,
  validateReceiveOperationHandleRecord,
  validateReceiveOperationLeaseRecord,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
  type WorkspaceActivationCandidateV1,
} from '../workspace/records'
import { snapshotIdentity } from '../workspace/canonical'
import { RECEIVE_STATE_INTENT_FROZEN } from '../workspace/state'
import { decodeStoredReceiveLifecycleState } from '../workspace/state-codec'
import type { CompatibleNameOperationBootstrapV1 } from '../file-system-access/compatible-name/model'
import type { FSACompatibleNameBootstrapRepository } from './indexeddb-root-binding'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationRepository,
  type ReceiveOperationHandleInventoryRepository,
  type ReceiveOperationTransition,
  type WorkspaceActivationJournalRepository,
} from '../workspace/repository'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_OPERATION_FILE_INDEX,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_KIND_INDEX,
  INDEXEDDB_BY_KIND_INDEX,
  INDEXEDDB_BY_STATE_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
  INDEXEDDB_RECEIVE_RECORD_STORE,
  isIndexedDbRecord,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'
import {
  applyCompatibleNameBootstrapTransaction,
  assertCompatibleNameBootstrapTransaction,
  assertCompatibleNameBootstrapTransition,
} from './indexeddb-compatible-name-ledger'
import {
  applyOperationTransition,
  abortConcurrency,
  abortIntegrity,
  assertAuxiliaryCapacity,
  assertCheckpointInstallCapacity,
  assertOperationConcurrency,
  assertOperationMutationOwnership,
  checkpointLineageAuthority,
  checkpointLineageAuthorityForCandidate,
  classifyCheckpointInventory,
  compareRecordIds,
  readCheckpointInventory,
  readStoredCheckpoint,
  resolveStoredCheckpointCandidate,
  stageStoredCheckpointUpdate,
  storedCheckpoint,
  validateCheckpointHandle,
  validateStoredReceiveRecord,
} from './indexeddb/repository-transactions'

export { IndexedDbOperationConcurrencyError } from './indexeddb/repository-transactions'

export type IndexedDbCandidateResolution =
  | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
  | Readonly<{ kind: 'quarantined'; checkpoint: FileCheckpointV2 }>

const RECEIVE_OPERATION_RECORD_BOUND = 1_048_576
const RECEIVE_OPERATION_PAGE_BOUND = 1_048_576
const RECEIVE_OPERATION_HANDLE_BOUND = 1_048_576
const WORKSPACE_ACTIVATION_CANDIDATE_BOUND = 4_096

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

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    this.#assertOpen()
    const authority = checkpointLineageAuthority(this.binding, request)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readonly')
    try {
      const inventory = await readCheckpointInventory(transaction, this.binding, authority)
      const decision = classifyCheckpointInventory(authority, inventory)
      await transactionCompletion(transaction)
      return decision
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async createInitialCheckpoint(
    candidate: FileCheckpointV2,
  ): Promise<InitialCheckpointCASResult> {
    this.#assertOpen()
    this.#assertBinding(candidate)
    const authority = checkpointLineageAuthorityForCandidate(this.binding, candidate)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      const inventory = await readCheckpointInventory(transaction, this.binding, authority)
      const decision = classifyCheckpointInventory(authority, inventory)
      if (decision.kind !== 'absent') {
        await transactionCompletion(transaction)
        return decision
      }
      assertCheckpointInstallCapacity(inventory, candidate)
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
        .put(storedCheckpoint(candidate))
      await transactionCompletion(transaction)
      return Object.freeze({
        kind: 'installed',
        lineageId: authority.lineageId,
        record: candidate,
      })
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async resolveCandidate(
    candidate: FileCheckpointV2,
    observation: IndexedDbCandidateResolution,
  ): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(candidate)
    const resolved = observation.kind === 'verified'
      ? observation.committed
      : observation.checkpoint
    this.#assertBinding(resolved)
    const expectedCommitState = observation.kind === 'verified'
      ? FILE_CHECKPOINT_COMMIT_VERIFIED
      : FILE_CHECKPOINT_COMMIT_QUARANTINED
    if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        resolved.recordId !== candidate.recordId ||
        resolved.commitState !== expectedCommitState) {
      throw new TypeError('checkpoint candidate observation is not a valid resolution')
    }
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      await resolveStoredCheckpointCandidate(transaction, candidate, resolved)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async stageCheckpointUpdate(
    previous: FileCheckpointV2,
    candidate: FileCheckpointV2,
  ): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(previous)
    this.#assertBinding(candidate)
    if (previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('checkpoint update requires a committed predecessor and candidate successor')
    }
    validateFileCheckpointTransition(previous, candidate)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      await stageStoredCheckpointUpdate(transaction, previous, candidate)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async commitCheckpointCandidate(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2,
  ): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(candidate)
    this.#assertBinding(committed)
    if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        committed.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('checkpoint promotion requires an exact candidate and committed successor')
    }
    validateFileCheckpointTransition(candidate, committed)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      await resolveStoredCheckpointCandidate(transaction, candidate, committed)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
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

function abortQuietly(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // A completed/aborted transaction already preserved the required no-write outcome.
  }
}

export class IndexedDbReceiveOperationRepository
implements ReceiveOperationRepository,
  ReceiveOperationHandleInventoryRepository,
  WorkspaceActivationJournalRepository,
  FSACompatibleNameBootstrapRepository {
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

  async commitFSACompatibleNameBootstrap(input: Readonly<{
    transition: ReceiveOperationTransition
    bootstrap: CompatibleNameOperationBootstrapV1
  }>): Promise<void> {
    this.#assertOpen()
    const prepared = await prepareReceiveOperationTransition(input.transition)
    const bootstrap = assertCompatibleNameBootstrapTransition(prepared, input.bootstrap)
    const transaction = this.#database.transaction([
      INDEXEDDB_RECEIVE_RECORD_STORE,
      INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      await assertOperationConcurrency(transaction, prepared)
      await assertOperationMutationOwnership(transaction, prepared)
      const insertBootstrap = await assertCompatibleNameBootstrapTransaction(
        transaction,
        bootstrap,
      )
      applyOperationTransition(transaction, prepared)
      if (insertBootstrap) applyCompatibleNameBootstrapTransaction(transaction, bootstrap)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
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

  async listWorkspaceActivationCandidates(): Promise<readonly WorkspaceActivationCandidateV1[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(INDEXEDDB_RECEIVE_RECORD_STORE, 'readonly')
    const values = await requestResult<unknown[]>(
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
        .index(INDEXEDDB_BY_KIND_INDEX)
        .getAll(
          IDBKeyRange.only(RECEIVE_RECORD_WORKSPACE_ACTIVATION),
          WORKSPACE_ACTIVATION_CANDIDATE_BOUND + 1,
        ),
    )
    await transactionCompletion(transaction)
    if (values.length > WORKSPACE_ACTIVATION_CANDIDATE_BOUND) {
      throw new DOMException('Workspace activation inventory exceeds its bound', 'QuotaExceededError')
    }
    return Object.freeze(await Promise.all(values.map(async (value) =>
      decodeStoredWorkspaceActivationCandidate(await validateStoredReceiveRecord(value)))))
  }

  async listInitialWorkspaceActivationOperationIds(): Promise<readonly string[]> {
    this.#assertOpen()
    const transaction = this.#database.transaction(INDEXEDDB_RECEIVE_RECORD_STORE, 'readonly')
    const values = await requestResult<unknown[]>(
      transaction.objectStore(INDEXEDDB_RECEIVE_RECORD_STORE)
        .index(INDEXEDDB_BY_STATE_INDEX)
        .getAll(
          IDBKeyRange.only(RECEIVE_STATE_INTENT_FROZEN),
          WORKSPACE_ACTIVATION_CANDIDATE_BOUND + 1,
        ),
    )
    await transactionCompletion(transaction)
    if (values.length > WORKSPACE_ACTIVATION_CANDIDATE_BOUND) {
      throw new DOMException('Initial workspace activation inventory exceeds its bound', 'QuotaExceededError')
    }
    const operationIds = await Promise.all(values.map(async (value) => {
      const record = await validateStoredReceiveRecord(value)
      const lifecycle = decodeStoredReceiveLifecycleState(record)
      if (lifecycle.kind !== 'intent-frozen') {
        throw new TypeError('Initial workspace activation index disagrees with lifecycle authority')
      }
      return lifecycle.operationId
    }))
    if (new Set(operationIds).size !== operationIds.length) {
      throw new TypeError('Initial workspace activation inventory repeats an operation')
    }
    return Object.freeze(operationIds.sort())
  }

  async readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    const canonicalOperationId = snapshotIdentity(operationId, 16, 'operation ID')
    const value = await this.#read<unknown>(
      INDEXEDDB_RECEIVE_LEASE_STORE,
      receiveOperationLeaseId(canonicalOperationId),
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

export type { PersistentHandleRecord }
