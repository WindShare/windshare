import { rejectBlockRollback, type BlockCursor, type BlockSnapshot } from './blocks'

export interface BlockRecord { readonly cursor: BlockCursor; readonly envelope: string }
export interface BlockCache {
  load(): Promise<BlockRecord | undefined>
  commit(snapshot: BlockSnapshot): Promise<void>
}
export class BlockCacheAccessDeniedError extends Error {
  constructor(cause: DOMException) {
    super('IndexedDB access denied for the authority cache', { cause })
    this.name = 'BlockCacheAccessDeniedError'
  }
}

export class MemoryBlockCache implements BlockCache {
  #record: BlockRecord | undefined
  load(): Promise<BlockRecord | undefined> { return Promise.resolve(this.#record) }
  async commit(snapshot: BlockSnapshot): Promise<void> {
    rejectBlockRollback(this.#record?.cursor, snapshot.cursor)
    this.#record = { cursor: snapshot.cursor, envelope: snapshot.envelope }
  }
}
const DATABASE = 'windshare-authority'
const STORE = 'blocks'
export class BrowserBlockCache implements BlockCache {
  readonly #root: string
  constructor(root: string) { this.#root = root }
  #open(): Promise<IDBDatabase> {
    return new Promise<IDBDatabase>((resolve, reject) => {
      let blocked = false
      const request = indexedDB.open(DATABASE, 1)
      request.onupgradeneeded = () => { request.result.createObjectStore(STORE) }
      request.onsuccess = () => { if (blocked) request.result.close(); else resolve(request.result) }
      request.onerror = () => reject(request.error)
      request.onblocked = () => { blocked = true; reject(new Error('Authority cache upgrade blocked')) }
    }).catch((failure: unknown) => {
      if (failure instanceof DOMException && failure.name === 'SecurityError') throw new BlockCacheAccessDeniedError(failure)
      throw failure
    })
  }
  async load(): Promise<BlockRecord | undefined> {
    const database = await this.#open()
    try {
      return await new Promise((resolve, reject) => {
        const request = database.transaction(STORE, 'readonly').objectStore(STORE).get(this.#root)
        request.onsuccess = () => resolve(request.result as BlockRecord | undefined)
        request.onerror = () => reject(request.error)
      })
    } finally { database.close() }
  }
  async commit(snapshot: BlockSnapshot): Promise<void> {
    const database = await this.#open()
    try {
      await new Promise<void>((resolve, reject) => {
        const transaction = database.transaction(STORE, 'readwrite'), store = transaction.objectStore(STORE)
        const request = store.get(this.#root)
        request.onsuccess = () => {
          try {
            rejectBlockRollback((request.result as BlockRecord | undefined)?.cursor, snapshot.cursor)
            store.put({ cursor: snapshot.cursor, envelope: snapshot.envelope }, this.#root)
          } catch (error) { reject(error); transaction.abort() }
        }
        transaction.oncomplete = () => resolve()
        transaction.onerror = () => reject(transaction.error)
        transaction.onabort = () => reject(transaction.error ?? new Error('Authority cache commit aborted'))
      })
    } finally { database.close() }
  }
}
