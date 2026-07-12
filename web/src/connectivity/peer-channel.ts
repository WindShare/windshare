import type { FrameChannel } from '../contracts'
import type { WebRTCFrameChannel } from '../transport/webrtc'

export interface PeerChannel extends FrameChannel {
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
  #cachedOwner: unknown
  #cachedTransport: unknown
  #cachedCombined: AggregateError | undefined

  constructor(
    channel: WebRTCFrameChannel,
    ownerDone: Promise<void>,
    failure: () => PeerOwnerFailure | undefined,
  ) {
    this.#channel = channel
    this.#failure = failure
    this.frames = channel.frames
    this.opened = channel.opened
    this.done = ownerDone
  }

  get state() {
    return this.#channel.state
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
