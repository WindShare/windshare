import type { ChannelState, Frame, FrameChannel } from '../../../contracts/channel'
import { equalBytes } from '../../../crypto/bytes'
import { decodeV2OpaqueRoute, encodeV2OpaqueRoute } from '../v2-protocol'
import { decodeSettlementNotice } from '../delivery-settlement-codec'
import type { RelayReceiveCapacity } from '../delivery-handshake-codec'
import type { ReceiveLedgerEvent } from '../../../contracts/relay-delivery-observation'
import { RelayHeartbeat, type RelayHeartbeatTrace } from '../heartbeat'
import { ReceiverCredit } from '../receiver-credit'
import { RelayReceiveBuffer } from './receive-buffer'
import {
  V2RelayReceiverError, V2_RELAY_HEARTBEAT_CLOSE_CODE, V2_RELAY_PROTOCOL_CLOSE_CODE,
  WEBSOCKET_CLOSING, WEBSOCKET_CLOSED, awaitSocketClose, decodeRelayErrorIfPresent,
  relayErrorMessage, waitForSendTurn, waitForBufferCapacity, waitForBufferDrain, type V2WebSocketPort,
} from './socket'

export const V2_RELAY_CLOSE_TIMEOUT_MILLISECONDS = 2_000
let nextReceiveConnectionId = 0n

export class V2OpaqueRelayFrameChannel implements FrameChannel {
  readonly frames: ReadableStream<Frame>
  readonly #socket: V2WebSocketPort
  readonly #relaySessionId: Uint8Array<ArrayBuffer>
  readonly #receiveQueue: RelayReceiveBuffer
  readonly #heartbeat: RelayHeartbeat
  readonly #credit: ReceiverCredit
  readonly #lifetime = new AbortController()
  readonly #socketLifetime = new AbortController()
  #state: ChannelState = 'open'
  #sendTail: Promise<void> = Promise.resolve()
  #closeTask: Promise<void> | undefined
  #feedbackTask: Promise<void> | undefined
  #feedbackRequested = false

