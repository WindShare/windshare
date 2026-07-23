import type { V2CatalogOperationClient } from '../catalog/v2-client'
import type { V2CatalogPageRequest, V2ShareDescriptor } from '../catalog/v2-records'
import {
  type V2ConnectivityActivation,
  type V2ContentIntent,
  type V2ContentSizeClass,
  V2ReceiverConnectivity,
} from '../connectivity/v2-receiver-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import {
  V2BlockBroker,
  V2BlockLaneAttemptsError,
  V2LaneSet,
  type V2BlockRouteEligibility,
  type V2BlockRouteObservation,
} from '../content/v2-broker'
import {
  V2CatalogSessionOperations,
  V2RevisionService,
  V2SessionBlockLane,
} from '../content/v2-session-services'
import { V2SessionRuntimeError, type V2LaneChange } from '../session/v2-runtime-types'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import {
  V2RelayReceiverError,
  type V2RelayReceiverConnection,
} from '../transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../transport/relay/v2-protocol'
import {
  type V2ContentGeneration,
  type V2ContentGenerationProvider,
  V2SupervisedContent,
} from './v2-supervised-content'
import { V2SupervisedConnectivity } from './v2-supervised-connectivity'
import {
  type V2ProtocolGenerationCore,
  type V2ReceiverSessionFactory,
  V2StaleShareInstanceError,
} from './v2-session-factory'

export const V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS = 100
export const V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS = 5_000

export interface V2ReconnectClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}

export interface V2ReceiverSupervisorOptions {
  readonly descriptor: V2ShareDescriptor
  readonly initial: V2ProtocolGenerationCore
  readonly sessionFactory: V2ReceiverSessionFactory
  readonly clock?: V2ReconnectClock
  readonly backoffMilliseconds?: (attempt: number) => number
  readonly offersFactory?: () => OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly onRecoveryError?: (error: unknown) => void
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
}

interface V2ReceiverGeneration extends V2ContentGeneration {
  readonly broker: V2BlockBroker
  relay: V2RelayReceiverConnection
  relayLaneId: number
  readonly session: V2ReceiverSessionRuntime
  readonly connectivity: V2ReceiverConnectivity
  readonly laneSecret: Uint8Array<ArrayBuffer>
  retired: boolean
  unsubscribe?: () => void
  closeTask?: Promise<void>
}

interface V2GenerationWaiter {
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
  readonly signal?: AbortSignal
  readonly abort?: () => void
}

/**
 * Receiver authority above ProtocolSession. Only this class may publish a new
 * generation, so old lane events, leases, and frames cannot mutate its successor.
 */
export class V2ReceiverReconnectSupervisor implements V2ContentGenerationProvider {
  readonly descriptor: V2ShareDescriptor
  readonly content: V2SupervisedContent
  readonly connectivity: V2SupervisedConnectivity
  readonly catalogOperations: V2CatalogOperationClient
  readonly #factory: V2ReceiverSessionFactory
  readonly #clock: V2ReconnectClock
  readonly #backoffMilliseconds: (attempt: number) => number
  readonly #offersFactory: (() => OfferChannelFactory) | undefined
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #onRecoveryError: (error: unknown) => void
  readonly #onBlockFetched: ((observation: V2BlockRouteObservation) => void) | undefined
  readonly #lifetime = new AbortController()
  readonly #waiters = new Set<V2GenerationWaiter>()
  #current: V2ReceiverGeneration
  #nextGeneration = 1
  #reconcileTask: Promise<void> | undefined
  #reconcileRequested = false
  #terminal: unknown
  #failed = false
  #stopped = false
  #closeTask: Promise<void> | undefined

