import type {
  OutputCheckpointJournal,
  OutputJournalPage,
  OutputJournalScan,
  PersistedOutputRecord,
} from '../persistence/journal'
import {
  OUTPUT_JOURNAL_PAGE_RECORD_LIMIT,
  outputRecordKey,
  snapshotOutputRecord,
} from '../persistence/journal'

const DATABASE_VERSION = 2
const CANDIDATE_STORE = 'checkpoint-candidates'
const COMMITTED_STORE = 'checkpoint-committed'
const HANDLE_STORE = 'persistent-handles'
const CLEANUP_STORE = 'cleanup-markers'
const SESSION_INDEX = 'by-session'

interface StoredRecord {
  readonly id: string
  readonly session: string
  readonly record: PersistedOutputRecord
}

interface StoredHandle {
  readonly id: string
  readonly session: string
  readonly handle: FileSystemHandle
}

interface StoredCleanupMarker {
  readonly id: string
  readonly session: string
  readonly target: string
}

export interface PersistentHandleRepository {
  putHandle(identity: string, handle: FileSystemHandle): Promise<void>
  getHandle(identity: string): Promise<FileSystemHandle | undefined>
  deleteHandle(identity: string): Promise<void>
}

/** IndexedDB is used because it can durably structured-clone FSA handles. */
export class IndexedDbOutputRepository
implements OutputCheckpointJournal, PersistentHandleRepository {
  readonly #database: IDBDatabase
  readonly #session: string
  #closed = false

  private constructor(database: IDBDatabase, backend: string, outputSessionId: string) {
    this.#database = database
    this.#session = `${backend}\0${outputSessionId}`
    database.addEventListener('versionchange', () => {
      this.#closed = true
      database.close()
    })
  }

  static async open(
    databaseName: string,
    backend: string,
    outputSessionId: string,
  ): Promise<IndexedDbOutputRepository> {
    if (databaseName.length === 0) throw new TypeError('IndexedDB name must not be empty')
    const database = await openDatabase(databaseName)
    return new IndexedDbOutputRepository(database, backend, outputSessionId)
  }

  scanCommitted(scan: OutputJournalScan): Promise<OutputJournalPage> {
    return this.#scan(COMMITTED_STORE, scan)
  }

  scanCandidates(scan: OutputJournalScan): Promise<OutputJournalPage> {
    return this.#scan(CANDIDATE_STORE, scan)
  }

  async writeCandidate(record: PersistedOutputRecord): Promise<void> {
    const transaction = this.#transaction(CANDIDATE_STORE, 'readwrite')
    transaction.objectStore(CANDIDATE_STORE).put(this.#storedRecord(record))
    await transactionCompletion(transaction)
  }

  async flushCandidate(key: string): Promise<void> {
    const transaction = this.#transaction(CANDIDATE_STORE, 'readonly')
    const candidate = await requestResult<StoredRecord | undefined>(
      transaction.objectStore(CANDIDATE_STORE).get(this.#key(key)),
    )
    await transactionCompletion(transaction)
    if (candidate === undefined) {
      throw new Error('Output checkpoint candidate was not durably written')
    }
  }

  async commitCandidate(key: string): Promise<void> {
    const transaction = this.#transaction(
      [CANDIDATE_STORE, COMMITTED_STORE],
      'readwrite',
    )
    const candidates = transaction.objectStore(CANDIDATE_STORE)
    const candidate = await requestResult<StoredRecord | undefined>(
      candidates.get(this.#key(key)),
    )
    if (candidate === undefined) {
      transaction.abort()
      throw new Error('Cannot commit a missing output checkpoint candidate')
    }
    transaction.objectStore(COMMITTED_STORE).put(candidate)
    candidates.delete(candidate.id)
    await transactionCompletion(transaction)
  }

  async readCommitted(key: string): Promise<PersistedOutputRecord | undefined> {
    const transaction = this.#transaction(COMMITTED_STORE, 'readonly')
    const stored = await requestResult<StoredRecord | undefined>(
      transaction.objectStore(COMMITTED_STORE).get(this.#key(key)),
    )
    await transactionCompletion(transaction)
    return stored === undefined ? undefined : snapshotOutputRecord(stored.record)
  }

  async discardCandidate(key: string): Promise<void> {
    await this.#delete(CANDIDATE_STORE, this.#key(key))
  }

  async deleteCommitted(key: string): Promise<void> {
    await this.#delete(COMMITTED_STORE, this.#key(key))
  }

  async putHandle(identity: string, handle: FileSystemHandle): Promise<void> {
    const transaction = this.#transaction(HANDLE_STORE, 'readwrite')
    const stored: StoredHandle = {
      id: this.#key(identity),
      session: this.#session,
      handle,
    }
    transaction.objectStore(HANDLE_STORE).put(stored)
    await transactionCompletion(transaction)
  }

  async getHandle(identity: string): Promise<FileSystemHandle | undefined> {
    const transaction = this.#transaction(HANDLE_STORE, 'readonly')
    const stored = await requestResult<StoredHandle | undefined>(
      transaction.objectStore(HANDLE_STORE).get(this.#key(identity)),
    )
    await transactionCompletion(transaction)
    return stored?.handle
  }

  async deleteHandle(identity: string): Promise<void> {
    await this.#delete(HANDLE_STORE, this.#key(identity))
  }

  async markCleanup(target: string): Promise<void> {
    if (target.length === 0) throw new TypeError('Output cleanup target is empty')
    const transaction = this.#transaction(CLEANUP_STORE, 'readwrite')
    const marker: StoredCleanupMarker = {
      id: this.#session,
      session: this.#session,
      target,
    }
    transaction.objectStore(CLEANUP_STORE).put(marker)
    await transactionCompletion(transaction)
  }

  async cleanupTarget(): Promise<string | undefined> {
    const transaction = this.#transaction(CLEANUP_STORE, 'readonly')
    const marker = await requestResult<StoredCleanupMarker | undefined>(
      transaction.objectStore(CLEANUP_STORE).get(this.#session),
    )
    await transactionCompletion(transaction)
    if (marker !== undefined && (marker.id !== this.#session || marker.session !== this.#session)) {
      throw new Error('Output cleanup marker escaped its session boundary')
    }
    return marker?.target
  }

  async clearCleanup(): Promise<void> {
    await this.#delete(CLEANUP_STORE, this.#session)
  }

  async deleteSessionData(): Promise<void> {
    const transaction = this.#transaction(
      [CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE],
      'readwrite',
    )
    await Promise.all([CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE].map((storeName) =>
      deleteIndexEntries(transaction.objectStore(storeName), SESSION_INDEX, this.#session)))
    await transactionCompletion(transaction)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  #storedRecord(record: PersistedOutputRecord): StoredRecord {
    return {
      id: this.#key(outputRecordKey(record)),
      session: this.#session,
      record: snapshotOutputRecord(record),
    }
  }

  #key(key: string): string {
    return `${this.#session}\0${key}`
  }

  async #scan(storeName: string, scan: OutputJournalScan): Promise<OutputJournalPage> {
    const transaction = this.#transaction(storeName, 'readonly')
    const stored = await scanRecords(
      transaction.objectStore(storeName),
      recordRange(this.#session, scan),
      scan.direction === 'ascending' ? 'next' : 'prev',
    )
    await transactionCompletion(transaction)
    const records = stored.map((entry) => this.#validatedRecord(entry))
    validateRecordOrder(records, scan)
    const last = records.at(-1)
    const frozenRecords = Object.freeze(records)
    if (records.length !== OUTPUT_JOURNAL_PAGE_RECORD_LIMIT || last === undefined) {
      return Object.freeze({ records: frozenRecords })
    }
    return Object.freeze({ records: frozenRecords, nextCursor: outputRecordKey(last) })
  }

  #validatedRecord(entry: StoredRecord): PersistedOutputRecord {
    const record = snapshotOutputRecord(entry.record)
    if (entry.session !== this.#session || entry.id !== this.#key(outputRecordKey(record))) {
      throw new Error('IndexedDB output journal key does not match its record')
    }
    return record
  }

  async #delete(storeName: string, key: string): Promise<void> {
    const transaction = this.#transaction(storeName, 'readwrite')
    transaction.objectStore(storeName).delete(key)
    await transactionCompletion(transaction)
  }

  #transaction(
    storeNames: string | string[],
    mode: IDBTransactionMode,
  ): IDBTransaction {
    if (this.#closed) {
      throw new DOMException(
        'Output checkpoint database is closed or version-obsolete',
        'InvalidStateError',
      )
    }
    return this.#database.transaction(storeNames, mode)
  }
}

function validateRecordOrder(
  records: readonly PersistedOutputRecord[],
  scan: OutputJournalScan,
): void {
  let previous = scan.cursor
  for (const record of records) {
    const key = outputRecordKey(record)
    if (previous !== undefined && !isAfterCursor(key, previous, scan.direction)) {
      throw new Error('IndexedDB output journal cursor order is not strictly monotonic')
    }
    previous = key
  }
}

function isAfterCursor(
  key: string,
  cursor: string,
  direction: OutputJournalScan['direction'],
): boolean {
  if (direction === 'ascending') return key > cursor
  return key < cursor
}

async function openDatabase(name: string): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB output checkpoints are unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () => {
    const database = request.result
    for (const storeName of [CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE, CLEANUP_STORE]) {
      if (database.objectStoreNames.contains(storeName)) continue
      database.createObjectStore(storeName, { keyPath: 'id' })
        .createIndex(SESSION_INDEX, 'session')
    }
  })
  let rejected = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => {
      rejected = true
      reject(new DOMException(
        'Output checkpoint database upgrade is blocked by another tab',
        'InvalidStateError',
      ))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (rejected) {
        request.result.close()
        return
      }
      resolve(request.result)
    }, { once: true })
  })
}