  constructor(socket: V2WebSocketPort, relaySessionId: Uint8Array, capacity: RelayReceiveCapacity,
    heartbeatTrace?: (event: RelayHeartbeatTrace) => void, receiveTrace?: (event: ReceiveLedgerEvent) => void) {
    this.#socket = socket
    this.#relaySessionId = relaySessionId.slice()
    this.#credit = new ReceiverCredit(relaySessionId)
    this.#receiveQueue = new RelayReceiveBuffer({
      connectionId: `relay-receiver-${++nextReceiveConnectionId}`, relaySessionId, capacity,
      wakeWriter: () => this.#wakeFeedback(), cancel: () => { this.close().catch(error => this.#fail(error)) },
      ...(receiveTrace === undefined ? {} : { observe: receiveTrace }),
    })
    this.frames = this.#receiveQueue.stream
    socket.addEventListener('message', this.#onMessage)
    socket.addEventListener('close', this.#onClose)
    socket.addEventListener('error', this.#onError)
    this.#heartbeat = new RelayHeartbeat(socket, error => this.#fail(error, V2_RELAY_HEARTBEAT_CLOSE_CODE, 'relay heartbeat failed'), heartbeatTrace)
  }

  get state(): ChannelState {
    return this.#state
  }

  send(frame: Frame, signal?: AbortSignal): Promise<void> {
    return this.#enqueue(frame, false, signal)
  }

  sendTerminal(frame: Frame, signal?: AbortSignal): Promise<void> {
    return this.#enqueue(frame, true, signal)
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  #enqueue(frame: Frame, terminal: boolean, signal?: AbortSignal): Promise<void> {
    let encoded: Uint8Array<ArrayBuffer>
    try {
      encoded = encodeV2OpaqueRoute({ relaySessionId: this.#relaySessionId, ciphertext: frame })
    } catch (error) {
      return Promise.reject(error)
    }
    const lifetime = this.#lifetime.signal
    const sendSignal = signal === undefined ? lifetime : AbortSignal.any([signal, lifetime])
    const previous = this.#sendTail
    const operation = waitForSendTurn(previous, sendSignal).then(async () => {
      await this.#credit.waitForCapacity(encoded.byteLength, sendSignal)
      await waitForBufferCapacity(this.#socket, sendSignal)
      sendSignal.throwIfAborted()
      // No asynchronous boundary may separate this reservation from socket.send:
      // aborted waits must leave both frame and complete wire-byte credit intact.
      this.#credit.consume(encoded.byteLength)
      try {
        this.#socket.send(encoded)
      } catch (error) {
        this.#fail(error)
        throw error
      }
      if (terminal) {
        await waitForBufferDrain(this.#socket, sendSignal)
        await this.close()
      }
    })
    // Aborting a queued caller must be prompt without letting its successor
    // overtake the still-active writer that preceded it.
    this.#sendTail = Promise.all([previous, operation.catch(() => undefined)]).then(() => undefined)
    return operation
  }

  readonly #onMessage = (event: MessageEvent): void => {
    if (this.#socketLifetime.signal.aborted) return
    try {
      if (!(event.data instanceof ArrayBuffer)) {
        throw new V2RelayReceiverError('Relay delivered a non-binary frame')
      }
      this.#dispatch(new Uint8Array(event.data))
      // Every accepted frame proves the relay alive, so liveness never waits on
      // an acknowledgement queued behind content.
      this.#heartbeat.observeInbound()
    } catch (error) {
      this.#fail(error)
    }
  }

  #dispatch(encoded: Uint8Array): void {
    if (this.#heartbeat.receive(encoded) || this.#credit.receive(encoded)) return
    const relayError = decodeRelayErrorIfPresent(encoded)
    if (relayError !== undefined) {
      throw new V2RelayReceiverError(relayErrorMessage(relayError), { relayError })
    }
    if (String.fromCharCode(...encoded.subarray(0, 4)) === 'WS2N') {
      const notice = decodeSettlementNotice(encoded)
      if (!equalBytes(notice.relaySessionId, this.#relaySessionId)) {
        throw new V2RelayReceiverError('Relay settled another receiver session')
      }
      this.#receiveQueue.notice(notice)
      // Application sends stop now, but the socket remains authoritative until
      // the accepted prefix has drained and its settlement is submitted.
      this.#state = 'closed'
      this.#lifetime.abort(new V2RelayReceiverError('Relay frame channel is closed'))
      return
    }
    const routed = decodeV2OpaqueRoute(encoded)
    if (!equalBytes(routed.relaySessionId, this.#relaySessionId)) {
      throw new V2RelayReceiverError('Relay delivered another receiver session')
    }
    this.#receiveQueue.receiveOwned(routed.ciphertext, encoded.byteLength)
  }

  readonly #onClose = (): void => {
    if (this.#socketLifetime.signal.aborted) return
    this.#state = 'closed'
    this.#removeListeners()
    this.#receiveQueue.disconnect()
  }

  readonly #onError = (): void => {
    this.#fail(new V2RelayReceiverError('Relay WebSocket failed'))
  }

  #fail(reason: unknown, code = V2_RELAY_PROTOCOL_CLOSE_CODE, closeReason = 'invalid relay frame'): void {
    if (this.#socketLifetime.signal.aborted) return
    this.#state = 'closed'
    this.#removeListeners(reason)
    this.#receiveQueue.fail(reason)
    if (this.#socket.readyState < WEBSOCKET_CLOSING) this.#socket.close(code, closeReason)
  }

  async #close(): Promise<void> {
    this.#state = 'closed'
    this.#receiveQueue.discard()
    if (!this.#socketLifetime.signal.aborted) {
      // At most one final control (32 bytes) bypasses local buffer pressure, just
      // like a heartbeat. close() queues after it; no absent notice is invented.
      try {
        const control = this.#receiveQueue.takeControl()
        if (control !== undefined) this.#socket.send(control)
      } catch (error) {
        this.#fail(error)
      }
      this.#removeListeners()
      this.#receiveQueue.disconnect()
    }
    if (this.#socket.readyState < WEBSOCKET_CLOSING) this.#socket.close(1000, 'receiver closed')
    if (this.#socket.readyState !== WEBSOCKET_CLOSED) {
      await awaitSocketClose(this.#socket, V2_RELAY_CLOSE_TIMEOUT_MILLISECONDS)
    }
  }

  #wakeFeedback(): void {
    if (this.#socketLifetime.signal.aborted) return
    this.#feedbackRequested = true
    if (this.#feedbackTask !== undefined) return
    this.#feedbackTask = Promise.resolve().then(async () => {
      while (this.#feedbackRequested) {
        this.#feedbackRequested = false
        await waitForBufferCapacity(this.#socket, this.#socketLifetime.signal)
        this.#socketLifetime.signal.throwIfAborted()
        const control = this.#receiveQueue.takeControl()
        if (control === undefined) continue
        // Taking a prefix and submitting it are one synchronous writer turn.
        // Content-credit waits never own this turn, and pending reads coalesce.
        this.#socket.send(control)
        if (this.#receiveQueue.settled) {
          this.close().catch(error => this.#fail(error))
          return
        }
      }
    }).catch(error => this.#fail(error)).finally(() => {
      this.#feedbackTask = undefined
      if (this.#feedbackRequested) this.#wakeFeedback()
    })
  }

  #removeListeners(reason: unknown = new V2RelayReceiverError('Relay frame channel is closed')): void {
    this.#lifetime.abort(reason)
    this.#socketLifetime.abort(reason)
    this.#heartbeat.close()
    this.#socket.removeEventListener('message', this.#onMessage)
    this.#socket.removeEventListener('close', this.#onClose)
    this.#socket.removeEventListener('error', this.#onError)
  }
}
