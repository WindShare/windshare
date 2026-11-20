import { runInitialJoinAttempt, type InitialJoinAttempt } from './connection-attempt/initial'
import { defaultReconnectBackoff, systemReconnectClock, waitingReconnectBackoff, type ReconnectClock } from './recovery-clock'
import { AllRelaysRestrictedError, isTerminalRecoveryFailure, latestRecoveryRetry, recoveryRetry } from './recovery-failure'
import type { RelayRecovery } from './relay-selection/recovery'
import { RecoveryWake } from './recovery-wake'
import type { RecoveryObservation } from './recovery-observation'

const INITIAL_JOIN_WINDOW_MILLISECONDS = 30_000

type JoinHoldReason = 'bandwidth' | 'authorization'
type JoinPauseReason = 'unreachable' | JoinHoldReason
export type InitialJoinState =
  | { readonly kind: 'connecting' }
  | { readonly kind: 'waiting'; readonly reason: 'sender' | JoinHoldReason; readonly retryAt?: number }
  | { readonly kind: 'paused'; readonly reason: JoinPauseReason; readonly retryAt?: number }
type JoinChoice = 'retry' | 'continue'

interface JoinFailure {
  readonly error: unknown
  readonly reason: JoinPauseReason
  readonly retryAt?: number
  readonly waitReason?: RecoveryObservation['waitReason']
}

const UNAVAILABLE_MESSAGES = {
  authorization: 'This share is temporarily unavailable through official relays.',
  bandwidth: 'The connection timed out while waiting for relay bandwidth.',
  unreachable: 'The sender is temporarily unreachable. Retry or continue waiting; the link may still be valid.',
} satisfies Record<JoinPauseReason, string>

export class InitialJoinUnavailableError extends Error {
  constructor(cause: unknown, reason: JoinPauseReason) {
    super(UNAVAILABLE_MESSAGES[reason], { cause })
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

  async wait(
    signal: AbortSignal,
    publish: () => void,
    accept: (choice: JoinChoice) => boolean = () => true,
  ): Promise<JoinChoice> {
    signal.throwIfAborted()
    let abort!: () => void
    try {
      return await new Promise<JoinChoice>((resolve, reject) => {
        this.#choose = choice => {
          if (accept(choice)) resolve(choice)
        }
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
  readonly relayRecovery?: Pick<RelayRecovery, 'nextRetry' | 'recordFailure'>
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
  #failure: JoinFailure | undefined

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
    const failure = this.#failure
    const reason = failure?.reason ?? 'unreachable'
    const retryAt = failure?.retryAt
    if (this.#options.control === undefined) throw new InitialJoinUnavailableError(failure?.error, reason)
    const remaining = retryAt === undefined ? 0 : Math.max(0, retryAt - this.#clock.now())
    let waitReason: RecoveryObservation['waitReason']
    if (reason !== 'unreachable') waitReason = reason
    else if (remaining > 0) waitReason = failure?.waitReason
    this.#observe({
      transition: 'waiting', failure: failure?.error,
      ...(waitReason !== undefined && { waitReason }),
      ...(remaining > 0 && { delayMilliseconds: remaining }),
    })
    const choice = await this.#options.control.wait(
      this.#options.signal,
      () => {
        const activeRetryAt = retryAt !== undefined && retryAt > this.#clock.now() ? retryAt : undefined
        this.#publish({ kind: 'paused', reason, ...(activeRetryAt !== undefined && { retryAt: activeRetryAt }) })
      },
      // A retry click cannot bypass the node's capacity or authorization deadline;
      // continuing to wait honors the remaining cooldown before dialing automatically.
      selected => selected === 'continue' || retryAt === undefined || this.#clock.now() >= retryAt,
    )
    this.#continuous = choice === 'continue'
    this.#deadline = this.#clock.now() + this.#window
    this.#attempt = 0
    this.#observe({ transition: 'retry_requested' })
    await this.#waitRemainingCooldown(failure)
    this.#failure = undefined
  }

  async #waitRemainingCooldown(failure: JoinFailure | undefined): Promise<void> {
    if (failure === undefined || failure.retryAt === undefined) return
    const { retryAt } = failure
    const wait = Math.max(0, retryAt - this.#clock.now())
    if (wait <= 0) return
    const reason = failure.reason === 'unreachable' ? 'sender' : failure.reason
    this.#publish({ kind: 'waiting', reason, retryAt })
    this.#observe({
      transition: 'waiting', delayMilliseconds: wait, failure: failure.error,
      waitReason: failure.waitReason ?? 'server',
    })
    await this.#clock.sleep(wait, this.#options.signal)
  }

  async #connect(): Promise<T> {
    this.#attempt += 1
    this.#observe({ transition: 'attempt_started' })
    const connected = await runInitialJoinAttempt({
      deadline: this.#continuous ? Infinity : this.#deadline,
      parent: this.#options.signal, now: () => this.#clock.now(),
      connect: this.#options.connect, close: this.#options.close,
      onRelayFailure: (base, failure) => this.#options.relayRecovery?.recordFailure(base, failure),
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
    const now = this.#clock.now()
    const retry = latestRecoveryRetry(this.#options.relayRecovery?.nextRetry(), recoveryRetry(error, now))
    const bandwidthWait = retry?.reason === 'bandwidth' ? 'bandwidth' : undefined
    const reason = error instanceof AllRelaysRestrictedError ? 'authorization' : bandwidthWait ?? 'unreachable'
    const required = Math.max(0, (retry?.retryAt ?? now) - now)
    const requiredUntil = required > 0 ? now + required : undefined
    const waitReason = retry?.reason ?? 'backoff'
    this.#failure = {
      error, reason, waitReason,
      ...(requiredUntil !== undefined && { retryAt: requiredUntil }),
    }
    // If a mandatory cooldown outlives the initial window, pausing immediately
    // avoids sleeping with no further automatic attempt ahead.
    if (!this.#continuous && requiredUntil !== undefined && requiredUntil >= this.#deadline) {
      this.#deadline = now
      return
    }
    const delay = this.#continuous ? waitingReconnectBackoff() : defaultReconnectBackoff(this.#attempt - 1)
    const backoff = this.#continuous ? delay : Math.max(0, Math.min(delay, this.#deadline - now))
    const wait = Math.max(required, backoff)
    const retryAt = now + wait
    if (reason !== 'unreachable') this.#publish({ kind: 'waiting', reason, retryAt })
    else if (this.#continuous) this.#publish({ kind: 'waiting', reason: 'sender', retryAt })
    this.#observe({ transition: 'waiting', delayMilliseconds: wait, failure: error, waitReason })
    await this.#sleep(required, retryAt)
  }

  async #sleep(required: number, retryAt: number): Promise<void> {
    const { control, signal } = this.#options
    // A retry click cannot bypass the node's capacity or authorization deadline.
    if (required > 0) await this.#clock.sleep(required, signal)
    const remaining = Math.max(0, retryAt - this.#clock.now())
    if (remaining <= 0) return
    if (control === undefined) await this.#clock.sleep(remaining, signal)
    else await control.sleep(this.#clock, remaining, signal)
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
