import {
  V2LaneSet,
  type V2BlockLane,
} from '../content/v2-lane-set'
import type {
  V2BlockRouteEligibility,
  V2BlockTransportRoute,
} from '../content/v2-route-policy'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import {
  createV2PeerPathIdentityValue,
  equalV2DiagnosticIdentities,
  type V2PeerAttemptIdentity,
  type V2PeerPathIdentity,
  type V2ProtocolSessionIdentity,
} from '../session/v2-identities'
import type { PeerChannel } from './peer-channel'
import {
  browserPeerConnectionAvailable,
  BrowserOfferChannelFactory,
  type OfferChannelFactory,
} from './peer-offer'
import type { V2ConnectivityTraceSource } from './diagnostics'
import {
  createV2PeerPathIdentity,
  V2SessionSignalingRoute,
} from './v2-session-signaling'
import {
  type V2PeerCandidatePublication,
  V2BrowserPeerAttemptExecutor,
} from './v2-peer-attempt'
import {
  browserV2PeerRecoveryClock,
  BrowserV2PeerRecoveryRearmSource,
  type V2PeerRecoveryActivation,
  type V2PeerRecoveryDependencies,
  V2PeerRecoverySupervisor,
} from './peer-set/path'
import { V2PeerSet } from './peer-set/peer-set'
import { encodeBase64Url } from '../crypto/bytes'
import { PeerPathControl } from './peer-set/path-control'
import { PeerNetworkGeneration } from './peer-set/network-generation'

export type V2ConnectivityPolicy = 'auto' | 'relay-only' | 'p2p-only'

export interface V2ContentLaneAdmissionObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
}

export type V2ContentLaneDetachmentObservation = V2ContentLaneAdmissionObservation

export interface V2ReceiverConnectivityOptions {
  readonly policy?: V2ConnectivityPolicy
  readonly session: V2ReceiverSessionRuntime
  readonly lanes: V2LaneSet
  readonly createBlockLane: (laneId: number) => V2BlockLane
  readonly relayLaneId?: number
  readonly offers?: OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly nativePeerUsable?: () => boolean
  readonly connectivityTrace?: V2ConnectivityTraceSource
  readonly onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
  readonly peerRecovery?: V2PeerRecoveryDependencies
}

export type V2ContentIntent = 'preview' | 'download'

export interface V2ConnectivityActivation {
  readonly routes: V2BlockRouteEligibility
  close(): void
}

/** Stable click-scoped authority shared by every ProtocolSession generation it spans. */
export class V2ConnectivityRouteAuthority implements V2BlockRouteEligibility {
  readonly #policy: V2ConnectivityPolicy
  readonly #listeners = new Set<() => void>()
  #active = true
  #closedReason: unknown

