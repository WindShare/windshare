import type { DownloadMetrics } from './download-metrics'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import { byteRange, type ByteRange } from '../content/geometry'
import {
  type V2BlockRangeReader,
  type V2BlockRangeReaderOptions,
  type V2BlockRouteEligibility,
  type V2BlockSlice,
  type V2ContentLaneStatus,
  type V2RouteAuthorizedBlockRangeReader,
  V2LaneSet,
} from '../content/v2-broker'
import {
  type V2OpenedRevision,
  type V2RevisionReader,
  V2RevisionChangedDuringRecoveryError,
  requiresV2RevisionLeaseReplacement,
  V2RevisionService,
} from '../content/v2-session-services'
import type { V2FileRevisionDescriptor } from '../content/v2-records'

export interface V2ContentGeneration {
  readonly id: number
  readonly revisions: V2RevisionService
  readonly broker: V2RouteAuthorizedBlockRangeReader
  readonly lanes: V2LaneSet
}

export interface V2ContentGenerationProvider {
  downloadMetrics?(routes: V2BlockRouteEligibility): DownloadMetrics | undefined
  execute<T>(
    signal: AbortSignal | undefined,
    operation: (generation: V2ContentGeneration) => Promise<T>,
  ): Promise<{ readonly generation: V2ContentGeneration; readonly value: T }>
  recover(
    generation: V2ContentGeneration,
    error: unknown,
    signal: AbortSignal | undefined,
  ): Promise<boolean>
  isCurrent(generation: V2ContentGeneration): boolean
  contentLaneCount(routes: V2BlockRouteEligibility): number
}

interface V2GenerationRevision {
  readonly generation: V2ContentGeneration
  readonly opened: V2OpenedRevision
}

export interface V2ScopedContent {
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly lanes: V2ContentLaneStatus
}

export class V2SupervisedContent {
  readonly #provider: V2ContentGenerationProvider
  readonly #randomBytes: (length: number) => Uint8Array
  readonly #authorizations = new Map<string, V2RevisionAuthorization>()
  #closed = false

  constructor(
    provider: V2ContentGenerationProvider,
    randomBytes: (length: number) => Uint8Array = secureRandomBytes,
  ) {
    this.#provider = provider
    this.#randomBytes = randomBytes
  }

  forRoutes(routes: V2BlockRouteEligibility): V2ScopedContent {
    this.#requireOpen()
    routes.assertActive()
    const metrics = this.#provider.downloadMetrics?.(routes)
    const status = new V2SupervisedLaneStatus(() => this.#currentLaneCount(routes))
    return Object.freeze({
      revisions: Object.freeze({
        open: (fileId: Uint8Array, signal?: AbortSignal) => this.#open(fileId, routes, signal),
      }),
      broker: Object.freeze({
        readRange: (
          descriptor: V2FileRevisionDescriptor,
          leaseId: Uint8Array,
          range: ByteRange,
          options: V2BlockRangeReaderOptions = {},
        ) => observeDownloadRange(this.#readRange(descriptor, leaseId, range, routes, options), descriptor, metrics),
      }),
      lanes: Object.freeze({
        get size() { return status.size },
        ...(metrics === undefined ? {} : { downloadConnectivity: (final = false) => metrics.snapshot(final) }),
      }),
    })
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    for (const authorization of this.#authorizations.values()) authorization.close()
    this.#authorizations.clear()
  }

  async #open(
    fileId: Uint8Array,
    routes: V2BlockRouteEligibility,
    signal?: AbortSignal,
  ): Promise<V2OpenedRevision> {
    this.#requireOpen()
    routes.assertActive()
    const normalizedFileId = fileId.slice()
    const result = await this.#provider.execute(signal, (generation) =>
      generation.revisions.open(normalizedFileId, routes, signal))
    try {
      routes.assertActive()
    } catch (error) {
      await result.value.release().catch(() => undefined)
      throw error
    }
    const token = this.#newToken()
    const authorization = new V2RevisionAuthorization(
      token,
      normalizedFileId,
      routes,
      result.generation,
      result.value,
      this.#provider,
      () => this.#authorizations.delete(encodeBase64Url(token)),
    )
    this.#authorizations.set(encodeBase64Url(token), authorization)
    return Object.freeze({
      descriptor: result.value.descriptor,
      leaseId: token.slice(),
      release: () => authorization.release(),
    })
  }

  async *#readRange(
    descriptor: V2FileRevisionDescriptor,
    token: Uint8Array,
    requested: ByteRange,
    routes: V2BlockRouteEligibility,
    options: V2BlockRangeReaderOptions,
  ): AsyncGenerator<V2BlockSlice> {
    this.#requireOpen()
    routes.assertActive()
    const authorization = this.#authorization(token)
    authorization.requireDescriptor(descriptor)
    authorization.requireRoutes(routes)
    const checked = descriptor.geometry.requireRange(requested)
    const progress: RangeProgress = { offset: checked.start, replacementOffset: undefined }
    authorization.beginRead()
    try {
      while (progress.offset < checked.end) {
        options.signal?.throwIfAborted()
        const binding = await authorization.binding(options.signal)
        try {
          yield* readGenerationRange(binding, progress, checked.end, routes, options)
        } catch (error) {
          await this.#recoverRead(authorization, binding, progress, checked.end, error, options.signal)
        }
      }
    } finally {
      authorization.endRead()
    }
  }

