import {
  createWindShareFrameChannel,
  type WebRTCFrameChannel,
} from '../transport/webrtc'
import { MAXIMUM_PEER_CANDIDATES } from '../session/operation-continuation'
import { abortReason } from './clock'
import type { CandidateCounts } from './diagnostics'
import { NegotiationEventQueue } from './negotiation-event-queue'
import {
  PeerNegotiationError,
  UnexpectedDataChannelError,
} from './errors'
import {
  OwnedPeerChannel,
  type PeerChannel,
  type PeerPathRoute,
  type PeerOwnerFailure,
} from './peer-channel'
import {
  awaitWithAbort,
  RemoteNegotiationState,
} from './peer-offer-remote'
import {
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
  type SignalingRoute,
} from './signaling'
import { BrowserPeerRoute } from './peer-route/route'
import { CandidateBudget } from './ice-policy/candidates'
import type { AttemptICEProfile } from './ice-policy/endpoints'
import { candidateFact, selectedPairFact, type PeerProviderFact } from './peer-set/provider-facts'
import { BrowserPeerConnections, closeBrowserPeer, type BrowserPeerConnection } from './browser-peer/connections'

export const DEFAULT_STUN_SERVER = 'stun:stun.l.google.com:19302'
export const MAX_ICE_CANDIDATES_PER_PEER = MAXIMUM_PEER_CANDIDATES

// Lifecycle and description events need their own reserve because candidate limits
// alone must not let a candidate burst hide cancellation or terminal settlement.
const NEGOTIATION_EVENT_RESERVE = 16
const SELECTED_PAIR_SAMPLE_MILLISECONDS = 5_000
const PROVIDER_OBSERVATION_CAPACITY = 64

export interface OfferChannelFactory {
  offer(
    route: SignalingRoute,
    signal: AbortSignal,
    observer?: PeerOfferAttemptObserver,
    profile?: AttemptICEProfile,
  ): Promise<PeerChannel>
}

/** Consumer-side milestone port; implementations never receive attempt authority. */
export interface PeerOfferAttemptObserver {
  providerFact?(fact: PeerProviderFact): void
  phaseChanged?(phase: 'signaling' | 'ice-check' | 'dtls-datachannel'): void
  offerCreated(candidateCounts: CandidateCounts | (() => CandidateCounts)): void
  offerSent(candidateCounts: CandidateCounts | (() => CandidateCounts)): void
  answerReceived(candidateCounts: CandidateCounts | (() => CandidateCounts)): void
  dataChannelOpened(
    candidateCounts: CandidateCounts | (() => CandidateCounts),
    ...unusedLegacyArguments: readonly unknown[]
  ): void
}

export interface BrowserOfferFactoryOptions {
  readonly configuration?: RTCConfiguration
  readonly peerConnections?: BrowserPeerConnections
  readonly maxCandidates?: number
}

export interface BrowserPeerConnectionEnvironment {
  readonly RTCPeerConnection?: unknown
}

/** Keeps relay fallback tied to the active runtime capability, not a browser-name allowlist. */
export function browserPeerConnectionAvailable(
  environment: BrowserPeerConnectionEnvironment = globalThis,
): boolean {
  return typeof environment.RTCPeerConnection === 'function'
}

const DEFAULT_CONFIGURATION: RTCConfiguration = {
  iceServers: [{ urls: [DEFAULT_STUN_SERVER] }],
}

export class BrowserOfferChannelFactory implements OfferChannelFactory {
  readonly #configuration: RTCConfiguration
  readonly #peerConnections: BrowserPeerConnections
  readonly #maxCandidates: number

  constructor(options: BrowserOfferFactoryOptions = {}) {
    const maximum = options.maxCandidates ?? MAX_ICE_CANDIDATES_PER_PEER
    if (
      !Number.isSafeInteger(maximum) || maximum <= 0 ||
      maximum > MAX_ICE_CANDIDATES_PER_PEER
    ) {
      throw new RangeError(
        `maximum ICE candidates must be between 1 and ${MAX_ICE_CANDIDATES_PER_PEER}`,
      )
    }
    this.#configuration = cloneConfiguration(options.configuration ?? DEFAULT_CONFIGURATION)
    this.#peerConnections = options.peerConnections ?? new BrowserPeerConnections()
    this.#maxCandidates = maximum
  }

