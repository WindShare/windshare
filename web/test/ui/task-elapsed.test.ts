import { expect, it } from 'vitest'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { formatElapsedTime } from '../../src/ui/tasks/elapsed'
import { presentTask } from '../../src/ui/tasks'
import { TASK_FIXTURES, taskFixture } from '../../src/ui/tasks/fixtures'
import { presentTaskProgress } from '../../src/ui/tasks/progress'

it.each([
  [0, 'Less than 1 sec'], [999, 'Less than 1 sec'], [1_234, '1 sec'],
  [59_999, '59 sec'], [60_000, '1 min 0 sec'], [125_000, '2 min 5 sec'],
  [3_661_000, '1 hr 1 min 1 sec'], [90_061_000, '1 day 1 hr 1 min 1 sec'],
])('formats elapsed %s ms with seconds preserved', (milliseconds, expected) => {
  expect(formatElapsedTime(milliseconds)).toBe(expected)
})

it('uses recorded result timing, not creation time or a new render timestamp', () => {
  const facts = TASK_FIXTURES['saved-cleanup']!
  const timed = {
    ...facts,
    lifecycle: {
      ...facts.lifecycle,
      timing: { startedAtMilliseconds: 1_000, activeMilliseconds: 125_000, finishedAtMilliseconds: 126_000 },
    },
  }
  expect(presentTask(timed).elapsedMilliseconds).toBe(125_000)
  expect(presentTask({ ...timed, display: { objectLabel: 'Changed label', createdAtMilliseconds: 10 } })
    .elapsedMilliseconds).toBe(125_000)
  const { timing, ...lifecycle } = timed.lifecycle
  expect(timing.finishedAtMilliseconds).toBe(126_000)
  expect(presentTask({ ...timed, lifecycle }).elapsedMilliseconds).toBeNull()
})

it('preserves exact paused progress after complete discovery and suppresses partial resumed discovery totals', () => {
  const pausedLifecycle = transitionReceiveState(initialReceiveState('p1'), {
    kind: 'paused',
    reason: 'user',
    completedFiles: 5n,
    completedBytes: 50n,
    retainedBytes: 52n,
  })
  const baseProgress = taskFixture().progress!

  // High-frequency same-tab pause after discovery completed preserves exact percentage & remaining bytes.
  const exactPaused = presentTaskProgress(taskFixture({
    lifecycle: pausedLifecycle,
    progress: {
      ...baseProgress,
      transferJobId: 'job-1',
      discovery: 'complete',
      discoveredFiles: 10,
      discoveredBytes: 100n,
    },
  }))
  expect(exactPaused).toMatchObject({
    mode: 'determinate',
    percentage: 52,
    remainingBytes: 48n,
    status: null,
  })

  // Mid-discovery re-pause where resumed job has seen fewer files/bytes than durable state stays indeterminate.
  const repausedMidDiscovery = presentTaskProgress(taskFixture({
    lifecycle: pausedLifecycle,
    progress: {
      ...baseProgress,
      transferJobId: 'job-2',
      discovery: 'open',
      discoveredFiles: 1,
      discoveredBytes: 10n,
      writtenBytes: 0n,
    },
  }))
  expect(repausedMidDiscovery).toMatchObject({
    mode: 'indeterminate',
    percentage: null,
    remainingBytes: null,
    status: null,
  })
  // Only completedBytes + writtenBytes details remain (no contradictory discovery lower-bound detail).
  expect(repausedMidDiscovery?.details).toHaveLength(2)
})
