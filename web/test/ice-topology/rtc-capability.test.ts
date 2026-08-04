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

  it('proves a retained local offer and always closes probe resources', async () => {
    const state = { channelClosed: false, peerClosed: false }
    class FakePeerConnection {
      localDescription: RTCSessionDescriptionInit | null = null
      createDataChannel(): Pick<RTCDataChannel, 'close'> {
        return { close: () => { state.channelClosed = true } }
      }
      async createOffer(): Promise<RTCSessionDescriptionInit> {
        return { type: 'offer', sdp: 'v=0\r\n' }
      }
      async setLocalDescription(description: RTCSessionDescriptionInit): Promise<void> {
        this.localDescription = description
      }
      close(): void {
        state.peerClosed = true
      }
    }

    const diagnostic = await probeRtcCapability({
      RTCPeerConnection: FakePeerConnection as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })

    expect(diagnostic).toMatchObject({ apiPresence: 'present', probeOutcome: 'succeeded' })
    expect(classifyRtcCapability(diagnostic)).toBe('available')
    expect(state).toEqual({ channelClosed: true, peerClosed: true })
  })

  it('retains the failing probe stage without inventing API absence', async () => {
    class FailingPeerConnection {
      createDataChannel(): Pick<RTCDataChannel, 'close'> {
        return { close() {} }
      }
      async createOffer(): Promise<RTCSessionDescriptionInit> {
        throw new Error('offer unavailable')
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
