import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
  fileCheckpointIsFinalized,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointCommit,
  type FinalFileCheckpointCommitReceipt,
  type FinalFileCheckpointProof,
  type InitialCheckpointCASResult,
  type OwnedFileRestart,
  type OwnedFileRestartResult,
  FILE_CHECKPOINT_BATCH_REQUEST_LIMIT,
  type CreatedFileCheckpointCommit,
  type RecoveryMetadataRetirementResult,
  type SemanticFileCheckpointJournal,
  type PersistentHandleRecord,
  type PersistentHandleInventoryRepository,
  type PersistentHandleRepository,
} from '../persistence/journal'
import { browserDeliveryRetainsTargetMetadata } from '../browser-delivery/indexeddb/target-retirement'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BROWSER_DELIVERY_FILE_STORE,
  INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'
import {
  abortConcurrency,
  abortIntegrity,
  assertAuxiliaryCapacity,
  checkpointLineageAuthority,
  compareRecordIds,
  readStoredCheckpoint,
  resolveStoredCheckpointCandidate,
  stageStoredCheckpointUpdate,
  validateCheckpointHandle,
} from './indexeddb/repository-transactions'
import {
  classifyIndexedCheckpointLineages,
  commitCreatedFileTransaction,
  commitDurableCheckpointCas,
  commitFinalFileTransaction,
  installIndexedInitialClaims,
  restartOwnedFileTransaction,
  retireRecoveryMetadataTransaction,
  scanStoredCheckpointPage,
  type IndexedDbSemanticTransactionFaults,
} from './indexeddb/checkpoint-authority-transactions'
import { IndexedDbFileCommitParticipants, type IndexedDbFileCommitParticipant } from './indexeddb/file-commit-participants'

export { IndexedDbOperationConcurrencyError } from './indexeddb/repository-transactions'

export type IndexedDbCandidateResolution =
  | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
  | Readonly<{ kind: 'quarantined'; checkpoint: FileCheckpointV2 }>

