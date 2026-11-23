import { RECEIVE_DATABASE_NAME, ReceiveStore } from '../../output/store/database'

export interface ReceiveStoreAccess {
  readonly requireStore: () => Promise<ReceiveStore>
  readonly openStoreOptional: () => Promise<ReceiveStore | undefined>
  readonly waitForTaskWrites: (taskId?: string) => Promise<void>
}

/** Only admitted output operations can open the database. Writers keep it until their locks are released. */
export class ReceiveStorage {
  readonly #access: ReceiveStoreAccess
  readonly #operations = new Set<Promise<void>>()
  readonly #pendingTaskWrites = new Map<string, Promise<void>>()
  #store: Promise<ReceiveStore> | undefined
  #closing: Promise<void> | undefined

  constructor(factory: IDBFactory | undefined, name = RECEIVE_DATABASE_NAME) {
    const requireStore = (): Promise<ReceiveStore> => {
      if (this.#closing !== undefined) return Promise.reject(new DOMException('Receiver closed', 'AbortError'))
      this.#store ??= ReceiveStore.open(name, factory).catch((error: unknown) => {
        this.#store = undefined
        throw error
      })
      return this.#store
    }
    this.#access = {
      requireStore,
      openStoreOptional: async () => typeof factory?.open === 'function' ? requireStore().catch(() => undefined) : undefined,
      waitForTaskWrites: async (taskId?: string) => {
        if (taskId !== undefined) {
          await this.#pendingTaskWrites.get(taskId)?.catch(() => undefined)
          return
        }
        const pending = [...this.#pendingTaskWrites.values()]
        if (pending.length > 0) await Promise.allSettled(pending)
      },
    }
  }

  run(operation: (access: ReceiveStoreAccess) => Promise<void>): Promise<void> {
    if (this.#closing !== undefined) return Promise.resolve()
    const pending = Promise.resolve().then(() => {
      if (this.#closing === undefined) return operation(this.#access)
    })
    this.#operations.add(pending)
    return pending.finally(() => this.#operations.delete(pending))
  }

  /**
   * Starts a background per-task persistence write synchronously so it opens the store
   * before an immediate `close()`, while letting subsequent scans and task mutations wait for it.
   */
  runTaskWrite(taskId: string, operation: (access: ReceiveStoreAccess) => Promise<void>): Promise<void> {
    if (this.#closing !== undefined) return Promise.resolve()
    const previous = this.#pendingTaskWrites.get(taskId)
    let started: Promise<void>
    if (previous === undefined) {
      try {
        started = Promise.resolve(operation(this.#access))
      } catch (error) {
        started = Promise.reject(error)
      }
    } else {
      started = previous.catch(() => undefined).then(() => {
        if (this.#closing === undefined) return operation(this.#access)
      })
    }
    const tracked = started.finally(() => {
      this.#operations.delete(tracked)
      if (this.#pendingTaskWrites.get(taskId) === tracked) {
        this.#pendingTaskWrites.delete(taskId)
      }
    })
    this.#operations.add(tracked)
    this.#pendingTaskWrites.set(taskId, tracked)
    return tracked
  }

  close(): Promise<void> {
    this.#closing ??= Promise.allSettled(this.#operations).then(async () => {
      const store = await this.#store?.catch(() => undefined)
      store?.close()
    })
    return this.#closing
  }
}
