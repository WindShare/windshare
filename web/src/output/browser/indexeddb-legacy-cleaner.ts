import {
  INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
  INDEXEDDB_LEGACY_V5_STORES,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'
import { acquireBrowserCheckpointCleanupLease } from './session-lease'

const LEGACY_FILE_CHECKPOINT_MARKER = 'windshare/file-checkpoint/v1'
const LEGACY_FILE_CHECKPOINT_NAMESPACE = '.windshare-output/checkpoints-v1'
const LEGACY_CLEANUP_PROGRESS_ID = 'windshare/indexeddb-v5-cleanup/v1'
const LEGACY_CLEANUP_OPERATION_ID = 'AQEBAQEBAQEBAQEBAQEBAQ'
const LEGACY_CLEANUP_KIND = 1
const LEGACY_SCAN_BATCH = 64

export const INDEXEDDB_LEGACY_CLEANUP_ORDER = Object.freeze([
  'file-checkpoint-v1-candidates',
  'file-checkpoint-v1-committed',
  'file-checkpoint-v1-handles',
  'file-checkpoint-v1-cleanup',
  'paused-task-descriptor-v1',
  'root-capability-v1',
  'resume-state-discard-v1',
  'file-checkpoint-v1-metadata',
] as const satisfies readonly (typeof INDEXEDDB_LEGACY_V5_STORES)[number][])

export type IndexedDbLegacyStoreName = (typeof INDEXEDDB_LEGACY_CLEANUP_ORDER)[number]
export type LegacyOwnershipDecision = 'owned' | 'foreign' | 'unknown'

export interface IndexedDbLegacyRow {
  readonly key: string
  readonly value: unknown
}

export interface IndexedDbLegacyCleanupProgress {
  readonly id: typeof LEGACY_CLEANUP_PROGRESS_ID
  readonly operationId: typeof LEGACY_CLEANUP_OPERATION_ID
  readonly kind: typeof LEGACY_CLEANUP_KIND
  readonly schemaVersion: 1
  readonly storeIndex: number
  readonly afterKey?: string
  readonly removed: number
  readonly state: 'pending' | 'completed'
}

export interface IndexedDbLegacyCleanupPage {
  readonly rows: readonly IndexedDbLegacyRow[]
  readonly done: boolean
}

export interface IndexedDbLegacyCleanupDatabase {
  readOrInitializeProgress(initial: IndexedDbLegacyCleanupProgress): Promise<unknown>
  scan(
    storeName: IndexedDbLegacyStoreName,
    afterKey: string | undefined,
    limit: number,
  ): Promise<IndexedDbLegacyCleanupPage>
  certifyAndDelete(
    storeName: IndexedDbLegacyStoreName,
    row: IndexedDbLegacyRow,
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<LegacyOwnershipDecision>
  advanceStore(
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<void>
  close(): void
}

export interface IndexedDbLegacyCleanupPorts {
  acquireLease(): Promise<{ readonly release: () => Promise<void> }>
  openDatabase(): Promise<IndexedDbLegacyCleanupDatabase>
}

export type IndexedDbLegacyCleanupReport =
  | Readonly<{ status: 'nothing-to-clean' | 'completed'; removed: number }>
  | Readonly<{
      status: 'needs-attention'
      removed: number
      storeName: IndexedDbLegacyStoreName
      key: string
      decision: Exclude<LegacyOwnershipDecision, 'owned'>
    }>

const INITIAL_PROGRESS: IndexedDbLegacyCleanupProgress = Object.freeze({
  id: LEGACY_CLEANUP_PROGRESS_ID,
  operationId: LEGACY_CLEANUP_OPERATION_ID,
  kind: LEGACY_CLEANUP_KIND,
  schemaVersion: 1,
  storeIndex: 0,
  removed: 0,
  state: 'pending',
})

/**
 * Cleanup advances in the same transaction that deletes one certified row.
 * Every crash cut therefore replays only an observation, never an unrecorded delete.
 */
export async function cleanIndexedDbLegacyStores(
  ports: IndexedDbLegacyCleanupPorts,
): Promise<IndexedDbLegacyCleanupReport> {
  const lease = await ports.acquireLease()
  let database: IndexedDbLegacyCleanupDatabase | undefined
  try {
    database = await ports.openDatabase()
    let progress = snapshotProgress(
      await database.readOrInitializeProgress(INITIAL_PROGRESS),
    )
    if (progress.state === 'completed') {
      return Object.freeze({ status: 'nothing-to-clean', removed: 0 })
    }

    const removedBefore = progress.removed
    while (progress.state === 'pending') {
      const storeName = INDEXEDDB_LEGACY_CLEANUP_ORDER[progress.storeIndex]
      if (storeName === undefined) {
        throw new Error('legacy cleanup progress escaped its store plan')
      }
      const page = await database.scan(storeName, progress.afterKey, LEGACY_SCAN_BATCH)
      if (page.rows.length === 0) {
        if (!page.done) throw new Error('legacy cleanup scan made no progress')
        const next = nextStoreProgress(progress)
        await database.advanceStore(progress, next)
        progress = next
        continue
      }

      for (const row of page.rows) {
        const next = nextRowProgress(progress, row.key)
        const decision = await database.certifyAndDelete(storeName, row, progress, next)
        if (decision !== 'owned') {
          return Object.freeze({
            status: 'needs-attention',
            removed: progress.removed - removedBefore,
            storeName,
            key: row.key,
            decision,
          })
        }
        progress = next
      }
    }

    const removed = progress.removed - removedBefore
    return Object.freeze({
      status: removed === 0 ? 'nothing-to-clean' : 'completed',
      removed,
    })
  } finally {
    try {
      database?.close()
    } finally {
      await lease.release()
    }
  }
}

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

  async readOrInitializeProgress(initial: IndexedDbLegacyCleanupProgress): Promise<unknown> {
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE)
    const existing = await requestResult<unknown>(store.get(LEGACY_CLEANUP_PROGRESS_ID))
    if (existing === undefined) store.put(initial)
    await transactionCompletion(transaction)
    return existing ?? initial
  }

  async scan(
    storeName: IndexedDbLegacyStoreName,
    afterKey: string | undefined,
    limit: number,
  ): Promise<IndexedDbLegacyCleanupPage> {
    if (!this.#database.objectStoreNames.contains(storeName)) {
      return Object.freeze({ rows: Object.freeze([]), done: true })
    }
    const transaction = this.#database.transaction(storeName, 'readonly')
    const rows = await scanLegacyRows(
      transaction.objectStore(storeName),
      afterKey,
      limit + 1,
    )
    await transactionCompletion(transaction)
    return Object.freeze({
      rows: Object.freeze(rows.slice(0, limit)),
      done: rows.length <= limit,
    })
  }

  async certifyAndDelete(
    storeName: IndexedDbLegacyStoreName,
    row: IndexedDbLegacyRow,
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<LegacyOwnershipDecision> {
    if (!this.#database.objectStoreNames.contains(storeName)) return 'unknown'
    const stores = storeName === 'file-checkpoint-v1-metadata'
      ? [INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, storeName]
      : [
          INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
          storeName,
          'file-checkpoint-v1-metadata',
        ].filter((name) => this.#database.objectStoreNames.contains(name))
    const transaction = this.#database.transaction(stores, 'readwrite')
    const control = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE)
    const currentProgress = snapshotProgress(await requestResult<unknown>(
      control.get(LEGACY_CLEANUP_PROGRESS_ID),
    ))
    if (!sameProgress(currentProgress, expected)) {
      transaction.abort()
      throw new Error('legacy cleanup progress changed outside its lease')
    }
    const currentRow = await requestResult<unknown>(
      transaction.objectStore(storeName).get(row.key),
    )
    if (currentRow === undefined) {
      transaction.abort()
      throw new Error('legacy cleanup row changed outside its lease')
    }
    const decision = await certifyLegacyOwnership(
      transaction,
      storeName,
      row.key,
      currentRow,
    )
    if (decision === 'owned') {
      transaction.objectStore(storeName).delete(row.key)
      control.put(next)
    }
    await transactionCompletion(transaction)
    return decision
  }

  async advanceStore(
    expected: IndexedDbLegacyCleanupProgress,
    next: IndexedDbLegacyCleanupProgress,
  ): Promise<void> {
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
      'readwrite',
    )
    const store = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE)
    const current = snapshotProgress(await requestResult<unknown>(
      store.get(LEGACY_CLEANUP_PROGRESS_ID),
    ))
    if (!sameProgress(current, expected)) {
      transaction.abort()
      throw new Error('legacy cleanup progress changed outside its lease')
    }
    store.put(next)
    await transactionCompletion(transaction)
  }

  close(): void {
    this.#database.close()
  }
}

function nextRowProgress(
  progress: IndexedDbLegacyCleanupProgress,
  key: string,
): IndexedDbLegacyCleanupProgress {
  return Object.freeze({
    ...progress,
    afterKey: requireKey(key),
    removed: checkedIncrement(progress.removed),
  })
}

function nextStoreProgress(
  progress: IndexedDbLegacyCleanupProgress,
): IndexedDbLegacyCleanupProgress {
  const storeIndex = progress.storeIndex + 1
  return Object.freeze({
    id: progress.id,
    operationId: progress.operationId,
    kind: progress.kind,
    schemaVersion: progress.schemaVersion,
    storeIndex,
    removed: progress.removed,
    state: storeIndex === INDEXEDDB_LEGACY_CLEANUP_ORDER.length ? 'completed' : 'pending',
  })
}

function snapshotProgress(value: unknown): IndexedDbLegacyCleanupProgress {
  if (!isRecord(value) ||
      value.id !== LEGACY_CLEANUP_PROGRESS_ID ||
      value.operationId !== LEGACY_CLEANUP_OPERATION_ID ||
      value.kind !== LEGACY_CLEANUP_KIND ||
      value.schemaVersion !== 1 ||
      !Number.isSafeInteger(value.storeIndex) ||
      (value.storeIndex as number) < 0 ||
      (value.storeIndex as number) > INDEXEDDB_LEGACY_CLEANUP_ORDER.length ||
      !Number.isSafeInteger(value.removed) ||
      (value.removed as number) < 0 ||
      (value.state !== 'pending' && value.state !== 'completed') ||
      (value.state === 'completed') !==
        (value.storeIndex === INDEXEDDB_LEGACY_CLEANUP_ORDER.length) ||
      (value.afterKey !== undefined && typeof value.afterKey !== 'string') ||
      (value.storeIndex === INDEXEDDB_LEGACY_CLEANUP_ORDER.length &&
        value.afterKey !== undefined)) {
    throw new Error('legacy cleanup progress has unknown ownership or shape')
  }
  return Object.freeze({
    id: LEGACY_CLEANUP_PROGRESS_ID,
    operationId: LEGACY_CLEANUP_OPERATION_ID,
    kind: LEGACY_CLEANUP_KIND,
    schemaVersion: 1,
    storeIndex: value.storeIndex as number,
    ...(value.afterKey === undefined ? {} : { afterKey: requireKey(value.afterKey) }),
    removed: value.removed as number,
    state: value.state,
  })
}

async function certifyLegacyOwnership(
  transaction: IDBTransaction,
  storeName: IndexedDbLegacyStoreName,
  key: string,
  value: unknown,
): Promise<LegacyOwnershipDecision> {
  if (storeName === 'file-checkpoint-v1-metadata') {
    return ownedMetadata(value, key) ? 'owned' : 'unknown'
  }
  if (!isRecord(value)) return 'unknown'
  const namespace = legacyRowNamespace(storeName, value)
  if (namespace === undefined || namespace.length === 0 ||
      !transaction.objectStoreNames.contains('file-checkpoint-v1-metadata')) {
    return 'unknown'
  }
  const metadata = await requestResult<unknown>(
    transaction.objectStore('file-checkpoint-v1-metadata').get(namespace),
  )
  if (metadata === undefined) return 'unknown'
  return ownedMetadata(metadata, namespace) ? 'owned' : 'foreign'
}

function legacyRowNamespace(
  storeName: IndexedDbLegacyStoreName,
  value: Record<string, unknown>,
): string | undefined {
  if (typeof value.namespace === 'string') return value.namespace
  if (storeName === 'paused-task-descriptor-v1' && typeof value.id === 'string') {
    return value.id
  }
  return undefined
}

function ownedMetadata(value: unknown, expectedId: string): boolean {
  return isRecord(value) &&
    value.id === expectedId &&
    value.marker === LEGACY_FILE_CHECKPOINT_MARKER &&
    value.namespaceName === LEGACY_FILE_CHECKPOINT_NAMESPACE
}

function scanLegacyRows(
  store: IDBObjectStore,
  afterKey: string | undefined,
  limit: number,
): Promise<IndexedDbLegacyRow[]> {
  return new Promise((resolve, reject) => {
    const rows: IndexedDbLegacyRow[] = []
    const request = store.openCursor(
      afterKey === undefined ? undefined : IDBKeyRange.lowerBound(afterKey, true),
    )
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || rows.length === limit) {
        resolve(rows)
        return
      }
      if (typeof cursor.primaryKey !== 'string') {
        reject(new TypeError('legacy cleanup encountered a non-text key'))
        return
      }
      rows.push(Object.freeze({ key: cursor.primaryKey, value: cursor.value }))
      cursor.continue()
    })
  })
}

function sameProgress(
  left: IndexedDbLegacyCleanupProgress,
  right: IndexedDbLegacyCleanupProgress,
): boolean {
  return left.id === right.id &&
    left.operationId === right.operationId &&
    left.kind === right.kind &&
    left.schemaVersion === right.schemaVersion &&
    left.storeIndex === right.storeIndex &&
    left.afterKey === right.afterKey &&
    left.removed === right.removed &&
    left.state === right.state
}

function checkedIncrement(value: number): number {
  if (!Number.isSafeInteger(value) || value >= Number.MAX_SAFE_INTEGER) {
    throw new Error('legacy cleanup removed-count overflow')
  }
  return value + 1
}

function requireKey(value: string): string {
  if (value.length === 0) throw new TypeError('legacy cleanup key must not be empty')
  return value
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
