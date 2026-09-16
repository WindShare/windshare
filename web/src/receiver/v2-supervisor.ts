import { isTerminalRecoveryFailure, isShareRecoveryFailure, isLaneRecoveryFailure, isSessionFailure, recoveryRetryAfter } from './recovery-failure'
import {
  defaultReconnectBackoff, generationReconnectBackoff, reconnectPhase, requireBackoff,
  systemReconnectClock, waitingReconnectBackoff, type V2ReconnectClock,
} from './recovery-clock'
import { RecoveryWake } from './recovery-wake'
import { observeRecovery, type RecoveryObservation } from './recovery-observation'
import { ReceiverConnectionState, type ReceiverReconnectActivity } from './connection-state'
import type { V2CatalogOperationClient } from '../catalog/v2-client'
import type { V2CatalogPageRequest, V2ShareDescriptor } from '../catalog/v2-records'
import {
  type V2ConnectivityActivation,
  type V2ContentLaneAdmissionObservation,
  type V2ContentLaneDetachmentObservation,
  type V2ContentIntent,
  type V2ConnectivityPolicy,
} from '../connectivity/v2-receiver-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type { V2ConnectivityTraceSource } from '../connectivity/diagnostics'
import type { V2PeerRecoveryDependencies } from '../connectivity/peer-set/path'
import { PeerAttemptBudget } from '../connectivity/peer-set/budget'
import { PeerNetworkGeneration } from '../connectivity/peer-set/network-generation'
import {
  type V2BlockRouteEligibility,
  type V2BlockDispatchObservation,
  type V2BlockRouteObservation,
} from '../content/v2-broker'
import { V2CatalogSessionOperations } from '../content/v2-session-services'
import { type V2LaneChange } from '../session/v2-runtime-types'
import {
  equalV2DiagnosticIdentities,
  type V2ProtocolSessionIdentity,
} from '../session/v2-identities'
import { encodeBase64Url } from '../crypto/bytes'
import {
  type V2ContentGeneration,
  type V2ContentGenerationProvider,
  V2SupervisedContent,
} from './v2-supervised-content'
import { V2SupervisedConnectivity } from './v2-supervised-connectivity'
import { V2ReceiverGenerationFactory, type V2ReceiverGeneration } from './v2-receiver-generation'
import {
  type V2ProtocolGenerationCore,
  type V2ReceiverSessionFactory,
} from './v2-session-factory'

import { OperationRecovery, type OperationRecoveryDecision } from './operation-recovery'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import { ReceiverPathActivity } from './path-activity'
import { DownloadMetrics } from './download-metrics'
import {
  GenerationRecoveryBudget,
  GenerationRecoveryExhaustedError,
  runGenerationRecovery,
} from './generation-recovery'

