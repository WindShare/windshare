import type { FrameChannel } from '../contracts/channel'
import type { WebRTCFrameChannel } from '../transport/webrtc'

export type PeerPathRoute = 'direct' | 'turn' | undefined

export interface PeerChannel extends FrameChannel {
  readonly pathRoute?: PeerPathRoute
  subscribePathRoute?(listener: (route: PeerPathRoute) => void): () => void
  readonly opened: Promise<void>
  readonly done: Promise<void>
  readonly reason: unknown
}

export interface PeerOwnerFailure {
  readonly reason: unknown
}

/** Keeps transport and negotiation ownership distinct while preserving both failures. */
export class OwnedPeerChannel implements PeerChannel {
  readonly frames: ReadableStream<Uint8Array>
  readonly opened: Promise<void>
  readonly done: Promise<void>
  readonly #channel: WebRTCFrameChannel
  readonly #failure: () => PeerOwnerFailure | undefined
  readonly #pathRoute: () => PeerPathRoute
  readonly #subscribePathRoute: (listener: (route: PeerPathRoute) => void) => () => void
  #cachedOwner: unknown
  #cachedTransport: unknown
  #cachedCombined: AggregateError | undefined

  constructor(
    channel: WebRTCFrameChannel,
    ownerDone: Promise<void>,
    failure: () => PeerOwnerFailure | undefined,
    pathRoute: () => PeerPathRoute = () => undefined,
    subscribePathRoute: (listener: (route: PeerPathRoute) => void) => () => void = () => () => undefined,
  ) {
    this.#channel = channel
    this.#failure = failure
    this.#pathRoute = pathRoute
    this.#subscribePathRoute = subscribePathRoute
    this.frames = channel.frames
    this.opened = channel.opened
    this.done = ownerDone
  }

  get state() {
    return this.#channel.state
  }

  get pathRoute(): PeerPathRoute { return this.#pathRoute() }

  subscribePathRoute(listener: (route: PeerPathRoute) => void): () => void {
    return this.#subscribePathRoute(listener)
  }

  get reason(): unknown {
    const owner = this.#failure()?.reason
    const transport = this.#channel.reason
    if (owner === undefined) {
      return transport
    }
    if (transport === undefined || transport === owner) {
      return owner
    }
    if (owner !== this.#cachedOwner || transport !== this.#cachedTransport) {
      this.#cachedOwner = owner
      this.#cachedTransport = transport
      this.#cachedCombined = new AggregateError(
        [owner, transport],
        'WebRTC negotiation owner and DataChannel both failed',
        { cause: owner },
      )
    }
    return this.#cachedCombined
  }

  send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    return this.#channel.send(frame, signal)
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    return this.#channel.sendTerminal(frame, signal)
  }

  close(): Promise<void> {
    return this.#channel.close()
  }
}
