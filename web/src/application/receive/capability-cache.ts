import {
  estimateAvailableStorageBytes,
  probeBrowserOutputCapabilities,
  type BrowserOutputCapabilities,
  type BrowserOutputWindow,
} from '../../output/backends/contract'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import { classifyNativeError } from '../../transfer/fault'
import type { BrowserCapabilitiesState } from './contracts'

export interface BrowserCapabilityCacheOptions {
  readonly windowPort: BrowserOutputWindow
  readonly signal: AbortSignal
  readonly outputTrace?: OutputTraceSource
  readonly isDisposed: () => boolean
  readonly onStateChange: () => void
  readonly onRecovered?: () => void
  readonly onStorageEstimateChanged?: () => void
}

/**
 * Coordinator-scoped browser capability probe cache.
 * Static browser features are probed once per session, while dynamic OPFS
 * storage availability refreshes silently in the background after mutations.
 */
export class BrowserCapabilityCache {
  readonly #windowPort: BrowserOutputWindow
  readonly #signal: AbortSignal
  readonly #outputTrace: OutputTraceSource | undefined
  readonly #isDisposed: () => boolean
  readonly #onStateChange: () => void
  readonly #onRecovered: (() => void) | undefined
  readonly #onStorageEstimateChanged: (() => void) | undefined
  #value: BrowserOutputCapabilities | undefined
  #pending: Promise<BrowserOutputCapabilities> | undefined
  #state: BrowserCapabilitiesState = 'probing'
  #storageRefreshEpoch = 0

  constructor(options: BrowserCapabilityCacheOptions) {
    this.#windowPort = options.windowPort
    this.#signal = options.signal
    this.#outputTrace = options.outputTrace
    this.#isDisposed = options.isDisposed
    this.#onStateChange = options.onStateChange
    this.#onRecovered = options.onRecovered
    this.#onStorageEstimateChanged = options.onStorageEstimateChanged
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
    const recovering = this.#state === 'failed'
    if (this.#state !== 'probing') {
      this.#state = 'probing'
      this.#onStateChange()
    }
    const pending = probeBrowserOutputCapabilities(this.#windowPort, this.#signal)
    this.#pending = pending
    return pending.then(
      caps => this.#handleProbeSuccess(pending, caps, recovering),
      (error: unknown) => this.#handleProbeFailure(pending, error),
    )
  }

  retry(onError?: (error: unknown) => void): void {
    if (this.#isDisposed() || this.#value !== undefined) return
    void this.ensure().catch((error: unknown) => {
      if (!this.#isDisposed()) onError?.(error)
    })
  }

  async refreshStorageEstimate(): Promise<void> {
    if (this.#isDisposed() || this.#value === undefined || !this.#value.opfs) return
    if (typeof this.#windowPort.navigator?.storage?.estimate !== 'function') return
    const epoch = ++this.#storageRefreshEpoch
    let nextBytes: bigint | null
    try {
      nextBytes = await estimateAvailableStorageBytes(this.#windowPort)
    } catch (error) {
      this.#traceFailure('storage_estimate_failed', error)
      return
    }
    if (this.#isDisposed() || epoch !== this.#storageRefreshEpoch || this.#value === undefined) return
    // Preserve the last known quota when a background estimate fails or returns non-finite numbers.
    if (nextBytes === null || nextBytes === this.#value.availableStorageBytes) return
    this.#value = Object.freeze({ ...this.#value, availableStorageBytes: nextBytes })
    this.#onStateChange()
    this.#onStorageEstimateChanged?.()
  }

  #handleProbeSuccess(
    pending: Promise<BrowserOutputCapabilities>,
    caps: BrowserOutputCapabilities,
    recovering: boolean,
  ): BrowserOutputCapabilities {
    if (this.#pending === pending) this.#pending = undefined
    if (this.#isDisposed()) return caps
    this.#value = caps
    this.#state = 'ready'
    this.#onStateChange()
    if (recovering) this.#onRecovered?.()
    return caps
  }

  #handleProbeFailure(
    pending: Promise<BrowserOutputCapabilities>,
    error: unknown,
  ): never {
    if (this.#pending === pending) this.#pending = undefined
    if (!this.#isDisposed()) {
      this.#state = 'failed'
      this.#traceFailure('capability_probe_failed', error)
      this.#onStateChange()
    }
    throw error
  }

  #traceFailure(transition: string, error: unknown): void {
    emitOutputTrace(this.#outputTrace, 'output_reservation', {
      transition,
      error_class: classifyNativeError(error),
    })
  }
}
