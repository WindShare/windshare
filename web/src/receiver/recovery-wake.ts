import type { ReconnectClock } from './recovery-clock'

// Short verification window after confirmed OS wake or network recovery so
// half-open TCP sockets detach quickly instead of waiting the full 45s timeout.
export const WAKE_PROBE_TIMEOUT_MILLISECONDS = 5_000
// Wall-clock advancement far beyond monotonic time indicates OS suspension
// (where monotonic clocks pause during sleep) rather than normal tab switching.
const WAKE_CLOCK_DRIFT_THRESHOLD_MILLISECONDS = 10_000

export type SystemWakeKind = 'online' | 'clock-jump' | 'visible'

interface VisibilityEventTarget extends Pick<EventTarget, 'addEventListener' | 'removeEventListener'> {
  readonly visibilityState?: DocumentVisibilityState
}

export interface SystemWakeMonitorOptions {
  readonly onWake: (kind: SystemWakeKind) => void
  readonly windowTarget?: Pick<EventTarget, 'addEventListener' | 'removeEventListener'>
  readonly documentTarget?: VisibilityEventTarget
  readonly wallNow?: () => number
  readonly monoNow?: () => number
}

/**
 * Event-driven OS wake and network-return detector. Avoids polling timers so
 * idle tabs consume no CPU and fake-timer tests remain deterministic.
 */
export class SystemWakeMonitor {
  readonly #onWake: (kind: SystemWakeKind) => void
  readonly #windowTarget: Pick<EventTarget, 'addEventListener' | 'removeEventListener'> | undefined
  readonly #documentTarget: VisibilityEventTarget | undefined
  readonly #wallNow: () => number
  readonly #monoNow: () => number
  #lastWall: number
  #lastMono: number
  #closed = false

  constructor(options: SystemWakeMonitorOptions) {
    this.#onWake = options.onWake
    this.#windowTarget = options.windowTarget ?? defaultWindowTarget()
    this.#documentTarget = options.documentTarget ?? defaultDocumentTarget()
    this.#wallNow = options.wallNow ?? (() => Date.now())
    this.#monoNow = options.monoNow ?? (() => performance.now())
    this.#lastWall = this.#wallNow()
    this.#lastMono = this.#monoNow()
    this.#windowTarget?.addEventListener('online', this.#onOnline)
    this.#documentTarget?.addEventListener('visibilitychange', this.#onVisibilityChange)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#windowTarget?.removeEventListener('online', this.#onOnline)
    this.#documentTarget?.removeEventListener('visibilitychange', this.#onVisibilityChange)
  }

  readonly #onOnline = (): void => {
    if (this.#closed) return
    this.#lastWall = this.#wallNow()
    this.#lastMono = this.#monoNow()
    this.#onWake('online')
  }

  readonly #onVisibilityChange = (): void => {
    if (this.#closed) return
    const wall = this.#wallNow()
    const mono = this.#monoNow()
    const wallDelta = wall - this.#lastWall
    const monoDelta = mono - this.#lastMono
    this.#lastWall = wall
    this.#lastMono = mono
    if (this.#documentTarget?.visibilityState !== 'visible') return
    this.#onWake(wallDelta - monoDelta >= WAKE_CLOCK_DRIFT_THRESHOLD_MILLISECONDS ? 'clock-jump' : 'visible')
  }
}

function defaultWindowTarget(): Pick<EventTarget, 'addEventListener' | 'removeEventListener'> | undefined {
  return typeof globalThis.addEventListener === 'function' ? globalThis : undefined
}

function defaultDocumentTarget(): VisibilityEventTarget | undefined {
  const doc = (globalThis as { readonly document?: VisibilityEventTarget }).document
  return doc !== undefined && typeof doc.addEventListener === 'function' ? doc : undefined
}

/** A retry request releases current delays; it never starts or cancels a handshake. */
export class RecoveryWake {
  readonly #waiting = new Set<() => void>()

  request(): void {
    for (const wake of this.#waiting) wake()
  }

  async sleep(clock: ReconnectClock, milliseconds: number, signal: AbortSignal): Promise<void> {
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
