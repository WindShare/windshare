import type { V2ReconnectClock } from './recovery-clock'

/** A retry request releases current delays; it never starts or cancels a handshake. */
export class RecoveryWake {
  readonly #waiting = new Set<() => void>()

  request(): void {
    for (const wake of this.#waiting) wake()
  }

  async sleep(clock: V2ReconnectClock, milliseconds: number, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    const delay = new AbortController()
    let wake!: () => void
    const requested = new Promise<void>(resolve => { wake = resolve })
    this.#waiting.add(wake)
    try {
      await Promise.race([requested, clock.sleep(milliseconds, AbortSignal.any([signal, delay.signal]))])
      signal.throwIfAborted()
    } finally {
      this.#waiting.delete(wake)
      delay.abort()
    }
  }
}
