import { bigintToSafeNumber, byteRange, type ByteRange } from './geometry'
import type { V2BlockRecord, V2FileRevisionDescriptor } from './v2-records'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import {
  SharedV2BlockRouteEligibility,
  type V2BlockRouteEligibility,
  type V2BlockTransportRoute,
} from './v2-route-policy'

export type { V2BlockRouteEligibility, V2BlockTransportRoute } from './v2-route-policy'

export const V2_BLOCK_BROKER_CACHE_BYTES = 64 * 1024 * 1024
export const V2_BLOCK_BROKER_PARALLEL_READS = 8
export const V2_BLOCK_BROKER_UPSTREAM_READS = 8

export type V2BlockPriority = 'preview' | 'download' | 'prefetch'

const PRIORITY_WEIGHTS: Readonly<Record<V2BlockPriority, number>> = Object.freeze({
  preview: 4,
  download: 2,
  prefetch: 1,
})
const PRIORITY_ORDER: readonly V2BlockPriority[] = ['preview', 'download', 'prefetch']

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
  readonly route: V2BlockTransportRoute
  inflight: number
  failed: boolean
}

export interface V2BlockRouteObservation {
  readonly laneId: number
  readonly route: V2BlockTransportRoute
  readonly fileId: string
  readonly localBlockIndex: bigint
}

export interface V2LaneSetOptions {
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
  readonly #onBlockFetched: (observation: V2BlockRouteObservation) => void
  #rotation = 0
  #closed = false

  constructor(options: V2LaneSetOptions = {}) {
    this.#onBlockFetched = options.onBlockFetched ?? (() => undefined)
  }

  add(lane: V2BlockLane, route: V2BlockTransportRoute): void {
    if (this.#closed) throw new Error('LaneSet is closed')
    if (!Number.isInteger(lane.id) || lane.id <= 0 || this.#lanes.has(lane.id)) {
      throw new TypeError('LaneSet requires a unique positive lane identity')
    }
    this.#lanes.set(lane.id, { lane, route, inflight: 0, failed: false })
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
      try {
        // Eligibility is sampled at dispatch. Once one legitimate consumer starts
        // a shared BlockRef load, later cancellation cannot retroactively make the
        // authenticated bytes illicit for another coalesced consumer.
        const record = await state.lane.fetchBlock(demand, signal)
        state.failed = false
        try {
          this.#onBlockFetched(Object.freeze({
            laneId: state.lane.id,
            route: state.route,
            fileId: demand.descriptor.fileIdText,
            localBlockIndex: demand.localBlockIndex,
          }))
        } catch {
          // Diagnostics cannot become transfer authority or corrupt an authenticated success.
        }
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

interface SharedBlockLoad {
  readonly controller: AbortController
  readonly demand: V2BlockDemand
  readonly routes: SharedV2BlockRouteEligibility
  readonly sequence: number
  promise: Promise<V2BlockRecord>
  resolve: (record: V2BlockRecord) => void
  reject: (reason: unknown) => void
  readonly priorities: Map<V2BlockPriority, number>
  priority: V2BlockPriority
  waiters: number
  started: boolean
  settled: boolean
}

interface CachedBlock {
  readonly record: V2BlockRecord
  touched: number
}

export interface V2BlockSlice {
  readonly offset: bigint
  readonly data: Uint8Array<ArrayBuffer>
}

export interface V2BlockRangeReader {
  readRange(
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    range: ByteRange,
    options?: {
      readonly signal?: AbortSignal
      readonly maximumParallel?: number
      readonly priority?: V2BlockPriority
    },
  ): AsyncGenerator<V2BlockSlice>
}

export interface V2ContentLaneStatus {
  readonly size: number
}

export interface V2BlockBrokerOptions {
  readonly maximumCacheBytes?: number
  readonly maximumUpstreamReads?: number
  readonly validateDemand?: (demand: V2BlockDemand) => unknown
}

export interface V2BlockReadOptions {
  readonly routes: V2BlockRouteEligibility
  readonly signal?: AbortSignal
  readonly priority?: V2BlockPriority
}

export interface V2BlockRangeOptions extends V2BlockReadOptions {
  readonly maximumParallel?: number
}

/** Receiver-scoped cache/singleflight; every new upstream dispatch carries consumer route authority. */
export class V2BlockBroker {
  readonly #lanes: V2LaneSet
  readonly #maximumCacheBytes: number
  readonly #maximumUpstreamReads: number
  readonly #validateDemand: (demand: V2BlockDemand) => unknown
  readonly #inflight = new Map<string, SharedBlockLoad>()
  readonly #queued = new Set<SharedBlockLoad>()
  readonly #cache = new Map<string, CachedBlock>()
  #cacheBytes = 0
  #clock = 0
  #activeLoads = 0
  #loadSequence = 0
  readonly #priorityServed: Record<V2BlockPriority, number> = {
    preview: 0,
    download: 0,
    prefetch: 0,
  }
  #closed = false

