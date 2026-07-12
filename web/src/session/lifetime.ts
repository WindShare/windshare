/** Owns polling and cancellation listeners for one receive-session run. */
export class SessionLifetime {
  readonly #controller = new AbortController()
  #externalAbortCleanup: (() => void) | undefined
  #poll: ReturnType<typeof setInterval> | undefined

  get signal(): AbortSignal {
    return this.#controller.signal
  }

  observeExternalAbort(
    signal: AbortSignal | undefined,
    onAbort: (reason: unknown) => void,
  ): void {
    if (signal === undefined) {
      return
    }
    const abort = () => {
      onAbort(signal.reason ?? new DOMException('Operation aborted', 'AbortError'))
    }
    if (signal.aborted) {
      abort()
      return
    }
    signal.addEventListener('abort', abort, { once: true })
    this.#externalAbortCleanup = () => signal.removeEventListener('abort', abort)
  }

  startPolling(milliseconds: number, poll: () => void): void {
    if (this.#poll !== undefined) {
      throw new Error('receive-session polling already started')
    }
    this.#poll = setInterval(poll, milliseconds)
  }

  stop(reason?: unknown): void {
    if (this.#poll !== undefined) {
      clearInterval(this.#poll)
      this.#poll = undefined
    }
    this.#externalAbortCleanup?.()
    this.#externalAbortCleanup = undefined
    if (!this.#controller.signal.aborted) {
      this.#controller.abort(reason)
    }
  }
}
