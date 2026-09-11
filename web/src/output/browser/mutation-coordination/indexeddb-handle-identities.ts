import type {
  FSAHandleIdentityRecord,
  FSAHandleIdentityStore,
} from './handle-identity'

const DATABASE_NAME = 'windshare-fsa-mutation-identities-v1'
const DATABASE_VERSION = 1
const HANDLE_STORE = 'handles'

/**
 * These identities outlive receive-operation records: removing recovery metadata
 * must never assign a second lock to a filesystem entry still in use by another tab.
 */
export class IndexedDbFSAHandleIdentityStore implements FSAHandleIdentityStore {
  readonly #factory: IDBFactory

  constructor(factory: IDBFactory = globalThis.indexedDB) {
    if (factory === undefined) {
      throw new DOMException('IndexedDB is required for coordinated FSA output', 'NotSupportedError')
    }
    this.#factory = factory
  }

  async readAll(): Promise<readonly FSAHandleIdentityRecord[]> {
    return this.#transaction('readonly', store => store.getAll())
  }

  async insert(handle: FileSystemHandle): Promise<string> {
    const key = await this.#transaction('readwrite', store => store.add({ handle }))
    if (typeof key !== 'number' || !Number.isSafeInteger(key) || key <= 0) {
      throw new DOMException('The FSA identity registry returned an invalid key', 'DataError')
    }
    return String(key)
  }

  async #transaction<T>(
    mode: IDBTransactionMode,
    request: (store: IDBObjectStore) => IDBRequest<T>,
  ): Promise<T> {
    const database = await openDatabase(this.#factory)
    try {
      return await new Promise<T>((resolve, reject) => {
        const transaction = database.transaction(HANDLE_STORE, mode)
        const pending = request(transaction.objectStore(HANDLE_STORE))
        transaction.oncomplete = () => { resolve(pending.result) }
        transaction.onabort = () => {
          reject(transaction.error ?? new DOMException('FSA identity transaction aborted', 'AbortError'))
        }
        transaction.onerror = () => {
          reject(transaction.error ?? pending.error ?? new DOMException('FSA identity transaction failed', 'UnknownError'))
        }
      })
    } finally {
      database.close()
    }
  }
}

function openDatabase(factory: IDBFactory): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = factory.open(DATABASE_NAME, DATABASE_VERSION)
    let blocked = false
    request.onupgradeneeded = () => {
      request.result.createObjectStore(HANDLE_STORE, { keyPath: 'identity', autoIncrement: true })
    }
    request.onsuccess = () => {
      if (blocked) request.result.close()
      else resolve(request.result)
    }
    request.onerror = () => { reject(request.error) }
    request.onblocked = () => {
      blocked = true
      reject(new DOMException('The FSA identity registry is blocked by another page', 'InvalidStateError'))
    }
  })
}
