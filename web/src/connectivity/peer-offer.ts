import {
  createWindShareFrameChannel,
  type WebRTCFrameChannel,
} from '../transport/webrtc'
import { V2_MAXIMUM_PEER_CANDIDATES } from '../session/v2-operation-continuation'
import { abortReason } from './clock'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
  UnexpectedDataChannelError,
} from './errors'
import {
  OwnedPeerChannel,
  type PeerChannel,
  type PeerOwnerFailure,
} from './peer-channel'
import {
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
  type SignalingRoute,
} from './signaling'

export const DEFAULT_STUN_SERVER = 'stun:stun.l.google.com:19302'
export const MAX_ICE_CANDIDATES_PER_PEER = V2_MAXIMUM_PEER_CANDIDATES

// Lifecycle and description events need their own reserve because candidate limits
// alone must not let a candidate burst hide cancellation or terminal settlement.
const NEGOTIATION_EVENT_RESERVE = 16

export interface OfferChannelFactory {
  offer(route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel>
}

export interface BrowserOfferFactoryOptions {
  readonly configuration?: RTCConfiguration
  readonly createPeerConnection?: (configuration: RTCConfiguration) => RTCPeerConnection
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
  readonly #createPeerConnection: (configuration: RTCConfiguration) => RTCPeerConnection
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
    this.#createPeerConnection = options.createPeerConnection ??
      ((configuration) => new RTCPeerConnection(configuration))
    this.#maxCandidates = maximum
  }

  async offer(route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel> {
    signal.throwIfAborted()
    let peer: RTCPeerConnection
    try {
      peer = this.#createPeerConnection(cloneConfiguration(this.#configuration))
    } catch (cause) {
      throw new PeerNegotiationError('could not create the PeerConnection', { cause })
    }
    let channel: WebRTCFrameChannel
    try {
      channel = createWindShareFrameChannel(peer)
    } catch (error) {
      closePeer(peer)
      throw error
    }
    const negotiation = new OfferNegotiation(peer, channel, route, this.#maxCandidates)
    negotiation.run(signal).catch(() => undefined)
    return negotiation.opened
  }
}

type NegotiationEvent =
  | { readonly type: 'channel-done' }
  | { readonly type: 'channel-opened' }
  | { readonly type: 'failure'; readonly reason: unknown }
  | { readonly type: 'local-candidate'; readonly candidate: RTCIceCandidateInit }
  | { readonly type: 'route-closed'; readonly reason?: unknown }
  | { readonly type: 'signal'; readonly signal: ConnectivitySignal }

class OfferNegotiation {
  readonly #peer: RTCPeerConnection
  readonly #channel: WebRTCFrameChannel
  readonly #route: SignalingRoute
  readonly #maximumCandidates: number
  readonly #events: EventQueue<NegotiationEvent>
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
  #localCandidateFailureReported = false
  #parentClosed = false

  constructor(
    peer: RTCPeerConnection,
    channel: WebRTCFrameChannel,
    route: SignalingRoute,
    maximumCandidates: number,
  ) {
    this.#peer = peer
    this.#channel = channel
    this.#route = route
    this.#maximumCandidates = maximumCandidates
    this.#ownedChannel = new OwnedPeerChannel(
      channel,
      this.#settled.promise,
      () => this.#ownerFailure,
    )
    this.#events = new EventQueue<NegotiationEvent>(
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
      this.#peer.removeEventListener('datachannel', this.#onUnexpectedDataChannel)
      this.#events.close()
      this.#localCandidateFingerprints.clear()
      // Parent-first teardown prevents a terminal-pending DataChannel from
      // pinning cancellation or a fatal signaling violation.
      this.#closeParent()
      await this.#reader?.cancel().catch(() => undefined)
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
    try {
      offer = await awaitWithAbort(this.#peer.createOffer(), signal)
      await awaitWithAbort(this.#peer.setLocalDescription(offer), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError('could not create the local offer', { cause })
    }
    const local = this.#peer.localDescription
    if (local === null || local.type !== SIGNAL_KIND_OFFER || local.sdp === '') {
      throw new PeerNegotiationError('local offer description is unavailable')
    }
    await this.#send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: local.type, sdp: local.sdp },
    }, signal)
  }

  async #processEvents(signal: AbortSignal): Promise<void> {
    const remote = new RemoteNegotiationState(this.#peer, this.#maximumCandidates)
    while (true) {
      const event = await this.#events.next()
      if (await this.#handleEvent(event, remote, signal) === 'done') {
        return
      }
    }
  }

  async #handleEvent(
    event: NegotiationEvent,
    remote: RemoteNegotiationState,
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
    if (event.type === 'channel-opened') {
      this.#publishOpenedChannel()
      return 'continue'
    }
    if (event.type === 'route-closed') {
      this.#handleRouteClosed(event.reason)
      return 'continue'
    }
    if (event.type === 'local-candidate') {
      if (this.#signalingAvailable) {
        await this.#sendCandidate(event.candidate, signal)
      }
      return 'continue'
    }
    if (this.#signalingAvailable) {
      await remote.accept(event.signal, signal)
    }
    return 'continue'
  }

