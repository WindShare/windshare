import { OriginCapacityDataError } from './errors'

export const ORIGIN_CAPACITY_DATABASE_NAME = 'windshare-workspace-budget'
export const ORIGIN_CAPACITY_DATABASE_VERSION = 4
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
    let rejected = false
    let upgradeFailure: OriginCapacityDataError | undefined
    const fail = (error: unknown) => { rejected = true; reject(error) }
    request.addEventListener('upgradeneeded', event => {
      // Pre-release formats are incompatible. Never drop accounting for files that may
      // still exist, nor extend an old schema while silently retaining unreadable rows.
      if (rejected || event.oldVersion !== 0) {
        upgradeFailure = new OriginCapacityDataError(`database=${name} schema=${event.oldVersion}; expected=${ORIGIN_CAPACITY_DATABASE_VERSION}. Clear this site's test data before retrying.`)
        // Settle through the request's error event, after the aborted upgrade closes.
        // Otherwise an immediate reopen/reset can race a connection still being released.
        request.result.close()
        request.transaction?.abort()
        return
      }
      for (const store of ORIGIN_CAPACITY_STORES) request.result.createObjectStore(store, { keyPath: 'id' })
    })
    request.addEventListener('blocked', () =>
      fail(new DOMException('Origin capacity database upgrade is blocked', 'InvalidStateError')), { once: true })
    request.addEventListener('error', () => fail(upgradeFailure ?? request.error), { once: true })
    request.addEventListener('success', () => {
      if (rejected) request.result.close()
      else resolve(request.result)
    }, { once: true })
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
  })
}

/**
 * IDB does not abort when an async caller throws after queuing successful writes.
 * Callbacks may await IDB requests only; unrelated async work can auto-commit the transaction.
 */
export async function capacityTransaction<T>(database: IDBDatabase,
  update: (transaction: IDBTransaction) => Promise<T>): Promise<T> {
  const transaction = database.transaction(ORIGIN_CAPACITY_STORES, 'readwrite', { durability: 'strict' })
  const completion = capacityTransactionCompletion(transaction)
  completion.catch(() => undefined)
  try {
    const result = await update(transaction)
    await completion
    return result
  } catch (error) {
    try { transaction.abort() } catch { /* A failed commit may already have ended the transaction. */ }
    await completion.catch(() => undefined)
    throw error
  }
}
