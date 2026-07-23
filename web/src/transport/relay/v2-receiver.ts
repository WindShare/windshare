import type { ChannelState, Frame, FrameChannel } from '../../contracts/channel'
import { equalBytes } from '../../crypto/bytes'
import type { Suite02CapabilityKey } from '../../crypto/suite02-link'
import { canonicalV2RelayEndpoint, type V2RelayEndpoint } from './v2-endpoint'
import {
  decodeV2DescriptorDelivery,
  decodeV2OpaqueRoute,
  decodeV2RelayError,
  decodeV2SessionRetired,
  encodeV2Join,
  encodeV2OpaqueRoute,
  V2_RELAY_ERROR,
  type V2RelayErrorFrame,
} from './v2-protocol'
import { BoundedStreamQueue } from './stream-queue'

const WEBSOCKET_OPEN = 1
const WEBSOCKET_CLOSING = 2
const WEBSOCKET_CLOSED = 3
const BUFFER_DRAIN_INTERVAL_MILLISECONDS = 8
const MAXIMUM_BUFFERED_BYTES = 4 * 65_536
export const V2_RELAY_RECEIVE_QUEUE_FRAMES = 32
export const V2_RELAY_CLOSE_TIMEOUT_MILLISECONDS = 2_000
export const V2_RELAY_JOIN_TIMEOUT_MILLISECONDS = 30_000
export const V2_RELAY_STARTING_WINDOW_MILLISECONDS = 5_000
const V2_RELAY_MINIMUM_RETRY_MILLISECONDS = 50

export interface V2WebSocketPort {
  binaryType: BinaryType
  readonly readyState: number
  readonly bufferedAmount: number
  send(data: ArrayBufferView<ArrayBuffer>): void
  close(code?: number, reason?: string): void
  addEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
    options?: AddEventListenerOptions,
  ): void
  removeEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
  ): void
}

export type V2WebSocketFactory = (endpoint: string) => V2WebSocketPort

export interface V2RelayReceiverConnection {
  readonly endpoint: V2RelayEndpoint
  readonly relaySessionId: Uint8Array<ArrayBuffer>
  readonly descriptorObject: Uint8Array<ArrayBuffer>
  readonly channel: FrameChannel
  close(): Promise<void>
}

export class V2RelayReceiverError extends Error {
  readonly relayError: V2RelayErrorFrame | undefined

  constructor(message: string, options?: ErrorOptions & { readonly relayError?: V2RelayErrorFrame }) {
    super(message, options)
    this.name = 'V2RelayReceiverError'
    this.relayError = options?.relayError
  }
}

export async function dialV2RelayReceiver(
  relayBase: string,
  capability: Suite02CapabilityKey,
  options: { readonly socketFactory?: V2WebSocketFactory; readonly signal?: AbortSignal } = {},
): Promise<V2RelayReceiverConnection> {
  const deadline = relayJoinDeadline(options.signal)
  const startedAt = Date.now()
  try {
    deadline.signal.throwIfAborted()
    const endpoint = await canonicalV2RelayEndpoint(relayBase)
    while (true) {
      try {
        return await dialV2RelayReceiverOnce(
          endpoint,
          capability,
          options.socketFactory ?? browserWebSocketFactory,
          deadline.signal,
        )
      } catch (error) {
        const elapsed = Date.now() - startedAt
        const remaining = V2_RELAY_STARTING_WINDOW_MILLISECONDS - elapsed
        if (
          !(error instanceof V2RelayReceiverError) ||
          error.relayError?.code !== V2_RELAY_ERROR.starting ||
          remaining <= 0
        ) throw error
        const retry = Math.max(
          V2_RELAY_MINIMUM_RETRY_MILLISECONDS,
          error.relayError.retryAfterMilliseconds,
        )
        await delay(Math.min(retry, remaining), deadline.signal)
      }
    }
  } finally {
    deadline.close()
  }
}

