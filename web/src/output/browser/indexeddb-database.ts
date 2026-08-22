export const CHECKPOINT_DATABASE_VERSION = 9
export const DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME = 'windshare-output-checkpoints'

export const INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE = 'file-checkpoint-v2-candidates'
export const INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE = 'file-checkpoint-v2-committed'
export const INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE = 'file-checkpoint-v2-handles'
export const INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE = 'file-checkpoint-v2-control'
export const INDEXEDDB_RECEIVE_RECORD_STORE = 'receive-operation-v2-records'
export const INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE = 'receive-operation-v2-manifest-pages'
export const INDEXEDDB_RECEIVE_HANDLE_STORE = 'receive-operation-v2-handles'
export const INDEXEDDB_RECEIVE_LEASE_STORE = 'receive-operation-v2-leases'
export const INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE = 'compatible-name-v1-operations'
export const INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE = 'compatible-name-v1-mappings'
export const INDEXEDDB_DIRECT_ZIP_STATE_STORE = 'direct-zip-state-v1'
export const INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE = 'direct-zip-candidates-v1'
export const INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE = 'direct-zip-layout-pages-v1'
export const INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE = 'direct-zip-central-pages-v1'
export const INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE = 'direct-zip-epoch-pages-v1'

const INDEXEDDB_LEGACY_RECEIVE_RECORD_STORE = 'receive-operation-v1-records'
const INDEXEDDB_LEGACY_RECEIVE_MANIFEST_PAGE_STORE = 'receive-operation-v1-manifest-pages'
const INDEXEDDB_LEGACY_RECEIVE_HANDLE_STORE = 'receive-operation-v1-handles'
const INDEXEDDB_LEGACY_RECEIVE_LEASE_STORE = 'receive-operation-v1-leases'

export const INDEXEDDB_BY_OPERATION_INDEX = 'by-operation'
export const INDEXEDDB_BY_OPERATION_FILE_INDEX = 'by-operation-file'
export const INDEXEDDB_BY_OPERATION_KIND_INDEX = 'by-operation-kind'
export const INDEXEDDB_BY_KIND_INDEX = 'by-kind'
export const INDEXEDDB_BY_REOPEN_KEY_INDEX = 'by-reopen-key'
export const INDEXEDDB_BY_STATE_INDEX = 'by-state'
export const INDEXEDDB_BY_EXPIRY_INDEX = 'by-expiry'
export const INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX = 'by-operation-commit-ordinal'
export const INDEXEDDB_BY_KIND_CANDIDATE_INDEX = 'by-kind-candidate'
export const INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX = 'by-operation-chain-page'

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
  readonly keyPath: string
  readonly indexes: readonly Readonly<{
    name: string
    keyPath: string | readonly string[]
  }>[]
}

export const INDEXEDDB_V6_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze([
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_LEGACY_RECEIVE_RECORD_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
    indexSchema(INDEXEDDB_BY_REOPEN_KEY_INDEX, 'reopenKey'),
    indexSchema(INDEXEDDB_BY_STATE_INDEX, 'state'),
    indexSchema(INDEXEDDB_BY_EXPIRY_INDEX, 'expiresAt'),
  ]),
  storeSchema(INDEXEDDB_LEGACY_RECEIVE_MANIFEST_PAGE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_LEGACY_RECEIVE_HANDLE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_LEGACY_RECEIVE_LEASE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
])

export const INDEXEDDB_V7_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze(
  INDEXEDDB_V6_STORE_SCHEMAS.map((schema) => schema.name === INDEXEDDB_LEGACY_RECEIVE_RECORD_STORE
    ? storeSchema(schema.name, schema.keyPath, [
        ...schema.indexes,
        indexSchema(INDEXEDDB_BY_KIND_INDEX, 'kind'),
      ])
    : schema),
)

export const INDEXEDDB_V8_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze([
  ...INDEXEDDB_V7_STORE_SCHEMAS,
  storeSchema(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE, 'operationId', []),
  storeSchema(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX, ['operationId', 'commitOrdinal']),
  ]),
])

export const INDEXEDDB_V9_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze([
  ...INDEXEDDB_V8_STORE_SCHEMAS.filter(schema =>
    schema.name !== INDEXEDDB_LEGACY_RECEIVE_RECORD_STORE &&
    schema.name !== INDEXEDDB_LEGACY_RECEIVE_MANIFEST_PAGE_STORE &&
    schema.name !== INDEXEDDB_LEGACY_RECEIVE_HANDLE_STORE &&
    schema.name !== INDEXEDDB_LEGACY_RECEIVE_LEASE_STORE),
  storeSchema(INDEXEDDB_RECEIVE_RECORD_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
    indexSchema(INDEXEDDB_BY_REOPEN_KEY_INDEX, 'reopenKey'),
    indexSchema(INDEXEDDB_BY_STATE_INDEX, 'state'),
    indexSchema(INDEXEDDB_BY_EXPIRY_INDEX, 'expiresAt'),
    indexSchema(INDEXEDDB_BY_KIND_INDEX, 'kind'),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_MANIFEST_PAGE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_HANDLE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_LEASE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
  storeSchema(INDEXEDDB_DIRECT_ZIP_STATE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
  ]),
  storeSchema(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kindByte']),
    indexSchema(INDEXEDDB_BY_KIND_CANDIDATE_INDEX, ['kindByte', 'id']),
  ]),
  ...[
    INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
    INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
    INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
  ].map(name => storeSchema(name, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX, [
      'operationId',
      'chainId',
      'pageOrdinal',
    ]),
  ])),
])