  #publishOpenedChannel(): void {
    if (!this.#openedChannel) {
      this.#openedChannel = true
      this.#opened.resolve(this.#ownedChannel)
    }
  }

  #handleRouteClosed(reason: unknown): void {
    if (!this.#reconcileOpenedChannel()) {
      throw new PeerNegotiationError('signaling closed before DataChannel Open', { cause: reason })
    }
    this.#disableSignaling()
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
    this.#reader?.cancel().catch(() => undefined)
  }

  #reconcileOpenedChannel(): boolean {
    if (!this.#openedChannel && this.#channel.state === 'open') {
      this.#publishOpenedChannel()
    }
    return this.#openedChannel
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
      closePeer(this.#peer)
    }
  }

  #onIceCandidate = (event: RTCPeerConnectionIceEvent): void => {
    if (event.candidate === null || this.#localCandidateFailureReported) {
      return
    }
    try {
      const candidate = canonicalCandidate(event.candidate.toJSON())
      const fingerprint = candidateIdentity(candidate)
      if (this.#localCandidateFingerprints.has(fingerprint)) return
      // Identity admission precedes both lifetime quota and event capacity so a
      // browser callback replay cannot consume either resource twice.
      this.#localCandidateFingerprints.add(fingerprint)
      if (this.#localCandidateFingerprints.size > this.#maximumCandidates) {
        this.#localCandidateFailureReported = true
        this.#interrupt(new CandidateLimitExceededError(this.#maximumCandidates))
        return
      }
      this.#events.push({ type: 'local-candidate', candidate })
    } catch (cause) {
      this.#localCandidateFailureReported = true
      this.#interrupt(
        new PeerNegotiationError('could not encode a local ICE candidate', { cause }),
      )
    }
  }

  #onConnectionStateChange = (): void => {
    if (this.#peer.connectionState === 'failed') {
      this.#interrupt(new PeerNegotiationError('PeerConnection entered failed state'))
    }
  }

  #onUnexpectedDataChannel = (event: RTCDataChannelEvent): void => {
    closeDataChannel(event.channel)
    this.#interrupt(new UnexpectedDataChannelError())
  }
}

class RemoteNegotiationState {
  readonly #peer: RTCPeerConnection
  readonly #maximumCandidates: number
  #descriptionReceived = false
  #remoteCandidates = 0
  #pendingCandidates: RTCIceCandidateInit[] = []

  constructor(peer: RTCPeerConnection, maximumCandidates: number) {
    this.#peer = peer
    this.#maximumCandidates = maximumCandidates
  }

  async accept(message: ConnectivitySignal, signal: AbortSignal): Promise<void> {
    if (message.kind === SIGNAL_KIND_CANDIDATE) {
      await this.#acceptCandidate(message.payload, signal)
      return
    }
    if (message.kind !== SIGNAL_KIND_ANSWER || this.#descriptionReceived) {
      throw new PeerNegotiationError(`unexpected signal kind ${JSON.stringify(message.kind)}`)
    }
    await this.#acceptAnswer(message.payload, signal)
  }

  async #acceptCandidate(payload: unknown, signal: AbortSignal): Promise<void> {
    this.#remoteCandidates += 1
    if (this.#remoteCandidates > this.#maximumCandidates) {
      throw new CandidateLimitExceededError(this.#maximumCandidates)
    }
    const candidate = decodeCandidate(payload)
    if (!this.#descriptionReceived) {
      this.#pendingCandidates.push(candidate)
      return
    }
    await this.#addCandidate(candidate, signal)
  }