async function dialV2RelayReceiverOnce(
  endpoint: V2RelayEndpoint,
  capability: Suite02CapabilityKey,
  socketFactory: V2WebSocketFactory,
  signal: AbortSignal,
): Promise<V2RelayReceiverConnection> {
  signal.throwIfAborted()
  const socket = socketFactory(endpoint.dialEndpoint)
  socket.binaryType = 'arraybuffer'
  try {
    await awaitSocketOpen(socket, signal)
    socket.send(encodeV2Join(capability.shareIdRaw))
    const deliveryBytes = await nextBinaryMessage(socket, signal)
    const relayError = decodeRelayErrorIfPresent(deliveryBytes)
    if (relayError !== undefined) {
      throw new V2RelayReceiverError(relayErrorMessage(relayError), { relayError })
    }
    const delivery = decodeV2DescriptorDelivery(deliveryBytes)
    const channel = new V2OpaqueRelayFrameChannel(socket, delivery.relaySessionId)
    return Object.freeze({
      endpoint,
      relaySessionId: delivery.relaySessionId.slice(),
      descriptorObject: delivery.object.slice(),
      channel,
      close: () => channel.close(),
    })
  } catch (error) {
    if (socket.readyState < WEBSOCKET_CLOSING) socket.close(1000, 'relay join failed')
    throw error
  }
}

function relayJoinDeadline(parent?: AbortSignal): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent?.reason ?? new DOMException('Relay join aborted', 'AbortError'),
  )
  parent?.addEventListener('abort', abort, { once: true })
  if (parent?.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(new DOMException('Relay join timed out', 'TimeoutError'))
  }, V2_RELAY_JOIN_TIMEOUT_MILLISECONDS)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
  }
}

class V2OpaqueRelayFrameChannel implements FrameChannel {
  readonly frames: ReadableStream<Frame>
  readonly #socket: V2WebSocketPort
  readonly #relaySessionId: Uint8Array<ArrayBuffer>
  readonly #receiveQueue: BoundedStreamQueue<Frame>
  #state: ChannelState = 'open'
  #sendTail: Promise<void> = Promise.resolve()
  #closeTask: Promise<void> | undefined

