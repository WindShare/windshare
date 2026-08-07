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
  recordBelongsToSession,
  snapshotOutputRecord,
} from '../persistence/journal'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  deriveCheckpointIdentity,
} from '../persistence/checkpoint'
import { durableCheckpointNamespaceKey } from '../persistence/namespace'
import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import {
  createOutputCapabilityIdentity,
  snapshotOutputCapabilityIdentity,
  type OutputCapabilityIdentity,
} from '../capability/contract'

/** Version 3 creates V1 stores; old schema stores remain untouched and unread. */
export const CHECKPOINT_DATABASE_VERSION = 3
export const DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME = 'windshare-output-checkpoints'
const CANDIDATE_STORE = 'file-checkpoint-v1-candidates'
const COMMITTED_STORE = 'file-checkpoint-v1-committed'
const HANDLE_STORE = 'file-checkpoint-v1-handles'
export const INDEXEDDB_CHECKPOINT_METADATA_STORE = 'file-checkpoint-v1-metadata'
const METADATA_STORE = INDEXEDDB_CHECKPOINT_METADATA_STORE
const CLEANUP_STORE = 'file-checkpoint-v1-cleanup'
const NAMESPACE_INDEX = 'by-namespace'
const ROOT_BINDING_PREFIX = `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0root-binding\0`
const ROOT_BINDING_BUCKET_LIMIT = 128
const ROOT_BINDING_INSTALL_RETRY_LIMIT = 32

interface StoredRecord {
  readonly id: string
  readonly namespace: string
  /** V1 records intentionally do not contain OutputSessionID. */
  readonly record: Omit<PersistedOutputRecord, 'outputSessionId'>
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

/** Stable intent-level root lock; unlike records it does not contain run state. */
interface IntentRootBindingMetadata {
  readonly id: string
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly transferIntentDigest: string
  readonly rootIdentity: string
}

/**
 * Root bindings are intentionally kept outside any intent namespace.  A bucket
 * is keyed by the browser-visible root name only as an index; every candidate
 * still requires `isSameEntry` verification before its identity is accepted.
 * Optimistic revisions make concurrent tabs append atomically without treating
 * a name or path as ownership proof.
 */
interface RootBindingEntry {
  readonly rootIdentity: string
  readonly handle: FileSystemDirectoryHandle
}

interface RootBindingMetadata {
  readonly id: string
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly rootName: string
  readonly revision: number
  readonly entries: readonly RootBindingEntry[]
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

export interface IndexedDbCheckpointBinding {
  readonly transferIntentDigest: string
  readonly rootIdentity: string
}

/** A bounded, namespace-owned IndexedDB adapter for FileCheckpointV1. */
export class IndexedDbOutputRepository implements OutputCheckpointJournal, PersistentHandleRepository {
  readonly binding: Required<Pick<CheckpointNamespaceBinding, 'backend' | 'transferIntentDigest' | 'rootIdentity'>> & Pick<CheckpointNamespaceBinding, 'outputSessionId'>
  readonly #database: IDBDatabase
  readonly #namespace: string
  readonly #runtimeSessionId: string
  #closed = false
  #cleanupPromise: Promise<IndexedDbCleanupReport> | undefined

  private constructor(
    database: IDBDatabase,
    backend: string,
    outputSessionId: string,
    binding: IndexedDbCheckpointBinding,
  ) {
    this.#database = database
    this.#runtimeSessionId = requireText(outputSessionId, 'output session')
    const transferIntentDigest = requireBindingIdentity(binding.transferIntentDigest, 'transfer intent digest')
    const rootIdentity = requireBindingIdentity(binding.rootIdentity, 'root identity')
    this.binding = Object.freeze({
      backend: requireText(backend, 'backend'),
      transferIntentDigest,
      rootIdentity,
      outputSessionId: this.#runtimeSessionId,
    })
    this.#namespace = durableCheckpointNamespaceKey(this.binding)
    database.addEventListener('versionchange', () => {
      this.#closed = true
      database.close()
    })
  }

  static async open(databaseName: string, backend: string, outputSessionId: string, binding: IndexedDbCheckpointBinding): Promise<IndexedDbOutputRepository> {
    if (databaseName.length === 0) throw new TypeError('IndexedDB name must not be empty')
    const database = await openIndexedDbCheckpointDatabase(databaseName)
    const repository = new IndexedDbOutputRepository(database, backend, outputSessionId, binding)
    try {
      await repository.#ensureOwnershipMetadata()
      return repository
    } catch (error) {
      repository.close()
      throw error
    }
  }

