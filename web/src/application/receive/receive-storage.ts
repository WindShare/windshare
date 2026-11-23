import { RECEIVE_DATABASE_NAME, ReceiveStore } from '../../output/store/database'

export interface ReceiveStoreAccess {
  readonly requireStore: () => Promise<ReceiveStore>
  readonly openStoreOptional: () => Promise<ReceiveStore | undefined>
}

/** Only admitted output operations can open the database. Writers keep it until their locks are released. */
export class ReceiveStorage {
  readonly #access: ReceiveStoreAccess
  readonly #operations = new Set<Promise<void>>()
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

  close(): Promise<void> {
    this.#closing ??= Promise.allSettled(this.#operations).then(async () => {
      const store = await this.#store?.catch(() => undefined)
      store?.close()
    })
    return this.#closing
  }
}