  async #recoverRead(
    authorization: V2RevisionAuthorization,
    binding: V2GenerationRevision,
    progress: RangeProgress,
    end: bigint,
    error: unknown,
    signal: AbortSignal | undefined,
  ): Promise<void> {
    signal?.throwIfAborted()
    if (requiresV2RevisionLeaseReplacement(error)) {
      if (progress.offset === end) return
      // A repeatedly expired fresh lease without any useful bytes cannot
      // create an unbounded reopen loop.
      if (progress.replacementOffset === progress.offset) throw error
      await authorization.replaceExpired(binding, signal)
      progress.replacementOffset = progress.offset
      return
    }
    if (!(await this.#provider.recover(binding.generation, error, signal))) throw error
    authorization.invalidate(binding)
  }

  #authorization(token: Uint8Array): V2RevisionAuthorization {
    if (token.byteLength !== 16) throw new TypeError('Supervised revision token has an invalid width')
    const authorization = this.#authorizations.get(encodeBase64Url(token))
    if (authorization === undefined) throw new Error('Supervised revision authorization is inactive')
    return authorization
  }

  #newToken(): Uint8Array<ArrayBuffer> {
    for (let attempt = 0; attempt < 16; attempt += 1) {
      const token = this.#randomBytes(16).slice()
      if (
        token.byteLength === 16 &&
        token.some((byte) => byte !== 0) &&
        !this.#authorizations.has(encodeBase64Url(token))
      ) return token
    }
    throw new Error('Unable to allocate a unique supervised revision token')
  }

  #currentLaneCount(routes: V2BlockRouteEligibility): number {
    if (this.#closed || !routes.active) return 0
    return this.#provider.contentLaneCount(routes)
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Supervised content is closed')
  }
}

async function* observeDownloadRange(
  source: AsyncGenerator<V2BlockSlice>, descriptor: V2FileRevisionDescriptor,
  metrics: DownloadMetrics | undefined,
): AsyncGenerator<V2BlockSlice> {
  const revision = descriptor.shareInstanceId + '/' + descriptor.fileIdText + '/' + descriptor.fileRevisionText
  try {
    while (true) {
      const endWait = metrics?.pending()
      let next: IteratorResult<V2BlockSlice>
      try { next = await source.next() } finally { endWait?.() }
      if (next.done) return
      const slice = next.value
      metrics?.delivered(revision, slice.offset, slice.offset + BigInt(slice.data.byteLength), slice.authenticatedRoute)
      // The caller can spend arbitrary time in output/user work at this yield.
      yield slice
    }
  } finally { await source.return(undefined) }
}

class V2SupervisedLaneStatus {
  readonly #size: () => number

  constructor(size: () => number) {
    this.#size = size
  }

  get size(): number {
    return this.#size()
  }
}

class V2RevisionAuthorization {
  readonly #token: Uint8Array<ArrayBuffer>
  readonly #fileId: Uint8Array<ArrayBuffer>
  readonly #routes: V2BlockRouteEligibility
  readonly #descriptor: V2FileRevisionDescriptor
  readonly #provider: V2ContentGenerationProvider
  readonly #onRelease: () => void
  readonly #unsubscribeRoutes: () => void
  readonly #lifetime = new AbortController()
  #current: V2GenerationRevision | undefined
  #bindingTask: Promise<V2GenerationRevision> | undefined
  #activeReads = 0
  #idleWaiters: Array<() => void> = []
  #released = false
  #closed = false
  #releaseTask: Promise<void> | undefined
  readonly #retirements = new Set<Promise<void>>()
  readonly #expiredBindings = new Set<V2GenerationRevision>()
  #retirementFailure: unknown

