import { runInitialJoinAttempt, type InitialJoinAttempt } from './connection-attempt/initial'
import { defaultReconnectBackoff, systemReconnectClock, waitingReconnectBackoff, type ReconnectClock } from './recovery-clock'
import { isTerminalRecoveryFailure, recoveryBandwidthWait, recoveryRetryAfter } from './recovery-failure'
import { RecoveryWake } from './recovery-wake'
import type { RecoveryObservation } from './recovery-observation'

const INITIAL_JOIN_WINDOW_MILLISECONDS = 30_000

export type InitialJoinState =
  | { readonly kind: 'connecting' }
  | { readonly kind: 'waiting'; readonly reason: 'sender' | 'bandwidth' }
  | { readonly kind: 'paused'; readonly reason: 'unreachable' | 'bandwidth' }
type JoinChoice = 'retry' | 'continue'

export class InitialJoinUnavailableError extends Error {
  constructor(cause: unknown) {
    super(recoveryBandwidthWait(cause) > 0
      ? 'The connection timed out while waiting for relay bandwidth.'
      : 'The sender is temporarily unreachable. Retry or continue waiting; the link may still be valid.', { cause })
    this.name = 'InitialJoinUnavailableError'
  }
}

/** The gateway keeps its capability while this control waits for an explicit extension. */
export class InitialJoinControl {
  #choose: ((choice: JoinChoice) => void) | undefined
  readonly #wake = new RecoveryWake()

  request(choice: JoinChoice): void {
    this.#choose?.(choice)
    this.#wake.request()
  }

  sleep(clock: ReconnectClock, milliseconds: number, signal: AbortSignal): Promise<void> {
    return this.#wake.sleep(clock, milliseconds, signal)
  }

  async wait(signal: AbortSignal, publish: () => void): Promise<JoinChoice> {
    signal.throwIfAborted()
    let abort!: () => void
    try {
      return await new Promise<JoinChoice>((resolve, reject) => {
        this.#choose = resolve
        abort = () => reject(signal.reason)
        signal.addEventListener('abort', abort, { once: true })
        publish()
      })
    } finally {
      this.#choose = undefined
      signal.removeEventListener('abort', abort)
    }
  }
}

export interface InitialJoinOptions {
  readonly clock?: ReconnectClock
  readonly windowMilliseconds?: number
  readonly control?: InitialJoinControl
  readonly onState?: (state: InitialJoinState) => void
  readonly observe?: (observation: RecoveryObservation) => void
}

interface InitialJoinAttemptOptions<T> extends InitialJoinOptions {
  readonly signal: AbortSignal
  readonly connect: (attempt: InitialJoinAttempt) => Promise<T>
  readonly close: (value: T) => Promise<void>
}

export async function runInitialJoin<T>(options: InitialJoinAttemptOptions<T>): Promise<T> {
  return new InitialJoinRecovery(options).run()
}

class InitialJoinRecovery<T> {
  readonly #options: InitialJoinAttemptOptions<T>
  readonly #clock: ReconnectClock
  readonly #window: number
  #deadline: number
  #continuous = false
  #attempt = 0
  #failure: unknown

  constructor(options: InitialJoinAttemptOptions<T>) {
    this.#options = options
    this.#clock = options.clock ?? systemReconnectClock
    this.#window = options.windowMilliseconds ?? INITIAL_JOIN_WINDOW_MILLISECONDS
    if (!Number.isFinite(this.#window) || this.#window <= 0) throw new RangeError('Initial join window must be positive')
    this.#deadline = this.#clock.now() + this.#window
  }

  async run(): Promise<T> {
    while (true) {
      this.#options.signal.throwIfAborted()
      await this.#ensureWindow()
      this.#publish(this.#continuous ? { kind: 'waiting', reason: 'sender' } : { kind: 'connecting' })
      try { return await this.#connect() } catch (error) { await this.#recover(error) }
    }
  }

  async #ensureWindow(): Promise<void> {
    if (this.#continuous || this.#clock.now() < this.#deadline) return
    if (this.#options.control === undefined) throw new InitialJoinUnavailableError(this.#failure)
    const bandwidth = recoveryBandwidthWait(this.#failure) > 0
    this.#observe({ transition: 'waiting', failure: this.#failure, ...(bandwidth && { waitReason: 'bandwidth' }) })
    const choice = await this.#options.control.wait(this.#options.signal, () => {
      this.#publish({ kind: 'paused', reason: bandwidth ? 'bandwidth' : 'unreachable' })
    })
    this.#continuous = choice === 'continue'
    this.#deadline = this.#clock.now() + this.#window
    this.#attempt = 0
    this.#observe({ transition: 'retry_requested' })
  }

  async #connect(): Promise<T> {
    this.#attempt += 1
    this.#observe({ transition: 'attempt_started' })
    const connected = await runInitialJoinAttempt({
      deadline: this.#continuous ? Infinity : this.#deadline,
      parent: this.#options.signal, now: () => this.#clock.now(),
      connect: this.#options.connect, close: this.#options.close,
      onBandwidthWait: milliseconds => {
        this.#publish({ kind: 'waiting', reason: 'bandwidth' })
        this.#observe({ transition: 'waiting', waitReason: 'bandwidth', delayMilliseconds: milliseconds })
      },
    })
    this.#observe({ transition: 'connected' })
    return connected
  }

  async #recover(error: unknown): Promise<void> {
    this.#options.signal.throwIfAborted()
    this.#observe({ transition: 'attempt_failed', failure: error })
    if (isTerminalRecoveryFailure(error)) {
      this.#observe({ transition: 'terminal', failure: error })
      throw error
    }
    this.#failure = error
    const bandwidth = recoveryBandwidthWait(error) > 0
    const required = recoveryRetryAfter(error)
    const delay = this.#continuous ? waitingReconnectBackoff() : defaultReconnectBackoff(this.#attempt - 1)
    const backoff = this.#continuous ? delay : Math.max(0, Math.min(delay, this.#deadline - this.#clock.now()))
    const wait = Math.max(required, backoff)
    if (bandwidth) this.#publish({ kind: 'waiting', reason: 'bandwidth' })
    this.#observe({ transition: 'waiting', delayMilliseconds: wait, failure: error,
      ...(bandwidth && { waitReason: 'bandwidth' }) })
    const { control, signal } = this.#options
    // A retry click cannot bypass the node's capacity or authorization deadline.
    if (required > 0 || control === undefined) await this.#clock.sleep(wait, signal)
    else await control.sleep(this.#clock, wait, signal)
  }

  #publish(state: InitialJoinState): void {
    try { this.#options.onState?.(state) } catch { /* Presentation cannot own connection recovery. */ }
  }

  #observe(value: Omit<RecoveryObservation, 'attempt' | 'phase'>): void {
    try {
      this.#options.observe?.({ attempt: this.#attempt, phase: this.#continuous ? 'waiting' : 'initial', ...value })
    } catch { /* Passive. */ }
  }
}
