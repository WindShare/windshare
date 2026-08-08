import {
  ensureOneShotIndexedDbLegacyCleanup,
  type IndexedDbLegacyCleanupReport,
} from '../../src/output/browser/indexeddb-legacy-cleaner'
import { openIndexedDbCheckpointDatabase } from '../../src/output/browser/indexeddb-repository'

const LEGACY_STORE_NAMES = [
  'checkpoint-candidates',
  'checkpoint-committed',
  'persistent-handles',
  'cleanup-markers',
] as const
const CURRENT_STORE_NAMES = [
  'file-checkpoint-v1-candidates',
  'file-checkpoint-v1-committed',
  'file-checkpoint-v1-handles',
  'file-checkpoint-v1-metadata',
  'file-checkpoint-v1-cleanup',
  'paused-task-descriptor-v1',
  'root-capability-v1',
] as const
const RECORDS_PER_LEGACY_STORE = 2
const PUBLISHED_SENTINEL_BYTES = Uint8Array.of(17, 34, 51, 68)

export interface IndexedDbLegacyCleanupIsolationProbe {
  readonly first: IndexedDbLegacyCleanupReport
  readonly second: IndexedDbLegacyCleanupReport
  readonly legacyCounts: readonly number[]
  readonly currentSentinelsPresent: readonly boolean[]
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
      currentSentinelsPresent,
      publishedSentinelBytes,
    }
  } finally {
    await root.removeEntry(publishedName).catch(() => undefined)
    await deleteIndexedDbLegacyCleanupDatabase(databaseName)
  }
}

export async function seedIndexedDbLegacyCleanup(databaseName: string): Promise<void> {
  const legacy = await openDatabase(databaseName, 2, (database) => {
    for (const storeName of LEGACY_STORE_NAMES) {
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
        transaction.objectStore(storeName).put({
          id: `${storeName}-opaque-${index}`,
          // These records deliberately have no legacy schema the cleaner could decode.
          arbitrary: [storeName.length, index, { nested: true }],
        })
      }
    }
    for (const storeName of CURRENT_STORE_NAMES) {
      transaction.objectStore(storeName).put(currentSentinel(storeName))
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
      transaction.objectStore(storeName).get(currentSentinel(storeName).id),
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

function currentSentinel(storeName: (typeof CURRENT_STORE_NAMES)[number]): {
  readonly id: string
  readonly sentinel: string
} {
  return Object.freeze({ id: `current-sentinel:${storeName}`, sentinel: storeName })
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

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}
