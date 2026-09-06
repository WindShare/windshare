import {
  DEFAULT_OUTPUT_DATABASE_NAME,
  INDEXEDDB_OPFS_TASK_STORE,
  INDEXEDDB_OPFS_ENTRY_STORE,
  INDEXEDDB_OPFS_PATH_STORE,
  INDEXEDDB_OPFS_DIRECTORY_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../../browser/indexeddb-database'
import {
  MAX_TASK_ENTRY_PAGE_SIZE,
  snapshotTaskCheckpoint,
  snapshotTaskEntry,
  type TaskCheckpoint,
  type TaskEntry,
  type TaskObjectRef,
  type TaskDirectoryPin,
} from './model'
import type { TaskCheckpointCommit, TaskCheckpointStore } from './store'
import { receiveOperationLeaseId, type ReceiveOperationLeaseRecord } from '../../workspace/records'

interface EntryRow { id: string; objectKey: string; sequenceKey: string; entry: TaskEntry }
interface PathRow { id: string; kind: 'file' | 'directory'; entryId?: string }
interface CheckpointRow { id: string; checkpoint: TaskCheckpoint }

export class IndexedDbTaskCheckpointStore implements TaskCheckpointStore {
  readonly #database: IDBDatabase
  readonly #object: TaskObjectRef
  readonly #key: string
  readonly #expectedLeaseId: string | undefined

  private constructor(database: IDBDatabase, object: TaskObjectRef, expectedLeaseId?: string) {
    this.#database = database
    this.#expectedLeaseId = expectedLeaseId
    this.#object = Object.freeze({ ...object })
    this.#key = JSON.stringify([object.operationId, object.objectId])
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(object: TaskObjectRef, databaseName = DEFAULT_OUTPUT_DATABASE_NAME, expectedLeaseId?: string): Promise<IndexedDbTaskCheckpointStore> {
    return new IndexedDbTaskCheckpointStore(await openIndexedDbCheckpointDatabase(databaseName), object, expectedLeaseId)
  }

  static async retireOperation(operationId: string, databaseName = DEFAULT_OUTPUT_DATABASE_NAME): Promise<void> {
    const database = await openIndexedDbCheckpointDatabase(databaseName)
    try {
      const transaction = database.transaction([
        INDEXEDDB_OPFS_TASK_STORE, INDEXEDDB_OPFS_ENTRY_STORE,
        INDEXEDDB_OPFS_PATH_STORE, INDEXEDDB_OPFS_DIRECTORY_STORE,
      ], 'readwrite')
      const completion = transactionCompletion(transaction)
      completion.catch(() => undefined)
      const prefix = JSON.stringify([operationId]).slice(0, -1) + ','
      const tasks = transaction.objectStore(INDEXEDDB_OPFS_TASK_STORE)
      await visitCursor(tasks.openCursor(IDBKeyRange.bound(prefix, prefix + '\uffff')), async cursor => {
        const row = cursor.value as CheckpointRow
        if (row.checkpoint.object.operationId !== operationId) throw new TypeError('Task cleanup escaped operation')
        const childPrefix = JSON.stringify([row.id]).slice(0, -1) + ','
        for (const name of [INDEXEDDB_OPFS_ENTRY_STORE, INDEXEDDB_OPFS_PATH_STORE, INDEXEDDB_OPFS_DIRECTORY_STORE]) {
          await visitCursor(transaction.objectStore(name).openCursor(
            IDBKeyRange.bound(childPrefix, childPrefix + '\uffff'),
          ), child => { child.delete(); return Promise.resolve() })
        }
        cursor.delete()
      })
      await completion
    } finally {
      database.close()
    }
  }

  async readCheckpoint(): Promise<TaskCheckpoint | undefined> {
    const row = await this.#read<CheckpointRow>(INDEXEDDB_OPFS_TASK_STORE, this.#key)
    return row === undefined ? undefined : snapshotTaskCheckpoint(row.checkpoint)
  }

  async readEntry(entryId: string): Promise<TaskEntry | undefined> {
    const row = await this.#read<EntryRow>(INDEXEDDB_OPFS_ENTRY_STORE, this.#entryKey(entryId))
    return row === undefined ? undefined : snapshotTaskEntry(row.entry)
  }

  async readDirectoryPin(directoryId: string): Promise<TaskDirectoryPin | undefined> {
    const row = await this.#read<{ pin: TaskDirectoryPin }>(INDEXEDDB_OPFS_DIRECTORY_STORE, this.#entryKey(directoryId))
    return row === undefined ? undefined : Object.freeze({ ...row.pin, sourcePath: Object.freeze([...row.pin.sourcePath]) })
  }

  async readPath(path: readonly string[]): Promise<TaskEntry | undefined> {
    const row = await this.#read<PathRow>(INDEXEDDB_OPFS_PATH_STORE, this.#pathKey(path))
    return row?.entryId === undefined ? undefined : this.readEntry(row.entryId)
  }

  async readEntries(input: Readonly<{ afterSequence?: bigint; limit: number }>): Promise<readonly TaskEntry[]> {
    if (!Number.isSafeInteger(input.limit) || input.limit < 1 || input.limit > MAX_TASK_ENTRY_PAGE_SIZE) {
      throw new TypeError('Task entry page exceeds its bounded read window')
    }
    const transaction = this.#database.transaction(INDEXEDDB_OPFS_ENTRY_STORE, 'readonly')
    const lower = [this.#key, input.afterSequence === undefined ? '' : sequenceKey(input.afterSequence)]
    const range = IDBKeyRange.bound(lower, [this.#key, '\uffff'], input.afterSequence !== undefined)
    const rows = await requestResult<EntryRow[]>(transaction.objectStore(INDEXEDDB_OPFS_ENTRY_STORE)
      .index('by-object-sequence').getAll(range, input.limit))
    await transactionCompletion(transaction)
    return Object.freeze(rows.map(row => snapshotTaskEntry(row.entry)))
  }

  async commit(input: TaskCheckpointCommit): Promise<void> {
    const checkpoint = snapshotTaskCheckpoint(input.checkpoint)
    const entries = input.entries.map(snapshotTaskEntry)
    if (!sameValue(checkpoint.object, this.#object)) throw new TypeError('Task checkpoint escaped object ownership')
    const transaction = this.#database.transaction([
      INDEXEDDB_OPFS_TASK_STORE, INDEXEDDB_OPFS_ENTRY_STORE, INDEXEDDB_OPFS_PATH_STORE, INDEXEDDB_OPFS_DIRECTORY_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite', { durability: 'strict' })
    const completion = transactionCompletion(transaction)
    // Handle an abort immediately, including failures before the caller reaches the await below.
    completion.catch(() => undefined)
    try {
      if (this.#expectedLeaseId !== undefined) {
        const lease = await requestResult<ReceiveOperationLeaseRecord | undefined>(
          transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE).get(receiveOperationLeaseId(this.#object.operationId)),
        )
        if (lease?.leaseId !== this.#expectedLeaseId) {
          throw new DOMException('Task checkpoint operation lease was superseded', 'InvalidStateError')
        }
      }
      const tasks = transaction.objectStore(INDEXEDDB_OPFS_TASK_STORE)
      const previous = await requestResult<CheckpointRow | undefined>(tasks.get(this.#key))
      if (previous?.checkpoint.generation !== input.expectedGeneration ||
          checkpoint.generation !== (input.expectedGeneration ?? 0n) + 1n) {
        throw new DOMException('Task checkpoint generation was superseded', 'InvalidStateError')
      }
      if (previous !== undefined) validateCheckpointAdvance(previous.checkpoint, checkpoint)
      await this.#writePins(transaction, input.directoryPins ?? [])
      await this.#writeEntries(transaction, entries, previous?.checkpoint, checkpoint)
      tasks.put({ id: this.#key, checkpoint } satisfies CheckpointRow)
      await completion
    } catch (error) {
      try { transaction.abort() } catch { /* A failed commit may already have aborted. */ }
      await completion.catch(() => undefined)
      throw error
    }
  }

  async #writePins(transaction: IDBTransaction, pins: readonly TaskDirectoryPin[]): Promise<void> {
    const directories = transaction.objectStore(INDEXEDDB_OPFS_DIRECTORY_STORE)
    for (const pin of pins) {
      if (!pin.directoryId || !pin.generation || !Array.isArray(pin.sourcePath)) {
        throw new TypeError('Task directory pin lacks authenticated generation evidence')
      }
      const id = this.#entryKey(pin.directoryId)
      const previousPin = await requestResult<{ pin: TaskDirectoryPin } | undefined>(directories.get(id))
      if (previousPin !== undefined && !sameValue(previousPin.pin, pin)) {
        throw new TypeError('Task discovery changed a pinned authenticated directory generation')
      }
      directories.put({ id, pin: { ...pin, sourcePath: [...pin.sourcePath] } })
    }
  }

  async #writeEntries(transaction: IDBTransaction, entries: readonly TaskEntry[], previous: TaskCheckpoint | undefined, checkpoint: TaskCheckpoint): Promise<void> {
    const entryStore = transaction.objectStore(INDEXEDDB_OPFS_ENTRY_STORE)
    const previousCount = previous?.entryCount ?? 0n
    let nextOffset = previous?.allocatedLength ?? 0n
    let inserted = 0n
    for (const entry of entries) {
      validatePhysicalCoverage(entry, checkpoint.physicalLength)
      const id = this.#entryKey(entry.entryId)
      const old = await requestResult<EntryRow | undefined>(entryStore.get(id))
      if (old !== undefined) validateEntryAdvance(old.entry, entry)
      else {
        if (entry.zipLayout?.sequence !== previousCount + inserted ||
            entry.zipLayout.localHeaderOffset !== nextOffset) {
          throw new TypeError('Task entry allocation must append a contiguous sequence')
        }
        inserted++
        nextOffset = entry.zipLayout.endOffset
      }
      await this.#claimPath(transaction, entry)
      entryStore.put({ id, objectKey: this.#key, sequenceKey: sequenceKey(entry.zipLayout!.sequence), entry } satisfies EntryRow)
    }
    if (checkpoint.entryCount !== previousCount + inserted || checkpoint.allocatedLength !== nextOffset) {
      throw new TypeError('Task checkpoint entry count disagrees with allocated entries')
    }
  }

  close(): void { this.#database.close() }

  async #claimPath(transaction: IDBTransaction, entry: TaskEntry): Promise<void> {
    const paths = transaction.objectStore(INDEXEDDB_OPFS_PATH_STORE)
    for (let depth = 1; depth <= entry.path.length; depth++) {
      const id = this.#pathKey(entry.path.slice(0, depth))
      const leaf = depth === entry.path.length
      const kind = leaf ? entry.kind : 'directory'
      const old = await requestResult<PathRow | undefined>(paths.get(id))
      if (old !== undefined && (old.kind !== kind ||
          (leaf && old.entryId !== undefined && old.entryId !== entry.entryId))) {
        throw new TypeError('Task entry conflicts with an allocated path or its directory topology')
      }
      const entryId = leaf ? entry.entryId : old?.entryId
      paths.put({ id, kind, ...(entryId === undefined ? {} : { entryId }) } satisfies PathRow)
    }
  }

  #entryKey(entryId: string): string { return JSON.stringify([this.#key, entryId]) }
  #pathKey(path: readonly string[]): string { return JSON.stringify([this.#key, ...path]) }
  async #read<T>(store: string, id: string): Promise<T | undefined> {
    const transaction = this.#database.transaction(store, 'readonly')
    const result = await requestResult<T | undefined>(transaction.objectStore(store).get(id))
    await transactionCompletion(transaction)
    return result
  }
}

function visitCursor(
  request: IDBRequest<IDBCursorWithValue | null>,
  visit: (cursor: IDBCursorWithValue) => Promise<void>,
): Promise<void> {
  return new Promise((resolve, reject) => {
    request.addEventListener('error', () => reject(request.error))
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) { resolve(); return }
      visit(cursor).then(() => cursor.continue(), reject)
    })
  })
}

function sequenceKey(sequence: bigint): string { return sequence.toString(16).padStart(16, '0') }

function validateCheckpointAdvance(previous: TaskCheckpoint, next: TaskCheckpoint): void {
  if (previous.artifactState === 'sealed' || next.allocatedLength < previous.allocatedLength ||
      (previous.discoveryComplete && !next.discoveryComplete) ||
      !sameValue(previous.selectedPaths, next.selectedPaths)) {
    throw new TypeError('Task checkpoint regressed immutable recovery authority')
  }
}

function validatePhysicalCoverage(entry: TaskEntry, physicalLength: bigint): void {
  if (entry.zipLayout === undefined || entry.ranges.some(range => entry.zipLayout!.payloadOffset + range.end > physicalLength)) {
    throw new TypeError('Task checkpoint payload ranges exceed physical object length')
  }
}

function validateEntryAdvance(previous: TaskEntry, next: TaskEntry): void {
  if (!sameValue({ ...previous, ranges: [], revisionFailure: undefined },
    { ...next, ranges: [], revisionFailure: undefined })) {
    throw new TypeError('Task entry changed its authenticated revision or fixed layout')
  }
  // Coalescing adjacent summaries may change CRC boundaries, but never drops committed coverage.
  for (const range of previous.ranges) {
    if (!next.ranges.some(candidate => candidate.start <= range.start && candidate.end >= range.end)) {
      throw new TypeError('Task entry discarded committed payload coverage')
    }
    const exact = next.ranges.find(candidate => candidate.start === range.start && candidate.end === range.end)
    if (exact !== undefined && exact.crc32 !== range.crc32) throw new TypeError('Task entry changed a committed CRC summary')
  }
}

function sameValue(left: unknown, right: unknown): boolean {
  const encode = (value: unknown) => JSON.stringify(value, (_, field: unknown) =>
    typeof field === 'bigint' ? { bigint: field.toString() } : field)
  return encode(left) === encode(right)
}
