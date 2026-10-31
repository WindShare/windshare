import { ContentAttemptLifetimes } from './scheduling/attempt-lifetimes'
import { BlockAttempts, type BlockRetryDecision } from './scheduling/attempts'
import { LanePerformance } from './scheduling/performance'
import { LaneSampling, type SamplingEvidence, type SamplingOutcome } from './scheduling/sampling'
import { BlockReceiveWindow, type BlockReceiveAdmission } from './scheduling/receive-window'
import { ContentAdmissions, type ContentAdmission } from './scheduling/admission'
import { LaneRequests, type RequestSchedulingObservation } from './scheduling/requests'
import { orderedContentLanes, demandBytes, type ContentLane, type ContentWork } from './scheduling/selection'
import { LaneRescues, rescueDecision, type RescueReason, type DispatchPurpose } from './scheduling/exploration'
import { ContentRaceWon, ContentRaceFailures, raceContent } from './scheduling/race'
import { SessionRuntimeError } from '../session/runtime-types'
import type { BlockRecord, FileRevisionDescriptor } from './records'
import type {
  BlockRouteEligibility,
  BlockTransportRoute,
} from './route-policy'

export interface BlockDemand {
  readonly descriptor: FileRevisionDescriptor
  readonly leaseId: Uint8Array
  readonly localBlockIndex: bigint
}

export interface BlockReadObserver {
  received(objectBytes: number): void
  admitted(admission: BlockReceiveAdmission): void
}

export interface BlockLane {
  readonly id: number
  fetchBlock(demand: BlockDemand, signal: AbortSignal, observer?: BlockReadObserver): Promise<BlockRecord>
  close?(): void
}

export class BlockLaneAttemptsError extends AggregateError {
  constructor(errors: readonly unknown[]) {
    super(errors, 'Receiver block attempts exhausted')
    this.name = 'BlockLaneAttemptsError'
  }
}

type LaneState = ContentLane
type FetchedBlock = { state: LaneState; observation: BlockDispatchObservation; record: BlockRecord }

export type { ContentWork } from './scheduling/selection'

export interface BlockDispatchObservation {
  readonly dispatchSequence: number
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: BlockTransportRoute
  readonly fileId: string
  readonly localBlockIndex: bigint
}

// Provenance follows the exact authenticated record through cache/singleflight.
// A WeakMap releases it with the bounded broker cache and never retains content.
const deliveredRoutes = new WeakMap<BlockRecord, BlockTransportRoute>()
export function authenticatedBlockRoute(record: BlockRecord): BlockTransportRoute | undefined {
 return deliveredRoutes.get(record)
}

export function inheritAuthenticatedBlockRoute(source: BlockRecord, target: BlockRecord): void {
  const route = deliveredRoutes.get(source)
  if (route !== undefined) deliveredRoutes.set(target, route)
}

export interface BlockRouteObservation extends BlockDispatchObservation {
  readonly usefulBytes: number
}

/** Joined-share authority; protocol generations borrow it instead of resetting evidence order. */
export class BlockDispatchSequenceAuthority {
  #current = 0

  next(): number {
    if (this.#current === Number.MAX_SAFE_INTEGER) {
      throw new RangeError('Block dispatch evidence sequence is exhausted')
    }
    this.#current += 1
    return this.#current
  }
}

export interface BlockSchedulingObservation extends BlockDispatchObservation {
  readonly admission?: BlockReceiveAdmission
  readonly purpose: DispatchPurpose
  readonly expectedMilliseconds: number
  readonly deliveryEvidence?: 'unknown' | 'recent' | 'stale'
  readonly rescueReason?: RescueReason
  readonly pendingBytes: number
  readonly bytesPerSecond: number
}

export type BlockRetryObservation = BlockRetryDecision & Readonly<{ fileId: string; localBlockIndex: bigint }>

export interface LaneSetOptions {
  readonly now?: () => number
  readonly onBlockScheduled?: (observation: BlockSchedulingObservation) => void
  readonly onBlockRetry?: (observation: BlockRetryObservation) => void
  readonly onRequestScheduled?: (observation: RequestSchedulingObservation) => void
  readonly dispatchSequence?: BlockDispatchSequenceAuthority
  readonly onBlockDispatched?: (observation: BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: BlockRouteObservation) => void
}

