import { describe, expect, it } from 'vitest'

import {
  classifyRtcCapability,
  parseRtcCapabilityDiagnostic,
  probeRtcCapability,
} from './rtc-capability'

describe('native RTC capability classification', () => {
  it('classifies API absence without constructing an attempt', async () => {
    const diagnostic = await probeRtcCapability({ setTimeout, clearTimeout })

    expect(diagnostic).toEqual({
      schemaVersion: 1,
      apiPresence: 'absent',
      probeOutcome: 'not-started',
      probeDeadlineMs: 5_000,
    })
    expect(classifyRtcCapability(diagnostic)).toBe('unavailable')
  })

  it('requires ICE, an opened DataChannel, and a local round trip', async () => {
    const state = { channelsClosed: 0, peersClosed: 0 }
    const peers: FakePeerConnection[] = []
    const diagnostic = await probeRtcCapability({
      RTCPeerConnection: class extends FakePeerConnection {
        constructor() {
          super(peers, state)
        }
      } as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })

    expect(diagnostic).toMatchObject({ apiPresence: 'present', probeOutcome: 'succeeded' })
    expect(classifyRtcCapability(diagnostic)).toBe('available')
    expect(state).toEqual({ channelsClosed: 2, peersClosed: 2 })
  })

  it('classifies an API that cannot gather a candidate as unusable', async () => {
    const state = { peersClosed: 0 }
    const diagnostic = await probeRtcCapability({
      RTCPeerConnection: class extends NoCandidatePeerConnection {
        constructor() {
          super(state)
        }
      } as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })

    expect(diagnostic).toMatchObject({
      apiPresence: 'present',
      probeOutcome: 'failed',
      failureCode: 'ice-gathering',
    })
    expect(classifyRtcCapability(diagnostic)).toBe('unusable')
    expect(state.peersClosed).toBe(2)
  })

  it('retains the failing probe stage without inventing API absence', async () => {
    class FailingPeerConnection extends EventTarget {
      localDescription: RTCSessionDescriptionInit | null = null

      createDataChannel(): RTCDataChannel {
        return { close() {} } as unknown as RTCDataChannel
      }

      createOffer(): Promise<RTCSessionDescriptionInit> {
        return Promise.reject(new Error('offer unavailable'))
      }

      close(): void {}
    }

    const diagnostic = await probeRtcCapability({
      RTCPeerConnection: FailingPeerConnection as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })

    expect(diagnostic).toMatchObject({
      apiPresence: 'present',
      probeOutcome: 'failed',
      failureCode: 'offer-creation',
      failureMessage: 'offer unavailable',
    })
    expect(classifyRtcCapability(diagnostic)).toBe('unusable')
  })

  it('validates the structured clone returned across the Playwright page boundary', () => {
    expect(() => parseRtcCapabilityDiagnostic({
      schemaVersion: 1,
      apiPresence: 'present',
      probeOutcome: 'failed',
      probeDeadlineMs: 5_000,
    })).toThrow(/field set/u)
  })
})

interface ProbeState {
  channelsClosed: number
  peersClosed: number
}

class FakeDataChannel extends EventTarget {
  readyState: RTCDataChannelState = 'connecting'
  #remote: FakeDataChannel | undefined
  private readonly state: ProbeState

  constructor(state: ProbeState) {
    super()
    this.state = state
  }

  connect(remote: FakeDataChannel): void {
    this.#remote = remote
  }

  open(): void {
    this.readyState = 'open'
    this.dispatchEvent(new Event('open'))
  }

  send(data: string): void {
    const remote = this.#remote
    if (remote === undefined) throw new Error('fake channel is not connected')
    queueMicrotask(() => remote.dispatchEvent(new MessageEvent('message', { data })))
  }

  close(): void {
    if (this.readyState === 'closed') return
    this.readyState = 'closed'
    this.state.channelsClosed += 1
  }
}

class FakePeerConnection extends EventTarget {
  localDescription: RTCSessionDescriptionInit | null = null
  iceGatheringState: RTCIceGatheringState = 'new'
  #channel: FakeDataChannel | undefined
  private readonly peers: FakePeerConnection[]
  private readonly state: ProbeState

  constructor(peers: FakePeerConnection[], state: ProbeState) {
    super()
    this.peers = peers
    this.state = state
    peers.push(this)
  }

  createDataChannel(): RTCDataChannel {
    this.#channel = new FakeDataChannel(this.state)
    return this.#channel as unknown as RTCDataChannel
  }

  async createOffer(): Promise<RTCSessionDescriptionInit> {
    return { type: 'offer', sdp: 'v=0\r\n' }
  }

  async createAnswer(): Promise<RTCSessionDescriptionInit> {
    return { type: 'answer', sdp: 'v=0\r\n' }
  }

  async setLocalDescription(description: RTCSessionDescriptionInit): Promise<void> {
    this.localDescription = {
      ...description,
      sdp: `${description.sdp ?? ''}a=candidate:fake 1 udp 1 127.0.0.1 9 typ host\r\n`,
    }
    this.iceGatheringState = 'complete'
    this.dispatchEvent(new Event('icegatheringstatechange'))
  }

  async setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
    if (description.type === 'offer') {
      const offerer = this.peers.find((peer) => peer !== this)
      const offererChannel = offerer === undefined ? undefined : offerer.#channel
      if (offererChannel !== undefined) {
        const answererChannel = new FakeDataChannel(this.state)
        this.#channel = answererChannel
        offererChannel.connect(answererChannel)
        answererChannel.connect(offererChannel)
        queueMicrotask(() => {
          const event = new Event('datachannel')
          Object.defineProperty(event, 'channel', { value: answererChannel })
          this.dispatchEvent(event)
        })
      }
    } else if (description.type === 'answer') {
      this.#channel?.open()
      const answerer = this.peers.find((peer) => peer !== this)
      if (answerer !== undefined) answerer.#channel?.open()
    }
  }

  close(): void {
    this.#channel?.close()
    this.state.peersClosed += 1
  }
}

class NoCandidatePeerConnection extends EventTarget {
  localDescription: RTCSessionDescriptionInit | null = null
  iceGatheringState: RTCIceGatheringState = 'new'
  private readonly state: { peersClosed: number }

  constructor(state: { peersClosed: number }) {
    super()
    this.state = state
  }

  createDataChannel(): RTCDataChannel {
    return {
      close() {},
    } as unknown as RTCDataChannel
  }

  async createOffer(): Promise<RTCSessionDescriptionInit> {
    return { type: 'offer', sdp: 'v=0\r\n' }
  }

  async setLocalDescription(description: RTCSessionDescriptionInit): Promise<void> {
    this.localDescription = description
    this.iceGatheringState = 'complete'
    this.dispatchEvent(new Event('icegatheringstatechange'))
  }

  close(): void {
    this.state.peersClosed += 1
  }
}
