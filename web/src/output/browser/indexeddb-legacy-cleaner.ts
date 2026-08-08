import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
} from '../persistence/checkpoint'
import {
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  openIndexedDbCheckpointDatabase,
} from './indexeddb-repository'
import { acquireBrowserCheckpointCleanupLease } from './session-lease'

const LEGACY_STORE_NAMES = [
  'checkpoint-candidates',
  'checkpoint-committed',
  'persistent-handles',
  'cleanup-markers',
] as const
const LEGACY_CLEANUP_SOURCE_VERSION = 2
const LEGACY_CLEANUP_METADATA_ID =
  `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0indexeddb-v2-cleanup`
const LEGACY_CLEANUP_METADATA_KEYS = [
  'id',
  'marker',
  'namespaceName',
  'sourceVersion',
  'step',
  'state',
  'removed',
] as const

export interface IndexedDbLegacyCleanupReport {
  readonly status: 'nothing-to-clean' | 'completed'
  readonly removed: number
}

export interface IndexedDbLegacyCleanupMetadata {
  readonly id: typeof LEGACY_CLEANUP_METADATA_ID
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly sourceVersion: typeof LEGACY_CLEANUP_SOURCE_VERSION
  readonly step: number
  readonly state: 'pending' | 'completed'
  readonly removed: number
}

export interface IndexedDbLegacyCleanupTransition {
  readonly step: number
  readonly state: IndexedDbLegacyCleanupMetadata['state']
}

/**
 * The transaction port keeps fault injection out of the production algorithm.
 * Implementations must atomically clear the named store and persist `transition`.
 */
export interface IndexedDbLegacyCleanupDatabase {
  readOrInitializeMetadata(initial: IndexedDbLegacyCleanupMetadata): Promise<unknown>
  clearLegacyStore(
    storeName: string,
    expected: IndexedDbLegacyCleanupMetadata,
    transition: IndexedDbLegacyCleanupTransition,
  ): Promise<unknown>
  close(): void
}

export interface IndexedDbLegacyCleanupPorts {
  acquireLease(): Promise<{ readonly release: () => Promise<void> }>
  openDatabase(): Promise<IndexedDbLegacyCleanupDatabase>
}

const INITIAL_LEGACY_CLEANUP_METADATA = Object.freeze({
  id: LEGACY_CLEANUP_METADATA_ID,
  marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
  namespaceName: FILE_CHECKPOINT_NAMESPACE,
  sourceVersion: LEGACY_CLEANUP_SOURCE_VERSION,
  step: 0,
  state: 'pending',
  removed: 0,
} satisfies IndexedDbLegacyCleanupMetadata)

/** Clears only the four app-owned legacy stores; current state is not an input. */
export async function cleanIndexedDbLegacyStores(
  ports: IndexedDbLegacyCleanupPorts,
): Promise<IndexedDbLegacyCleanupReport> {
  const lease = await ports.acquireLease()
  let database: IndexedDbLegacyCleanupDatabase | undefined
  try {
    database = await ports.openDatabase()
    let metadata = snapshotMetadata(
      await database.readOrInitializeMetadata(INITIAL_LEGACY_CLEANUP_METADATA),
    )
    if (metadata.state === 'completed') {
      return Object.freeze({ status: 'nothing-to-clean', removed: 0 })
    }

    const removedBefore = metadata.removed
    while (metadata.step < LEGACY_STORE_NAMES.length) {
      const storeName = LEGACY_STORE_NAMES[metadata.step]
      if (storeName === undefined) throw new Error('legacy cleanup step exceeds its store plan')
      const transition = nextTransition(metadata)
      const next = snapshotMetadata(
        await database.clearLegacyStore(storeName, metadata, transition),
      )
      assertTransition(metadata, transition, next)
      metadata = next
    }
    return Object.freeze({
      status: metadata.removed === removedBefore ? 'nothing-to-clean' : 'completed',
      removed: metadata.removed - removedBefore,
    })
  } finally {
    try {
      database?.close()
    } finally {
      await lease.release()
    }
  }
}

/** Production composition supplies the only database opener used by the cleaner. */
export function ensureOneShotIndexedDbLegacyCleanup(
  databaseName: string,
): Promise<IndexedDbLegacyCleanupReport> {
  return cleanIndexedDbLegacyStores({
    acquireLease: () => acquireBrowserCheckpointCleanupLease(databaseName),
    openDatabase: async () => new BrowserLegacyCleanupDatabase(
      await openIndexedDbCheckpointDatabase(databaseName),
    ),
  })
}

class BrowserLegacyCleanupDatabase implements IndexedDbLegacyCleanupDatabase {
  readonly #database: IDBDatabase

