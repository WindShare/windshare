import { TURNPaths } from './turn-paths'
import { PeerTabAdmission } from './peer-set/tab-admission'
import { DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS, DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS } from './peer-set/contract'
import {
  LaneSet,
  type BlockLane,
} from '../content/lane-set'
import type {
  BlockRouteEligibility,
  BlockTransportRoute,
} from '../content/route-policy'
import type { ReceiverSessionRuntime } from '../session/runtime'
import {
  createPeerPathIdentityValue,
  equalDiagnosticIdentities,
  type PeerAttemptIdentity,
  type PeerPathIdentity,
  type ProtocolSessionIdentity,
} from '../session/identities'
import type { PeerChannel } from './peer-channel'
import { BrowserPeerConnections } from './browser-peer/connections'
import {
  browserPeerConnectionAvailable,
  BrowserOfferChannelFactory,
  type OfferChannelFactory,
} from './peer-offer'
import type { ConnectivityTraceSource } from './diagnostics'
import {
  createPeerPathIdentity,
  SessionSignalingRoute,
} from './session-signaling'
import {
  type PeerCandidatePublication,
  BrowserPeerAttemptExecutor,
} from './peer-attempt'
import {
  browserPeerRecoveryClock,
  BrowserPeerRecoveryRearmSource,
  type PeerRecoveryActivation,
  type PeerRecoveryDependencies,
  type PeerRecoverySupervisorOptions,
  PeerRecoverySupervisor,
} from './peer-set/path'
import { PeerSet } from './peer-set/peer-set'
import { encodeBase64Url } from '../crypto/bytes'
import { PeerPathControl } from './peer-set/path-control'
import { PeerNetworkGeneration } from './peer-set/network-generation'
import type { ConnectionPurpose } from './signaling-codec'
import { PeerSlotPool } from '../session/peer-slots/slots'
import { PeerRecoveryTrace } from './peer-set/trace'
import { ThroughputController, THROUGHPUT_SAMPLE_MS, type ThroughputDecision, type ThroughputPath } from './peer-set/throughput'

const MAXIMUM_THROUGHPUT_PATHS = 3
const turnAdmission = new PeerTabAdmission()

export type ConnectivityPolicy = 'auto' | 'relay-only' | 'p2p-only'

export interface ContentLaneAdmissionObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: BlockTransportRoute
}

export type ContentLaneDetachmentObservation = ContentLaneAdmissionObservation

export interface ReceiverConnectivityOptions {
  readonly policy?: ConnectivityPolicy
  readonly session: ReceiverSessionRuntime
  readonly lanes: LaneSet
  readonly createBlockLane: (laneId: number) => BlockLane
  readonly relayLaneId?: number
  readonly offers?: OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly nativePeerUsable?: () => boolean
  readonly connectivityTrace?: ConnectivityTraceSource
  readonly onContentLaneAdmitted?: (observation: ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: ContentLaneDetachmentObservation) => void
  readonly peerRecovery?: PeerRecoveryDependencies
}

export type ContentIntent = 'preview' | 'download'

export interface ConnectivityActivation {
  readonly routes: BlockRouteEligibility
  close(): void
}

/** Stable click-scoped authority shared by every ProtocolSession generation it spans. */
export class ConnectivityRouteAuthority implements BlockRouteEligibility {
  readonly #policy: ConnectivityPolicy
  readonly #listeners = new Set<() => void>()
  #active = true
  #closedReason: unknown

