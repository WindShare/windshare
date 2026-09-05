import { describe, expect, it } from 'vitest'
import { DownloadMetrics, MAXIMUM_DOWNLOAD_INTERVALS } from '../../src/receiver/download-metrics'

describe('per-download connectivity accounting', () => {
  it('preserves revision deduplication and counts only pending fallback across generation gaps', () => {
    let now = 0
    const metrics = new DownloadMetrics('download', true, () => now)
    metrics.delivered('revision-a', 0n, 100n, 'direct')
    now = 1_000
    metrics.availability(false)
    now += 60_000 // User/output idle contributes no transport stall.
    let end = metrics.pending()
    now += 3_000
    end(); end()
    now += 60_000
    end = metrics.pending()
    now += 2_000
    metrics.delivered('revision-a', 50n, 150n, 'turn')
    now += 1_000
    end()
    metrics.delivered('revision-a', 0n, 150n, 'application-relay')
    metrics.delivered('revision-b', 0n, 100n, 'application-relay')
    metrics.availability(true)
    const final = metrics.snapshot(true)
    expect(final).toMatchObject({
      first_direct_elapsed_ms: 0, direct_bytes: '100', turn_bytes: '50',
      application_relay_bytes: '100', direct_fraction: 0.4, fallback_stall_ms: 5_000,
      incomplete: false, final: true,
    })
    now += 60_000
    metrics.pending()(); metrics.availability(false); metrics.evidenceLost()
    metrics.delivered('later', 0n, 100n, 'direct')
    expect(metrics.snapshot()).toEqual(final)
  })
  it('keeps unknown evidence and bounded retention explicit', () => {
    let now = 0
    const metrics = new DownloadMetrics('download', false, () => now)
    expect(metrics.snapshot().first_direct_elapsed_ms).toBeNull()
    now = 250
    metrics.availability(true)
    metrics.delivered('r', 0n, 10n)
    expect(metrics.snapshot()).toMatchObject({
      first_direct_elapsed_ms: 250, unknown_bytes: '10', incomplete: true, direct_fraction: null,
    })
    const bounded = new DownloadMetrics('bounded', false)
    for (let index = 0; index <= MAXIMUM_DOWNLOAD_INTERVALS; index++) {
      bounded.delivered(String(index), 0n, 1n, 'direct')
    }
    expect(bounded.snapshot()).toMatchObject({ direct_bytes: String(MAXIMUM_DOWNLOAD_INTERVALS), incomplete: true })
  })
  it('merges overlap without assigning duplicates to another route and rejects clock regression', () => {
    let now = 10
    const metrics = new DownloadMetrics('ranges', false, () => now)
    metrics.delivered('r', 10n, 20n, 'direct')
    metrics.delivered('r', 30n, 40n, 'turn')
    metrics.delivered('r', 0n, 50n, 'application-relay')
    expect(metrics.snapshot()).toMatchObject({ direct_bytes: '10', turn_bytes: '10', application_relay_bytes: '30' })
    now = 0
    metrics.availability(true)
    expect(metrics.snapshot().incomplete).toBe(true)
    metrics.delivered('', 0n, 1n); metrics.delivered('r', 2n, 1n)
  })
})
