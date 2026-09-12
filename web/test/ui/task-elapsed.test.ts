import { expect, it } from 'vitest'
import { formatElapsedTime } from '../../src/ui/tasks/elapsed'
import { presentTask } from '../../src/ui/tasks'
import { TASK_FIXTURES } from '../../src/ui/tasks/fixtures'

it.each([
  [0, 'Less than 1 sec'], [999, 'Less than 1 sec'], [1_234, '1 sec'],
  [59_999, '59 sec'], [60_000, '1 min 0 sec'], [125_000, '2 min 5 sec'],
  [3_661_000, '1 hr 1 min 1 sec'], [90_061_000, '1 day 1 hr 1 min 1 sec'],
])('formats elapsed %s ms with seconds preserved', (milliseconds, expected) => {
  expect(formatElapsedTime(milliseconds)).toBe(expected)
})

it('uses recorded result timing, not creation time or a new render timestamp', () => {
  const facts = TASK_FIXTURES['saved-cleanup']!
  const timed = { ...facts, lifecycle: { ...facts.lifecycle,
    timing: { startedAtMilliseconds: 1_000, resultReadyAtMilliseconds: 126_000 } } }
  expect(presentTask(timed).elapsedMilliseconds).toBe(125_000)
  expect(presentTask({ ...timed, display: { objectLabel: 'Changed label', createdAtMilliseconds: 10 } })
    .elapsedMilliseconds).toBe(125_000)
  const { timing, ...lifecycle } = timed.lifecycle
  expect(timing.resultReadyAtMilliseconds).toBe(126_000)
  expect(presentTask({ ...timed, lifecycle }).elapsedMilliseconds).toBeNull()
})
