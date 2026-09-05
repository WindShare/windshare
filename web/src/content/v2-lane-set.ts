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

interface LaneState {
  readonly lane: V2BlockLane
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
  inflight: number
  failed: boolean
}

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

export interface V2LaneSetOptions {
  readonly dispatchSequence?: V2BlockDispatchSequenceAuthority
  readonly onBlockDispatched?: (observation: V2BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
}

interface PendingLane {
  readonly routes?: V2BlockRouteEligibility
  readonly resolve: (laneId: number) => void
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
  #rotation = 0
  #closed = false

  constructor(options: V2LaneSetOptions = {}) {
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
    this.#lanes.set(lane.id, { lane, laneEpoch, route, inflight: 0, failed: false })
    this.#wakeWaiters()
  }

  remove(laneId: number): void {
    this.#lanes.get(laneId)?.lane.close?.()
    this.#lanes.delete(laneId)
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

  waitForLane(signal?: AbortSignal): Promise<number> {
    return this.#waitForMatchingLane(undefined, signal)
  }

  waitForEligibleLane(
    routes: V2BlockRouteEligibility,
    signal?: AbortSignal,
  ): Promise<number> {
    return this.#waitForMatchingLane(routes, signal)
  }

  async fetch(
    demand: V2BlockDemand,
    routes: V2BlockRouteEligibility,
    signal: AbortSignal,
  ): Promise<V2BlockRecord> {
    signal.throwIfAborted()
    routes.assertActive()
    const failures: unknown[] = []
    const attempted = new Set<LaneState>()
    while (true) {
      routes.assertActive()
      const state = this.#orderedCandidates(routes).find((candidate) => !attempted.has(candidate))
      if (state === undefined) {
        await this.#awaitReplacementOrThrow(failures, attempted, routes, signal)
        continue
      }
      attempted.add(state)
      signal.throwIfAborted()
      state.inflight += 1
      const observation = Object.freeze({
        dispatchSequence: this.#dispatchSequence.next(),
        laneId: state.lane.id,
        laneEpoch: state.laneEpoch,
        route: state.route,
        fileId: demand.descriptor.fileIdText,
        localBlockIndex: demand.localBlockIndex,
      })
      try {
        // The sequence is allocated immediately before invocation so a relay-cut
        // fence classifies dispatch authority rather than completion timing.
        this.#onBlockDispatched(observation)
      } catch {
        // Diagnostics cannot delay, redirect, or cancel authenticated block work.
      }
      try {
        // Eligibility is sampled at dispatch. Once one legitimate consumer starts
        // a shared BlockRef load, later cancellation cannot retroactively make the
        // authenticated bytes illicit for another coalesced consumer.
        const record = await state.lane.fetchBlock(demand, signal)
        state.failed = false
        this.#observeFetched(state, observation, record)
        return record
      } catch (error) {
        if (signal.aborted) throw signal.reason ?? error
        if (!isRetryableLaneFailure(error)) throw error
        failures.push(error)
        state.failed = true
      } finally {
        state.inflight -= 1
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
    for (const state of this.#lanes.values()) state.lane.close?.()
    this.#lanes.clear()
    const reason = new Error('LaneSet is closed')
    for (const waiter of [...this.#waiters]) this.#rejectWaiter(waiter, reason)
  }

  #waitForMatchingLane(
    routes: V2BlockRouteEligibility | undefined,
    signal: AbortSignal | undefined,
  ): Promise<number> {
    if (this.#closed) return Promise.reject(new Error('LaneSet is closed'))
    signal?.throwIfAborted()
    routes?.assertActive()
    const available = this.#orderedCandidates(routes)[0]
    if (available !== undefined) return Promise.resolve(available.lane.id)
    return new Promise<number>((resolve, reject) => {
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

  #orderedCandidates(routes?: V2BlockRouteEligibility): LaneState[] {
    const candidates = [...this.#lanes.values()].filter(
      (candidate) => routes?.allows(candidate.route) ?? true,
    )
    candidates.sort((left, right) => {
      if (left.failed !== right.failed) return left.failed ? 1 : -1
      if (left.inflight !== right.inflight) return left.inflight - right.inflight
      return left.lane.id - right.lane.id
    })
    const first = candidates[0]
    const rotationWidth = first === undefined
      ? 0
      : candidates.findIndex((candidate) =>
        candidate.failed !== first.failed || candidate.inflight !== first.inflight)
    const tiedWidth = rotationWidth < 0 ? candidates.length : rotationWidth
    if (tiedWidth > 1) {
      const offset = this.#rotation % tiedWidth
      this.#rotation += 1
      const tied = candidates.slice(0, tiedWidth)
      return [
        ...tied.slice(offset),
        ...tied.slice(0, offset),
        ...candidates.slice(tiedWidth),
      ]
    }
    return candidates
  }

  async #awaitReplacementOrThrow(
    failures: readonly unknown[],
    attempted: ReadonlySet<LaneState>,
    routes: V2BlockRouteEligibility,
    signal: AbortSignal,
  ): Promise<void> {
    const eligible = this.#orderedCandidates(routes)
    if (eligible.length === 0) {
      await this.waitForEligibleLane(routes, signal)
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
      const candidate = this.#orderedCandidates(waiter.routes)[0]
      if (candidate === undefined) return
      this.#finishWaiter(waiter)
      waiter.resolve(candidate.lane.id)
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
