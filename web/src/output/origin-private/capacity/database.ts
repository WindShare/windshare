export const ORIGIN_CAPACITY_DATABASE_NAME = 'windshare-workspace-budget'
export const ORIGIN_CAPACITY_DATABASE_VERSION = 3
export const WORKSPACE_CLAIM_STORE = 'workspace-budget-claims'
export const WORKSPACE_OBJECT_STORE = 'workspace-object-capacity'
export const STAGING_FILE_STORE = 'staging-file-capacity'
export const ORIGIN_CAPACITY_STORES = [WORKSPACE_CLAIM_STORE, WORKSPACE_OBJECT_STORE, STAGING_FILE_STORE]
export const CAPACITY_INVENTORY_BOUND = 1_048_576

export async function openOriginCapacityDatabase(name = ORIGIN_CAPACITY_DATABASE_NAME): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB origin capacity authority is unavailable', 'NotSupportedError')
  }
  if (name.length === 0) throw new TypeError('Origin capacity database name is empty')
  const request = indexedDB.open(name, ORIGIN_CAPACITY_DATABASE_VERSION)
  return new Promise((resolve, reject) => {
    request.addEventListener('upgradeneeded', () => {
      // A schema extension cannot release reservations for bytes that still exist on disk.
      for (const store of ORIGIN_CAPACITY_STORES) {
        if (!request.result.objectStoreNames.contains(store)) request.result.createObjectStore(store, { keyPath: 'id' })
      }
    })
    request.addEventListener('blocked', () => reject(new DOMException('Origin capacity database upgrade is blocked', 'InvalidStateError')), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
  })
}

export function capacityRequest<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

export function capacityTransactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error ??
      new DOMException('Origin capacity transaction aborted', 'AbortError')), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}