export class IndexedDbFileCheckpointRepository
implements SemanticFileCheckpointJournal,
  PersistentHandleRepository,
  PersistentHandleInventoryRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #database: IDBDatabase
  readonly #faults: IndexedDbSemanticTransactionFaults | undefined
  readonly #fileCommits = new IndexedDbFileCommitParticipants()
  #retirementInProgress = false
  #closed = false

  private constructor(
    database: IDBDatabase,
    binding: CheckpointNamespaceBinding,
    faults?: IndexedDbSemanticTransactionFaults,
  ) {
    this.#database = database
    this.binding = binding
    this.#faults = faults
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    binding: CheckpointNamespaceBinding,
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
    faults?: IndexedDbSemanticTransactionFaults,
  ): Promise<IndexedDbFileCheckpointRepository> {
    return new IndexedDbFileCheckpointRepository(
      await openIndexedDbCheckpointDatabase(databaseName),
      binding,
      faults,
    )
  }

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    return (await this.classifyLineages([request]))[0]!
  }

  async classifyLineages(
    requests: readonly CheckpointLineageLookupRequest[],
  ): Promise<readonly CheckpointLineageDecision[]> {
    this.#assertBatchSize(requests.length)
    const authorities = requests.map(request => checkpointLineageAuthority(this.binding, request))
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readonly')
    try {
      const decisions = await classifyIndexedCheckpointLineages(
        transaction,
        this.binding,
        authorities,
      )
      await transactionCompletion(transaction)
      return decisions
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async createInitialCheckpoint(
    candidate: FileCheckpointV2,
  ): Promise<InitialCheckpointCASResult> {
    return (await this.installInitialClaims([candidate]))[0]!
  }

  enlistFileCommit(fileId: string, participant: IndexedDbFileCommitParticipant): () => void {
    this.#assertOpen()
    return this.#fileCommits.enlistFileCommit(fileId, participant)
  }

  async installInitialClaims(
    candidates: readonly FileCheckpointV2[],
  ): Promise<readonly InitialCheckpointCASResult[]> {
    this.#assertBatchSize(candidates.length)
    for (const candidate of candidates) this.#assertBinding(candidate)
    const participation = this.#fileCommits.prepare(candidates)
    const transaction = this.#database.transaction([...new Set([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      ...participation.stores,
    ])], 'readwrite')
    const completion = transactionCompletion(transaction)
    completion.catch(() => undefined)
    try {
      const results = await installIndexedInitialClaims(transaction, this.binding, candidates)
      const publish = await participation.apply(transaction, results.flatMap(result =>
        result.kind === 'installed' || result.kind === 'exact'
          ? [{ kind: 'initial-claim' as const, checkpoint: result.record }] : []))
      await completion
      publish()
      return results
    } catch (error) {
      abortQuietly(transaction)
      await completion.catch(() => undefined)
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

  async commitCreatedFile(input: CreatedFileCheckpointCommit): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(input.candidate)
    this.#assertBinding(input.committed)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    ], 'readwrite')
    try {
      await commitCreatedFileTransaction(transaction, this.binding, input)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async commitDurableCut(previous: FileCheckpointV2, durable: FileCheckpointV2): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(previous)
    this.#assertBinding(durable)
    if (previous.phase !== FILE_CHECKPOINT_PHASE_ACTIVE ||
        (durable.phase !== FILE_CHECKPOINT_PHASE_ACTIVE &&
         durable.phase !== FILE_CHECKPOINT_PHASE_PAUSED)) {
      throw new TypeError('durable cut requires an active predecessor and active or paused successor')
    }
    await this.#commitDurableCas(previous, durable)
  }

  async resumePausedCheckpoint(paused: FileCheckpointV2, active: FileCheckpointV2): Promise<void> {
    this.#assertOpen()
    this.#assertBinding(paused)
    this.#assertBinding(active)
    if (paused.phase !== FILE_CHECKPOINT_PHASE_PAUSED || active.phase !== FILE_CHECKPOINT_PHASE_ACTIVE ||
        paused.checkpointGeneration !== active.checkpointGeneration) {
      throw new TypeError('checkpoint resume requires a paused-to-active lifecycle CAS')
    }
    await this.#commitDurableCas(paused, active)
  }

  async restartOwnedFile(input: OwnedFileRestart): Promise<OwnedFileRestartResult> {
    this.#assertOpen()
    this.#assertBinding(input.previous)
    this.#assertBinding(input.reset)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    ], 'readwrite')
    try {
      const result = await restartOwnedFileTransaction(transaction, this.binding, input)
      await transactionCompletion(transaction)
      return result
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

  async commitFinalFile(input: FinalFileCheckpointCommit): Promise<FinalFileCheckpointCommitReceipt> {
    this.#assertOpen()
    this.#assertBinding(input.expectedCommittedCheckpoint)
    this.#assertBinding(input.finalCheckpoint)
    if (input.expectedCommittedCheckpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
        !fileCheckpointIsFinalized(input.finalCheckpoint) ||
        input.expectedPersistedOwnedFileIdentity !== input.finalCheckpoint.ownedObjectId) {
      throw new TypeError('final file commit requires a verified finalized checkpoint transition')
    }
    validateFileCheckpointTransition(input.expectedCommittedCheckpoint, input.finalCheckpoint)
    const participation = this.#fileCommits.prepare([input.finalCheckpoint])
    const transaction = this.#database.transaction([...new Set([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      ...participation.stores,
    ])], 'readwrite')
    const completion = transactionCompletion(transaction)
    completion.catch(() => undefined)
    try {
      // Both authorities validate their predecessors in this cut; neither needs to reread the other's queued writes.
      const [receipt, publish] = await Promise.all([
        commitFinalFileTransaction(transaction, this.binding, input, this.#faults),
        participation.apply(transaction, [{ kind: 'final-file', checkpoint: input.finalCheckpoint }]),
      ])
      await completion
      publish()
      return receipt
    } catch (error) {
      abortQuietly(transaction)
      await completion.catch(() => undefined)
      throw error
    }
  }

  async countCheckpointCandidates(): Promise<bigint> {
    this.#assertOpen()
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      'readonly',
    )
    const count = await requestResult<number>(
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
        .index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(this.binding.operationId)),
    )
    await transactionCompletion(transaction)
    return BigInt(count)
  }

  async retireRecoveryMetadataBatch(
    limit: number = RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT,
  ): Promise<RecoveryMetadataRetirementResult> {
    this.#assertOpen()
    if (!Number.isInteger(limit) || limit < 1 || limit > RECOVERY_METADATA_RETIREMENT_BATCH_LIMIT) {
      throw new TypeError('recovery metadata retirement batch exceeds its fixed limit')
    }
    const transaction = this.#database.transaction([
      INDEXEDDB_BROWSER_DELIVERY_FILE_STORE,
      INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    ], 'readwrite')
    // The caller holds terminal mutation authority for the entire bounded retirement.
    // Checking local delivery retention once avoids re-scanning delivery rows per batch.
    if (!this.#retirementInProgress && await browserDeliveryRetainsTargetMetadata(transaction, this.binding)) {
      await transactionCompletion(transaction)
      return Object.freeze({ deletedRows: 0, state: 'retained-for-local-delivery' })
    }
    const result = await retireRecoveryMetadataTransaction(transaction, this.binding, limit)
    await transactionCompletion(transaction)
    this.#retirementInProgress = result.state === 'more'
    return result
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
    const limit = scan.limit ?? 128
    const page = await scanStoredCheckpointPage(
      transaction,
      this.binding,
      storeName,
      scan,
      limit,
    )
    await transactionCompletion(transaction)
    return validateFileCheckpointPage(page, scan, this.binding)
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

  async #commitDurableCas(previous: FileCheckpointV2, next: FileCheckpointV2): Promise<void> {
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      await commitDurableCheckpointCas(transaction, previous, next)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  #assertBatchSize(length: number): void {
    this.#assertOpen()
    if (!Number.isInteger(length) || length < 1 || length > FILE_CHECKPOINT_BATCH_REQUEST_LIMIT) {
      throw new TypeError('checkpoint repository batch is outside its fixed bound')
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

export { IndexedDbReceiveOperationRepository } from './indexeddb/receive-operation-repository'

export type { PersistentHandleRecord }
