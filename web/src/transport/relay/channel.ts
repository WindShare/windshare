import {
  MAX_FRAME_BYTES,
  MIN_FRAME_BYTES,
  type ChannelState,
  type FrameChannel,
} from '../../contracts/channel'
import { SessionOutbox } from './outbox'
import {
  encodeForwardEnvelope,
  encodeSignaling,
  encodeTerminalForwardEnvelope,
  formatSessionId,
  type SessionId,
} from './protocol'
import { type QueueIngressResult, BoundedStreamQueue } from './stream-queue'
import { RelaySocketClosedError, type RelaySocket } from './socket'

export const RELAY_INBOUND_FRAME_CAPACITY = 32
export const RELAY_SIGNAL_CAPACITY = 16
export const RELAY_OUTBOUND_FRAME_CAPACITY = 16
export const RELAY_CLOSE_TIMEOUT_MS = 2_000

export interface RelaySignal {
  readonly kind: string
  readonly payload: unknown
}

export class RelayChannelClosedError extends Error {
  constructor(message = 'relay frame channel is closed') {
    super(message)
    this.name = 'RelayChannelClosedError'
  }
}

export class RelaySessionIngressError extends Error {
  readonly lane: 'frames' | 'signals'

  constructor(lane: 'frames' | 'signals') {
    super(`relay session inbound ${lane} queue is full`)
    this.name = 'RelaySessionIngressError'
    this.lane = lane
  }
}

function validateFrame(frame: Uint8Array): void {
  if (frame.byteLength < MIN_FRAME_BYTES || frame.byteLength > MAX_FRAME_BYTES) {
    throw new RangeError(
      `frame must be ${MIN_FRAME_BYTES}..${MAX_FRAME_BYTES} bytes`,
    )
  }
}

/** One physical relay receiver connection owns exactly one channel/session ID. */
export class RelayFrameChannel implements FrameChannel {
  readonly #sessionId: SessionId
  readonly #socket: RelaySocket
  readonly #frames: BoundedStreamQueue<Uint8Array>
  readonly #signals: BoundedStreamQueue<RelaySignal>
  readonly #outbox: SessionOutbox
  readonly frames: ReadableStream<Uint8Array>
  readonly signalMessages: ReadableStream<RelaySignal>
  #state: ChannelState = 'open'
  #reason: unknown
  #terminalStarted = false
  #terminalPromise: Promise<void> | undefined
  #socketClosePromise: Promise<void> | undefined

