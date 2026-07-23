/**
 * Admits only active work. Callers retain compact domain cursors instead of
 * turning a wide catalog generation into an equally wide promise queue.
 */
export class BoundedTaskPool {
  readonly #limit: number
  readonly #active = new Set<Promise<void>>()
  #failure: { readonly reason: unknown } | undefined

  constructor(limit: number) {
    if (!Number.isSafeInteger(limit) || limit <= 0) {
      throw new RangeError('bounded task limit must be a positive safe integer')
    }
    this.#limit = limit
  }

  get hasCapacity(): boolean {
    return this.#active.size < this.#limit
  }

  run(task: () => Promise<void>): void {
    if (!this.hasCapacity) {
      throw new Error('bounded task pool has no available slot')
    }
    const running = Promise.resolve()
      .then(task)
      .catch((reason: unknown) => {
        this.#failure ??= Object.freeze({ reason })
      })
      .finally(() => {
        this.#active.delete(running)
      })
    this.#active.add(running)
  }

  async waitForCapacity(): Promise<void> {
    while (!this.hasCapacity) {
      await Promise.race(this.#active)
    }
  }

  async drain(): Promise<void> {
    await this.settle()
    if (this.#failure !== undefined) {
      throw this.#failure.reason
    }
  }

  async settle(): Promise<void> {
    while (this.#active.size > 0) {
      await Promise.all(this.#active)
    }
  }
}
