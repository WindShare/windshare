import { describe, expect, it } from 'vitest'
import {
  selectedCandidatePairFromStats,
  type RTCStatsLike,
} from './performance-browser'

function selectedStats(): RTCStatsLike[] {
  return [
    {
      id: 'transport-1',
      type: 'transport',
      selectedCandidatePairId: 'pair-1',
    },
    {
      id: 'pair-1',
      type: 'candidate-pair',
      state: 'succeeded',
      nominated: true,
      localCandidateId: 'local-1',
      remoteCandidateId: 'remote-1',
      bytesSent: 123,
      bytesReceived: 456,
      currentRoundTripTime: 0.002,
    },
    {
      id: 'local-1',
      type: 'local-candidate',
      address: '127.0.0.1',
      port: 51000,
      protocol: 'udp',
      candidateType: 'host',
      networkType: 'ethernet',
    },
    {
      id: 'remote-1',
      type: 'remote-candidate',
      ip: '127.0.0.1',
      port: 51001,
      protocol: 'udp',
      candidateType: 'host',
    },
  ]
}

describe('browser performance candidate-pair evidence', () => {
  it('resolves the transport-selected pair and both endpoint candidates', () => {
    expect(selectedCandidatePairFromStats(selectedStats())).toEqual({
      id: 'pair-1',
      state: 'succeeded',
      nominated: true,
      bytesSent: 123,
      bytesReceived: 456,
      currentRoundTripTimeSeconds: 0.002,
      local: {
        id: 'local-1',
        address: '127.0.0.1',
        port: 51000,
        protocol: 'udp',
        candidateType: 'host',
        networkType: 'ethernet',
        relayProtocol: null,
      },
      remote: {
        id: 'remote-1',
        address: '127.0.0.1',
        port: 51001,
        protocol: 'udp',
        candidateType: 'host',
        networkType: null,
        relayProtocol: null,
      },
    })
  })

  it('rejects a succeeded pair that is not selected by the transport', () => {
    const stats = selectedStats().filter((stat) => stat.type !== 'transport')
    expect(() => selectedCandidatePairFromStats(stats)).toThrow(
      'expose 0 selected candidate pairs',
    )
  })

  it('rejects incomplete selected-pair endpoint evidence', () => {
    const stats = selectedStats().filter((stat) => stat.id !== 'remote-1')
    expect(() => selectedCandidatePairFromStats(stats)).toThrow(
      'selected ICE candidate remote-1 is unavailable',
    )
  })
})
