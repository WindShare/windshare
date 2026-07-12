import type { FrameChannel } from '../contracts/channel'
import {
  MAX_CHUNK_COUNT,
  createChunkIndex,
  type ChunkIndex,
  type TransferPlan,
} from '../contracts/selection'
import type { BlockSink } from '../contracts/sink'
import {
  createChannelEntry,
  type ChannelEntry,
  type Flight,
  type RequestSend,
} from './channel-entry'
import { ChannelSettlement } from './channel-settlement'
import { combineSinkCleanupFailure } from './cleanup-failure'
import { SessionCompletion } from './completion'
import { CompactDemand } from './demand'
import { BlockDelivery } from './delivery'
import { SessionLifetime } from './lifetime'
import {
  decodeFrame,
  encodeRequest,
  isFatalErrorCode,
  type BlockFrame,
  type ErrorFrame,
} from './frame'
import {
  BlockAttemptsExhaustedError,
  PeerSessionError,
  REQUEST_WINDOW_BLOCKS,
  SessionClosedError,
  type BlockOpener,
  type ReceiveSessionOptions,
  type ReceiveSessionSnapshot,
  type ReceiveSessionState,
} from './model'
import { normalizeReceiveOptions } from './receive-options'
import { BlockReassembler, ReassemblyViolation } from './reassembly'

const SCORE_RECENT_WEIGHT = 0.3

export class ReceiveSession {
  readonly #opener: BlockOpener
  readonly #demand: CompactDemand
  readonly #delivery: BlockDelivery
  readonly #maxBlockBytes: number
  readonly #requestTimeoutMs: number
  readonly #maxBlockAttempts: number
  readonly #pollIntervalMs: number
  readonly #now: () => number
  readonly #completion: SessionCompletion
  readonly #lifetime: SessionLifetime
  readonly #channelSettlement: ChannelSettlement
  readonly #channels = new Set<ChannelEntry>()
  readonly #assigned = new Map<ChunkIndex, Flight>()
  readonly #attempts = new Map<ChunkIndex, number>()

