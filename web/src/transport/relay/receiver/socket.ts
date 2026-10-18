import { decodeV2RelayError, V2_RELAY_ERROR, type V2RelayErrorFrame } from '../v2-protocol'

export const WEBSOCKET_OPEN = 1
export const WEBSOCKET_CLOSING = 2
export const WEBSOCKET_CLOSED = 3
// Browser close() only accepts 1000 or application codes from 3000 to 4999.
export const V2_RELAY_HEARTBEAT_CLOSE_CODE = 4001
export const V2_RELAY_PROTOCOL_CLOSE_CODE = 4002
const BUFFER_DRAIN_INTERVAL_MILLISECONDS = 8
const MAXIMUM_BUFFERED_BYTES = 4 * 65_536

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

export class V2RelayReceiverError extends Error {
  readonly relayError: V2RelayErrorFrame | undefined

  constructor(message: string, options?: ErrorOptions & { readonly relayError?: V2RelayErrorFrame }) {
    super(message, options)
    this.name = 'V2RelayReceiverError'
    this.relayError = options?.relayError
  }
}

export function waitForSendTurn(previous: Promise<void>, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.reject(signal.reason)
  return new Promise<void>((resolve, reject) => {
    const aborted = () => reject(signal.reason)
    signal.addEventListener('abort', aborted, { once: true })
    previous.then(() => {
      signal.removeEventListener('abort', aborted)
      resolve()
    })
  })
}

export function decodeRelayErrorIfPresent(encoded: Uint8Array): V2RelayErrorFrame | undefined {
  if (encoded.byteLength < 4) return undefined
  const magic = String.fromCharCode(...encoded.subarray(0, 4))
  return magic === 'WS2E' ? decodeV2RelayError(encoded) : undefined
}

export function relayErrorMessage(error: V2RelayErrorFrame): string {
  if (error.code === V2_RELAY_ERROR.stopped) return 'This share was stopped permanently.'
  if (error.code === V2_RELAY_ERROR.starting) return 'This share is still starting.'
  if (error.code === V2_RELAY_ERROR.notFound) return 'This share is not currently available.'
  if (error.code === V2_RELAY_ERROR.admission) return 'The relay is currently at capacity.'
  return 'The relay rejected this receiver join.'
}

export async function waitForBufferCapacity(socket: V2WebSocketPort, signal?: AbortSignal): Promise<void> {
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

export async function waitForBufferDrain(socket: V2WebSocketPort, signal?: AbortSignal): Promise<void> {
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

export function awaitSocketClose(socket: V2WebSocketPort, timeoutMilliseconds: number): Promise<void> {
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
