import {
  V2LaneSet,
  type V2BlockLane,
} from '../content/v2-lane-set'
import type {
  V2BlockRouteEligibility,
  V2BlockTransportRoute,
} from '../content/v2-route-policy'
import { encodeBase64Url } from '../crypto/bytes'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type { PeerChannel } from './peer-channel'
import {
  browserPeerConnectionAvailable,
  BrowserOfferChannelFactory,
  type OfferChannelFactory,
} from './peer-offer'
import {
  createV2PeerPathIdentity,
  type V2ConnectivityObserver,
  type V2SessionSignalingObserver,
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
} from './v2-peer-recovery'

export type { V2ConnectivityObserver } from './v2-session-signaling'

export interface V2ContentLaneAdmissionObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: V2BlockTransportRoute
}

export type V2ContentLaneDetachmentObservation = V2ContentLaneAdmissionObservation

export interface V2ReceiverConnectivityOptions {
  readonly session: V2ReceiverSessionRuntime
  readonly lanes: V2LaneSet
  readonly createBlockLane: (laneId: number) => V2BlockLane
  readonly relayLaneId?: number
  readonly offers?: OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly nativePeerUsable?: () => boolean
  readonly connectivityObserver?: V2ConnectivityObserver
  readonly now?: () => number
  readonly onPeerError?: (error: unknown) => void
  readonly onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
  readonly observePeerSignaling?: V2SessionSignalingObserver
  readonly peerRecovery?: V2PeerRecoveryDependencies
}

export type V2ContentIntent = 'preview' | 'download'

export interface V2ConnectivityActivation {
  readonly routes: V2BlockRouteEligibility
  close(): void
}

/** Stable click-scoped authority shared by every ProtocolSession generation it spans. */
export class V2ConnectivityRouteAuthority implements V2BlockRouteEligibility {
  readonly #listeners = new Set<() => void>()
  #active = true
  #closedReason: unknown

  get active(): boolean {
    return this.#active
  }

