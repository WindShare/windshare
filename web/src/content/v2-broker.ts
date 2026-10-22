import { OrderedBlockWindow, RangeBufferBudget, IMMEDIATE_BLOCK_CONSUMER, type BlockConsumer } from './scheduling/range-window'
import type { ContentCapacity, ContentPriority as V2BlockPriority } from './scheduling/selection'
import { bigintToSafeNumber, byteRange, type ByteRange } from './geometry'
import type { V2BlockRecord, V2FileRevisionDescriptor } from './v2-records'
import {
  SharedV2BlockRouteEligibility,
  type V2BlockRouteEligibility,
} from './v2-route-policy'
import { V2LaneSet, authenticatedBlockRoute, inheritAuthenticatedBlockRoute, type V2BlockDemand } from './v2-lane-set'
import type { V2BlockTransportRoute } from './v2-route-policy'
import { MAXIMUM_SUPPLEMENTS } from './scheduling/exploration'
import { BlockBufferPool, type BlockLoadBuffers } from './scheduling/block-buffers'
import type { DownloadConnectivitySnapshot } from '../diagnostics/trace/transfer-payload'

export type { V2BlockRouteEligibility, V2BlockTransportRoute } from './v2-route-policy'
export {
  V2BlockDispatchSequenceAuthority,
  V2BlockLaneAttemptsError,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockDispatchObservation,
  type V2BlockLane,
  type V2BlockRouteObservation,
  type V2LaneSetOptions,
} from './v2-lane-set'

export const V2_BLOCK_BROKER_BUFFER_BYTES = 64 * 1024 * 1024
export const V2_BLOCK_BROKER_RANGE_BUFFER_BYTES = 64 * 1024 * 1024
export const V2_BLOCK_BROKER_PARALLEL_READS = 8
export const V2_BLOCK_BROKER_UPSTREAM_READS = 8

export type { ContentPriority as V2BlockPriority } from './scheduling/selection'

interface ObservedBlockConsumer extends BlockConsumer {
  readonly priority: V2BlockPriority
  readonly onReceive?: (objectBytes: number) => void
  readonly signal?: AbortSignal
}

interface SharedBlockLoad {
  readonly capacity: ContentCapacity
  releaseConsumer: (() => void) | undefined
  readonly buffers: BlockLoadBuffers
  record: V2BlockRecord | undefined
  retired: boolean
  activeAttempts: number
  readonly controller: AbortController
  readonly demand: V2BlockDemand
  readonly signal: AbortSignal
  readonly consumers: Set<ObservedBlockConsumer>
  readonly distance: () => bigint
  readonly routes: SharedV2BlockRouteEligibility
  readonly sequence: number
  readonly onReceive: (objectBytes: number) => void
  promise: Promise<V2BlockRecord>
  resolve: (record: V2BlockRecord) => void
  reject: (reason: unknown) => void
  readonly priorities: Map<V2BlockPriority, number>
  priority: V2BlockPriority
  waiters: number
  started: boolean
  settled: boolean
}

export interface V2BlockSlice {
  readonly authenticatedRoute?: V2BlockTransportRoute
  readonly offset: bigint
  readonly data: Uint8Array<ArrayBuffer>
}

export interface V2BlockRangeReaderOptions {
  /** New sealed-object traffic, including retries; never output or resume coverage. */
  readonly onReceive?: (objectBytes: number) => void
  readonly signal?: AbortSignal
  readonly maximumParallel?: number
  readonly priority?: V2BlockPriority
}

// Function-property variance prevents a route-authorized method from masquerading as this scoped port.
export interface V2BlockRangeReader {
  /** Slices are non-empty, contiguous, ascending, and cover the requested half-open range exactly. */
  readonly readRange: (
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    range: ByteRange,
    options?: V2BlockRangeReaderOptions,
  ) => AsyncGenerator<V2BlockSlice>
}

export interface V2ContentLaneStatus {
  readonly downloadConnectivity?: (final?: boolean) => DownloadConnectivitySnapshot
  readonly size: number
}

export interface V2BlockBrokerOptions {
  /** Broker payload budget: shared results, attempt buffers, and evictable cache. */
  readonly maximumBlockBufferBytes?: number
  /** Idle cache borrows unused block capacity. Zero disables retention, not transfers. */
  readonly maximumRetainedCacheBytes?: number
  readonly maximumRangeBufferBytes?: number
  readonly maximumUpstreamReads?: number
  readonly validateDemand?: (demand: V2BlockDemand) => unknown
}

