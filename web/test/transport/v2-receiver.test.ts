import { afterEach, describe, expect, it, vi } from 'vitest'

import { DEFAULT_RELAY_RECEIVE_CAPACITY } from '../../src/transport/relay/receiver/capacity'
import { decodeDeliveryJoin } from '../../src/transport/relay/delivery-handshake-codec'
import { encodeSettlementNotice } from '../../src/transport/relay/delivery-settlement-codec'
import {
  dialRelayReceiver,
  RELAY_HEARTBEAT_CLOSE_CODE,
  RELAY_PROTOCOL_CLOSE_CODE,
} from '../../src/transport/relay/receiver'
import {
  decodeConnectionProbe,
  encodeConnectionProbeAck,
  encodeOpaqueRoute,
  encodeSessionCredit,
} from '../../src/transport/relay/protocol'

import { capability, FakeSocket, relaySessionId } from './relay-receiver-fixture'

afterEach(() => vi.useRealTimers())

describe('v2 relay receiver ingress', () => {
  it('routes the socket and JOIN with the same raw share identity', async () => {
    const socket = new FakeSocket()
    const socketFactory = vi.fn(() => socket)
    const mutableCapability = { ...capability, shareIdRaw: capability.shareIdRaw.slice() }
    const pending = dialRelayReceiver('https://relay.invalid/base?token=a%20b', mutableCapability, {
      socketFactory,
    })
    mutableCapability.shareIdRaw.fill(9)
    const connection = await pending
    try {
      expect(socketFactory).toHaveBeenCalledExactlyOnceWith(
        'wss://relay.invalid/base/v2/ws?token=a%20b&share_id=AwMDAwMDAwMDAwMD',
      )
      expect(decodeDeliveryJoin(socket.sent[0]!).shareId).toEqual(capability.shareIdRaw)
      expect(connection.endpoint.relayIdentityEndpoint).toBe('wss://relay.invalid/base/v2/ws')
    } finally {
      await connection.close()
    }
  })

  it.each(['share_id=other', 'share%5fid=AwMDAwMDAwMDAwMD', 'share_id=x&share_id=y'])(
    'rejects a preconfigured route before opening a socket: %s', async query => {
      const socketFactory = vi.fn(() => new FakeSocket())
      await expect(dialRelayReceiver(`https://relay.invalid?${query}`, capability, {
        socketFactory,
      })).rejects.toThrow(/share_id/u)
      expect(socketFactory).not.toHaveBeenCalled()
    },
  )

  it('fails a silently disconnected relay without waiting for a WebSocket close event', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const read = connection.channel.frames.getReader().read()
    const rejected = expect(read).rejects.toMatchObject({ name: 'RelayHeartbeatError' })
    await vi.advanceTimersByTimeAsync(60_000)
    await rejected
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBe(RELAY_HEARTBEAT_CLOSE_CODE)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('fails the lane instead of buffering beyond its ciphertext frame budget', async () => {
    const socket = new FakeSocket()
    const connection = await dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const opaque = encodeOpaqueRoute({
      relaySessionId,
      ciphertext: new Uint8Array(65_536).fill(7),
    })

    const permitted = Math.floor(Number(DEFAULT_RELAY_RECEIVE_CAPACITY.encodedBytes) / opaque.byteLength)
    expect(permitted).toBeLessThanOrEqual(Number(DEFAULT_RELAY_RECEIVE_CAPACITY.frames))
    for (let index = 0; index < permitted; index += 1) {
      socket.message(opaque)
    }
    expect(socket.closeCode).toBeUndefined()
    socket.message(opaque)
    expect(socket.closeCode).toBe(RELAY_PROTOCOL_CLOSE_CODE)

    const reader = connection.channel.frames.getReader()
    await expect(reader.read()).rejects.toThrow(/capacity/)
  })

  it('bounds a relay that opens but withholds descriptor delivery', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket(false)
    const pending = dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const rejected = expect(pending).rejects.toMatchObject({ name: 'TimeoutError' })
    for (let turn = 0; turn < 8; turn += 1) await Promise.resolve()
    await vi.advanceTimersByTimeAsync(30_000)

    await rejected
  })

  it('settles an empty retired receiver channel before closing', async () => {
    const socket = new FakeSocket()
    const connection = await dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const reader = connection.channel.frames.getReader()

    socket.message(encodeSettlementNotice({ kind: 'settlement-notice', totalFrames: 0n, totalEncodedBytes: 0n, relaySessionId }))
    await settleSends()
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBe(1000)
    await expect(reader.read()).resolves.toEqual({ done: true, value: undefined })

    socket.message(encodeSettlementNotice({ kind: 'settlement-notice', totalFrames: 0n, totalEncodedBytes: 0n, relaySessionId }))
    expect(socket.closeCode).toBe(1000)
  })

  it('fails the receiver link on a malformed retirement control', async () => {
    const socket = new FakeSocket()
    const connection = await dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const malformed = encodeSettlementNotice({ kind: 'settlement-notice', totalFrames: 0n, totalEncodedBytes: 0n, relaySessionId })
    malformed[5] = 1

    socket.message(malformed)
    expect(socket.closeCode).toBe(RELAY_PROTOCOL_CLOSE_CODE)
    await expect(connection.channel.frames.getReader().read()).rejects.toMatchObject({ kind: 'malformed' })
  })
})

