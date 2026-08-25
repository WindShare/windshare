import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  MAX_CHECKPOINT_AUXILIARY_ENTRIES_PER_OPERATION,
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
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type InitialCheckpointCASResult,
  type OwnedFileRestart,
  type OwnedFileRestartResult,
  FILE_CHECKPOINT_BATCH_REQUEST_LIMIT,
  type CreatedFileCheckpointCommit,
  type SemanticFileCheckpointJournal,
  type PersistentHandleRecord,
  type PersistentHandleInventoryRepository,
  type PersistentHandleRepository,
} from '../persistence/journal'
import { materializationLedgerPathKey } from '../materialization-ledger/codec'
import {
  type FinalFileMaterializationCommit,
  type FinalFileMaterializationCommitReceipt,
  type MaterializationLedgerJournal,
  type MaterializationLedgerPageSummaryScan,
  type MaterializationLedgerRetirementResult,
} from '../materialization-ledger/journal'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  type MaterializationDirectoryAdmittedEntryV1,
  type MaterializationDirectoryFinalizedEntryV1,
  type MaterializationFinalFileProofV1,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryPage,
  type MaterializationLedgerPageRequest,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerSealPurpose,
  type MaterializationLedgerSealV1,
} from '../materialization-ledger/model'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_FILE_FINAL_PROOF_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
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
  installIndexedInitialClaims,
  restartOwnedFileTransaction,
  scanStoredCheckpointPage,
} from './indexeddb/checkpoint-authority-transactions'
import type { IndexedDbSemanticTransactionFaults } from './indexeddb/materialization-ledger-transactions'
import { IndexedDbMaterializationLedgerParticipant } from './indexeddb/materialization-ledger-repository'
import { snapshotMaterializationRootRelativePath } from '../../transfer/job/coordinate/direct-tree'

export { IndexedDbOperationConcurrencyError } from './indexeddb/repository-transactions'

export type IndexedDbCandidateResolution =
  | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
  | Readonly<{ kind: 'quarantined'; checkpoint: FileCheckpointV2 }>

export class IndexedDbFileCheckpointRepository
implements SemanticFileCheckpointJournal,
  MaterializationLedgerJournal,
  PersistentHandleRepository,
  PersistentHandleInventoryRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #database: IDBDatabase
  readonly #ledger: IndexedDbMaterializationLedgerParticipant
  #closed = false

  private constructor(
    database: IDBDatabase,
    binding: CheckpointNamespaceBinding,
    faults?: IndexedDbSemanticTransactionFaults,
  ) {
    this.#database = database
    this.binding = binding
    this.#ledger = new IndexedDbMaterializationLedgerParticipant({
      database,
      checkpointBinding: binding,
      ...(faults === undefined ? {} : { faults }),
      assertOpen: () => this.#assertOpen(),
    })
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

  async installInitialClaims(
    candidates: readonly FileCheckpointV2[],
  ): Promise<readonly InitialCheckpointCASResult[]> {
    this.#assertBatchSize(candidates.length)
    for (const candidate of candidates) this.#assertBinding(candidate)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    ], 'readwrite')
    try {
      const results = await installIndexedInitialClaims(transaction, this.binding, candidates)
      await transactionCompletion(transaction)
      return results
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
    const pathKey = await materializationLedgerPathKey(
      snapshotMaterializationRootRelativePath(input.previous.canonicalPath),
    )
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      INDEXEDDB_FILE_FINAL_PROOF_STORE,
      INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
    ], 'readwrite')
    try {
      const result = await restartOwnedFileTransaction(transaction, this.binding, input, pathKey)
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

  commitFinalFile(input: FinalFileMaterializationCommit): Promise<FinalFileMaterializationCommitReceipt> {
    return this.#ledger.commitFinalFile(input)
  }

  appendDirectoryAdmission(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryAdmittedEntryV1,
  ): Promise<'insert' | 'idempotent'> {
    return this.#ledger.appendDirectoryAdmission(binding, entry)
  }

  appendDirectoryFinalization(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryFinalizedEntryV1,
  ): Promise<'insert' | 'idempotent'> {
    return this.#ledger.appendDirectoryFinalization(binding, entry)
  }

  scanMaterializationLedgerEntries(
    binding: MaterializationLedgerBindingV1,
    request: MaterializationLedgerPageRequest,
  ): Promise<MaterializationLedgerEntryPage> {
    return this.#ledger.scanMaterializationLedgerEntries(binding, request)
  }

  countCheckpointCandidates(binding: MaterializationLedgerBindingV1): Promise<bigint> {
    return this.#ledger.countCheckpointCandidates(binding)
  }

  persistMaterializationLedgerPage(
    binding: MaterializationLedgerBindingV1,
    page: MaterializationLedgerPageSummaryV1,
  ): Promise<'insert' | 'idempotent'> {
    return this.#ledger.persistMaterializationLedgerPage(binding, page)
  }

  scanMaterializationLedgerPages(
    binding: MaterializationLedgerBindingV1,
    sealId: string,
    afterPageOrdinal?: bigint,
  ): Promise<MaterializationLedgerPageSummaryScan> {
    return this.#ledger.scanMaterializationLedgerPages(binding, sealId, afterPageOrdinal)
  }

  persistMaterializationLedgerSeal(
    binding: MaterializationLedgerBindingV1,
    seal: MaterializationLedgerSealV1,
  ): Promise<'insert' | 'idempotent'> {
    return this.#ledger.persistMaterializationLedgerSeal(binding, seal)
  }

  sealMaterializationLedger(input: Readonly<{
    binding: MaterializationLedgerBindingV1
    sealSequence: bigint
    purpose: MaterializationLedgerSealPurpose
  }>): Promise<MaterializationLedgerSealV1> {
    return this.#ledger.sealMaterializationLedger(input)
  }

  readMaterializationFinalProof(
    binding: MaterializationLedgerBindingV1,
    proofId: string,
  ): Promise<MaterializationFinalFileProofV1 | undefined> {
    return this.#ledger.readMaterializationFinalProof(binding, proofId)
  }

  retireMaterializationLedgerBatch(
    binding: MaterializationLedgerBindingV1,
    limit: typeof MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  ): Promise<MaterializationLedgerRetirementResult> {
    return this.#ledger.retireMaterializationLedgerBatch(binding, limit)
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
