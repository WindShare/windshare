import { afterEach, describe, expect, it, vi } from 'vitest'

import { RELAY_RECEIVE_WINDOW_BYTES } from '../../src/transport/relay/receive-credit-codec'
import type { Suite02CapabilityKey } from '../../src/crypto/suite02-link'
import {
  dialV2RelayReceiver,
  V2_RELAY_RECEIVE_QUEUE_FRAMES,
  V2_RELAY_HEARTBEAT_CLOSE_CODE,
  V2_RELAY_PROTOCOL_CLOSE_CODE,
  type V2WebSocketPort,
} from '../../src/transport/relay/v2-receiver'
import {
  decodeV2ConnectionProbe,
  decodeV2OpaqueRoute,
  encodeV2ConnectionProbeAck,
  encodeV2DescriptorDelivery,
  encodeV2OpaqueRoute,
  encodeV2SessionCredit,
  encodeV2SessionRetired,
} from '../../src/transport/relay/v2-protocol'

const relaySessionId = Uint8Array.of(1, 0, 0, 0, 0, 0, 0, 1)
const capability: Suite02CapabilityKey = Object.freeze({
  suite: 2,
  readSecret: new Uint8Array(16).fill(1),
  pkHash: new Uint8Array(16).fill(2),
  shareIdRaw: new Uint8Array(12).fill(3),
  shareId: 'share',
})

class FakeSocket implements V2WebSocketPort {
  binaryType: BinaryType = 'arraybuffer'
  readyState = 1
  bufferedAmount = 0
  closeCode: number | undefined
  readonly sent: Uint8Array<ArrayBuffer>[] = []
  sendFailure: Error | undefined
  initialCredit: { frames: number; bytes: number } | undefined
  readonly #listeners = new Map<string, Set<(event: unknown) => void>>()
  readonly #descriptor: boolean

  constructor(descriptor = true) {
    this.#descriptor = descriptor
  }

  send(data: ArrayBufferView<ArrayBuffer>): void {
    if (this.sendFailure !== undefined) throw this.sendFailure
    const encoded = new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
    this.sent.push(encoded.slice())
    if (String.fromCharCode(...encoded.subarray(0, 4)) !== 'WS2J' || !this.#descriptor) return
    queueMicrotask(() => {
      this.message(encodeV2DescriptorDelivery({ relaySessionId, object: Uint8Array.of(1) }))
      if (this.initialCredit !== undefined) this.grant(this.initialCredit.frames, this.initialCredit.bytes)
    })
  }

  close(code?: number): void {
    if (code !== undefined && code !== 1000 && (code < 3000 || code > 4999)) {
      throw new DOMException('Invalid browser close code', 'InvalidAccessError')
    }
    this.closeCode = code
    this.readyState = 3
    this.#emit('close', {})
  }

  addEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
  ): void {
    const listeners = this.#listeners.get(type) ?? new Set()
    listeners.add(listener as (event: unknown) => void)
    this.#listeners.set(type, listeners)
  }

  removeEventListener<K extends keyof WebSocketEventMap>(
    type: K,
    listener: (event: WebSocketEventMap[K]) => void,
  ): void {
    this.#listeners.get(type)?.delete(listener as (event: unknown) => void)
  }

  message(bytes: Uint8Array): void {
    const data = bytes.slice().buffer
    this.#emit('message', { data })
  }

  grant(frames: number, bytes: number): void {
    this.message(encodeV2SessionCredit({ relaySessionId, frames, bytes }))
  }

  fail(): void {
    this.#emit('error', {})
  }

  get opaqueFrames(): Uint8Array[] {
    return this.sent
      .filter(encoded => String.fromCharCode(...encoded.subarray(0, 4)) === 'WS2O')
      .map(encoded => decodeV2OpaqueRoute(encoded).ciphertext)
  }

