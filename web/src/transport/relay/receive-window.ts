import type { Frame } from '../../contracts/channel'
import { BoundedStreamQueue } from './stream-queue'
import { V2RelayProtocolError } from './v2-protocol'
import {
  encodeReceiveCredit, RELAY_OPAQUE_ROUTE_HEADER_BYTES, RELAY_RECEIVE_CREDIT_BATCH_FRAMES,
  RELAY_RECEIVE_WINDOW_BYTES, RELAY_RECEIVE_WINDOW_FRAMES,
} from './receive-credit-codec'

/** Grants reserve ingress storage until the stream's sole consumer takes a frame. */
export class RelayReceiveWindow {
  readonly #sessionId: Uint8Array
  readonly #send: (encoded: Uint8Array<ArrayBuffer>) => void
  readonly #queue: BoundedStreamQueue<Frame>
  #frames = 0
  #bytes = 0
  #returnedFrames = 0
  #returnedBytes = 0
  #closed = false

  constructor(sessionId: Uint8Array, send: (encoded: Uint8Array<ArrayBuffer>) => void, cancel: () => void) {
    this.#sessionId = sessionId.slice()
    this.#send = send
    this.#queue = new BoundedStreamQueue(RELAY_RECEIVE_WINDOW_FRAMES, cancel, frame => this.#consumed(frame))
  }

  get stream(): ReadableStream<Frame> { return this.#queue.stream }

  start(): void {
    this.#grant(RELAY_RECEIVE_WINDOW_FRAMES, RELAY_RECEIVE_WINDOW_BYTES)
  }

  push(frame: Frame): void {
    const bytes = frame.byteLength + RELAY_OPAQUE_ROUTE_HEADER_BYTES
    if (this.#frames === 0 || this.#bytes < bytes) {
      throw new V2RelayProtocolError('malformed', 'Relay exceeded its advertised receive credit')
    }
    this.#frames -= 1
    this.#bytes -= bytes
    if (this.#queue.push(frame) === 'overflow') {
      throw new V2RelayProtocolError('malformed', 'Relay receive queue exceeded its reserved capacity')
    }
  }

  close(): void {
    this.#closed = true
    this.#queue.close()
  }

  fail(reason: unknown): void {
    this.#closed = true
    this.#queue.fail(reason)
  }

  #consumed(frame: Frame): void {
    if (this.#closed) return
    this.#returnedFrames += 1
    this.#returnedBytes += frame.byteLength + RELAY_OPAQUE_ROUTE_HEADER_BYTES
    // Refill while most of the window is still available. No per-frame round
    // trip or idle timer may turn a short response into stop-and-wait delivery.
    if (this.#returnedFrames < RELAY_RECEIVE_CREDIT_BATCH_FRAMES) return
    const frames = this.#returnedFrames
    const bytes = this.#returnedBytes
    this.#returnedFrames = 0
    this.#returnedBytes = 0
    this.#grant(frames, bytes)
  }

  #grant(frames: number, bytes: number): void {
    this.#frames += frames
    this.#bytes += bytes
    this.#send(encodeReceiveCredit({ relaySessionId: this.#sessionId, frames, bytes }))
  }
}
