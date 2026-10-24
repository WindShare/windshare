import { byteRange, type ByteRange } from '../../content/geometry'
import {
  type BlockRangeReader,
  type BlockRangeReaderOptions,
  type BlockSlice,
} from '../../content/broker'
import {
  RevisionCapacityBusyError,
  type OpenedRevision,
  type RevisionReader,
} from '../../content/session-services'
import { encodeBase64Url } from '../../crypto/bytes'
import {
  protocolFailureFact,
  type FailureFact,
  type ReceivedProtocolError,
} from '../../diagnostics/incident'
import {
  createProtocolSessionIdentity,
  type ProtocolSessionIdentity,
} from '../../session/identities'

export const REVISION_CAPACITY_WAIT_BUDGET_MILLISECONDS = 2 * 60 * 1_000
export const REVISION_CAPACITY_MAXIMUM_WAIT_BUDGET_MILLISECONDS = 10 * 60 * 1_000
export const REVISION_CAPACITY_ADDITIVE_JITTER_LIMIT_MILLISECONDS = 250
export const REVISION_CAPACITY_MAXIMUM_ADDITIVE_JITTER_LIMIT_MILLISECONDS = 1_000
export const REVISION_CAPACITY_VISIBILITY_THRESHOLD_MILLISECONDS = 500
export const REVISION_CAPACITY_MAXIMUM_VISIBILITY_THRESHOLD_MILLISECONDS = 5_000
export const REVISION_CAPACITY_WAIT_ID_BYTES = 16

export type RevisionCapacitySurface = 'revision_open' | 'block_range'

export interface RevisionCapacityClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}

export interface ProtocolSessionReplacementWaiter {
  waitForProtocolSessionReplacement(
    issuingIdentity: ProtocolSessionIdentity,
    signal: AbortSignal,
  ): Promise<void>
}

export interface RevisionCapacityWaitSnapshot {
  readonly activeWaiters: number
  readonly accumulatedWaitMilliseconds: number
  readonly attempts: number
  readonly visible: boolean
}

export type RevisionCapacityTrace = Readonly<{
  transition:
    | 'capacity_retry_scheduled'
    | 'capacity_retry_succeeded'
    | 'capacity_wait_budget_paused'
    | 'capacity_wait_cancelled'
    | 'capacity_generation_replaced'
  waitId: string
  surface: RevisionCapacitySurface
  protocolSessionId: string
  protocolOperationId: string
  attempt: number
  senderHintMilliseconds: number
  jitterMilliseconds: number
  delayMilliseconds: number
  accumulatedWaitMilliseconds: number
  activeWaiters: number
}>

export interface RevisionCapacityPolicyOptions {
  readonly generation: ProtocolSessionReplacementWaiter
  readonly waitBudgetMilliseconds?: number
  readonly additiveJitterLimitMilliseconds?: number
  readonly visibilityThresholdMilliseconds?: number
  readonly clock?: RevisionCapacityClock
  readonly random?: () => number
  readonly randomBytes?: (length: number) => Uint8Array
}

export interface RevisionCapacityCoordinatorOptions extends RevisionCapacityPolicyOptions {
  readonly onProgress?: (snapshot: RevisionCapacityWaitSnapshot) => void
  readonly onTrace?: (event: RevisionCapacityTrace) => void
}

export class RevisionCapacityWaitBudgetError extends Error {
  readonly surface: RevisionCapacitySurface
  readonly protocolFailure: ReceivedProtocolError
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(surface: RevisionCapacitySurface, capacity: RevisionCapacityBusyError) {
    super('Sender revision capacity stayed busy for the receive wait budget', { cause: capacity })
    this.name = 'RevisionCapacityWaitBudgetError'
    this.surface = surface
    this.protocolFailure = capacity.protocolFailure
    this.failureFact = protocolFailureFact({
      stage: 'protocol_operation',
      recoveryDisposition: 'resumable_receive',
      protocolFailure: capacity.protocolFailure,
    })
  }
}

/**
 * One coordinator accounts elapsed union time, so four blocked workers consume
 * the receive budget at wall-clock speed instead of charging it four times.
 */