export interface V2RouteAuthorizedBlockReadOptions {
  readonly onReceive?: (objectBytes: number) => void
  readonly consumer?: BlockConsumer
  readonly routes: V2BlockRouteEligibility
  readonly signal?: AbortSignal
  readonly priority?: V2BlockPriority
}

export interface V2RouteAuthorizedBlockRangeOptions extends V2BlockRangeReaderOptions {
  readonly routes: V2BlockRouteEligibility
}

export interface V2RouteAuthorizedBlockRangeReader {
  readonly readRouteAuthorizedRange: (
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    range: ByteRange,
    options: V2RouteAuthorizedBlockRangeOptions,
  ) => AsyncGenerator<V2BlockSlice>
}

/** Receiver-scoped cache/singleflight; every new upstream dispatch carries consumer route authority. */
export class V2BlockBroker implements V2RouteAuthorizedBlockRangeReader {
  readonly #lanes: V2LaneSet
  readonly #lifetime = new AbortController()
  readonly #rangeBudget: RangeBufferBudget
  readonly #buffers: BlockBufferPool
  readonly #maximumUpstreamReads: number
  readonly #validateDemand: (demand: V2BlockDemand) => unknown
  readonly #inflight = new Map<string, SharedBlockLoad>()
  readonly #queued = new Set<SharedBlockLoad>()
  #activeLoads = 0
  #loadSequence = 0
  readonly #unsubscribeBudget: () => void
  #closed = false