  constructor(sessionId: SessionId, socket: RelaySocket) {
    this.#sessionId = sessionId.slice() as SessionId
    this.#socket = socket
    this.#frames = new BoundedStreamQueue(
      RELAY_INBOUND_FRAME_CAPACITY,
      () => {
        this.close().catch(() => undefined)
      },
    )
    this.#signals = new BoundedStreamQueue(RELAY_SIGNAL_CAPACITY)
    this.frames = this.#frames.stream
    this.signalMessages = this.#signals.stream
    this.#outbox = new SessionOutbox(
      socket,
      RELAY_OUTBOUND_FRAME_CAPACITY,
      (reason) => this.#failOutbound(reason),
    )
  }

  get state(): ChannelState {
    return this.#state
  }

  get reason(): unknown {
    return this.#reason
  }

  get sessionId(): SessionId {
    return this.#sessionId.slice() as SessionId
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    validateFrame(frame)
    this.#requireOpen(signal)
    await this.#outbox.enqueue(
      { type: 'binary', data: encodeForwardEnvelope(this.#sessionId, frame) },
      signal,
    )
  }

  sendSignal(kind: string, payload: unknown, signal?: AbortSignal): Promise<void> {
    this.#requireOpen(signal)
    const data = encodeSignaling({
      type: 'signal',
      sessionId: formatSessionId(this.#sessionId),
      kind,
      payload,
    })
    return this.#outbox.enqueue({ type: 'text', data }, signal)
  }

  sendKeepalive(signal?: AbortSignal): Promise<void> {
    this.#requireOpen(signal)
    return this.#outbox.enqueue(
      { type: 'text', data: encodeSignaling({ type: 'keepalive' }) },
      signal,
    )
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    validateFrame(frame)
    this.#claimTerminal(signal)
    const operation = this.#writeTerminal(
      {
        type: 'binary',
        data: encodeTerminalForwardEnvelope(this.#sessionId, frame),
      },
      signal,
    )
    this.#terminalPromise = operation
    return operation
  }

  #claimTerminal(signal?: AbortSignal): void {
    this.#requireOpen(signal)
    this.#terminalStarted = true
  }

  async #writeTerminal(
    item: { readonly type: 'binary'; readonly data: Uint8Array } | {
      readonly type: 'text'
      readonly data: string
    },
    signal?: AbortSignal,
  ): Promise<void> {
    let failed = false
    let failure: unknown
    try {
      await this.#outbox.sendTerminal(item, signal)
    } catch (error) {
      failed = true
      failure = error
    }
    this.#shut(failed ? failure : undefined, true)
    try {
      await this.#startSocketClose()
    } catch (error) {
      if (!failed) {
        failed = true
        failure = error
      }
    }
    if (failed) {
      throw failure
    }
  }

  async close(): Promise<void> {
    if (this.#terminalPromise !== undefined) {
      try {
        await this.#terminalPromise
      } catch {
        // The terminal caller receives the transport failure; close stays idempotent.
      }
      return
    }
    if (this.#state === 'closed') {
      await this.#socketClosePromise?.catch(() => undefined)
      return
    }
    this.#terminalStarted = true
    const bye = encodeSignaling({
      type: 'bye',
      sessionId: formatSessionId(this.#sessionId),
    })
    const controller = new AbortController()
    const timeout = setTimeout(
      () => controller.abort(new DOMException('Relay close timed out', 'TimeoutError')),
      RELAY_CLOSE_TIMEOUT_MS,
    )
    const operation = this.#writeTerminal({ type: 'text', data: bye }, controller.signal)
      .finally(() => clearTimeout(timeout))
    this.#terminalPromise = operation
    try {
      await operation
    } catch (error) {
      if (!(error instanceof RelaySocketClosedError)) {
        throw error
      }
    }
  }

  #requireOpen(signal?: AbortSignal): void {
    if (signal?.aborted === true) {
      throw signal.reason ?? new DOMException('Operation aborted', 'AbortError')
    }
    if (this.#state !== 'open' || this.#terminalStarted) {
      throw new RelayChannelClosedError()
    }
  }

  deliverFrame(frame: Uint8Array): QueueIngressResult {
    if (this.#state !== 'open') {
      return 'closed'
    }
    validateFrame(frame)
    return this.#frames.push(frame.slice())
  }

  deliverSignal(signal: RelaySignal): QueueIngressResult {
    if (this.#state !== 'open') {
      return 'closed'
    }
    return this.#signals.push(structuredClone(signal))
  }

  deliverTerminal(frame: Uint8Array): QueueIngressResult {
    if (this.#state !== 'open') {
      return 'closed'
    }
    validateFrame(frame)
    const result = this.#frames.pushTerminal(frame.slice())
    if (result === 'accepted') {
      // The frame queue owns closure after draining its reserved final item.
      this.#shut(undefined, false)
    }
    return result
  }

  remoteClose(reason?: unknown): void {
    this.#shut(reason, true)
  }

  failIngress(lane: 'frames' | 'signals'): RelaySessionIngressError {
    const error = new RelaySessionIngressError(lane)
    this.#shut(error, true)
    return error
  }

  #failOutbound(reason: unknown): void {
    this.remoteClose(reason)
    this.#startSocketClose().catch(() => undefined)
  }

  #startSocketClose(): Promise<void> {
    if (this.#socketClosePromise === undefined) {
      this.#socketClosePromise = Promise.resolve().then(() => this.#socket.close())
      this.#socketClosePromise.catch(() => undefined)
    }
    return this.#socketClosePromise
  }

  #shut(reason: unknown, closeFrames: boolean): void {
    if (this.#state === 'closed') {
      return
    }
    this.#state = 'closed'
    this.#reason = reason
    this.#terminalStarted = true
    this.#outbox.shutdown(reason ?? new RelayChannelClosedError())
    if (closeFrames) {
      this.#frames.close()
    }
    this.#signals.close()
  }
}