export async function openIndexedDbCheckpointDatabase(name: string): Promise<IDBDatabase> {
  if (name.length === 0) throw new TypeError('IndexedDB name must not be empty')
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB output repository is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, CHECKPOINT_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', (event) =>
    installIndexedDbV9Schema(
      request.result,
      request.transaction ?? undefined,
      event.oldVersion,
    ))
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

export function installIndexedDbV9Schema(
  database: IDBDatabase,
  transaction?: IDBTransaction,
  oldVersion = CHECKPOINT_DATABASE_VERSION,
): void {
  installSchemas(database, INDEXEDDB_V9_STORE_SCHEMAS, transaction)
  if (oldVersion <= 0 || oldVersion >= CHECKPOINT_DATABASE_VERSION) return
  if (transaction === undefined) {
    throw new DOMException('IndexedDB v9 migration lacks its versionchange transaction', 'InvalidStateError')
  }

  // Legacy receive-operation/lifecycle rows cannot authorize a strict V2 reopen. Deleting
  // their stores makes a mixed decoder impossible while leaving user-visible files intact.
  for (const name of [
    INDEXEDDB_LEGACY_RECEIVE_RECORD_STORE,
    INDEXEDDB_LEGACY_RECEIVE_MANIFEST_PAGE_STORE,
    INDEXEDDB_LEGACY_RECEIVE_HANDLE_STORE,
    INDEXEDDB_LEGACY_RECEIVE_LEASE_STORE,
  ]) {
    if (database.objectStoreNames.contains(name)) database.deleteObjectStore(name)
  }
  for (const name of [
    INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
    INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
    INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
    INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
  ]) {
    if (database.objectStoreNames.contains(name)) transaction.objectStore(name).clear()
  }
}

export function installIndexedDbV6Schema(database: IDBDatabase): void {
  for (const schema of INDEXEDDB_V6_STORE_SCHEMAS) {
    if (database.objectStoreNames.contains(schema.name)) continue
    const store = database.createObjectStore(schema.name, { keyPath: schema.keyPath })
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
      : database.createObjectStore(schema.name, { keyPath: schema.keyPath })
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

export function installIndexedDbV8Schema(
  database: IDBDatabase,
  transaction?: IDBTransaction,
  oldVersion = CHECKPOINT_DATABASE_VERSION,
): void {
  for (const schema of INDEXEDDB_V8_STORE_SCHEMAS) {
    const store = database.objectStoreNames.contains(schema.name)
      ? transaction?.objectStore(schema.name)
      : database.createObjectStore(schema.name, { keyPath: schema.keyPath })
    if (store === undefined) {
      throw new DOMException('IndexedDB schema upgrade lacks its versionchange transaction', 'InvalidStateError')
    }
    for (const index of schema.indexes) {
      if (!store.indexNames.contains(index.name)) {
        store.createIndex(index.name, index.keyPath, { unique: false, multiEntry: false })
      }
    }
  }

  if (oldVersion <= 0 || oldVersion >= CHECKPOINT_DATABASE_VERSION) return
  if (transaction === undefined) {
    throw new DOMException('IndexedDB v8 migration lacks its versionchange transaction', 'InvalidStateError')
  }
  // ReceiveIntent v1 cannot be reinterpreted under the v2 digest domain. Clearing only
  // browser metadata leaves the user's already-materialized filesystem entries untouched.
  for (const schema of INDEXEDDB_V7_STORE_SCHEMAS) {
    transaction.objectStore(schema.name).clear()
  }
}

function installSchemas(
  database: IDBDatabase,
  schemas: readonly IndexedDbStoreSchema[],
  transaction?: IDBTransaction,
): void {
  for (const schema of schemas) {
    const store = database.objectStoreNames.contains(schema.name)
      ? transaction?.objectStore(schema.name)
      : database.createObjectStore(schema.name, { keyPath: schema.keyPath })
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
  keyPath: string,
  indexes: readonly IndexedDbStoreSchema['indexes'][number][],
): IndexedDbStoreSchema {
  return Object.freeze({ name, keyPath, indexes: Object.freeze(indexes) })
}

function indexSchema(
  name: string,
  keyPath: string | readonly string[],
): IndexedDbStoreSchema['indexes'][number] {
  return Object.freeze({ name, keyPath })
}
