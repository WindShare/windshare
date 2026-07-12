export type RelaySocketState = 'connecting' | 'open' | 'closed'

export type RelaySocketMessage =
  | { readonly type: 'text'; readonly data: string }
  | { readonly type: 'binary'; readonly data: Uint8Array }

export interface RelaySocket {
  readonly state: RelaySocketState
  readonly messages: ReadableStream<RelaySocketMessage>
  sendText(text: string, signal?: AbortSignal): Promise<void>
  sendBinary(data: Uint8Array, signal?: AbortSignal): Promise<void>
  close(): Promise<void>
}

export interface RelaySocketFactory {
  connect(url: string, signal?: AbortSignal): Promise<RelaySocket>
}

export class RelaySocketClosedError extends Error {
  constructor(message = 'relay socket is closed') {
    super(message)
    this.name = 'RelaySocketClosedError'
  }
}

export class RelaySocketConnectError extends Error {
  constructor(message = 'relay WebSocket connection failed') {
    super(message)
    this.name = 'RelaySocketConnectError'
  }
}

export class RelaySocketIngressError extends Error {
  constructor(message = 'relay socket inbound queue is full', options?: ErrorOptions) {
    super(message, options)
    this.name = 'RelaySocketIngressError'
  }
}

const MANIFEST_ENVELOPE_PREFIX_BYTES = 1
const DEFAULT_MAX_BUFFERED_BYTES = 64 * 1024
const DEFAULT_INGRESS_MESSAGES = 64
const DEFAULT_MAX_BINARY_MESSAGE_BYTES = Math.max(
  MANIFEST_ENVELOPE_PREFIX_BYTES + MAX_SEALED_MANIFEST_BYTES,
  ROUTED_ENVELOPE_BYTES + MAX_FRAME_BYTES,
)
const BACKPRESSURE_POLL_MS = 4
const CLOSE_WAIT_MS = 1_000
const UTF8_ENCODER = new TextEncoder()

// Browser WebSocket.close only permits 1000 or application-defined 3000..4999
// codes. Using the RFC status 1009 directly would throw before closing the socket.
export const RELAY_INGRESS_CLOSE_CODE = 4_009

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('Operation aborted', 'AbortError')
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted === true) {
    throw abortReason(signal)
  }
}

function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    throwIfAborted(signal)
    const timer = setTimeout(finish, milliseconds)
    function finish(): void {
      signal?.removeEventListener('abort', cancel)
      resolve()
    }
    function cancel(): void {
      clearTimeout(timer)
      signal?.removeEventListener('abort', cancel)
      reject(signal === undefined ? new Error('aborted') : abortReason(signal))
    }
    signal?.addEventListener('abort', cancel, { once: true })
  })
}

export interface BrowserRelaySocketOptions {
  readonly maxBufferedBytes?: number
  readonly ingressMessages?: number
  readonly maxTextMessageBytes?: number
  readonly maxBinaryMessageBytes?: number
}

function checkedPositiveInteger(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new RangeError(`${label} must be a positive safe integer`)
  }
  return value
}

function checkedIngressLimit(value: number, ceiling: number, label: string): number {
  const checked = checkedPositiveInteger(value, label)
  if (checked > ceiling) {
    throw new RangeError(`${label} must not exceed the protocol limit ${ceiling}`)
  }
  return checked
}

/** Browser WebSocket adapter with cancellation-aware bufferedAmount backpressure. */
export class BrowserRelaySocket implements RelaySocket {
  readonly #socket: WebSocket
  readonly #maxBufferedBytes: number
  readonly #maxTextMessageBytes: number
  readonly #maxBinaryMessageBytes: number
  readonly #closed = new AbortController()
  readonly messages: ReadableStream<RelaySocketMessage>
  #state: RelaySocketState = 'open'
  #controller: ReadableStreamDefaultController<RelaySocketMessage> | undefined
  #ingressChain = Promise.resolve()
  #streamSettled = false
  #closePromise: Promise<void> | undefined