export class RevisionCapacityCoordinator {
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly #sourceRevisions: RevisionReader
  readonly #sourceBroker: BlockRangeReader
  readonly #generation: ProtocolSessionReplacementWaiter
  readonly #clock: RevisionCapacityClock
  readonly #random: () => number
  readonly #randomBytes: (length: number) => Uint8Array
  readonly #waitBudgetMilliseconds: number
  readonly #jitterLimitMilliseconds: number
  readonly #visibilityThresholdMilliseconds: number
  readonly #onProgress: ((snapshot: RevisionCapacityWaitSnapshot) => void) | undefined
  readonly #onTrace: ((event: RevisionCapacityTrace) => void) | undefined
  #activeWaiters = 0
  #activeSince: number | undefined
  #accumulatedWaitMilliseconds = 0
  #attempts = 0
  #visibilityTimer: AbortController | undefined

  constructor(
    source: Readonly<{ revisions: RevisionReader; broker: BlockRangeReader }>,
    options: RevisionCapacityCoordinatorOptions,
  ) {
    this.#sourceRevisions = source.revisions
    this.#sourceBroker = source.broker
    this.#generation = options.generation
    this.#clock = options.clock ?? SYSTEM_REVISION_CAPACITY_CLOCK
    this.#random = options.random ?? Math.random
    this.#randomBytes = options.randomBytes ?? systemRandomBytes
    this.#waitBudgetMilliseconds = boundedInteger(
      options.waitBudgetMilliseconds ?? REVISION_CAPACITY_WAIT_BUDGET_MILLISECONDS,
      1,
      REVISION_CAPACITY_MAXIMUM_WAIT_BUDGET_MILLISECONDS,
      'revision capacity wait budget',
    )
    this.#jitterLimitMilliseconds = boundedInteger(
      options.additiveJitterLimitMilliseconds ??
        REVISION_CAPACITY_ADDITIVE_JITTER_LIMIT_MILLISECONDS,
      0,
      REVISION_CAPACITY_MAXIMUM_ADDITIVE_JITTER_LIMIT_MILLISECONDS,
      'revision capacity additive jitter limit',
    )
    this.#visibilityThresholdMilliseconds = boundedInteger(
      options.visibilityThresholdMilliseconds ??
        REVISION_CAPACITY_VISIBILITY_THRESHOLD_MILLISECONDS,
      0,
      REVISION_CAPACITY_MAXIMUM_VISIBILITY_THRESHOLD_MILLISECONDS,
      'revision capacity visibility threshold',
    )
    this.#onProgress = options.onProgress
    this.#onTrace = options.onTrace
    this.revisions = Object.freeze({
      open: (fileId: Uint8Array, signal?: AbortSignal) => this.#open(fileId, signal),
    })
    this.broker = Object.freeze({
      readRange: (
        descriptor: Parameters<BlockRangeReader['readRange']>[0],
        leaseId: Parameters<BlockRangeReader['readRange']>[1],
        range: Parameters<BlockRangeReader['readRange']>[2],
        readOptions?: Parameters<BlockRangeReader['readRange']>[3],
      ) => this.#readRange(descriptor, leaseId, range, readOptions),
    })
  }

  snapshot(): RevisionCapacityWaitSnapshot {
    const now = this.#clock.now()
    const activeElapsed = this.#activeSince === undefined
      ? 0
      : nonnegativeElapsed(this.#activeSince, now)
    return Object.freeze({
      activeWaiters: this.#activeWaiters,
      // Trace payloads use integer milliseconds; ceiling also prevents a high-resolution
      // clock from understating the finite receive budget.
      accumulatedWaitMilliseconds: Math.ceil(boundedSum(
        this.#accumulatedWaitMilliseconds,
        activeElapsed,
      )),
      attempts: this.#attempts,
      visible: this.#activeWaiters > 0 &&
        activeElapsed >= this.#visibilityThresholdMilliseconds,
    })
  }

  async #open(fileId: Uint8Array, signal?: AbortSignal): Promise<OpenedRevision> {
    let operation: RevisionCapacityWaitOperation | undefined
    try {
      while (true) {
        signal?.throwIfAborted()
        try {
          const opened = await this.#sourceRevisions.open(fileId, signal)
          operation?.succeed()
          return opened
        } catch (error) {
          if (!(error instanceof RevisionCapacityBusyError)) throw error
          operation ??= this.#operation('revision_open')
          await operation.wait(error, signal)
        }
      }
    } finally {
      operation?.stop()
    }
  }

  async *#readRange(
    descriptor: Parameters<BlockRangeReader['readRange']>[0],
    leaseId: Parameters<BlockRangeReader['readRange']>[1],
    range: ByteRange,
    options?: BlockRangeReaderOptions,
  ): AsyncGenerator<BlockSlice> {
    let nextOffset = range.start
    let operation: RevisionCapacityWaitOperation | undefined
    try {
      while (nextOffset < range.end) {
        options?.signal?.throwIfAborted()
        try {
          for await (const slice of this.#sourceBroker.readRange(
            descriptor,
            leaseId,
            byteRange(nextOffset, range.end),
            options,
          )) {
            nextOffset = slice.offset + BigInt(slice.data.byteLength)
            if (operation !== undefined) {
              operation.succeed()
              operation = undefined
            }
            yield slice
          }
          operation?.succeed()
          operation = undefined
          return
        } catch (error) {
          if (!(error instanceof RevisionCapacityBusyError)) throw error
          operation ??= this.#operation('block_range')
          await operation.wait(error, options?.signal)
        }
      }
      operation?.succeed()
    } finally {
      operation?.stop()
    }
  }

  #operation(surface: RevisionCapacitySurface): RevisionCapacityWaitOperation {
    return new RevisionCapacityWaitOperation(this, surface, this.#newWaitId())
  }

  #newWaitId(): string {
    const bytes = this.#randomBytes(REVISION_CAPACITY_WAIT_ID_BYTES)
    if (!(bytes instanceof Uint8Array) ||
        bytes.byteLength !== REVISION_CAPACITY_WAIT_ID_BYTES ||
        bytes.every(value => value === 0)) {
      throw new TypeError('revision capacity wait ID source returned an invalid identity')
    }
    return encodeBase64Url(bytes)
  }

  activate(): RevisionCapacityWaitSnapshot {
    if (this.#activeWaiters === 0) {
      this.#activeSince = this.#clock.now()
      this.#armVisibilityTimer()
    }
    this.#activeWaiters += 1
    return this.#publishProgress()
  }

  finish(): RevisionCapacityWaitSnapshot {
    if (this.#activeWaiters === 0) return this.snapshot()
    this.#activeWaiters -= 1
    if (this.#activeWaiters === 0) {
      const now = this.#clock.now()
      if (this.#activeSince !== undefined) {
        this.#accumulatedWaitMilliseconds = boundedSum(
          this.#accumulatedWaitMilliseconds,
          nonnegativeElapsed(this.#activeSince, now),
        )
      }
      this.#activeSince = undefined
      this.#visibilityTimer?.abort()
      this.#visibilityTimer = undefined
    }
    return this.#publishProgress()
  }

  planWait(): Readonly<{
    snapshot: RevisionCapacityWaitSnapshot
    remainingMilliseconds: number
    jitterMilliseconds: number
  }> {
    this.#attempts = boundedSum(this.#attempts, 1)
    const snapshot = this.#publishProgress()
    const sample = this.#random()
    if (!Number.isFinite(sample) || sample < 0 || sample >= 1) {
      throw new RangeError('revision capacity jitter source must return a unit-interval sample')
    }
    const jitterMilliseconds = Math.floor(
      sample * (this.#jitterLimitMilliseconds + 1),
    )
    return Object.freeze({
      snapshot,
      remainingMilliseconds: Math.max(
        0,
        this.#waitBudgetMilliseconds - snapshot.accumulatedWaitMilliseconds,
      ),
      jitterMilliseconds,
    })
  }

  budgetExhausted(snapshot: RevisionCapacityWaitSnapshot): boolean {
    return snapshot.accumulatedWaitMilliseconds >= this.#waitBudgetMilliseconds
  }

  async raceDelay(
    delayMilliseconds: number,
    issuingIdentity: ProtocolSessionIdentity,
    signal: AbortSignal,
  ): Promise<'timer' | 'generation'> {
    signal.throwIfAborted()
    const timer = new AbortController()
    const generation = new AbortController()
    const abort = () => {
      timer.abort(signal.reason)
      generation.abort(signal.reason)
    }
    signal.addEventListener('abort', abort, { once: true })
    const timerTask = settleRace(
      'timer',
      this.#clock.sleep(delayMilliseconds, timer.signal),
    )
    const generationTask = settleRace(
      'generation',
      this.#generation.waitForProtocolSessionReplacement(issuingIdentity, generation.signal),
    )
    try {
      const outcome = await Promise.race([timerTask, generationTask])
      if (outcome.kind === 'timer') generation.abort(new DOMException('Delay completed', 'AbortError'))
      else timer.abort(new DOMException('ProtocolSession replaced', 'AbortError'))
      await Promise.allSettled([timerTask, generationTask])
      if (signal.aborted) throw signal.reason
      if (outcome.error !== undefined) throw outcome.error
      return outcome.kind
    } finally {
      signal.removeEventListener('abort', abort)
      timer.abort()
      generation.abort()
    }
  }

  trace(event: RevisionCapacityTrace): void {
    try {
      this.#onTrace?.(event)
    } catch {
      // Diagnostics are passive and cannot alter transfer scheduling authority.
    }
  }

  #publishProgress(): RevisionCapacityWaitSnapshot {
    const snapshot = this.snapshot()
    try {
      this.#onProgress?.(snapshot)
    } catch {
      // Product observation cannot alter the shared receive budget.
    }
    return snapshot
  }

  #armVisibilityTimer(): void {
    this.#visibilityTimer?.abort()
    if (this.#visibilityThresholdMilliseconds === 0) return
    const timer = new AbortController()
    this.#visibilityTimer = timer
    this.#clock.sleep(this.#visibilityThresholdMilliseconds, timer.signal).then(() => {
      if (this.#visibilityTimer !== timer || this.#activeWaiters === 0) return
      this.#publishProgress()
    }).catch(() => undefined)
  }
}

