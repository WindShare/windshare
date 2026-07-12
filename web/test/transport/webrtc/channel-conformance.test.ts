import { describe, expect, it } from 'vitest'
import {
  DATA_CHANNEL_HIGH_WATER_BYTES,
  DATA_CHANNEL_LOW_WATER_BYTES,
  TERMINAL_ACK_CONTROL,
  TERMINAL_INTENT_CONTROL,
  WebRTCRemoteClosedError,
  wrapWindShareDataChannel,
} from '../../../src/transport/webrtc'
import {
  FakeRTCDataChannel,
  FakeRTCPeerConnection,
  readAll,
  settle,
} from './fakes'

function channelFixture(): {
  readonly raw: FakeRTCDataChannel
  readonly channel: ReturnType<typeof wrapWindShareDataChannel>
} {
  const raw = new FakeRTCDataChannel()
  const peer = new FakeRTCPeerConnection(raw)
  return {
    raw,
    channel: wrapWindShareDataChannel(peer.asPeer(), raw.asDataChannel()),
  }
}

describe('WebRTC FrameChannel conformance matrix', () => {
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
    const { raw, channel } = channelFixture()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const outbound = Uint8Array.of(1, 2, 3)
    const sending = channel.send(outbound)
    outbound.fill(9)
    await settle()
    raw.setBufferedAmount(0)
    await sending
    expect(raw.sent).toEqual([Uint8Array.of(1, 2, 3)])

    const inbound = Uint8Array.of(4, 5)
    raw.receiveBinary(inbound)
    inbound.fill(8)
    expect((await channel.frames.getReader().read()).value).toEqual(Uint8Array.of(4, 5))
    await channel.close()
  })

  it('backpressure-cancellation', async () => {
    const { raw, channel } = channelFixture()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const controller = new AbortController()
    const waiting = channel.send(Uint8Array.of(99), controller.signal)
    await settle()
    controller.abort(new DOMException('cancelled', 'AbortError'))
    await expect(waiting).rejects.toMatchObject({ name: 'AbortError' })
    expect(raw.sent).toHaveLength(0)
    await channel.close()
  })

  it('backpressure-recovery', async () => {
    const { raw, channel } = channelFixture()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const waiting = channel.send(Uint8Array.of(2))
    let completed = false
    waiting.then(() => {
      completed = true
    }).catch(() => undefined)
    await settle()

    raw.emitBufferedAmountLow()
    await settle()
    expect(completed).toBe(false)

    raw.setBufferedAmount(DATA_CHANNEL_LOW_WATER_BYTES + 1)
    raw.emitBufferedAmountLow()
    await settle()
    expect(completed).toBe(false)

    raw.setBufferedAmount(DATA_CHANNEL_LOW_WATER_BYTES)
    await waiting
    expect(raw.sent).toEqual([Uint8Array.of(2)])
    await channel.close()
  })

  it('backpressure-remote-close', async () => {
    const { raw, channel } = channelFixture()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const waiting = channel.send(Uint8Array.of(2))
    await settle()
    raw.remoteClose()
    await expect(waiting).rejects.toBeInstanceOf(WebRTCRemoteClosedError)
    expect(channel.reason).toBeInstanceOf(WebRTCRemoteClosedError)
  })

  it('outbound-terminal', async () => {
    const { raw, channel } = channelFixture()
    raw.sendHook = (data) => {
      if (data instanceof Uint8Array) {
        queueMicrotask(() => raw.receiveText(TERMINAL_ACK_CONTROL))
      }
    }
    await channel.sendTerminal(Uint8Array.of(3, 2, 1))
    expect(raw.sent).toEqual([
      TERMINAL_INTENT_CONTROL,
      Uint8Array.of(3, 2, 1),
    ])
    expect(channel.state).toBe('closed')
    expect(raw.closeCalls).toBe(1)
    await expect(channel.send(Uint8Array.of(1))).rejects.toThrow(/closed/u)
  })

  it('terminal-not-overtaken-by-close', async () => {
    const { raw, channel } = channelFixture()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    raw.sendHook = (data) => {
      if (data instanceof Uint8Array) {
        queueMicrotask(() => raw.receiveText(TERMINAL_ACK_CONTROL))
      }
    }
    const terminal = channel.sendTerminal(Uint8Array.of(3))
    await settle()
    const close = channel.close()
    await settle()
    expect(raw.closeCalls).toBe(0)
    expect(raw.sent).toHaveLength(0)

    raw.setBufferedAmount(0)
    await Promise.all([terminal, close])
    expect(raw.sent).toEqual([TERMINAL_INTENT_CONTROL, Uint8Array.of(3)])
    expect(raw.closeCalls).toBe(1)
  })

  it('inbound-terminal-before-close', async () => {
    const { raw, channel } = channelFixture()
    raw.receiveBinary(Uint8Array.of(1))
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveBinary(Uint8Array.of(3))
    await channel.done

    expect(channel.state).toBe('closed')
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(1), Uint8Array.of(3)])
    expect(raw.sent).toEqual([TERMINAL_ACK_CONTROL])
    expect(raw.closeCalls).toBe(0)
    await channel.close()
    expect(raw.closeCalls).toBe(1)
  })

  it('close-idempotence', async () => {
    const { raw, channel } = channelFixture()
    await Promise.all([channel.close(), channel.close(), channel.close()])
    expect(raw.closeCalls).toBe(1)
    await channel.close()
    expect(raw.closeCalls).toBe(1)
  })

  it('remote-close-and-late-traffic', async () => {
    const { raw, channel } = channelFixture()
    raw.receiveBinary(Uint8Array.of(1))
    raw.remoteClose()
    raw.receiveBinary(Uint8Array.of(2))
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(1)])
    expect(channel.state).toBe('closed')
  })
})