  constructor(lanes: V2LaneSet, options: V2BlockBrokerOptions = {}) {
    const maximumCacheBytes = options.maximumCacheBytes ?? V2_BLOCK_BROKER_CACHE_BYTES
    const maximumUpstreamReads = options.maximumUpstreamReads ?? V2_BLOCK_BROKER_UPSTREAM_READS
    if (!Number.isSafeInteger(maximumCacheBytes) || maximumCacheBytes <= 0) {
      throw new RangeError('Block broker cache budget must be a positive safe integer')
    }
    if (!Number.isSafeInteger(maximumUpstreamReads) || maximumUpstreamReads <= 0 ||
        maximumUpstreamReads > V2_BLOCK_BROKER_UPSTREAM_READS) {
      throw new RangeError('Block broker upstream concurrency exceeds its receiver budget')
    }
    this.#lanes = lanes
    this.#maximumCacheBytes = maximumCacheBytes
    this.#maximumUpstreamReads = maximumUpstreamReads
    this.#validateDemand = options.validateDemand ?? (() => undefined)
  }

  async readBlock(
    demand: V2BlockDemand,
    options: V2BlockReadOptions,
  ): Promise<V2BlockRecord> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    options.routes.assertActive()
    this.#requireAuthorized(demand)
    const key = demandKey(demand)
    const cached = this.#cache.get(key)
    if (cached !== undefined) {
      cached.touched = ++this.#clock
      this.#requireAuthorized(demand)
      return cached.record
    }
    let load = this.#inflight.get(key)
    if (load === undefined) {
      load = this.#createLoad(demand, options.priority ?? 'download')
      this.#inflight.set(key, load)
      this.#queued.add(load)
      queueMicrotask(() => this.#drainQueue())
    }
    const priority = options.priority ?? 'download'
    const releaseRoutes = load.routes.add(options.routes)
    // A canceled consumer loses dispatch authority synchronously. Waiting for the
    // rejected promise continuation would leave one microtask where stale relay
    // eligibility could start new upstream work.
    const abortRoutes = () => releaseRoutes()
    options.signal?.addEventListener('abort', abortRoutes, { once: true })
    this.#addPriority(load, priority)
    load.waiters += 1
    try {
      const record = await awaitWithAbort(load.promise, options.signal)
      options.routes.assertActive()
      this.#requireAuthorized(demand)
      return record
    } finally {
      options.signal?.removeEventListener('abort', abortRoutes)
      load.waiters -= 1
      this.#removePriority(load, priority)
      releaseRoutes()
      if (load.waiters === 0 && !load.settled) {
        const reason = new DOMException('Last block consumer left', 'AbortError')
        load.controller.abort(reason)
        if (!load.started) this.#settleQueued(load, reason)
      }
    }
  }

  async *readRange(
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    range: ByteRange,
    options: V2BlockRangeOptions,
  ): AsyncGenerator<V2BlockSlice> {
    options.routes.assertActive()
    const plan = descriptor.geometry.plan(range)
    const maximumParallel = options.maximumParallel ?? V2_BLOCK_BROKER_PARALLEL_READS
    if (!Number.isSafeInteger(maximumParallel) || maximumParallel <= 0 ||
        maximumParallel > V2_BLOCK_BROKER_PARALLEL_READS) {
      throw new RangeError('Range read parallelism exceeds its consumer budget')
    }
    const pending = new Map<bigint, Promise<V2BlockRecord>>()
    const controller = new AbortController()
    const unlink = forwardAbort(options.signal, controller)
    let scheduled = plan.blocks.first
    let emitted = plan.blocks.first
    try {
      while (emitted < plan.blocks.end) {
        controller.signal.throwIfAborted()
        while (scheduled < plan.blocks.end && pending.size < maximumParallel) {
          const index = scheduled
          pending.set(index, this.readBlock(
            { descriptor, leaseId, localBlockIndex: index },
            {
              routes: options.routes,
              signal: controller.signal,
              priority: options.priority ?? 'download',
            },
          ))
          scheduled += 1n
        }
        const promise = pending.get(emitted)
        if (promise === undefined) throw new Error('Block broker scheduling lost an index')
        const record = await promise
        pending.delete(emitted)
        const slice = plan.sliceForBlock(emitted)
        if (slice === undefined) throw new Error('Block broker produced an out-of-range block')
        const start = bigintToSafeNumber(slice.offsetWithinBlock, 'block slice offset')
        const length = bigintToSafeNumber(
          slice.requestedBytes.end - slice.requestedBytes.start,
          'block slice length',
        )
        yield Object.freeze({
          offset: slice.requestedBytes.start,
          data: record.data.slice(start, start + length),
        })
        emitted += 1n
      }
    } finally {
      unlink()
      if (pending.size > 0) {
        controller.abort(new DOMException('Range consumer left', 'AbortError'))
        await Promise.allSettled(pending.values())
      }
    }
  }