function recordRange(session: string, scan: OutputJournalScan): IDBKeyRange {
  const kindPrefix = scan.kind === undefined ? '' : `${scan.kind}:`
  const prefix = `${session}\0${kindPrefix}`
  const boundary = `${prefix}\uffff`
  if (scan.direction === 'ascending') {
    const lower = scan.cursor === undefined ? prefix : `${session}\0${scan.cursor}`
    return IDBKeyRange.bound(lower, boundary, scan.cursor !== undefined, false)
  }
  const upper = scan.cursor === undefined ? boundary : `${session}\0${scan.cursor}`
  return IDBKeyRange.bound(prefix, upper, false, scan.cursor !== undefined)
}

function scanRecords(
  store: IDBObjectStore,
  range: IDBKeyRange,
  direction: IDBCursorDirection,
): Promise<StoredRecord[]> {
  return new Promise<StoredRecord[]>((resolve, reject) => {
    const records: StoredRecord[] = []
    const request = store.openCursor(range, direction)
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || records.length === OUTPUT_JOURNAL_PAGE_RECORD_LIMIT) {
        resolve(records)
        return
      }
      records.push(cursor.value as StoredRecord)
      if (records.length === OUTPUT_JOURNAL_PAGE_RECORD_LIMIT) {
        resolve(records)
        return
      }
      cursor.continue()
    })
  })
}

function deleteIndexEntries(
  store: IDBObjectStore,
  indexName: string,
  value: IDBValidKey,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = store.index(indexName).openKeyCursor(IDBKeyRange.only(value))
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) {
        resolve()
        return
      }
      store.delete(cursor.primaryKey)
      cursor.continue()
    })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}
