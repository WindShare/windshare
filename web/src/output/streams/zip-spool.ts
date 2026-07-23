const ZIP_SPOOL_DATABASE = 'windshare-zip-central-directory'
const ZIP_SPOOL_DATABASE_VERSION = 3
const ZIP_SPOOL_CHUNK_STORE = 'central-directory-chunks'
const ZIP_SPOOL_NAMESPACE_STORE = 'central-directory-namespaces'
const ZIP_SPOOL_CHUNK_MAXIMUM_BYTES = 256 * 1024
const ZIP_SPOOL_CHUNK_MAXIMUM_RECORDS = 256
const ZIP_SPOOL_MAXIMUM_CHUNK_INDEX = Number.MAX_SAFE_INTEGER
export const ZIP_SPOOL_NAMESPACE_LEASE_MILLISECONDS = 300_000
export const ZIP_SPOOL_NAMESPACE_HEARTBEAT_MILLISECONDS = 60_000

export interface ZipCentralDirectoryManifest {
  readonly chunkCount: number
  readonly recordCount: bigint
  readonly byteLength: bigint
}

export interface ZipCentralDirectorySpool {
  append(record: Uint8Array): Promise<void>
  seal(): Promise<ZipCentralDirectoryManifest>
  readChunk(index: number): Promise<Uint8Array | undefined>
  clear(): Promise<void>
}

export interface IndexedDbZipSpoolOptions {
  readonly databaseName?: string
  readonly namespace?: string
  readonly now?: () => number
  readonly leaseMilliseconds?: number
  readonly heartbeatMilliseconds?: number
  readonly token?: string
}

interface StoredZipChunk {
  readonly namespace: string
  readonly index: number
  readonly bytes: Uint8Array
}

interface StoredZipNamespace {
  readonly id: string
  readonly token: string
  readonly expiresAtMilliseconds: number
}

export class IndexedDbZipCentralDirectorySpool implements ZipCentralDirectorySpool {
  readonly #namespace: string
  readonly #token: string
  readonly #databaseName: string
  readonly #now: () => number
  readonly #leaseMilliseconds: number
  readonly #heartbeatMilliseconds: number
  readonly #pending: Uint8Array[] = []
  #database: Promise<IDBDatabase> | undefined
  #pendingBytes = 0
  #chunkCount = 0
  #recordCount = 0n
  #byteLength = 0n
  #heartbeatTimer: ReturnType<typeof setInterval> | undefined
  #failure: unknown
  #registered = false
  #sealed = false
  #cleared = false

