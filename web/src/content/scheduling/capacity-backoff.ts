import type { BandwidthWaitError, CapacityPressure } from '../../contracts/capacity'
import type { SamplingOutcome } from './sampling'

export type ContentAdmissionOutcome = SamplingOutcome | BandwidthWaitError

type CapacityState =
  | { readonly kind: 'ready' }
  | { readonly kind: 'cooldown'; readonly until: number }
  | { readonly kind: 'waiting'; readonly until: number; readonly pressure: CapacityPressure; readonly unsubscribe: () => void }
  | { readonly kind: 'probing'; readonly until: number; readonly pressure: CapacityPressure }

export class LaneCapacityBackoff {
  #state: CapacityState = { kind: 'ready' }
  readonly #wake: () => void

  constructor(wake: () => void) { this.#wake = wake }

  readiness(now: number, inflight: number): 'ready' | 'recoverable' | 'waiting' {
    const state = this.#state
    if (state.kind === 'ready') return 'ready'
    if (state.kind === 'probing') return 'waiting'
    if (now >= state.until) {
      this.#transition({ kind: 'ready' })
      return 'ready'
    }
    // Let the old requests settle first so their late failures cannot re-arm
    // the lane while the recovery request is checking it.
    return state.kind === 'waiting' && inflight === 0 && state.pressure.cleared ? 'recoverable' : 'waiting'
  }

  deadline(): number {
    const state = this.#state
    return state.kind === 'waiting' || state.kind === 'cooldown' ? state.until : Infinity
  }

  start(now: number, inflight: number, recovery: boolean): boolean {
    const readiness = this.readiness(now, inflight)
    if (!recovery) return readiness === 'ready'
    const state = this.#state
    if (readiness !== 'recoverable' || state.kind !== 'waiting') return false
    this.#transition({ kind: 'probing', until: state.until, pressure: state.pressure })
    return true
  }

  settle(outcome: ContentAdmissionOutcome, now: number): void {
    const state = this.#state
    if (typeof outcome === 'string') {
      if (state.kind === 'probing') {
        this.#transition(outcome === 'completed' && state.pressure.cleared
          ? { kind: 'ready' } : { kind: 'cooldown', until: state.until })
      }
      return
    }

    const until = Math.max('until' in state ? state.until : 0, now + outcome.retryAfterMilliseconds)
    // A failed probe spends the early opportunity for this wait, even when
    // the relay reports another brief release before the fallback deadline.
    if (state.kind === 'probing' || (state.kind === 'cooldown' && state.until > now) || outcome.pressure === undefined) {
      this.#transition({ kind: 'cooldown', until })
    } else {
      const pressure = outcome.pressure
      if (state.kind === 'waiting' && state.pressure === pressure) {
        this.#state = { ...state, until }
        return
      }
      this.#transition({ kind: 'waiting', until, pressure, unsubscribe: pressure.subscribeClearance(() => this.#wake()) })
    }
  }

  close(): void { this.#transition({ kind: 'ready' }) }

  #transition(next: CapacityState): void {
    if (this.#state.kind === 'waiting') this.#state.unsubscribe()
    this.#state = next
  }
}
