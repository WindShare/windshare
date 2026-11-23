import {
  probeBrowserOutputCapabilities,
  type BrowserOutputCapabilities,
  type BrowserOutputWindow,
} from '../../output/backends/contract'
import type { BrowserCapabilitiesState } from './contracts'

/**
 * Coordinator-scoped browser capability probe cache.
 * Switching selections or shares never aborts an in-flight probe, and a failed
 * probe transitions to `'failed'` so callers can retry explicitly.
 */
export class BrowserCapabilityCache {
  readonly #windowPort: BrowserOutputWindow
  readonly #signal: AbortSignal
  readonly #isDisposed: () => boolean
  readonly #onStateChange: () => void
  #value: BrowserOutputCapabilities | undefined
  #pending: Promise<BrowserOutputCapabilities> | undefined
  #state: BrowserCapabilitiesState = 'probing'

  constructor(options: Readonly<{
    windowPort: BrowserOutputWindow
    signal: AbortSignal
    isDisposed: () => boolean
    onStateChange: () => void
  }>) {
    this.#windowPort = options.windowPort
    this.#signal = options.signal
    this.#isDisposed = options.isDisposed
    this.#onStateChange = options.onStateChange
  }

  get state(): BrowserCapabilitiesState {
    return this.#state
  }

  get value(): BrowserOutputCapabilities | undefined {
    return this.#value
  }

  ensure(): Promise<BrowserOutputCapabilities> {
    if (this.#value !== undefined) return Promise.resolve(this.#value)
    if (this.#pending !== undefined) return this.#pending
    if (this.#state !== 'probing') {
      this.#state = 'probing'
      this.#onStateChange()
    }
    const pending = probeBrowserOutputCapabilities(this.#windowPort, this.#signal)
    this.#pending = pending
    return pending.then(
      caps => {
        if (this.#pending === pending) this.#pending = undefined
        if (this.#isDisposed()) return caps
        this.#value = caps
        this.#state = 'ready'
        this.#onStateChange()
        return caps
      },
      (error: unknown) => {
        if (this.#pending === pending) this.#pending = undefined
        if (!this.#isDisposed()) {
          this.#state = 'failed'
          this.#onStateChange()
        }
        throw error
      },
    )
  }

  retry(): void {
    if (this.#isDisposed() || this.#value !== undefined) return
    void this.ensure().catch(() => undefined)
  }
}
