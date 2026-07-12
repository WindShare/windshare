import { readFile } from 'node:fs/promises'
import { describe, expect, it } from 'vitest'
import {
  DATA_CHANNEL_HIGH_WATER_BYTES,
  DATA_CHANNEL_LABEL,
  DATA_CHANNEL_LOW_WATER_BYTES,
  DATA_CHANNEL_PROTOCOL,
  TERMINAL_ACK_CONTROL,
  TERMINAL_INTENT_CONTROL,
  WebRTCChannelClosedError,
  WebRTCChannelNotOpenError,
  WebRTCDataChannelConfigurationError,
  WebRTCIngressOverflowError,
  WebRTCPeerProtocolError,
  WebRTCRemoteClosedError,
  WebRTCTerminalNotAcknowledgedError,
  WebRTCTransportError,
  createWindShareFrameChannel,
  wrapWindShareDataChannel,
} from '../../../src/transport/webrtc'
import {
  FakeRTCDataChannel,
  FakeRTCPeerConnection,
  readAll,
  settle,
} from './fakes'

function wrap(
  raw = new FakeRTCDataChannel(),
  maximumMessageSize = 256 * 1024,
): {
  readonly raw: FakeRTCDataChannel
  readonly channel: ReturnType<typeof wrapWindShareDataChannel>
} {
  const peer = new FakeRTCPeerConnection(raw, maximumMessageSize)
  return {
    raw,
    channel: wrapWindShareDataChannel(peer.asPeer(), raw.asDataChannel()),
  }
}

describe('WebRTC DataChannel construction', () => {
  it('creates the exact in-band reliable channel without starting SDP or ICE', async () => {
    const raw = new FakeRTCDataChannel()
    const peer = new FakeRTCPeerConnection(raw)
    const channel = createWindShareFrameChannel(peer.asPeer())
    await channel.opened

    expect(peer.createDataChannelCalls).toBe(1)
    expect(peer.createOfferCalls).toBe(0)
    expect(peer.lastLabel).toBe(DATA_CHANNEL_LABEL)
    expect(peer.lastOptions).toEqual({
      negotiated: false,
      ordered: true,
      protocol: DATA_CHANNEL_PROTOCOL,
    })
    expect(raw.binaryType).toBe('arraybuffer')
    expect(raw.bufferedAmountLowThreshold).toBe(DATA_CHANNEL_LOW_WATER_BYTES)
    await channel.close()
  })

  it('publishes connecting and open deterministically', async () => {
    const raw = new FakeRTCDataChannel({ readyState: 'connecting' })
    const { channel } = wrap(raw)
    expect(channel.state).toBe('connecting')
    await expect(channel.send(Uint8Array.of(1))).rejects.toBeInstanceOf(
      WebRTCChannelNotOpenError,
    )

    raw.open()
    await channel.opened
    expect(channel.state).toBe('open')
    await channel.close()
  })

  it('reconciles a message observed after native Open but before the open callback', async () => {
    const raw = new FakeRTCDataChannel({ readyState: 'connecting' })
    const { channel } = wrap(raw)
    raw.readyState = 'open'

    raw.receiveBinary(Uint8Array.of(7))
    await channel.opened
    expect(channel.state).toBe('open')
    expect((await channel.frames.getReader().read()).value).toEqual(Uint8Array.of(7))
    await channel.close()
  })

  it('rejects an open callback that fires before the native channel is Open', async () => {
    const raw = new FakeRTCDataChannel({ readyState: 'connecting' })
    const { channel } = wrap(raw)

    raw.dispatchEvent(new Event('open'))
    await expect(channel.opened).rejects.toBeInstanceOf(WebRTCChannelNotOpenError)
    expect(channel.state).toBe('closed')
    expect(raw.closeCalls).toBe(1)
  })

  it('rejects an insufficient SCTP message capability at open', async () => {
    const raw = new FakeRTCDataChannel({ readyState: 'connecting' })
    const { channel } = wrap(raw, 65_535)
    raw.open()

    await expect(channel.opened).rejects.toBeInstanceOf(
      WebRTCDataChannelConfigurationError,
    )
    expect(channel.state).toBe('closed')
    expect(raw.closeCalls).toBe(1)
  })

  it.each([
    ['label', { label: 'wrong-label' }],
    ['protocol', { protocol: 'windshare-v1-invalid' }],
    ['ordering', { ordered: false }],
    ['packet lifetime', { maxPacketLifeTime: 1 }],
    ['retransmits', { maxRetransmits: 1 }],
    ['negotiation', { negotiated: true }],
    ['initial state', { readyState: 'closing' as const }],
  ])('rejects invalid %s before taking callback ownership', (_name, options) => {
    const raw = new FakeRTCDataChannel(options)
    const peer = new FakeRTCPeerConnection(raw)
    expect(() => wrapWindShareDataChannel(peer.asPeer(), raw.asDataChannel())).toThrow(
      WebRTCDataChannelConfigurationError,
    )
    expect(raw.closeCalls).toBe(0)
  })

  it('closes a factory-owned raw channel when actual settings are invalid', () => {
    const raw = new FakeRTCDataChannel({ protocol: 'windshare-v1-invalid' })
    const peer = new FakeRTCPeerConnection(raw)

    expect(() => createWindShareFrameChannel(peer.asPeer())).toThrow(
      WebRTCDataChannelConfigurationError,
    )
    expect(raw.closeCalls).toBe(1)
  })

  it('mirrors the accepted Go terminal-control fixture exactly', async () => {
    const fixturePath = new URL(
      '../../../../transport/webrtc/testdata/terminal-control.json',
      import.meta.url,
    )
    const fixture = JSON.parse(await readFile(fixturePath, 'utf8')) as {
      readonly terminalIntent: string
      readonly terminalAck: string
      readonly sequence: readonly string[]
    }
    expect(fixture.terminalIntent).toBe(TERMINAL_INTENT_CONTROL)
    expect(fixture.terminalAck).toBe(TERMINAL_ACK_CONTROL)
    expect(fixture.sequence).toEqual([
      TERMINAL_INTENT_CONTROL,
      'binary-message-without-prefix',
      TERMINAL_ACK_CONTROL,
    ])
  })
})