interface PendingLane {
  readonly routes?: BlockRouteEligibility
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
  readonly signal?: AbortSignal
  abort?: () => void
  unsubscribeRoutes?: () => void
}

export type RemainingWorkObservation = () => { bytes: number; known: boolean }

export class LaneSet {
  readonly #remaining = new Map<RemainingWorkObservation, number>()
  #authenticatedBytes = 0
  #outputBytes = 0
  readonly #lanes = new Map<number, LaneState>()
  readonly #useful = new Map<number, { bytes: number; admittedAt: number; lastAt: number | undefined }>()
  readonly #waiters = new Set<PendingLane>()
  readonly #dispatchSequence: BlockDispatchSequenceAuthority
  readonly #onBlockDispatched: (observation: BlockDispatchObservation) => void
  readonly #onBlockFetched: (observation: BlockRouteObservation) => void
  readonly #now: () => number
  readonly #onBlockScheduled: ((observation: BlockSchedulingObservation) => void) | undefined
  readonly #onBlockRetry: ((observation: BlockRetryObservation) => void) | undefined
  readonly requests: LaneRequests
  readonly #admissions: ContentAdmissions
  readonly #rescues = new LaneRescues()
  readonly #attemptLifetimes = new ContentAttemptLifetimes()
  readonly #lifetime = new AbortController()
  #closed = false

