import type { FrameChannel } from '../../contracts/channel'
import type { Suite02CapabilityKey } from '../../crypto/suite02-link'
import { v2RelayEndpointForShare, type V2RelayEndpoint } from './v2-endpoint'
import { decodeV2DescriptorDelivery } from './v2-protocol'
import { encodeDeliveryJoin, type RelayReceiveCapacity } from './delivery-handshake-codec'
import type { RelayHeartbeatTrace } from './heartbeat'
import type { ReceiveLedgerEvent } from '../../contracts/relay-delivery-observation'
import { receiverCapacity } from './receiver/capacity'
import { V2OpaqueRelayFrameChannel } from './receiver/channel'
import {
  V2RelayReceiverError, WEBSOCKET_OPEN, WEBSOCKET_CLOSING,
  decodeRelayErrorIfPresent, relayErrorMessage, type V2WebSocketFactory, type V2WebSocketPort,
} from './receiver/socket'
export {
  V2RelayReceiverError, V2_RELAY_HEARTBEAT_CLOSE_CODE, V2_RELAY_PROTOCOL_CLOSE_CODE,
  type V2WebSocketFactory, type V2WebSocketPort,
} from './receiver/socket'
export { V2_RELAY_CLOSE_TIMEOUT_MILLISECONDS } from './receiver/channel'
export const V2_RELAY_JOIN_TIMEOUT_MILLISECONDS = 30_000

export interface V2RelayReceiverConnection {
  readonly endpoint: V2RelayEndpoint
  readonly relaySessionId: Uint8Array<ArrayBuffer>
  readonly descriptorObject: Uint8Array<ArrayBuffer>
  readonly channel: FrameChannel
  close(): Promise<void>
}

export async function dialV2RelayReceiver(
  relayBase: string,
  capability: Suite02CapabilityKey,
  options: {
    readonly socketFactory?: V2WebSocketFactory
    readonly signal?: AbortSignal
    readonly heartbeatTrace?: (event: RelayHeartbeatTrace) => void
    readonly receiveCapacity?: RelayReceiveCapacity
    readonly receiveTrace?: (event: ReceiveLedgerEvent) => void
  } = {},
): Promise<V2RelayReceiverConnection> {
  const deadline = relayJoinDeadline(options.signal)
  try {
    deadline.signal.throwIfAborted()
    // Routing and JOIN must use the same identity across asynchronous URL preparation.
    const shareId = capability.shareIdRaw.slice()
    const capacity = receiverCapacity(options.receiveCapacity)
    const join = encodeDeliveryJoin({ shareId, receiveCapacity: capacity })
    const endpoint = await v2RelayEndpointForShare(relayBase, shareId)
    return await dialV2RelayReceiverOnce(
      endpoint, join, options.socketFactory ?? browserWebSocketFactory,
      deadline.signal, capacity, options.heartbeatTrace, options.receiveTrace,
    )
  } finally {
    deadline.close()
  }
}

async function dialV2RelayReceiverOnce(
  endpoint: V2RelayEndpoint,
  join: Uint8Array<ArrayBuffer>,
  socketFactory: V2WebSocketFactory,
  signal: AbortSignal,
  capacity: RelayReceiveCapacity,
  heartbeatTrace?: (event: RelayHeartbeatTrace) => void,
  receiveTrace?: (event: ReceiveLedgerEvent) => void,
): Promise<V2RelayReceiverConnection> {
  signal.throwIfAborted()
  const socket = socketFactory(endpoint.dialEndpoint)
  socket.binaryType = 'arraybuffer'
  try {
    await awaitSocketOpen(socket, signal)
    return await receiveDescriptor(socket, signal, () => socket.send(join), deliveryBytes => {
      const relayError = decodeRelayErrorIfPresent(deliveryBytes)
      if (relayError !== undefined) {
        throw new V2RelayReceiverError(relayErrorMessage(relayError), { relayError })
      }
      const delivery = decodeV2DescriptorDelivery(deliveryBytes)
      // Install the channel listener in the descriptor event itself: the relay
      // sends its initial credit next, before the caller resumes its join.
      const channel = new V2OpaqueRelayFrameChannel(socket, delivery.relaySessionId, capacity, heartbeatTrace, receiveTrace)
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
  }, V2_RELAY_JOIN_TIMEOUT_MILLISECONDS)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
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

function receiveDescriptor(
  socket: V2WebSocketPort,
  signal: AbortSignal,
  join: () => void,
  accept: (encoded: Uint8Array) => V2RelayReceiverConnection,
): Promise<V2RelayReceiverConnection> {
  signal.throwIfAborted()
  return new Promise<V2RelayReceiverConnection>((resolve, reject) => {
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
      try {
        resolve(accept(new Uint8Array(event.data)))
      } catch (error) {
        reject(error)
      }
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
    signal.addEventListener('abort', aborted, { once: true })
    try {
      join()
    } catch (error) {
      cleanup()
      reject(error)
    }
  })
}
