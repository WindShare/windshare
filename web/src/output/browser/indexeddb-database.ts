export const CHECKPOINT_DATABASE_VERSION = 7
export const DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME = 'windshare-output-checkpoints'

export const INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE = 'file-checkpoint-v2-candidates'
export const INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE = 'file-checkpoint-v2-committed'
export const INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE = 'file-checkpoint-v2-handles'
export const INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE = 'file-checkpoint-v2-control'
export const INDEXEDDB_RECEIVE_RECORD_STORE = 'receive-operation-v1-records'
export const INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE = 'receive-operation-v1-manifest-pages'
export const INDEXEDDB_RECEIVE_HANDLE_STORE = 'receive-operation-v1-handles'
export const INDEXEDDB_RECEIVE_LEASE_STORE = 'receive-operation-v1-leases'

export const INDEXEDDB_BY_OPERATION_INDEX = 'by-operation'
export const INDEXEDDB_BY_OPERATION_FILE_INDEX = 'by-operation-file'
export const INDEXEDDB_BY_OPERATION_KIND_INDEX = 'by-operation-kind'
export const INDEXEDDB_BY_KIND_INDEX = 'by-kind'
export const INDEXEDDB_BY_REOPEN_KEY_INDEX = 'by-reopen-key'
export const INDEXEDDB_BY_STATE_INDEX = 'by-state'
export const INDEXEDDB_BY_EXPIRY_INDEX = 'by-expiry'

export const INDEXEDDB_LEGACY_V5_STORES = Object.freeze([
  'file-checkpoint-v1-candidates',
  'file-checkpoint-v1-committed',
  'file-checkpoint-v1-handles',
  'file-checkpoint-v1-metadata',
  'file-checkpoint-v1-cleanup',
  'paused-task-descriptor-v1',
  'root-capability-v1',
  'resume-state-discard-v1',
] as const)

export interface IndexedDbStoreSchema {
  readonly name: string
  readonly indexes: readonly Readonly<{
    name: string
    keyPath: string | readonly string[]
  }>[]
}

export const INDEXEDDB_V6_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze([
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_RECORD_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
    indexSchema(INDEXEDDB_BY_REOPEN_KEY_INDEX, 'reopenKey'),
    indexSchema(INDEXEDDB_BY_STATE_INDEX, 'state'),
    indexSchema(INDEXEDDB_BY_EXPIRY_INDEX, 'expiresAt'),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_HANDLE_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_LEASE_STORE, [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
])

export const INDEXEDDB_V7_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze(
  INDEXEDDB_V6_STORE_SCHEMAS.map((schema) => schema.name === INDEXEDDB_RECEIVE_RECORD_STORE
    ? storeSchema(schema.name, [
        ...schema.indexes,
        indexSchema(INDEXEDDB_BY_KIND_INDEX, 'kind'),
      ])
    : schema),
)

export async function openIndexedDbCheckpointDatabase(name: string): Promise<IDBDatabase> {
  if (name.length === 0) throw new TypeError('IndexedDB name must not be empty')
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB output repository is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, CHECKPOINT_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () =>
    installIndexedDbV7Schema(request.result, request.transaction ?? undefined))
  let blocked = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => {
      blocked = true
      reject(new DOMException(
        'Output repository upgrade is blocked by another tab',
        'InvalidStateError',
      ))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (blocked) request.result.close()
      else resolve(request.result)
    }, { once: true })
  })
}

export function installIndexedDbV6Schema(database: IDBDatabase): void {
  for (const schema of INDEXEDDB_V6_STORE_SCHEMAS) {
    if (database.objectStoreNames.contains(schema.name)) continue
    const store = database.createObjectStore(schema.name, { keyPath: 'id' })
    for (const index of schema.indexes) {
      store.createIndex(index.name, index.keyPath, { unique: false, multiEntry: false })
    }
  }
}

export function installIndexedDbV7Schema(
  database: IDBDatabase,
  transaction?: IDBTransaction,
): void {
  for (const schema of INDEXEDDB_V7_STORE_SCHEMAS) {
    const store = database.objectStoreNames.contains(schema.name)
      ? transaction?.objectStore(schema.name)
      : database.createObjectStore(schema.name, { keyPath: 'id' })
    if (store === undefined) {
      throw new DOMException('IndexedDB schema upgrade lacks its versionchange transaction', 'InvalidStateError')
    }
    for (const index of schema.indexes) {
      if (!store.indexNames.contains(index.name)) {
        store.createIndex(index.name, index.keyPath, { unique: false, multiEntry: false })
      }
    }
  }
}

export function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

export function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}

export function isIndexedDbRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function storeSchema(
  name: string,
  indexes: readonly IndexedDbStoreSchema['indexes'][number][],
): IndexedDbStoreSchema {
  return Object.freeze({ name, indexes: Object.freeze(indexes) })
}

function indexSchema(
  name: string,
  keyPath: string | readonly string[],
): IndexedDbStoreSchema['indexes'][number] {
  return Object.freeze({ name, keyPath })
}
