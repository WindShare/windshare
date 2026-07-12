import { describe, expect, it } from 'vitest'
import {
  ENVELOPE_TERMINAL_FORWARD,
  RELAY_INBOUND_FRAME_CAPACITY,
  RELAY_OUTBOUND_FRAME_CAPACITY,
  RELAY_SIGNAL_CAPACITY,
  RelayFrameChannel,
  RelaySessionIngressError,
  createSessionId,
  decodeRelayEnvelope,
} from '../../src/transport/relay'
import { FakeRelaySocket, gate, readAll, settle } from './helpers'
import { BoundedStreamQueue } from '../../src/transport/relay/stream-queue'

const sessionId = createSessionId(Uint8Array.of(0, 1, 2, 3, 4, 5, 6, 7))

function channelFixture(): { socket: FakeRelaySocket; channel: RelayFrameChannel } {
  const socket = new FakeRelaySocket()
  return { socket, channel: new RelayFrameChannel(sessionId, socket) }
}

describe('relay FrameChannel conformance matrix', () => {
  it('state-and-frame-bounds', async () => {
    const { channel } = channelFixture()
    expect(channel.state).toBe('open')
    await expect(channel.send(new Uint8Array())).rejects.toBeInstanceOf(RangeError)
    await expect(channel.send(new Uint8Array(65_537))).rejects.toBeInstanceOf(RangeError)
    expect(channel.state).toBe('open')
    await channel.close()
    expect(channel.state).toBe('closed')
  })

  it('payload-ownership', async () => {
    const { socket, channel } = channelFixture()
    const outbound = Uint8Array.of(1, 2, 3)
    await channel.send(outbound)
    outbound.fill(9)
    await settle()
    expect(decodeRelayEnvelope(socket.sentBinary[0]!).type).toBe('forward')
    expect((decodeRelayEnvelope(socket.sentBinary[0]!) as { frame: Uint8Array }).frame).toEqual(
      Uint8Array.of(1, 2, 3),
    )

    const inbound = Uint8Array.of(4, 5)
    channel.deliverFrame(inbound)
    inbound.fill(8)
    const reader = channel.frames.getReader()
    const received = await reader.read()
    expect(received.value).toEqual(Uint8Array.of(4, 5))
    reader.releaseLock()
    await channel.close()
  })

  it('backpressure-cancellation', async () => {
    const { socket, channel } = channelFixture()
    const blocked = gate()
    socket.sendBinaryHook = () => blocked.promise
    await Promise.all(
      Array.from({ length: RELAY_OUTBOUND_FRAME_CAPACITY }, (_, index) =>
        channel.send(Uint8Array.of(index + 1)),
      ),
    )
    const abort = new AbortController()
    const waiting = channel.send(Uint8Array.of(99), abort.signal)
    abort.abort(new DOMException('cancelled', 'AbortError'))
    await expect(waiting).rejects.toMatchObject({ name: 'AbortError' })
    blocked.open()
    await channel.close()
  })

  it('backpressure-recovery', async () => {
    const { socket, channel } = channelFixture()
    const blocked = gate()
    socket.sendBinaryHook = () => blocked.promise
    await Promise.all(
      Array.from({ length: RELAY_OUTBOUND_FRAME_CAPACITY }, () => channel.send(Uint8Array.of(1))),
    )
    const waiting = channel.send(Uint8Array.of(2))
    let settled = false
    waiting.then(() => {
      settled = true
    }).catch(() => undefined)
    await settle()
    expect(settled).toBe(false)
    blocked.open()
    await waiting
    await channel.close()
  })

  it('backpressure-remote-close', async () => {
    const { socket, channel } = channelFixture()
    const blocked = gate()
    socket.sendBinaryHook = () => blocked.promise
    await Promise.all(
      Array.from({ length: RELAY_OUTBOUND_FRAME_CAPACITY }, () => channel.send(Uint8Array.of(1))),
    )
    const waiting = channel.send(Uint8Array.of(2))
    channel.remoteClose(new Error('peer closed'))
    await expect(waiting).rejects.toThrow(/terminal/u)
    blocked.open()
  })

  it('settles the physical socket when an accepted outbound write later fails', async () => {
    const { socket, channel } = channelFixture()
    const failure = new Error('physical send failed')
    const closing = gate()
    socket.sendBinaryHook = async () => Promise.reject(failure)
    socket.closeHook = () => closing.promise
    await channel.send(Uint8Array.of(1))
    await settle()
    expect(channel.state).toBe('closed')
    expect(channel.reason).toBe(failure)
    expect(socket.closeCalls).toBeGreaterThan(0)
    const close = channel.close()
    let closeSettled = false
    close.then(() => {
      closeSettled = true
    }).catch(() => undefined)
    await settle()
    expect(closeSettled).toBe(false)
    closing.open()
    await close
  })

  it('outbound-terminal', async () => {
    const { socket, channel } = channelFixture()
    await channel.sendTerminal(Uint8Array.of(3, 2, 1))
    expect(socket.sentBinary.at(-1)?.[0]).toBe(ENVELOPE_TERMINAL_FORWARD)
    expect(channel.state).toBe('closed')
    await expect(channel.send(Uint8Array.of(1))).rejects.toThrow(/closed/u)
  })

  it('preserves the terminal send failure when physical close also fails', async () => {
    const { socket, channel } = channelFixture()
    const sendFailure = new Error('terminal send failed')
    socket.sendBinaryHook = async () => Promise.reject(sendFailure)
    socket.closeError = new Error('physical close failed')
    await expect(channel.sendTerminal(Uint8Array.of(1))).rejects.toBe(sendFailure)
    expect(channel.reason).toBe(sendFailure)
    expect(channel.state).toBe('closed')
  })

  it('terminal-not-overtaken-by-close', async () => {
    const { socket, channel } = channelFixture()
    const blocked = gate()
    socket.sendBinaryHook = () => blocked.promise
    const terminal = channel.sendTerminal(Uint8Array.of(3))
    await settle()
    const close = channel.close()
    expect(socket.sentText).toHaveLength(0)
    blocked.open()
    await Promise.all([terminal, close])
    expect(socket.sentText).toHaveLength(0)
    expect(socket.sentBinary.at(-1)?.[0]).toBe(ENVELOPE_TERMINAL_FORWARD)
  })

  it('inbound-terminal-before-close', async () => {
    const { channel } = channelFixture()
    expect(channel.deliverFrame(Uint8Array.of(1))).toBe('accepted')
    expect(channel.deliverTerminal(Uint8Array.of(3))).toBe('accepted')
    expect(channel.state).toBe('closed')
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(1), Uint8Array.of(3)])
  })

  it('close-idempotence', async () => {
    const { socket, channel } = channelFixture()
    await Promise.all([channel.close(), channel.close(), channel.close()])
    expect(socket.sentText).toHaveLength(1)
    await channel.close()
    expect(socket.sentText).toHaveLength(1)
  })

  it('remote-close-and-late-traffic', async () => {
    const { channel } = channelFixture()
    channel.deliverFrame(Uint8Array.of(1))
    channel.remoteClose()
    expect(channel.deliverFrame(Uint8Array.of(2))).toBe('closed')
    expect(channel.deliverTerminal(Uint8Array.of(3))).toBe('closed')
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(1)])
  })
})

