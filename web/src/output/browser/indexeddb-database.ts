export const OUTPUT_DATABASE_VERSION = 12
export const CHECKPOINT_DATABASE_VERSION = OUTPUT_DATABASE_VERSION
export const DEFAULT_OUTPUT_DATABASE_NAME = 'windshare-output-checkpoints'
export const DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME = DEFAULT_OUTPUT_DATABASE_NAME

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
export const INDEXEDDB_FILE_FINAL_PROOF_STORE = 'file-final-proof-v1'
export const INDEXEDDB_BROWSER_SAVE_POLICY_STORE = 'browser-save-policy-v1'
export const INDEXEDDB_BROWSER_DELIVERY_FILE_STORE = 'browser-file-delivery-v1'
export const INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE = 'materialization-ledger-v1-entries'
export const INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE = 'materialization-ledger-v1-pages'
export const INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE = 'materialization-ledger-v1-seals'

export const INDEXEDDB_BY_OPERATION_INDEX = 'by-operation'
export const INDEXEDDB_BY_OPERATION_FILE_INDEX = 'by-operation-file'
export const INDEXEDDB_BY_OPERATION_KIND_INDEX = 'by-operation-kind'
export const INDEXEDDB_BY_KIND_INDEX = 'by-kind'
export const INDEXEDDB_BY_REOPEN_KEY_INDEX = 'by-reopen-key'
export const INDEXEDDB_BY_STATE_INDEX = 'by-state'
export const INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX = 'by-operation-commit-ordinal'
export const INDEXEDDB_BY_KIND_CANDIDATE_INDEX = 'by-kind-candidate'
export const INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX = 'by-operation-chain-page'
export const INDEXEDDB_BY_OPERATION_LINEAGE_INDEX = 'by-operation-lineage'
export const INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX = 'by-operation-owned-object'
export const INDEXEDDB_BY_OPERATION_RECORD_INDEX = 'by-operation-record'
export const INDEXEDDB_BY_OPERATION_FILE_RECORD_INDEX = 'by-operation-file-record'
export const INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX = 'by-operation-path-order'
export const INDEXEDDB_BY_OPERATION_SEAL_PAGE_INDEX = 'by-operation-seal-page'
export const INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX = 'by-operation-record-proof'
export const INDEXEDDB_BY_OPERATION_SEAL_SEQUENCE_INDEX = 'by-operation-seal-sequence'

export const INDEXEDDB_OPFS_TASK_STORE = 'opfs-task-checkpoints-v1'
export const INDEXEDDB_OPFS_ENTRY_STORE = 'opfs-task-entries-v1'
export const INDEXEDDB_OPFS_PATH_STORE = 'opfs-task-paths-v1'
export const INDEXEDDB_OPFS_DIRECTORY_STORE = 'opfs-task-directories-v1'

export const INDEXEDDB_BY_OBJECT_SEQUENCE_INDEX = 'by-object-sequence'

export interface IndexedDbStoreSchema {
  readonly name: string
  readonly keyPath: string | readonly string[]
  readonly indexes: readonly Readonly<{
    name: string
    keyPath: string | readonly string[]
    unique?: boolean
  }>[]
}