class RevisionCapacityWaitOperation {
  readonly #coordinator: RevisionCapacityCoordinator
  readonly #surface: RevisionCapacitySurface
  readonly #waitId: string
  #active = false
  #closed = false
  #lastCapacity: RevisionCapacityBusyError | undefined
  #lastAttempt = 0

  constructor(
    coordinator: RevisionCapacityCoordinator,
    surface: RevisionCapacitySurface,
    waitId: string,
  ) {
    this.#coordinator = coordinator
    this.#surface = surface
    this.#waitId = waitId
  }

  async wait(capacity: RevisionCapacityBusyError, signal?: AbortSignal): Promise<void> {
    if (this.#closed) throw new Error('revision capacity wait operation is already closed')
    const effectiveSignal = signal ?? NEVER_ABORTED_SIGNAL
    effectiveSignal.throwIfAborted()
    this.#lastCapacity = capacity
    if (!this.#active) {
      this.#active = true
      this.#coordinator.activate()
    }
    const plan = this.#coordinator.planWait()
    this.#lastAttempt = boundedSum(this.#lastAttempt, 1)
    if (plan.remainingMilliseconds <= 0) {
      this.#pause(capacity)
    }
    const desiredDelay = boundedSum(
      capacity.retryAfterMilliseconds,
      plan.jitterMilliseconds,
    )
    const delayMilliseconds = Math.min(desiredDelay, plan.remainingMilliseconds)
    const budgetBound = delayMilliseconds >= plan.remainingMilliseconds
    this.#trace('capacity_retry_scheduled', capacity, {
      ...plan.snapshot,
      jitterMilliseconds: plan.jitterMilliseconds,
      delayMilliseconds,
    })
    let outcome: 'timer' | 'generation'
    try {
      outcome = await this.#coordinator.raceDelay(
        delayMilliseconds,
        createProtocolSessionIdentity(
          capacity.protocolFailure.correlation.protocolSessionId.copyBytes(),
        ),
        effectiveSignal,
      )
    } catch (error) {
      const snapshot = this.#finish()
      this.#trace('capacity_wait_cancelled', capacity, snapshot)
      throw error
    }
    if (outcome === 'generation') {
      this.#trace('capacity_generation_replaced', capacity, this.#deactivate())
      return
    }
    const snapshot = this.#coordinator.snapshot()
    if (budgetBound || this.#coordinator.budgetExhausted(snapshot)) {
      this.#pause(capacity)
    }
  }

  succeed(): void {
    if (this.#closed || this.#lastCapacity === undefined) return
    const capacity = this.#lastCapacity
    const snapshot = this.#finish()
    this.#trace('capacity_retry_succeeded', capacity, snapshot)
  }

  stop(): void {
    if (this.#closed) return
    this.#finish()
  }

  #pause(capacity: RevisionCapacityBusyError): never {
    const snapshot = this.#finish()
    this.#trace('capacity_wait_budget_paused', capacity, snapshot)
    throw new RevisionCapacityWaitBudgetError(this.#surface, capacity)
  }

  #finish(): RevisionCapacityWaitSnapshot {
    if (this.#closed) return this.#coordinator.snapshot()
    this.#closed = true
    return this.#deactivate()
  }

  #deactivate(): RevisionCapacityWaitSnapshot {
    if (!this.#active) return this.#coordinator.snapshot()
    this.#active = false
    return this.#coordinator.finish()
  }

  #trace(
    transition: RevisionCapacityTrace['transition'],
    capacity: RevisionCapacityBusyError,
    snapshot: RevisionCapacityWaitSnapshot & Readonly<{
      jitterMilliseconds?: number
      delayMilliseconds?: number
    }>,
  ): void {
    this.#coordinator.trace(Object.freeze({
      transition,
      waitId: this.#waitId,
      surface: this.#surface,
      protocolSessionId: encodeBase64Url(
        capacity.protocolFailure.correlation.protocolSessionId.copyBytes(),
      ),
      protocolOperationId: encodeBase64Url(
        capacity.protocolFailure.correlation.protocolOperationId.copyBytes(),
      ),
      attempt: this.#lastAttempt,
      senderHintMilliseconds: capacity.retryAfterMilliseconds,
      jitterMilliseconds: snapshot.jitterMilliseconds ?? 0,
      delayMilliseconds: snapshot.delayMilliseconds ?? 0,
      accumulatedWaitMilliseconds: snapshot.accumulatedWaitMilliseconds,
      activeWaiters: snapshot.activeWaiters,
    }))
  }
}

