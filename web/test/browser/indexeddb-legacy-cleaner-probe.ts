import {
  ensureOneShotIndexedDbLegacyCleanup,
  INDEXEDDB_LEGACY_CLEANUP_ORDER,
  type IndexedDbLegacyCleanupReport,
} from '../../src/output/browser/indexeddb-legacy-cleaner'
import {
  CHECKPOINT_DATABASE_VERSION,
  INDEXEDDB_LEGACY_V5_STORES,
  INDEXEDDB_V9_STORE_SCHEMAS,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../../src/output/browser/indexeddb-database'

const LEGACY_STORE_NAMES = INDEXEDDB_LEGACY_CLEANUP_ORDER
const CURRENT_STORE_SCHEMAS = INDEXEDDB_V9_STORE_SCHEMAS
const CURRENT_STORE_NAMES = Object.freeze(CURRENT_STORE_SCHEMAS.map(({ name }) => name))
const RECORDS_PER_LEGACY_STORE = 2
const LEGACY_FILE_CHECKPOINT_MARKER = 'windshare/file-checkpoint/v1'
const LEGACY_FILE_CHECKPOINT_NAMESPACE = '.windshare-output/checkpoints-v1'
const PUBLISHED_SENTINEL_BYTES = Uint8Array.of(17, 34, 51, 68)

export interface IndexedDbLegacyCleanupIsolationProbe {
  readonly first: IndexedDbLegacyCleanupReport
  readonly second: IndexedDbLegacyCleanupReport
  readonly legacyCounts: readonly number[]
  readonly currentStoreCount: number
  readonly currentSentinelsPreserved: boolean
  readonly publishedSentinelBytes: readonly number[]
}

export async function probeIndexedDbLegacyCleanupIsolation(
  databaseName: string,
): Promise<IndexedDbLegacyCleanupIsolationProbe> {
  const root = await navigator.storage.getDirectory()
  const publishedName = `legacy-cleaner-published-${crypto.randomUUID()}`
  try {
    await seedIndexedDbLegacyCleanup(databaseName)
    await writePublishedSentinel(root, publishedName)
    const first = await ensureOneShotIndexedDbLegacyCleanup(databaseName)
    const legacyCounts = await legacyStoreCounts(databaseName)
    const currentSentinelsPresent = await currentStoreSentinels(databaseName)
    const publishedSentinelBytes = await readPublishedSentinel(root, publishedName)
    const second = await ensureOneShotIndexedDbLegacyCleanup(databaseName)
    return {
      first,
      second,
      legacyCounts,
      currentStoreCount: currentSentinelsPresent.length,
      currentSentinelsPreserved: currentSentinelsPresent.every(Boolean),
      publishedSentinelBytes,
    }
  } finally {
    await root.removeEntry(publishedName).catch(() => undefined)
    await deleteIndexedDbLegacyCleanupDatabase(databaseName)
  }
}

export async function seedIndexedDbLegacyCleanup(databaseName: string): Promise<void> {
  const legacy = await openDatabase(databaseName, CHECKPOINT_DATABASE_VERSION - 1, (database) => {
    for (const storeName of INDEXEDDB_LEGACY_V5_STORES) {
      database.createObjectStore(storeName, { keyPath: 'id' })
    }
  })
  legacy.close()

  const database = await openIndexedDbCheckpointDatabase(databaseName)
  try {
    const transaction = database.transaction(
      [...LEGACY_STORE_NAMES, ...CURRENT_STORE_NAMES],
      'readwrite',
    )
    for (const storeName of LEGACY_STORE_NAMES) {
      for (let index = 0; index < RECORDS_PER_LEGACY_STORE; index += 1) {
        transaction.objectStore(storeName).put(ownedLegacyRow(storeName, index))
      }
    }
    for (const schema of CURRENT_STORE_SCHEMAS) {
      transaction.objectStore(schema.name).put(currentSentinel(schema.name, schema.keyPath))
    }
    await transactionCompletion(transaction)
  } finally {
    database.close()
  }
}

export function runIndexedDbLegacyCleanup(
  databaseName: string,
): Promise<IndexedDbLegacyCleanupReport> {
  return ensureOneShotIndexedDbLegacyCleanup(databaseName)
}

export async function legacyStoreCounts(databaseName: string): Promise<readonly number[]> {
  const database = await openDatabase(databaseName)
  try {
    const transaction = database.transaction(LEGACY_STORE_NAMES, 'readonly')
    const counts = await Promise.all(
      LEGACY_STORE_NAMES.map((storeName) => requestResult(
        transaction.objectStore(storeName).count(),
      )),
    )
    await transactionCompletion(transaction)
    return counts
  } finally {
    database.close()
  }
}

export function deleteIndexedDbLegacyCleanupDatabase(databaseName: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(databaseName)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('blocked', () => reject(
      new DOMException('Legacy cleanup test database deletion was blocked', 'InvalidStateError'),
    ), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

async function currentStoreSentinels(databaseName: string): Promise<readonly boolean[]> {
  const database = await openDatabase(databaseName)
  try {
    const transaction = database.transaction(CURRENT_STORE_NAMES, 'readonly')
    const records = await Promise.all(CURRENT_STORE_NAMES.map((storeName) => requestResult<unknown>(
      transaction.objectStore(storeName).get(currentSentinelKey(storeName)),
    )))
    await transactionCompletion(transaction)
    return records.map((record, index) => {
      const storeName = CURRENT_STORE_NAMES[index]
      return typeof record === 'object' && record !== null &&
        'sentinel' in record && record.sentinel === storeName
    })
  } finally {
    database.close()
  }
}

function currentSentinel(storeName: string, keyPath: string): Readonly<Record<string, string>> {
  return Object.freeze({
    [keyPath]: currentSentinelKey(storeName),
    sentinel: storeName,
  })
}

function currentSentinelKey(storeName: string): string {
  return `current-sentinel:${storeName}`
}

function ownedLegacyRow(
  storeName: (typeof LEGACY_STORE_NAMES)[number],
  index: number,
): Readonly<Record<string, unknown>> {
  const namespace = `windshare/test/legacy-owned-namespace/${index}`
  if (storeName === 'file-checkpoint-v1-metadata') {
    return Object.freeze({
      id: namespace,
      marker: LEGACY_FILE_CHECKPOINT_MARKER,
      namespaceName: LEGACY_FILE_CHECKPOINT_NAMESPACE,
    })
  }
  return Object.freeze({
    id: `${storeName}-owned-${index}`,
    namespace,
  })
}

async function writePublishedSentinel(
  root: FileSystemDirectoryHandle,
  name: string,
): Promise<void> {
  const handle = await root.getFileHandle(name, { create: true })
  const writer = await handle.createWritable()
  await writer.write(PUBLISHED_SENTINEL_BYTES)
  await writer.close()
}

async function readPublishedSentinel(
  root: FileSystemDirectoryHandle,
  name: string,
): Promise<readonly number[]> {
  const handle = await root.getFileHandle(name)
  const file = await handle.getFile()
  return [...new Uint8Array(await file.arrayBuffer())]
}

function openDatabase(
  name: string,
  version?: number,
  upgrade?: (database: IDBDatabase) => void,
): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = version === undefined ? indexedDB.open(name) : indexedDB.open(name, version)
    request.addEventListener('upgradeneeded', () => upgrade?.(request.result), { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}