  async offer(
    route: SignalingRoute,
    signal: AbortSignal,
    observer?: PeerOfferAttemptObserver,
    profile?: AttemptICEProfile,
  ): Promise<PeerChannel> {
    signal.throwIfAborted()
    let connection: BrowserPeerConnection
    try {
      const configuration = cloneConfiguration(this.#configuration)
      if (profile !== undefined) {
        const trustedTurn = (configuration.iceServers ?? []).flatMap((server) => {
          const urls = (typeof server.urls === 'string' ? [server.urls] : server.urls)
            .filter((url) => /^turns?:/i.test(url))
          return urls.length === 0 ? [] : [{ ...server, urls }]
        })
        configuration.iceServers = [...trustedTurn,
          ...(configuration.iceTransportPolicy === 'relay' || profile.urls.length === 0 ? [] : [{ urls: [...profile.urls] }])]
      }
      connection = this.#peerConnections.create(configuration, (fact) => observer?.providerFact?.(fact))
    } catch (cause) {
      throw new PeerNegotiationError('could not create the PeerConnection', { cause })
    }
    let channel: WebRTCFrameChannel
    try {
      channel = createWindShareFrameChannel(connection.peer)
    } catch (error) {
      closeBrowserPeer(connection.peer)
      throw error
    }
    const negotiation = new OfferNegotiation(
      connection,
      channel,
      route,
      this.#maxCandidates,
      observer,
    )
    negotiation.run(signal).catch(() => undefined)
    return negotiation.opened
  }
}

type NegotiationEvent =
  | { readonly type: 'channel-done' }
  | { readonly type: 'channel-opened' }
  | { readonly type: 'path-changed' }
  | { readonly type: 'failure'; readonly reason: unknown }
  | { readonly type: 'local-candidate'; readonly candidate: RTCIceCandidateInit }
  | { readonly type: 'route-closed'; readonly reason?: unknown }
  | { readonly type: 'signal'; readonly signal: ConnectivitySignal }

class OfferNegotiation {
  readonly #connection: BrowserPeerConnection
  readonly #peer: RTCPeerConnection
  readonly #channel: WebRTCFrameChannel
  readonly #route: SignalingRoute
  readonly #candidateBudget: CandidateBudget
  readonly #path: BrowserPeerRoute
  readonly #routeListeners = new Set<(route: PeerPathRoute) => void>()
  #selectedPairID: string | undefined
  #selectedAt = 0
  #statsPending: Promise<void> | undefined
  #statsTimer: ReturnType<typeof globalThis.setInterval> | undefined
  readonly #startedAt = performance.now()
  readonly #providerFacts: PeerProviderFact[] = []
  #providerFactsScheduled = false
  #providerFactsLost = 0
  readonly #observer: PeerOfferAttemptObserver | undefined
  readonly #remote: RemoteNegotiationState
  readonly #events: NegotiationEventQueue<NegotiationEvent>
  readonly #opened = deferred<PeerChannel>()
  readonly #settled = deferred<void>()
  readonly #interruption = new AbortController()
  readonly #ownedChannel: OwnedPeerChannel
  #ownerFailure: PeerOwnerFailure | undefined
  #reader: ReadableStreamDefaultReader<ConnectivitySignal> | undefined
  #readerTask: Promise<void> | undefined
  #openedChannel = false
  #signalingAvailable = true
  readonly #localCandidateFingerprints = new Set<string>()
  readonly #candidateStop = new AbortController()
  #candidateTask: Promise<void> = Promise.resolve()
  #localCandidateFailureReported = false
  #parentClosed = false

