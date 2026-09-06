import { describe, expect, it } from 'vitest'
import { presentTask } from '../../src/ui/tasks'
import { taskFixture, TASK_FIXTURES } from '../../src/ui/tasks/fixtures'
import { EMPTY_V2_PROGRESS } from '../../src/ui/v2-model'

const MIB = 1024n * 1024n
const resumed = {
  ...EMPTY_V2_PROGRESS, discovery: 'complete' as const,
  discoveredFiles: 10, discoveredBytes: 100n * MIB,
  completedFiles: 9, completedBytes: 90n * MIB,
  writtenBytes: MIB, materializedBytes: 91n * MIB,
}

describe('task progress native authority', () => {
  it('counts authenticated reused payload once without adding overlapping receipt and completed bytes', () => {
    const task = presentTask(taskFixture({ progress: resumed }))
    expect(task.progress?.percentage).toBe(91)
    expect(task.publication).toBe('unpublished')
  })

  it('keeps an open denominator indeterminate even with authenticated reused payload', () => {
    const task = presentTask(taskFixture({ progress: { ...resumed, discovery: 'open' } }))
    expect(task.progress).toMatchObject({ mode: 'indeterminate', percentage: null })
  })

  it('keeps direct ZIP progress on its own checkpoint-aware payload authority', () => {
    const task = presentTask(taskFixture({
      progress: { ...resumed, materializedBytes: MIB },
      directZipProgress: {
        ...TASK_FIXTURES.verifying!.directZipProgress!,
        receivedSelectedBytes: 77n * MIB,
      },
    }))
    expect(task.progress?.percentage).toBe(77)
  })

  it('shows native local finishing ahead of remote connection failure without publishing or completing a partial result', () => {
    const task = presentTask(taskFixture({
      progress: { ...resumed, phase: 'finishing' },
      blocking: { kind: 'share-ended' },
      completeness: 'partial',
    }))
    expect(task.stage).toBe('finishing')
    expect(task.completeness).toBe('partial')
    expect(task.publication).toBe('unpublished')
  })

  it('does not infer local finishing from byte equality before the native worker boundary', () => {
    const task = presentTask(taskFixture({
      progress: { ...resumed, writtenBytes: 100n * MIB, materializedBytes: 100n * MIB,
        completedFiles: 10, completedBytes: 100n * MIB, phase: 'receiving' },
    }))
    expect(task.stage).toBe('downloading')
    expect(task.publication).toBe('unpublished')
  })
})