  allows(): boolean {
    return this.#active
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
  readonly protocolSessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly laneId: number
  readonly laneEpoch: number
  readonly peer: PeerChannel
  readonly route: V2SessionSignalingRoute
  closeTask?: Promise<void>
}

/** Browsing keeps relay control-only; explicit content activation admits both routes. */
export class V2ReceiverConnectivity {
  readonly #session: V2ReceiverSessionRuntime
  readonly #lanes: V2LaneSet
  readonly #createBlockLane: (laneId: number) => V2BlockLane
  readonly #offers: OfferChannelFactory
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #nativePeerUsable: () => boolean
  readonly #connectivityObserver: V2ConnectivityObserver | undefined
  readonly #now: () => number
  readonly #onPeerError: (error: unknown) => void
  readonly #onContentLaneAdmitted: (
    observation: V2ContentLaneAdmissionObservation,
  ) => void
  readonly #onContentLaneDetached: (
    observation: V2ContentLaneDetachmentObservation,
  ) => void
  readonly #observePeerSignaling: V2SessionSignalingObserver
  readonly #peerRecoveryOptions: V2PeerRecoveryDependencies
  readonly #protocolSessionId: string
  readonly #lifetime = new AbortController()
  readonly #admitted = new Map<number, { readonly laneEpoch: number; readonly route: V2BlockTransportRoute }>()
  readonly #laneEpochs = new Map<number, number>()
  readonly #activations = new Map<number, V2ActiveConnectivity>()
  readonly #peerCleanupTasks = new Map<V2InstalledPeer, Promise<void>>()
  readonly #unsubscribeLaneChanges: () => void
  #peerRecovery: V2PeerRecoverySupervisor | undefined
  #peerPathId: string | undefined
  #installedPeer: V2InstalledPeer | undefined
  #relayLaneId: number
  #closeTask: Promise<void> | undefined
  #nextActivation = 1

  constructor(options: V2ReceiverConnectivityOptions) {
    this.#session = options.session
    this.#lanes = options.lanes
    this.#createBlockLane = options.createBlockLane
    this.#relayLaneId = options.relayLaneId ?? options.session.initialLaneId
    this.#offers = options.offers ?? new BrowserOfferChannelFactory()
    this.#randomBytes = options.randomBytes
    this.#nativePeerUsable = options.nativePeerUsable ?? (() => browserPeerConnectionAvailable())
    this.#connectivityObserver = options.connectivityObserver
    this.#now = options.now ?? (() => performance.now())
    this.#onPeerError = options.onPeerError ?? (() => undefined)
    this.#onContentLaneAdmitted = options.onContentLaneAdmitted ?? (() => undefined)
    this.#onContentLaneDetached = options.onContentLaneDetached ?? (() => undefined)
    this.#observePeerSignaling = options.observePeerSignaling ?? (() => undefined)
    this.#peerRecoveryOptions = options.peerRecovery ?? {}
    this.#protocolSessionId = encodeBase64Url(options.session.keys.protocolSessionId)
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

  begin(
    _intent: V2ContentIntent,
    options: {
      readonly routeAuthority?: V2ConnectivityRouteAuthority
    } = {},
  ): V2ConnectivityActivation {
    this.#lifetime.signal.throwIfAborted()
    const id = this.#nextActivation++
    const routes = options.routeAuthority ?? new V2ConnectivityRouteAuthority()
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

  replaceRelayLane(laneId: number): void {
    if (!this.#session.laneIds().includes(laneId)) {
      throw new Error('Replacement relay lane is not attached to this ProtocolSession')
    }
    const previous = this.#relayLaneId
    this.#relayLaneId = laneId
    if (previous !== laneId && this.#admitted.has(previous)) {
      const admitted = this.#admitted.get(previous)
      if (admitted !== undefined) this.#removeAdmittedLane(previous, admitted.laneEpoch)
    }
    if (this.#activations.size > 0) this.#admitRelay()
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    const reason = new DOMException('Receiver connectivity closed', 'AbortError')
    this.#lifetime.abort(reason)
    const recoveryClose = this.#peerRecovery?.close()
    for (const activationId of [...this.#activations.keys()]) {
      this.#endActivation(activationId, reason)
    }
    this.#unsubscribeLaneChanges()
    const installed = this.#installedPeer
    this.#installedPeer = undefined
    if (installed !== undefined) this.#startInstalledPeerCleanup(installed)
    await Promise.allSettled([
      ...(recoveryClose === undefined ? [] : [recoveryClose]),
      this.#joinPeerCleanup(),
    ])
  }

  #activatePeerRecovery(): V2PeerRecoveryActivation | undefined {
    if (!this.#capabilityAllowsAttempt()) return undefined
    return this.#ensurePeerRecovery().activate()
  }

  #ensurePeerRecovery(): V2PeerRecoverySupervisor {
    if (this.#peerRecovery !== undefined) return this.#peerRecovery
    const peerPathIdentity = this.#randomBytes === undefined
      ? createV2PeerPathIdentity()
      : createV2PeerPathIdentity(this.#randomBytes)
    const peerPathId = encodeBase64Url(peerPathIdentity)
    const clock = this.#peerRecoveryOptions.clock ?? browserV2PeerRecoveryClock
    const attempts = new V2BrowserPeerAttemptExecutor({
      session: this.#session,
      offers: this.#offers,
      peerPathIdentity,
      clock,
      publish: (candidate) => this.#publishPeer(candidate),
      ...(this.#randomBytes === undefined ? {} : { randomBytes: this.#randomBytes }),
      ...(this.#connectivityObserver === undefined
        ? {}
        : { connectivityObserver: this.#connectivityObserver }),
      observePeerSignaling: this.#observePeerSignaling,
      ...(this.#peerRecoveryOptions.observeAttempt === undefined
        ? {}
        : { observeAttempt: this.#peerRecoveryOptions.observeAttempt }),
      now: this.#now,
      onFailure: (error) => this.#reportPeerError(error),
    })
    this.#peerPathId = peerPathId
    this.#peerRecovery = new V2PeerRecoverySupervisor({
      protocolSessionId: this.#protocolSessionId,
      peerPathId,
      attempts,
      ...(this.#peerRecoveryOptions.policy === undefined
        ? {}
        : { policy: this.#peerRecoveryOptions.policy }),
      clock,
      ...(this.#peerRecoveryOptions.random === undefined
        ? {}
        : { random: this.#peerRecoveryOptions.random }),
      rearmSource: this.#peerRecoveryOptions.rearmSource ?? new BrowserV2PeerRecoveryRearmSource(),
      ...(this.#peerRecoveryOptions.observer === undefined
        ? {}
        : { observer: this.#peerRecoveryOptions.observer }),
    })
    return this.#peerRecovery
  }

  #capabilityAllowsAttempt(): boolean {
    try {
      if (this.#nativePeerUsable()) return true
      // A caller may know more than constructor presence (for example, a local
      // ICE/DataChannel probe), but that probe never owns relay eligibility.
    } catch (error) {
      this.#reportPeerError(error)
    }
    return false
  }

  #publishPeer(candidate: V2PeerCandidatePublication): boolean {
    if (
      this.#lifetime.signal.aborted || this.#installedPeer !== undefined ||
      candidate.protocolSessionId !== this.#protocolSessionId ||
      candidate.peerPathId !== this.#peerPathId ||
      this.#laneEpochs.get(candidate.laneId) !== candidate.laneEpoch ||
      !this.#session.laneIds().includes(candidate.laneId)
    ) return false
    if (!this.#admit(candidate.laneId, candidate.laneEpoch, 'peer')) return false
    this.#installedPeer = { ...candidate }
    return true
  }

  #reportPeerError(error: unknown): void {
    try {
      this.#onPeerError(error)
    } catch {
      // Failure diagnostics cannot reject the owned attempt task after cleanup.
    }
  }


  #endActivation(
    id: number,
    reason: unknown = new DOMException('Content activation closed', 'AbortError'),
  ): void {
    const active = this.#activations.get(id)
    if (active === undefined) return
    this.#activations.delete(id)
    active.peerRecovery?.close()
    if (active.ownsRoutes) active.routes.close(reason)
  }

  #peerDetached(laneId: number, laneEpoch: number): void {
    const installed = this.#installedPeer
    if (
      installed === undefined || installed.protocolSessionId !== this.#protocolSessionId ||
      installed.peerPathId !== this.#peerPathId || installed.laneId !== laneId ||
      installed.laneEpoch !== laneEpoch
    ) return
    this.#installedPeer = undefined
    this.#peerRecovery?.peerDetached({
      protocolSessionId: installed.protocolSessionId,
      peerPathId: installed.peerPathId,
      laneId,
      laneEpoch,
    })
    this.#startInstalledPeerCleanup(installed)
  }

  #startInstalledPeerCleanup(installed: V2InstalledPeer): void {
    if (installed.closeTask !== undefined) return
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
    const laneEpoch = this.#laneEpochs.get(this.#relayLaneId)
    if (laneEpoch === undefined) {
      throw new Error('Relay lane epoch is unavailable during content admission')
    }
    this.#admit(this.#relayLaneId, laneEpoch, 'relay')
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
