import type { FrameChannel } from '../../contracts/channel'
import type { Suite02CapabilityKey } from '../../crypto/suite02-link'
import { relayEndpointForShare, type RelayEndpoint } from './endpoint'
import { decodeDescriptorDelivery } from './protocol'
import { encodeDeliveryJoin, type RelayReceiveCapacity } from './delivery-handshake-codec'
import type { RelayHeartbeatTrace } from './heartbeat'
import type { ReceiveLedgerEvent } from '../../contracts/relay-delivery-observation'
import { receiverCapacity } from './receiver/capacity'
import { OpaqueRelayFrameChannel } from './receiver/channel'
import {
  RelayReceiverError, WEBSOCKET_OPEN, WEBSOCKET_CLOSING,
  decodeRelayErrorIfPresent, relayErrorMessage, type WebSocketFactory, type WebSocketPort,
} from './receiver/socket'
export {
  RelayReceiverError, RELAY_HEARTBEAT_CLOSE_CODE, RELAY_PROTOCOL_CLOSE_CODE,
  type WebSocketFactory, type WebSocketPort,
} from './receiver/socket'
export { RELAY_CLOSE_TIMEOUT_MILLISECONDS } from './receiver/channel'
export const RELAY_JOIN_TIMEOUT_MILLISECONDS = 30_000

export interface RelayReceiverConnection {
  readonly endpoint: RelayEndpoint
  readonly relaySessionId: Uint8Array<ArrayBuffer>
  readonly descriptorObject: Uint8Array<ArrayBuffer>
  readonly channel: FrameChannel
  close(): Promise<void>
}

export async function dialRelayReceiver(
  relayBase: string,
  capability: Suite02CapabilityKey,
  options: {
    readonly socketFactory?: WebSocketFactory
    readonly signal?: AbortSignal
    readonly heartbeatTrace?: (event: RelayHeartbeatTrace) => void
    readonly receiveCapacity?: RelayReceiveCapacity
    readonly receiveTrace?: (event: ReceiveLedgerEvent) => void
  } = {},
): Promise<RelayReceiverConnection> {
  const deadline = relayJoinDeadline(options.signal)
  try {
    deadline.signal.throwIfAborted()
    // Routing and JOIN must use the same identity across asynchronous URL preparation.
    const shareId = capability.shareIdRaw.slice()
    const capacity = receiverCapacity(options.receiveCapacity)
    const join = encodeDeliveryJoin({ shareId, receiveCapacity: capacity })
    const endpoint = await relayEndpointForShare(relayBase, shareId)
    return await dialRelayReceiverOnce(
      endpoint, join, options.socketFactory ?? browserWebSocketFactory,
      deadline.signal, capacity, options.heartbeatTrace, options.receiveTrace,
    )
  } finally {
    deadline.close()
  }
}

async function dialRelayReceiverOnce(
  endpoint: RelayEndpoint,
  join: Uint8Array<ArrayBuffer>,
  socketFactory: WebSocketFactory,
  signal: AbortSignal,
  capacity: RelayReceiveCapacity,
  heartbeatTrace?: (event: RelayHeartbeatTrace) => void,
  receiveTrace?: (event: ReceiveLedgerEvent) => void,
): Promise<RelayReceiverConnection> {
  signal.throwIfAborted()
  const socket = socketFactory(endpoint.dialEndpoint)
  socket.binaryType = 'arraybuffer'
  try {
    await awaitSocketOpen(socket, signal)
    return await receiveDescriptor(socket, signal, () => socket.send(join), deliveryBytes => {
      const relayError = decodeRelayErrorIfPresent(deliveryBytes)
      if (relayError !== undefined) {
        throw new RelayReceiverError(relayErrorMessage(relayError), { relayError })
      }
      const delivery = decodeDescriptorDelivery(deliveryBytes)
      // Install the channel listener in the descriptor event itself: the relay
      // sends its initial credit next, before the caller resumes its join.
      const channel = new OpaqueRelayFrameChannel(socket, delivery.relaySessionId, capacity, heartbeatTrace, receiveTrace)
      return Object.freeze({
        endpoint,
        relaySessionId: delivery.relaySessionId.slice(),
        descriptorObject: delivery.object.slice(),
        channel,
        close: () => channel.close(),
      })
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
  }, RELAY_JOIN_TIMEOUT_MILLISECONDS)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
  }
}

function browserWebSocketFactory(endpoint: string): WebSocketPort {
  // No subprotocol is offered: the endpoint is versioned by its exact path.
  return new WebSocket(endpoint)
}

function awaitSocketOpen(socket: WebSocketPort, signal?: AbortSignal): Promise<void> {
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
      reject(new RelayReceiverError('Unable to open relay WebSocket'))
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

function receiveDescriptor(
  socket: WebSocketPort,
  signal: AbortSignal,
  join: () => void,
  accept: (encoded: Uint8Array) => RelayReceiverConnection,
): Promise<RelayReceiverConnection> {
  signal.throwIfAborted()
  return new Promise<RelayReceiverConnection>((resolve, reject) => {
    const cleanup = () => {
      socket.removeEventListener('message', received)
      socket.removeEventListener('close', closed)
      socket.removeEventListener('error', failed)
      signal?.removeEventListener('abort', aborted)
    }
    const received = (event: MessageEvent) => {
      cleanup()
      if (!(event.data instanceof ArrayBuffer)) {
        reject(new RelayReceiverError('Relay handshake frame is not binary'))
        return
      }
      try {
        resolve(accept(new Uint8Array(event.data)))
      } catch (error) {
        reject(error)
      }
    }
    const closed = () => {
      cleanup()
      reject(new RelayReceiverError('Relay closed before descriptor delivery'))
    }
    const failed = () => {
      cleanup()
      reject(new RelayReceiverError('Relay failed before descriptor delivery'))
    }
    const aborted = () => {
      cleanup()
      reject(signal?.reason ?? new DOMException('Relay join aborted', 'AbortError'))
    }
    socket.addEventListener('message', received, { once: true })
    socket.addEventListener('close', closed, { once: true })
    socket.addEventListener('error', failed, { once: true })
    signal.addEventListener('abort', aborted, { once: true })
    try {
      join()
    } catch (error) {
      cleanup()
      reject(error)
    }
  })
}
