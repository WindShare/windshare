import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
} from '../persistence/checkpoint'
import {
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  IndexedDbOutputRepository,
  openIndexedDbCheckpointDatabase,
  type IndexedDbCheckpointBinding,
  type IndexedDbCleanupReport,
} from './indexeddb-repository'
import {
  acquireBrowserCheckpointCleanupLease,
  acquireBrowserOutputSessionLease,
} from './session-lease'

const LEGACY_STORE_NAMES = [
  'checkpoint-candidates',
  'checkpoint-committed',
  'persistent-handles',
  'cleanup-markers',
] as const
const LEGACY_CLEANUP_METADATA_ID =
  `${FILE_CHECKPOINT_OWNERSHIP_MARKER}\0${FILE_CHECKPOINT_NAMESPACE}\0indexeddb-v2-cleanup`

interface LegacyCleanupMetadata {
  readonly id: typeof LEGACY_CLEANUP_METADATA_ID
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespaceName: typeof FILE_CHECKPOINT_NAMESPACE
  readonly sourceVersion: 2
  readonly step: number
  readonly state: 'pending' | 'completed'
  readonly removed: number
}

/** Clears only app-owned legacy stores; published filesystem objects are untouched. */
export async function ensureOneShotIndexedDbLegacyCleanup(
  databaseName: string,
): Promise<IndexedDbCleanupReport> {
  const lease = await acquireBrowserCheckpointCleanupLease(databaseName)
  let database: IDBDatabase | undefined
  try {
    database = await openIndexedDbCheckpointDatabase(databaseName)
    let metadata = await readOrInitializeMetadata(database)
    if (metadata.state === 'completed') {
      return Object.freeze({ status: 'nothing-to-clean', removed: 0 })
    }
    const removedBefore = metadata.removed
    while (metadata.step < LEGACY_STORE_NAMES.length) {
      metadata = await clearLegacyStore(database, metadata)
    }
    return Object.freeze({
      status: metadata.removed === removedBefore ? 'nothing-to-clean' : 'completed',
      removed: metadata.removed - removedBefore,
    })
  } finally {
    database?.close()
    await lease.release()
  }
}

/** Explicit destructive entrypoint for a V1 namespace plus the global legacy cleanup. */
export async function runOneShotIndexedDbCheckpointCleanup(options: {
  readonly databaseName: string
  readonly backend: string
  readonly outputSessionId?: string
  readonly binding: IndexedDbCheckpointBinding
}): Promise<IndexedDbCleanupReport> {
  const legacy = await ensureOneShotIndexedDbLegacyCleanup(options.databaseName)
  const repository = await IndexedDbOutputRepository.open(
    options.databaseName,
    options.backend,
    options.outputSessionId ?? `cleanup-${crypto.randomUUID?.() ?? Date.now().toString(36)}`,
    options.binding,
  )
  let lease: Awaited<ReturnType<typeof acquireBrowserOutputSessionLease>> | undefined
  try {
    lease = await acquireBrowserOutputSessionLease(repository.binding)
    const current = await repository.runOwnedCleanup()
    const removed = legacy.removed + current.removed
    return Object.freeze({ status: removed === 0 ? 'nothing-to-clean' : 'completed', removed })
  } finally {
    repository.close()
    await lease?.release()
  }
}

async function readOrInitializeMetadata(database: IDBDatabase): Promise<LegacyCleanupMetadata> {
  const transaction = database.transaction(INDEXEDDB_CHECKPOINT_METADATA_STORE, 'readwrite')
  const store = transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE)
  const existing = await requestResult<LegacyCleanupMetadata | undefined>(
    store.get(LEGACY_CLEANUP_METADATA_ID),
  )
  const metadata = existing ?? {
    id: LEGACY_CLEANUP_METADATA_ID,
    marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespaceName: FILE_CHECKPOINT_NAMESPACE,
    sourceVersion: 2,
    step: 0,
    state: 'pending',
    removed: 0,
  } satisfies LegacyCleanupMetadata
  validateMetadata(metadata)
  if (existing === undefined) store.put(metadata)
  await transactionCompletion(transaction)
  return metadata
}

async function clearLegacyStore(
  database: IDBDatabase,
  expected: LegacyCleanupMetadata,
): Promise<LegacyCleanupMetadata> {
  const legacyStore = LEGACY_STORE_NAMES[expected.step]
  if (legacyStore === undefined) throw new Error('legacy cleanup step exceeds its store plan')
  const hasLegacyStore = database.objectStoreNames.contains(legacyStore)
  const transaction = database.transaction(
    hasLegacyStore
      ? [INDEXEDDB_CHECKPOINT_METADATA_STORE, legacyStore]
      : INDEXEDDB_CHECKPOINT_METADATA_STORE,
    'readwrite',
  )
  const metadataStore = transaction.objectStore(INDEXEDDB_CHECKPOINT_METADATA_STORE)
  const current = await requestResult<LegacyCleanupMetadata | undefined>(
    metadataStore.get(LEGACY_CLEANUP_METADATA_ID),
  )
  if (current === undefined) {
    transaction.abort()
    throw new Error('legacy cleanup metadata disappeared during cleanup')
  }
  validateMetadata(current)
  if (current.step !== expected.step || current.removed !== expected.removed || current.state !== 'pending') {
    transaction.abort()
    throw new Error('legacy cleanup metadata changed outside its exclusive lease')
  }
  const removed = hasLegacyStore
    ? await requestResult<number>(transaction.objectStore(legacyStore).count())
    : 0
  if (hasLegacyStore) transaction.objectStore(legacyStore).clear()
  const step = current.step + 1
  const next: LegacyCleanupMetadata = {
    ...current,
    step,
    state: step === LEGACY_STORE_NAMES.length ? 'completed' : 'pending',
    removed: current.removed + removed,
  }
  metadataStore.put(next)
  await transactionCompletion(transaction)
  return next
}

function validateMetadata(metadata: LegacyCleanupMetadata): void {
  if (metadata.id !== LEGACY_CLEANUP_METADATA_ID ||
      metadata.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      metadata.namespaceName !== FILE_CHECKPOINT_NAMESPACE ||
      metadata.sourceVersion !== 2 ||
      !Number.isSafeInteger(metadata.step) || metadata.step < 0 ||
      metadata.step > LEGACY_STORE_NAMES.length ||
      !Number.isSafeInteger(metadata.removed) || metadata.removed < 0 ||
      (metadata.state !== 'pending' && metadata.state !== 'completed') ||
      (metadata.state === 'completed') !== (metadata.step === LEGACY_STORE_NAMES.length)) {
    throw new Error('legacy checkpoint cleanup metadata has unknown ownership or progress')
  }
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