  constructor(options: V2ReceiverSupervisorOptions) {
    this.descriptor = options.descriptor
    this.#factory = options.sessionFactory
    this.#clock = options.clock ?? systemReconnectClock
    this.#backoffMilliseconds = options.backoffMilliseconds ?? defaultReconnectBackoff
    this.#offersFactory = options.offersFactory
    this.#randomBytes = options.randomBytes
    this.#onRecoveryError = options.onRecoveryError ?? (() => undefined)
    this.#onBlockFetched = options.onBlockFetched
    this.connectivity = new V2SupervisedConnectivity(() => this.#clock.now())
    this.#current = this.#createGeneration(options.initial)
    this.connectivity.bind(this.#current.connectivity)
    this.content = new V2SupervisedContent(this, options.randomBytes)
    this.catalogOperations = Object.freeze({
      fetchPage: (request: V2CatalogPageRequest, signal: AbortSignal) =>
        this.execute(signal, (generation) =>
        new V2CatalogSessionOperations(generation.session).fetchPage(request, signal))
        .then((result) => result.value),
      failProtocol: async (reason: unknown) => this.#failTerminal(reason),
    })
  }

  get generationId(): number {
    return this.#current.id
  }

  get isStopped(): boolean {
    return this.#stopped
  }

  beginConnectivity(
    intent: V2ContentIntent,
    sizeClass: V2ContentSizeClass = 'unknown',
  ): V2ConnectivityActivation {
    return this.connectivity.begin(intent, sizeClass)
  }

  async execute<T>(
    signal: AbortSignal | undefined,
    operation: (generation: V2ReceiverGeneration) => Promise<T>,
  ): Promise<{ readonly generation: V2ReceiverGeneration; readonly value: T }> {
    let sameGenerationRetries = 0
    while (true) {
      const generation = await this.#ready(signal)
      try {
        const value = await operation(generation)
        return Object.freeze({ generation, value })
      } catch (error) {
        signal?.throwIfAborted()
        this.#throwIfTerminal()
        if (!isRecoverableOperationFailure(error, generation, this.#current)) throw error
        if (this.isCurrent(generation) && generation.session.laneIds().length > 0 &&
            sameGenerationRetries < 2) {
          sameGenerationRetries += 1
          await Promise.resolve()
          continue
        }
        sameGenerationRetries = 0
        await this.#waitForGenerationAfter(generation.id, signal)
      }
    }
  }

  async recover(
    generation: V2ContentGeneration,
    error: unknown,
    signal: AbortSignal | undefined,
  ): Promise<boolean> {
    signal?.throwIfAborted()
    this.#throwIfTerminal()
    const managed = generation as V2ReceiverGeneration
    if (!this.isCurrent(generation) || managed.retired) {
      await this.#ready(signal)
      return true
    }
    if (!isLaneRecoveryFailure(error) || managed.lanes.size > 0) return false
    try {
      await managed.lanes.waitForLane(signal)
      return true
    } catch {
      signal?.throwIfAborted()
      this.#throwIfTerminal()
      if (!this.isCurrent(generation) || managed.retired) {
        await this.#ready(signal)
        return true
      }
      return false
    }
  }

  isCurrent(generation: V2ContentGeneration): boolean {
    return this.#current === generation && !this.#current.retired && !this.#stopped
  }

  contentLaneCount(routes: V2BlockRouteEligibility): number {
    if (this.#stopped || this.#current.retired || !routes.active) return 0
    return this.#current.lanes.eligibleSize(routes)
  }

  waitForGenerationAfter(generationId: number, signal?: AbortSignal): Promise<void> {
    return this.#waitForGenerationAfter(generationId, signal)
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  #createGeneration(core: V2ProtocolGenerationCore): V2ReceiverGeneration {
    const lanes = new V2LaneSet(
      this.#onBlockFetched === undefined ? {} : { onBlockFetched: this.#onBlockFetched },
    )
    const brokerOwner: { current?: V2BlockBroker } = {}
    const readSecret = this.#factory.copyReadSecret()
    let revisions: V2RevisionService | undefined
    let broker: V2BlockBroker | undefined
    let connectivity: V2ReceiverConnectivity | undefined
    let laneSecret: Uint8Array<ArrayBuffer> | undefined
    try {
      revisions = new V2RevisionService(
        core.session,
        this.descriptor,
        readSecret,
        lanes,
        {
          beforeLeaseRelease: (leaseId) => brokerOwner.current?.waitForLeaseIdle(leaseId) ??
            Promise.reject(new Error('Generation block broker is unavailable during lease release')),
        },
      )
      const revisionService = revisions
      broker = new V2BlockBroker(lanes, {
        validateDemand: (demand) => revisionService.leaseError(demand.leaseId),
      })
      brokerOwner.current = broker
      const contentSecret = readSecret.slice()
      laneSecret = contentSecret
      connectivity = new V2ReceiverConnectivity({
        session: core.session,
        lanes,
        relayLaneId: core.relayLaneId,
        createBlockLane: (laneId) => new V2SessionBlockLane(
          laneId,
          core.session,
          this.descriptor,
          contentSecret,
          revisionService,
        ),
        ...(this.#offersFactory === undefined ? {} : { offers: this.#offersFactory() }),
        ...(this.#randomBytes === undefined ? {} : { randomBytes: this.#randomBytes }),
        onPeerError: this.#onRecoveryError,
      })
      const generation: V2ReceiverGeneration = {
        id: this.#nextGeneration++,
        relay: core.relay,
        relayLaneId: core.relayLaneId,
        session: core.session,
        lanes,
        revisions,
        broker,
        connectivity,
        laneSecret,
        retired: false,
      }
      generation.unsubscribe = core.session.subscribeLaneChanges((change) =>
        this.#laneChanged(generation, change))
      return generation
    } catch (error) {
      connectivity?.close().catch(() => undefined)
      broker?.close()
      revisions?.close()
      lanes.close()
      laneSecret?.fill(0)
      throw error
    } finally {
      readSecret.fill(0)
    }
  }

  #laneChanged(generation: V2ReceiverGeneration, change: V2LaneChange): void {
    if (!this.isCurrent(generation) || change.type !== 'detached') return
    if (generation.session.isClosed || isSessionFailure(change.failure)) {
      this.#failTerminal(change.failure ?? new Error('ProtocolSession closed terminally'))
      return
    }
    if (generation.session.laneIds().length === 0) {
      generation.retired = true
      generation.session.close().catch(() => undefined)
    }
    this.#requestReconcile()
    this.#wakeWaiters()
  }

  #requestReconcile(): void {
    if (this.#stopped || this.#failed) return
    this.#reconcileRequested = true
    if (this.#reconcileTask !== undefined) return
    const task = this.#reconcile()
      .catch((error: unknown) => this.#failTerminal(error))
      .finally(() => {
        if (this.#reconcileTask === task) this.#reconcileTask = undefined
        if (this.#reconcileRequested && !this.#stopped && !this.#failed) {
          this.#requestReconcile()
        }
      })
    this.#reconcileTask = task
  }

  async #reconcile(): Promise<void> {
    let attempt = 0
    while (!this.#stopped && !this.#failed) {
      this.#reconcileRequested = false
      try {
        if (!(await this.#reconcileGeneration(this.#current))) return
        attempt = 0
      } catch (error) {
        if (!(await this.#waitAfterRecoveryFailure(error, attempt++))) return
      }
    }
  }

  async #reconcileGeneration(generation: V2ReceiverGeneration): Promise<boolean> {
    if (generation.retired || generation.session.laneIds().length === 0) {
      await this.#replaceGeneration(generation)
      return true
    }
    if (!generation.session.laneIds().includes(generation.relayLaneId)) {
      await this.#replaceRelay(generation)
      return true
    }
    return false
  }

  async #waitAfterRecoveryFailure(error: unknown, attempt: number): Promise<boolean> {
    if (this.#stopped || this.#lifetime.signal.aborted) return false
    if (isTerminalRecoveryFailure(error)) {
      this.#failTerminal(error)
      return false
    }
    this.#onRecoveryError(error)
    const delay = requireBackoff(this.#backoffMilliseconds(attempt))
    try {
      await this.#clock.sleep(delay, this.#lifetime.signal)
      return true
    } catch {
      return false
    }
  }

  async #replaceRelay(generation: V2ReceiverGeneration): Promise<void> {
    const attached = await this.#factory.attachRelay(generation.session, this.#lifetime.signal)
    if (!this.isCurrent(generation) || generation.retired) {
      await attached.relay.close().catch(() => undefined)
      return
    }
    try {
      generation.connectivity.replaceRelayLane(attached.laneId)
    } catch (error) {
      await attached.relay.close().catch(() => undefined)
      throw error
    }
    const previous = generation.relay
    generation.relay = attached.relay
    generation.relayLaneId = attached.laneId
    await previous.close().catch(() => undefined)
    this.#wakeWaiters()
  }

  async #replaceGeneration(previous: V2ReceiverGeneration): Promise<void> {
    const core = await this.#factory.connectFresh(this.#lifetime.signal)
    if (this.#stopped || this.#current !== previous) {
      await closeCore(core)
      return
    }
    let next: V2ReceiverGeneration
    try {
      next = this.#createGeneration(core)
    } catch (error) {
      await closeCore(core)
      throw error
    }
    this.#current = next
    this.connectivity.bind(next.connectivity)
    this.#wakeWaiters()
    await this.#closeGeneration(previous)
  }

  async #ready(signal?: AbortSignal): Promise<V2ReceiverGeneration> {
    while (true) {
      signal?.throwIfAborted()
      this.#throwIfTerminal()
      const current = this.#current
      if (!current.retired && current.session.laneIds().length > 0) return current
      await this.#waitForWake(signal)
    }
  }

  async #waitForGenerationAfter(generationId: number, signal?: AbortSignal): Promise<void> {
    while (this.#current.id <= generationId || this.#current.retired) {
      signal?.throwIfAborted()
      this.#throwIfTerminal()
      await this.#waitForWake(signal)
    }
  }

  #waitForWake(signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    return new Promise<void>((resolve, reject) => {
      const abort = () => {
        this.#waiters.delete(waiter)
        reject(signal?.reason ?? new DOMException('Generation wait aborted', 'AbortError'))
      }
      const waiter: V2GenerationWaiter = {
        resolve: () => {
          signal?.removeEventListener('abort', abort)
          resolve()
        },
        reject: (reason) => {
          signal?.removeEventListener('abort', abort)
          reject(reason)
        },
        ...(signal === undefined ? {} : { signal, abort }),
      }
      this.#waiters.add(waiter)
      signal?.addEventListener('abort', abort, { once: true })
      if (signal?.aborted) abort()
    })
  }

  #wakeWaiters(): void {
    for (const waiter of this.#waiters) waiter.resolve()
    this.#waiters.clear()
  }

  #failTerminal(reason: unknown): void {
    if (this.#failed || this.#stopped) return
    this.#failed = true
    this.#terminal = reason
    this.#lifetime.abort(reason)
    for (const waiter of this.#waiters) waiter.reject(reason)
    this.#waiters.clear()
    this.#current.retired = true
    this.content.close()
    this.connectivity.close().catch(() => undefined)
    this.#factory.close()
    this.#closeGeneration(this.#current).catch(() => undefined)
  }

  #throwIfTerminal(): void {
    if (this.#stopped) throw new DOMException('Receiver supervisor stopped', 'AbortError')
    if (this.#failed) throw this.#terminal
  }

  async #close(): Promise<void> {
    if (this.#stopped) return
    // Stop authority is published before any close can emit lane events.
    this.#stopped = true
    this.#lifetime.abort(new DOMException('Receiver supervisor stopped', 'AbortError'))
    for (const waiter of this.#waiters) waiter.reject(this.#lifetime.signal.reason)
    this.#waiters.clear()
    this.#current.retired = true
    this.content.close()
    await Promise.allSettled([
      this.connectivity.close(),
      this.#closeGeneration(this.#current),
      ...(this.#reconcileTask === undefined ? [] : [this.#reconcileTask]),
    ])
    this.#factory.close()
  }

  #closeGeneration(generation: V2ReceiverGeneration): Promise<void> {
    generation.closeTask ??= closeGeneration(generation)
    return generation.closeTask
  }
}

function isTerminalRecoveryFailure(error: unknown): boolean {
  return error instanceof V2StaleShareInstanceError ||
    (error instanceof V2RelayReceiverError && error.relayError?.code === V2_RELAY_ERROR.stopped)
}

async function closeGeneration(generation: V2ReceiverGeneration): Promise<void> {
  generation.retired = true
  generation.unsubscribe?.()
  delete generation.unsubscribe
  generation.laneSecret.fill(0)
  generation.broker.close()
  generation.revisions.close()
  generation.lanes.close()
  await Promise.allSettled([
    generation.connectivity.close(),
    generation.session.close(),
    generation.relay.close(),
  ])
}

async function closeCore(core: V2ProtocolGenerationCore): Promise<void> {
  await Promise.allSettled([core.session.close(), core.relay.close()])
}

function isRecoverableOperationFailure(
  error: unknown,
  generation: V2ReceiverGeneration,
  current: V2ReceiverGeneration,
): boolean {
  return generation !== current || generation.retired || isLaneRecoveryFailure(error)
}

function isLaneRecoveryFailure(error: unknown): boolean {
  return error instanceof V2BlockLaneAttemptsError ||
    (error instanceof V2SessionRuntimeError && error.scope === 'lane')
}

function isSessionFailure(error: unknown): boolean {
  return error instanceof V2SessionRuntimeError && error.scope === 'session'
}

function requireBackoff(milliseconds: number): number {
  if (!Number.isFinite(milliseconds) || milliseconds < 0 ||
      milliseconds > V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS) {
    throw new RangeError('Receiver reconnect backoff is outside its bounded range')
  }
  return milliseconds
}

function defaultReconnectBackoff(attempt: number): number {
  return Math.min(
    V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS * 2 ** Math.min(attempt, 8),
    V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS,
  )
}

const systemReconnectClock: V2ReconnectClock = Object.freeze({
  now: () => performance.now(),
  sleep: (milliseconds: number, signal: AbortSignal) => abortableDelay(milliseconds, signal),
})

function abortableDelay(milliseconds: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, milliseconds)
    const abort = () => {
      globalThis.clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Reconnect delay aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}