  #state: ReceiveSessionState = 'idle'
  constructor(
    plan: TransferPlan,
    sink: BlockSink,
    opener: BlockOpener,
    options: ReceiveSessionOptions,
  ) {
    const normalized = normalizeReceiveOptions(options, sink.deliveryOrder)
    const { deliveryOrder } = normalized
    this.#completion = new SessionCompletion()
    this.#lifetime = new SessionLifetime()
    this.#channelSettlement = new ChannelSettlement()
    this.#opener = opener
    this.#demand = new CompactDemand(
      plan.chunks,
      sink,
      deliveryOrder === 'ascending',
    )
    this.#delivery = new BlockDelivery(
      sink,
      this.#demand,
      deliveryOrder,
      () => this.#schedule(),
      (reason) => this.#terminate(reason),
    )
    this.#maxBlockBytes = normalized.maxBlockBytes
    this.#requestTimeoutMs = normalized.requestTimeoutMs
    this.#maxBlockAttempts = normalized.maxBlockAttempts
    this.#pollIntervalMs = normalized.pollIntervalMs
    this.#now = normalized.now
  }

  get state(): ReceiveSessionState {
    return this.#state
  }

  snapshot(): ReceiveSessionSnapshot {
    return Object.freeze({
      state: this.#state,
      channels: [...this.#channels].filter((entry) => !entry.retired).length,
      assignedBlocks: this.#assigned.size,
      retryBlocks: this.#demand.retryCount,
      bufferedBlocks: this.#delivery.bufferedCount,
      maxBufferedBlocks: this.#delivery.maxBufferedCount,
    })
  }

  addChannel(channel: FrameChannel): void {
    if (
      this.#state === 'finalizing' ||
      this.#state === 'completed' ||
      this.#state === 'failed' ||
      this.#state === 'closed'
    ) {
      throw new SessionClosedError('cannot add a channel after session termination')
    }
    if ([...this.#channels].some((entry) => entry.channel === channel)) {
      throw new TypeError('channel is already part of this session')
    }
    if (channel.frames.locked) {
      throw new TypeError('channel receive stream already has a consumer')
    }
    const entry = createChannelEntry(channel)
    this.#channels.add(entry)
    if (this.#state === 'running') {
      this.#startReader(entry)
      this.#schedule()
    }
  }

  start(signal?: AbortSignal): Promise<void> {
    if (this.#state !== 'idle') {
      return Promise.reject(new Error('receive session can only be started once'))
    }
    this.#state = 'running'
    try {
      this.#lifetime.observeExternalAbort(signal, (reason) => this.#terminate(reason))
      if (this.#state !== 'running') {
        return this.#completion.promise
      }
      this.#demand.start()
      for (const entry of this.#channels) {
        this.#startReader(entry)
      }
      this.#lifetime.startPolling(this.#pollIntervalMs, () => this.#onTick())
      this.#schedule()
    } catch (error) {
      this.#terminate(error)
    }
    return this.#completion.promise
  }

  async close(reason: unknown = new SessionClosedError()): Promise<void> {
    if (
      this.#state === 'finalizing' ||
      this.#state === 'completed' ||
      this.#state === 'failed' ||
      this.#state === 'closed'
    ) {
      await this.#completion.waitIgnoringFailure()
      return
    }
    this.#terminate(reason, 'closed')
    await this.#completion.waitIgnoringFailure()
  }

  #startReader(entry: ChannelEntry): void {
    if (entry.reader !== undefined || entry.retired || this.#state !== 'running') {
      return
    }
    entry.reader = entry.channel.frames.getReader()
    const reader = this.#readChannel(entry).catch((error) => this.#terminate(error))
    this.#channelSettlement.trackReader(reader)
  }

  async #readChannel(entry: ChannelEntry): Promise<void> {
    const reader = entry.reader
    if (reader === undefined) {
      return
    }
    try {
      while (this.#state === 'running') {
        const result = await reader.read()
        if (result.done) {
          this.#dropChannel(entry)
          return
        }
        await this.#onFrame(entry, result.value)
      }
    } catch {
      this.#dropChannel(entry)
    } finally {
      reader.releaseLock()
    }
  }

  #onTick(): void {
    if (this.#state !== 'running') {
      return
    }
    for (const entry of this.#channels) {
      if (!entry.retired && entry.channel.state === 'closed') {
        this.#retireChannel(entry)
      }
    }
    this.#expireAssignments()
    this.#schedule()
  }

  #schedule(): void {
    try {
      this.#scheduleUnsafe()
    } catch (error) {
      this.#terminate(error)
    }
  }

  #scheduleUnsafe(): void {
    if (this.#state !== 'running') {
      return
    }
    const open = [...this.#channels]
      .filter((entry) => !entry.retired && entry.channel.state === 'open')
      .sort((left, right) => left.score - right.score)

    for (const entry of open) {
      if (entry.sending !== undefined) {
        continue
      }
      const free = REQUEST_WINDOW_BLOCKS - entry.inflight.size
      if (free <= 0) {
        continue
      }
      const indices = this.#eligible(free)
      if (indices.length === 0) {
        break
      }
      this.#beginRequest(entry, indices)
    }
    this.#checkCompletion()
  }

  #beginRequest(entry: ChannelEntry, indices: readonly ChunkIndex[]): void {
    const controller = new AbortController()
    const abortFromSession = () => controller.abort(this.#lifetime.signal.reason)
    this.#lifetime.signal.addEventListener('abort', abortFromSession, { once: true })
    const timeout = setTimeout(
      () => controller.abort(new DOMException('Request send timed out', 'TimeoutError')),
      this.#requestTimeoutMs,
    )
    const task: RequestSend = {
      indices,
      controller,
      cleanup: () => {
        clearTimeout(timeout)
        this.#lifetime.signal.removeEventListener('abort', abortFromSession)
      },
    }
    entry.sending = task
    const started = this.#now()
    for (const index of indices) {
      entry.inflight.add(index)
      this.#assigned.set(index, { channel: entry, sentAt: started, pending: true })
    }

    const frame = encodeRequest(indices.map(BigInt))
    entry.channel.send(frame, controller.signal).then(
      () => this.#finishRequestSend(entry, task),
      () => this.#failRequestSend(entry, task),
    )
  }

  #finishRequestSend(entry: ChannelEntry, task: RequestSend): void {
    task.cleanup()
    if (entry.sending !== task) {
      return
    }
    entry.sending = undefined
    if (entry.retired || this.#state !== 'running') {
      return
    }
    const acceptedAt = this.#now()
    for (const index of task.indices) {
      const flight = this.#assigned.get(index)
      if (flight?.channel !== entry || !flight.pending) {
        continue
      }
      flight.pending = false
      flight.sentAt = acceptedAt
      this.#attempts.set(index, (this.#attempts.get(index) ?? 0) + 1)
    }
    this.#schedule()
  }

  #failRequestSend(entry: ChannelEntry, task: RequestSend): void {
    task.cleanup()
    if (entry.sending !== task) {
      return
    }
    entry.sending = undefined
    if (this.#state === 'running') {
      this.#retireChannel(entry)
      this.#schedule()
    }
  }

  #eligible(wanted: number): ChunkIndex[] {
    return this.#demand.take(
      wanted,
      (index) =>
        this.#assigned.has(index) ||
        this.#delivery.unavailable(index),
    )
  }

  #expireAssignments(): void {
    const now = this.#now()
    for (const [index, flight] of this.#assigned) {
      if (flight.pending || now - flight.sentAt < this.#requestTimeoutMs) {
        continue
      }
      flight.channel.score = Math.max(flight.channel.score, this.#requestTimeoutMs)
      this.#releaseAssignment(index, flight, true)
      const attempts = this.#attempts.get(index) ?? 0
      if (attempts >= this.#maxBlockAttempts) {
        this.#terminate(new BlockAttemptsExhaustedError(index, attempts, 'timeout'))
        return
      }
    }
  }

  #releaseAssignment(index: ChunkIndex, flight: Flight, retry: boolean): void {
    this.#assigned.delete(index)
    flight.channel.inflight.delete(index)
    flight.channel.partial.delete(index)
    if (retry && this.#delivery.order === 'any') {
      this.#demand.retry(index)
    }
  }

  #refundAttempt(index: ChunkIndex): void {
    const attempts = this.#attempts.get(index) ?? 0
    if (attempts <= 1) {
      this.#attempts.delete(index)
    } else {
      this.#attempts.set(index, attempts - 1)
    }
  }

  #retireChannel(entry: ChannelEntry): void {
    if (entry.retired) {
      return
    }
    entry.retired = true
    entry.sending?.controller.abort(new SessionClosedError('channel retired'))
    entry.sending?.cleanup()
    entry.sending = undefined
    for (const [index, flight] of this.#assigned) {
      if (flight.channel !== entry) {
        continue
      }
      this.#releaseAssignment(index, flight, true)
      this.#refundAttempt(index)
    }
    entry.inflight.clear()
    entry.partial.clear()
    this.#channelSettlement.close(entry.channel)
  }

  #dropChannel(entry: ChannelEntry): void {
    this.#retireChannel(entry)
    this.#channels.delete(entry)
    if (this.#state === 'running') {
      this.#schedule()
    }
  }

  async #onFrame(entry: ChannelEntry, wire: Uint8Array): Promise<void> {
    if (!this.#channels.has(entry) || this.#state !== 'running') {
      return
    }
    let frame
    try {
      frame = decodeFrame(wire)
    } catch {
      this.#dropChannel(entry)
      return
    }
    if (frame.type === 'error') {
      this.#onPeerError(entry, frame)
      return
    }
    if (frame.type === 'request') {
      this.#dropChannel(entry)
      return
    }
    await this.#onBlock(entry, frame)
  }

  #onPeerError(entry: ChannelEntry, frame: ErrorFrame): void {
    if (isFatalErrorCode(frame.code)) {
      this.#terminate(new PeerSessionError(frame))
    } else {
      this.#dropChannel(entry)
    }
  }

  async #onBlock(entry: ChannelEntry, frame: BlockFrame): Promise<void> {
    if (frame.index >= BigInt(MAX_CHUNK_COUNT)) {
      return
    }
    const index = createChunkIndex(Number(frame.index))
    const flight = this.#assigned.get(index)
    if (flight?.channel !== entry) {
      return
    }
    if (flight.pending) {
      flight.pending = false
      this.#attempts.set(index, (this.#attempts.get(index) ?? 0) + 1)
    }

    let reassembler = entry.partial.get(index)
    if (reassembler === undefined) {
      reassembler = new BlockReassembler(this.#maxBlockBytes)
      entry.partial.set(index, reassembler)
    }
    let ciphertext: Uint8Array | undefined
    try {
      ciphertext = reassembler.add(frame)
    } catch (error) {
      if (error instanceof ReassemblyViolation) {
        this.#dropChannel(entry)
        return
      }
      throw error
    }
    if (ciphertext === undefined) {
      return
    }
    entry.partial.delete(index)
    await this.#openAndDeliver(index, flight, ciphertext)
  }

  async #openAndDeliver(
    index: ChunkIndex,
    flight: Flight,
    ciphertext: Uint8Array,
  ): Promise<void> {
    let plaintext: Uint8Array
    try {
      plaintext = await this.#opener.open(index, ciphertext)
    } catch (error) {
      if (this.#assigned.get(index) !== flight || this.#state !== 'running') {
        return
      }
      this.#releaseAssignment(index, flight, true)
      const attempts = this.#attempts.get(index) ?? 0
      if (attempts >= this.#maxBlockAttempts) {
        this.#terminate(
          new BlockAttemptsExhaustedError(
            index,
            attempts,
            error instanceof Error ? error.message : 'authentication failed',
          ),
        )
      } else {
        this.#schedule()
      }
      return
    }
    if (this.#assigned.get(index) !== flight || this.#state !== 'running') {
      return
    }
    this.#releaseAssignment(index, flight, false)
    this.#attempts.delete(index)
    this.#updateScore(flight)
    this.#delivery.accept(index, plaintext)
  }

  #updateScore(flight: Flight): void {
    const duration = Math.max(0, this.#now() - flight.sentAt)
    const current = flight.channel.score
    flight.channel.score =
      current === 0
        ? duration
        : (1 - SCORE_RECENT_WEIGHT) * current + SCORE_RECENT_WEIGHT * duration
  }

  #checkCompletion(): void {
    if (this.#state !== 'running' || !this.#demand.exhausted) {
      return
    }
    const demandPending =
      this.#assigned.size > 0 ||
      this.#demand.retryCount > 0 ||
      this.#delivery.pending ||
      (this.#delivery.order === 'ascending' && this.#demand.orderedCount > 0)
    if (!demandPending) {
      this.#complete().catch((error) => this.#terminate(error))
    }
  }

  async #complete(): Promise<void> {
    if (this.#state !== 'running') {
      return
    }
    this.#state = 'finalizing'
    this.#stopActivity()
    try {
      await this.#delivery.finalize()
      await this.#closeChannels()
      this.#state = 'completed'
      this.#completion.succeed()
    } catch (error) {
      this.#state = 'failed'
      const failure = await this.#abortSink(error)
      await this.#closeChannels()
      this.#completion.fail(failure)
    }
  }

  #terminate(reason: unknown, state: 'failed' | 'closed' = 'failed'): void {
    if (
      this.#state === 'finalizing' ||
      this.#state === 'completed' ||
      this.#state === 'failed' ||
      this.#state === 'closed'
    ) {
      return
    }
    this.#state = state
    this.#stopActivity(reason)
    this.#settleFailure(reason).catch(() => undefined)
  }

  #stopActivity(reason?: unknown): void {
    this.#lifetime.stop(reason)
    for (const entry of this.#channels) {
      entry.sending?.controller.abort(reason)
      entry.sending?.cleanup()
      entry.sending = undefined
    }
  }

  async #settleFailure(reason: unknown): Promise<void> {
    const failure = await this.#abortSink(reason)
    await this.#closeChannels()
    this.#completion.fail(failure)
  }

  async #abortSink(reason: unknown): Promise<unknown> {
    try {
      await this.#delivery.abort(reason)
      return reason
    } catch (cleanupError) {
      // Cleanup truth is part of transfer settlement: callers must not mistake
      // an AbortError for proof that exact-owned partial output was removed.
      return combineSinkCleanupFailure(
        reason,
        cleanupError,
        'Receive session failed and sink cleanup also failed',
      )
    }
  }

  async #closeChannels(): Promise<void> {
    for (const entry of this.#channels) {
      this.#retireChannel(entry)
    }
    await this.#channelSettlement.settle()
    this.#channels.clear()
  }
}
