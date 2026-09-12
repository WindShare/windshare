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
export type ContentPriority = 'preview' | 'download' | 'prefetch'

export interface ContentWork {
  readonly demand: V2BlockDemand
  readonly routes: V2BlockRouteEligibility
  readonly signal: AbortSignal
  readonly priority: ContentPriority
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
    completionCost(left.performance.estimateCompletionMilliseconds(bytes), left.route !== 'direct') -
      completionCost(right.performance.estimateCompletionMilliseconds(bytes), right.route !== 'direct') ||
    left.inflight - right.inflight || left.lane.id - right.lane.id)
}

const PRIMARY_FRONTIER_RESERVE = 2

function exploratoryWork<T extends ContentWork>(work: readonly T[], active: Iterable<ContentWork>): T[] {
  // One ordered reader has only one frontier. Reserve the current and next
  // independent frontiers for normal allocation; later readers can sample a
  // path even when a sustained file batch replenishes its queue one at a time.
  const frontiers = [...active, ...work].filter(item =>
    item.priority === work[0]?.priority && item.distance() === 0n)
  const reserved = new Set(frontiers.slice(0, PRIMARY_FRONTIER_RESERVE))
  return work.map((item, index) => ({ item, index, distance: item.distance() }))
    .filter(({ item, distance }) => distance > 0n || (item.priority !== 'preview' && !reserved.has(item)))
    .sort((left, right) => {
      if (left.distance === right.distance) return right.index - left.index
      return left.distance > right.distance ? -1 : 1
    })
    .map(({ item }) => item)
}

/** One receiver allocation owner assigns unique queued blocks before reserving lane bytes. */
export class ContentAllocation {
  #lastProbe: number | undefined
  readonly #active = new Map<ContentWork, DispatchPurpose>()

  select<T extends ContentWork>(work: readonly T[], lanes: readonly ContentLane[], now: number): ContentAssignment<T> {
    const first = work[0]
    if (first === undefined) throw new Error('Content allocation requires queued work')
    const primary = orderedContentLanes(lanes, first)[0]
    if (![...this.#active.values()].includes('probe') && lanes.some(lane => lane.inflight > 0) &&
      (this.#lastProbe === undefined || now - this.#lastProbe >= PROBE_INTERVAL_MILLISECONDS)) {
      for (const item of exploratoryWork(work, this.#active.keys())) {
        const candidates = orderedContentLanes(lanes, item)
        const candidate = candidates.filter(lane => lane !== candidates[0] && !lane.failed &&
          lane.performance.pendingBytes === 0 &&
          (lane.performance.lastAttempt === undefined || now - lane.performance.lastAttempt >= PROBE_INTERVAL_MILLISECONDS))
          .sort((left, right) => (left.performance.lastAttempt ?? -Infinity) - (right.performance.lastAttempt ?? -Infinity))[0]
        if (candidate === undefined) continue
        this.#lastProbe = now
        this.#active.set(item, 'probe')
        return { work: item, lane: candidate, purpose: 'probe' }
      }
    }
    this.#active.set(first, 'content')
    return { work: first, lane: primary, purpose: 'content' }
  }

  completed(work: ContentWork): void {
    this.#active.delete(work)
  }
}
