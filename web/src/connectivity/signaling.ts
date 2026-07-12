import type { FrameChannel } from '../contracts'
import type { RelaySignal } from '../transport/relay'

export const SIGNAL_KIND_OFFER = 'offer'
export const SIGNAL_KIND_ANSWER = 'answer'
export const SIGNAL_KIND_CANDIDATE = 'candidate'

export interface ConnectivitySignal {
  readonly kind: string
  readonly payload: unknown
}

export interface SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void>
}

export interface RelayConnectivityChannel extends FrameChannel {
  readonly signalMessages: ReadableStream<RelaySignal>
  sendSignal(kind: string, payload: unknown, signal?: AbortSignal): Promise<void>
}

/** Projects one relay session's control lane without importing fallback policy. */
export class RelaySignalingRoute implements SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  readonly #channel: RelayConnectivityChannel

  constructor(channel: RelayConnectivityChannel) {
    this.#channel = channel
    this.messages = channel.signalMessages
  }

  send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void> {
    if (signal.kind === '') {
      return Promise.reject(new TypeError('signaling kind must not be empty'))
    }
    return this.#channel.sendSignal(signal.kind, structuredClone(signal.payload), abort)
  }
}