  constructor(options: IndexedDbZipSpoolOptions = {}) {
    this.#databaseName = options.databaseName ?? ZIP_SPOOL_DATABASE
    this.#namespace = options.namespace ?? randomNamespace()
    this.#token = options.token ?? randomNamespace()
    this.#now = options.now ?? Date.now
    this.#leaseMilliseconds = options.leaseMilliseconds ?? ZIP_SPOOL_NAMESPACE_LEASE_MILLISECONDS
    this.#heartbeatMilliseconds =
      options.heartbeatMilliseconds ?? ZIP_SPOOL_NAMESPACE_HEARTBEAT_MILLISECONDS
    if (this.#databaseName.length === 0) throw new TypeError('ZIP spool database name is empty')
    if (this.#namespace.length === 0 || this.#token.length === 0) {
      throw new TypeError('ZIP spool namespace identity is empty')
    }
    requirePositiveSafeInteger(this.#leaseMilliseconds, 'ZIP spool namespace lease')
    requirePositiveSafeInteger(this.#heartbeatMilliseconds, 'ZIP spool namespace heartbeat')
    if (this.#heartbeatMilliseconds >= this.#leaseMilliseconds) {
      throw new RangeError('ZIP spool heartbeat must be shorter than its namespace lease')
    }
  }

  async append(record: Uint8Array): Promise<void> {
    this.#requireWritable()
    if (!(record instanceof Uint8Array) || record.byteLength === 0) {
      throw new TypeError('ZIP central-directory record must contain bytes')
    }
    if (record.byteLength > ZIP_SPOOL_CHUNK_MAXIMUM_BYTES) {
      throw new RangeError('ZIP central-directory record exceeds the durable chunk bound')
    }
    if (this.#pending.length > 0 &&
        (this.#pending.length >= ZIP_SPOOL_CHUNK_MAXIMUM_RECORDS ||
          this.#pendingBytes + record.byteLength > ZIP_SPOOL_CHUNK_MAXIMUM_BYTES)) {
      await this.#flush()
    }
    this.#pending.push(record.slice())
    this.#pendingBytes += record.byteLength
    this.#recordCount += 1n
    this.#byteLength += BigInt(record.byteLength)
    if (this.#pendingBytes >= ZIP_SPOOL_CHUNK_MAXIMUM_BYTES) await this.#flush()
  }

  async seal(): Promise<ZipCentralDirectoryManifest> {
    this.#requireHealthy()
    if (this.#cleared) throw new Error('ZIP central-directory spool is cleared')
    if (!this.#sealed) {
      await this.#flush()
      this.#sealed = true
    }
    return Object.freeze({
      chunkCount: this.#chunkCount,
      recordCount: this.#recordCount,
      byteLength: this.#byteLength,
    })
  }

  async readChunk(index: number): Promise<Uint8Array | undefined> {
    this.#requireHealthy()
    if (!this.#sealed || this.#cleared) {
      throw new Error('ZIP central-directory spool must be sealed before replay')
    }
    if (!Number.isSafeInteger(index) || index < 0) throw new RangeError('ZIP spool index is invalid')
    if (index >= this.#chunkCount) return undefined
    const database = await this.#openDatabase()
    const transaction = database.transaction(
      [ZIP_SPOOL_CHUNK_STORE, ZIP_SPOOL_NAMESPACE_STORE],
      'readonly',
    )
    await this.#requireOwnedNamespace(transaction.objectStore(ZIP_SPOOL_NAMESPACE_STORE))
    const stored = await requestResult<StoredZipChunk | undefined>(
      transaction.objectStore(ZIP_SPOOL_CHUNK_STORE).get(this.#chunkKey(index)),
    )
    await transactionCompletion(transaction)
    if (stored === undefined) throw new Error('ZIP central-directory spool lost a durable chunk')
    validateChunk(stored, this.#namespace, index)
    return stored.bytes.slice()
  }

  async clear(): Promise<void> {
    if (this.#cleared) return
    this.#pending.length = 0
    this.#pendingBytes = 0
    if (this.#heartbeatTimer !== undefined) clearInterval(this.#heartbeatTimer)
    this.#heartbeatTimer = undefined
    const databasePromise = this.#database
    if (databasePromise === undefined) {
      this.#cleared = true
      return
    }
    const database = await databasePromise
    try {
      const transaction = database.transaction(
        [ZIP_SPOOL_CHUNK_STORE, ZIP_SPOOL_NAMESPACE_STORE],
        'readwrite',
      )
      const namespaces = transaction.objectStore(ZIP_SPOOL_NAMESPACE_STORE)
      const registered = await requestResult<StoredZipNamespace | undefined>(
        namespaces.get(this.#namespace),
      )
      if (registered !== undefined) validateNamespace(registered)
      if (registered === undefined || registered.token === this.#token) {
        transaction.objectStore(ZIP_SPOOL_CHUNK_STORE).delete(namespaceRange(this.#namespace))
      }
      if (registered?.token === this.#token) namespaces.delete(this.#namespace)
      await transactionCompletion(transaction)
      this.#cleared = true
    } finally {
      database.close()
    }
  }

  async #flush(): Promise<void> {
    if (this.#pending.length === 0) return
    this.#requireHealthy()
    const bytes = concatenate(this.#pending, this.#pendingBytes)
    const index = this.#chunkCount
    if (index >= ZIP_SPOOL_MAXIMUM_CHUNK_INDEX) {
      throw new RangeError('ZIP central-directory spool exceeded its chunk index bound')
    }
    const database = await this.#openDatabase()
    await this.#ensureRegistered(database)
    const transaction = database.transaction(
      [ZIP_SPOOL_CHUNK_STORE, ZIP_SPOOL_NAMESPACE_STORE],
      'readwrite',
    )
    await this.#requireOwnedNamespace(transaction.objectStore(ZIP_SPOOL_NAMESPACE_STORE))
    const stored: StoredZipChunk = { namespace: this.#namespace, index, bytes }
    transaction.objectStore(ZIP_SPOOL_CHUNK_STORE).put(stored)
    await transactionCompletion(transaction)
    this.#pending.length = 0
    this.#pendingBytes = 0
    this.#chunkCount += 1
  }

  async #ensureRegistered(database: IDBDatabase): Promise<void> {
    if (this.#registered) return
    const now = this.#now()
    requireSafeTime(now)
    const transaction = database.transaction(
      [ZIP_SPOOL_CHUNK_STORE, ZIP_SPOOL_NAMESPACE_STORE],
      'readwrite',
    )
    const namespaces = transaction.objectStore(ZIP_SPOOL_NAMESPACE_STORE)
    const chunks = transaction.objectStore(ZIP_SPOOL_CHUNK_STORE)
    await sweepExpiredNamespaces(namespaces, chunks, now)
    const existing = await requestResult<StoredZipNamespace | undefined>(
      namespaces.get(this.#namespace),
    )
    if (existing !== undefined) {
      transaction.abort()
      throw new Error('ZIP central-directory namespace is already live')
    }
    namespaces.put(this.#namespaceRecord())
    await transactionCompletion(transaction)
    this.#registered = true
    this.#heartbeatTimer = setInterval(() => {
      this.#heartbeat().catch((error: unknown) => { this.#failure = error })
    }, this.#heartbeatMilliseconds)
  }

  async #heartbeat(): Promise<void> {
    if (!this.#registered || this.#cleared) return
    const database = await this.#openDatabase()
    const transaction = database.transaction(ZIP_SPOOL_NAMESPACE_STORE, 'readwrite')
    const store = transaction.objectStore(ZIP_SPOOL_NAMESPACE_STORE)
    const existing = await requestResult<StoredZipNamespace | undefined>(
      store.get(this.#namespace),
    )
    const now = this.#now()
    requireSafeTime(now)
    if (existing === undefined || existing.token !== this.#token ||
        existing.expiresAtMilliseconds <= now) {
      transaction.abort()
      throw new Error('ZIP central-directory namespace lease changed')
    }
    validateNamespace(existing)
    store.put(this.#namespaceRecord())
    await transactionCompletion(transaction)
  }

  async #requireOwnedNamespace(store: IDBObjectStore): Promise<void> {
    const existing = await requestResult<StoredZipNamespace | undefined>(
      store.get(this.#namespace),
    )
    const now = this.#now()
    requireSafeTime(now)
    if (existing !== undefined) validateNamespace(existing)
    if (existing === undefined || existing.token !== this.#token ||
        existing.expiresAtMilliseconds <= now) {
      throw new Error('ZIP central-directory namespace lease changed')
    }
  }

  #namespaceRecord(): StoredZipNamespace {
    const now = this.#now()
    requireSafeTime(now)
    const expiresAtMilliseconds = now + this.#leaseMilliseconds
    requireSafeTime(expiresAtMilliseconds)
    return { id: this.#namespace, token: this.#token, expiresAtMilliseconds }
  }

  #openDatabase(): Promise<IDBDatabase> {
    this.#database ??= openDatabase(this.#databaseName, (reason) => { this.#failure = reason })
    return this.#database
  }

  #chunkKey(index: number): [namespace: string, index: number] {
    return [this.#namespace, index]
  }

  #requireHealthy(): void {
    if (this.#failure !== undefined) {
      throw new Error('ZIP spool database is unavailable or version-obsolete', { cause: this.#failure })
    }
  }

  #requireWritable(): void {
    this.#requireHealthy()
    if (this.#sealed || this.#cleared) throw new Error('ZIP central-directory spool is not writable')
  }
}

async function openDatabase(
  name: string,
  invalidated: (reason: unknown) => void,
): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB ZIP metadata spooling is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, ZIP_SPOOL_DATABASE_VERSION)
  let rejected = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('upgradeneeded', (event) => {
      const database = request.result
      if (event.oldVersion < ZIP_SPOOL_DATABASE_VERSION) {
        // Chunk identity is structural. Resetting the pre-v3 delimiter schema
        // prevents one namespace from aliasing or sweeping another namespace.
        if (database.objectStoreNames.contains(ZIP_SPOOL_CHUNK_STORE)) {
          database.deleteObjectStore(ZIP_SPOOL_CHUNK_STORE)
        }
        if (database.objectStoreNames.contains(ZIP_SPOOL_NAMESPACE_STORE)) {
          database.deleteObjectStore(ZIP_SPOOL_NAMESPACE_STORE)
        }
        database.createObjectStore(ZIP_SPOOL_CHUNK_STORE, {
          keyPath: ['namespace', 'index'],
        })
        database.createObjectStore(ZIP_SPOOL_NAMESPACE_STORE, { keyPath: 'id' })
      }
    })
    request.addEventListener('blocked', () => {
      rejected = true
      reject(new DOMException(
        'ZIP metadata database upgrade is blocked by another tab',
        'InvalidStateError',
      ))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const database = request.result
      if (rejected) {
        database.close()
        return
      }
      database.addEventListener('versionchange', () => {
        invalidated(new DOMException('ZIP metadata database version changed', 'InvalidStateError'))
        database.close()
      })
      resolve(database)
    }, { once: true })
  })
}

function sweepExpiredNamespaces(
  namespaces: IDBObjectStore,
  chunks: IDBObjectStore,
  nowMilliseconds: number,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = namespaces.openCursor()
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) {
        resolve()
        return
      }
      const record = cursor.value as StoredZipNamespace
      try {
        validateNamespace(record)
      } catch (error) {
        reject(error)
        return
      }
      if (record.expiresAtMilliseconds <= nowMilliseconds) {
        chunks.delete(namespaceRange(record.id))
        cursor.delete()
      }
      cursor.continue()
    })
  })
}

function validateNamespace(record: StoredZipNamespace): void {
  if (typeof record.id !== 'string' || record.id.length === 0 ||
      typeof record.token !== 'string' || record.token.length === 0 ||
      !Number.isSafeInteger(record.expiresAtMilliseconds) ||
      record.expiresAtMilliseconds < 0) {
    throw new TypeError('ZIP central-directory namespace record is invalid')
  }
}

function validateChunk(
  record: StoredZipChunk,
  namespace: string,
  index: number,
): void {
  if (record.namespace !== namespace || record.index !== index ||
      !(record.bytes instanceof Uint8Array)) {
    throw new TypeError('ZIP central-directory chunk record is invalid')
  }
}

function namespaceRange(namespace: string): IDBKeyRange {
  return IDBKeyRange.bound(
    [namespace, 0],
    [namespace, ZIP_SPOOL_MAXIMUM_CHUNK_INDEX],
  )
}

function concatenate(chunks: readonly Uint8Array[], byteLength: number): Uint8Array {
  const output = new Uint8Array(byteLength)
  let offset = 0
  for (const chunk of chunks) {
    output.set(chunk, offset)
    offset += chunk.byteLength
  }
  return output
}

function randomNamespace(): string {
  if (typeof crypto === 'undefined') throw new Error('Secure randomness is unavailable')
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return [...bytes].map((value) => value.toString(16).padStart(2, '0')).join('')
}

function requirePositiveSafeInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value <= 0) throw new RangeError(`${label} must be positive`)
}

function requireSafeTime(value: number): void {
  if (!Number.isSafeInteger(value) || value < 0) throw new RangeError('ZIP spool clock is invalid')
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}
