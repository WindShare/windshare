import { describe, expect, it } from 'vitest'
import {
  MAX_SAFE_RENAMED_SAMPLE,
  RECEIVE_LIFECYCLE_KINDS,
  initialReceiveState,
  isActiveReceiveState,
  isDeliveredReceiveState,
  isResumableReceiveState,
  isTerminalReceiveState,
  receiveElapsedMilliseconds,
  safeRenamedSummary,
  transitionReceiveState,
} from '../../src/transfer/state'

describe('receive lifecycle state model', () => {
  it('defines exactly eight orthogonal lifecycle states', () => {
    expect(RECEIVE_LIFECYCLE_KINDS).toEqual([
      'preparing',
      'downloading',
      'paused',
      'ready-to-save',
      'download-started',
      'saved',
      'partial',
      'failed',
    ])
  })

  it('tracks generation and active elapsed milliseconds across pause and resume', () => {
    const t0 = 10_000
    const initial = initialReceiveState('task-1', t0)
    expect(initial.kind).toBe('preparing')
    expect(initial.generation).toBe(1n)
    expect(isActiveReceiveState(initial)).toBe(true)

    const running = transitionReceiveState(initial, { kind: 'downloading', phase: 'receiving' }, t0 + 100)
    expect(running.generation).toBe(2n)
    expect(receiveElapsedMilliseconds(running.timing, t0 + 2_600)).toBe(2_500)

    const paused = transitionReceiveState(running, {
      kind: 'paused',
      reason: 'user',
      completedFiles: 1n,
      completedBytes: 512n,
      retainedBytes: 512n,
      discoveredFiles: 2n,
      discoveredBytes: 1_024n,
      discoveryComplete: true,
    }, t0 + 3_100)
    expect(isResumableReceiveState(paused)).toBe(true)
    expect(isActiveReceiveState(paused)).toBe(false)
    expect(receiveElapsedMilliseconds(paused.timing, t0 + 99_000)).toBe(3_000)

    const resumed = transitionReceiveState(paused, { kind: 'downloading', phase: 'receiving' }, t0 + 10_000)
    const saved = transitionReceiveState(resumed, { kind: 'saved', savedFiles: 2n, savedBytes: 1_024n }, t0 + 12_000)
    expect(receiveElapsedMilliseconds(saved.timing, t0 + 50_000)).toBe(5_000)
    expect(isDeliveredReceiveState(saved)).toBe(true)
    expect(isTerminalReceiveState(saved)).toBe(true)
  })

  it('distinguishes retained OPFS browser handoff from ephemeral portable handoff', () => {
    const base = initialReceiveState('task-2', 1_000)
    const retained = transitionReceiveState(base, { kind: 'download-started', retained: true, fileName: 'a.zip' }, 2_000)
    const ephemeral = transitionReceiveState(base, { kind: 'download-started', retained: false, fileName: 'a.zip' }, 2_000)

    expect(isDeliveredReceiveState(retained)).toBe(true)
    expect(isTerminalReceiveState(retained)).toBe(false)
    expect(isDeliveredReceiveState(ephemeral)).toBe(true)
    expect(isTerminalReceiveState(ephemeral)).toBe(true)
  })

  it('bounds safe-renamed sample entries', () => {
    const sample = Array.from({ length: MAX_SAFE_RENAMED_SAMPLE + 5 }, (_, index) => ({
      logicalPath: [`file-${index}`],
      physicalName: `safe-${index}`,
    }))
    const summary = safeRenamedSummary({ count: sample.length, sample })
    expect(summary.count).toBe(MAX_SAFE_RENAMED_SAMPLE + 5)
    expect(summary.sample).toHaveLength(MAX_SAFE_RENAMED_SAMPLE)
  })
})