function settleRace<Kind extends 'timer' | 'generation'>(
  kind: Kind,
  promise: Promise<void>,
): Promise<Readonly<{ kind: Kind; error?: unknown }>> {
  return promise.then(
    () => Object.freeze({ kind }),
    error => Object.freeze({ kind, error }),
  )
}

function boundedInteger(value: number, minimum: number, maximum: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new RangeError(`${label} is outside its bounded range`)
  }
  return value
}

function boundedSum(left: number, right: number): number {
  return Math.min(Number.MAX_SAFE_INTEGER, left + right)
}

function nonnegativeElapsed(start: number, end: number): number {
  if (!Number.isFinite(start) || !Number.isFinite(end)) {
    throw new RangeError('revision capacity clock returned a non-finite time')
  }
  return Math.max(0, end - start)
}

function systemRandomBytes(length: number): Uint8Array {
  return globalThis.crypto.getRandomValues(new Uint8Array(length))
}

const SYSTEM_REVISION_CAPACITY_CLOCK: RevisionCapacityClock = Object.freeze({
  now: () => performance.now(),
  sleep: (milliseconds: number, signal: AbortSignal) => new Promise<void>((resolve, reject) => {
    signal.throwIfAborted()
    const timeout = setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, milliseconds)
    const abort = () => {
      clearTimeout(timeout)
      reject(signal.reason ?? new DOMException('Capacity wait aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  }),
})

const NEVER_ABORTED_SIGNAL = new AbortController().signal