  constructor(socket: WebSocket, options: BrowserRelaySocketOptions = {}) {
    if (socket.readyState !== WebSocket.OPEN) {
      throw new TypeError('browser relay socket requires an open WebSocket')
    }
    this.#socket = socket
    this.#socket.binaryType = 'arraybuffer'
    this.#maxBufferedBytes = checkedPositiveInteger(
      options.maxBufferedBytes ?? DEFAULT_MAX_BUFFERED_BYTES,
      'maximum buffered bytes',
    )
    this.#maxTextMessageBytes = checkedIngressLimit(
      options.maxTextMessageBytes ?? MAX_SIGNALING_MESSAGE_BYTES,
      MAX_SIGNALING_MESSAGE_BYTES,
      'maximum text message bytes',
    )
    this.#maxBinaryMessageBytes = checkedIngressLimit(
      options.maxBinaryMessageBytes ?? DEFAULT_MAX_BINARY_MESSAGE_BYTES,
      DEFAULT_MAX_BINARY_MESSAGE_BYTES,
      'maximum binary message bytes',
    )
    const ingressMessages = checkedPositiveInteger(
      options.ingressMessages ?? DEFAULT_INGRESS_MESSAGES,
      'ingress message capacity',
    )
    this.messages = new ReadableStream<RelaySocketMessage>(
      {
        start: (controller) => {
          this.#controller = controller
          socket.addEventListener('message', this.#onMessage)
          socket.addEventListener('close', this.#onClose)
          socket.addEventListener('error', this.#onError)
        },
        cancel: () => {
          this.#streamSettled = true
          return this.close()
        },
      },
      { highWaterMark: ingressMessages },
    )
  }

  get state(): RelaySocketState {
    return this.#state
  }

  async sendText(text: string, signal?: AbortSignal): Promise<void> {
    await this.#send(text, signal)
  }

  async sendBinary(data: Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.#send(data.slice(), signal)
  }

  async #send(data: string | Uint8Array, signal?: AbortSignal): Promise<void> {
    throwIfAborted(signal)
    while (this.#socket.bufferedAmount >= this.#maxBufferedBytes) {
      this.#requireOpen()
      const waitSignal = signal === undefined
        ? this.#closed.signal
        : AbortSignal.any([signal, this.#closed.signal])
      await delay(BACKPRESSURE_POLL_MS, waitSignal)
    }
    this.#requireOpen()
    this.#socket.send(typeof data === 'string' ? data : Uint8Array.from(data))
  }

  #requireOpen(): void {
    if (this.#state !== 'open' || this.#socket.readyState !== WebSocket.OPEN) {
      throw new RelaySocketClosedError()
    }
  }

  #onMessage = (event: MessageEvent<unknown>): void => {
    // Blob conversion is sequenced because independent arrayBuffer promises could
    // otherwise reorder a reliable WebSocket stream.
    this.#ingressChain = this.#ingressChain
      .then(() => this.#acceptMessage(event.data))
      .catch((cause: unknown) => {
        const error = cause instanceof RelaySocketIngressError
          ? cause
          : new RelaySocketIngressError('relay WebSocket ingress conversion failed', {
              cause,
            })
        this.#fail(error)
      })
  }

  async #acceptMessage(data: unknown): Promise<void> {
    if (this.#state !== 'open') {
      return
    }
    if (typeof data === 'string') {
      if (
        data.length > this.#maxTextMessageBytes ||
        UTF8_ENCODER.encode(data).byteLength > this.#maxTextMessageBytes
      ) {
        throw new RelaySocketIngressError('relay WebSocket text message is too large')
      }
      this.#enqueue({ type: 'text', data })
      return
    }
    if (data instanceof ArrayBuffer) {
      this.#requireBinarySize(data.byteLength)
      this.#enqueue({ type: 'binary', data: new Uint8Array(data).slice() })
      return
    }
    if (data instanceof Blob) {
      this.#requireBinarySize(data.size)
      const bytes = new Uint8Array(await data.arrayBuffer())
      this.#requireBinarySize(bytes.byteLength)
      this.#enqueue({ type: 'binary', data: bytes })
      return
    }
    throw new RelaySocketIngressError('relay WebSocket delivered an unsupported message value')
  }

  #requireBinarySize(byteLength: number): void {
    if (byteLength > this.#maxBinaryMessageBytes) {
      throw new RelaySocketIngressError('relay WebSocket binary message is too large')
    }
  }

  #enqueue(message: RelaySocketMessage): void {
    if (this.#state !== 'open') {
      return
    }
    const controller = this.#controller
    if (controller === undefined || (controller.desiredSize ?? 0) <= 0) {
      this.#fail(new RelaySocketIngressError())
      return
    }
    controller.enqueue(message)
  }

  #onClose = (): void => {
    if (this.#state === 'closed') {
      return
    }
    this.#state = 'closed'
    this.#closed.abort(new RelaySocketClosedError('relay WebSocket closed remotely'))
    this.#detach()
    this.#closeReadable()
  }

  #onError = (): void => {
    this.#fail(new RelaySocketClosedError('relay WebSocket failed'))
  }

  #fail(reason: unknown): void {
    if (this.#state === 'closed') {
      return
    }
    this.#state = 'closed'
    this.#closed.abort(reason)
    this.#detach()
    this.#errorReadable(reason)
    this.#closePromise = this.#waitForPhysicalClose(
      RELAY_INGRESS_CLOSE_CODE,
      'relay ingress failure',
    )
  }

  #detach(): void {
    this.#socket.removeEventListener('message', this.#onMessage)
    this.#socket.removeEventListener('close', this.#onClose)
    this.#socket.removeEventListener('error', this.#onError)
  }

  close(): Promise<void> {
    if (this.#closePromise !== undefined) {
      return this.#closePromise
    }
    if (this.#state === 'closed') {
      return Promise.resolve()
    }
    const operation = this.#closePhysical()
    this.#closePromise = operation
    return operation
  }

  async #closePhysical(): Promise<void> {
    this.#state = 'closed'
    this.#closed.abort(new RelaySocketClosedError())
    this.#detach()
    this.#closeReadable()
    await this.#waitForPhysicalClose(1000, 'normal closure')
  }

  #waitForPhysicalClose(code: number, reason: string): Promise<void> {
    if (this.#socket.readyState === WebSocket.CLOSED) {
      return Promise.resolve()
    }
    return new Promise<void>((resolve, reject) => {
      const finish = () => {
        clearTimeout(timeout)
        this.#socket.removeEventListener('close', finish)
        resolve()
      }
      const timeout = setTimeout(finish, CLOSE_WAIT_MS)
      this.#socket.addEventListener('close', finish, { once: true })
      try {
        this.#socket.close(code, reason)
      } catch (error) {
        clearTimeout(timeout)
        this.#socket.removeEventListener('close', finish)
        reject(error)
      }
    })
  }

  #closeReadable(): void {
    if (this.#streamSettled) {
      return
    }
    this.#streamSettled = true
    this.#controller?.close()
  }

  #errorReadable(reason: unknown): void {
    if (this.#streamSettled) {
      return
    }
    this.#streamSettled = true
    this.#controller?.error(reason)
  }
}