const wireBytes = (frame: Uint8Array) => encodeOpaqueRoute({ relaySessionId, ciphertext: frame }).byteLength
const connect = (socket: FakeSocket) => dialRelayReceiver('https://relay.invalid', capability, { socketFactory: () => socket })
async function settleSends(): Promise<void> {
  const promiseTurns = 20
  for (let turn = 0; turn < promiseTurns; turn += 1) await Promise.resolve()
}

describe('v2 relay receiver send credit', () => {
  it('installs its credit listener before descriptor handling yields', async () => {
    const socket = new FakeSocket()
    const frame = Uint8Array.of(7)
    socket.initialCredit = { frames: 1, bytes: wireBytes(frame) }
    const connection = await connect(socket)
    await connection.channel.send(frame)
    expect(socket.opaqueFrames).toEqual([frame])
    await connection.close()
  })

  it('waits for both frame and complete encoded-byte credit and owns caller bytes', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7, 8)
    const completed = vi.fn()
    const pending = connection.channel.send(frame).then(completed)
    const length = wireBytes(frame)
    frame.fill(9)
    await settleSends()
    expect(socket.opaqueFrames).toEqual([])
    socket.grant(1, length - 1)
    await settleSends()
    expect(completed).not.toHaveBeenCalled()
    socket.grant(0, 1)
    await pending
    expect(socket.opaqueFrames).toEqual([Uint8Array.of(7, 8)])
    await connection.close()
  })

  it('cancels a queued send promptly without allowing successors to overtake the active writer', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(1, 2)
    const first = connection.channel.send(frame)
    const abort = new AbortController()
    const canceled = connection.channel.send(Uint8Array.of(3), abort.signal)
    const rejected = expect(canceled).rejects.toThrow('queued canceled')
    const last = connection.channel.send(Uint8Array.of(4))
    await settleSends()
    abort.abort(new Error('queued canceled'))
    await rejected

    socket.grant(1, wireBytes(frame) - 1)
    await settleSends()
    expect(socket.opaqueFrames).toEqual([])
    socket.grant(0, 1)
    await first
    expect(socket.opaqueFrames).toEqual([frame])
    socket.grant(1, wireBytes(Uint8Array.of(4)))
    await last
    expect(socket.opaqueFrames).toEqual([frame, Uint8Array.of(4)])
    await connection.close()
  })

  it('cancels local buffer pressure without consuming granted credit', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7)
    socket.bufferedAmount = 4 << 20
    socket.grant(1, wireBytes(frame))
    const abort = new AbortController()
    const pending = connection.channel.send(frame, abort.signal)
    const rejected = expect(pending).rejects.toThrow('buffer canceled')
    await settleSends()
    abort.abort(new Error('buffer canceled'))
    await rejected
    expect(socket.opaqueFrames).toEqual([])
    socket.bufferedAmount = 0
    await connection.channel.send(frame)
    expect(socket.opaqueFrames).toEqual([frame])
    await connection.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('wakes buffer pressure immediately when the channel closes', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7)
    socket.bufferedAmount = 4 << 20
    socket.grant(1, wireBytes(frame))
    const pending = connection.channel.send(frame)
    const rejected = expect(pending).rejects.toThrow(/closed/)
    await settleSends()
    await connection.close()
    await rejected
    expect(socket.opaqueFrames).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
  })

  it('detects silent loss and wakes a credit wait without another grant or close event', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const pending = connection.channel.send(Uint8Array.of(7))
    const rejected = expect(pending).rejects.toMatchObject({ name: 'RelayHeartbeatError' })
    await vi.advanceTimersByTimeAsync(60_000)
    await rejected
    expect(socket.closeCode).toBe(RELAY_HEARTBEAT_CLOSE_CODE)
    expect(socket.opaqueFrames).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
  })

  it('treats accepted frames as liveness while probe acknowledgements stay queued', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const inbound = encodeOpaqueRoute({ relaySessionId, ciphertext: Uint8Array.of(9) })
    for (let elapsed = 0; elapsed < 180_000; elapsed += 15_000) {
      await vi.advanceTimersByTimeAsync(15_000)
      socket.message(inbound)
    }
    expect(connection.channel.state).toBe('open')
    await connection.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('does not debit a grant when the caller cancels before the physical send', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7)
    const abort = new AbortController()
    const pending = connection.channel.send(frame, abort.signal)
    const rejected = expect(pending).rejects.toThrow('credit canceled')
    await settleSends()
    socket.grant(1, wireBytes(frame))
    abort.abort(new Error('credit canceled'))
    await rejected
    await connection.channel.send(frame)
    expect(socket.opaqueFrames).toEqual([frame])
    await connection.close()
  })

  it.each(['local', 'remote', 'error', 'retired'] as const)('wakes pending and queued credit waits on %s closure', async cause => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const pending = [connection.channel.send(Uint8Array.of(1)), connection.channel.send(Uint8Array.of(2))]
    const rejected = pending.map(send => expect(send).rejects.toThrow(/closed|WebSocket failed/))
    await settleSends()
    if (cause === 'local') await connection.close()
    if (cause === 'remote') socket.close()
    if (cause === 'error') socket.fail()
    if (cause === 'retired') socket.message(encodeSettlementNotice({ kind: 'settlement-notice', totalFrames: 0n, totalEncodedBytes: 0n, relaySessionId }))
    await Promise.all(rejected)
    expect(socket.opaqueFrames).toEqual([])
    expect(connection.channel.state).toBe('closed')
  })

  it('keeps heartbeat and credit responsive while the content receive queue is full', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const trace = vi.fn()
    const connection = await dialRelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket, heartbeatTrace: trace,
    })
    const frame = Uint8Array.of(7)
    for (let index = 0; index < Number(DEFAULT_RELAY_RECEIVE_CAPACITY.frames); index += 1) {
      socket.message(encodeOpaqueRoute({ relaySessionId, ciphertext: frame }))
    }
    const pending = connection.channel.send(frame)
    await vi.advanceTimersByTimeAsync(15_000)
    const probe = socket.sent.at(-1)!
    const nonce = decodeConnectionProbe(probe)
    expect(socket.opaqueFrames).toEqual([])
    socket.message(encodeConnectionProbeAck(nonce))
    expect(trace).toHaveBeenLastCalledWith(expect.objectContaining({ stage: 'acknowledged' }))
    socket.grant(1, wireBytes(frame))
    await pending
    expect(socket.opaqueFrames).toEqual([frame])
    expect(connection.channel.state).toBe('open')
    await connection.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['frame overflow', 'byte overflow', 'wrong session', 'malformed'] as const)('fails pending sends on %s credit', async invalid => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7)
    const pending = connection.channel.send(frame)
    const rejected = expect(pending).rejects.toThrow(/receiver window|another receiver session|WS2W/)
    const read = expect(connection.channel.frames.getReader().read()).rejects.toThrow()
    await settleSends()
    if (invalid === 'frame overflow') { socket.grant(64, 0); socket.grant(1, 0) }
    if (invalid === 'byte overflow') { socket.grant(0, 4 << 20); socket.grant(0, 1) }
    if (invalid === 'wrong session') {
      const other = relaySessionId.slice()
      other[0] = 2
      socket.message(encodeSessionCredit({ relaySessionId: other, frames: 1, bytes: wireBytes(frame) }))
    }
    if (invalid === 'malformed') {
      const malformed = encodeSessionCredit({ relaySessionId, frames: 1, bytes: wireBytes(frame) })
      malformed[5] = 1
      socket.message(malformed)
    }
    await Promise.all([rejected, read])
    expect(socket.closeCode).toBe(RELAY_PROTOCOL_CLOSE_CODE)
    expect(socket.opaqueFrames).toEqual([])
  })

  it('ends the channel and wakes the next sender when a physical send throws', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const frame = Uint8Array.of(7)
    const reason = new Error('physical write failed')
    socket.grant(2, 2 * wireBytes(frame))
    socket.sendFailure = reason
    const pending = connection.channel.send(frame)
    const next = connection.channel.send(frame)
    await Promise.all([expect(pending).rejects.toBe(reason), expect(next).rejects.toBe(reason)])
    await expect(connection.channel.frames.getReader().read()).rejects.toBe(reason)
    expect(connection.channel.state).toBe('closed')
  })

  it('credits terminal frames and preserves their final position before closing', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    const first = Uint8Array.of(1)
    const terminal = Uint8Array.of(2)
    const ordinary = connection.channel.send(first)
    const end = connection.channel.sendTerminal(terminal)
    const late = expect(connection.channel.send(Uint8Array.of(3))).rejects.toThrow(/closed/)
    socket.grant(2, wireBytes(first) + wireBytes(terminal))
    await Promise.all([ordinary, end, late])
    expect(socket.opaqueFrames).toEqual([first, terminal])
    expect(connection.channel.state).toBe('closed')
  })

  it('rejects invalid frame sizes immediately without waiting for credit or mutating the channel', async () => {
    const socket = new FakeSocket()
    const connection = await connect(socket)
    await expect(connection.channel.send(new Uint8Array())).rejects.toThrow(/invalid length/)
    await expect(connection.channel.send(new Uint8Array(65_537))).rejects.toThrow(/invalid length/)
    expect(socket.opaqueFrames).toEqual([])
    expect(connection.channel.state).toBe('open')
    await connection.close()
  })
})
