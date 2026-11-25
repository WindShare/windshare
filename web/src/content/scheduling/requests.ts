import type { BlockRouteEligibility, BlockTransportRoute } from '../route-policy'
import { OperationRetryError, SessionRuntimeError } from '../../session/runtime-types'

const INITIAL_DIRECT_RESPONSE_MILLISECONDS = 25
const INITIAL_RELAY_RESPONSE_MILLISECONDS = 250
const MINIMUM_RESPONSE_MILLISECONDS = 1
const RESPONSE_SAMPLE_WEIGHT = 0.25
const RESPONSE_SAMPLE_LIFETIME_MILLISECONDS = 30_000
const MAXIMUM_OPERATION_WINDOW_RETRIES = 3

export type LaneRequestKind = 'open_revisions' | 'list_children' | 'renew_lease' | 'release_lease'
interface ContentQueueEstimate {
  estimateQueueMilliseconds(): number
}
export interface RequestLane {
  readonly id: number
  readonly epoch: number
  readonly route: BlockTransportRoute
  readonly content?: ContentQueueEstimate
}
export interface RequestSchedulingObservation {
  readonly sequence: number
  readonly kind: LaneRequestKind
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: BlockTransportRoute
  readonly expectedMilliseconds: number
  readonly pendingRequests: number
  readonly transition: 'dispatched' | 'completed' | 'retried' | 'failed' | 'cancelled'
  readonly elapsedMilliseconds: number
}
interface ResponseSample {
  readonly milliseconds: number
  readonly at: number
}
interface RequestState {
  lane: RequestLane
  readonly response: Map<LaneRequestKind, ResponseSample>
  pending: number
  failed: boolean
}
export interface LaneRequest {
  readonly kind: LaneRequestKind
  readonly routes?: BlockRouteEligibility
  readonly signal?: AbortSignal
}

/** Request reservations account for their own lifetime, independently of content allocation. */
export class LaneRequests {
  readonly #states = new Map<number, RequestState>()
  readonly #changed = new Set<() => void>()
  readonly #lifetime = new AbortController()
  readonly #now: () => number
  readonly #observe: ((observation: RequestSchedulingObservation) => void) | undefined
  #sequence = 0

  constructor(options: {
    readonly now?: () => number
    readonly observe?: (observation: RequestSchedulingObservation) => void
  } = {}) {
    this.#now = options.now ?? (() => performance.now())
    this.#observe = options.observe
  }

