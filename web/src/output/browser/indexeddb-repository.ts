import type {
  CheckpointNamespaceBinding,
  OutputCheckpointJournal,
  OutputJournalPage,
  OutputJournalScan,
  PersistedOutputRecord,
} from '../persistence/journal'
import {
  OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT,
  outputRecordKey,
  recordBelongsToCheckpointNamespace,
  snapshotOutputRecord,
} from '../persistence/journal'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
} from '../persistence/checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  durableCheckpointNamespaceKey,
} from '../persistence/namespace'
import {
  INDEXEDDB_CHECKPOINT_CANDIDATE_STORE as CANDIDATE_STORE,
  INDEXEDDB_CHECKPOINT_CLEANUP_STORE as CLEANUP_STORE,
  INDEXEDDB_CHECKPOINT_COMMITTED_STORE as COMMITTED_STORE,
  INDEXEDDB_CHECKPOINT_HANDLE_STORE as HANDLE_STORE,
  INDEXEDDB_CHECKPOINT_METADATA_STORE as METADATA_STORE,
  INDEXEDDB_CHECKPOINT_NAMESPACE_INDEX as NAMESPACE_INDEX,
  INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
  INDEXEDDB_RESUME_STATE_DISCARD_STORE,
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'
import {
  sameResumeStateDiscardMarker,
  snapshotResumeStateDiscardMarker,
  storedResumeStateDiscardMarker,
  type IndexedDbResumeStateDiscardMarker,
  type IndexedDbResumeStateDiscardPhase,
} from './resume-state/records'

export {
  CHECKPOINT_DATABASE_VERSION,
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
  INDEXEDDB_RESUME_STATE_DISCARD_STORE,
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  openIndexedDbCheckpointDatabase,
} from './indexeddb-database'
export {
  resolveIndexedDbRootIdentity,
  verifyIndexedDbRootIdentity,
} from './indexeddb-root-binding'
export type {
  IndexedDbResumeStateDiscardMarker,
  IndexedDbResumeStateDiscardPhase,
} from './resume-state/records'

interface StoredRecord {
  readonly id: string
  readonly namespace: string
  readonly record: PersistedOutputRecord
}

interface StoredHandle {
  readonly id: string
  readonly namespace: string
  readonly handle: FileSystemHandle
}

interface NamespaceMetadata {
  readonly id: string
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly transferIntentDigest: string
  readonly rootIdentity: string
  readonly state: 'active' | 'cleanup-pending'
  readonly cleanupStep: number
}

interface StoredCleanupMarker {
  readonly id: string
  readonly namespace: string
  readonly target: string
  readonly step: number
}

export interface PersistentHandleRepository {
  putHandle(identity: string, handle: FileSystemHandle): Promise<void>
  getHandle(identity: string): Promise<FileSystemHandle | undefined>
  deleteHandle(identity: string): Promise<void>
}

/** A bounded, namespace-owned IndexedDB adapter for FileCheckpointV1. */
export class IndexedDbOutputRepository implements OutputCheckpointJournal, PersistentHandleRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #database: IDBDatabase
  readonly #namespace: string
  #closed = false
  #cleanupPromise: Promise<IndexedDbCleanupReport> | undefined

  private constructor(
    database: IDBDatabase,
    binding: CheckpointNamespaceBinding,
  ) {
    this.#database = database
    this.binding = durableCheckpointNamespaceIdentity(binding)
    this.#namespace = durableCheckpointNamespaceKey(this.binding)
    database.addEventListener('versionchange', () => {
      this.#closed = true
      database.close()
    })
  }

  static async open(
    databaseName: string,
    binding: CheckpointNamespaceBinding,
  ): Promise<IndexedDbOutputRepository> {
    return IndexedDbOutputRepository.#open(databaseName, binding, true)
  }

  static async openExisting(
    databaseName: string,
    binding: CheckpointNamespaceBinding,
  ): Promise<IndexedDbOutputRepository> {
    return IndexedDbOutputRepository.#open(databaseName, binding, false)
  }

  static async #open(
    databaseName: string,
    binding: CheckpointNamespaceBinding,
    initialize: boolean,
  ): Promise<IndexedDbOutputRepository> {
    if (databaseName.length === 0) throw new TypeError('IndexedDB name must not be empty')
    const database = await openIndexedDbCheckpointDatabase(databaseName)
    const repository = new IndexedDbOutputRepository(database, binding)
    try {
      if (initialize) await repository.#ensureOwnershipMetadata()
      else await repository.#requireOwnershipMetadata()
      return repository
    } catch (error) {
      repository.close()
      throw error
    }
  }

  scanCommitted(scan: OutputJournalScan): Promise<OutputJournalPage> { return this.#scan(COMMITTED_STORE, scan) }
  scanCandidates(scan: OutputJournalScan): Promise<OutputJournalPage> { return this.#scan(CANDIDATE_STORE, scan) }

  async writeCandidate(record: PersistedOutputRecord): Promise<void> {
    this.#assertRecord(record)
    const transaction = this.#transaction(CANDIDATE_STORE, 'readwrite')
    transaction.objectStore(CANDIDATE_STORE).put(this.#storedRecord(record))
    await transactionCompletion(transaction)
  }

  async flushCandidate(key: string): Promise<void> {
    const transaction = this.#transaction(CANDIDATE_STORE, 'readonly')
    const candidate = await requestResult<StoredRecord | undefined>(transaction.objectStore(CANDIDATE_STORE).get(this.#key(key)))
    await transactionCompletion(transaction)
    if (candidate === undefined) throw new Error('Output checkpoint candidate was not durably written')
    this.#assertStoredRecord(candidate)
  }

  async commitCandidate(key: string): Promise<void> {
    const transaction = this.#transaction([CANDIDATE_STORE, COMMITTED_STORE], 'readwrite')
    const candidates = transaction.objectStore(CANDIDATE_STORE)
    const candidate = await requestResult<StoredRecord | undefined>(candidates.get(this.#key(key)))
    if (candidate === undefined) {
      transaction.abort()
      throw new Error('Cannot commit a missing output checkpoint candidate')
    }
    this.#assertStoredRecord(candidate)
    transaction.objectStore(COMMITTED_STORE).put(candidate)
    candidates.delete(candidate.id)
    await transactionCompletion(transaction)
  }

  async readCommitted(key: string): Promise<PersistedOutputRecord | undefined> {
    const transaction = this.#transaction(COMMITTED_STORE, 'readonly')
    const stored = await requestResult<StoredRecord | undefined>(transaction.objectStore(COMMITTED_STORE).get(this.#key(key)))
    await transactionCompletion(transaction)
    if (stored === undefined) return undefined
    this.#assertStoredRecord(stored)
    return snapshotOutputRecord(stored.record)
  }

  async discardCandidate(key: string): Promise<void> { await this.#delete(CANDIDATE_STORE, this.#key(key)) }
  async deleteCommitted(key: string): Promise<void> { await this.#delete(COMMITTED_STORE, this.#key(key)) }

  async putHandle(identity: string, handle: FileSystemHandle): Promise<void> {
    const transaction = this.#transaction(HANDLE_STORE, 'readwrite')
    transaction.objectStore(HANDLE_STORE).put({ id: this.#key(`handle:${identity}`), namespace: this.#namespace, handle } satisfies StoredHandle)
    await transactionCompletion(transaction)
  }

  async getHandle(identity: string): Promise<FileSystemHandle | undefined> {
    const transaction = this.#transaction(HANDLE_STORE, 'readonly')
    const stored = await requestResult<StoredHandle | undefined>(transaction.objectStore(HANDLE_STORE).get(this.#key(`handle:${identity}`)))
    await transactionCompletion(transaction)
    if (stored !== undefined && stored.namespace !== this.#namespace) throw new Error('IndexedDB handle escaped its checkpoint namespace')
    return stored?.handle
  }

  async deleteHandle(identity: string): Promise<void> { await this.#delete(HANDLE_STORE, this.#key(`handle:${identity}`)) }

  async readResumeStateDiscard(): Promise<IndexedDbResumeStateDiscardMarker | undefined> {
    const transaction = this.#transaction(INDEXEDDB_RESUME_STATE_DISCARD_STORE, 'readonly')
    const stored = await requestResult<unknown>(
      transaction.objectStore(INDEXEDDB_RESUME_STATE_DISCARD_STORE).get(this.#namespace),
    )
    await transactionCompletion(transaction)
    return stored === undefined
      ? undefined
      : snapshotResumeStateDiscardMarker(stored, this.#namespace)
  }

  async beginResumeStateDiscard(
    marker: IndexedDbResumeStateDiscardMarker,
  ): Promise<IndexedDbResumeStateDiscardMarker> {
    if (marker.phase === 'exported') {
      throw new TypeError('Resume-state discard cannot begin after external publication')
    }
    const requested = snapshotResumeStateDiscardMarker({
      ...marker,
      id: this.#namespace,
      namespace: this.#namespace,
      schemaVersion: 1,
    }, this.#namespace)
    const transaction = this.#transaction(INDEXEDDB_RESUME_STATE_DISCARD_STORE, 'readwrite')
    const store = transaction.objectStore(INDEXEDDB_RESUME_STATE_DISCARD_STORE)
    const existingValue = await requestResult<unknown>(store.get(this.#namespace))
    if (existingValue === undefined) {
      store.add(storedResumeStateDiscardMarker(requested, this.#namespace))
    } else {
      const existing = snapshotResumeStateDiscardMarker(existingValue, this.#namespace)
      if (!sameResumeStateDiscardMarker(existing, requested)) {
        transaction.abort()
        throw new Error('Resume-state discard journal conflicts with this operation')
      }
    }
    await transactionCompletion(transaction)
    return requested
  }

  async advanceResumeStateDiscard(
    expected: IndexedDbResumeStateDiscardMarker,
    phase: IndexedDbResumeStateDiscardPhase,
  ): Promise<IndexedDbResumeStateDiscardMarker> {
    if (expected.phase !== 'exporting' || phase !== 'exported') {
      throw new TypeError('Resume-state discard has no requested phase transition')
    }
    const currentExpected = snapshotResumeStateDiscardMarker({
      ...expected,
      id: this.#namespace,
      namespace: this.#namespace,
      schemaVersion: 1,
    }, this.#namespace)
    const transaction = this.#transaction(INDEXEDDB_RESUME_STATE_DISCARD_STORE, 'readwrite')
    const store = transaction.objectStore(INDEXEDDB_RESUME_STATE_DISCARD_STORE)
    const stored = await requestResult<unknown>(store.get(this.#namespace))
    if (stored === undefined || !sameResumeStateDiscardMarker(
      snapshotResumeStateDiscardMarker(stored, this.#namespace),
      currentExpected,
    )) {
      transaction.abort()
      throw new Error('Resume-state discard journal changed before its phase transition')
    }
    const next = Object.freeze({ ...currentExpected, phase })
    store.put(storedResumeStateDiscardMarker(next, this.#namespace))
    await transactionCompletion(transaction)
    return next
  }

  async markCleanup(target: string): Promise<void> {
    if (target.length === 0) throw new TypeError('Output cleanup target is empty')
    const transaction = this.#transaction([CLEANUP_STORE, METADATA_STORE], 'readwrite')
    transaction.objectStore(CLEANUP_STORE).put({ id: this.#namespace, namespace: this.#namespace, target, step: 0 } satisfies StoredCleanupMarker)
    const metadata = await requestResult<NamespaceMetadata | undefined>(transaction.objectStore(METADATA_STORE).get(this.#namespace))
    if (metadata !== undefined) transaction.objectStore(METADATA_STORE).put({ ...metadata, state: 'cleanup-pending', cleanupStep: 0 })
    await transactionCompletion(transaction)
  }

  async cleanupTarget(): Promise<string | undefined> {
    const transaction = this.#transaction(CLEANUP_STORE, 'readonly')
    const marker = await requestResult<StoredCleanupMarker | undefined>(transaction.objectStore(CLEANUP_STORE).get(this.#namespace))
    await transactionCompletion(transaction)
    if (marker !== undefined && marker.namespace !== this.#namespace) throw new Error('Output cleanup marker escaped its namespace')
    return marker?.target
  }

  async clearCleanup(): Promise<void> {
    const transaction = this.#transaction([CLEANUP_STORE, METADATA_STORE], 'readwrite')
    transaction.objectStore(CLEANUP_STORE).delete(this.#namespace)
    const metadata = await requestResult<NamespaceMetadata | undefined>(transaction.objectStore(METADATA_STORE).get(this.#namespace))
    if (metadata !== undefined) transaction.objectStore(METADATA_STORE).put({ ...metadata, state: 'active', cleanupStep: 0 })
    await transactionCompletion(transaction)
  }

  /**
   * Removes only records in this V1 namespace. Legacy stores are deliberately
   * absent from this transaction: old state is a cleaner input, never a read path.
   */
  async deleteSessionData(): Promise<void> {
    const transaction = this.#transaction([CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE], 'readwrite')
    await Promise.all([CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE].map((storeName) => deleteNamespaceEntries(transaction.objectStore(storeName), NAMESPACE_INDEX, this.#namespace)))
    await transactionCompletion(transaction)
  }

  /**
   * This is the irreversible metadata boundary for an explicit user discard.
   * Physical output is settled first; one transaction then removes every
   * resumable witness together with the descriptor and stored root capability.
   */
  async commitResumeStateDiscard(
    descriptorKey: string,
    rootCapabilityRef: string,
  ): Promise<void> {
    if (descriptorKey !== this.#namespace || rootCapabilityRef.length === 0) {
      throw new TypeError('resume-state discard binding does not match this namespace')
    }
    const stores = [
      CANDIDATE_STORE,
      COMMITTED_STORE,
      HANDLE_STORE,
      METADATA_STORE,
      CLEANUP_STORE,
      INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
      INDEXEDDB_ROOT_CAPABILITY_STORE,
      INDEXEDDB_RESUME_STATE_DISCARD_STORE,
    ] as const
    const transaction = this.#transaction([...stores], 'readwrite')
    await Promise.all([CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE].map((storeName) =>
      deleteNamespaceEntries(
        transaction.objectStore(storeName),
        NAMESPACE_INDEX,
        this.#namespace,
      )))
    transaction.objectStore(METADATA_STORE).delete(this.#namespace)
    transaction.objectStore(CLEANUP_STORE).delete(this.#namespace)
    transaction.objectStore(INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE).delete(descriptorKey)
    transaction.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE).delete(rootCapabilityRef)
    transaction.objectStore(INDEXEDDB_RESUME_STATE_DISCARD_STORE).delete(this.#namespace)
    await transactionCompletion(transaction)
  }

  /** One-shot cleanup metadata entrypoint; safe to call repeatedly after a crash. */
  async runOwnedCleanup(): Promise<IndexedDbCleanupReport> {
    if (this.#cleanupPromise !== undefined) return this.#cleanupPromise
    const operation = this.#runOwnedCleanup()
    const shared = operation.finally(() => {
      if (this.#cleanupPromise === shared) this.#cleanupPromise = undefined
    })
    this.#cleanupPromise = shared
    return shared
  }

  async #runOwnedCleanup(): Promise<IndexedDbCleanupReport> {
    const target = await this.cleanupTarget()
    if (target === undefined) {
      return Object.freeze({ status: 'nothing-to-clean', removed: 0 })
    }
    const removed = await this.#namespaceEntryCount()
    await this.deleteSessionData()
    await this.clearCleanup()
    return Object.freeze({ status: 'completed', removed })
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  #storedRecord(record: PersistedOutputRecord): StoredRecord {
    return {
      id: this.#key(outputRecordKey(record)),
      namespace: this.#namespace,
      record: snapshotOutputRecord(record),
    }
  }

  #assertRecord(record: PersistedOutputRecord): void {
    if (!recordBelongsToCheckpointNamespace(record, this.binding)) {
      throw new Error('Output checkpoint belongs to another intent or root namespace')
    }
    snapshotOutputRecord(record)
  }

  #assertStoredRecord(entry: StoredRecord): void {
    if (entry.namespace !== this.#namespace || entry.id !== this.#key(outputRecordKey(entry.record as PersistedOutputRecord))) throw new Error('IndexedDB checkpoint key does not match its namespace')
    this.#assertRecord(entry.record)
  }

  #key(key: string): string { return `${this.#namespace}\0${key}` }

  async #scan(storeName: string, scan: OutputJournalScan): Promise<OutputJournalPage> {
    const transaction = this.#transaction(storeName, 'readonly')
    const stored = await scanRecords(transaction.objectStore(storeName), recordRange(this.#namespace, scan), scan.direction === 'ascending' ? 'next' : 'prev')
    await transactionCompletion(transaction)
    const records = stored.map((entry) => {
      this.#assertStoredRecord(entry)
      return snapshotOutputRecord(entry.record)
    })
    validateRecordOrder(records, scan)
    const page = Object.freeze(records)
    const last = records.at(-1)
    return records.length !== OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT || last === undefined
      ? Object.freeze({ records: page })
      : Object.freeze({ records: page, nextCursor: outputRecordKey(last) })
  }

  async #delete(storeName: string, key: string): Promise<void> {
    const transaction = this.#transaction(storeName, 'readwrite')
    transaction.objectStore(storeName).delete(key)
    await transactionCompletion(transaction)
  }

  async #ensureOwnershipMetadata(): Promise<void> {
    const transaction = this.#transaction(METADATA_STORE, 'readwrite')
    const store = transaction.objectStore(METADATA_STORE)
    const existing = await requestResult<NamespaceMetadata | undefined>(store.get(this.#namespace))
    if (existing !== undefined && !this.#ownershipMetadataMatches(existing)) {
      transaction.abort()
      throw new Error('FileCheckpointV1 namespace ownership does not match this output root')
    }
    if (existing === undefined) store.put({ id: this.#namespace, marker: FILE_CHECKPOINT_OWNERSHIP_MARKER, namespaceName: FILE_CHECKPOINT_NAMESPACE, backend: this.binding.backend, transferIntentDigest: this.binding.transferIntentDigest, rootIdentity: this.binding.rootIdentity, state: 'active', cleanupStep: 0 } satisfies NamespaceMetadata)
    await transactionCompletion(transaction)
  }

  async #requireOwnershipMetadata(): Promise<void> {
    const transaction = this.#transaction(METADATA_STORE, 'readonly')
    const existing = await requestResult<NamespaceMetadata | undefined>(
      transaction.objectStore(METADATA_STORE).get(this.#namespace),
    )
    await transactionCompletion(transaction)
    if (existing === undefined || !this.#ownershipMetadataMatches(existing) ||
        existing.state !== 'active' || existing.cleanupStep !== 0) {
      throw new Error('FileCheckpointV1 namespace ownership is missing or does not match')
    }
  }

  #ownershipMetadataMatches(existing: NamespaceMetadata): boolean {
    return existing.marker === FILE_CHECKPOINT_OWNERSHIP_MARKER &&
      existing.namespaceName === FILE_CHECKPOINT_NAMESPACE &&
      existing.backend === this.binding.backend &&
      existing.transferIntentDigest === this.binding.transferIntentDigest &&
      existing.rootIdentity === this.binding.rootIdentity
  }

  async #namespaceEntryCount(): Promise<number> {
    const stores = [CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE] as const
    const transaction = this.#transaction([...stores], 'readonly')
    const counts = await Promise.all(stores.map((name) => requestResult<number>(
      transaction.objectStore(name).index(NAMESPACE_INDEX).count(IDBKeyRange.only(this.#namespace)),
    )))
    await transactionCompletion(transaction)
    return counts.reduce((total, count) => total + count, 0)
  }

  #transaction(storeNames: string | string[], mode: IDBTransactionMode): IDBTransaction {
    if (this.#closed) throw new DOMException('Output checkpoint database is closed or version-obsolete', 'InvalidStateError')
    return this.#database.transaction(storeNames, mode)
  }

}

export interface IndexedDbCleanupReport {
  readonly status: 'nothing-to-clean' | 'completed'
  readonly removed: number
}

function validateRecordOrder(records: readonly PersistedOutputRecord[], scan: OutputJournalScan): void {
  let previous = scan.cursor
  for (const record of records) {
    const key = outputRecordKey(record)
    if (previous !== undefined && !isAfterCursor(key, previous, scan.direction)) throw new Error('IndexedDB checkpoint cursor order is not strictly monotonic')
    previous = key
  }
}
function isAfterCursor(key: string, cursor: string, direction: OutputJournalScan['direction']): boolean { return direction === 'ascending' ? key > cursor : key < cursor }

function recordRange(namespace: string, scan: OutputJournalScan): IDBKeyRange {
  const kindPrefix = scan.kind === undefined ? '' : `${scan.kind}:`
  const prefix = `${namespace}\0${kindPrefix}`
  const boundary = `${prefix}\uffff`
  if (scan.direction === 'ascending') {
    const lower = scan.cursor === undefined ? prefix : `${namespace}\0${scan.cursor}`
    return IDBKeyRange.bound(lower, boundary, scan.cursor !== undefined, false)
  }
  const upper = scan.cursor === undefined ? boundary : `${namespace}\0${scan.cursor}`
  return IDBKeyRange.bound(prefix, upper, false, scan.cursor !== undefined)
}
function scanRecords(store: IDBObjectStore, range: IDBKeyRange, direction: IDBCursorDirection): Promise<StoredRecord[]> {
  return new Promise<StoredRecord[]>((resolve, reject) => {
    const records: StoredRecord[] = []
    const request = store.openCursor(range, direction)
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || records.length === OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT) { resolve(records); return }
      records.push(cursor.value as StoredRecord)
      if (records.length === OUTPUT_CHECKPOINT_PAGE_RECORD_LIMIT) { resolve(records); return }
      cursor.continue()
    })
  })
}
function deleteNamespaceEntries(store: IDBObjectStore, indexName: string, value: IDBValidKey): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = store.index(indexName).openKeyCursor(IDBKeyRange.only(value))
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) { resolve(); return }
      store.delete(cursor.primaryKey)
      cursor.continue()
    })
  })
}
