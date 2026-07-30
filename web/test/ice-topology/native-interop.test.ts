import { readFile } from 'node:fs/promises'

import { describe, expect, it } from 'vitest'

import {
  buildNativeInteropFailureEvidence,
  classifyNativeInteropFailure,
  selectedPairFromStats,
  validateNativeInteropProof,
  verifySerializedTestIceTopologyLock,
} from './native-interop'
import { probeRtcCapability } from './rtc-capability'

const PROFILE_SHA256 = '7d1082df592602db632e83b538b6d758fb71c66971b46e685f992b2c7a76c7ae'
const RESOLUTION_SHA256 = '2fac2dd7a746ea4a853081553db529d5a996c75fc81be53d54d843fc5cd64cf6'
const ATTEMPT_ID = Buffer.alloc(16, 7).toString('base64url')

describe('native browser selected-pair evidence', () => {
  it('uses the transport-selected candidate pair and preserves public endpoint diagnostics', () => {
    expect(selectedPairFromStats(stats())).toEqual({
      candidatePairId: 'pair-selected',
      local: {
        candidateId: 'browser-local',
        candidateType: 'host',
        protocol: 'udp',
        address: '192.0.2.10',
        port: 40_000,
      },
      remote: {
        candidateId: 'browser-remote',
        candidateType: 'host',
        protocol: 'udp',
        port: 40_001,
      },
    })
  })

  it('fails closed when public stats do not identify one authoritative pair', () => {
    const withoutTransport = stats().filter(({ type }) => type !== 'transport').map((record) =>
      record.type === 'candidate-pair' ? { ...record, nominated: false } : record)
    expect(() => selectedPairFromStats(withoutTransport)).toThrow(/0 nominated succeeded/u)
    expect(() => selectedPairFromStats([
      ...withoutTransport,
      { id: 'pair-other', type: 'candidate-pair', selected: true },
      { id: 'pair-third', type: 'candidate-pair', selected: true },
    ])).toThrow(/2 explicitly selected/u)
  })

  it('requires correlated attempts and resolution-bound direct endpoints on both sides', async () => {
    const lock = await topologyLock()
    const browserPair = selectedPairFromStats(stats())
    const pionPair = {
      local: {
        candidateType: 'host',
        protocol: 'udp',
        address: '192.0.2.10',
        port: 40_001,
        addressFamily: 'ipv4',
      },
      remote: {
        candidateType: 'prflx',
        protocol: 'udp',
        address: '192.0.2.10',
        port: 40_000,
        addressFamily: 'ipv4',
      },
    }
    expect(validateNativeInteropProof(
      ATTEMPT_ID,
      browserPair,
      ATTEMPT_ID,
      pionPair,
      lock,
    )).toMatchObject({
      browser: { attemptId: ATTEMPT_ID },
      pion: { attemptId: ATTEMPT_ID },
    })
    expect(() => validateNativeInteropProof(
      ATTEMPT_ID,
      browserPair,
      Buffer.alloc(16, 8).toString('base64url'),
      pionPair,
      lock,
    )).toThrow(/different attempts/u)
    expect(() => validateNativeInteropProof(
      ATTEMPT_ID,
      browserPair,
      ATTEMPT_ID,
      {
        ...pionPair,
        local: { ...pionPair.local, candidateType: 'relay' },
      },
      lock,
    )).toThrow(/resolution-bound direct/u)
  })

  it('preserves the authoritative Pion root cause over a downstream browser closure', () => {
    expect(classifyNativeInteropFailure(
      new Error('WebRTC remote channel closed'),
      ['capture topology-bound Pion selected pair: candidate is outside the topology policy'],
    )).toEqual({
      failureCode: 'selected-pair',
      failureMessage:
        'Pion: capture topology-bound Pion selected pair: candidate is outside the topology policy; ' +
        'Browser: WebRTC remote channel closed',
    })
    expect(classifyNativeInteropFailure(
      new DOMException('native negotiation exceeded its named deadline', 'TimeoutError'),
    )).toMatchObject({ failureCode: 'interop-deadline' })

    expect(buildNativeInteropFailureEvidence(
      ATTEMPT_ID,
      null,
      ATTEMPT_ID,
      null,
      new Error('channel closed'),
      ['capture selected pair failed'],
    )).toMatchObject({
      browser: { attemptId: ATTEMPT_ID, selectedPair: null },
      pion: { attemptId: ATTEMPT_ID, selectedPair: null },
      failureCode: 'selected-pair',
    })
    expect(() => buildNativeInteropFailureEvidence(
      ATTEMPT_ID,
      null,
      Buffer.alloc(16, 8).toString('base64url'),
      null,
      new Error('channel closed'),
    )).toThrow(/different attempts/u)
  })
})

describe('native RTC capability classification', () => {
  it('classifies API absence without constructing an attempt', async () => {
    expect(await probeRtcCapability({
      setTimeout,
      clearTimeout,
    })).toEqual({
      schemaVersion: 1,
      apiPresence: 'absent',
      probeOutcome: 'not-started',
      probeDeadlineMs: 5_000,
    })
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
    const evidence = await probeRtcCapability({
      RTCPeerConnection: FakePeerConnection as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })
    expect(evidence).toMatchObject({ apiPresence: 'present', probeOutcome: 'succeeded' })
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
    expect(await probeRtcCapability({
      RTCPeerConnection: FailingPeerConnection as unknown as typeof RTCPeerConnection,
      setTimeout,
      clearTimeout,
    })).toMatchObject({
      apiPresence: 'present',
      probeOutcome: 'failed',
      failureCode: 'offer-creation',
      failureMessage: 'offer unavailable',
    })
  })
})

function stats(): Record<string, unknown>[] {
  return [
    { id: 'transport-data', type: 'transport', selectedCandidatePairId: 'pair-selected' },
    {
      id: 'pair-selected',
      type: 'candidate-pair',
      localCandidateId: 'browser-local',
      remoteCandidateId: 'browser-remote',
      state: 'succeeded',
      nominated: true,
    },
    {
      id: 'browser-local',
      type: 'local-candidate',
      candidateType: 'host',
      protocol: 'UDP',
      address: '192.0.2.10',
      port: 40_000,
    },
    {
      id: 'browser-remote',
      type: 'remote-candidate',
      candidateType: 'host',
      protocol: 'udp',
      port: 40_001,
    },
  ]
}

async function topologyLock() {
  const [profile, resolution] = await Promise.all([
    fixture('pr-same-host-kernel-route-ipv4.json'),
    fixture('pr-same-host-kernel-route-ipv4-resolution.json'),
  ])
  return verifySerializedTestIceTopologyLock({
    profile: JSON.parse(profile) as unknown,
    resolution: JSON.parse(resolution) as unknown,
    profileSha256: PROFILE_SHA256,
    resolutionSha256: RESOLUTION_SHA256,
  })
}

function fixture(name: string): Promise<string> {
  return readFile(new URL(`../../../testdata/test-ice-topology/${name}`, import.meta.url), 'utf8')
}