  constructor(socket: V2WebSocketPort, relaySessionId: Uint8Array) {
    this.#socket = socket
    this.#relaySessionId = relaySessionId.slice()
    this.#receiveQueue = new BoundedStreamQueue(
      V2_RELAY_RECEIVE_QUEUE_FRAMES,
      () => { this.close().catch(() => undefined) },
    )
    this.frames = this.#receiveQueue.stream
    socket.addEventListener('message', this.#onMessage)
    socket.addEventListener('close', this.#onClose)
    socket.addEventListener('error', this.#onError)
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
    const owned = frame.slice()
    const operation = this.#sendTail.then(async () => {
      signal?.throwIfAborted()
      if (this.#state !== 'open' || this.#socket.readyState !== WEBSOCKET_OPEN) {
        throw new V2RelayReceiverError('Relay frame channel is closed')
      }
      await waitForBufferCapacity(this.#socket, signal)
      this.#socket.send(encodeV2OpaqueRoute({
        relaySessionId: this.#relaySessionId,
        ciphertext: owned,
      }))
      if (terminal) {
        await waitForBufferDrain(this.#socket, signal)
        await this.close()
      }
    })
    this.#sendTail = operation.catch(() => undefined)
    return operation
  }

  readonly #onMessage = (event: MessageEvent): void => {
    if (this.#state !== 'open') return
    try {
      if (!(event.data instanceof ArrayBuffer)) {
        throw new V2RelayReceiverError('Relay delivered a non-binary frame')
      }
      const encoded = new Uint8Array(event.data)
      const relayError = decodeRelayErrorIfPresent(encoded)
      if (relayError !== undefined) {
        throw new V2RelayReceiverError(relayErrorMessage(relayError), { relayError })
      }
      const retired = decodeSessionRetiredIfPresent(encoded)
      if (retired !== undefined) {
        // A relay retirement is idempotent: stale controls cannot acquire authority over
        // a different receiver session, and exact retirement ends this channel cleanly.
        if (equalBytes(retired, this.#relaySessionId)) this.#retire()
        return
      }
      const routed = decodeV2OpaqueRoute(encoded)
      if (!equalBytes(routed.relaySessionId, this.#relaySessionId)) {
        throw new V2RelayReceiverError('Relay delivered another receiver session')
      }
      if (this.#receiveQueue.push(routed.ciphertext.slice()) === 'overflow') {
        throw new V2RelayReceiverError('Relay receive queue exceeded its bounded frame budget')
      }
    } catch (error) {
      this.#fail(error)
    }
  }

  readonly #onClose = (): void => {
    if (this.#state === 'closed') return
    this.#state = 'closed'
    this.#removeListeners()
    this.#receiveQueue.close()
  }

  readonly #onError = (): void => {
    this.#fail(new V2RelayReceiverError('Relay WebSocket failed'))
  }

  #fail(reason: unknown): void {
    if (this.#state === 'closed') return
    this.#state = 'closed'
    this.#removeListeners()
    this.#receiveQueue.fail(reason)
    if (this.#socket.readyState < WEBSOCKET_CLOSING) this.#socket.close(1002, 'invalid relay frame')
  }

  #retire(): void {
    if (this.#state === 'closed') return
    this.#state = 'closed'
    this.#removeListeners()
    this.#receiveQueue.close()
    if (this.#socket.readyState < WEBSOCKET_CLOSING) this.#socket.close(1000, 'relay session retired')
  }

  async #close(): Promise<void> {
    if (this.#state === 'closed') return
    this.#state = 'closed'
    this.#removeListeners()
    if (this.#socket.readyState < WEBSOCKET_CLOSING) this.#socket.close(1000, 'receiver closed')
    if (this.#socket.readyState !== WEBSOCKET_CLOSED) {
      await awaitSocketClose(this.#socket, V2_RELAY_CLOSE_TIMEOUT_MILLISECONDS)
    }
    try {
      this.#receiveQueue.close()
    } catch {
      // A prior transport failure may already have errored the stream.
    }
  }

  #removeListeners(): void {
    this.#socket.removeEventListener('message', this.#onMessage)
    this.#socket.removeEventListener('close', this.#onClose)
    this.#socket.removeEventListener('error', this.#onError)
  }
}

function browserWebSocketFactory(endpoint: string): V2WebSocketPort {
  // No subprotocol is offered: the endpoint is versioned by its exact path.
  return new WebSocket(endpoint)
}

function awaitSocketOpen(socket: V2WebSocketPort, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted()
  if (socket.readyState === WEBSOCKET_OPEN) return Promise.resolve()
  return new Promise<void>((resolve, reject) => {
    const cleanup = () => {
      socket.removeEventListener('open', opened)
      socket.removeEventListener('error', failed)
      signal?.removeEventListener('abort', aborted)
    }
    const opened = () => {
      cleanup()
      resolve()
    }
    const failed = () => {
      cleanup()
      reject(new V2RelayReceiverError('Unable to open relay WebSocket'))
    }
    const aborted = () => {
      cleanup()
      socket.close(1000, 'join aborted')
      reject(signal?.reason ?? new DOMException('Relay join aborted', 'AbortError'))
    }
    socket.addEventListener('open', opened, { once: true })
    socket.addEventListener('error', failed, { once: true })
    signal?.addEventListener('abort', aborted, { once: true })
  })
}

function nextBinaryMessage(socket: V2WebSocketPort, signal?: AbortSignal): Promise<Uint8Array> {
  signal?.throwIfAborted()
  return new Promise<Uint8Array>((resolve, reject) => {
    const cleanup = () => {
      socket.removeEventListener('message', received)
      socket.removeEventListener('close', closed)
      socket.removeEventListener('error', failed)
      signal?.removeEventListener('abort', aborted)
    }
    const received = (event: MessageEvent) => {
      cleanup()
      if (!(event.data instanceof ArrayBuffer)) {
        reject(new V2RelayReceiverError('Relay handshake frame is not binary'))
        return
      }
      resolve(new Uint8Array(event.data))
    }
    const closed = () => {
      cleanup()
      reject(new V2RelayReceiverError('Relay closed before descriptor delivery'))
    }
    const failed = () => {
      cleanup()
      reject(new V2RelayReceiverError('Relay failed before descriptor delivery'))
    }
    const aborted = () => {
      cleanup()
      reject(signal?.reason ?? new DOMException('Relay join aborted', 'AbortError'))
    }
    socket.addEventListener('message', received, { once: true })
    socket.addEventListener('close', closed, { once: true })
    socket.addEventListener('error', failed, { once: true })
    signal?.addEventListener('abort', aborted, { once: true })
  })
}

function decodeRelayErrorIfPresent(encoded: Uint8Array): V2RelayErrorFrame | undefined {
  if (encoded.byteLength < 4) return undefined
  const magic = String.fromCharCode(...encoded.subarray(0, 4))
  return magic === 'WS2E' ? decodeV2RelayError(encoded) : undefined
}

function decodeSessionRetiredIfPresent(encoded: Uint8Array): Uint8Array | undefined {
  if (encoded.byteLength < 4) return undefined
  const magic = String.fromCharCode(...encoded.subarray(0, 4))
  return magic === 'WS2F' ? decodeV2SessionRetired(encoded).relaySessionId : undefined
}

function relayErrorMessage(error: V2RelayErrorFrame): string {
  if (error.code === V2_RELAY_ERROR.stopped) return 'This share was stopped permanently.'
  if (error.code === V2_RELAY_ERROR.starting) return 'This share is still starting.'
  if (error.code === V2_RELAY_ERROR.notFound) return 'This share is not currently available.'
  if (error.code === V2_RELAY_ERROR.admission) return 'The relay is currently at capacity.'
  return 'The relay rejected this receiver join.'
}

async function waitForBufferCapacity(socket: V2WebSocketPort, signal?: AbortSignal): Promise<void> {
  while (socket.bufferedAmount > MAXIMUM_BUFFERED_BYTES) {
    if (socket.readyState !== WEBSOCKET_OPEN) {
      throw new V2RelayReceiverError('Relay closed while applying send backpressure')
    }
    await delay(BUFFER_DRAIN_INTERVAL_MILLISECONDS, signal)
  }
  if (socket.readyState !== WEBSOCKET_OPEN) {
    throw new V2RelayReceiverError('Relay closed before the buffered frame could be sent')
  }
}

async function waitForBufferDrain(socket: V2WebSocketPort, signal?: AbortSignal): Promise<void> {
  while (socket.bufferedAmount > 0) {
    if (socket.readyState !== WEBSOCKET_OPEN) {
      throw new V2RelayReceiverError('Relay closed before terminal delivery drained')
    }
    await delay(BUFFER_DRAIN_INTERVAL_MILLISECONDS, signal)
  }
  if (socket.readyState !== WEBSOCKET_OPEN) {
    throw new V2RelayReceiverError('Relay closed while confirming terminal delivery')
  }
}

function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted()
  return new Promise<void>((resolve, reject) => {
    const cleanup = () => signal?.removeEventListener('abort', aborted)
    const timeout = globalThis.setTimeout(() => {
      cleanup()
      resolve()
    }, milliseconds)
    const aborted = () => {
      globalThis.clearTimeout(timeout)
      cleanup()
      reject(signal?.reason ?? new DOMException('Operation aborted', 'AbortError'))
    }
    signal?.addEventListener('abort', aborted, { once: true })
  })
}

function awaitSocketClose(socket: V2WebSocketPort, timeoutMilliseconds: number): Promise<void> {
  if (socket.readyState === WEBSOCKET_CLOSED) return Promise.resolve()
  return new Promise<void>((resolve) => {
    const finish = () => {
      globalThis.clearTimeout(timeout)
      socket.removeEventListener('close', finish)
      resolve()
    }
    const timeout = globalThis.setTimeout(finish, timeoutMilliseconds)
    socket.addEventListener('close', finish, { once: true })
  })
}
