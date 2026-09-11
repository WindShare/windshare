import { ContentAttemptLifetimes } from './scheduling/attempt-lifetimes'
import { LanePerformance } from './scheduling/performance'
import { LaneRequests, type RequestSchedulingObservation } from './scheduling/requests'
import { ContentAllocation, orderedContentLanes, demandBytes, type ContentLane, type ContentWork } from './scheduling/selection'
import { LaneRescues, rescueDue, type DispatchPurpose } from './scheduling/exploration'
import { ContentRaceWon, ContentRaceFailures, raceContent } from './scheduling/race'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import type { V2BlockRecord, V2FileRevisionDescriptor } from './v2-records'
import type {
  V2BlockRouteEligibility,
  V2BlockTransportRoute,
} from './v2-route-policy'

export interface V2BlockDemand {
  readonly descriptor: V2FileRevisionDescriptor
  readonly leaseId: Uint8Array
  readonly localBlockIndex: bigint
}

export interface V2BlockLane {
  readonly id: number
  fetchBlock(demand: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord>
  close?(): void
}

export class V2BlockLaneAttemptsError extends AggregateError {
  constructor(errors: readonly unknown[]) {
    super(errors, 'Every receiver content lane failed')
    this.name = 'V2BlockLaneAttemptsError'
  }
}

type LaneState = ContentLane
type FetchedBlock = { state: LaneState; observation: V2BlockDispatchObservation; record: V2BlockRecord }

export type { ContentWork } from './scheduling/selection'

export interface V2BlockDispatchObservation {
  readonly dispatchSequence: number
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
  readonly fileId: string
  readonly localBlockIndex: bigint
}

// Provenance follows the exact authenticated record through cache/singleflight.
// A WeakMap releases it with the bounded broker cache and never retains content.
const deliveredRoutes = new WeakMap<V2BlockRecord, V2BlockTransportRoute>()
export function authenticatedBlockRoute(record: V2BlockRecord): V2BlockTransportRoute | undefined {
 return deliveredRoutes.get(record)
}

export interface V2BlockRouteObservation extends V2BlockDispatchObservation {
  readonly usefulBytes: number
}

/** Joined-share authority; protocol generations borrow it instead of resetting evidence order. */
export class V2BlockDispatchSequenceAuthority {
  #current = 0

  next(): number {
    if (this.#current === Number.MAX_SAFE_INTEGER) {
      throw new RangeError('Block dispatch evidence sequence is exhausted')
    }
    this.#current += 1
    return this.#current
  }
}

export interface V2BlockSchedulingObservation extends V2BlockDispatchObservation {
  readonly purpose: DispatchPurpose
  readonly expectedMilliseconds: number
  readonly pendingBytes: number
  readonly bytesPerSecond: number
}

export interface V2LaneSetOptions {
  readonly now?: () => number
  readonly onBlockScheduled?: (observation: V2BlockSchedulingObservation) => void
  readonly onRequestScheduled?: (observation: RequestSchedulingObservation) => void
  readonly dispatchSequence?: V2BlockDispatchSequenceAuthority
  readonly onBlockDispatched?: (observation: V2BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
}

interface PendingLane {
  readonly routes?: V2BlockRouteEligibility
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
  readonly signal?: AbortSignal
  abort?: () => void
  unsubscribeRoutes?: () => void
}

export class V2LaneSet {
  readonly #lanes = new Map<number, LaneState>()
  readonly #waiters = new Set<PendingLane>()
  readonly #dispatchSequence: V2BlockDispatchSequenceAuthority
  readonly #onBlockDispatched: (observation: V2BlockDispatchObservation) => void
  readonly #onBlockFetched: (observation: V2BlockRouteObservation) => void
  readonly #now: () => number
  readonly #onBlockScheduled: ((observation: V2BlockSchedulingObservation) => void) | undefined
  readonly requests: LaneRequests
  readonly #allocation = new ContentAllocation()
  readonly #rescues = new LaneRescues()
  readonly #attemptLifetimes = new ContentAttemptLifetimes()
  readonly #lifetime = new AbortController()
  #closed = false