export interface V2ReceiverSupervisorOptions {
  readonly policy?: V2ConnectivityPolicy
  readonly descriptor: V2ShareDescriptor
  readonly initial: V2ProtocolGenerationCore
  readonly sessionFactory: V2ReceiverSessionFactory
  readonly clock?: V2ReconnectClock
  readonly generationRecovery?: GenerationRecoveryBudget
  readonly generationBackoffMilliseconds?: (attempt: number) => number
  readonly offersFactory?: () => OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly nativePeerUsable?: () => boolean
  readonly protocolTrace?: V2ProtocolTraceSource
  readonly connectivityTrace?: V2ConnectivityTraceSource
  readonly peerRecovery?: V2PeerRecoveryDependencies
  readonly onRecoveryError?: (error: unknown) => void
  readonly onBlockDispatched?: (observation: V2BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
  readonly onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
}

interface V2GenerationWaiter {
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
  readonly signal?: AbortSignal
  readonly abort?: () => void
}

export interface V2ProtocolGenerationObservation {
  readonly generationId: number
  readonly protocolSessionId: string
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
}

export type V2ProtocolGenerationListener = (
  observation: V2ProtocolGenerationObservation,
) => void

/**
 * Receiver authority above ProtocolSession. Only this class may publish a new
 * generation, so old lane events, leases, and frames cannot mutate its successor.
 */
export class V2ReceiverReconnectSupervisor implements V2ContentGenerationProvider {
  readonly descriptor: V2ShareDescriptor
  readonly pathActivity = new ReceiverPathActivity()
  readonly connection = new ReceiverConnectionState()
  readonly #downloads = new Map<V2BlockRouteEligibility, DownloadMetrics>()
  #directUsable = false
  readonly content: V2SupervisedContent
  readonly connectivity: V2SupervisedConnectivity
  readonly catalogOperations: V2CatalogOperationClient
  readonly #factory: V2ReceiverSessionFactory
  readonly #policy: V2ConnectivityPolicy
  readonly #clock: V2ReconnectClock
  readonly #generationRecovery: GenerationRecoveryBudget
  readonly #recoveryWake = new RecoveryWake()
  readonly #networkAvailable = () => this.requestReconnect()
  #recoveryAttempt = 0
  #recoveryPhase: 'fast' | 'waiting' = 'fast'
  readonly #generationBackoffMilliseconds: (attempt: number) => number
  readonly #protocolTrace: V2ProtocolTraceSource | undefined
  #operationSequence = 0
  readonly #onRecoveryError: (error: unknown) => void
  readonly #lifetime = new AbortController()
  readonly #waiters = new Set<V2GenerationWaiter>()
  readonly #generationListeners = new Set<V2ProtocolGenerationListener>()
  readonly #generations: V2ReceiverGenerationFactory
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
    this.#policy = options.policy ?? 'auto'
    this.#factory = options.sessionFactory
    this.#clock = options.clock ?? systemReconnectClock
    this.#generationRecovery = options.generationRecovery ?? new GenerationRecoveryBudget()
    this.#generationBackoffMilliseconds = options.generationBackoffMilliseconds ?? generationReconnectBackoff
    this.#protocolTrace = options.protocolTrace
    this.#onRecoveryError = options.onRecoveryError ?? (() => undefined)
    this.#generations = new V2ReceiverGenerationFactory({
      descriptor: this.descriptor,
      factory: this.#factory,
      policy: this.#policy,
      clock: this.#clock,
      recoveryWake: this.#recoveryWake,
      backoffMilliseconds: defaultReconnectBackoff,
      pathActivity: this.pathActivity,
      offersFactory: options.offersFactory,
      randomBytes: options.randomBytes,
      nativePeerUsable: options.nativePeerUsable,
      protocolTrace: this.#protocolTrace,
      connectivityTrace: options.connectivityTrace,
      peerRecovery: {
        ...options.peerRecovery,
        network: options.peerRecovery?.network ?? new PeerNetworkGeneration(),
        budget: options.peerRecovery?.budget ?? new PeerAttemptBudget(),
      },
      onBlockDispatched: options.onBlockDispatched,
      onBlockFetched: options.onBlockFetched,
      onContentLaneAdmitted: options.onContentLaneAdmitted,
      onContentLaneDetached: options.onContentLaneDetached,
      onLaneChanged: (generation, change) => this.#laneChanged(generation, change),
      onRelayFailure: (error) => {
        if (isShareRecoveryFailure(error)) {
          this.#failTerminal(error)
          return 'stop'
        }
        this.#observeRecoveryError(error)
        return isTerminalRecoveryFailure(error) ? 'stop' : 'retry'
      },
    })
    globalThis.addEventListener?.('online', this.#networkAvailable)
    this.pathActivity.subscribe(snapshot => {
      this.#directUsable = snapshot.lanes.some(lane => lane.route === 'direct')
      for (const metrics of this.#downloads.values()) metrics.availability(this.#directUsable)
    })
    this.connectivity = new V2SupervisedConnectivity(this.#policy)
    this.#current = this.#generations.create(this.#nextGeneration++, options.initial)
    this.pathActivity.generationInstalled(this.#current.id)
    this.connectivity.bind(this.#current.connectivity)
    this.#current.relays.start()
    this.content = new V2SupervisedContent(this, options.randomBytes)
    this.catalogOperations = Object.freeze({
      fetchPage: (request: V2CatalogPageRequest, signal: AbortSignal) =>
        this.execute(signal, (generation) =>
        new V2CatalogSessionOperations(generation.session, generation.lanes.requests).fetchPage(request, signal))
        .then((result) => result.value),
      failProtocol: async (reason: unknown) => this.#failTerminal(reason),
    })
  }

  get generationId(): number {
    return this.#current.id
  }

  get protocolSessionId(): string {
    return encodeBase64Url(this.#current.session.keys.protocolSessionId)
  }

  get protocolSessionIdentity(): V2ProtocolSessionIdentity {
    return this.#current.session.protocolSessionIdentity
  }

  get isStopped(): boolean {
    return this.#stopped
  }

  requestReconnect(): void {
    if (this.#stopped || this.#failed) return
    this.#traceConnection({ transition: 'retry_requested' })
    this.#recoveryWake.request()
  }

  beginConnectivity(intent: V2ContentIntent): V2ConnectivityActivation {
    const activation = this.connectivity.begin(intent)
    if (intent === 'download') {
      const metrics = new DownloadMetrics(crypto.randomUUID(), this.#directUsable, () => this.#clock.now())
      this.#downloads.set(activation.routes, metrics)
      const unsubscribe = activation.routes.subscribe(() => {
        if (activation.routes.active) return
        metrics.snapshot(true)
        this.#downloads.delete(activation.routes)
        unsubscribe()
      })
    }
    return activation
  }

  downloadMetrics(routes: V2BlockRouteEligibility): DownloadMetrics | undefined {
    return this.#downloads.get(routes)
  }

  async execute<T>(
    signal: AbortSignal | undefined,
    operation: (generation: V2ReceiverGeneration) => Promise<T>,
  ): Promise<{ readonly generation: V2ReceiverGeneration; readonly value: T }> {
    const recovery = new OperationRecovery()
    const operationSequence = ++this.#operationSequence
    while (true) {
      const generation = await this.#ready(signal)
      recovery.beginAttempt(generation.availability)
      try {
        const value = await operation(generation)
        return Object.freeze({ generation, value })
      } catch (error) {
        await this.#recoverOperation(generation, recovery, operationSequence, error, signal)
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
      await managed.lanes.waitForContentAdmission(signal)
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

  waitForProtocolSessionReplacement(
    issuingIdentity: V2ProtocolSessionIdentity,
    signal: AbortSignal,
  ): Promise<void> {
    signal.throwIfAborted()
    if (!equalV2DiagnosticIdentities(issuingIdentity, this.#current.session.protocolSessionIdentity)) {
      return Promise.resolve()
    }
    return this.#waitForGenerationAfter(this.#current.id, signal)
  }

  subscribeProtocolGeneration(listener: V2ProtocolGenerationListener): () => void {
    this.#generationListeners.add(listener)
    return () => this.#generationListeners.delete(listener)
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  #laneChanged(generation: V2ReceiverGeneration, change: V2LaneChange): void {
    if (!this.isCurrent(generation)) return
    generation.availability = Object.freeze({
      generationId: generation.id, revision: generation.availability.revision + 1,
    })
    if (change.type === 'attached') {
      this.#wakeWaiters()
      return
    }
    generation.lanes.requests.remove(change.laneId, change.laneEpoch)
    if (generation.session.isClosed || isSessionFailure(change.failure)) {
      this.#failTerminal(change.failure ?? new Error('ProtocolSession closed terminally'))
      return
    }
    if (generation.session.laneIds().length === 0) {
      generation.retired = true
      this.pathActivity.generationRetired(generation.id)
      generation.session.close().catch(() => undefined)
    }
    generation.relays.detached(change.laneId)
    if (generation.retired) generation.relays.close().catch(() => undefined)
    this.#requestReconcile()
    this.#wakeWaiters()
  }

  async #recoverOperation(
    generation: V2ReceiverGeneration,
    recovery: OperationRecovery,
    operationSequence: number,
    error: unknown,
    signal?: AbortSignal,
  ): Promise<void> {
    signal?.throwIfAborted()
    this.#throwIfTerminal()
    if (!isRecoverableOperationFailure(error, generation, this.#current)) throw error
    const availability = generation.availability
    const decision = recovery.decide(this.isCurrent(generation) ? availability : undefined)
    this.#traceOperationRecovery(generation, operationSequence, recovery, decision)
    if (decision.transition === 'exhausted') throw error
    if (decision.transition === 'wait_for_generation') {
      await this.#waitForGenerationAfter(generation.id, signal)
    } else if (decision.transition === 'wait_for_availability' &&
        this.isCurrent(generation) && generation.availability === availability) {
      await this.#waitForOperationRetry(decision.delayMilliseconds, signal)
    }
  }

  async #waitForOperationRetry(milliseconds: number, signal?: AbortSignal): Promise<void> {
    const wait = new AbortController()
    const combined = AbortSignal.any([
      wait.signal, this.#lifetime.signal, ...(signal === undefined ? [] : [signal]),
    ])
    try {
      // Subscribe before arming the delay so a lane event cannot be lost. The
      // losing branch is cancelled to release both the timer and its waiter.
      await Promise.race([
        this.#waitForWake(combined),
        this.#clock.sleep(milliseconds, combined),
      ])
    } finally {
      wait.abort()
    }
  }

  #traceOperationRecovery(
    generation: V2ReceiverGeneration,
    operationSequence: number,
    recovery: OperationRecovery,
    decision: OperationRecoveryDecision,
  ): void {
    try {
      this.#protocolTrace?.current?.({
        eventName: 'operation_recovery',
        correlation: { protocolSessionId: generation.session.protocolSessionIdentity },
        operationSequence,
        generationId: generation.id,
        availabilityRevision: generation.availability.revision,
        laneCount: generation.session.laneIds().length,
        unchangedAvailabilityRetries: recovery.unchangedAvailabilityRetries,
        ...decision,
      })
    } catch {
      // Recovery observation cannot acquire authority over the caller's result.
    }
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
    this.#recoveryAttempt = 0
    const startedAt = this.#clock.now()
    while (!this.#lifetime.signal.aborted) {
      this.#reconcileRequested = false
      this.#recoveryPhase = reconnectPhase(this.#recoveryAttempt, this.#clock.now() - startedAt)
      try {
        if (!(await this.#reconcileGeneration(this.#current))) return
      } catch (error) {
        if (!(error instanceof GenerationRecoveryExhaustedError)) this.#traceConnection({ transition: 'attempt_failed', failure: error })
        if (!(await this.#waitAfterRecoveryFailure(error, startedAt))) return
      }
    }
  }

  async #reconcileGeneration(generation: V2ReceiverGeneration): Promise<boolean> {
    if (generation.retired || generation.session.laneIds().length === 0) {
      await this.#replaceGeneration(generation)
      return true
    }
    return false
  }

  async #waitAfterRecoveryFailure(error: unknown, startedAt: number): Promise<boolean> {
    if (this.#stopped || this.#lifetime.signal.aborted) return false
    if (isTerminalRecoveryFailure(error)) {
      this.#failTerminal(error)
      return false
    }
    this.#observeRecoveryError(error)
    const now = this.#clock.now()
    this.#recoveryPhase = error instanceof GenerationRecoveryExhaustedError
      ? 'waiting' : reconnectPhase(this.#recoveryAttempt, now - startedAt)
    const capacityDelay = this.#generationRecovery.nextCapacityMilliseconds(now)
    const serverDelay = recoveryRetryAfter(error)
    const requiredDelay = Math.max(capacityDelay, serverDelay)
    const backoff = this.#recoveryPhase === 'waiting' ? waitingReconnectBackoff()
      : requireBackoff(this.#generationBackoffMilliseconds(this.#recoveryAttempt - 1))
    const delay = Math.max(requiredDelay, backoff)
    const retryAt = now + delay
    try {
      // A manual request can skip backoff, but cannot manufacture capacity or
      // override the service's retry deadline. Publish that same distinction to UI.
      if (requiredDelay > 0) await this.#waitForReconnect(requiredDelay, {
        kind: 'waiting', reason: serverDelay >= capacityDelay ? 'server' : 'capacity', retryAt,
      }, error)
      const remaining = Math.max(0, retryAt - this.#clock.now())
      if (remaining > 0) await this.#waitForReconnect(remaining, {
        kind: 'waiting', reason: 'backoff', retryAt,
      }, error)
      return true
    } catch {
      return false
    }
  }

  async #waitForReconnect(
    milliseconds: number,
    activity: Extract<ReceiverReconnectActivity, { kind: 'waiting' }>,
    failure: unknown,
  ): Promise<void> {
    // Arm the wake before notifying observers, which may immediately request a retry.
    const wait = activity.reason === 'backoff'
      ? this.#recoveryWake.sleep(this.#clock, milliseconds, this.#lifetime.signal)
      : this.#clock.sleep(milliseconds, this.#lifetime.signal)
    this.connection.reconnecting(activity)
    this.#traceConnection({ transition: 'waiting', delayMilliseconds: milliseconds,
      waitReason: activity.reason, failure })
    await wait
  }

  async #replaceGeneration(previous: V2ReceiverGeneration): Promise<void> {
    const reservation = this.#generationRecovery.reserve(this.#clock.now())
    this.#recoveryAttempt += 1
    this.connection.reconnecting({ kind: 'connecting' })
    this.#traceConnection({ transition: 'attempt_started' })
    const core = await runGenerationRecovery({
      reservation,
      parent: this.#lifetime.signal,
      now: () => this.#clock.now(),
      connect: (signal) => this.#factory.connectFresh(signal),
      close: closeCore,
    })
    if (this.#stopped || this.#current !== previous) {
      await closeCore(core)
      return
    }
    let next: V2ReceiverGeneration
    try {
      next = this.#generations.create(this.#nextGeneration++, core)
    } catch (error) {
      await closeCore(core)
      throw error
    }
    this.#current = next
    this.#traceConnection({ transition: 'connected' })
    this.connection.connected()
    this.pathActivity.generationInstalled(next.id)
    this.connectivity.bind(next.connectivity)
    next.relays.start()
    this.#wakeWaiters()
    this.#publishGenerationInstalled(next)
    await previous.close()
  }

  #publishGenerationInstalled(generation: V2ReceiverGeneration): void {
    const observation = Object.freeze({
      generationId: generation.id,
      protocolSessionId: encodeBase64Url(generation.session.keys.protocolSessionId),
      protocolSessionIdentity: generation.session.protocolSessionIdentity,
    })
    for (const listener of this.#generationListeners) {
      try {
        listener(observation)
      } catch {
        // A controller observer cannot revoke the generation already installed by this owner.
      }
    }
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
    this.#traceConnection({ transition: 'terminal', failure: reason })
    this.connection.failed(reason)
    this.#lifetime.abort(reason)
    globalThis.removeEventListener?.('online', this.#networkAvailable)
    for (const waiter of this.#waiters) waiter.reject(reason)
    this.#waiters.clear()
    this.#current.retired = true
    this.#generationListeners.clear()
    this.content.close()
    this.pathActivity.close()
    this.connectivity.close().catch(() => undefined)
    this.#factory.close()
    this.#current.close().catch(() => undefined)
  }

  #throwIfTerminal(): void {
    if (this.#stopped) throw new DOMException('Receiver supervisor stopped', 'AbortError')
    if (this.#failed) throw this.#terminal
  }

  #traceConnection(observation: Omit<RecoveryObservation, 'attempt' | 'phase'>): void {
    observeRecovery(this.#protocolTrace, { generationId: this.#current.id,
      shareInstanceId: this.descriptor.shareInstanceId,
      correlation: { protocolSessionId: this.#current.session.protocolSessionIdentity } },
    { attempt: this.#recoveryAttempt, phase: this.#recoveryPhase, ...observation })
  }

  #observeRecoveryError(error: unknown): void {
    try { this.#onRecoveryError(error) } catch { /* Passive observation cannot terminate recovery. */ }
  }

  async #close(): Promise<void> {
    if (this.#stopped) return
    // Stop authority is published before any close can emit lane events.
    this.#stopped = true
    this.connection.close()
    this.#lifetime.abort(new DOMException('Receiver supervisor stopped', 'AbortError'))
    globalThis.removeEventListener?.('online', this.#networkAvailable)
    for (const waiter of this.#waiters) waiter.reject(this.#lifetime.signal.reason)
    this.#waiters.clear()
    this.#current.retired = true
    this.#generationListeners.clear()
    this.content.close()
    this.pathActivity.close()
    await Promise.allSettled([
      this.connectivity.close(),
      this.#current.close(),
      ...(this.#reconcileTask === undefined ? [] : [this.#reconcileTask]),
    ])
    this.#factory.close()
  }
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