describe('relay inbound isolation', () => {
  it('releases buffered ownership when its only stream consumer cancels', async () => {
    const queue = new BoundedStreamQueue<Uint8Array>(2)
    queue.push(Uint8Array.of(1))
    queue.push(Uint8Array.of(2))
    expect(queue.bufferedCount).toBe(2)
    await queue.stream.getReader().cancel()
    expect(queue.bufferedCount).toBe(0)
  })

  it('reserves terminal capacity after ordinary saturation', async () => {
    const { channel } = channelFixture()
    for (let index = 0; index < RELAY_INBOUND_FRAME_CAPACITY; index += 1) {
      expect(channel.deliverFrame(Uint8Array.of(index))).toBe('accepted')
    }
    expect(channel.deliverTerminal(Uint8Array.of(255))).toBe('accepted')
    const frames = await readAll(channel.frames)
    expect(frames).toHaveLength(RELAY_INBOUND_FRAME_CAPACITY + 1)
    expect(frames.at(-1)).toEqual(Uint8Array.of(255))
  })

  it('contains overflow to one physical session', async () => {
    const first = channelFixture()
    const second = channelFixture()
    for (let index = 0; index < RELAY_INBOUND_FRAME_CAPACITY; index += 1) {
      first.channel.deliverFrame(Uint8Array.of(index))
    }
    expect(first.channel.deliverFrame(Uint8Array.of(99))).toBe('overflow')
    expect(first.channel.failIngress('frames')).toBeInstanceOf(RelaySessionIngressError)
    expect(first.channel.state).toBe('closed')
    expect(second.channel.deliverFrame(Uint8Array.of(7))).toBe('accepted')
    const reader = second.channel.frames.getReader()
    expect((await reader.read()).value).toEqual(Uint8Array.of(7))
    await reader.cancel()
  })

  it('bounds signaling independently from data frames', () => {
    const { channel } = channelFixture()
    for (let index = 0; index < RELAY_SIGNAL_CAPACITY; index += 1) {
      expect(channel.deliverSignal({ kind: 'candidate', payload: { index } })).toBe(
        'accepted',
      )
    }
    expect(channel.deliverSignal({ kind: 'candidate', payload: {} })).toBe('overflow')
    expect(channel.deliverFrame(Uint8Array.of(1))).toBe('accepted')
  })
})