  /**
   * Test/browser-probe adapter for fixtures that do not model a picker-issued
   * root or frozen intent. Production factories must call open with both
   * identities explicitly; this adapter is intentionally named so accidental
   * fallback cannot become a durable runtime path.
   */
  static openForTest(
    databaseName: string,
    backend: string,
    outputSessionId: string,
  ): Promise<IndexedDbOutputRepository> {
    return IndexedDbOutputRepository.open(databaseName, backend, outputSessionId, {
      transferIntentDigest: deriveCheckpointIdentity(`windshare/test-intent/v1\0${backend}`),
      rootIdentity: deriveCheckpointIdentity(`windshare/test-root/v1\0${backend}`),
    })
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
    return this.#restoreRuntimeSession(stored.record)
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
    const durable = { ...record }
    Reflect.deleteProperty(durable, 'outputSessionId')
    return { id: this.#key(outputRecordKey(record)), namespace: this.#namespace, record: durable as Omit<PersistedOutputRecord, 'outputSessionId'> }
  }

  #restoreRuntimeSession(record: Omit<PersistedOutputRecord, 'outputSessionId'>): PersistedOutputRecord {
    return snapshotOutputRecord({ ...record, outputSessionId: this.#runtimeSessionId } as PersistedOutputRecord)
  }

  #assertRecord(record: PersistedOutputRecord): void {
    if (!recordBelongsToSession(record, this.binding)) throw new Error('Output checkpoint belongs to another intent or root namespace')
    snapshotOutputRecord(record)
  }

  #assertStoredRecord(entry: StoredRecord): void {
    if (entry.namespace !== this.#namespace || entry.id !== this.#key(outputRecordKey(entry.record as PersistedOutputRecord))) throw new Error('IndexedDB checkpoint key does not match its namespace')
    this.#assertRecord(this.#restoreRuntimeSession(entry.record))
  }

  #key(key: string): string { return `${this.#namespace}\0${key}` }

  async #scan(storeName: string, scan: OutputJournalScan): Promise<OutputJournalPage> {
    const transaction = this.#transaction(storeName, 'readonly')
    const stored = await scanRecords(transaction.objectStore(storeName), recordRange(this.#namespace, scan), scan.direction === 'ascending' ? 'next' : 'prev')
    await transactionCompletion(transaction)
    const records = stored.map((entry) => {
      this.#assertStoredRecord(entry)
      return this.#restoreRuntimeSession(entry.record)
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
    const intentBindingId = this.#intentBindingKey()
    const [existing, existingIntentBinding] = await Promise.all([
      requestResult<NamespaceMetadata | undefined>(store.get(this.#namespace)),
      requestResult<IntentRootBindingMetadata | undefined>(store.get(intentBindingId)),
    ])
    if (existing !== undefined && (existing.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER || existing.namespaceName !== FILE_CHECKPOINT_NAMESPACE || existing.backend !== this.binding.backend || existing.transferIntentDigest !== this.binding.transferIntentDigest || existing.rootIdentity !== this.binding.rootIdentity)) {
      transaction.abort()
      throw new Error('FileCheckpointV1 namespace ownership does not match this output root')
    }
    if (existingIntentBinding !== undefined &&
        (existingIntentBinding.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
         existingIntentBinding.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
         existingIntentBinding.backend !== this.binding.backend ||
         existingIntentBinding.transferIntentDigest !== this.binding.transferIntentDigest ||
         existingIntentBinding.rootIdentity !== this.binding.rootIdentity)) {
      transaction.abort()
      throw new Error('FileCheckpointV1 intent is already bound to another output root')
    }
    if (existing === undefined) store.put({ id: this.#namespace, marker: FILE_CHECKPOINT_OWNERSHIP_MARKER, namespaceName: FILE_CHECKPOINT_NAMESPACE, backend: this.binding.backend, transferIntentDigest: this.binding.transferIntentDigest, rootIdentity: this.binding.rootIdentity, state: 'active', cleanupStep: 0 } satisfies NamespaceMetadata)
    if (existingIntentBinding === undefined) store.put({
      id: intentBindingId,
      marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
      namespaceName: FILE_CHECKPOINT_NAMESPACE,
      backend: this.binding.backend,
      transferIntentDigest: this.binding.transferIntentDigest,
      rootIdentity: this.binding.rootIdentity,
    } satisfies IntentRootBindingMetadata)
    await transactionCompletion(transaction)
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

  #intentBindingKey(): string {
    return `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0intent-root\0${this.binding.backend}\0${this.binding.transferIntentDigest}`
  }
}

export interface IndexedDbCleanupReport {
  readonly status: 'nothing-to-clean' | 'completed'
  readonly removed: number
}

/**
 * Resolve the stable identity for a picker-issued root.  The handle comparison
 * is performed before an identity is accepted; a name, path, or caller-supplied
 * label is never treated as proof of ownership.  The first open writes the
 * binding in the WindShare metadata store, and every later open verifies the
 * persisted handle with the browser's capability relation.
 */
export async function resolveIndexedDbRootIdentity(options: {
  readonly databaseName?: string
  readonly backend: string
  readonly root: FileSystemDirectoryHandle
}): Promise<OutputCapabilityIdentity> {
  const databaseName = options.databaseName ?? DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME
  const backend = requireText(options.backend, 'backend')
  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const rootName = requireRootName(options.root)
    for (let attempt = 0; attempt < ROOT_BINDING_INSTALL_RETRY_LIMIT; attempt += 1) {
      const existing = await readRootBindingBucket(database, backend, rootName)
      const matched = await matchingRootBinding(existing?.entries ?? [], options.root)
      if (matched !== undefined) return matched
      if (existing !== undefined && existing.entries.length >= ROOT_BINDING_BUCKET_LIMIT) {
        throw new DOMException('Too many output roots share this browser name', 'QuotaExceededError')
      }

      // A random identity is generated only after every existing binding has
      // failed the browser's same-entry check.  The persisted handle is the
      // durable proof used by future tabs; no lexical/path fallback is possible.
      const identity = createOutputCapabilityIdentity()
      const candidate: RootBindingEntry = {
        rootIdentity: encodeBase64Url(identity),
        handle: options.root,
      }
      if (!await installRootBindingEntry(database, backend, rootName, existing, candidate)) continue

      // Verify the committed handle before returning its identity.  The
      // optimistic revision check in installRootBindingEntry makes concurrent
      // tabs converge on one bucket revision rather than minting namespaces.
      const persisted = await readRootBindingBucket(database, backend, rootName)
      const verified = await matchingRootBinding(persisted?.entries ?? [], options.root)
      if (verified !== undefined) return verified
    }
    throw new DOMException('The output root identity could not be installed safely', 'InvalidStateError')
  } finally {
    database.close()
  }
}

async function readRootBindingBucket(
  database: IDBDatabase,
  backend: string,
  rootName: string,
): Promise<RootBindingMetadata | undefined> {
  const transaction = database.transaction(METADATA_STORE, 'readonly')
  const raw = await requestResult<unknown>(
    transaction.objectStore(METADATA_STORE).get(rootBindingKey(backend, rootName)),
  )
  await transactionCompletion(transaction)
  if (raw === undefined) return undefined
  if (!isRecord(raw)) throw new Error('IndexedDB root binding metadata is not an object')
  const bucket = raw as unknown as RootBindingMetadata
  validateRootBinding(bucket, backend, rootName)
  return bucket
}

async function matchingRootBinding(
  entries: readonly RootBindingEntry[],
  root: FileSystemDirectoryHandle,
): Promise<OutputCapabilityIdentity | undefined> {
  for (const entry of entries) {
    if (entry.handle.kind !== 'directory') {
      throw new Error('IndexedDB root binding is not a directory capability')
    }
    if (!await root.isSameEntry(entry.handle)) continue
    const identity = decodeBase64Url(entry.rootIdentity)
    if (identity === undefined) throw new Error('IndexedDB root binding identity is not canonical')
    return snapshotOutputCapabilityIdentity(identity, 'persisted root identity')
  }
  return undefined
}

async function installRootBindingEntry(
  database: IDBDatabase,
  backend: string,
  rootName: string,
  expected: RootBindingMetadata | undefined,
  candidate: RootBindingEntry,
): Promise<boolean> {
  const transaction = database.transaction(METADATA_STORE, 'readwrite')
  const store = transaction.objectStore(METADATA_STORE)
  const currentRaw = await requestResult<unknown>(store.get(rootBindingKey(backend, rootName)))
  if (currentRaw !== undefined && !isRecord(currentRaw)) {
    transaction.abort()
    throw new Error('IndexedDB root binding metadata is not an object')
  }
  const current = currentRaw === undefined ? undefined : currentRaw as unknown as RootBindingMetadata
  if (current !== undefined) validateRootBinding(current, backend, rootName)
  const expectedRevision = expected?.revision ?? 0
  const currentRevision = current?.revision ?? 0
  if (currentRevision !== expectedRevision) {
    await transactionCompletion(transaction)
    return false
  }
  const entries = current === undefined ? [] : [...current.entries]
  entries.push(candidate)
  const next: RootBindingMetadata = {
    id: rootBindingKey(backend, rootName),
    marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespaceName: FILE_CHECKPOINT_NAMESPACE,
    backend,
    rootName,
    revision: currentRevision + 1,
    entries: Object.freeze(entries),
  }
  store.put(next)
  await transactionCompletion(transaction)
  return true
}

function validateRootBinding(value: RootBindingMetadata, backend: string, rootName: string): void {
  if (value.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      value.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
      value.backend !== backend ||
      value.rootName !== rootName ||
      value.id !== rootBindingKey(backend, rootName) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1 ||
      !Array.isArray(value.entries) || value.entries.length > ROOT_BINDING_BUCKET_LIMIT) {
    throw new Error('IndexedDB root binding ownership is invalid')
  }
  for (const entry of value.entries) {
    const handle = (entry as unknown as { readonly handle?: FileSystemHandle }).handle
    if (!isRecord(entry) || typeof entry.rootIdentity !== 'string' ||
        handle === undefined || handle.kind !== 'directory') {
      throw new Error('IndexedDB root binding entry is invalid')
    }
    const identity = decodeBase64Url(entry.rootIdentity)
    if (identity === undefined) throw new Error('IndexedDB root binding identity is not canonical')
    snapshotOutputCapabilityIdentity(identity, 'persisted root identity')
  }
}

function rootBindingPrefix(backend: string): string {
  return `${ROOT_BINDING_PREFIX}${backend}\0`
}

function rootBindingKey(backend: string, rootName: string): string {
  return `${rootBindingPrefix(backend)}${encodeBase64Url(new TextEncoder().encode(rootName))}`
}

function requireRootName(root: FileSystemDirectoryHandle): string {
  if (typeof root.name !== 'string') throw new TypeError('output root handle has no stable name')
  return root.name
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
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

export async function openIndexedDbCheckpointDatabase(name: string): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') throw new DOMException('IndexedDB output checkpoints are unavailable', 'NotSupportedError')
  const request = indexedDB.open(name, CHECKPOINT_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () => {
    const database = request.result
    for (const storeName of [CANDIDATE_STORE, COMMITTED_STORE, HANDLE_STORE]) {
      if (!database.objectStoreNames.contains(storeName)) database.createObjectStore(storeName, { keyPath: 'id' }).createIndex(NAMESPACE_INDEX, 'namespace')
    }
    if (!database.objectStoreNames.contains(METADATA_STORE)) database.createObjectStore(METADATA_STORE, { keyPath: 'id' })
    if (!database.objectStoreNames.contains(CLEANUP_STORE)) database.createObjectStore(CLEANUP_STORE, { keyPath: 'id' })
  })
  let rejected = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => { rejected = true; reject(new DOMException('Output checkpoint database upgrade is blocked by another tab', 'InvalidStateError')) }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => { if (rejected) request.result.close(); else resolve(request.result) }, { once: true })
  })
}

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
function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => { request.addEventListener('success', () => resolve(request.result), { once: true }); request.addEventListener('error', () => reject(request.error), { once: true }) })
}
function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => { transaction.addEventListener('complete', () => resolve(), { once: true }); transaction.addEventListener('abort', () => reject(transaction.error), { once: true }); transaction.addEventListener('error', () => reject(transaction.error), { once: true }) })
}
function requireText(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${label} must not be empty`)
  }
  return value
}
function requireBindingIdentity(value: string | undefined, label: string): string {
  if (value === undefined || value.length === 0) {
    throw new TypeError(`IndexedDB FileCheckpointV1 requires an explicit ${label}`)
  }
  return value
}
