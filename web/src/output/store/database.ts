import type {
  ReceiveFileRecord,
  ReceiveTaskRecord,
  ZipSpoolChunkRecord,
} from './types'
import { fileRecordId, spoolChunkId } from './types'

export const RECEIVE_DATABASE_NAME = 'windshare-receive-v1'
export const RECEIVE_DATABASE_VERSION = 1

export const TASKS_STORE = 'tasks'
export const FILES_STORE = 'files'
export const ZIP_SPOOL_STORE = 'zip-spool'

export const BY_SHARE_INDEX = 'by-share'
export const BY_TASK_INDEX = 'by-task'

/**
 * Single unified IndexedDB for all browser receive persistence:
 * 3 simple JSON stores (`tasks`, `files`, `zip-spool`) with 2 secondary indexes.
 * Relies directly on IndexedDB's native structured clone and transaction atomicity.
 */
export class ReceiveStore {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
  }

  static async open(
    databaseName = RECEIVE_DATABASE_NAME,
    factory: IDBFactory | undefined = globalThis.indexedDB,
  ): Promise<ReceiveStore> {
    if (databaseName.length === 0) throw new TypeError('Receive database name must not be empty')
    if (factory === undefined) {
      throw new DOMException('IndexedDB is unavailable', 'NotSupportedError')
    }
    const database = await openReceiveDatabase(factory, databaseName)
    return new ReceiveStore(database)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async putTask(task: ReceiveTaskRecord): Promise<void> {
    await this.#run(TASKS_STORE, 'readwrite', tx => {
      tx.objectStore(TASKS_STORE).put(task)
    })
  }

  async getTask(taskId: string): Promise<ReceiveTaskRecord | undefined> {
    return this.#run(TASKS_STORE, 'readonly', tx =>
      requestResult<ReceiveTaskRecord | undefined>(tx.objectStore(TASKS_STORE).get(taskId)))
  }

  async listTasks(): Promise<readonly ReceiveTaskRecord[]> {
    const rows = await this.#run(TASKS_STORE, 'readonly', tx =>
      requestResult<ReceiveTaskRecord[]>(tx.objectStore(TASKS_STORE).getAll()))
    return Object.freeze(rows.sort((left, right) => right.updatedAt - left.updatedAt))
  }

  async listTasksByShare(shareInstance: string): Promise<readonly ReceiveTaskRecord[]> {
    const rows = await this.#run(TASKS_STORE, 'readonly', tx =>
      requestResult<ReceiveTaskRecord[]>(
        tx.objectStore(TASKS_STORE).index(BY_SHARE_INDEX).getAll(shareInstance),
      ))
    return Object.freeze(rows.sort((left, right) => right.updatedAt - left.updatedAt))
  }

  async putFile(file: ReceiveFileRecord): Promise<void> {
    await this.#run(FILES_STORE, 'readwrite', tx => {
      tx.objectStore(FILES_STORE).put(file)
    })
  }

  async getFile(taskId: string, fileId: string): Promise<ReceiveFileRecord | undefined> {
    return this.#run(FILES_STORE, 'readonly', tx =>
      requestResult<ReceiveFileRecord | undefined>(
        tx.objectStore(FILES_STORE).get(fileRecordId(taskId, fileId)),
      ))
  }

  async listTaskFiles(taskId: string): Promise<readonly ReceiveFileRecord[]> {
    const rows = await this.#run(FILES_STORE, 'readonly', tx =>
      requestResult<ReceiveFileRecord[]>(
        tx.objectStore(FILES_STORE).index(BY_TASK_INDEX).getAll(taskId),
      ))
    return Object.freeze(rows)
  }

  async putTaskAndFile(task: ReceiveTaskRecord, file: ReceiveFileRecord): Promise<void> {
    await this.#run([TASKS_STORE, FILES_STORE], 'readwrite', tx => {
      tx.objectStore(TASKS_STORE).put(task)
      tx.objectStore(FILES_STORE).put(file)
    })
  }

  async deleteTask(taskId: string): Promise<void> {
    await this.#run([TASKS_STORE, FILES_STORE, ZIP_SPOOL_STORE], 'readwrite', async tx => {
      tx.objectStore(TASKS_STORE).delete(taskId)
      await deleteByTaskIndex(tx.objectStore(FILES_STORE), taskId)
      await deleteByTaskIndex(tx.objectStore(ZIP_SPOOL_STORE), taskId)
    })
  }

  async putZipSpoolChunk(chunk: ZipSpoolChunkRecord): Promise<void> {
    await this.#run(ZIP_SPOOL_STORE, 'readwrite', tx => {
      tx.objectStore(ZIP_SPOOL_STORE).put(chunk)
    })
  }

  async getZipSpoolChunk(taskId: string, index: number): Promise<Uint8Array | undefined> {
    const row = await this.#run(ZIP_SPOOL_STORE, 'readonly', tx =>
      requestResult<ZipSpoolChunkRecord | undefined>(
        tx.objectStore(ZIP_SPOOL_STORE).get(spoolChunkId(taskId, index)),
      ))
    return row?.bytes
  }

  async deleteZipSpool(taskId: string): Promise<void> {
    await this.#run(ZIP_SPOOL_STORE, 'readwrite', async tx => {
      await deleteByTaskIndex(tx.objectStore(ZIP_SPOOL_STORE), taskId)
    })
  }

  async #run<T>(
    stores: string | readonly string[],
    mode: IDBTransactionMode,
    work: (tx: IDBTransaction) => T | Promise<T>,
  ): Promise<T> {
    const tx = this.#database.transaction(stores as string | string[], mode)
    const done = transactionCompletion(tx)
    done.catch(() => undefined)
    try {
      const value = await work(tx)
      await done
      return value
    } catch (error) {
      try { tx.abort() } catch { /* Transaction may already be finished. */ }
      await done.catch(() => undefined)
      throw error
    }
  }
}