  constructor(policy: ConnectivityPolicy = 'auto') { this.#policy = policy }

  get active(): boolean {
    return this.#active
  }

  allows(route: BlockTransportRoute): boolean {
    return this.#active && (this.#policy === 'auto' ||
      (this.#policy === 'relay-only' ? route === 'application-relay' : route === 'direct'))
  }

  assertActive(): void {
    if (this.#active) return
    throw this.#closedReason ?? new DOMException('Content activation is closed', 'AbortError')
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  close(reason: unknown = new DOMException('Content activation is closed', 'AbortError')): void {
    if (!this.#active) return
    this.#active = false
    this.#closedReason = reason
    this.#notify()
    this.#listeners.clear()
  }

  #notify(): void {
    for (const listener of this.#listeners) {
      try {
        listener()
      } catch {
        // Route changes wake independent waiters; an observer cannot own policy.
      }
    }
  }
}

interface ActiveConnectivity {
  readonly routes: ConnectivityRouteAuthority
  readonly ownsRoutes: boolean
  peerRecovery: PeerRecoveryActivation | undefined
}

interface InstalledPeer {
  readonly protocolSessionId: ProtocolSessionIdentity
  readonly peerPathId: PeerPathIdentity
  readonly attemptId: PeerAttemptIdentity
  readonly laneId: number
  readonly laneEpoch: number
  readonly peer: PeerChannel
  readonly route: SessionSignalingRoute
  closeTask?: Promise<void>
  unsubscribePathRoute?: () => void
}

/** Browsing keeps relay control-only; explicit content activation admits both routes. */
export class ReceiverConnectivity {
  readonly #optionalPaths = new Map<string, { identity: PeerPathIdentity; activation?: PeerRecoveryActivation }>()
  #throughputTask: Promise<void> | undefined
  readonly #policy: ConnectivityPolicy
  readonly #session: ReceiverSessionRuntime
  readonly #lanes: LaneSet
  readonly #createBlockLane: (laneId: number) => BlockLane
  readonly #offers: OfferChannelFactory
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #nativePeerUsable: () => boolean
  readonly #connectivityTrace: ConnectivityTraceSource | undefined
  readonly #onContentLaneAdmitted: (
    observation: ContentLaneAdmissionObservation,
  ) => void
  readonly #onContentLaneDetached: (
    observation: ContentLaneDetachmentObservation,
  ) => void
  readonly #peerRecoveryOptions: PeerRecoveryDependencies
  readonly #protocolSessionId: ProtocolSessionIdentity
  readonly #lifetime = new AbortController()
  readonly #admitted = new Map<number, { readonly laneEpoch: number; readonly route: BlockTransportRoute }>()
  readonly #laneEpochs = new Map<number, number>()
  readonly #activations = new Map<number, ActiveConnectivity>()
  readonly #peerCleanupTasks = new Map<InstalledPeer, Promise<void>>()
  readonly #unsubscribeLaneChanges: () => void
  readonly #slots = new PeerSlotPool()
  readonly #peers = new PeerSet(this.#slots)
  readonly #turn: TURNPaths
  readonly #pathControls = new Map<string, PeerPathControl>()
  #peerPathId: PeerPathIdentity | undefined
  readonly #installedPeers = new Map<string, InstalledPeer>()
  readonly #relayLaneIds = new Set<number>()
  #browse: PeerRecoveryActivation | undefined
  #closeTask: Promise<void> | undefined
  #nextActivation = 1

  constructor(options: ReceiverConnectivityOptions) {
    const peerConnections = new BrowserPeerConnections()
    this.#session = options.session
    this.#lanes = options.lanes
    this.#createBlockLane = options.createBlockLane
    this.#policy = options.policy ?? 'auto'
    this.#relayLaneIds.add(options.relayLaneId ?? options.session.initialLaneId)
    this.#offers = options.offers ?? new BrowserOfferChannelFactory({ peerConnections })
    this.#randomBytes = options.randomBytes
    this.#nativePeerUsable = options.nativePeerUsable ?? (() => browserPeerConnectionAvailable())
    this.#connectivityTrace = options.connectivityTrace
    this.#onContentLaneAdmitted = options.onContentLaneAdmitted ?? (() => undefined)
    this.#onContentLaneDetached = options.onContentLaneDetached ?? (() => undefined)
    this.#peerRecoveryOptions = { ...options.peerRecovery,
      network: options.peerRecovery?.network ?? new PeerNetworkGeneration() }
    this.#protocolSessionId = options.session.protocolSessionIdentity
    const clock = this.#peerRecoveryOptions.clock ?? browserPeerRecoveryClock
    this.#turn = new TURNPaths({
      session: this.#session, slots: this.#slots, now: () => clock.now(),
      identity: () => createPeerPathIdentity(this.#randomBytes),
      hasDelivered: lane => this.#lanes.hasDelivered(lane.laneId, lane.laneEpoch),
      start: (grant, identity, ordinal) => new BrowserPeerAttemptExecutor({
        session: this.#session, offers: this.#offers, peerPathIdentity: identity.copyBytes(), clock,
        tabAdmission: turnAdmission, admissionReceiver: this, turnGrant: grant,
        publish: candidate => this.#publishPeer(candidate, 'turn'),
        ...(this.#randomBytes === undefined ? {} : { randomBytes: this.#randomBytes }),
        ...(this.#connectivityTrace === undefined ? {} : { trace: this.#connectivityTrace }),
      }).createAttempt({ ...ordinal, protocolSessionId: this.#protocolSessionId, peerPathId: identity, purpose: 'turn-relay',
        waveOrdinal: 1, waveAttemptOrdinal: 1, sessionAttemptOrdinal: Number(ordinal.attemptSequence),
        admissionWaitBudgetMilliseconds: DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS,
        negotiationBudgetMilliseconds: DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS,
        admissionBudgetMilliseconds: DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS, phaseChanged: () => undefined }),
      trace: (stage, detail, path, lane) => new PeerRecoveryTrace(this.#protocolSessionId, path, this.#connectivityTrace)
        .emit(() => ({ stage, ...detail }), undefined, lane),
      release: async (lane, retirement) => { await this.#releasePeer(lane.laneId, lane.laneEpoch, retirement) },
    })
    this.#laneEpochs.set(options.session.initialLaneId, options.session.keys.initialLaneEpoch)
    this.#unsubscribeLaneChanges = this.#session.subscribeLaneChanges((change) => {
      if (change.type === 'attached') {
        this.#laneEpochs.set(change.laneId, change.laneEpoch)
      } else if (change.type === 'unavailable') {
        this.#withdrawAdmittedLane(change.laneId, change.laneEpoch)
      } else {
        if (this.#laneEpochs.get(change.laneId) === change.laneEpoch) {
          this.#laneEpochs.delete(change.laneId)
          this.#withdrawAdmittedLane(change.laneId, change.laneEpoch)
          this.#lanes.remove(change.laneId)
        }
        this.#peerDetached(change.laneId, change.laneEpoch)
      }
    })
  }

  beginBrowse(): void {
    if (this.#browse !== undefined || this.#policy !== 'auto' || !this.#capabilityAllowsAttempt()) return
    this.#browse = this.#ensurePeerRecovery().browse()
  }

  begin(
    _intent: ContentIntent,
    options: {
      readonly routeAuthority?: ConnectivityRouteAuthority
    } = {},
  ): ConnectivityActivation {
    this.#lifetime.signal.throwIfAborted()
    const id = this.#nextActivation++
    const routes = options.routeAuthority ?? new ConnectivityRouteAuthority(this.#policy)
    routes.assertActive()
    const active: {
      routes: ConnectivityRouteAuthority
      ownsRoutes: boolean
      peerRecovery: PeerRecoveryActivation | undefined
    } = {
      routes,
      ownsRoutes: options.routeAuthority === undefined,
      peerRecovery: undefined,
    }
    this.#activations.set(id, active)
    try {
      // Content must be usable before direct negotiation can yield, fail, or stall.
      this.#admitRelay()
    } catch (error) {
      this.#activations.delete(id)
      if (options.routeAuthority === undefined) routes.close(error)
      throw error
    }
    active.peerRecovery = this.#activatePeerRecovery()
    if (this.#policy !== 'relay-only' && this.#capabilityAllowsAttempt()) {
      this.#throughputTask ??= this.#runThroughput()
    }
    if (!this.#activations.has(id)) active.peerRecovery?.close()
    let closed = false
    return Object.freeze({
      routes,
      close: () => {
        if (closed) return
        closed = true
        this.#endActivation(id)
      },
    })
  }

  addRelayLane(laneId: number): void {
    if (!this.#session.availableLaneIds().includes(laneId)) {
      throw new Error('Replacement relay lane is not attached to this ProtocolSession')
    }
    this.#relayLaneIds.add(laneId)
    const epoch = this.#laneEpochs.get(laneId)
    if (epoch !== undefined) this.#lanes.requests.add({ id: laneId, epoch, route: 'application-relay' })
    if (this.#activations.size > 0) this.#admitRelay()
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    const reason = new DOMException('Receiver connectivity closed', 'AbortError')
    this.#lifetime.abort(reason)
    this.#browse?.close()
    this.#browse = undefined
    const recoveryClose = this.#peers.close()
    const controlsClose = [...this.#pathControls.values()].map((control) => control.close())
    this.#pathControls.clear()
    for (const activationId of [...this.#activations.keys()]) {
      this.#endActivation(activationId, reason)
    }
    this.#unsubscribeLaneChanges()
    for (const installed of this.#installedPeers.values()) this.#startInstalledPeerCleanup(installed)
    this.#installedPeers.clear()
    await Promise.allSettled([
      ...controlsClose,
      ...(recoveryClose === undefined ? [] : [recoveryClose]),
      this.#joinPeerCleanup(),
      this.#throughputTask, this.#turn.reset(),
    ])
  }

  #activatePeerRecovery(): PeerRecoveryActivation | undefined {
    if (this.#policy === 'relay-only') return undefined
    if (!this.#capabilityAllowsAttempt()) {
      this.#peerUnavailable()
      return undefined
    }
    const path = this.#ensurePeerRecovery()
    for (const control of this.#pathControls.values()) control.activate()
    return path.activate()
  }

  #ensurePeerRecovery(): PeerRecoverySupervisor {
    if (this.#peerPathId !== undefined) return this.#peers.path(this.#peerPathId)!
    const peerPathIdentity = this.#randomBytes === undefined
      ? createPeerPathIdentity()
      : createPeerPathIdentity(this.#randomBytes)
    const peerPathId = createPeerPathIdentityValue(peerPathIdentity)
    const recovery = this.#peers.add(this.#recoveryOptions(peerPathId, () => this.#peerUnavailable(), 'initial'))
    this.#peerPathId = peerPathId
    const clock = this.#peerRecoveryOptions.clock ?? browserPeerRecoveryClock
    this.#pathControls.set(encodeBase64Url(peerPathIdentity), new PeerPathControl(
      this.#session, peerPathIdentity, this.#peerRecoveryOptions.network!, clock, (notice) => recovery.pathChanged(notice)))
    return recovery
  }

  #recoveryOptions(peerPathId: PeerPathIdentity, onUnavailable: () => void, purpose: ConnectionPurpose): Omit<PeerRecoverySupervisorOptions, 'slot'> {
    const peerPathIdentity = peerPathId.copyBytes()
    const clock = this.#peerRecoveryOptions.clock ?? browserPeerRecoveryClock
    const budget = this.#peerRecoveryOptions.budget
    const attempts = new BrowserPeerAttemptExecutor({
      ...(this.#peerRecoveryOptions.tabAdmission === undefined ? {} : { tabAdmission: this.#peerRecoveryOptions.tabAdmission }),
      admissionReceiver: this.#peerRecoveryOptions.budget ?? this,
      session: this.#session,
      offers: this.#offers,
      peerPathIdentity,
      clock,
      publish: (candidate) => this.#publishPeer(candidate),
      ...(this.#randomBytes === undefined ? {} : { randomBytes: this.#randomBytes }),
      ...(this.#connectivityTrace === undefined ? {} : { trace: this.#connectivityTrace }),
    })
    return {
      purpose,
      protocolSessionId: this.#protocolSessionId,
      peerPathId,
      attempts,
      ...(this.#peerRecoveryOptions.network === undefined ? {} : { network: this.#peerRecoveryOptions.network }),
      ...(this.#peerRecoveryOptions.endpoints === undefined ? {} : { endpoints: this.#peerRecoveryOptions.endpoints }),
      ...(budget === undefined ? {} : { budget }),
      releaseLane: (lane) => this.#releasePeer(lane.laneId, lane.laneEpoch),
      onUnavailable,
      ...(this.#peerRecoveryOptions.policy === undefined
        ? {}
        : { policy: this.#peerRecoveryOptions.policy }),
      clock,
      ...(this.#peerRecoveryOptions.random === undefined
        ? {}
        : { random: this.#peerRecoveryOptions.random }),
      rearmSource: this.#peerRecoveryOptions.rearmSource ?? new BrowserPeerRecoveryRearmSource(),
      ...(this.#connectivityTrace === undefined ? {} : { trace: this.#connectivityTrace }),
    }
  }

  #capabilityAllowsAttempt(): boolean {
    try {
      if (this.#nativePeerUsable()) return true
      // A caller may know more than constructor presence (for example, a local
      // ICE/DataChannel probe), but that probe never owns relay eligibility.
    } catch {
      return false
    }
    return false
  }

  #peerUnavailable(): void {
    if (this.#policy !== 'p2p-only') return
    if (this.#peers.hasEligiblePath || [...this.#admitted.values()].some(lane => lane.route === 'direct')) return
    const error = new Error('No authenticated direct path is available within the current connection wave')
    for (const [id, active] of this.#activations) {
      active.routes.close(error)
      this.#endActivation(id, error)
    }
  }

  async #runThroughput(): Promise<void> {
    const clock = this.#peerRecoveryOptions.clock ?? browserPeerRecoveryClock
    const controller = new ThroughputController()
    try {
      while (!this.#lifetime.signal.aborted) {
        await clock.sleep(THROUGHPUT_SAMPLE_MS, this.#lifetime.signal)
        if (this.#lifetime.signal.aborted) break
        const demand = this.#lanes.schedulingDemand()
        const paths = this.#throughputPaths()
        if (this.#policy === 'auto' && this.#activations.size > 0) this.#turn.step(demand)
        const pending = this.#peers.hasPendingPath || clock.now() < this.#turn.settlingUntil
        const decision = controller.step(clock.now(), { ...demand, paths, pending,
          unfinishedBytes: this.#activations.size === 0 ? 0 : demand.unfinishedBytes,
          networkWaitingBytes: this.#activations.size === 0 ? 0 : demand.networkWaitingBytes }, MAXIMUM_THROUGHPUT_PATHS)
        await this.#applyParallelism(decision)
        this.#traceParallelism(decision, demand, paths.length)
      }
    } catch (error) {
      if (!this.#lifetime.signal.aborted) throw error
    }
  }

  #throughputPaths(): ThroughputPath[] {
    const paths: ThroughputPath[] = []
    for (const [pathId, peer] of this.#installedPeers) {
      if (peer.peer.pathRoute !== 'direct') continue
      const work = this.#lanes.samplingEvidence(peer.laneId, peer.laneEpoch)
      if (work !== undefined) paths.push({ identity: { pathId, laneId: peer.laneId, laneEpoch: peer.laneEpoch },
        optional: this.#optionalPaths.has(pathId), work })
    }
    return paths
  }

  async #applyParallelism(decision: ThroughputDecision): Promise<void> {
    if (decision.action === 'explore' && this.#peers.size < MAXIMUM_THROUGHPUT_PATHS) this.#openOptional()
    const target = decision.path
    if (target === undefined) return
    if (decision.action !== 'sample') this.#lanes.stopSampling(target.laneId, target.laneEpoch)
    const peer = this.#installedPeers.get(target.pathId)
    if (peer === undefined || peer.laneId !== target.laneId || peer.laneEpoch !== target.laneEpoch) return
    if (decision.action === 'sample') this.#lanes.sampleLane(target.laneId, target.laneEpoch)
    if (decision.action === 'retire') {
      const entry = this.#optionalPaths.get(target.pathId)
      if (entry !== undefined) await this.#retireOptional(entry.identity)
    }
  }

  #traceParallelism(decision: ThroughputDecision, demand: ReturnType<LaneSet['schedulingDemand']>, paths: number): void {
    if (decision.action === 'hold' || this.#peerPathId === undefined) return
    new PeerRecoveryTrace(this.#protocolSessionId, this.#peerPathId, this.#connectivityTrace).emit(() => ({
      stage: 'parallelism', action: decision.action, reason: decision.reason ?? '', paths,
      networkWaitingBytes: demand.networkWaitingBytes, inFlightBytes: demand.inFlightBytes,
      remainingBytes: demand.remainingBytes, remainingKnown: demand.remainingKnown,
      queuedBytes: demand.queuedBytes, unfinishedBytes: demand.unfinishedBytes, capacityWaitingBytes: demand.capacityWaitingBytes,
      usefulRate: decision.usefulRate ?? 0, outputRate: decision.outputRate ?? 0,
      baselineUsefulRate: decision.baselineUsefulRate ?? 0, baselineOutputRate: decision.baselineOutputRate ?? 0,
      ...(decision.retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds: decision.retryAfterMilliseconds }),
      ...(decision.path === undefined ? {} : { trialPath: decision.path }),
      ...(decision.work === undefined ? {} : { trialWork: decision.work }),
    }))
  }

  #openOptional(): void {
    const identity = createPeerPathIdentityValue(createPeerPathIdentity(this.#randomBytes))
    const key = encodeBase64Url(identity.copyBytes())
    if (this.#peers.path(identity) !== undefined) return
    const path = this.#peers.tryAdd(this.#recoveryOptions(identity, () => {
      this.#retireOptional(identity).then(() => this.#peerUnavailable()).catch(() => this.#peerUnavailable())
    }, 'throughput'))
    if (!path) return
    const entry: { identity: PeerPathIdentity; activation?: PeerRecoveryActivation } = { identity }
    this.#optionalPaths.set(key, entry)
    const clock = this.#peerRecoveryOptions.clock ?? browserPeerRecoveryClock
    const control = new PeerPathControl(this.#session, identity.copyBytes(), this.#peerRecoveryOptions.network!, clock,
      notice => path.pathChanged(notice))
    this.#pathControls.set(key, control)
    control.activate()
    entry.activation = path.activate()
    // Immediate resource denial can retire the entry from inside activate().
    if (!this.#optionalPaths.has(key)) entry.activation.close()
  }

  async #retireOptional(identity: PeerPathIdentity): Promise<void> {
    const key = encodeBase64Url(identity.copyBytes())
    const entry = this.#optionalPaths.get(key)
    if (entry === undefined) return
    this.#optionalPaths.delete(key)
    entry.activation?.close()
    const control = this.#pathControls.get(key)
    this.#pathControls.delete(key)
    const installed = this.#installedPeers.get(key)
    if (installed !== undefined) this.#releasePeer(installed.laneId, installed.laneEpoch)
    await Promise.allSettled([this.#peers.remove(identity), control?.close()])
  }

  #publishPeer(candidate: PeerCandidatePublication, expected: 'direct' | 'turn' = 'direct'): boolean {
    if (
      this.#lifetime.signal.aborted || this.#installedPeers.has(encodeBase64Url(candidate.peerPathId.copyBytes())) ||
      !equalDiagnosticIdentities(candidate.protocolSessionId, this.#protocolSessionId) ||
      (expected === 'turn' ? !this.#turn.owns(candidate.peerPathId) : this.#peers.path(candidate.peerPathId) === undefined) ||
      this.#laneEpochs.get(candidate.laneId) !== candidate.laneEpoch ||
      !this.#session.availableLaneIds().includes(candidate.laneId)
    ) return false
    const pathRoute = candidate.peer.pathRoute
    if (pathRoute !== expected || !this.#admit(candidate.laneId, candidate.laneEpoch, pathRoute)) return false
    const installed: InstalledPeer = { ...candidate }
    this.#installedPeers.set(encodeBase64Url(candidate.peerPathId.copyBytes()), installed)
    const unsubscribe = candidate.peer.subscribePathRoute?.((nextRoute) => {
      if (nextRoute === pathRoute || this.#lifetime.signal.aborted) return
      // Route class is authenticated lane metadata. Retiring the old lane
      // prevents blocks from claiming a direct route after pair loss.
      this.#removeAdmittedLane(candidate.laneId, candidate.laneEpoch)
      this.#peerDetached(candidate.laneId, candidate.laneEpoch)
    })
    if (unsubscribe !== undefined) installed.unsubscribePathRoute = unsubscribe
    return true
  }

  #endActivation(
    id: number,
    reason: unknown = new DOMException('Content activation closed', 'AbortError'),
  ): void {
    const active = this.#activations.get(id)
    if (active === undefined) return
    this.#activations.delete(id)
    if (this.#activations.size === 0) void this.#turn.reset()
    if (this.#activations.size === 0) for (const control of this.#pathControls.values()) control.deactivate()
    active.peerRecovery?.close()
    if (active.ownsRoutes) active.routes.close(reason)
  }

  #peerDetached(laneId: number, laneEpoch: number): void {
    const installed = [...this.#installedPeers.values()].find((peer) =>
      peer.laneId === laneId && peer.laneEpoch === laneEpoch)
    if (
      installed === undefined ||
      !equalDiagnosticIdentities(installed.protocolSessionId, this.#protocolSessionId) ||
      installed.laneId !== laneId ||
      installed.laneEpoch !== laneEpoch
    ) return
    this.#installedPeers.delete(encodeBase64Url(installed.peerPathId.copyBytes()))
    this.#turn.detached(installed.peerPathId, { laneId, laneEpoch })
    this.#peers.path(installed.peerPathId)?.peerDetached({
      attemptId: installed.attemptId,
      protocolSessionId: installed.protocolSessionId,
      peerPathId: installed.peerPathId,
      laneId,
      laneEpoch,
    })
    this.#startInstalledPeerCleanup(installed)
  }

  #releasePeer(laneId: number, laneEpoch: number, retirement?: Promise<void>): Promise<void> | undefined {
    const matches = (peer: InstalledPeer) => peer.laneId === laneId && peer.laneEpoch === laneEpoch
    const installed = [...this.#installedPeers.values()].find(matches)
    if (installed === undefined) return [...this.#peerCleanupTasks.keys()].find(matches)?.closeTask ?? retirement
    this.#removeAdmittedLane(laneId, laneEpoch)
    this.#installedPeers.delete(encodeBase64Url(installed.peerPathId.copyBytes()))
    return this.#startInstalledPeerCleanup(installed, retirement)
  }

  #startInstalledPeerCleanup(installed: InstalledPeer, retirement?: Promise<void>): Promise<void> {
    if (installed.closeTask !== undefined) return installed.closeTask
    installed.unsubscribePathRoute?.()
    const closeTask = Promise.allSettled([
      // Record retirement before transport cleanup can finish the receive stream.
      retirement ?? this.#session.retireLane(installed.laneId, installed.laneEpoch),
      Promise.resolve().then(() => installed.peer.close()),
      Promise.resolve().then(() => installed.route.close()),
    ]).then(() => {
      this.#peerCleanupTasks.delete(installed)
    })
    installed.closeTask = closeTask
    this.#peerCleanupTasks.set(installed, closeTask)
    return closeTask
  }

  async #joinPeerCleanup(): Promise<void> {
    while (this.#peerCleanupTasks.size > 0) {
      await Promise.allSettled([...this.#peerCleanupTasks.values()])
    }
  }

  #admit(laneId: number, laneEpoch: number, route: BlockTransportRoute): boolean {
    const existing = this.#admitted.get(laneId)
    if (existing !== undefined) {
      return existing.laneEpoch === laneEpoch && existing.route === route
    }
    if (!this.#session.availableLaneIds().includes(laneId)) return false
    this.#lanes.add(this.#createBlockLane(laneId), route, laneEpoch)
    this.#admitted.set(laneId, { laneEpoch, route })
    try {
      this.#onContentLaneAdmitted(Object.freeze({ laneId, laneEpoch, route }))
    } catch {
      // Admission is already authoritative; diagnostics cannot revoke or corrupt it.
    }
    return true
  }

  #admitRelay(): void {
    for (const laneId of this.#relayLaneIds) {
      const laneEpoch = this.#laneEpochs.get(laneId)
      if (laneEpoch !== undefined) this.#admit(laneId, laneEpoch, 'application-relay')
    }
  }

  #removeAdmittedLane(laneId: number, laneEpoch: number): void {
    if (this.#admitted.get(laneId)?.laneEpoch !== laneEpoch) return
    this.#withdrawAdmittedLane(laneId, laneEpoch)
    this.#lanes.remove(laneId)
  }

  #withdrawAdmittedLane(laneId: number, laneEpoch: number): void {
    const admitted = this.#admitted.get(laneId)
    if (admitted === undefined || admitted.laneEpoch !== laneEpoch) return
    this.#admitted.delete(laneId)
    this.#lanes.withdraw(laneId, laneEpoch)
    try {
      this.#onContentLaneDetached(Object.freeze({
        laneId,
        laneEpoch,
        route: admitted.route,
      }))
    } catch {
      // The lane is already ineligible; diagnostics cannot re-admit it.
    }
  }
}