  constructor(policy: V2ConnectivityPolicy = 'auto') { this.#policy = policy }

  get active(): boolean {
    return this.#active
  }

  allows(route: V2BlockTransportRoute): boolean {
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

interface V2ActiveConnectivity {
  readonly routes: V2ConnectivityRouteAuthority
  readonly ownsRoutes: boolean
  peerRecovery: V2PeerRecoveryActivation | undefined
}

interface V2InstalledPeer {
  readonly protocolSessionId: V2ProtocolSessionIdentity
  readonly peerPathId: V2PeerPathIdentity
  readonly attemptId: V2PeerAttemptIdentity
  readonly laneId: number
  readonly laneEpoch: number
  readonly peer: PeerChannel
  readonly route: V2SessionSignalingRoute
  closeTask?: Promise<void>
  unsubscribePathRoute?: () => void
}

/** Browsing keeps relay control-only; explicit content activation admits both routes. */
export class V2ReceiverConnectivity {
  readonly #policy: V2ConnectivityPolicy
  readonly #session: V2ReceiverSessionRuntime
  readonly #lanes: V2LaneSet
  readonly #createBlockLane: (laneId: number) => V2BlockLane
  readonly #offers: OfferChannelFactory
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #nativePeerUsable: () => boolean
  readonly #connectivityTrace: V2ConnectivityTraceSource | undefined
  readonly #onContentLaneAdmitted: (
    observation: V2ContentLaneAdmissionObservation,
  ) => void
  readonly #onContentLaneDetached: (
    observation: V2ContentLaneDetachmentObservation,
  ) => void
  readonly #peerRecoveryOptions: V2PeerRecoveryDependencies
  readonly #protocolSessionId: V2ProtocolSessionIdentity
  readonly #lifetime = new AbortController()
  readonly #admitted = new Map<number, { readonly laneEpoch: number; readonly route: V2BlockTransportRoute }>()
  readonly #laneEpochs = new Map<number, number>()
  readonly #activations = new Map<number, V2ActiveConnectivity>()
  readonly #peerCleanupTasks = new Map<V2InstalledPeer, Promise<void>>()
  readonly #unsubscribeLaneChanges: () => void
  readonly #peers = new V2PeerSet()
  readonly #pathControls = new Map<string, PeerPathControl>()
  #peerPathId: V2PeerPathIdentity | undefined
  readonly #installedPeers = new Map<string, V2InstalledPeer>()
  readonly #relayLaneIds = new Set<number>()
  #browse: V2PeerRecoveryActivation | undefined
  #closeTask: Promise<void> | undefined
  #nextActivation = 1

  constructor(options: V2ReceiverConnectivityOptions) {
    this.#session = options.session
    this.#lanes = options.lanes
    this.#createBlockLane = options.createBlockLane
    this.#policy = options.policy ?? 'auto'
    this.#relayLaneIds.add(options.relayLaneId ?? options.session.initialLaneId)
    this.#offers = options.offers ?? new BrowserOfferChannelFactory()
    this.#randomBytes = options.randomBytes
    this.#nativePeerUsable = options.nativePeerUsable ?? (() => browserPeerConnectionAvailable())
    this.#connectivityTrace = options.connectivityTrace
    this.#onContentLaneAdmitted = options.onContentLaneAdmitted ?? (() => undefined)
    this.#onContentLaneDetached = options.onContentLaneDetached ?? (() => undefined)
    this.#peerRecoveryOptions = { ...options.peerRecovery,
      network: options.peerRecovery?.network ?? new PeerNetworkGeneration() }
    this.#protocolSessionId = options.session.protocolSessionIdentity
    this.#laneEpochs.set(options.session.initialLaneId, options.session.keys.initialLaneEpoch)
    this.#unsubscribeLaneChanges = this.#session.subscribeLaneChanges((change) => {
      if (change.type === 'attached') {
        this.#laneEpochs.set(change.laneId, change.laneEpoch)
      } else {
        if (this.#laneEpochs.get(change.laneId) === change.laneEpoch) {
          this.#laneEpochs.delete(change.laneId)
        }
        this.#removeAdmittedLane(change.laneId, change.laneEpoch)
        this.#peerDetached(change.laneId, change.laneEpoch)
      }
    })
  }

  beginBrowse(): void {
    if (this.#browse !== undefined || this.#policy !== 'auto' || !this.#capabilityAllowsAttempt()) return
    this.#browse = this.#ensurePeerRecovery().browse()
  }

  begin(
    _intent: V2ContentIntent,
    options: {
      readonly routeAuthority?: V2ConnectivityRouteAuthority
    } = {},
  ): V2ConnectivityActivation {
    this.#lifetime.signal.throwIfAborted()
    const id = this.#nextActivation++
    const routes = options.routeAuthority ?? new V2ConnectivityRouteAuthority(this.#policy)
    routes.assertActive()
    const active: {
      routes: V2ConnectivityRouteAuthority
      ownsRoutes: boolean
      peerRecovery: V2PeerRecoveryActivation | undefined
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
    if (!this.#session.laneIds().includes(laneId)) {
      throw new Error('Replacement relay lane is not attached to this ProtocolSession')
    }
    this.#relayLaneIds.add(laneId)
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
    ])
  }

  #activatePeerRecovery(): V2PeerRecoveryActivation | undefined {
    if (this.#policy === 'relay-only') return undefined
    if (!this.#capabilityAllowsAttempt()) {
      this.#peerUnavailable()
      return undefined
    }
    const path = this.#ensurePeerRecovery()
    for (const control of this.#pathControls.values()) control.activate()
    return path.activate()
  }

  #ensurePeerRecovery(): V2PeerRecoverySupervisor {
    if (this.#peerPathId !== undefined) return this.#peers.path(this.#peerPathId)!
    const peerPathIdentity = this.#randomBytes === undefined
      ? createV2PeerPathIdentity()
      : createV2PeerPathIdentity(this.#randomBytes)
    const peerPathId = createV2PeerPathIdentityValue(peerPathIdentity)
    const clock = this.#peerRecoveryOptions.clock ?? browserV2PeerRecoveryClock
    const attempts = new V2BrowserPeerAttemptExecutor({
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
    this.#peerPathId = peerPathId
    const recovery = this.#peers.add({
      protocolSessionId: this.#protocolSessionId,
      peerPathId,
      attempts,
      ...(this.#peerRecoveryOptions.network === undefined ? {} : { network: this.#peerRecoveryOptions.network }),
      ...(this.#peerRecoveryOptions.endpoints === undefined ? {} : { endpoints: this.#peerRecoveryOptions.endpoints }),
      ...(this.#peerRecoveryOptions.budget === undefined ? {} : { budget: this.#peerRecoveryOptions.budget }),
      releaseLane: (lane) => this.#releasePeer(lane.laneId, lane.laneEpoch),
      onUnavailable: () => this.#peerUnavailable(),
      ...(this.#peerRecoveryOptions.policy === undefined
        ? {}
        : { policy: this.#peerRecoveryOptions.policy }),
      clock,
      ...(this.#peerRecoveryOptions.random === undefined
        ? {}
        : { random: this.#peerRecoveryOptions.random }),
      rearmSource: this.#peerRecoveryOptions.rearmSource ?? new BrowserV2PeerRecoveryRearmSource(),
      ...(this.#connectivityTrace === undefined ? {} : { trace: this.#connectivityTrace }),
    })
    this.#pathControls.set(encodeBase64Url(peerPathIdentity), new PeerPathControl(
      this.#session, peerPathIdentity, this.#peerRecoveryOptions.network!, clock, (notice) => recovery.pathChanged(notice),
      this.#peerRecoveryOptions.providerProfile))
    return recovery
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
    const error = new Error('No authenticated direct path is available within the current connection wave')
    for (const [id, active] of this.#activations) {
      active.routes.close(error)
      this.#endActivation(id, error)
    }
  }

  #publishPeer(candidate: V2PeerCandidatePublication): boolean {
    if (
      this.#lifetime.signal.aborted || this.#installedPeers.has(encodeBase64Url(candidate.peerPathId.copyBytes())) ||
      !equalV2DiagnosticIdentities(candidate.protocolSessionId, this.#protocolSessionId) ||
      this.#peerPathId === undefined ||
      !equalV2DiagnosticIdentities(candidate.peerPathId, this.#peerPathId) ||
      this.#laneEpochs.get(candidate.laneId) !== candidate.laneEpoch ||
      !this.#session.laneIds().includes(candidate.laneId)
    ) return false
    const pathRoute = candidate.peer.pathRoute
    if (this.#policy === 'p2p-only' && pathRoute !== 'direct') return false
    if (pathRoute === undefined || !this.#admit(candidate.laneId, candidate.laneEpoch, pathRoute)) return false
    const installed: V2InstalledPeer = { ...candidate }
    this.#installedPeers.set(encodeBase64Url(candidate.peerPathId.copyBytes()), installed)
    const unsubscribe = candidate.peer.subscribePathRoute?.((nextRoute) => {
      if (nextRoute === pathRoute || this.#lifetime.signal.aborted) return
      // Route class is authenticated lane metadata. Retiring the old lane also
      // prevents blocks spanning a direct↔TURN switch from claiming one route.
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
    if (this.#activations.size === 0) for (const control of this.#pathControls.values()) control.revoke()
    active.peerRecovery?.close()
    if (active.ownsRoutes) active.routes.close(reason)
  }

  #peerDetached(laneId: number, laneEpoch: number): void {
    const installed = [...this.#installedPeers.values()].find((peer) =>
      peer.laneId === laneId && peer.laneEpoch === laneEpoch)
    if (
      installed === undefined ||
      !equalV2DiagnosticIdentities(installed.protocolSessionId, this.#protocolSessionId) ||
      this.#peerPathId === undefined ||
      !equalV2DiagnosticIdentities(installed.peerPathId, this.#peerPathId) ||
      installed.laneId !== laneId ||
      installed.laneEpoch !== laneEpoch
    ) return
    this.#installedPeers.delete(encodeBase64Url(installed.peerPathId.copyBytes()))
    this.#peers.path(installed.peerPathId)?.peerDetached({
      attemptId: installed.attemptId,
      protocolSessionId: installed.protocolSessionId,
      peerPathId: installed.peerPathId,
      laneId,
      laneEpoch,
    })
    this.#startInstalledPeerCleanup(installed)
  }

  #releasePeer(laneId: number, laneEpoch: number): void {
    const installed = [...this.#installedPeers.values()].find((peer) =>
      peer.laneId === laneId && peer.laneEpoch === laneEpoch)
    if (installed === undefined) return
    this.#removeAdmittedLane(laneId, laneEpoch)
    this.#installedPeers.delete(encodeBase64Url(installed.peerPathId.copyBytes()))
    this.#startInstalledPeerCleanup(installed)
  }

  #startInstalledPeerCleanup(installed: V2InstalledPeer): void {
    if (installed.closeTask !== undefined) return
    installed.unsubscribePathRoute?.()
    const closeTask = Promise.allSettled([
      Promise.resolve().then(() => installed.peer.close()),
      Promise.resolve().then(() => installed.route.close()),
    ]).then(() => {
      this.#peerCleanupTasks.delete(installed)
    })
    installed.closeTask = closeTask
    this.#peerCleanupTasks.set(installed, closeTask)
  }

  async #joinPeerCleanup(): Promise<void> {
    while (this.#peerCleanupTasks.size > 0) {
      await Promise.allSettled([...this.#peerCleanupTasks.values()])
    }
  }

  #admit(laneId: number, laneEpoch: number, route: V2BlockTransportRoute): boolean {
    const existing = this.#admitted.get(laneId)
    if (existing !== undefined) {
      return existing.laneEpoch === laneEpoch && existing.route === route
    }
    if (!this.#session.laneIds().includes(laneId)) return false
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
    const admitted = this.#admitted.get(laneId)
    if (admitted === undefined || admitted.laneEpoch !== laneEpoch) return
    this.#admitted.delete(laneId)
    this.#lanes.remove(laneId)
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
