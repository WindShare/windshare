import type { ProgressChange, TransferProgressSource } from '../../../transfer/progress/observation'

export const RECEIVE_PRESENTATION_INTERVAL_MS = 250

export interface PresentationClock {
  now(): number
  schedule(delay: number, callback: () => void): { cancel(): void }
}
export const BROWSER_PRESENTATION_CLOCK: PresentationClock = {
  now: () => performance.now(),
  schedule: (delay, callback) => {
    const timer = setTimeout(callback, delay)
    return { cancel: () => clearTimeout(timer) }
  },
}

/** Only presentation is paced. Ledger, output, and command facts advance synchronously. */
export class ReceivePresentation {
  readonly #clock: PresentationClock
  readonly #publish: (progress: TransferProgressSource | undefined, stateChanged: boolean) => void
  #source: TransferProgressSource | undefined
  #pending: { cancel(): void } | undefined
  #generation = 0
  #lastPublished = -Infinity
  #progressChanged = false
  #stateChanged = false
  #closed = false

  constructor(publish: (progress: TransferProgressSource | undefined, stateChanged: boolean) => void, clock = BROWSER_PRESENTATION_CLOCK) {
    this.#publish = publish
    this.#clock = clock
  }

  observe(source: TransferProgressSource, change: ProgressChange): void {
    if (this.#closed) return
    if (this.#source !== source) {
      this.reset()
      this.#source = source
      change = 'transition'
    }
    this.#progressChanged = true
    this.#request(change)
  }

  changed(change: ProgressChange): void {
    if (this.#closed) return
    this.#stateChanged = true
    this.#request(change)
  }

  flush(): void {
    this.#pending?.cancel()
    this.#pending = undefined
    if (this.#closed || (!this.#progressChanged && !this.#stateChanged)) return
    const source = this.#progressChanged ? this.#source : undefined
    const stateChanged = this.#stateChanged
    this.#progressChanged = false
    this.#stateChanged = false
    this.#lastPublished = this.#clock.now()
    this.#publish(source, stateChanged)
  }

  reset(): void {
    this.#generation++
    this.#pending?.cancel()
    this.#pending = undefined
    this.#source = undefined
    this.#progressChanged = false
    this.#lastPublished = -Infinity
  }

  close(): void {
    this.reset()
    this.#stateChanged = false
    this.#closed = true
  }

  #request(change: ProgressChange): void {
    const delay = this.#lastPublished + RECEIVE_PRESENTATION_INTERVAL_MS - this.#clock.now()
    if (change === 'transition' || delay <= 0) { this.flush(); return }
    if (this.#pending !== undefined) return
    const generation = this.#generation
    this.#pending = this.#clock.schedule(delay, () => {
      if (generation === this.#generation) this.flush()
    })
  }
}
