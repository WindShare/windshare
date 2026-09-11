import type { V2BlockDemand, V2BlockLane } from '../v2-lane-set'
import type { V2BlockRouteEligibility, V2BlockTransportRoute } from '../v2-route-policy'
import { completionCost, LanePerformance } from './performance'
import { PROBE_INTERVAL_MILLISECONDS, type DispatchPurpose } from './exploration'

export interface ContentLane {
  readonly lane: V2BlockLane
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
  readonly performance: LanePerformance
  inflight: number
  failed: boolean
}
export interface ContentWork {
  readonly demand: V2BlockDemand
  readonly routes: V2BlockRouteEligibility
  readonly signal: AbortSignal
  /** Zero identifies a consumer's output frontier; positive distance is bounded read-ahead. */
  readonly distance: () => bigint
}
export interface ContentAssignment<T extends ContentWork> {
  readonly work: T
  readonly lane: ContentLane | undefined
  readonly purpose: DispatchPurpose
}

export function demandBytes(demand: V2BlockDemand): number {
  const range = demand.descriptor.geometry.blockPlaintext(demand.localBlockIndex)
  return Number(range.end - range.start)
}

export function orderedContentLanes(
  lanes: readonly ContentLane[], work: ContentWork,
): ContentLane[] {
  const bytes = demandBytes(work.demand)
  return lanes.filter(candidate => work.routes.allows(candidate.route)).sort((left, right) =>
    Number(left.failed) - Number(right.failed) ||
    completionCost(left.performance.estimate(bytes), left.route !== 'direct') -
      completionCost(right.performance.estimate(bytes), right.route !== 'direct') ||
    left.inflight - right.inflight || left.lane.id - right.lane.id)
}

/** One receiver allocation owner assigns unique queued blocks before reserving lane bytes. */
export class ContentAllocation {
  #lastProbe: number | undefined
  #probeActive = false

  select<T extends ContentWork>(work: readonly T[], lanes: readonly ContentLane[], now: number): ContentAssignment<T> {
    const first = work[0]
    if (first === undefined) throw new Error('Content allocation requires queued work')
    const primary = orderedContentLanes(lanes, first)[0]
    if (!this.#probeActive && lanes.some(lane => lane.inflight > 0) &&
      (this.#lastProbe === undefined || now - this.#lastProbe >= PROBE_INTERVAL_MILLISECONDS)) {
      // Explore independent work farthest from an output frontier. Completing an
      // unrelated fast block must not cancel the only measurement of this lane.
      const ahead = [...work].filter(item => item.distance() > 0n)
        .sort((left, right) => Number(right.distance() - left.distance()))
      for (const item of ahead) {
        const candidates = orderedContentLanes(lanes, item)
        const candidate = candidates.filter(lane => lane !== primary && !lane.failed &&
          lane.performance.pendingBytes === 0 &&
          (lane.performance.lastAttempt === undefined || now - lane.performance.lastAttempt >= PROBE_INTERVAL_MILLISECONDS))
          .sort((left, right) => (left.performance.lastAttempt ?? -Infinity) - (right.performance.lastAttempt ?? -Infinity))[0]
        if (candidate === undefined) continue
        this.#lastProbe = now
        this.#probeActive = true
        return { work: item, lane: candidate, purpose: 'probe' }
      }
    }
    return { work: first, lane: primary, purpose: 'content' }
  }

  completed(purpose: DispatchPurpose): void {
    if (purpose === 'probe') this.#probeActive = false
  }
}