  constructor(options: LaneSetOptions = {}) {
    this.#now = options.now ?? (() => performance.now())
    this.#admissions = new ContentAdmissions(() => [...this.#lanes.values()], this.#now)
    this.requests = new LaneRequests({ now: this.#now, ...(options.onRequestScheduled === undefined ? {} : { observe: options.onRequestScheduled }) })
    this.#onBlockScheduled = options.onBlockScheduled
    this.#onBlockRetry = options.onBlockRetry
    this.#dispatchSequence = options.dispatchSequence ?? new BlockDispatchSequenceAuthority()
    this.#onBlockDispatched = options.onBlockDispatched ?? (() => undefined)
    this.#onBlockFetched = options.onBlockFetched ?? (() => undefined)
  }

  add(lane: BlockLane, route: BlockTransportRoute, laneEpoch = 0): void {
    if (this.#closed) throw new Error('LaneSet is closed')
    if (!Number.isInteger(lane.id) || lane.id <= 0 || this.#lanes.has(lane.id)) {
      throw new TypeError('LaneSet requires a unique positive lane identity')
    }
    if (!Number.isInteger(laneEpoch) || laneEpoch < 0 || laneEpoch > 0xffff_ffff) {
      throw new TypeError('LaneSet requires an unsigned lane epoch')
    }
    const performance = new LanePerformance()
    const receive = new BlockReceiveWindow(this.#now)
    const unsubscribeReceive = receive.subscribeAvailable(() => this.wakeContent())
    this.#lanes.set(lane.id, { lane, laneEpoch, route, inflight: 0, failed: false, performance, sampling: new LaneSampling(), receive, unsubscribeReceive })
    this.#useful.set(lane.id, { bytes: 0, admittedAt: this.#now(), lastAt: undefined })
    this.requests.add({ id: lane.id, epoch: laneEpoch, route, content: receive })
    this.#wakeWaiters()
  }

  remove(laneId: number): void {
    this.#lanes.get(laneId)?.unsubscribeReceive()
    this.#lanes.get(laneId)?.lane.close?.()
    this.#lanes.delete(laneId)
    this.#useful.delete(laneId)
    this.requests.remove(laneId)
    this.#wakeWaiters()
  }

  get size(): number {
    return this.#lanes.size
  }

  recordOutput(bytes: number): void { this.#outputBytes += bytes }

  observeRemainingWork(snapshot: RemainingWorkObservation): () => void {
    this.#remaining.set(snapshot, (this.#remaining.get(snapshot) ?? 0) + 1)
    let closed = false
    return () => {
      if (closed) return
      closed = true
      const count = this.#remaining.get(snapshot)! - 1
      if (count === 0) this.#remaining.delete(snapshot)
      else this.#remaining.set(snapshot, count)
    }
  }

  schedulingDemand(): {
    queuedBytes: number; queuedBlocks: number; networkWaitingBytes: number; capacityWaitingBytes: number;
    inFlightBytes: number; unfinishedBytes: number; usefulBytes: number; outputBytes: number;
    remainingBytes: number; remainingKnown: boolean;
  } {
    const demand = this.#admissions.demand()
    let inFlightBytes = 0, unfinishedBytes = 0
    for (const lane of this.#lanes.values()) {
      inFlightBytes += lane.performance.pendingBytes
      unfinishedBytes += lane.receive.unfinishedBytes
    }
    let remainingBytes = 0, remainingKnown = this.#remaining.size > 0
    for (const snapshot of this.#remaining.keys()) {
      const work = snapshot()
      remainingBytes += work.bytes
      remainingKnown &&= work.known && Number.isSafeInteger(remainingBytes)
    }
    return { ...demand, inFlightBytes, unfinishedBytes, usefulBytes: this.#authenticatedBytes, outputBytes: this.#outputBytes,
      remainingBytes: this.#remaining.size > 0 ? remainingBytes : demand.queuedBytes + inFlightBytes, remainingKnown }
  }

  laneIds(): readonly number[] {
    return Object.freeze([...this.#lanes.keys()])
  }

  measureLane(laneId: number, laneEpoch: number): { usefulBytes: number; unhelpful: boolean; busyMilliseconds: number; pendingBytes: number } {
    const state = this.#lanes.get(laneId)
    const useful = this.#useful.get(laneId)
    if (state === undefined || state.laneEpoch !== laneEpoch || useful === undefined) return { usefulBytes: 0, unhelpful: false, busyMilliseconds: 0, pendingBytes: 0 }
    const quietWindowMs = 30_000
    const minimumRateRatio = 1.5
    const sampledIdle = state.performance.hasSuccessfulSample && state.inflight === 0 &&
      this.#now() - (useful.lastAt ?? useful.admittedAt) >= quietWindowMs
    const unhelpful = sampledIdle && [...this.#lanes.values()].some((other) => {
      const recent = this.#useful.get(other.lane.id)?.lastAt
      return other !== state && !other.failed && recent !== undefined && this.#now() - recent < quietWindowMs &&
        other.performance.hasSuccessfulSample && other.performance.bytesPerSecond > state.performance.bytesPerSecond * minimumRateRatio
    })
    return { usefulBytes: useful.bytes, unhelpful,
      busyMilliseconds: state.performance.busyMilliseconds(this.#now()), pendingBytes: state.performance.pendingBytes }
  }

  sampleLane(laneId: number, laneEpoch: number): void {
    const state = this.#lanes.get(laneId)
    if (state === undefined || state.laneEpoch !== laneEpoch) return
    for (const lane of this.#lanes.values()) lane.sampling.stop()
    state.sampling.start(this.#now())
    this.wakeContent()
  }

  stopSampling(laneId: number, laneEpoch: number): void {
    const state = this.#lanes.get(laneId)
    if (state?.laneEpoch === laneEpoch) state.sampling.stop()
  }

  samplingEvidence(laneId: number, laneEpoch: number): SamplingEvidence | undefined {
    const state = this.#lanes.get(laneId)
    return state?.laneEpoch === laneEpoch ? state.sampling.snapshot(this.#now()) : undefined
  }

  eligibleSize(routes: BlockRouteEligibility): number {
    routes.assertActive()
    return [...this.#lanes.values()].filter((candidate) => routes.allows(candidate.route)).length
  }

  waitForContentAdmission(signal?: AbortSignal): Promise<void> {
    return this.#waitForContentAvailability(undefined, signal)
  }

  async fetch(demand: BlockDemand, routes: BlockRouteEligibility, signal: AbortSignal): Promise<BlockRecord> {
    return this.fetchWork({ demand, routes, signal, priority: 'download', distance: () => 0n })
  }

  wakeContent(): void {
    this.#admissions.wake()
  }

  async fetchWork(work: ContentWork): Promise<BlockRecord> {
    const { demand, routes } = work
    const signal = AbortSignal.any([work.signal, this.#lifetime.signal])
    signal.throwIfAborted()
    routes.assertActive()
    const failures: unknown[] = []
    const attempts = new BlockAttempts(this.#onBlockRetry === undefined ? undefined : decision =>
      this.#onBlockRetry!({ ...decision, fileId: demand.descriptor.fileIdText, localBlockIndex: demand.localBlockIndex }))
    let supplemented = false
    while (true) {
      signal.throwIfAborted()
      routes.assertActive()
      const admission = await this.#admit(work, signal, attempts, failures)
      try {
        const winner = await raceContent(
          signal,
          attemptSignal => this.#fetchAttempt(admission, demand, attemptSignal, attempts, work.onReceive),
          attemptSignal => {
            if (supplemented) return undefined
            const rescue = this.#rescue(work, attempts, admission, attemptSignal)
            supplemented = rescue !== undefined
            return rescue
          },
          isRetryableLaneFailure,
        )
        this.#observeFetched(winner.state, winner.observation, winner.record)
        return winner.record
      } catch (error) {
        if (signal.aborted) throw signal.reason ?? error
        failures.push(...retryableLaneFailures(error))
      }
    }
  }

  async #admit(
    work: ContentWork, signal: AbortSignal, attempts: BlockAttempts, failures: readonly unknown[],
  ): Promise<ContentAdmission> {
    while (!this.#availableLanes(work.routes).some(lane => attempts.allows(lane))) {
      await this.#awaitReplacementOrThrow(failures, attempts, work.routes, signal)
    }
    const admission = await this.#admissions.request(work, signal, lane => attempts.allows(lane))
    if (signal.aborted) { admission.close('cancelled'); throw signal.reason }
    return admission
  }

  #rescue(
    work: ContentWork, attempts: BlockAttempts, primary: ContentAdmission, signal: AbortSignal,
  ): Promise<() => Promise<FetchedBlock>> | undefined {
    if (work.distance() > 0n) return undefined
    work.routes.assertActive()
    const candidate = orderedContentLanes([...this.#lanes.values()], work).find(lane => attempts.allows(lane))
    const decide = (lane: ContentLane) => work.distance() > 0n ? undefined
      : rescueDecision(primary.permit.progress(), primary.expected, lane.receive.estimateCompletion(demandBytes(work.demand)))
    if (candidate === undefined || decide(candidate) === undefined) return undefined
    const base = work.capacity
    const rescueWork: ContentWork = { ...work, capacity: {
      lastServed: () => base?.lastServed() ?? 0,
      readiness: () => this.#rescues.available ? base?.readiness() ?? 'ready' : 'network',
      acquire: () => {
        if (!this.#rescues.acquire()) return undefined
        let release: (() => void) | undefined
        try { release = base?.acquire() } catch (error) { this.#rescues.release(); throw error }
        if (base !== undefined && release === undefined) { this.#rescues.release(); return undefined }
        return () => { release?.(); this.#rescues.release() }
      },
    } }
    return this.#admissions.requestRescue(rescueWork, signal, lane => attempts.allows(lane), decide)
      .then(admission => () => this.#fetchAttempt(admission, work.demand, signal, attempts, work.onReceive))
  }

  waitForLeaseIdle(leaseId: Uint8Array): Promise<void> {
    return this.#attemptLifetimes.waitForLeaseIdle(leaseId)
  }

  async #fetchAttempt(
    admission: ContentAdmission,
    demand: BlockDemand,
    signal: AbortSignal,
    attempts: BlockAttempts,
    onReceive?: (objectBytes: number) => void,
  ): Promise<FetchedBlock> {
    const { lane: state, purpose, started, expected, permit, rescueReason } = admission
    const expectedMilliseconds = expected.milliseconds
    const bytes = demandBytes(demand)
    const releaseAttempt = this.#attemptLifetimes.begin(demand.leaseId)
    let outcome: SamplingOutcome = 'cancelled'
    let received = 0
    try {
      signal.throwIfAborted()
      const observation = Object.freeze({
        dispatchSequence: this.#dispatchSequence.next(),
        laneId: state.lane.id, laneEpoch: state.laneEpoch, route: state.route,
        fileId: demand.descriptor.fileIdText, localBlockIndex: demand.localBlockIndex,
      })
      attempts.started(state, observation.dispatchSequence)
      try {
        this.#onBlockDispatched(observation)
      } catch { /* Diagnostics cannot redirect authenticated work. */ }
      try {
        this.#onBlockScheduled?.(Object.freeze({
          ...observation, purpose, expectedMilliseconds, deliveryEvidence: expected.evidence,
          ...(rescueReason === undefined ? {} : { rescueReason }), admission: permit.admission,
          pendingBytes: state.performance.pendingBytes,
          bytesPerSecond: state.performance.bytesPerSecond,
        }))
      } catch { /* Scheduling observers cannot become transfer authority. */ }
      const record = await state.lane.fetchBlock(demand, signal, {
        received: bytes => { received += bytes; permit.receive(bytes); onReceive?.(bytes) },
        admitted: admission => {
          try {
            this.#onBlockScheduled?.({ ...observation, purpose, expectedMilliseconds, deliveryEvidence: expected.evidence,
              ...(rescueReason === undefined ? {} : { rescueReason }), admission,
              pendingBytes: state.performance.pendingBytes, bytesPerSecond: state.performance.bytesPerSecond })
          } catch { /* Admission evidence cannot redirect content. */ }
        },
      })
      if (received < bytes) permit.receive(bytes - received)
      outcome = 'completed'
      state.failed = false
      return { state, observation, record }
    } catch (error) {
      outcome = signal.aborted ? 'cancelled' : 'failed'
      if (signal.reason instanceof ContentRaceWon) outcome = 'superseded'
      if (!signal.aborted && isRetryableLaneFailure(error)) {
        state.failed = true
        attempts.failed(state)
      }
      throw error
    } finally {
      releaseAttempt()
      admission.close(outcome)
      if (outcome === 'superseded') {
        state.performance.superseded(bytes, this.#now() - started)
      }
    }
  }

  #observeFetched(state: LaneState, observation: BlockDispatchObservation, record: BlockRecord): void {
	this.#authenticatedBytes += record.data.byteLength
    // A retired route may finish valid content, but cannot attest which replacement path carried it.
    if (this.#lanes.get(state.lane.id) !== state) return
    deliveredRoutes.set(record, state.route)
    const useful = this.#useful.get(state.lane.id)!
    useful.bytes = Math.min(Number.MAX_SAFE_INTEGER, useful.bytes + record.data.byteLength)
    useful.lastAt = this.#now()
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
    for (const state of this.#lanes.values()) { state.unsubscribeReceive(); state.lane.close?.() }
    this.#lanes.clear()
    const reason = new Error('LaneSet is closed')
    for (const waiter of [...this.#waiters]) this.#rejectWaiter(waiter, reason)
  }

  #waitForContentAvailability(
    routes: BlockRouteEligibility | undefined,
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

  #availableLanes(routes?: BlockRouteEligibility): LaneState[] {
    return [...this.#lanes.values()].filter(candidate => routes?.allows(candidate.route) ?? true)
  }

  async #awaitReplacementOrThrow(
    failures: readonly unknown[],
    attempts: BlockAttempts,
    routes: BlockRouteEligibility,
    signal: AbortSignal,
  ): Promise<void> {
    const eligible = this.#availableLanes(routes)
    if (!attempts.exhausted && eligible.length === 0) {
      await this.#waitForContentAvailability(routes, signal)
      return
    }
    if (eligible.some(candidate => attempts.allows(candidate))) return
    if (failures.length === 0) throw new Error('No eligible content lane is available')
    attempts.reportExhausted()
    throw new BlockLaneAttemptsError(failures)
  }

  #wakeWaiters(): void {
    this.wakeContent()
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

function retryableLaneFailures(error: unknown): readonly unknown[] {
  if (!(error instanceof ContentRaceFailures)) throw error
  const fatalIndex = error.errors.findIndex((failure: unknown) => !isRetryableLaneFailure(failure))
  if (fatalIndex >= 0) throw error.errors[fatalIndex]
  return error.errors
}

function isRetryableLaneFailure(error: unknown): boolean {
  return (error instanceof SessionRuntimeError && error.scope === 'lane') ||
    (error instanceof DOMException && error.name === 'AbortError')
}
