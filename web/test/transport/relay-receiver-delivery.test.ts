import { afterEach, describe, expect, it, vi } from 'vitest'
import { decodeDeliveryConsumed } from '../../src/transport/relay/delivery-consumed-codec'
import { decodeDeliveryJoin, type RelayReceiveCapacity } from '../../src/transport/relay/delivery-handshake-codec'
import { decodeSettlementAck, encodeSettlementNotice } from '../../src/transport/relay/delivery-settlement-codec'
import type { ReceiveLedgerEvent } from '../../src/contracts/relay-delivery-observation'
import { dialV2RelayReceiver, V2_RELAY_PROTOCOL_CLOSE_CODE, type V2RelayReceiverConnection } from '../../src/transport/relay/v2-receiver'
import { encodeV2OpaqueRoute } from '../../src/transport/relay/v2-protocol'
import { capability, FakeSocket, relaySessionId } from './relay-receiver-fixture'

const frame = Uint8Array.of(7, 8)
const envelope = encodeV2OpaqueRoute({ relaySessionId, ciphertext: frame })
const connections: V2RelayReceiverConnection[] = []
const controls = (socket: FakeSocket, magic: string) => socket.sent.filter(bytes =>
  String.fromCharCode(...bytes.subarray(0, 4)) === magic)
const notice = (frames: number, id = relaySessionId) => encodeSettlementNotice({
  kind: 'settlement-notice', relaySessionId: id,
  totalFrames: BigInt(frames), totalEncodedBytes: BigInt(frames * envelope.byteLength),
})
const totals = (frames: number) => ({
  relaySessionId, totalFrames: BigInt(frames), totalEncodedBytes: BigInt(frames * envelope.byteLength),
})
async function writerTurn(): Promise<void> {
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve()
}
async function connect(socket: FakeSocket, options: {
  receiveCapacity?: RelayReceiveCapacity
  receiveTrace?: (event: ReceiveLedgerEvent) => void
} = {}): Promise<V2RelayReceiverConnection> {
  const connection = await dialV2RelayReceiver('https://relay.invalid', capability, { socketFactory: () => socket, ...options })
  connections.push(connection)
  return connection
}
afterEach(async () => {
  await Promise.all(connections.splice(0).map(connection => connection.close()))
  vi.useRealTimers()
})

