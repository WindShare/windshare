import { describe, expect, it, vi } from 'vitest'

import {
  containedBrowserCandidatePath,
  type ContainedBrowserSampleOutput,
  type ContainedBrowserSampleSecret,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-sample.ts'
import { containedBrowserSampleChild } from '../../scripts/browser-network-matrix/linux-topology/sample-child.ts'

const CONTROL_SECRET = 'controlAuthority0123456789'
const TURN_SECRET = 'turnAuthority012345678901'
// Fixed public endpoints make projection tests deterministic without performing network I/O.
// eslint-disable-next-line sonarjs/no-hardcoded-ip
const LOCAL_CANDIDATE_ADDRESS = '8.8.4.4'
// eslint-disable-next-line sonarjs/no-hardcoded-ip
const REMOTE_CANDIDATE_ADDRESS = '1.1.1.1'

describe('contained browser child secret boundary', () => {
  it('reduces browser stats to the exact public candidate path before child output', () => {
    expect(containedBrowserCandidatePath(publicStats())).toEqual({
      selectedPair: 'present',
      localCandidateType: 'relay',
      localAddress: LOCAL_CANDIDATE_ADDRESS,
      localPort: 50_000,
      remoteCandidateType: 'host',
      remoteAddress: REMOTE_CANDIDATE_ADDRESS,
      remotePort: 40_000,
      protocol: 'udp',
    })
  })

  it('rejects vendor secret fields without reflecting their names or values', () => {
    const hostile = publicStats()
    hostile[2] = {
      ...hostile[2],
      credential: TURN_SECRET,
      authorization: CONTROL_SECRET,
    }
    expect(() => containedBrowserCandidatePath(hostile)).toThrow(
      'contained browser candidate path projection is invalid',
    )
    try {
      containedBrowserCandidatePath(hostile)
    } catch (cause) {
      expect(String(cause)).not.toContain(TURN_SECRET)
      expect(String(cause)).not.toContain(CONTROL_SECRET)
      expect(String(cause)).not.toContain('authorization')
      expect(String(cause)).not.toContain('credential')
    }
  })

  it('erases the byte-owned control authority after output and output failure', async () => {
    for (const writeFailure of [false, true]) {
      const credential = Buffer.from(CONTROL_SECRET)
      const secret = {
        control: { credential },
      } as unknown as ContainedBrowserSampleSecret
      const output = { schemaVersion: 'test-output' } as unknown as ContainedBrowserSampleOutput
      const operation = containedBrowserSampleChild(['--browser', 'chromium'], {
        loadSecret: vi.fn().mockResolvedValue(secret),
        run: vi.fn().mockResolvedValue(output),
        writeOutput: writeFailure
          ? () => { throw new Error('injected output failure') }
          : vi.fn(),
      })
      if (writeFailure) await expect(operation).rejects.toThrow('injected output failure')
      else await expect(operation).resolves.toBe(output)
      expect([...credential]).toEqual(new Array(credential.byteLength).fill(0))
    }
  })
})

function publicStats(): Record<string, unknown>[] {
  return [
    {
      id: 'transport',
      type: 'transport',
      selectedCandidatePairId: 'pair',
    },
    {
      id: 'pair',
      type: 'candidate-pair',
      localCandidateId: 'local',
      remoteCandidateId: 'remote',
      selected: true,
      nominated: true,
      state: 'succeeded',
      protocol: 'udp',
    },
    {
      id: 'local',
      type: 'local-candidate',
      candidateType: 'relay',
      address: LOCAL_CANDIDATE_ADDRESS,
      ip: null,
      port: 50_000,
      protocol: 'udp',
    },
    {
      id: 'remote',
      type: 'remote-candidate',
      candidateType: 'host',
      address: REMOTE_CANDIDATE_ADDRESS,
      ip: null,
      port: 40_000,
      protocol: 'udp',
    },
  ]
}