  constructor(lanes: V2LaneSet, options: V2BlockBrokerOptions = {}) {
    const maximumBlockBufferBytes = options.maximumBlockBufferBytes ?? V2_BLOCK_BROKER_BUFFER_BYTES
    const maximumUpstreamReads = options.maximumUpstreamReads ?? V2_BLOCK_BROKER_UPSTREAM_READS
    if (!Number.isSafeInteger(maximumUpstreamReads) || maximumUpstreamReads <= 0 ||
        maximumUpstreamReads > V2_BLOCK_BROKER_UPSTREAM_READS) {
      throw new RangeError('Block broker upstream concurrency exceeds its receiver budget')
    }
    this.#lanes = lanes
    this.#rangeBudget = new RangeBufferBudget(options.maximumRangeBufferBytes ?? V2_BLOCK_BROKER_RANGE_BUFFER_BYTES)
    this.#buffers = new BlockBufferPool(maximumBlockBufferBytes,
      options.maximumRetainedCacheBytes ?? maximumBlockBufferBytes)
    const unsubscribeRange = this.#rangeBudget.subscribe(() => this.#lanes.wakeContent())
    const unsubscribeShared = this.#buffers.subscribe(() => this.#lanes.wakeContent())
    this.#unsubscribeBudget = () => { unsubscribeRange(); unsubscribeShared() }
    this.#maximumUpstreamReads = maximumUpstreamReads
    this.#validateDemand = options.validateDemand ?? (() => undefined)
  }

  async readBlock(
    demand: V2BlockDemand,
    options: V2RouteAuthorizedBlockReadOptions,
  ): Promise<V2BlockRecord> {
    this.#requireOpen()
    options.signal?.throwIfAborted()
    options.routes.assertActive()
    this.#requireAuthorized(demand)
    const key = demandKey(demand)
    if (this.#buffers.has(key)) {
      await options.consumer?.ready(options.signal ?? this.#lifetime.signal)
      this.#requireOpen()
      options.routes.assertActive()
      this.#requireAuthorized(demand)
      // A capacity wait cannot pin an otherwise evictable cache entry.
      const cached = this.#buffers.get(key)
      if (cached !== undefined) return copyRecord(cached)
    }
    let load = this.#inflight.get(key)
    if (load === undefined) {
      load = this.#createLoad(demand, options.priority ?? 'download')
      this.#inflight.set(key, load)
      this.#queued.add(load)
      queueMicrotask(() => this.#drainQueue())
    }
    const consumer: ObservedBlockConsumer = {
      ...(options.consumer ?? IMMEDIATE_BLOCK_CONSUMER),
      priority: options.priority ?? 'download',
      ...(options.onReceive === undefined ? {} : { onReceive: options.onReceive }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    }
    load.consumers.add(consumer)
    const priority = options.priority ?? 'download'
    const releaseRoutes = load.routes.add(options.routes)
    // A canceled consumer loses dispatch authority synchronously. Waiting for the
    // rejected promise continuation would leave one microtask where stale relay
    // eligibility could start new upstream work.
    const abortRoutes = () => releaseRoutes()
    options.signal?.addEventListener('abort', abortRoutes, { once: true })
    this.#addPriority(load, priority)
    load.waiters += 1
    this.#lanes.wakeContent()
    try {
      const record = await awaitWithAbort(load.promise, options.signal)
      await consumer.ready(AbortSignal.any([load.signal, options.signal ?? this.#lifetime.signal]))
      options.routes.assertActive()
      this.#requireAuthorized(demand)
      return copyRecord(record)
    } finally {
      options.signal?.removeEventListener('abort', abortRoutes)
      load.waiters -= 1
      load.consumers.delete(consumer)
      this.#removePriority(load, priority)
      releaseRoutes()
      if (load.waiters === 0 && load.settled) this.#retireLoad(key, load)
      if (load.waiters === 0 && !load.settled) {
        const reason = new DOMException('Last block consumer left', 'AbortError')
        if (this.#inflight.get(key) === load) this.#inflight.delete(key)
        load.controller.abort(reason)
        if (!load.started) this.#settleQueued(load, reason)
      }
    }
  }

  async *readRouteAuthorizedRange(
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    range: ByteRange,
    options: V2RouteAuthorizedBlockRangeOptions,
  ): AsyncGenerator<V2BlockSlice> {
    options.routes.assertActive()
    const plan = descriptor.geometry.plan(range)
    const maximumParallel = options.maximumParallel ?? V2_BLOCK_BROKER_PARALLEL_READS
    if (!Number.isSafeInteger(maximumParallel) || maximumParallel <= 0 ||
        maximumParallel > V2_BLOCK_BROKER_PARALLEL_READS) {
      throw new RangeError('Range read parallelism exceeds its consumer budget')
    }
    const controller = new AbortController()
    const signal = AbortSignal.any([this.#lifetime.signal, ...(options.signal === undefined ? [] : [options.signal])])
    const unlink = forwardAbort(signal, controller)
    const window = new OrderedBlockWindow({
      first: plan.blocks.first, end: plan.blocks.end, parallel: maximumParallel, budget: this.#rangeBudget,
      priority: options.priority ?? 'download',
      bytes: index => {
        const block = descriptor.geometry.blockPlaintext(index)
        return bigintToSafeNumber(block.end - block.start, 'buffered block bytes')
      },
      read: (localBlockIndex, consumer) => this.readBlock({ descriptor, leaseId, localBlockIndex }, {
        routes: options.routes, signal: controller.signal, priority: options.priority ?? 'download', consumer,
        ...(options.onReceive === undefined ? {} : { onReceive: options.onReceive }),
      }),
    })
    try {
      for (let emitted = plan.blocks.first; emitted < plan.blocks.end; emitted += 1n) {
        const record = await window.next(controller.signal)
        const slice = plan.sliceForBlock(emitted)
        if (slice === undefined) throw new Error('Block broker produced an out-of-range block')
        const start = bigintToSafeNumber(slice.offsetWithinBlock, 'block slice offset')
        const length = bigintToSafeNumber(slice.requestedBytes.end - slice.requestedBytes.start, 'block slice length')
        const data = record.data.subarray(start, start + length)
        yield Object.freeze({
          offset: slice.requestedBytes.start, data,
          ...(authenticatedBlockRoute(record) === undefined ? {} : { authenticatedRoute: authenticatedBlockRoute(record)! }),
        })
        // Advancing the iterator acknowledges that the sink no longer owns this
        // independent copy. Network receipt alone cannot release output bytes.
        window.consumed()
      }
    } finally {
      unlink()
      controller.abort(new DOMException('Range consumer left', 'AbortError'))
      await window.close()
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
      if (loads.length === 0) {
        // A winner can unblock output before its canceled rescue attempts have settled.
        await this.#lanes.waitForLeaseIdle(leaseId)
        return
      }
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
    this.#buffers.invalidate(prefix)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#unsubscribeBudget()
    this.#lifetime.abort(new Error('Block broker closed'))
    for (const load of this.#inflight.values()) {
      load.controller.abort(new Error('Block broker closed'))
      if (!load.started) this.#settleQueued(load, new Error('Block broker closed'))
    }
    this.#inflight.clear()
    this.#queued.clear()
    this.#buffers.clearCache()
  }

  #createLoad(demand: V2BlockDemand, priority: V2BlockPriority): SharedBlockLoad {
    let resolve!: (record: V2BlockRecord) => void
    let reject!: (reason: unknown) => void
    const promise = new Promise<V2BlockRecord>((accepted, rejected) => {
      resolve = accepted
      reject = rejected
    })
    const controller = new AbortController()
    const consumers = new Set<ObservedBlockConsumer>()
    const block = demand.descriptor.geometry.blockPlaintext(demand.localBlockIndex)
    const bytes = bigintToSafeNumber(block.end - block.start, 'shared block bytes')
    const buffers = this.#buffers.createLoad(bytes)
    const attemptLimit = () => this.#maximumUpstreamReads + (load.activeAttempts > 0 ? MAXIMUM_SUPPLEMENTS : 0)
    const load: SharedBlockLoad = {
      capacity: {
        lastServed: () => Math.min(...[...consumers].map(consumer => consumer.lastServed())),
        available: () => this.#activeLoads < attemptLimit() &&
          buffers.available(load.priority) &&
          (load.releaseConsumer !== undefined || [...consumers].some(consumer => !consumer.signal?.aborted && consumer.canDispatch())),
        acquire: () => {
          if (this.#activeLoads >= attemptLimit() ||
            !buffers.available(load.priority)) return undefined
          const hadConsumer = load.releaseConsumer !== undefined
          if (!hadConsumer) {
            const consumer = [...consumers].find(candidate => !candidate.signal?.aborted && candidate.canDispatch())
            if (consumer === undefined) return undefined
            load.releaseConsumer = consumer.acquire()
            if (load.releaseConsumer === undefined) return undefined
          }
          const releaseAttempt = buffers.acquireAttempt(load.priority)
          if (releaseAttempt === undefined) {
            if (!hadConsumer) { load.releaseConsumer?.(); load.releaseConsumer = undefined }
            return undefined
          }
          this.#activeLoads++
          load.activeAttempts++
          return () => { releaseAttempt(); this.#activeLoads--; load.activeAttempts--; this.#lanes.wakeContent() }
        },
      },
      buffers, releaseConsumer: undefined, record: undefined, retired: false, activeAttempts: 0,
      controller, signal: controller.signal, consumers,
      onReceive: bytes => {
        const observers = new Set([...consumers].filter(consumer => !consumer.signal?.aborted)
          .map(consumer => consumer.onReceive))
        for (const observe of observers) {
          try { observe?.(bytes) } catch { /* Receipt observations cannot settle shared content. */ }
        }
      },
      distance: () => [...consumers].filter(consumer => consumer.priority === load.priority).reduce((distance, consumer) => {
        const current = consumer.distance()
        return current < distance ? current : distance
      }, BigInt(Number.MAX_SAFE_INTEGER)),
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
    return load
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
    for (const load of this.#queued) {
      this.#queued.delete(load)
      load.started = true
      const key = demandKey(load.demand)
      this.#lanes.fetchWork(load)
        .then((record) => {
          load.settled = true
          if (load.controller.signal.aborted || load.waiters === 0) {
            load.reject(load.controller.signal.reason ?? new DOMException('Block load abandoned', 'AbortError'))
            return
          }
          load.record = record
          load.resolve(record)
        }, (error: unknown) => { load.settled = true; load.reject(error) })
        .finally(() => {
          load.settled = true
          load.releaseConsumer?.()
          load.releaseConsumer = undefined
          this.#lanes.wakeContent()
          if (load.waiters === 0) this.#retireLoad(key, load)
          this.#drainQueue()
        })
    }
  }

  #settleQueued(load: SharedBlockLoad, reason: unknown): void {
    if (load.settled) return
    load.settled = true
    this.#queued.delete(load)
    const key = demandKey(load.demand)
    if (this.#inflight.get(key) === load) this.#inflight.delete(key)
    load.reject(reason)
  }

  #retireLoad(key: string, load: SharedBlockLoad): void {
    if (load.retired) return
    load.retired = true
    if (this.#inflight.get(key) === load) this.#inflight.delete(key)
    load.buffers.release()
    if (!this.#closed && !load.signal.aborted && load.record !== undefined) this.#buffers.retain(key, load.record)
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Block broker is closed')
  }

  #requireAuthorized(demand: V2BlockDemand): void {
    const failure = this.#validateDemand(demand)
    if (failure !== undefined) throw failure
  }
}

function copyRecord(record: V2BlockRecord): V2BlockRecord {
  const copy = { ...record, data: record.data.slice() }
  // Route provenance belongs to the authenticated bytes, including consumer copies.
  inheritAuthenticatedBlockRoute(record, copy)
  return copy
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
