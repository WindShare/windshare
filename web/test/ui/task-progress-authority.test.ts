import { describe, expect, it } from 'vitest'
import { presentTask } from '../../src/ui/tasks'
import { taskFixture, TASK_FIXTURES } from '../../src/ui/tasks/fixtures'
import { EMPTY_V2_PROGRESS } from '../../src/ui/v2-model'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { storedReceiveLifecycleState, decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'

const MIB = 1024n * 1024n
const resumed = {
  ...EMPTY_V2_PROGRESS, discovery: 'complete' as const,
  discoveredFiles: 10, discoveredBytes: 100n * MIB,
  completedFiles: 9, completedBytes: 90n * MIB,
  writtenBytes: MIB, materializedBytes: 91n * MIB,
}

describe('task progress native authority', () => {
  it('keeps incoming object traffic separate from output completion', () => {
    const task = presentTask(taskFixture({ progress: {
      ...EMPTY_V2_PROGRESS, discovery: 'complete', discoveredFiles: 1,
      discoveredBytes: 8n * MIB, receivedObjectBytes: MIB,
    } }))
    expect(task.progress).toMatchObject({
      percentage: 0, receivedObjectBytes: MIB, writtenBytes: 0n, remainingBytes: 8n * MIB,
    })
    expect(task.publication).toBe('unpublished')
  })


  it('shows partially retained bytes after a paused task is persisted and reloaded', async () => {
    const record = await storedReceiveLifecycleState({
      kind: 'resumable-receive', payloadKind: 'file-set',
      operationId: encodeBase64Url(new Uint8Array(16).fill(1)),
      receiveIntentDigest: encodeBase64Url(new Uint8Array(32).fill(2)),
      checkpointSetDigest: encodeBase64Url(new Uint8Array(32).fill(3)),
      generation: 2n, completedFileCount: 0n, completedBytes: 0n, retainedBytes: 3n * MIB,
      selectionFacts: { discovery: 'complete', discoveredFileCount: 1n, discoveredBytes: 12n * MIB },
    })
    const task = presentTask(taskFixture({
      progress: null, directZipProgress: null, lifecycle: decodeStoredReceiveLifecycleState(record),
    }))
    expect(task.progress?.label).toBe('3.0 MiB retained for continuation')
    expect(task.progress?.details).toContain('0 completed files retained.')
    expect(task.publication).toBe('unpublished')
    const paused = presentTask(taskFixture({
      lifecycle: decodeStoredReceiveLifecycleState(record),
      progress: { ...EMPTY_V2_PROGRESS, discovery: 'complete', discoveredFiles: 1, discoveredBytes: 12n * MIB },
    }))
    expect(paused.progress?.label).toContain('3.0 MiB / 12.0 MiB written or reused')
    expect(paused.progress?.percentage).toBe(25)
  })

  it('counts authenticated reused payload once without adding overlapping receipt and completed bytes', () => {
    const task = presentTask(taskFixture({ progress: resumed }))
    expect(task.progress?.percentage).toBe(91)
    expect(task.publication).toBe('unpublished')
  })

  it('keeps an open denominator indeterminate even with authenticated reused payload', () => {
    const task = presentTask(taskFixture({ progress: { ...resumed, discovery: 'open' } }))
    expect(task.progress).toMatchObject({ mode: 'indeterminate', percentage: null })
  })

  it('updates direct ZIP receipt between checkpoints without advancing restart-safe bytes', () => {
    const task = presentTask(taskFixture({
      progress: { ...resumed, materializedBytes: 60n * MIB },
      directZipProgress: {
        ...TASK_FIXTURES.verifying!.directZipProgress!,
        receivedSelectedBytes: 77n * MIB,
        writtenSelectedBytes: 60n * MIB,
        safeResumeBytes: 32n * MIB,
      },
    }))
    expect(task.progress?.percentage).toBe(77)
    expect(task.progress?.label).toContain('77.0 MiB / 100.0 MiB received')
    expect(task.progress?.details).toContain('60.0 MiB written or reused in the ZIP.')
    expect(task.progress?.writtenBytes).toBe(MIB)
    expect(task.progress?.remainingBytes).toBe(23n * MIB)
    expect(task.progress?.details).toContain('32.0 MiB safe to resume after restart.')
  })

  it('retains saved direct ZIP progress before transfer replay rebuilds live observations', () => {
    const task = presentTask(taskFixture({
      progress: { ...resumed, materializedBytes: 0n },
      directZipProgress: {
        ...TASK_FIXTURES.verifying!.directZipProgress!,
        receivedSelectedBytes: 64n * MIB,
        writtenSelectedBytes: 64n * MIB,
        safeResumeBytes: 64n * MIB,
      },
    }))
    expect(task.progress?.percentage).toBe(64)
    expect(task.progress?.details).toContain('64.0 MiB safe to resume after restart.')
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