  async waitForLeaseIdle(leaseId: Uint8Array): Promise<void> {
    // The upstream operation is authorized with the lease of its first waiter.
    // Keep that lease alive until a shared request has produced its authenticated
    // block, even when a different consumer is the one still awaiting the result.
    while (true) {
      const loads = [...this.#inflight.values()].filter(
        (load) => !load.settled && sameBytes(load.demand.leaseId, leaseId),
      )
      if (loads.length === 0) return
      await Promise.allSettled(loads.map((load) => load.promise))
    }
  }

  invalidateRevision(descriptor: V2FileRevisionDescriptor): void {
    const prefix = revisionKey(descriptor)
    for (const [key, load] of this.#inflight) {
      if (key.startsWith(prefix)) {
        const reason = new Error('File revision was invalidated')
        load.controller.abort(reason)
        if (!load.started) this.#settleQueued(load, reason)
      }
    }
    for (const [key, cached] of this.#cache) {
      if (key.startsWith(prefix)) {
        this.#cache.delete(key)
        this.#cacheBytes -= cached.record.data.byteLength
      }
    }
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    for (const load of this.#inflight.values()) {
      load.controller.abort(new Error('Block broker closed'))
      if (!load.started) this.#settleQueued(load, new Error('Block broker closed'))
    }
    this.#inflight.clear()
    this.#queued.clear()
    this.#cache.clear()
    this.#cacheBytes = 0
  }