  constructor(
    connection: BrowserPeerConnection,
    channel: WebRTCFrameChannel,
    route: SignalingRoute,
    maximumCandidates: number,
    observer: PeerOfferAttemptObserver | undefined,
  ) {
    this.#connection = connection
    this.#peer = connection.peer
    this.#channel = channel
    this.#route = route
    this.#candidateBudget = new CandidateBudget(maximumCandidates)
    this.#observer = observer
    this.#path = new BrowserPeerRoute(() => this.#peer.sctp?.transport?.iceTransport, (route) => this.#pathChanged(route))
    this.#remote = new RemoteNegotiationState(connection, maximumCandidates)
    this.#ownedChannel = new OwnedPeerChannel(
      channel,
      this.#settled.promise,
      () => this.#ownerFailure,
      () => this.#path.current,
      (listener) => { this.#routeListeners.add(listener); return () => this.#routeListeners.delete(listener) },
    )
    this.#events = new NegotiationEventQueue<NegotiationEvent>(
      maximumCandidates * 2 + NEGOTIATION_EVENT_RESERVE,
      () => ({
        type: 'failure',
        reason: new PeerNegotiationError('negotiation event queue is full'),
      }),
      (event) => {
        if (event.type === 'failure') {
          this.#interrupt(event.reason)
        }
      },
    )
  }

  get opened(): Promise<PeerChannel> {
    return this.#opened.promise
  }

