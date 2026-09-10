import type { LanePerformance } from './performance'

export const PROBE_INTERVAL_MILLISECONDS = 5000
export const HEDGE_CHECK_MILLISECONDS = 100
const HEDGE_DELAY_FACTOR = 2
export const MAXIMUM_SUPPLEMENTS = 2
export type DispatchPurpose = 'content' | 'probe' | 'rescue'

/** Supplement limits are shared by all reads; probes never keep an idle download alive. */
export class LaneExploration {
  #active = 0
  #probes = 0
  #lastProbe: number | undefined

  acquire(purpose: 'probe' | 'rescue', now: number): boolean {
    if (this.#active >= MAXIMUM_SUPPLEMENTS) return false
    if (purpose === 'probe') {
      if (this.#probes !== 0 ||
          (this.#lastProbe !== undefined && now - this.#lastProbe < PROBE_INTERVAL_MILLISECONDS)) return false
      this.#probes += 1
      this.#lastProbe = now
    }
    this.#active += 1
    return true
  }

  release(purpose: 'probe' | 'rescue'): void {
    this.#active -= 1
    if (purpose === 'probe') this.#probes -= 1
  }
}

export function probeDue(performance: LanePerformance, now: number): boolean {
  return performance.pendingBytes === 0 &&
    (performance.lastAttempt === undefined || now - performance.lastAttempt >= PROBE_INTERVAL_MILLISECONDS)
}

export function rescueDue(elapsed: number, primaryEstimate: number, alternativeEstimate: number): boolean {
  return elapsed >= Math.max(HEDGE_CHECK_MILLISECONDS,
    HEDGE_DELAY_FACTOR * Math.min(primaryEstimate, alternativeEstimate))
}