// This is the current layout, not a history of layouts the browser must emulate.
export const INDEXEDDB_OUTPUT_STORE_SCHEMAS: readonly IndexedDbStoreSchema[] = Object.freeze([
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
    indexSchema(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX, ['operationId', 'lineageId']),
    indexSchema(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX, ['operationId', 'ownedObjectId'], true),
    indexSchema(INDEXEDDB_BY_OPERATION_RECORD_INDEX, ['operationId', 'id']),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_RECORD_INDEX, ['operationId', 'fileId', 'id']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId']),
    indexSchema(INDEXEDDB_BY_OPERATION_LINEAGE_INDEX, ['operationId', 'lineageId']),
    indexSchema(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX, ['operationId', 'ownedObjectId'], true),
    indexSchema(INDEXEDDB_BY_OPERATION_RECORD_INDEX, ['operationId', 'id']),
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_RECORD_INDEX, ['operationId', 'fileId', 'id']),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX, ['operationId', 'ownedObjectId'], true),
  ]),
  storeSchema(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
  ]),
  storeSchema(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE, 'operationId', []),
  storeSchema(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX, ['operationId', 'commitOrdinal']),
  ]),
  storeSchema(INDEXEDDB_RECEIVE_RECORD_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_KIND_INDEX, ['operationId', 'kind']),
    indexSchema(INDEXEDDB_BY_REOPEN_KEY_INDEX, 'reopenKey'),
    indexSchema(INDEXEDDB_BY_STATE_INDEX, 'state'),
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
  storeSchema(INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX, ['operationId', 'chainId', 'pageOrdinal']),
  ]),
  storeSchema(INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX, ['operationId', 'chainId', 'pageOrdinal']),
  ]),
  storeSchema(INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX, ['operationId', 'chainId', 'pageOrdinal']),
  ]),
  storeSchema(INDEXEDDB_FILE_FINAL_PROOF_STORE, 'proofId', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX, ['operationId', 'recordId'], true),
  ]),
  storeSchema(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE, 'entryId', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX, ['operationId', 'pathKey', 'entryOrder'], true),
  ]),
  storeSchema(INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE, 'pageId', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_SEAL_PAGE_INDEX, ['operationId', 'sealId', 'pageOrdinal'], true),
  ]),
  storeSchema(INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE, 'sealId', [
    indexSchema(INDEXEDDB_BY_OPERATION_INDEX, 'operationId'),
    indexSchema(INDEXEDDB_BY_OPERATION_SEAL_SEQUENCE_INDEX, ['operationId', 'sealSequence'], true),
  ]),
  storeSchema(INDEXEDDB_OPFS_TASK_STORE, 'id', []),
  storeSchema(INDEXEDDB_OPFS_ENTRY_STORE, 'id', [
    indexSchema(INDEXEDDB_BY_OBJECT_SEQUENCE_INDEX, ['objectKey', 'sequenceKey'], true),
  ]),
  storeSchema(INDEXEDDB_OPFS_PATH_STORE, 'id', []),
  storeSchema(INDEXEDDB_OPFS_DIRECTORY_STORE, 'id', []),
  storeSchema(INDEXEDDB_BROWSER_SAVE_POLICY_STORE, 'operationId', []),
  storeSchema(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, ['operationId', 'fileId'], [
    indexSchema(INDEXEDDB_BY_OPERATION_FILE_INDEX, ['operationId', 'fileId'], true),
  ]),
])

export const INDEXEDDB_OPERATION_STORE_SCHEMAS = INDEXEDDB_OUTPUT_STORE_SCHEMAS.filter(
  (schema): schema is IndexedDbStoreSchema & { readonly keyPath: string } =>
    typeof schema.keyPath === 'string' &&
    (schema.keyPath === 'operationId' ||
      schema.indexes.some(index => index.name === INDEXEDDB_BY_OPERATION_INDEX)),
)

export async function openIndexedDbCheckpointDatabase(name: string): Promise<IDBDatabase> {
  if (name.length === 0) throw new TypeError('IndexedDB name must not be empty')
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB output repository is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, OUTPUT_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', event => {
    if (event.oldVersion !== 0) {
      request.transaction?.addEventListener('complete', () => console.info({
        event: 'output.database.schema_reset',
        database: name,
        previous_version: event.oldVersion,
        current_version: OUTPUT_DATABASE_VERSION,
        disposition: 'incompatible-metadata-discarded',
      }), { once: true })
    }
    installOutputDatabaseSchema(request.result)
  })
  let blocked = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => {
      blocked = true
      reject(new DOMException('Output repository upgrade is blocked by another tab', 'InvalidStateError'))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (blocked) request.result.close()
      else {
        request.result.addEventListener('versionchange', () => request.result.close())
        resolve(request.result)
      }
    }, { once: true })
  })
}

// Called only inside IndexedDB's versionchange transaction. Reopening the current
// format never enters here. Unknown records cannot grant filesystem authority,
// so incompatible metadata is discarded without invoking any persisted handle.
export function installOutputDatabaseSchema(database: IDBDatabase): void {
  for (const name of Array.from(database.objectStoreNames)) database.deleteObjectStore(name)
  for (const schema of INDEXEDDB_OUTPUT_STORE_SCHEMAS) {
    const store = database.createObjectStore(schema.name, {
      keyPath: typeof schema.keyPath === 'string' ? schema.keyPath : [...schema.keyPath],
    })
    for (const index of schema.indexes) {
      store.createIndex(index.name, typeof index.keyPath === 'string' ? index.keyPath : [...index.keyPath], {
        unique: index.unique ?? false,
        multiEntry: false,
      })
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
  keyPath: string | readonly string[],
  indexes: readonly IndexedDbStoreSchema['indexes'][number][],
): IndexedDbStoreSchema {
  return Object.freeze({ name, keyPath, indexes: Object.freeze(indexes) })
}

function indexSchema(
  name: string,
  keyPath: string | readonly string[],
  unique = false,
): IndexedDbStoreSchema['indexes'][number] {
  return Object.freeze({ name, keyPath, ...(unique ? { unique: true } : {}) })
}
