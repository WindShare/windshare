export const RELAY_BANDWIDTH_WAIT_VISIBILITY_THRESHOLD_MILLISECONDS = 500

export interface BandwidthWaitVisibilityOptions {
  readonly subscribe?: (listener: (waiting: boolean) => void) => () => void
  readonly onChange: (visible: boolean) => void
  readonly thresholdMilliseconds?: number
}

/**
 * Delays showing relay bandwidth waits briefly so momentary dips or concurrent
 * P2P handovers never flicker the task card, while clearing immediately once
 * bandwidth recovers.
 */
export class BandwidthWaitVisibility {
  readonly #onChange: (visible: boolean) => void
  readonly #thresholdMilliseconds: number
  #unsubscribe: (() => void) | undefined
  #timer: ReturnType<typeof setTimeout> | undefined
  #waiting = false
  #visible = false
  #closed = false

  constructor(options: BandwidthWaitVisibilityOptions) {
    this.#onChange = options.onChange
    this.#thresholdMilliseconds = options.thresholdMilliseconds ?? RELAY_BANDWIDTH_WAIT_VISIBILITY_THRESHOLD_MILLISECONDS
    if (options.subscribe !== undefined) {
      this.#unsubscribe = options.subscribe(waiting => this.#observe(waiting))
    }
  }

  get visible(): boolean {
    return this.#visible
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#waiting = false
    this.#clearTimer()
    const unsubscribe = this.#unsubscribe
    this.#unsubscribe = undefined
    unsubscribe?.()
    this.#visible = false
  }

  #observe(waiting: boolean): void {
    if (this.#closed || waiting === this.#waiting) return
    this.#waiting = waiting
    if (!waiting) {
      this.#clearTimer()
      if (!this.#visible) return
      this.#visible = false
      this.#notify(false)
      return
    }
    if (this.#thresholdMilliseconds <= 0) {
      this.#visible = true
      this.#notify(true)
      return
    }
    this.#clearTimer()
    this.#timer = setTimeout(() => {
      this.#timer = undefined
      if (this.#closed || !this.#waiting || this.#visible) return
      this.#visible = true
      this.#notify(true)
    }, this.#thresholdMilliseconds)
  }

  #clearTimer(): void {
    if (this.#timer === undefined) return
    clearTimeout(this.#timer)
    this.#timer = undefined
  }

  #notify(visible: boolean): void {
    try {
      this.#onChange(visible)
    } catch {
      // Visibility observation is passive and cannot alter transfer authority.
    }
  }
}
