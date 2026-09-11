export const PROBE_INTERVAL_MILLISECONDS = 5000
export const HEDGE_CHECK_MILLISECONDS = 10
const HEDGE_DELAY_FACTOR = 2
export const MAXIMUM_SUPPLEMENTS = 2
export type DispatchPurpose = 'content' | 'probe' | 'rescue'

/** Rescue authority is independent of the budget for sampling unique queued work. */
export class LaneRescues {
  #active = 0
  acquire(): boolean {
    if (this.#active >= MAXIMUM_SUPPLEMENTS) return false
    this.#active += 1
    return true
  }
  release(): void { this.#active -= 1 }
}

export function rescueDue(elapsed: number, primaryEstimate: number, alternativeEstimate: number): boolean {
  if (elapsed < HEDGE_CHECK_MILLISECONDS) return false
  const remaining = Math.max(0, primaryEstimate - elapsed)
  // Queue residence is expected work, not a stall. Rescue only when moving the
  // frontier can beat that remaining work, or the original estimate is overdue.
  return remaining > HEDGE_DELAY_FACTOR * alternativeEstimate ||
    elapsed >= HEDGE_DELAY_FACTOR * primaryEstimate
}
