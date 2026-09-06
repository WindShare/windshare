import { describe, expect, it, vi } from 'vitest'
import { projectSourceRevisionFailures, SOURCE_REVISION_FAILURE_DISPLAY_LIMIT } from '../../src/output/resume/source-revision-failures'
import type { TaskEntry } from '../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../src/output/origin-private/task-checkpoint/store'

describe('retained source revision failure projection', () => {
  it('scans bounded pages, preserves failed paths and counts failures beyond the displayed sample without writes', async () => {
    const entries = Array.from({ length: 300 }, (_, index): TaskEntry => ({
      entryId: String(index), path: ['archive', String(index)], kind: 'file',
      source: { shareInstance: 'share', directoryId: 'root', generation: 'original', sourcePath: ['source', String(index)] },
      revision: { fileId: String(index), fileRevision: 'old', exactSize: 8n }, ranges: [],
      zipLayout: { entryId: String(index), sequence: BigInt(index), localHeaderOffset: 0n,
        payloadOffset: 1n, exactSize: 8n, descriptorOffset: 9n, endOffset: 10n, encodingVersion: 1 },
      ...(index % 2 === 0 ? { revisionFailure: 'revision-changed' } : {}),
    }))
    const readEntries = vi.fn(async ({ afterSequence, limit }: { afterSequence?: bigint; limit: number }) =>
      entries.slice(Number((afterSequence ?? -1n) + 1n), Number((afterSequence ?? -1n) + 1n) + limit))
    const commit = vi.fn(async () => { throw new Error('Read-only projection wrote metadata') })
    const store: TaskCheckpointStore = {
      readEntries, commit, readCheckpoint: async () => undefined, readEntry: async () => undefined,
      readDirectoryPin: async () => undefined, readPath: async () => undefined, close: () => undefined,
    }
    const summary = await projectSourceRevisionFailures(store, 'share')
    expect(summary?.count).toBe(150n)
    expect(summary?.files).toHaveLength(SOURCE_REVISION_FAILURE_DISPLAY_LIMIT)
    expect(summary?.files[0]).toEqual({ entryId: '0', path: ['archive', '0'], sourcePath: ['source', '0'] })
    expect(readEntries.mock.calls.every(([request]) => request.limit <= 128)).toBe(true)
    expect(readEntries).toHaveBeenCalledTimes(4)
    expect(commit).not.toHaveBeenCalled()
    expect(entries[0]?.revision?.fileRevision).toBe('old')
  })
})