describe('WebRTC lifecycle and protocol failures', () => {
  it.each([
    ['unknown control', (raw: FakeRTCDataChannel) => raw.receiveText('unknown-control')],
    ['unsolicited ACK', (raw: FakeRTCDataChannel) => raw.receiveText(TERMINAL_ACK_CONTROL)],
    ['empty binary', (raw: FakeRTCDataChannel) => raw.receiveBinary(new Uint8Array())],
    ['oversized binary', (raw: FakeRTCDataChannel) => raw.receiveBinary(new Uint8Array(65_537))],
    ['non-ArrayBuffer binary', (raw: FakeRTCDataChannel) => raw.receiveUnknown(new Blob())],
  ])('fails closed on %s', async (_name, deliver) => {
    const { raw, channel } = wrap()
    deliver(raw)
    await channel.done
    expect(channel.state).toBe('closed')
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
    expect(raw.closeCalls).toBe(1)
  })

  it('rejects a duplicate terminal intent', async () => {
    const { raw, channel } = wrap()
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
  })

  it('classifies close after an intent without a frame as a peer failure', async () => {
    const { raw, channel } = wrap()
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.remoteClose()
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
    expect((channel.reason as Error).cause).toBeInstanceOf(WebRTCRemoteClosedError)
  })

  it('preserves a published terminal when the peer closes before ACK capacity', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveBinary(Uint8Array.of(7))
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(7)])
    await settle()
    expect(raw.sent).toHaveLength(0)

    raw.remoteClose()
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCRemoteClosedError)
    expect(raw.sent).toHaveLength(0)
  })

  it('freezes blocked sends at the native DataChannel closing boundary', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const sending = channel.send(Uint8Array.of(7))
    await settle()

    raw.readyState = 'closing'
    raw.dispatchEvent(new Event('closing'))
    await expect(sending).rejects.toBeInstanceOf(WebRTCRemoteClosedError)
    expect(channel.reason).toBeInstanceOf(WebRTCRemoteClosedError)
    expect(channel.state).toBe('closed')
    expect(raw.sent).toHaveLength(0)

    const closing = channel.close()
    raw.remoteClose()
    await closing
  })

  it('bounds unconsumed inbound frames and fails the channel locally', async () => {
    const { raw, channel } = wrap()
    for (let index = 0; index < 32; index += 1) {
      raw.receiveBinary(Uint8Array.of(index))
    }
    raw.receiveBinary(Uint8Array.of(33))
    await channel.done

    expect(channel.reason).toBeInstanceOf(WebRTCIngressOverflowError)
    expect(await readAll(channel.frames)).toHaveLength(32)
  })

  it('reserves one inbound slot for a terminal after ordinary saturation', async () => {
    const { raw, channel } = wrap()
    for (let index = 0; index < 32; index += 1) {
      raw.receiveBinary(Uint8Array.of(index))
    }
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveBinary(Uint8Array.of(0xff))

    await channel.done
    const frames = await readAll(channel.frames)
    expect(frames).toHaveLength(33)
    expect(frames.at(-1)).toEqual(Uint8Array.of(0xff))
    expect(raw.sent).toEqual([TERMINAL_ACK_CONTROL])
    await channel.close()
  })

  it('fails typed when the receive consumer cancels after remote terminal intent', async () => {
    const { raw, channel } = wrap()
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    await channel.frames.getReader().cancel('consumer stopped')

    raw.receiveBinary(Uint8Array.of(0xff))
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCChannelClosedError)
    expect(raw.sent).toHaveLength(0)
    await channel.close()
  })

  it('rejects a second binary frame while the remote terminal ACK is capacity-blocked', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveBinary(Uint8Array.of(0xfe))

    raw.receiveBinary(Uint8Array.of(0xfd))
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(0xfe)])
    expect(raw.sent).toHaveLength(0)
  })

  it('fails a genuinely pre-open message instead of reviving later', async () => {
    const raw = new FakeRTCDataChannel({ readyState: 'connecting' })
    const { channel } = wrap(raw)
    raw.receiveBinary(Uint8Array.of(1))
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
    raw.open()
    expect(channel.state).toBe('closed')
  })

  it('propagates a raw send failure and closes exactly once', async () => {
    const { raw, channel } = wrap()
    raw.sendHook = () => {
      throw new Error('synthetic send failure')
    }
    await expect(channel.send(Uint8Array.of(1))).rejects.toBeInstanceOf(
      WebRTCTransportError,
    )
    expect(channel.reason).toBeInstanceOf(WebRTCTransportError)
    expect(raw.closeCalls).toBe(1)
  })

  it('cancels a capacity-blocked terminal without enqueueing controls', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const controller = new AbortController()
    const terminal = channel.sendTerminal(Uint8Array.of(9), controller.signal)
    await settle()
    controller.abort(new DOMException('cancel terminal', 'AbortError'))

    await expect(terminal).rejects.toBeInstanceOf(
      WebRTCTerminalNotAcknowledgedError,
    )
    expect(raw.sent).toHaveLength(0)
    expect(channel.reason).toBeInstanceOf(WebRTCTerminalNotAcknowledgedError)
    expect(raw.closeCalls).toBe(1)
  })

  it('classifies remote close before local terminal ACK with both lifecycle types', async () => {
    const { raw, channel } = wrap()
    const terminal = channel.sendTerminal(Uint8Array.of(9))
    await settle()
    expect(raw.sent).toEqual([TERMINAL_INTENT_CONTROL, Uint8Array.of(9)])

    raw.remoteClose()
    const failure = await terminal.then(
      () => undefined,
      (error: unknown) => error,
    )
    expect(failure).toBeInstanceOf(WebRTCTerminalNotAcknowledgedError)
    expect((failure as Error).cause).toBeInstanceOf(WebRTCRemoteClosedError)
    expect(channel.reason).toBe(failure)
  })

  it('retains peer-protocol failure beneath a local terminal acknowledgement failure', async () => {
    const { raw, channel } = wrap()
    const terminal = channel.sendTerminal(Uint8Array.of(9))
    await settle()
    expect(raw.sent).toEqual([TERMINAL_INTENT_CONTROL, Uint8Array.of(9)])

    raw.receiveText('unknown-control')
    const failure = await terminal.then(
      () => undefined,
      (error: unknown) => error,
    )
    expect(failure).toBeInstanceOf(WebRTCTerminalNotAcknowledgedError)
    expect((failure as Error).cause).toBeInstanceOf(WebRTCPeerProtocolError)
    expect(channel.reason).toBe(failure)
  })

  it('retains a transport callback beneath a local terminal acknowledgement failure', async () => {
    const { raw, channel } = wrap()
    const terminal = channel.sendTerminal(Uint8Array.of(9))
    await settle()
    const transportCause = new Error('synthetic terminal transport failure')

    raw.fail(transportCause)
    const failure = await terminal.then(
      () => undefined,
      (error: unknown) => error,
    )
    expect(failure).toBeInstanceOf(WebRTCTerminalNotAcknowledgedError)
    expect((failure as Error).cause).toBeInstanceOf(WebRTCTransportError)
    expect(((failure as Error).cause as Error).cause).toBe(transportCause)
    expect(channel.reason).toBe(failure)
  })

  it('classifies a transport callback after remote intent as a missing terminal frame', async () => {
    const { raw, channel } = wrap()
    const transportCause = new Error('synthetic pre-terminal transport failure')
    raw.receiveText(TERMINAL_INTENT_CONTROL)

    raw.fail(transportCause)
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCPeerProtocolError)
    expect((channel.reason as Error).cause).toBeInstanceOf(WebRTCTransportError)
    expect(((channel.reason as Error).cause as Error).cause).toBe(transportCause)
  })

  it('serializes send decisions across repeated high-water transitions', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    raw.sendHook = () => raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const first = channel.send(Uint8Array.of(1))
    const second = channel.send(Uint8Array.of(2))
    await settle()

    raw.setBufferedAmount(DATA_CHANNEL_LOW_WATER_BYTES)
    await first
    await settle()
    let secondCompleted = false
    second.then(() => {
      secondCompleted = true
    }).catch(() => undefined)
    await settle()
    expect(secondCompleted).toBe(false)
    expect(raw.sent).toEqual([Uint8Array.of(1)])

    raw.setBufferedAmount(DATA_CHANNEL_LOW_WATER_BYTES)
    await second
    expect(raw.sent).toEqual([Uint8Array.of(1), Uint8Array.of(2)])
    await channel.close()
  })

  it('snapshots terminal payloads in both directions', async () => {
    const outbound = wrap()
    outbound.raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    outbound.raw.sendHook = (data) => {
      if (data instanceof Uint8Array) {
        queueMicrotask(() => outbound.raw.receiveText(TERMINAL_ACK_CONTROL))
      }
    }
    const outboundFrame = Uint8Array.of(4, 5, 6)
    const terminal = outbound.channel.sendTerminal(outboundFrame)
    outboundFrame.fill(9)
    await settle()
    outbound.raw.setBufferedAmount(0)
    await terminal
    expect(outbound.raw.sent).toEqual([
      TERMINAL_INTENT_CONTROL,
      Uint8Array.of(4, 5, 6),
    ])

    const inbound = wrap()
    const inboundFrame = Uint8Array.of(7, 8, 9)
    inbound.raw.receiveText(TERMINAL_INTENT_CONTROL)
    inbound.raw.receiveBinary(inboundFrame)
    inboundFrame.fill(1)
    await inbound.channel.done
    expect(await readAll(inbound.channel.frames)).toEqual([Uint8Array.of(7, 8, 9)])
    await inbound.channel.close()
  })

  it('preserves the final frame when terminal ACK transmission fails', async () => {
    const { raw, channel } = wrap()
    raw.sendHook = (data) => {
      if (data === TERMINAL_ACK_CONTROL) {
        throw new Error('synthetic ACK failure')
      }
    }
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    raw.receiveBinary(Uint8Array.of(7))
    await channel.done

    expect(await readAll(channel.frames)).toEqual([Uint8Array.of(7)])
    expect(channel.reason).toBeInstanceOf(WebRTCTransportError)
    expect(raw.closeCalls).toBe(1)
  })

  it('freezes ordinary outbound traffic as soon as a remote intent is admitted', async () => {
    const { raw, channel } = wrap()
    raw.receiveText(TERMINAL_INTENT_CONTROL)
    await expect(channel.send(Uint8Array.of(1))).rejects.toThrow(/terminal/u)
    raw.remoteClose()
    await channel.done
  })

  it('wakes a capacity-blocked ordinary send when remote terminal intent is admitted', async () => {
    const { raw, channel } = wrap()
    raw.setBufferedAmount(DATA_CHANNEL_HIGH_WATER_BYTES)
    const sending = channel.send(Uint8Array.of(1))
    await settle()

    raw.receiveText(TERMINAL_INTENT_CONTROL)
    await expect(sending).rejects.toThrow(/terminal/u)
    expect(raw.sent).toHaveLength(0)
    raw.remoteClose()
    await channel.done
  })

  it('linearizes terminal admission and close without wire overtaking', async () => {
    for (let iteration = 0; iteration < 50; iteration += 1) {
      const { raw, channel } = wrap()
      raw.sendHook = (data) => {
        if (data instanceof Uint8Array) {
          queueMicrotask(() => raw.receiveText(TERMINAL_ACK_CONTROL))
        }
      }
      if (iteration % 2 === 0) {
        const terminal = channel.sendTerminal(Uint8Array.of(iteration + 1))
        const closing = channel.close()
        await Promise.all([terminal, closing])
        expect(raw.sent).toEqual([
          TERMINAL_INTENT_CONTROL,
          Uint8Array.of(iteration + 1),
        ])
      } else {
        const closing = channel.close()
        await expect(channel.sendTerminal(Uint8Array.of(iteration + 1))).rejects.toThrow(
          /closed/u,
        )
        await closing
        expect(raw.sent).toHaveLength(0)
      }
      expect(raw.closeCalls).toBe(1)
    }
  })

  it('keeps the first transport error stable when close follows', async () => {
    const { raw, channel } = wrap()
    const failure = new Error('synthetic receive failure')
    raw.fail(failure)
    raw.remoteClose()
    await channel.done
    expect(channel.reason).toBeInstanceOf(WebRTCTransportError)
    expect((channel.reason as Error).cause).toBe(failure)
  })
})