  #createLoad(demand: V2BlockDemand, priority: V2BlockPriority): SharedBlockLoad {
    let resolve!: (record: V2BlockRecord) => void
    let reject!: (reason: unknown) => void
    const promise = new Promise<V2BlockRecord>((accepted, rejected) => {
      resolve = accepted
      reject = rejected
    })
    return {
      controller: new AbortController(),
      demand,
      routes: new SharedV2BlockRouteEligibility(),
      sequence: this.#loadSequence++,
      promise,
      resolve,
      reject,
      priorities: new Map(),
      priority,
      waiters: 0,
      started: false,
      settled: false,
    }
  }

  #addPriority(load: SharedBlockLoad, priority: V2BlockPriority): void {
    load.priorities.set(priority, (load.priorities.get(priority) ?? 0) + 1)
    load.priority = highestPriority(load.priorities)
  }

  #removePriority(load: SharedBlockLoad, priority: V2BlockPriority): void {
    const remaining = (load.priorities.get(priority) ?? 0) - 1
    if (remaining <= 0) load.priorities.delete(priority)
    else load.priorities.set(priority, remaining)
    if (load.priorities.size > 0) load.priority = highestPriority(load.priorities)
  }

  #drainQueue(): void {
    if (this.#closed) return
    while (this.#activeLoads < this.#maximumUpstreamReads) {
      const load = this.#nextQueued()
      if (load === undefined) return
      this.#queued.delete(load)
      load.started = true
      this.#activeLoads += 1
      const key = demandKey(load.demand)
      this.#lanes.fetch(load.demand, load.routes, load.controller.signal)
        .then((record) => {
          if (load.controller.signal.aborted || load.waiters === 0) {
            load.reject(load.controller.signal.reason ?? new DOMException('Block load abandoned', 'AbortError'))
            return
          }
          this.#commitCache(key, record)
          load.resolve(record)
        }, (error: unknown) => load.reject(error))
        .finally(() => {
          load.settled = true
          this.#activeLoads -= 1
          if (this.#inflight.get(key) === load) this.#inflight.delete(key)
          this.#drainQueue()
        })
    }
  }

  #nextQueued(): SharedBlockLoad | undefined {
    for (const priority of PRIORITY_ORDER) {
      if (this.#priorityServed[priority] >= PRIORITY_WEIGHTS[priority]) continue
      const candidate = this.#oldestQueued(priority)
      if (candidate !== undefined) {
        this.#priorityServed[priority] += 1
        return candidate
      }
    }
    if (![...this.#queued].some((load) => !load.settled)) return undefined
    // A fresh weighted round is permitted when the remaining queued priorities
    // have exhausted their shares; absent classes never make useful work wait.
    for (const priority of PRIORITY_ORDER) this.#priorityServed[priority] = 0
    return this.#nextQueued()
  }

  #oldestQueued(priority: V2BlockPriority): SharedBlockLoad | undefined {
    let oldest: SharedBlockLoad | undefined
    for (const load of this.#queued) {
      if (load.settled || load.priority !== priority) continue
      if (oldest === undefined || load.sequence < oldest.sequence) oldest = load
    }
    return oldest
  }

  #settleQueued(load: SharedBlockLoad, reason: unknown): void {
    if (load.settled) return
    load.settled = true
    this.#queued.delete(load)
    const key = demandKey(load.demand)
    if (this.#inflight.get(key) === load) this.#inflight.delete(key)
    load.reject(reason)
  }

  #commitCache(key: string, record: V2BlockRecord): void {
    const bytes = record.data.byteLength
    if (bytes > this.#maximumCacheBytes) return
    while (this.#cacheBytes + bytes > this.#maximumCacheBytes) this.#evictOldest()
    this.#cache.set(key, { record, touched: ++this.#clock })
    this.#cacheBytes += bytes
  }

  #evictOldest(): void {
    let oldestKey: string | undefined
    let oldestTouch = Number.POSITIVE_INFINITY
    for (const [key, cached] of this.#cache) {
      if (cached.touched < oldestTouch) {
        oldestKey = key
        oldestTouch = cached.touched
      }
    }
    if (oldestKey === undefined) return
    const cached = this.#cache.get(oldestKey)
    this.#cache.delete(oldestKey)
    if (cached !== undefined) this.#cacheBytes -= cached.record.data.byteLength
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Block broker is closed')
  }

  #requireAuthorized(demand: V2BlockDemand): void {
    const failure = this.#validateDemand(demand)
    if (failure !== undefined) throw failure
  }
}

function highestPriority(priorities: ReadonlyMap<V2BlockPriority, number>): V2BlockPriority {
  if ((priorities.get('preview') ?? 0) > 0) return 'preview'
  if ((priorities.get('download') ?? 0) > 0) return 'download'
  return 'prefetch'
}

function demandKey(demand: V2BlockDemand): string {
  return `${revisionKey(demand.descriptor)}${demand.localBlockIndex}`
}

function revisionKey(descriptor: V2FileRevisionDescriptor): string {
  return `${descriptor.shareInstanceId}\0${descriptor.fileIdText}\0${descriptor.fileRevisionText}\0`
}

function awaitWithAbort<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (signal === undefined) return promise
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const aborted = () => reject(signal.reason ?? new DOMException('Block read aborted', 'AbortError'))
    signal.addEventListener('abort', aborted, { once: true })
    promise.then(
      (value) => {
        signal.removeEventListener('abort', aborted)
        resolve(value)
      },
      (error) => {
        signal.removeEventListener('abort', aborted)
        reject(error)
      },
    )
  })
}

function forwardAbort(signal: AbortSignal | undefined, controller: AbortController): () => void {
  if (signal === undefined) return () => undefined
  const abort = () => controller.abort(signal.reason ?? new DOMException('Range read aborted', 'AbortError'))
  signal.addEventListener('abort', abort, { once: true })
  if (signal.aborted) abort()
  return () => signal.removeEventListener('abort', abort)
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((value, index) => value === right[index])
}

export function wholeFileRange(descriptor: V2FileRevisionDescriptor): ByteRange {
  return byteRange(0n, descriptor.exactSize)
}

function isRetryableLaneFailure(error: unknown): boolean {
  return (error instanceof V2SessionRuntimeError && error.scope === 'lane') ||
    (error instanceof DOMException && error.name === 'AbortError')
}