export function installReceiveSchema(database: IDBDatabase): void {
  for (const name of Array.from(database.objectStoreNames)) {
    database.deleteObjectStore(name)
  }
  const tasks = database.createObjectStore(TASKS_STORE, { keyPath: 'taskId' })
  tasks.createIndex(BY_SHARE_INDEX, 'shareInstance', { unique: false })

  const files = database.createObjectStore(FILES_STORE, { keyPath: 'id' })
  files.createIndex(BY_TASK_INDEX, 'taskId', { unique: false })

  const spool = database.createObjectStore(ZIP_SPOOL_STORE, { keyPath: 'id' })
  spool.createIndex(BY_TASK_INDEX, 'taskId', { unique: false })
}

function openReceiveDatabase(factory: IDBFactory, name: string): Promise<IDBDatabase> {
  const request = factory.open(name, RECEIVE_DATABASE_VERSION)
  request.addEventListener('upgradeneeded', () => {
    installReceiveSchema(request.result)
  })
  let blocked = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('blocked', () => {
      blocked = true
      reject(new DOMException('Receive database upgrade is blocked by another tab', 'InvalidStateError'))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (blocked) {
        request.result.close()
        return
      }
      request.result.addEventListener('versionchange', () => request.result.close())
      resolve(request.result)
    }, { once: true })
  })
}

async function deleteByTaskIndex(store: IDBObjectStore, taskId: string): Promise<void> {
  const keys = await requestResult<IDBValidKey[]>(store.index(BY_TASK_INDEX).getAllKeys(taskId))
  for (const key of keys) store.delete(key)
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(tx: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    tx.addEventListener('complete', () => resolve(), { once: true })
    tx.addEventListener('abort', () => reject(tx.error ?? new DOMException('Transaction aborted', 'AbortError')), { once: true })
    tx.addEventListener('error', () => reject(tx.error ?? new DOMException('Transaction failed', 'UnknownError')), { once: true })
  })
}