  add(lane: RequestLane): void {
    this.#lifetime.signal.throwIfAborted()
    const current = this.#states.get(lane.id)
    if (current !== undefined && current.lane.epoch === lane.epoch) current.lane = { ...current.lane, ...lane }
    else this.#states.set(lane.id, { lane, response: new Map(), pending: 0, failed: false })
    for (const wake of [...this.#changed]) wake()
  }

  remove(id: number, epoch?: number): void {
    const current = this.#states.get(id)
    if (current !== undefined && (epoch === undefined || current.lane.epoch === epoch)) {
      this.#states.delete(id)
      for (const wake of [...this.#changed]) wake()
    }
  }

  async run<T>(
    request: LaneRequest,
    operation: (route: { readonly laneId: number; readonly signal: AbortSignal }) => Promise<T>,
  ): Promise<T> {
    const signal = AbortSignal.any([this.#lifetime.signal, ...(request.signal === undefined ? [] : [request.signal])])
    let windowRetries = 0
    const retriedStates = new Set<RequestState>()
    while (true) {
      const reservation = await this.#reserve(request, signal, retriedStates)
      try {
        signal.throwIfAborted()
        request.routes?.assertActive()
        const state = reservation.state
        if (this.#states.get(state.lane.id) !== state || !(request.routes?.allows(state.lane.route) ?? true)) continue
        // Validate and invoke in the same turn: detach or policy changes cannot
        // invalidate an idle reservation between its last check and dispatch.
        return await this.#execute(
          request,
          signal,
          reservation,
          operation,
          windowRetries < MAXIMUM_OPERATION_WINDOW_RETRIES,
        )
      } catch (error) {
        if (!signal.aborted && error instanceof OperationRetryError && windowRetries < MAXIMUM_OPERATION_WINDOW_RETRIES) {
          retriedStates.add(reservation.state)
          windowRetries += 1
          continue
        }
        throw error
      } finally {
        reservation.state.pending -= 1
      }
    }
  }

  async #execute<T>(
    request: LaneRequest,
    signal: AbortSignal,
    reservation: { state: RequestState; expectedMilliseconds: number },
    operation: (route: { readonly laneId: number; readonly signal: AbortSignal }) => Promise<T>,
    canRetryWindow: boolean,
  ): Promise<T> {
    const { state, expectedMilliseconds } = reservation
    const started = this.#now()
    const sequence = ++this.#sequence
    const observe = (transition: RequestSchedulingObservation['transition']) => {
      try {
        this.#observe?.({ sequence, kind: request.kind, laneId: state.lane.id, laneEpoch: state.lane.epoch,
          route: state.lane.route, expectedMilliseconds, pendingRequests: state.pending,
          transition, elapsedMilliseconds: Math.max(0, this.#now() - started) })
      } catch { /* Observations never acquire request or lease authority. */ }
    }
    observe('dispatched')
    try {
      signal.throwIfAborted()
      request.routes?.assertActive()
      const value = await operation({ laneId: state.lane.id, signal })
      const now = this.#now()
      const elapsed = Math.max(MINIMUM_RESPONSE_MILLISECONDS, now - started)
      this.#recordResponse(state, request.kind, elapsed, now)
      state.failed = false
      observe('completed')
      return value
    } catch (error) {
      if (!signal.aborted && error instanceof OperationRetryError) {
        const now = this.#now()
        const elapsed = Math.max(MINIMUM_RESPONSE_MILLISECONDS, now - started)
        // A replay-window decline proves the carrier stayed alive, so never
        // latch state.failed; only raise the latency sample when real delay
        // exceeded the lane's current estimate, decaying after sample expiry.
        if (elapsed > this.#responseMilliseconds(state, request.kind, now)) {
          state.response.set(request.kind, { milliseconds: elapsed, at: now })
        }
        observe(canRetryWindow ? 'retried' : 'failed')
        throw error
      }
      if (!signal.aborted && error instanceof SessionRuntimeError && error.scope === 'lane') state.failed = true
      observe(signal.aborted ? 'cancelled' : 'failed')
      throw error
    }
  }

  close(): void {
    this.#lifetime.abort(new Error('Request lanes are closed'))
    this.#states.clear()
  }

  async #reserve(
    request: LaneRequest,
    signal: AbortSignal,
    retriedStates: ReadonlySet<RequestState>,
  ): Promise<{ state: RequestState; expectedMilliseconds: number }> {
    while (true) {
      signal.throwIfAborted()
      request.routes?.assertActive()
      const now = this.#now()
      const states = [...this.#states.values()].filter(state => request.routes?.allows(state.lane.route) ?? true)
      states.sort((left, right) => Number(left.failed) - Number(right.failed) ||
        Number(retriedStates.has(left)) - Number(retriedStates.has(right)) ||
        this.#cost(left, request.kind, now) - this.#cost(right, request.kind, now) || left.lane.id - right.lane.id)
      const state = states[0]
      if (state !== undefined) {
        const expectedMilliseconds = this.#cost(state, request.kind, now)
        state.pending += 1
        return { state, expectedMilliseconds }
      }
      await new Promise<void>((resolve, reject) => {
        let finished = false
        const subscription: { unsubscribe?: () => void } = {}
        const finish = (reason?: unknown) => {
          if (finished) return
          finished = true
          this.#changed.delete(wake)
          signal.removeEventListener('abort', abort)
          subscription.unsubscribe?.()
          if (reason === undefined) resolve()
          else reject(reason)
        }
        const wake = () => finish()
        const abort = () => finish(signal.reason)
        this.#changed.add(wake)
        signal.addEventListener('abort', abort, { once: true })
        const unsubscribe = request.routes?.subscribe(wake)
        if (unsubscribe !== undefined) subscription.unsubscribe = unsubscribe
        if (finished) unsubscribe?.()
        if (signal.aborted) abort()
      })
    }
  }

  #recordResponse(state: RequestState, kind: LaneRequestKind, elapsed: number, now: number): void {
    const previous = state.response.get(kind)
    const fresh = previous !== undefined && now - previous.at < RESPONSE_SAMPLE_LIFETIME_MILLISECONDS
    const milliseconds = !fresh || elapsed > previous.milliseconds
      ? elapsed
      : previous.milliseconds + RESPONSE_SAMPLE_WEIGHT * (elapsed - previous.milliseconds)
    state.response.set(kind, { milliseconds, at: now })
  }

  #responseMilliseconds(state: RequestState, kind: LaneRequestKind, now: number): number {
    const sample = state.response.get(kind)
    if (sample !== undefined && now - sample.at < RESPONSE_SAMPLE_LIFETIME_MILLISECONDS) {
      return sample.milliseconds
    }
    return state.lane.route === 'direct'
      ? INITIAL_DIRECT_RESPONSE_MILLISECONDS
      : INITIAL_RELAY_RESPONSE_MILLISECONDS
  }

  #cost(state: RequestState, kind: LaneRequestKind, now: number): number {
    const response = this.#responseMilliseconds(state, kind, now)
    const queuedContent = state.lane.content?.estimateQueueMilliseconds() ?? 0
    return response * (state.pending + 1) + queuedContent
  }
}
