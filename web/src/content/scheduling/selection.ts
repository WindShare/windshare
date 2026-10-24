import type { V2BlockDemand, V2BlockLane } from '../v2-lane-set'
import type { BlockReceiveWindow } from './receive-window'
import type { V2BlockRouteEligibility, V2BlockTransportRoute } from '../v2-route-policy'
import { LanePerformance } from './performance'
import { LaneSampling } from './sampling'
import { completionCost } from './completion'
import { PROBE_INTERVAL_MILLISECONDS, isExploratoryDispatch, type DispatchPurpose } from './exploration'

export interface ContentLane {
  readonly lane: V2BlockLane
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
  readonly performance: LanePerformance
  readonly sampling: LaneSampling
  readonly receive: BlockReceiveWindow
  readonly unsubscribeReceive: () => void
  inflight: number
  failed: boolean
}
export type ContentPriority = 'preview' | 'download' | 'prefetch'

export interface ContentCapacity {
  readonly lastServed: () => number
  readonly readiness: () => 'ready' | 'network' | 'consumer'
  readonly acquire: () => (() => void) | undefined
}

export interface ContentWork {
  readonly capacity?: ContentCapacity
  /** Newly assembled sealed-object bytes; not authenticated output or resumable coverage. */
  readonly onReceive?: (objectBytes: number) => void
  readonly demand: V2BlockDemand
  readonly routes: V2BlockRouteEligibility
  readonly signal: AbortSignal
  readonly priority: ContentPriority
  /** Zero identifies a consumer's output frontier; positive distance is bounded read-ahead. */
  readonly distance: () => bigint
}
export interface ContentAssignment<T extends ContentWork> {
  readonly work: T
  readonly lane: ContentLane
  readonly purpose: DispatchPurpose
}

export type ContentSelection<T extends ContentWork> =
  | { readonly kind: 'dispatch'; readonly assignment: ContentAssignment<T> }
  | { readonly kind: 'probe-deadline'; readonly at: number }
  | { readonly kind: 'blocked' }

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
    completionCost(left.performance.estimateCompletionMilliseconds(bytes), left.route) -
      completionCost(right.performance.estimateCompletionMilliseconds(bytes), right.route) ||
    left.inflight - right.inflight || left.lane.id - right.lane.id)
}

function probeOpportunity(lane: ContentLane, item: ContentWork, priority: ContentPriority | undefined,
  now: number, globalDue: number): { at: number; purpose: 'probe' | 'sample' } {
  if (item.priority === priority && lane.sampling.available(demandBytes(item.demand), now)) {
    return { at: now, purpose: 'sample' }
  }
  const laneDue = lane.performance.lastAttempt === undefined ? now : lane.performance.lastAttempt + PROBE_INTERVAL_MILLISECONDS
  return { at: Math.max(globalDue, laneDue), purpose: 'probe' }
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

  select<T extends ContentWork>(
    work: readonly T[], lanes: readonly ContentLane[], now: number,
    eligible: (lane: ContentLane, work: T) => boolean,
  ): ContentSelection<T> {
    const primaryFor = (item: T) => orderedContentLanes(lanes, item).find(lane => eligible(lane, item))
    const first = work.find(item => primaryFor(item)?.receive.available)
    const primary = first === undefined ? undefined : primaryFor(first)
    const probe = this.#probe(work, lanes, now, eligible)
    if (probe.kind === 'dispatch') return probe
    return first === undefined || primary === undefined ? probe
      : { kind: 'dispatch', assignment: { work: first, lane: primary, purpose: 'content' } }
  }

  #probe<T extends ContentWork>(
    work: readonly T[], lanes: readonly ContentLane[], now: number,
    eligible: (lane: ContentLane, work: T) => boolean,
  ): ContentSelection<T> {
    if (lanes.length < 2 || [...this.#active.values()].some(isExploratoryDispatch) || !lanes.some(lane => lane.inflight > 0)) {
      return { kind: 'blocked' }
    }
    const globalDue = this.#lastProbe === undefined ? now : this.#lastProbe + PROBE_INTERVAL_MILLISECONDS
    let nextDue = Infinity
    for (const item of exploratoryWork(work, this.#active.keys())) {
      const candidates = orderedContentLanes(lanes, item)
      const available = candidates.filter(lane => eligible(lane, item) && lane.receive.available &&
        lane !== candidates[0] && !lane.failed && lane.performance.pendingBytes === 0)
        .sort((left, right) => (left.performance.lastAttempt ?? -Infinity) - (right.performance.lastAttempt ?? -Infinity))
      for (const lane of available) {
        const opportunity = probeOpportunity(lane, item, work[0]?.priority, now, globalDue)
        if (opportunity.at <= now) return { kind: 'dispatch', assignment: { work: item, lane, purpose: opportunity.purpose } }
        nextDue = Math.min(nextDue, opportunity.at)
      }
    }
    // Only time-blocked, otherwise eligible work needs a deadline. Capacity and
    // route changes have their own events, so idle paths never need polling.
    return Number.isFinite(nextDue) ? { kind: 'probe-deadline', at: nextDue } : { kind: 'blocked' }
  }

  started(work: ContentWork, purpose: DispatchPurpose, now: number): void {
    if (isExploratoryDispatch(purpose)) this.#lastProbe = now
    this.#active.set(work, purpose)
  }

  completed(work: ContentWork): void {
    this.#active.delete(work)
  }
}
