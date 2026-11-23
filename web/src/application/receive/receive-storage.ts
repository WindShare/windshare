import { RECEIVE_DATABASE_NAME, ReceiveStore } from '../../output/store/database'

/** Output operations keep their database connection until their writers and Web Locks have closed. */
export class ReceiveStorage {
  readonly #factory: IDBFactory | undefined
  readonly #name: string
  readonly #operations = new Set<Promise<void>>()
  #store: Promise<ReceiveStore> | undefined
  #closing: Promise<void> | undefined

  constructor(factory: IDBFactory | undefined, name = RECEIVE_DATABASE_NAME) {
    this.#factory = factory
    this.#name = name
  }

  run(operation: () => Promise<void>): Promise<void> {
    if (this.#closing !== undefined) return Promise.resolve()
    const pending = Promise.resolve().then(operation)
    this.#operations.add(pending)
    return pending.finally(() => this.#operations.delete(pending))
  }

  require(): Promise<ReceiveStore> {
    this.#store ??= ReceiveStore.open(this.#name, this.#factory).catch((error: unknown) => {
      this.#store = undefined
      throw error
    })
    return this.#store
  }

  async optional(): Promise<ReceiveStore | undefined> {
    if (typeof this.#factory?.open !== 'function') return undefined
    return this.require().catch(() => undefined)
  }

  close(): Promise<void> {
    this.#closing ??= Promise.allSettled(this.#operations).then(async () => {
      const store = await this.#store
      store?.close()
    })
    return this.#closing
  }
}