describe('browser cumulative relay delivery', () => {
  it.each([
    { frames: 2n, encodedBytes: 1000n },
    { frames: 100n, encodedBytes: BigInt(2 * envelope.byteLength) },
  ])('bounds ingress with JOIN capacity $frames frames / $encodedBytes bytes', async capacity => {
    const socket = new FakeSocket()
    const supplied = { ...capacity }
    const connecting = connect(socket, { receiveCapacity: supplied })
    supplied.frames = 1000n
    supplied.encodedBytes = 100_000n
    const connection = await connecting
    expect(decodeDeliveryJoin(socket.sent[0]!).receiveCapacity).toEqual(capacity)
    socket.message(envelope)
    socket.message(envelope)
    await writerTurn()
    expect(controls(socket, 'WS2B')).toEqual([])
    expect(socket.closeCode).toBeUndefined()
    socket.message(envelope)
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)
    await expect(connection.channel.frames.getReader().read()).rejects.toMatchObject({ code: 'capacity' })
  })

  it('refuses an unrepresentable local capacity before creating a socket', async () => {
    const socketFactory = vi.fn(() => new FakeSocket())
    for (const frames of [0n, -1n, BigInt(Number.MAX_SAFE_INTEGER) + 1n]) {
      await expect(dialV2RelayReceiver('https://relay.invalid', capability, {
        socketFactory, receiveCapacity: { frames, encodedBytes: 100n },
      })).rejects.toMatchObject({ code: 'config' })
    }
    expect(socketFactory).not.toHaveBeenCalled()
  })

  it('keeps a one-envelope host window moving while outbound content has no grant', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const abort = new AbortController()
    const outbound = connection.channel.send(frame, abort.signal)
    const rejected = expect(outbound).rejects.toThrow('withdrawn')
    const reader = connection.channel.frames.getReader()
    for (let count = 1; count <= 4; count += 1) {
      socket.message(envelope)
      await writerTurn()
      expect(controls(socket, 'WS2B')).toHaveLength(count - 1)
      expect((await reader.read()).value).toEqual(frame)
      await writerTurn()
      expect(decodeDeliveryConsumed(controls(socket, 'WS2B').at(-1)!)).toEqual(totals(count))
    }
    expect(socket.opaqueFrames).toEqual([])
    abort.abort(new Error('withdrawn'))
    await rejected
  })

  it('coalesces consumption until socket readiness without charging feedback to content credit', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const reader = connection.channel.frames.getReader()
    socket.bufferedAmount = 4 << 20
    for (let count = 0; count < 3; count += 1) {
      socket.message(envelope)
      await reader.read()
    }
    await writerTurn()
    expect(controls(socket, 'WS2B')).toEqual([])
    socket.bufferedAmount = 0
    await vi.advanceTimersByTimeAsync(8)
    expect(controls(socket, 'WS2B').map(decodeDeliveryConsumed)).toEqual([totals(3)])
  })

  it('waits for the full accepted prefix to drain before acknowledging remote retirement', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const reader = connection.channel.frames.getReader()
    socket.message(envelope)
    socket.message(envelope)
    socket.message(notice(2))
    socket.message(notice(2))
    await writerTurn()
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBeUndefined()
    expect(controls(socket, 'WS2S')).toEqual([])
    expect((await reader.read()).value).toEqual(frame)
    await writerTurn()
    expect(controls(socket, 'WS2S')).toEqual([])
    expect((await reader.read()).value).toEqual(frame)
    await writerTurn()
    expect(controls(socket, 'WS2S').map(decodeSettlementAck)).toEqual([{ kind: 'settlement-ack', ...totals(2) }])
    expect(socket.closeCode).toBe(1000)
    expect((await reader.read()).done).toBe(true)
  })

  it('settles cancelled queued frames even under local socket pressure without reporting them as consumed', async () => {
    const socket = new FakeSocket()
    const observed: ReceiveLedgerEvent[] = []
    const connection = await connect(socket, { receiveTrace: event => observed.push(event) })
    const reader = connection.channel.frames.getReader()
    socket.message(envelope)
    socket.message(envelope)
    await reader.read()
    await writerTurn()
    socket.bufferedAmount = 4 << 20
    socket.message(notice(2))
    await reader.cancel()
    await writerTurn()
    expect(controls(socket, 'WS2B').map(decodeDeliveryConsumed)).toEqual([totals(1)])
    expect(controls(socket, 'WS2S').map(decodeSettlementAck)).toEqual([{ kind: 'settlement-ack', ...totals(2) }])
    expect(observed.find(event => event.action === 'settlement')?.progress).toMatchObject({
      consumed: { frames: 1n }, discarded: { frames: 1n },
    })
    expect(socket.closeCode).toBe(1000)
  })

  it('discards on owner close without inventing a missing settlement notice', async () => {
    const socket = new FakeSocket()
    const observed: ReceiveLedgerEvent[] = []
    const connection = await connect(socket, { receiveTrace: event => observed.push(event) })
    socket.message(envelope)
    await connection.close()
    expect(controls(socket, 'WS2B')).toEqual([])
    expect(controls(socket, 'WS2S')).toEqual([])
    expect(observed.find(event => event.action === 'discard')?.progress?.discarded.frames).toBe(1n)
    expect((await connection.channel.frames.getReader().read()).done).toBe(true)
  })

  it('ends feedback authority on disconnect while allowing the accepted prefix to drain', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    socket.message(envelope)
    socket.close()
    const reader = connection.channel.frames.getReader()
    expect((await reader.read()).value).toEqual(frame)
    expect((await reader.read()).done).toBe(true)
    await writerTurn()
    expect(controls(socket, 'WS2B')).toEqual([])
    expect(controls(socket, 'WS2S')).toEqual([])

    const replacement = new FakeSocket()
    const next = await connect(replacement)
    replacement.message(envelope)
    await next.channel.frames.getReader().read()
    await writerTurn()
    expect(controls(replacement, 'WS2B').map(decodeDeliveryConsumed)).toEqual([totals(1)])
  })

  it.each(['totals', 'session', 'late frame'] as const)('rejects invalid retirement %s without false settlement', async invalid => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    socket.message(envelope)
    if (invalid === 'totals') socket.message(notice(0))
    if (invalid === 'session') socket.message(notice(1, new Uint8Array(8).fill(9)))
    if (invalid === 'late frame') { socket.message(notice(1)); socket.message(envelope) }
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)
    await expect(connection.channel.frames.getReader().read()).rejects.toThrow()
    expect(controls(socket, 'WS2S')).toEqual([])
  })

  it('fails the generation if cumulative feedback cannot be physically submitted', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const reader = connection.channel.frames.getReader()
    socket.message(envelope)
    socket.message(envelope)
    const error = new Error('feedback write failed')
    socket.sendFailure = error
    await reader.read()
    await writerTurn()
    await expect(reader.read()).rejects.toBe(error)
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)
  })
})