  async #acceptAnswer(payload: unknown, signal: AbortSignal): Promise<void> {
    const answer = decodeAnswer(payload)
    try {
      await awaitWithAbort(this.#peer.setRemoteDescription(answer), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError('could not set the remote answer', { cause })
    }
    this.#descriptionReceived = true
    for (const candidate of this.#pendingCandidates) {
      await this.#addCandidate(candidate, signal)
    }
    this.#pendingCandidates = []
  }

  async #addCandidate(candidate: RTCIceCandidateInit, signal: AbortSignal): Promise<void> {
    try {
      await awaitWithAbort(this.#peer.addIceCandidate(candidate), signal)
    } catch (cause) {
      if (signal.aborted) {
        throw abortReason(signal)
      }
      throw new PeerNegotiationError('could not add a remote ICE candidate', { cause })
    }
  }
}

class EventQueue<T> {
  readonly #items: T[] = []
  readonly #capacity: number
  readonly #overflow: () => T
  readonly #onOverflow: (overflow: T) => void
  #waiting: ((value: T) => void) | undefined
  #closed = false
  #overflowed = false

  constructor(capacity: number, overflow: () => T, onOverflow: (overflow: T) => void) {
    this.#capacity = capacity
    this.#overflow = overflow
    this.#onOverflow = onOverflow
  }

  push(value: T): void {
    if (this.#closed || this.#overflowed) {
      return
    }
    const waiting = this.#waiting
    if (waiting !== undefined) {
      this.#waiting = undefined
      waiting(value)
      return
    }
    if (this.#items.length >= this.#capacity) {
      const overflow = this.#overflow()
      this.#items.length = 0
      this.#items.push(overflow)
      this.#overflowed = true
      this.#onOverflow(overflow)
      return
    }
    this.#items.push(value)
  }

  next(): Promise<T> {
    const value = this.#items.shift()
    if (value !== undefined) {
      return Promise.resolve(value)
    }
    return new Promise<T>((resolve) => {
      this.#waiting = resolve
    })
  }

  close(): void {
    this.#closed = true
    this.#items.length = 0
  }
}

function decodeAnswer(payload: unknown): RTCSessionDescriptionInit {
  if (!isRecord(payload) || payload.type !== SIGNAL_KIND_ANSWER ||
      typeof payload.sdp !== 'string' || payload.sdp === '') {
    throw new PeerNegotiationError('answer payload is invalid')
  }
  return { type: 'answer', sdp: payload.sdp }
}

function decodeCandidate(payload: unknown): RTCIceCandidateInit {
  if (!isRecord(payload) || typeof payload.candidate !== 'string' || payload.candidate === '') {
    throw new PeerNegotiationError('ICE candidate payload is invalid')
  }
  return structuredClone(payload) as RTCIceCandidateInit
}

function canonicalCandidate(payload: unknown): RTCIceCandidateInit {
  if (!isRecord(payload) || typeof payload.candidate !== 'string' || payload.candidate === '') {
    throw new TypeError('ICE candidate text is missing')
  }
  const sdpMid = optionalCandidateString(payload.sdpMid, 'sdpMid')
  const usernameFragment = optionalCandidateString(payload.usernameFragment, 'usernameFragment')
  const line = optionalCandidateLine(payload.sdpMLineIndex)
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

function candidateIdentity(candidate: RTCIceCandidateInit): string {
  return JSON.stringify([
    candidate.candidate,
    candidate.sdpMid ?? null,
    candidate.sdpMLineIndex ?? null,
    candidate.usernameFragment ?? null,
  ])
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function cloneConfiguration(configuration: RTCConfiguration): RTCConfiguration {
  return structuredClone(configuration)
}

function closePeer(peer: RTCPeerConnection): void {
  try {
    peer.close()
  } catch {
    // Construction/negotiation failure is the actionable cause; cleanup must
    // remain best-effort and cannot replace it with a second browser exception.
  }
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

function awaitWithAbort<T>(operation: Promise<T>, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const aborted = () => reject(abortReason(signal))
    signal.addEventListener('abort', aborted, { once: true })
    operation.then(
      (value) => {
        signal.removeEventListener('abort', aborted)
        resolve(value)
      },
      (reason: unknown) => {
        signal.removeEventListener('abort', aborted)
        reject(reason)
      },
    )
  })
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
