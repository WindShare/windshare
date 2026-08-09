import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  CHECKPOINT_DATABASE_VERSION,
  INDEXEDDB_V6_STORE_SCHEMAS,
} from '../../src/output/browser/indexeddb-database'
import {
  IndexedDbReceiveOperationRepository,
} from '../../src/output/browser/indexeddb-repository'

const LEGACY_PAUSED_STORE = 'paused-task-descriptor-v1'

export interface IndexedDbV6Probe {
  readonly blockedUpgrade: string
  readonly blockedRequestClosedLate: boolean
  readonly versionChange: string
  readonly schemaVersion: number
  readonly v6StoresPresent: boolean
  readonly legacyStoreRetainedForCleanup: boolean
  readonly legacyRowsVisibleToV6: boolean
}

export async function probeIndexedDbV6Replacement(): Promise<IndexedDbV6Probe> {
  const blocked = await probeBlockedUpgrade()
  const versionChange = await probeVersionChange()
  const replacement = await probeReplacement()
  return Object.freeze({
    blockedUpgrade: blocked.rejection,
    blockedRequestClosedLate: blocked.deleted,
    versionChange,
    ...replacement,
  })
}

async function probeBlockedUpgrade(): Promise<{
  readonly rejection: string
  readonly deleted: boolean
}> {
  const databaseName = `w3c-v6-blocked-${crypto.randomUUID()}`
  const blocker = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION - 1, (database) => {
    database.createObjectStore(LEGACY_PAUSED_STORE, { keyPath: 'id' })
  })
  const rejection = await rejectionName(IndexedDbReceiveOperationRepository.open(databaseName))
  blocker.close()
  return Object.freeze({ rejection, deleted: await deleteDatabase(databaseName) })
}

async function probeVersionChange(): Promise<string> {
  const databaseName = `w3c-v6-versionchange-${crypto.randomUUID()}`
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const upgrader = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION + 1)
  const rejection = await rejectionName(repository.listRecords(identity(16, 0x31)))
  repository.close()
  upgrader.close()
  await deleteDatabase(databaseName)
  return rejection
}

async function probeReplacement(): Promise<Pick<
  IndexedDbV6Probe,
  | 'schemaVersion'
  | 'v6StoresPresent'
  | 'legacyStoreRetainedForCleanup'
  | 'legacyRowsVisibleToV6'
>> {
  const databaseName = `w3c-v6-replacement-${crypto.randomUUID()}`
  const operationId = identity(16, 0x41)
  const legacy = await openRawDatabase(
    databaseName,
    CHECKPOINT_DATABASE_VERSION - 1,
    (database, transaction) => {
      const store = database.createObjectStore(LEGACY_PAUSED_STORE, { keyPath: 'id' })
      transaction.addEventListener('complete', () => undefined, { once: true })
      store.put({ id: 'obsolete-pause', operationId })
    },
  )
  legacy.close()
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const raw = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION)
  const names = new Set(Array.from(raw.objectStoreNames))
  const result = Object.freeze({
    schemaVersion: raw.version,
    v6StoresPresent: INDEXEDDB_V6_STORE_SCHEMAS.every((schema) => names.has(schema.name)),
    legacyStoreRetainedForCleanup: names.has(LEGACY_PAUSED_STORE),
    legacyRowsVisibleToV6: (await repository.listRecords(operationId)).length !== 0,
  })
  raw.close()
  repository.close()
  await deleteDatabase(databaseName)
  return result
}

function openRawDatabase(
  name: string,
  version: number,
  upgrade?: (database: IDBDatabase, transaction: IDBTransaction) => void,
): Promise<IDBDatabase> {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(name, version)
    request.addEventListener('upgradeneeded', () => {
      const transaction = request.transaction
      if (transaction === null) throw new Error('IndexedDB upgrade lacks a transaction')
      upgrade?.(request.result, transaction)
    }, { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

async function rejectionName(operation: Promise<unknown>): Promise<string> {
  try {
    await operation
    return 'resolved'
  } catch (error) {
    if (error instanceof DOMException || error instanceof Error) return error.name
    return String(error)
  }
}

function deleteDatabase(name: string): Promise<boolean> {
  return new Promise<boolean>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(true), { once: true })
    request.addEventListener('blocked', () => resolve(false), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
