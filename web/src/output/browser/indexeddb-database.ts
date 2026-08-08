/** Version 5 adds the crash-replay journal for semantic current-state discard. */
export const CHECKPOINT_DATABASE_VERSION = 5
export const DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME = 'windshare-output-checkpoints'

export const INDEXEDDB_CHECKPOINT_CANDIDATE_STORE = 'file-checkpoint-v1-candidates'
export const INDEXEDDB_CHECKPOINT_COMMITTED_STORE = 'file-checkpoint-v1-committed'
export const INDEXEDDB_CHECKPOINT_HANDLE_STORE = 'file-checkpoint-v1-handles'
export const INDEXEDDB_CHECKPOINT_METADATA_STORE = 'file-checkpoint-v1-metadata'
export const INDEXEDDB_CHECKPOINT_CLEANUP_STORE = 'file-checkpoint-v1-cleanup'
export const INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE = 'paused-task-descriptor-v1'
export const INDEXEDDB_ROOT_CAPABILITY_STORE = 'root-capability-v1'
export const INDEXEDDB_RESUME_STATE_DISCARD_STORE = 'resume-state-discard-v1'
export const INDEXEDDB_CHECKPOINT_NAMESPACE_INDEX = 'by-namespace'

const CHECKPOINT_RECORD_STORES = [
  INDEXEDDB_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_CHECKPOINT_HANDLE_STORE,
] as const

const CHECKPOINT_SINGLETON_STORES = [
  INDEXEDDB_CHECKPOINT_METADATA_STORE,
  INDEXEDDB_CHECKPOINT_CLEANUP_STORE,
  INDEXEDDB_PAUSED_TASK_DESCRIPTOR_STORE,
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  INDEXEDDB_RESUME_STATE_DISCARD_STORE,
] as const

export async function openIndexedDbCheckpointDatabase(name: string): Promise<IDBDatabase> {
  if (name.length === 0) throw new TypeError('IndexedDB name must not be empty')
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB output checkpoints are unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, CHECKPOINT_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () => {
    const database = request.result
    for (const storeName of CHECKPOINT_RECORD_STORES) {
      if (database.objectStoreNames.contains(storeName)) continue
      database.createObjectStore(storeName, { keyPath: 'id' })
        .createIndex(INDEXEDDB_CHECKPOINT_NAMESPACE_INDEX, 'namespace')
    }
    for (const storeName of CHECKPOINT_SINGLETON_STORES) {
      if (!database.objectStoreNames.contains(storeName)) {
        database.createObjectStore(storeName, { keyPath: 'id' })
      }
    }
  })
  let rejected = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => {
      rejected = true
      reject(new DOMException(
        'Output checkpoint database upgrade is blocked by another tab',
        'InvalidStateError',
      ))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (rejected) request.result.close()
      else resolve(request.result)
    }, { once: true })
  })
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
