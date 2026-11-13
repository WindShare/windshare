import { rejectRollback, type DirectoryCursor, type NetworkSnapshot } from './node-directory'

export interface DirectoryRecord {
  readonly cursor: DirectoryCursor
  readonly envelope: string
  readonly aliases: Readonly<Record<string, string>>
}
export interface DirectoryCache {
  load(): Promise<DirectoryRecord | undefined>
  commit(snapshot: NetworkSnapshot): Promise<void>
}

function nextRecord(previous: DirectoryRecord | undefined, snapshot: NetworkSnapshot): DirectoryRecord {
  rejectRollback(previous?.cursor, snapshot.cursor)
  const aliases: Record<string, string> = { ...previous?.aliases }
  for (const node of snapshot.nodes) {
    if (Object.hasOwn(aliases, node.alias) && aliases[node.alias] !== node.identity) throw new Error('Network directory alias reassigned')
    Object.defineProperty(aliases, node.alias, { value: node.identity, enumerable: true, configurable: true, writable: true })
  }
  const record = Object.freeze({ cursor: snapshot.cursor, envelope: snapshot.envelope, aliases: Object.freeze(aliases) })
  const maximumCacheBytes = 1024 * 1024
  if (new TextEncoder().encode(JSON.stringify(record)).byteLength > maximumCacheBytes) throw new Error('Network alias history is full')
  return record
}

export class MemoryDirectoryCache implements DirectoryCache {
  #record: DirectoryRecord | undefined
  load(): Promise<DirectoryRecord | undefined> { return Promise.resolve(this.#record) }
  commit(snapshot: NetworkSnapshot): Promise<void> {
    try { this.#record = nextRecord(this.#record, snapshot); return Promise.resolve() } catch (error) { return Promise.reject(error) }
  }
}

const DATABASE = 'windshare-network'
const STORE = 'trust'
const KEY = 'directory'

export class BrowserDirectoryCache implements DirectoryCache {
  async #open(): Promise<IDBDatabase> {
    return new Promise((resolve, reject) => {
      let blocked = false
      const request = indexedDB.open(DATABASE, 1)
      request.onupgradeneeded = () => { request.result.createObjectStore(STORE) }
      request.onsuccess = () => { if (blocked) request.result.close(); else resolve(request.result) }
      request.onerror = () => reject(request.error)
      request.onblocked = () => { blocked = true; reject(new Error('Network cache upgrade blocked')) }
    })
  }

  async load(): Promise<DirectoryRecord | undefined> {
    const db = await this.#open()
    try {
      return await new Promise((resolve, reject) => {
        const tx = db.transaction(STORE, 'readonly'), request = tx.objectStore(STORE).get(KEY)
        request.onsuccess = () => resolve(request.result as DirectoryRecord | undefined)
        request.onerror = () => reject(request.error)
      })
    } finally { db.close() }
  }

  async commit(snapshot: NetworkSnapshot): Promise<void> {
    const db = await this.#open()
    try {
      await new Promise<void>((resolve, reject) => {
        const tx = db.transaction(STORE, 'readwrite'), store = tx.objectStore(STORE), request = store.get(KEY)
        // Verification is finished before this transaction; rollback state and
        // alias history commit atomically even when two tabs refresh together.
        request.onsuccess = () => {
          try { store.put(nextRecord(request.result as DirectoryRecord | undefined, snapshot), KEY) }
          catch (error) { reject(error); tx.abort() }
        }
        tx.oncomplete = () => resolve()
        tx.onerror = () => reject(tx.error)
        tx.onabort = () => reject(tx.error ?? new Error('Network cache update aborted'))
      })
    } finally { db.close() }
  }
}