  #emit(type: string, event: unknown): void {
    // DOM dispatch does not invoke listeners added while the event is executing.
    for (const listener of [...this.#listeners.get(type) ?? []]) listener(event)
  }
}

afterEach(() => vi.useRealTimers())

describe('v2 relay receiver ingress', () => {
  it('fails a silently disconnected relay without waiting for a WebSocket close event', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const read = connection.channel.frames.getReader().read()
    const rejected = expect(read).rejects.toMatchObject({ name: 'RelayHeartbeatError' })
    await vi.advanceTimersByTimeAsync(60_000)
    await rejected
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBe(V2_RELAY_HEARTBEAT_CLOSE_CODE)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('fails the lane instead of buffering beyond its ciphertext frame budget', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const opaque = encodeV2OpaqueRoute({
      relaySessionId,
      ciphertext: new Uint8Array(65_536).fill(7),
    })

    const permitted = Math.floor(RELAY_RECEIVE_WINDOW_BYTES / opaque.byteLength)
    expect(permitted).toBeLessThanOrEqual(V2_RELAY_RECEIVE_QUEUE_FRAMES)
    for (let index = 0; index < permitted; index += 1) {
      socket.message(opaque)
    }
    expect(socket.closeCode).toBeUndefined()
    socket.message(opaque)
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)

    const reader = connection.channel.frames.getReader()
    await expect(reader.read()).rejects.toThrow(/receive credit/)
  })

  it('bounds a relay that opens but withholds descriptor delivery', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket(false)
    const pending = dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const rejected = expect(pending).rejects.toMatchObject({ name: 'TimeoutError' })
    for (let turn = 0; turn < 8; turn += 1) await Promise.resolve()
    await vi.advanceTimersByTimeAsync(30_000)

    await rejected
  })

  it('retires only the exact receiver channel and treats stale retirement as a no-op', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const reader = connection.channel.frames.getReader()

    socket.message(encodeV2SessionRetired({
      relaySessionId: Uint8Array.of(2, 0, 0, 0, 0, 0, 0, 1),
    }))
    expect(connection.channel.state).toBe('open')
    expect(socket.closeCode).toBeUndefined()

    socket.message(encodeV2SessionRetired({ relaySessionId }))
    expect(connection.channel.state).toBe('closed')
    expect(socket.closeCode).toBe(1000)
    await expect(reader.read()).resolves.toEqual({ done: true, value: undefined })

    socket.message(encodeV2SessionRetired({ relaySessionId }))
    expect(socket.closeCode).toBe(1000)
  })

  it('fails the receiver link on a malformed retirement control', async () => {
    const socket = new FakeSocket()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket,
    })
    const malformed = encodeV2SessionRetired({ relaySessionId })
    malformed[5] = 1

    socket.message(malformed)
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)
    await expect(connection.channel.frames.getReader().read()).rejects.toThrow(/WS2F/)
  })
})

const wireBytes = (frame: Uint8Array) => encodeV2OpaqueRoute({ relaySessionId, ciphertext: frame }).byteLength
const connect = (socket: FakeSocket) => dialV2RelayReceiver('https://relay.invalid', capability, { socketFactory: () => socket })
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
    expect(socket.closeCode).toBe(V2_RELAY_HEARTBEAT_CLOSE_CODE)
    expect(socket.opaqueFrames).toEqual([])
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
    if (cause === 'retired') socket.message(encodeV2SessionRetired({ relaySessionId }))
    await Promise.all(rejected)
    expect(socket.opaqueFrames).toEqual([])
    expect(connection.channel.state).toBe('closed')
  })

  it('keeps heartbeat and credit responsive while the content receive queue is full', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket()
    const trace = vi.fn()
    const connection = await dialV2RelayReceiver('https://relay.invalid', capability, {
      socketFactory: () => socket, heartbeatTrace: trace,
    })
    const frame = Uint8Array.of(7)
    for (let index = 0; index < V2_RELAY_RECEIVE_QUEUE_FRAMES; index += 1) {
      socket.message(encodeV2OpaqueRoute({ relaySessionId, ciphertext: frame }))
    }
    const pending = connection.channel.send(frame)
    await vi.advanceTimersByTimeAsync(15_000)
    const probe = socket.sent.at(-1)!
    const nonce = decodeV2ConnectionProbe(probe)
    expect(socket.opaqueFrames).toEqual([])
    socket.message(encodeV2ConnectionProbeAck(nonce))
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
      socket.message(encodeV2SessionCredit({ relaySessionId: other, frames: 1, bytes: wireBytes(frame) }))
    }
    if (invalid === 'malformed') {
      const malformed = encodeV2SessionCredit({ relaySessionId, frames: 1, bytes: wireBytes(frame) })
      malformed[5] = 1
      socket.message(malformed)
    }
    await Promise.all([rejected, read])
    expect(socket.closeCode).toBe(V2_RELAY_PROTOCOL_CLOSE_CODE)
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
