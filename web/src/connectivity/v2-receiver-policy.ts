import {
  V2LaneSet,
  type V2BlockLane,
  type V2BlockRouteEligibility,
  type V2BlockTransportRoute,
} from '../content/v2-broker'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type { PeerChannel } from './peer-channel'
import { BrowserOfferChannelFactory, type OfferChannelFactory } from './peer-offer'
import {
  createV2PeerBinding,
  type V2SessionSignalingObserver,
  V2SessionSignalingRoute,
} from './v2-session-signaling'

export const V2_RELAY_CONTENT_FALLBACK_MILLISECONDS = 8_000
export const V2_P2P_CONNECT_TIMEOUT_MILLISECONDS = 10_000

export interface V2ReceiverConnectivityOptions {
  readonly session: V2ReceiverSessionRuntime
  readonly lanes: V2LaneSet
  readonly createBlockLane: (laneId: number) => V2BlockLane
  readonly relayLaneId?: number
  readonly offers?: OfferChannelFactory
  readonly randomBytes?: (length: number) => Uint8Array
  readonly onPeerError?: (error: unknown) => void
  readonly observePeerSignaling?: V2SessionSignalingObserver
}

export type V2ContentIntent = 'preview' | 'download'
export type V2ContentSizeClass = 'small' | 'large' | 'unknown'

export interface V2ConnectivityActivation {
  readonly routes: V2BlockRouteEligibility
  observeSizeClass(sizeClass: V2ContentSizeClass): void
  close(): void
}

/** Stable click-scoped authority; session generations may contribute lanes but cannot reset its t0. */
export class V2ConnectivityRouteAuthority implements V2BlockRouteEligibility {
  readonly #listeners = new Set<() => void>()
  #relay = false
  #active = true
  #closedReason: unknown

  get active(): boolean {
    return this.#active
  }

  allows(route: V2BlockTransportRoute): boolean {
    return this.#active && (route === 'peer' || (route === 'relay' && this.#relay))
  }