  async run(callerSignal: AbortSignal): Promise<void> {
    const aborted = () => this.#interrupt(abortReason(callerSignal))
    callerSignal.addEventListener('abort', aborted, { once: true })
    this.#peer.addEventListener('icecandidate', this.#onIceCandidate)
    this.#peer.addEventListener('connectionstatechange', this.#onConnectionStateChange)
    this.#peer.addEventListener('iceconnectionstatechange', this.#onIceStateChange)
    this.#peer.addEventListener('icecandidateerror', this.#onIceError)
    this.#peer.addEventListener('datachannel', this.#onUnexpectedDataChannel)
    this.#channel.opened.then(
      () => this.#events.push({ type: 'channel-opened' }),
      (reason: unknown) => this.#interrupt(reason),
    )
    this.#channel.done.then(() => {
      if (this.#channel.reason !== undefined) {
        this.#interrupt(this.#channel.reason)
        return
      }
      this.#events.push({ type: 'channel-done' })
    })

    try {
      if (callerSignal.aborted) {
        aborted()
      }
      const operationSignal = this.#interruption.signal
      operationSignal.throwIfAborted()
      try {
        this.#reader = this.#route.messages.getReader()
      } catch (cause) {
        throw new PeerNegotiationError('could not acquire the signaling route', { cause })
      }
      this.#readerTask = this.#readSignals(this.#reader)
      await this.#createAndSendOffer(operationSignal)
      await this.#processEvents(operationSignal)
    } catch (error) {
      const failure = this.#recordOwnerFailure(error)
      this.#opened.reject(failure.reason)
    } finally {
      callerSignal.removeEventListener('abort', aborted)
      this.#peer.removeEventListener('icecandidate', this.#onIceCandidate)
      this.#peer.removeEventListener('connectionstatechange', this.#onConnectionStateChange)
      this.#peer.removeEventListener('iceconnectionstatechange', this.#onIceStateChange)
      this.#peer.removeEventListener('icecandidateerror', this.#onIceError)
      globalThis.clearInterval(this.#statsTimer)
      this.#peer.removeEventListener('datachannel', this.#onUnexpectedDataChannel)
      this.#candidateStop.abort()
      await this.#candidateTask
      this.#events.close()
      this.#path.close()
      this.#routeListeners.clear()
      this.#localCandidateFingerprints.clear()
      // Parent-first teardown prevents a terminal-pending DataChannel from
      // pinning cancellation or a fatal signaling violation.
      this.#closeParent()
      await this.#reader?.cancel(this.#ownerFailure?.reason).catch(() => undefined)
      await this.#readerTask?.catch(() => undefined)
      try {
        this.#reader?.releaseLock()
      } catch {
        // Parent and DataChannel cleanup still own settlement if a custom stream
        // violates the reader-release contract during teardown.
      }
      await this.#channel.close().catch(() => undefined)
      this.#settled.resolve(undefined)
    }
  }

  async #createAndSendOffer(signal: AbortSignal): Promise<void> {
    let offer: RTCSessionDescriptionInit
    let operation: 'create-offer' | 'set-local-description' = 'create-offer'
    try {
      offer = await awaitWithAbort(this.#peer.createOffer(), signal)
      operation = 'set-local-description'
      await awaitWithAbort(this.#peer.setLocalDescription(offer), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      this.#connection.descriptionFailed(operation, cause)
      throw new PeerNegotiationError('could not create the local offer', { cause })
    }
    const local = this.#peer.localDescription
    if (local === null || local.type !== SIGNAL_KIND_OFFER || local.sdp === '') {
      throw new PeerNegotiationError('local offer description is unavailable')
    }
    this.#observe((observer) => observer.offerCreated(() => this.#candidateCounts()))
    await this.#send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: local.type, sdp: local.sdp },
    }, signal)
    this.#observe((observer) => observer.offerSent(() => this.#candidateCounts()))
  }

  async #processEvents(signal: AbortSignal): Promise<void> {
    while (true) {
      const event = await this.#events.next()
      if (await this.#handleEvent(event, signal) === 'done') {
        return
      }
    }
  }

  async #handleEvent(
    event: NegotiationEvent,
    signal: AbortSignal,
  ): Promise<'continue' | 'done'> {
    if (event.type === 'failure') {
      throw event.reason
    }
    signal.throwIfAborted()
    if (event.type === 'channel-done') {
      if (!this.#openedChannel) {
        throw this.#channel.reason ?? new PeerNegotiationError('DataChannel closed before Open')
      }
      return 'done'
    }
    if (event.type === 'path-changed') {
      this.#publishOpenedChannel()
      return 'continue'
    }
    if (event.type === 'channel-opened') {
      this.#path.refresh()
      this.#sampleStats().catch(() => undefined)
      this.#statsTimer ??= globalThis.setInterval(() => {
        this.#sampleStats().catch(() => undefined)
      }, SELECTED_PAIR_SAMPLE_MILLISECONDS)
      this.#publishOpenedChannel()
      return 'continue'
    }
    if (event.type === 'route-closed') {
      this.#handleRouteClosed(event.reason)
      return 'continue'
    }
    if (event.type === 'local-candidate') {
      if (this.#signalingAvailable) {
        this.#queueCandidate(event.candidate, signal)
      }
      return 'continue'
    }
    if (this.#signalingAvailable) {
      const accepted = await this.#remote.accept(event.signal, signal)
      if (accepted === 'answer') {
        this.#observe((observer) => observer.answerReceived(() => this.#candidateCounts()))
        this.#observe((observer) => observer.phaseChanged?.('ice-check'))
      }
    }
    return 'continue'
  }

  #publishOpenedChannel(): void {
    if (!this.#openedChannel && this.#channel.state === 'open' && this.#path.current !== undefined) {
      this.#openedChannel = true
      this.#observe((observer) => observer.dataChannelOpened(() => this.#candidateCounts()))
      this.#opened.resolve(this.#ownedChannel)
    }
  }

  #candidateCounts(): CandidateCounts {
    return Object.freeze({
      localEmitted: this.#localCandidateFingerprints.size,
      remoteAccepted: this.#remote.acceptedCandidates,
    })
  }

  #observe(report: (observer: PeerOfferAttemptObserver) => void): void {
    if (this.#observer === undefined) return
    try {
      report(this.#observer)
    } catch {
      // Evidence is a consumer diagnostic and cannot become negotiation authority.
    }
  }

  #handleRouteClosed(reason: unknown): void {
    if (!this.#reconcileOpenedChannel()) {
      throw new PeerNegotiationError('signaling closed before DataChannel Open', { cause: reason })
    }
    this.#disableSignaling()
  }

  #queueCandidate(candidate: RTCIceCandidateInit, parent: AbortSignal): void {
    // The lifetime candidate budget bounds this serial chain. Relay backpressure
    // must not keep answer, Open, or teardown events behind an outbound send.
    const signal = AbortSignal.any([parent, this.#candidateStop.signal])
    this.#candidateTask = this.#candidateTask.then(async () => {
      if (!this.#signalingAvailable || signal.aborted) return
      await this.#sendCandidate(candidate, signal)
    }).catch((reason: unknown) => {
      if (!this.#candidateStop.signal.aborted) this.#interrupt(reason)
    })
  }

  async #sendCandidate(candidate: RTCIceCandidateInit, signal: AbortSignal): Promise<void> {
    try {
      await this.#send({ kind: SIGNAL_KIND_CANDIDATE, payload: candidate }, signal)
    } catch (error) {
      if (signal.aborted || !this.#reconcileOpenedChannel()) {
        throw error
      }
      // Relay signaling is optional after Open; the established SCTP path owns
      // its remaining lifetime independently.
      this.#disableSignaling()
    }
  }

  async #send(message: ConnectivitySignal, signal: AbortSignal): Promise<void> {
    try {
      await awaitWithAbort(this.#route.send(message, signal), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError(`could not send ${message.kind}`, { cause })
    }
  }

  #disableSignaling(): void {
    if (!this.#signalingAvailable) {
      return
    }
    this.#signalingAvailable = false
    this.#candidateStop.abort()
    this.#reader?.cancel().catch(() => undefined)
  }

  #reconcileOpenedChannel(): boolean {
    if (!this.#openedChannel && this.#channel.state === 'open') {
      this.#path.refresh()
      this.#publishOpenedChannel()
    }
    return this.#openedChannel || this.#channel.state === 'open'
  }

  async #readSignals(
    reader: ReadableStreamDefaultReader<ConnectivitySignal>,
  ): Promise<void> {
    try {
      while (true) {
        const result = await reader.read()
        if (result.done) {
          this.#events.push({ type: 'route-closed' })
          return
        }
        this.#events.push({ type: 'signal', signal: structuredClone(result.value) })
      }
    } catch (reason) {
      this.#events.push({ type: 'route-closed', reason })
    }
  }

  #interrupt(reason: unknown): void {
    if (this.#ownerFailure !== undefined) {
      return
    }
    const failure = this.#recordOwnerFailure(reason)
    // Fatal callbacks must own teardown immediately; serializing this behind a
    // pending browser promise would let later cancellation replace the real cause.
    this.#closeParent()
    this.#interruption.abort(failure.reason)
    this.#events.push({ type: 'failure', reason: failure.reason })
  }

  #recordOwnerFailure(reason: unknown): PeerOwnerFailure {
    this.#ownerFailure ??= { reason: normalizeOwnerFailure(reason) }
    return this.#ownerFailure
  }

  #closeParent(): void {
    if (!this.#parentClosed) {
      this.#parentClosed = true
      closeBrowserPeer(this.#peer)
    }
  }

  #onIceCandidate = (event: RTCPeerConnectionIceEvent): void => {
    if (event.candidate === null || this.#localCandidateFailureReported) {
      return
    }
    try {
      const candidate = candidateForSignaling(event.candidate.toJSON())
      // Firefox emits a non-null, empty candidate before the legacy null event.
      // Pion reports the same end-of-candidates marker as nil, so both adapters
      // keep gathering completion local instead of consuming wire candidate quota.
      if (candidate === undefined) return
      const decision = this.#candidateBudget.accept(candidate.candidate ?? '')
      this.#fact(candidateFact(candidate.candidate ?? '', decision.reason))
      if (!decision.accepted) return
      this.#localCandidateFingerprints.add(candidate.candidate ?? '')
      this.#events.push({ type: 'local-candidate', candidate })
    } catch (cause) {
      this.#localCandidateFailureReported = true
      this.#interrupt(
        new PeerNegotiationError('could not encode a local ICE candidate', { cause }),
      )
    }
  }

  #onConnectionStateChange = (): void => {
    this.#path.refresh()
    this.#sampleStats().catch(() => undefined)
    if (this.#peer.connectionState === 'failed') {
      this.#interrupt(new PeerNegotiationError('PeerConnection entered failed state'))
    }
  }

  #onIceStateChange = (): void => {
    const state = this.#peer.iceConnectionState
    this.#path.refresh()
    const phase = state === 'connected' || state === 'completed' ? 'dtls-datachannel' : 'ice-check'
    this.#fact({ kind: 'state', phase, state, elapsedMs: performance.now() - this.#startedAt })
    if (state === 'checking' || state === 'connected' || state === 'completed') this.#observer?.phaseChanged?.(phase)
    this.#sampleStats().catch(() => undefined)
  }

  #onIceError = (event: RTCPeerConnectionIceErrorEvent): void => {
    this.#fact({ kind: 'ice-error', code: event.errorCode, endpoint: event.url?.slice(0, 256) ?? 'unknown' })
  }

  #fact(fact: PeerProviderFact): void {
    if (this.#providerFacts.length < PROVIDER_OBSERVATION_CAPACITY) this.#providerFacts.push(fact)
    else this.#providerFactsLost += 1
    if (this.#providerFactsScheduled) return
    this.#providerFactsScheduled = true
    queueMicrotask(() => this.#flushProviderFacts())
  }

  #flushProviderFacts(): void {
    this.#providerFactsScheduled = false
    const facts = this.#providerFacts.splice(0)
    if (this.#providerFactsLost > 0) {
      facts.push({ kind: 'observer-loss', count: this.#providerFactsLost })
      this.#providerFactsLost = 0
    }
    for (const fact of facts) {
      try { this.#observer?.providerFact?.(fact) } catch { /* Observation has no provider authority. */ }
    }
  }

  #sampleStats(): Promise<void> {
    if (this.#statsPending !== undefined) return this.#statsPending
    if (typeof this.#peer.getStats !== 'function' || this.#parentClosed) return Promise.resolve()
    const task = this.#peer.getStats().then((stats) => {
      const now = performance.now()
      const selected = selectedPairFact(stats, this.#selectedPairID, this.#selectedAt, now)
      if (this.#parentClosed) return
      this.#path.observeStats(selected?.fact.route)
      if (selected === undefined) return
      if (selected.id !== this.#selectedPairID) this.#selectedAt = now
      this.#selectedPairID = selected.id
      this.#fact(selected.fact)
    }).catch(() => undefined).finally(() => { if (this.#statsPending === task) this.#statsPending = undefined })
    this.#statsPending = task
    return task
  }

  #pathChanged(route: PeerPathRoute): void {
    this.#fact({ kind: 'state', phase: 'ice-check', state: route === undefined ? 'route-unavailable' : `route-${route}`, elapsedMs: performance.now() - this.#startedAt })
    if (!this.#openedChannel && route !== undefined) this.#events.push({ type: 'path-changed' })
    for (const listener of this.#routeListeners) {
      try { listener(route) } catch { /* A subscriber cannot own provider settlement. */ }
    }
  }

  #onUnexpectedDataChannel = (event: RTCDataChannelEvent): void => {
    closeDataChannel(event.channel)
    this.#interrupt(new UnexpectedDataChannelError())
  }
}

function candidateForSignaling(payload: unknown): RTCIceCandidateInit | undefined {
  if (!isRecord(payload) || typeof payload.candidate !== 'string') {
    throw new TypeError('ICE candidate text is missing')
  }
  const sdpMid = optionalCandidateString(payload.sdpMid, 'sdpMid')
  const usernameFragment = optionalCandidateString(payload.usernameFragment, 'usernameFragment')
  const line = optionalCandidateLine(payload.sdpMLineIndex)
  if (payload.candidate === '') return undefined
  return Object.freeze({
    candidate: payload.candidate,
    sdpMid,
    sdpMLineIndex: line ?? null,
    usernameFragment,
  })
}

function optionalCandidateLine(value: unknown): number | null {
  if (value === undefined || value === null) return null
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0 || value > 0xffff) {
    throw new TypeError('ICE candidate m-line index is invalid')
  }
  return value
}

function optionalCandidateString(value: unknown, label: string): string | null {
  if (value === undefined || value === null) return null
  if (typeof value !== 'string') throw new TypeError(`ICE candidate ${label} is invalid`)
  return value
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function cloneConfiguration(configuration: RTCConfiguration): RTCConfiguration {
  return structuredClone(configuration)
}

function closeDataChannel(channel: RTCDataChannel): void {
  try {
    channel.close()
  } catch {
    // The unexpected channel is already a peer violation; cleanup cannot replace
    // that typed failure with a browser-specific close exception.
  }
}

function normalizeOwnerFailure(reason: unknown): unknown {
  return reason ?? new PeerNegotiationError('negotiation stopped without an error reason')
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
} {
  let settled = false
  let accept!: (value: T) => void
  let decline!: (reason: unknown) => void
  const promise = new Promise<T>((resolve, reject) => {
    accept = resolve
    decline = reject
  })
  return {
    promise,
    resolve: (value) => {
      if (!settled) {
        settled = true
        accept(value)
      }
    },
    reject: (reason) => {
      if (!settled) {
        settled = true
        decline(reason)
      }
    },
  }
}
