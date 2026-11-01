import { afterEach, expect, it, vi } from 'vitest'
import { encodeBlockRequest } from '../../src/content/flow'
import { OperationRouter } from '../../src/session/operation-router'
import { EnvelopeSealer } from '../../src/session/envelope'
import { decodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { OpaqueRelayFrameChannel } from '../../src/transport/relay/receiver/channel'
import { encodeSettlementNotice } from '../../src/transport/relay/delivery-settlement-codec'
import { encodeOpaqueRoute } from '../../src/transport/relay/protocol'
import { projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { fragmentRecord } from '../content/v2-fragment-fixture'
import { FakeSocket, relaySessionId } from '../transport/relay-receiver-fixture'
import { BINDING, KEY, id, runtimeOnChannel } from './v2-send-fixture'

afterEach(() => vi.restoreAllMocks())

class DeferredCloseSocket extends FakeSocket {
  override close(code?: number): void {
    this.closeCode = code
    this.readyState = 2
  }

  finishClose(): void { super.close(1000) }
}

it('withdraws a settled relay immediately, drains its authenticated prefix, and still owns delayed cleanup', async () => {
  const route = vi.spyOn(OperationRouter.prototype, 'route')
  const socket = new DeferredCloseSocket()
  const channel = new OpaqueRelayFrameChannel(socket, relaySessionId, { frames: 8n, encodedBytes: 100_000n })
  const { runtime, events } = runtimeOnChannel(channel)
  socket.grant(8, 100_000)
  try {
    const operation = await runtime.beginOperation(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(7), [0n]))
    const fragment = fragmentRecord(operation.id, Uint8Array.of(1, 2, 3))[0]!
    const sealer = new EnvelopeSealer(KEY, { ...BINDING, direction: 1 })
    const routed = encodeOpaqueRoute({ relaySessionId, ciphertext: await sealer.seal(fragment) })
    socket.message(routed)
    await vi.waitFor(() => expect(route).toHaveBeenCalledOnce())
    await route.mock.results[0]!.value

    socket.message(encodeSettlementNotice({ kind: 'settlement-notice', relaySessionId,
      totalFrames: 1n, totalEncodedBytes: BigInt(routed.byteLength) }))
    expect(channel.unavailable.aborted).toBe(true)
    expect(runtime.availableLaneIds()).toEqual([])
    expect(runtime.hasAttachedLanes).toBe(true)
    await expect(runtime.beginOperation(MESSAGE_KIND.listChildren, Uint8Array.of(0x80)))
      .rejects.toMatchObject({ scope: 'lane' })
    expect(socket.opaqueFrames).toHaveLength(1)

    // The receive pump has reached EOF while the application has not yet taken
    // its authenticated response. Detachment must preserve that response.
    await vi.waitFor(() => expect(socket.readyState).toBe(2))
    expect(runtime.hasAttachedLanes).toBe(true)
    await expect(operation.next()).resolves.toEqual(decodeMessage(fragment))
    await expect(operation.next()).rejects.toMatchObject({ scope: 'lane' })
    await vi.waitFor(() => expect(runtime.hasAttachedLanes).toBe(false))
    expect(socket.readyState).toBe(2)
    const transitions = events.filter(event => event.eventName === 'lane_transition').map(event => event.transition)
    expect(transitions).toEqual(['attached', 'unavailable', 'detached'])
    for (const event of events) expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(event))).not.toThrow()

    let closed = false
    const closing = runtime.close().then(() => { closed = true })
    await Promise.resolve()
    expect(closed).toBe(false)
    socket.finishClose()
    await closing
  } finally {
    socket.finishClose()
    await runtime.close()
  }
})

it('explicit stop can cancel a retained response and join cleanup without waiting for its consumer', async () => {
  const route = vi.spyOn(OperationRouter.prototype, 'route')
  const socket = new DeferredCloseSocket()
  const channel = new OpaqueRelayFrameChannel(socket, relaySessionId, { frames: 8n, encodedBytes: 100_000n })
  const { runtime } = runtimeOnChannel(channel)
  socket.grant(8, 100_000)
  try {
    const operation = await runtime.beginOperation(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(7), [0n]))
    const fragment = fragmentRecord(operation.id, Uint8Array.of(1, 2, 3))[0]!
    const sealer = new EnvelopeSealer(KEY, { ...BINDING, direction: 1 })
    const routed = encodeOpaqueRoute({ relaySessionId, ciphertext: await sealer.seal(fragment) })
    socket.message(routed)
    await vi.waitFor(() => expect(route).toHaveBeenCalledOnce())
    await route.mock.results[0]!.value
    socket.message(encodeSettlementNotice({ kind: 'settlement-notice', relaySessionId,
      totalFrames: 1n, totalEncodedBytes: BigInt(routed.byteLength) }))
    await vi.waitFor(() => expect(socket.readyState).toBe(2))
    const closing = runtime.close()
    socket.finishClose()
    await closing
    await expect(operation.next()).rejects.toMatchObject({ scope: 'session' })
  } finally {
    socket.finishClose()
    await runtime.close()
  }
})