  assertActive(): void {
    if (this.#active) return
    throw this.#closedReason ?? new DOMException('Content activation is closed', 'AbortError')
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  admitRelay(): void {
    this.assertActive()
    if (this.#relay) return
    this.#relay = true
    this.#notify()
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
  readonly controller: AbortController
  readonly intent: V2ContentIntent
  readonly routes: V2ConnectivityRouteAuthority
  readonly ownsRoutes: boolean
  sizeClass: V2ContentSizeClass
}

/**
 * Browsing keeps the joined relay as a control lane, while content admission is
 * governed independently by the 0/8 policy. This separation prevents catalog UI
 * work from silently opting large or still-unknown selections into relay traffic.
 */
export class V2ReceiverConnectivity {
  readonly #session: V2ReceiverSessionRuntime
  readonly #lanes: V2LaneSet
  readonly #createBlockLane: (laneId: number) => V2BlockLane
  readonly #offers: OfferChannelFactory
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #onPeerError: (error: unknown) => void
  readonly #observePeerSignaling: V2SessionSignalingObserver
  readonly #lifetime = new AbortController()
  readonly #admitted = new Set<number>()
  readonly #routes = new Set<V2SessionSignalingRoute>()
  readonly #activations = new Map<number, V2ActiveConnectivity>()
  readonly #relayDemand = new Set<number>()
  readonly #fallbackTasks = new Set<Promise<void>>()
  readonly #unsubscribeLaneChanges: () => void
  #peer: PeerChannel | undefined
  #peerRoute: V2SessionSignalingRoute | undefined
  #peerLaneId: number | undefined
  #pendingPeer: PeerChannel | undefined
  #pendingPeerRoute: V2SessionSignalingRoute | undefined
  #pendingPeerLaneId: number | undefined
  #peerReconnectLaneId: number | undefined
  #peerController: AbortController | undefined
  #peerTask: Promise<void> | undefined
  #peerReconnectRequested = false
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
    this.#onPeerError = options.onPeerError ?? (() => undefined)
    this.#observePeerSignaling = options.observePeerSignaling ?? (() => undefined)
    this.#unsubscribeLaneChanges = this.#session.subscribeLaneChanges((change) => {
      if (change.type === 'detached') {
        this.#admitted.delete(change.laneId)
        this.#lanes.remove(change.laneId)
        if (change.laneId === this.#peerLaneId || change.laneId === this.#pendingPeerLaneId) {
          this.#peerDetached(change.laneId)
        }
      }
    })
  }

  begin(
    intent: V2ContentIntent,
    sizeClass: V2ContentSizeClass = 'unknown',
    options: {
      readonly relayFallbackMilliseconds?: number
      readonly routeAuthority?: V2ConnectivityRouteAuthority
    } = {},
  ): V2ConnectivityActivation {
    this.#lifetime.signal.throwIfAborted()
    const relayFallbackMilliseconds = options.relayFallbackMilliseconds ??
      V2_RELAY_CONTENT_FALLBACK_MILLISECONDS
    if (!Number.isFinite(relayFallbackMilliseconds) || relayFallbackMilliseconds < 0 ||
        relayFallbackMilliseconds > V2_RELAY_CONTENT_FALLBACK_MILLISECONDS) {
      throw new RangeError('Relay fallback delay is outside its frozen policy window')
    }
    const id = this.#nextActivation++
    const controller = new AbortController()
    const routes = options.routeAuthority ?? new V2ConnectivityRouteAuthority()
    routes.assertActive()
    this.#activations.set(id, {
      controller,
      intent,
      routes,
      ownsRoutes: options.routeAuthority === undefined,
      sizeClass,
    })
    if (routes.allows('relay') || (intent === 'download' && sizeClass === 'small')) {
      this.#requestRelay(id)
    }
    this.#ensurePeer()
    const fallback = this.#admitRelayAfterDelay(
      id,
      controller.signal,
      relayFallbackMilliseconds,
    )
      .finally(() => this.#fallbackTasks.delete(fallback))
    this.#fallbackTasks.add(fallback)
    let closed = false
    return Object.freeze({
      routes,
      observeSizeClass: (observed: V2ContentSizeClass) => {
        if (closed || intent !== 'download' || observed !== 'small') return
        const active = this.#activations.get(id)
        if (active !== undefined) active.sizeClass = observed
        this.#requestRelay(id)
      },
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
      this.#admitted.delete(previous)
      this.#lanes.remove(previous)
    }
    if (this.#relayDemand.size > 0) this.#admit(laneId, 'relay')
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    const reason = new DOMException('Receiver connectivity closed', 'AbortError')
    this.#lifetime.abort(reason)
    for (const activationId of [...this.#activations.keys()]) {
      this.#endActivation(activationId, reason)
    }
    this.#relayDemand.clear()
    this.#peerController?.abort(new DOMException('Receiver connectivity closed', 'AbortError'))
    this.#unsubscribeLaneChanges()
    await Promise.allSettled([...this.#routes].map((route) => route.close()))
    this.#routes.clear()
    await this.#peer?.close().catch(() => undefined)
    await Promise.allSettled([
      ...(this.#peerTask === undefined ? [] : [this.#peerTask]),
      ...this.#fallbackTasks,
    ])
  }

  #ensurePeer(): void {
    if (this.#peerTask !== undefined) {
      if (this.#peerController?.signal.aborted && this.#activations.size > 0 &&
          !this.#lifetime.signal.aborted) this.#peerReconnectRequested = true
      return
    }
    if (this.#peer !== undefined || this.#activations.size === 0 || this.#lifetime.signal.aborted) return
    this.#peerReconnectRequested = false
    const controller = new AbortController()
    this.#peerController = controller
    const task = this.#connectPeer(controller.signal)
      .finally(() => {
        if (this.#peerTask === task) this.#peerTask = undefined
        if (this.#peerController === controller) this.#peerController = undefined
        if (this.#peerReconnectRequested) this.#ensurePeer()
      })
    this.#peerTask = task
  }

  async #connectPeer(signal: AbortSignal): Promise<void> {
    const attempt = peerAttemptDeadline(signal)
    let route: V2SessionSignalingRoute | undefined
    let peer: PeerChannel | undefined
    try {
      const binding = this.#randomBytes === undefined
        ? createV2PeerBinding()
        : createV2PeerBinding(this.#randomBytes)
      route = new V2SessionSignalingRoute(this.#session, binding, this.#observePeerSignaling)
      this.#routes.add(route)
      peer = await this.#offers.offer(route, attempt.signal)
      attempt.signal.throwIfAborted()
      const grant = await this.#session.requestLaneGrant(
        this.#peerReconnectLaneId ?? 0,
        { signal: attempt.signal },
      )
      this.#pendingPeer = peer
      this.#pendingPeerRoute = route
      this.#pendingPeerLaneId = grant.laneId
      await this.#session.attachGrantedLane(peer, grant, attempt.signal)
      if (
        this.#pendingPeer !== peer ||
        this.#pendingPeerLaneId !== grant.laneId ||
        !this.#session.laneIds().includes(grant.laneId)
      ) {
        throw new Error('Peer lane detached during admission')
      }
      this.#peer = peer
      this.#peerRoute = route
      this.#peerLaneId = grant.laneId
      this.#peerReconnectLaneId = undefined
      this.#pendingPeer = undefined
      this.#pendingPeerRoute = undefined
      this.#pendingPeerLaneId = undefined
      this.#admit(grant.laneId, 'peer')
    } catch (error) {
      if (this.#pendingPeer === peer) {
        this.#pendingPeer = undefined
        this.#pendingPeerRoute = undefined
        this.#pendingPeerLaneId = undefined
      }
      await peer?.close().catch(() => undefined)
      await route?.close().catch(() => undefined)
      if (route !== undefined) this.#routes.delete(route)
      if (!signal.aborted) {
        if (this.#activations.size > 0) this.#requestRelayForAll()
        this.#onPeerError(error)
      }
    } finally {
      attempt.close()
    }
  }

  async #admitRelayAfterDelay(
    id: number,
    signal: AbortSignal,
    delayMilliseconds: number,
  ): Promise<void> {
    try {
      await delay(delayMilliseconds, signal)
      this.#requestRelay(id)
    } catch (error) {
      if (!signal.aborted) throw error
    }
  }

  #endActivation(
    id: number,
    reason: unknown = new DOMException('Content activation closed', 'AbortError'),
  ): void {
    const active = this.#activations.get(id)
    if (active === undefined) return
    this.#activations.delete(id)
    this.#relayDemand.delete(id)
    active.controller.abort(reason)
    if (active.ownsRoutes) active.routes.close(reason)
    if (this.#activations.size === 0 && this.#peer === undefined) {
      this.#peerController?.abort(new DOMException('Last content activation closed', 'AbortError'))
    }
  }

  #peerDetached(laneId: number): void {
    const peer = this.#peer ?? this.#pendingPeer
    const route = this.#peerRoute ?? this.#pendingPeerRoute
    this.#peer = undefined
    this.#peerRoute = undefined
    this.#peerLaneId = undefined
    this.#pendingPeer = undefined
    this.#pendingPeerRoute = undefined
    this.#pendingPeerLaneId = undefined
    this.#peerReconnectLaneId = laneId
    peer?.close().catch(() => undefined)
    if (route !== undefined) {
      this.#routes.delete(route)
      route.close().catch(() => undefined)
    }
    if (this.#activations.size > 0) {
      this.#requestRelayForAll()
      // Detachment can arrive in the final microtask of attachment; remember the
      // replacement intent until the completing attempt releases its task slot.
      this.#peerReconnectRequested = true
      this.#ensurePeer()
    }
  }

  #admit(laneId: number, route: V2BlockTransportRoute): void {
    if (this.#admitted.has(laneId) || !this.#session.laneIds().includes(laneId)) return
    this.#lanes.add(this.#createBlockLane(laneId), route)
    this.#admitted.add(laneId)
  }

  #requestRelay(activationId: number): void {
    const active = this.#activations.get(activationId)
    if (active === undefined) return
    active.routes.admitRelay()
    this.#relayDemand.add(activationId)
    this.#admitRelay()
  }

  #requestRelayForAll(): void {
    for (const activationId of this.#activations.keys()) this.#requestRelay(activationId)
  }

  #admitRelay(): void {
    this.#admit(this.#relayLaneId, 'relay')
  }
}

function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, milliseconds)
    const abort = () => {
      clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Connectivity timer aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}

function peerAttemptDeadline(parent: AbortSignal): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent.reason ?? new DOMException('Peer attempt aborted', 'AbortError'),
  )
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(new DOMException('Peer connection attempt timed out', 'TimeoutError'))
  }, V2_P2P_CONNECT_TIMEOUT_MILLISECONDS)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent.removeEventListener('abort', abort)
    },
  }
}
