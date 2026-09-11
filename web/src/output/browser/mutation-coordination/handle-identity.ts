import { IndexedDbFSAHandleIdentityStore } from './indexeddb-handle-identities'
import {
  browserLockManager,
  type BrowserLockManagerRuntime,
} from './web-lock'

const REGISTRY_LOCK_NAME = 'windshare/fsa-handle-identity-registry/v1'

export interface FSAHandleIdentityRecord {
  readonly identity: string | number
  readonly handle: FileSystemHandle
}

export interface FSAHandleIdentityStore {
  readAll(): Promise<readonly FSAHandleIdentityRecord[]>
  insert(handle: FileSystemHandle): Promise<string>
}

export interface FSAHandleIdentityResolver {
  resolve(handle: FileSystemHandle): Promise<string>
}

/**
 * A persisted native handle is the only cross-tab authority for browser entry identity.
 * The global lock spans comparison and insertion, but each IndexedDB transaction
 * finishes before awaiting native isSameEntry so it cannot auto-close mid-update.
 */
export class FSAHandleIdentityRegistry implements FSAHandleIdentityResolver {
  readonly #store: FSAHandleIdentityStore
  readonly #manager: BrowserLockManagerRuntime

  constructor(options: Readonly<{
    store?: FSAHandleIdentityStore
    manager?: BrowserLockManagerRuntime
  }> = {}) {
    this.#store = options.store ?? new IndexedDbFSAHandleIdentityStore()
    this.#manager = options.manager ?? browserLockManager()
  }

  async resolve(handle: FileSystemHandle): Promise<string> {
    requireHandle(handle)
    let identity: string | undefined
    await this.#manager.request(REGISTRY_LOCK_NAME, { mode: 'exclusive' }, async lock => {
      if (lock === null) {
        throw new DOMException('The FSA identity registry lock was not acquired', 'InvalidStateError')
      }
      const records = await this.#store.readAll()
      for (const record of records) {
        requireHandle(record.handle)
        requireIdentity(String(record.identity))
        if (record.handle.kind !== handle.kind) continue
        if (await handle.isSameEntry(record.handle)) {
          identity = String(record.identity)
          return
        }
      }
      identity = await this.#store.insert(handle)
      requireIdentity(identity)
    })
    if (identity === undefined) {
      throw new DOMException('The FSA entry identity could not be established', 'InvalidStateError')
    }
    return identity
  }
}

function requireHandle(handle: FileSystemHandle): void {
  if (
    handle === null || typeof handle !== 'object' ||
    (handle.kind !== 'file' && handle.kind !== 'directory') ||
    typeof handle.isSameEntry !== 'function'
  ) {
    throw new TypeError('FSA mutation coordination requires a comparable native handle')
  }
}

function requireIdentity(identity: string): void {
  if (!/^[1-9]\d*$/.test(identity)) {
    throw new DOMException('The FSA identity registry contains an invalid identity', 'DataError')
  }
}