  constructor(options: V2LaneSetOptions = {}) {
    this.#now = options.now ?? (() => performance.now())
    this.requests = new LaneRequests({ now: this.#now, ...(options.onRequestScheduled === undefined ? {} : { observe: options.onRequestScheduled }) })
    this.#onBlockScheduled = options.onBlockScheduled
    this.#dispatchSequence = options.dispatchSequence ?? new V2BlockDispatchSequenceAuthority()
    this.#onBlockDispatched = options.onBlockDispatched ?? (() => undefined)
    this.#onBlockFetched = options.onBlockFetched ?? (() => undefined)
  }

  add(lane: V2BlockLane, route: V2BlockTransportRoute, laneEpoch = 0): void {
    if (this.#closed) throw new Error('LaneSet is closed')
    if (!Number.isInteger(lane.id) || lane.id <= 0 || this.#lanes.has(lane.id)) {
      throw new TypeError('LaneSet requires a unique positive lane identity')
    }
    if (!Number.isInteger(laneEpoch) || laneEpoch < 0 || laneEpoch > 0xffff_ffff) {
      throw new TypeError('LaneSet requires an unsigned lane epoch')
    }
    const performance = new LanePerformance()
    this.#lanes.set(lane.id, { lane, laneEpoch, route, inflight: 0, failed: false, performance })
    this.requests.add({ id: lane.id, epoch: laneEpoch, route, content: performance })
    this.#wakeWaiters()
  }

  remove(laneId: number): void {
    this.#lanes.get(laneId)?.lane.close?.()
    this.#lanes.delete(laneId)
    this.requests.remove(laneId)
    this.#wakeWaiters()
  }

  get size(): number {
    return this.#lanes.size
  }

  laneIds(): readonly number[] {
    return Object.freeze([...this.#lanes.keys()])
  }

  eligibleSize(routes: V2BlockRouteEligibility): number {
    routes.assertActive()
    return [...this.#lanes.values()].filter((candidate) => routes.allows(candidate.route)).length
  }

  waitForContentAdmission(signal?: AbortSignal): Promise<void> {
    return this.#waitForContentAvailability(undefined, signal)
  }

  async fetch(demand: V2BlockDemand, routes: V2BlockRouteEligibility, signal: AbortSignal): Promise<V2BlockRecord> {
    return this.dispatch([{ demand, routes, signal, distance: () => 0n }]).result
  }

  dispatch<T extends ContentWork>(queued: readonly T[]): { readonly work: T; readonly result: Promise<V2BlockRecord> } {
    try {
      const assignment = this.#allocation.select(queued, [...this.#lanes.values()], this.#now())
      const result = this.#fetch(assignment.work, assignment.lane, assignment.purpose)
        .finally(() => this.#allocation.completed(assignment.purpose))
      return { work: assignment.work, result }
    } catch (error) {
      const work = queued[0]
      if (work === undefined) throw error
      return { work, result: Promise.reject(error) }
    }
  }

  async #fetch(work: ContentWork, assigned: LaneState | undefined, purpose: DispatchPurpose): Promise<V2BlockRecord> {
    const { demand, routes } = work
    const signal = AbortSignal.any([work.signal, this.#lifetime.signal])
    signal.throwIfAborted()
    routes.assertActive()
    const failures: unknown[] = []
    const attempted = new Set<LaneState>()
    const bytes = demandBytes(demand)
    let supplemented = false
    while (true) {
      signal.throwIfAborted()
      routes.assertActive()
      const state = this.#selectAttemptLane(work, assigned, attempted)
      assigned = undefined
      if (state === undefined) {
        await this.#awaitReplacementOrThrow(failures, attempted, routes, signal)
        continue
      }
      attempted.add(state)
      const started = this.#now()
      const estimate = state.performance.estimate(bytes)
      try {
        const winner = await raceContent(
          signal,
          attemptSignal => this.#fetchAttempt(state, demand, attemptSignal, purpose),
          attemptSignal => {
            if (supplemented) return undefined
            const rescue = this.#rescue(work, attempted, started, estimate, attemptSignal)
            supplemented = rescue !== undefined
            return rescue
          },
          isRetryableLaneFailure,
        )
        this.#observeFetched(winner.state, winner.observation, winner.record)
        return winner.record
      } catch (error) {
        if (signal.aborted) throw signal.reason ?? error
        if (!(error instanceof ContentRaceFailures)) throw error
        const fatalIndex = error.errors.findIndex((failure: unknown) => !isRetryableLaneFailure(failure))
        if (fatalIndex >= 0) throw error.errors[fatalIndex]
        failures.push(...error.errors)
      }
    }
  }

  #selectAttemptLane(work: ContentWork, assigned: LaneState | undefined, attempted: ReadonlySet<LaneState>): LaneState | undefined {
    if (assigned !== undefined && this.#lanes.get(assigned.lane.id) === assigned && work.routes.allows(assigned.route)) return assigned
    return orderedContentLanes([...this.#lanes.values()], work).find(candidate => !attempted.has(candidate))
  }

  #rescue(
    work: ContentWork, attempted: Set<LaneState>, started: number, estimate: number, signal: AbortSignal,
  ): Promise<FetchedBlock> | undefined {
    if (work.distance() > 0n) return undefined
    work.routes.assertActive()
    const candidate = orderedContentLanes([...this.#lanes.values()], work).find(lane => !attempted.has(lane))
    if (candidate === undefined ||
      !rescueDue(this.#now() - started, estimate, candidate.performance.estimate(demandBytes(work.demand))) ||
      !this.#rescues.acquire()) return undefined
    attempted.add(candidate)
    return this.#fetchAttempt(candidate, work.demand, signal, 'rescue').finally(() => this.#rescues.release())
  }

  waitForLeaseIdle(leaseId: Uint8Array): Promise<void> {
    return this.#attemptLifetimes.waitForLeaseIdle(leaseId)
  }

  async #fetchAttempt(
    state: LaneState,
    demand: V2BlockDemand,
    signal: AbortSignal,
    purpose: DispatchPurpose,
  ): Promise<FetchedBlock> {
    signal.throwIfAborted()
    const bytes = demandBytes(demand)
    const started = this.#now()
    const expectedMilliseconds = state.performance.estimate(bytes)
    const observation = Object.freeze({
      dispatchSequence: this.#dispatchSequence.next(),
      laneId: state.lane.id,
      laneEpoch: state.laneEpoch,
      route: state.route,
      fileId: demand.descriptor.fileIdText,
      localBlockIndex: demand.localBlockIndex,
    })
    const releaseAttempt = this.#attemptLifetimes.begin(demand.leaseId)
    state.inflight += 1
    state.performance.begin(started, bytes)
    try {
      this.#onBlockDispatched(observation)
    } catch { /* Diagnostics cannot redirect authenticated work. */ }
    try {
      this.#onBlockScheduled?.(Object.freeze({
        ...observation, purpose, expectedMilliseconds,
        pendingBytes: state.performance.pendingBytes,
        bytesPerSecond: state.performance.bytesPerSecond,
      }))
    } catch { /* Scheduling observers cannot become transfer authority. */ }
    let successful = false
    try {
      const record = await state.lane.fetchBlock(demand, signal)
      successful = true
      state.failed = false
      return { state, observation, record }
    } catch (error) {
      if (!signal.aborted && isRetryableLaneFailure(error)) state.failed = true
      throw error
    } finally {
      releaseAttempt()
      state.inflight -= 1
      state.performance.complete(this.#now(), bytes, successful)
      if (!successful && signal.reason instanceof ContentRaceWon) {
        state.performance.superseded(bytes, this.#now() - started)
      }
    }
  }

  #observeFetched(state: LaneState, observation: V2BlockDispatchObservation, record: V2BlockRecord): void {
    // A retired route may finish valid content, but cannot attest which replacement path carried it.
    if (this.#lanes.get(state.lane.id) !== state) return
    deliveredRoutes.set(record, state.route)
    try {
      this.#onBlockFetched(Object.freeze({ ...observation, usefulBytes: record.data.byteLength }))
    } catch {
      // Diagnostics cannot become transfer authority or corrupt an authenticated success.
    }
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.requests.close()
    this.#lifetime.abort(new Error('LaneSet is closed'))
    for (const state of this.#lanes.values()) state.lane.close?.()
    this.#lanes.clear()
    const reason = new Error('LaneSet is closed')
    for (const waiter of [...this.#waiters]) this.#rejectWaiter(waiter, reason)
  }

  #waitForContentAvailability(
    routes: V2BlockRouteEligibility | undefined,
    signal: AbortSignal | undefined,
  ): Promise<void> {
    if (this.#closed) return Promise.reject(new Error('LaneSet is closed'))
    signal?.throwIfAborted()
    routes?.assertActive()
    const available = this.#availableLanes(routes)[0]
    if (available !== undefined) return Promise.resolve()
    return new Promise<void>((resolve, reject) => {
      const waiter: PendingLane = {
        ...(routes === undefined ? {} : { routes }),
        resolve,
        reject,
        ...(signal === undefined ? {} : { signal }),
      }
      waiter.abort = () => this.#rejectWaiter(
        waiter,
        signal?.reason ?? new DOMException('Content lane wait aborted', 'AbortError'),
      )
      if (routes !== undefined) {
        waiter.unsubscribeRoutes = routes.subscribe(() => this.#settleWaiter(waiter))
      }
      this.#waiters.add(waiter)
      signal?.addEventListener('abort', waiter.abort, { once: true })
      this.#settleWaiter(waiter)
    })
  }

  #availableLanes(routes?: V2BlockRouteEligibility): LaneState[] {
    return [...this.#lanes.values()].filter(candidate => routes?.allows(candidate.route) ?? true)
  }

  async #awaitReplacementOrThrow(
    failures: readonly unknown[],
    attempted: ReadonlySet<LaneState>,
    routes: V2BlockRouteEligibility,
    signal: AbortSignal,
  ): Promise<void> {
    const eligible = this.#availableLanes(routes)
    if (eligible.length === 0) {
      await this.#waitForContentAvailability(routes, signal)
      return
    }
    if (eligible.some((candidate) => !attempted.has(candidate))) return
    if (failures.length === 0) throw new Error('No eligible content lane is available')
    throw new V2BlockLaneAttemptsError(failures)
  }

  #wakeWaiters(): void {
    for (const waiter of [...this.#waiters]) this.#settleWaiter(waiter)
  }

  #settleWaiter(waiter: PendingLane): void {
    if (!this.#waiters.has(waiter)) return
    try {
      waiter.routes?.assertActive()
      const candidate = this.#availableLanes(waiter.routes)[0]
      if (candidate === undefined) return
      this.#finishWaiter(waiter)
      waiter.resolve()
    } catch (error) {
      this.#rejectWaiter(waiter, error)
    }
  }

  #rejectWaiter(waiter: PendingLane, reason: unknown): void {
    if (!this.#waiters.has(waiter)) return
    this.#finishWaiter(waiter)
    waiter.reject(reason)
  }

  #finishWaiter(waiter: PendingLane): void {
    this.#waiters.delete(waiter)
    if (waiter.signal !== undefined && waiter.abort !== undefined) {
      waiter.signal.removeEventListener('abort', waiter.abort)
    }
    waiter.unsubscribeRoutes?.()
  }
}

function isRetryableLaneFailure(error: unknown): boolean {
  return (error instanceof V2SessionRuntimeError && error.scope === 'lane') ||
    (error instanceof DOMException && error.name === 'AbortError')
}
