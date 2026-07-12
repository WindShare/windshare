/** Owns the one-shot result shared by start, close, and asynchronous pumps. */
export class SessionCompletion {
  readonly promise: Promise<void>
  readonly #resolve: () => void
  readonly #reject: (reason?: unknown) => void

  constructor() {
    let resolve!: () => void
    let reject!: (reason?: unknown) => void
    this.promise = new Promise<void>((innerResolve, innerReject) => {
      resolve = innerResolve
      reject = innerReject
    })
    this.#resolve = resolve
    this.#reject = reject
  }

  succeed(): void {
    this.#resolve()
  }

  fail(reason: unknown): void {
    this.#reject(reason)
  }

  async waitIgnoringFailure(): Promise<void> {
    try {
      await this.promise
    } catch {
      // State exposes termination; close callers need not catch the same reason twice.
    }
  }
}