export type BrowserWebSocketFactory = (url: string) => WebSocket

export class BrowserRelaySocketFactory implements RelaySocketFactory {
  readonly #createWebSocket: BrowserWebSocketFactory
  readonly #options: BrowserRelaySocketOptions

  constructor(
    createWebSocket: BrowserWebSocketFactory = (url) => new WebSocket(url),
    options: BrowserRelaySocketOptions = {},
  ) {
    this.#createWebSocket = createWebSocket
    this.#options = options
  }

  async connect(url: string, signal?: AbortSignal): Promise<RelaySocket> {
    throwIfAborted(signal)
    let socket: WebSocket
    try {
      socket = this.#createWebSocket(url)
    } catch {
      // Browser constructor errors can embed the complete credential-bearing URL.
      throw new RelaySocketConnectError()
    }
    await this.#waitUntilOpen(socket, signal)
    return new BrowserRelaySocket(socket, this.#options)
  }

  #waitUntilOpen(socket: WebSocket, signal?: AbortSignal): Promise<void> {
    if (socket.readyState === WebSocket.OPEN) {
      return Promise.resolve()
    }
    if (socket.readyState !== WebSocket.CONNECTING) {
      socket.close()
      return Promise.reject(
        new RelaySocketClosedError('relay WebSocket closed during connection'),
      )
    }
    return new Promise((resolve, reject) => {
      const cleanup = () => {
        socket.removeEventListener('open', opened)
        socket.removeEventListener('error', failed)
        socket.removeEventListener('close', failed)
        signal?.removeEventListener('abort', aborted)
      }
      const opened = () => {
        cleanup()
        resolve()
      }
      const failed = () => {
        cleanup()
        socket.close()
        reject(new RelaySocketClosedError('relay WebSocket closed during connection'))
      }
      const aborted = () => {
        cleanup()
        socket.close()
        reject(signal === undefined ? new Error('aborted') : abortReason(signal))
      }
      socket.addEventListener('open', opened, { once: true })
      socket.addEventListener('error', failed, { once: true })
      socket.addEventListener('close', failed, { once: true })
      signal?.addEventListener('abort', aborted, { once: true })
    })
  }
}
import { MAX_FRAME_BYTES } from '../../contracts/channel'
import { MAX_SEALED_MANIFEST_BYTES } from '../../contracts/manifest'
import {
  MAX_SIGNALING_MESSAGE_BYTES,
  ROUTED_ENVELOPE_BYTES,
} from './protocol'
