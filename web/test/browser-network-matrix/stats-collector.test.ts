import { describe, expect, it } from 'vitest'

import {
  networkCandidatePathFromStats,
} from '../../scripts/browser-network-matrix/stats-collector.ts'

// Fixed documentation endpoints make normalization assertions deterministic without network I/O.
const LOCAL_CANDIDATE_ADDRESS = '192.0.2.10'
const REMOTE_CANDIDATE_ADDRESS = '198.51.100.20'
const LOCAL_CANDIDATE_PORT = 50_000
const REMOTE_CANDIDATE_PORT = 40_000

describe('browser network matrix selected-pair stats collector', () => {
  it('preserves selected-pair absence as truthful restricted-UDP evidence', () => {
    expect(networkCandidatePathFromStats([])).toEqual({
      selectedPair: 'absent',
      localCandidateType: null,
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    })
  })

  it('resolves a transport-selected pair and normalizes its protocol', () => {
    expect(networkCandidatePathFromStats(selectedPairStats())).toEqual({
      selectedPair: 'present',
      localCandidateType: 'srflx',
      localAddress: LOCAL_CANDIDATE_ADDRESS,
      localPort: LOCAL_CANDIDATE_PORT,
      remoteCandidateType: 'host',
      remoteAddress: REMOTE_CANDIDATE_ADDRESS,
      remotePort: REMOTE_CANDIDATE_PORT,
      protocol: 'udp',
    })
  })

  it('uses a unique nominated succeeded pair when selected fields are omitted', () => {
    const records = selectedPairStats()
      .filter(({ type }) => type !== 'transport')
      .map((record) => record.type === 'candidate-pair'
        ? { ...record, selected: undefined, nominated: true, state: 'succeeded' }
        : record)
    expect(networkCandidatePathFromStats(records).selectedPair).toBe('present')
  })

  it.each([
    {
      name: 'multiple transport-selected pairs',
      mutate: (records: Record<string, unknown>[]) => records.push({
        id: 'transport-two',
        type: 'transport',
        selectedCandidatePairId: 'pair-two',
      }),
    },
    {
      name: 'duplicate stats IDs',
      mutate: (records: Record<string, unknown>[]) => records.push({ id: 'local-one', type: 'codec' }),
    },
    {
      name: 'contradictory protocols',
      mutate: (records: Record<string, unknown>[]) => {
        const remote = records.find(({ id }) => id === 'remote-one')
        if (remote !== undefined) remote.protocol = 'tcp'
      },
    },
    {
      name: 'unknown candidate vocabulary',
      mutate: (records: Record<string, unknown>[]) => {
        const local = records.find(({ id }) => id === 'local-one')
        if (local !== undefined) local.candidateType = 'mystery'
      },
    },
    {
      name: 'dangling candidate reference',
      mutate: (records: Record<string, unknown>[]) => {
        const pair = records.find(({ id }) => id === 'pair-one')
        if (pair !== undefined) pair.remoteCandidateId = 'missing'
      },
    },
  ])('fails closed for $name', ({ mutate }) => {
    const records = selectedPairStats().map((record) => ({ ...record }))
    mutate(records)
    expect(() => networkCandidatePathFromStats(records)).toThrow()
  })

  it('does not let malformed unselected records fabricate a path', () => {
    expect(networkCandidatePathFromStats([
      { id: 'failed-pair', type: 'candidate-pair', state: 'failed' },
      { id: 'unrelated', type: 'local-candidate', candidateType: 'mystery' },
    ])).toEqual({
      selectedPair: 'absent',
      localCandidateType: null,
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    })
  })
})

function selectedPairStats(): Record<string, unknown>[] {
  return [
    { id: 'transport-one', type: 'transport', selectedCandidatePairId: 'pair-one' },
    {
      id: 'pair-one',
      type: 'candidate-pair',
      localCandidateId: 'local-one',
      remoteCandidateId: 'remote-one',
      protocol: 'UDP',
    },
    {
      id: 'local-one',
      type: 'local-candidate',
      candidateType: 'srflx',
      address: LOCAL_CANDIDATE_ADDRESS,
      port: LOCAL_CANDIDATE_PORT,
      protocol: 'udp',
    },
    {
      id: 'remote-one',
      type: 'remote-candidate',
      candidateType: 'host',
      address: REMOTE_CANDIDATE_ADDRESS,
      port: REMOTE_CANDIDATE_PORT,
      protocol: 'udp',
    },
  ]
}
