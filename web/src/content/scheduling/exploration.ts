import { conservativeDeliveryMilliseconds, type DeliveryEstimate, type DeliveryProgress } from './completion'

export const PROBE_INTERVAL_MILLISECONDS = 5000
export const MINIMUM_RESCUE_MILLISECONDS = 10
export const RESCUE_POLL_MILLISECONDS = 10
const HEDGE_DELAY_FACTOR = 2
export const MAXIMUM_SUPPLEMENTS = 2
export type DispatchPurpose = 'content' | 'probe' | 'sample' | 'rescue'

export function isExploratoryDispatch(purpose: DispatchPurpose): boolean {
  return purpose === 'probe' || purpose === 'sample'
}

/** Rescue authority is independent of the budget for sampling unique queued work. */
export class LaneRescues {
  #active = 0
  get available(): boolean { return this.#active < MAXIMUM_SUPPLEMENTS }
  acquire(): boolean {
    if (this.#active >= MAXIMUM_SUPPLEMENTS) return false
    this.#active += 1
    return true
  }
  release(): void { this.#active -= 1 }
}

export type RescueReason = 'faster-delivery' | 'no-progress' | 'bootstrap-overdue'

export function rescueDecision(
  progress: DeliveryProgress, initial: DeliveryEstimate, alternative: DeliveryEstimate,
): RescueReason | undefined {
  if (progress.remainingBytes === 0 || progress.elapsedMilliseconds < MINIMUM_RESCUE_MILLISECONDS) return undefined
  const alternativeMilliseconds = conservativeDeliveryMilliseconds(alternative)
  // Give an active request at least one alternative delivery interval before
  // spending duplicate work on a transient startup or scheduling burst.
  if (progress.elapsedMilliseconds < alternativeMilliseconds) return undefined
  if (alternative.evidence !== 'unknown') {
    if (progress.remaining.milliseconds > HEDGE_DELAY_FACTOR * alternativeMilliseconds) return 'faster-delivery'
    // An overdue forecast is not proof of zero remaining work. Stop waiting
    // when the request is silent long enough for a measured alternative to help.
    if (progress.silentMilliseconds >= Math.max(initial.milliseconds, HEDGE_DELAY_FACTOR * alternativeMilliseconds)) {
      return 'no-progress'
    }
    return undefined
  }
  // Unknown alternatives retain a bounded liveness escape, but cannot consume a
  // rescue merely by presenting an optimistic bootstrap score.
  return progress.silentMilliseconds >= HEDGE_DELAY_FACTOR * Math.max(initial.milliseconds, alternative.milliseconds)
    ? 'bootstrap-overdue' : undefined
}