  constructor(
    token: Uint8Array,
    fileId: Uint8Array,
    routes: V2BlockRouteEligibility,
    generation: V2ContentGeneration,
    opened: V2OpenedRevision,
    provider: V2ContentGenerationProvider,
    onRelease: () => void,
  ) {
    this.#token = token.slice()
    this.#fileId = fileId.slice()
    this.#routes = routes
    this.#descriptor = opened.descriptor
    this.#provider = provider
    this.#onRelease = onRelease
    this.#unsubscribeRoutes = routes.subscribe(() => {
      if (routes.active) return
      this.#lifetime.abort(new DOMException('Content activation closed', 'AbortError'))
      this.release().catch(() => undefined)
    })
    this.#current = { generation, opened }
  }

  requireDescriptor(descriptor: V2FileRevisionDescriptor): void {
    if (!sameRevision(this.#descriptor, descriptor)) {
      throw new TypeError('Supervised revision token was rebound to another file revision')
    }
  }

  requireRoutes(routes: V2BlockRouteEligibility): void {
    if (this.#routes !== routes) {
      throw new TypeError('Supervised revision token was rebound to another content activation')
    }
    this.#routes.assertActive()
  }

  beginRead(): void {
    if (this.#released) throw new Error('Supervised revision authorization is released')
    this.#activeReads += 1
  }

  endRead(): void {
    this.#activeReads -= 1
    if (this.#activeReads !== 0) return
    for (const resolve of this.#idleWaiters.splice(0)) resolve()
  }

  async binding(signal?: AbortSignal): Promise<V2GenerationRevision> {
    if (this.#closed) throw new Error('Supervised revision authorization is closed')
    while (true) {
      signal?.throwIfAborted()
      const existing = this.#current
      if (existing !== undefined && this.#provider.isCurrent(existing.generation)) return existing
      const task = this.#bindingTask ?? this.#startBinding()
      const binding = await awaitBinding(task, signal)
      if (this.#provider.isCurrent(binding.generation)) return binding
    }
  }

  async replaceExpired(binding: V2GenerationRevision, signal?: AbortSignal): Promise<void> {
    if (this.#current === binding) {
      this.#current = undefined
      this.#expiredBindings.add(binding)
    }
    await this.binding(signal)
  }

  #retireExpiredBindings(): void {
    for (const binding of this.#expiredBindings) this.#retire(binding)
    this.#expiredBindings.clear()
  }

  #retire(binding: V2GenerationRevision): void {
    if (!this.#provider.isCurrent(binding.generation)) return
    const task = binding.opened.release().catch((error: unknown) => {
      if (!requiresV2RevisionLeaseReplacement(error)) this.#retirementFailure ??= error
    })
    this.#retirements.add(task)
    task.then(() => this.#retirements.delete(task))
  }

  invalidate(binding: V2GenerationRevision): void {
    // A physical lane replacement does not burn a ProtocolSession lease. Only a
    // generation transition requires reopening the authorization on new keys.
    if (this.#current === binding && !this.#provider.isCurrent(binding.generation)) {
      this.#current = undefined
    }
  }

  release(): Promise<void> {
    this.#releaseTask ??= this.#release()
    return this.#releaseTask
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#released = true
    this.#lifetime.abort(new DOMException('Supervised revision authorization closed', 'AbortError'))
    this.#unsubscribeRoutes()
    this.#current = undefined
    this.#expiredBindings.clear()
    this.#token.fill(0)
    for (const resolve of this.#idleWaiters.splice(0)) resolve()
  }

  async #release(): Promise<void> {
    if (this.#released) return
    this.#released = true
    await this.#waitForIdle()
    this.#closed = true
    this.#lifetime.abort(new DOMException('Supervised revision authorization released', 'AbortError'))
    await this.#bindingTask?.catch(() => undefined)
    const current = this.#current
    this.#current = undefined
    try {
      if (current !== undefined) this.#retire(current)
      this.#retireExpiredBindings()
      await Promise.all(this.#retirements)
      if (this.#retirementFailure !== undefined) throw this.#retirementFailure
    } finally {
      this.#unsubscribeRoutes()
      this.#token.fill(0)
      this.#onRelease()
    }
  }

  #waitForIdle(): Promise<void> {
    if (this.#activeReads === 0) return Promise.resolve()
    return new Promise((resolve) => this.#idleWaiters.push(resolve))
  }

  #startBinding(): Promise<V2GenerationRevision> {
    const task = this.#openBinding()
    this.#bindingTask = task
    task.then(
      () => {
        if (this.#bindingTask === task) this.#bindingTask = undefined
      },
      () => {
        if (this.#bindingTask === task) this.#bindingTask = undefined
      },
    )
    return task
  }

  async #openBinding(): Promise<V2GenerationRevision> {
    while (true) {
      this.#lifetime.signal.throwIfAborted()
      this.#routes.assertActive()
      const result = await this.#provider.execute(this.#lifetime.signal, (generation) =>
        generation.revisions.open(this.#fileId, this.#routes, this.#lifetime.signal))
      try {
        this.#routes.assertActive()
      } catch (error) {
        await result.value.release().catch(() => undefined)
        throw error
      }
      if (this.#lifetime.signal.aborted || !this.#provider.isCurrent(result.generation)) {
        await result.value.release().catch(() => undefined)
        this.#lifetime.signal.throwIfAborted()
        continue
      }
      if (!sameRevision(this.#descriptor, result.value.descriptor)) {
        await result.value.release().catch(() => undefined)
        throw new V2RevisionChangedDuringRecoveryError()
      }
      const next = { generation: result.generation, opened: result.value }
      this.#current = next
      // Acquire and verify first: relinquishing the last old lease can retire
      // the sender's stable source. Broker retirement is then independent of
      // useful reads, including readers suspended inside an output consumer.
      this.#retireExpiredBindings()
      return next
    }
  }
}

function sameRevision(
  left: V2FileRevisionDescriptor,
  right: V2FileRevisionDescriptor,
): boolean {
  return equalBytes(left.shareInstance, right.shareInstance) &&
    equalBytes(left.fileId, right.fileId) &&
    equalBytes(left.fileRevision, right.fileRevision) &&
    left.exactSize === right.exactSize &&
    left.geometry.blockSize === right.geometry.blockSize &&
    left.modifiedTime?.seconds === right.modifiedTime?.seconds &&
    left.modifiedTime?.nanoseconds === right.modifiedTime?.nanoseconds &&
    left.modifiedTime?.precision === right.modifiedTime?.precision
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(length)
  crypto.getRandomValues(value)
  return value
}

function requireNextOffset(slice: V2BlockSlice, expected: bigint, end: bigint): bigint {
  if (slice.offset !== expected || slice.data.byteLength === 0) {
    throw new Error('Recovered block reader returned a gap or empty slice')
  }
  const next = expected + BigInt(slice.data.byteLength)
  if (next > end) throw new Error('Recovered block reader exceeded the requested range')
  return next
}

function awaitBinding<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (signal === undefined) return promise
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(
      signal.reason ?? new DOMException('Revision recovery aborted', 'AbortError'),
    )
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
    promise.then(
      (value) => {
        signal.removeEventListener('abort', abort)
        resolve(value)
      },
      (error: unknown) => {
        signal.removeEventListener('abort', abort)
        reject(error)
      },
    )
  })
}

interface RangeProgress {
  offset: bigint
  replacementOffset: bigint | undefined
}

async function* readGenerationRange(
  binding: V2GenerationRevision,
  progress: RangeProgress,
  end: bigint,
  routes: V2BlockRouteEligibility,
  options: V2BlockRangeReaderOptions,
): AsyncGenerator<V2BlockSlice> {
  let yielded = false
  for await (const slice of binding.generation.broker.readRouteAuthorizedRange(
    binding.opened.descriptor, binding.opened.leaseId, byteRange(progress.offset, end),
    { ...options, routes },
  )) {
    yielded = true
    progress.offset = requireNextOffset(slice, progress.offset, end)
    yield slice
  }
  if (!yielded && progress.offset < end) {
    throw new Error('Recovered block reader ended before the requested range')
  }
}