  constructor(database: IDBDatabase) {
    this.#database = database
  }

  async readOrInitializeMetadata(
    initial: IndexedDbLegacyCleanupMetadata,
  ): Promise<unknown> {
    const transaction = this.#database.transaction(
      INDEXEDDB_CHECKPOINT_METADATA_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE)
    const existing = await requestResult<unknown>(store.get(LEGACY_CLEANUP_METADATA_ID))
    if (existing === undefined) store.put(initial)
    await transactionCompletion(transaction)
    return existing ?? initial
  }

  async clearLegacyStore(
    storeName: string,
    expected: IndexedDbLegacyCleanupMetadata,
    transition: IndexedDbLegacyCleanupTransition,
  ): Promise<unknown> {
    if (!isLegacyStoreName(storeName)) throw new Error('legacy cleanup requested an unknown store')
    const hasLegacyStore = this.#database.objectStoreNames.contains(storeName)
    const transaction = this.#database.transaction(
      hasLegacyStore
        ? [INDEXEDDB_CHECKPOINT_METADATA_STORE, storeName]
        : INDEXEDDB_CHECKPOINT_METADATA_STORE,
      'readwrite',
    )
    const metadataStore = transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE)
    const currentValue = await requestResult<unknown>(metadataStore.get(LEGACY_CLEANUP_METADATA_ID))
    if (currentValue === undefined) {
      transaction.abort()
      throw new Error('legacy cleanup metadata disappeared during cleanup')
    }
    const current = snapshotMetadata(currentValue)
    if (!sameMetadata(current, expected)) {
      transaction.abort()
      throw new Error('legacy cleanup metadata changed outside its exclusive lease')
    }

    const removed = hasLegacyStore
      ? await requestResult<number>(transaction.objectStore(storeName).count())
      : 0
    if (hasLegacyStore) transaction.objectStore(storeName).clear()
    const next = Object.freeze({
      ...current,
      ...transition,
      removed: current.removed + removed,
    }) satisfies IndexedDbLegacyCleanupMetadata
    // Progress shares the clearing transaction so every observable cut is restartable.
    metadataStore.put(next)
    await transactionCompletion(transaction)
    return next
  }

  close(): void {
    this.#database.close()
  }
}

function nextTransition(
  metadata: IndexedDbLegacyCleanupMetadata,
): IndexedDbLegacyCleanupTransition {
  const step = metadata.step + 1
  return Object.freeze({
    step,
    state: step === LEGACY_STORE_NAMES.length ? 'completed' : 'pending',
  })
}

function assertTransition(
  previous: IndexedDbLegacyCleanupMetadata,
  expected: IndexedDbLegacyCleanupTransition,
  next: IndexedDbLegacyCleanupMetadata,
): void {
  if (next.step !== expected.step || next.state !== expected.state ||
      next.removed < previous.removed) {
    throw new Error('legacy cleanup database returned an invalid progress transition')
  }
}

function snapshotMetadata(value: unknown): IndexedDbLegacyCleanupMetadata {
  if (!isRecord(value) || !hasExactKeys(value, LEGACY_CLEANUP_METADATA_KEYS) ||
      value.id !== LEGACY_CLEANUP_METADATA_ID ||
      value.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      value.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
      value.sourceVersion !== LEGACY_CLEANUP_SOURCE_VERSION ||
      !Number.isSafeInteger(value.step) || (value.step as number) < 0 ||
      (value.step as number) > LEGACY_STORE_NAMES.length ||
      !Number.isSafeInteger(value.removed) || (value.removed as number) < 0 ||
      (value.state !== 'pending' && value.state !== 'completed') ||
      (value.state === 'completed') !== (value.step === LEGACY_STORE_NAMES.length)) {
    throw new Error('legacy checkpoint cleanup metadata has unknown ownership or progress')
  }
  return Object.freeze({
    id: value.id,
    marker: value.marker,
    namespaceName: value.namespaceName,
    sourceVersion: value.sourceVersion,
    step: value.step as number,
    state: value.state,
    removed: value.removed as number,
  })
}

function sameMetadata(
  left: IndexedDbLegacyCleanupMetadata,
  right: IndexedDbLegacyCleanupMetadata,
): boolean {
  return LEGACY_CLEANUP_METADATA_KEYS.every((key) => left[key] === right[key])
}

function isLegacyStoreName(value: string): value is (typeof LEGACY_STORE_NAMES)[number] {
  return LEGACY_STORE_NAMES.some((storeName) => storeName === value)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  const actual = Object.keys(value)
  return actual.length === keys.length && keys.every((key) => Object.hasOwn(value, key))
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
