import { describe, expect, it } from 'vitest'
import { CatalogPageStoreError } from '../../src/catalog/page-store'
import { byteRange } from '../../src/content/geometry'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { loadFSARestorePlan, writeFSARestoreArtifact } from '../../src/output/backends/fsa-restore'
import { nextFSALifecycle } from '../../src/output/backends/fsa-settlement'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import { summarizeStoredTaskFiles, type ReceiveFileRecord } from '../../src/output/store/types'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, MemoryReceiveStore } from './fsa-fixture'

const SIGNAL = new AbortController().signal
const SPEC = Object.freeze({ shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] })

function settled(
  status: SettledTransferJobResult['status'],
  overrides: Partial<SettledTransferJobResult> = {},
): SettledTransferJobResult {
  return {
    status,
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 0n,
    discoveredBytes: 0n,
    discoveryComplete: false,
    failedFiles: 0n,
    failedDirectories: 0n,
    ...overrides,
  }
}

async function seedPausedFolderWithSafeRename(taskId: string) {
  const memory = new MemoryReceiveStore()
  const store = memory as unknown as ReceiveStore
  const parent = new FakeDirectoryHandle('Downloads', memory.events, name => name.includes(':'))
  const traces: OutputTraceEvent[] = []
  const diagnostics = {
    backend: 'file_system_access' as const,
    trace: { current: (event: OutputTraceEvent) => traces.push(event) },
  }

  const run1 = await openNewFSASession({
    task: {
      taskId,
      selection: SPEC,
      selectionKey: `sel-${taskId}`,
      artifact: { kind: 'directory-tree', name: 'Bundle' },
      lifecycle: initialReceiveState(taskId),
    },
    parentHandle: parent as unknown as FileSystemDirectoryHandle,
    store,
    diagnostics,
  })

  const fileA = await run1.transferOutput.openFile({
    fileId: 'f-a', path: ['a.bin'], sourcePath: ['Bundle', 'a.bin'], revision: 'r1', exactSize: 4n,
  }, SIGNAL)
  await fileA.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
  await fileA.commit([byteRange(0n, 4n)])

  const fileRenamed = await run1.transferOutput.openFile({
    fileId: 'f-renamed',
    path: ['notes:2026.txt'],
    sourcePath: ['Bundle', 'notes:2026.txt'],
    revision: 'r2',
    exactSize: 6n,
  }, SIGNAL)
  await fileRenamed.writeAt(0n, Uint8Array.of(5, 6, 7, 8, 9, 10))
  await fileRenamed.commit([byteRange(0n, 6n)])

  const fileUnfinished = await run1.transferOutput.openFile({
    fileId: 'f-unfinished',
    path: ['sub', 'partial.bin'],
    sourcePath: ['Bundle', 'sub', 'partial.bin'],
    revision: 'r3',
    exactSize: 10n,
  }, SIGNAL)
  await fileUnfinished.writeAt(0n, Uint8Array.of(11, 12))
  await fileUnfinished.pause([byteRange(0n, 2n)])

  const paused = await run1.settle(settled('paused', {
    pauseReason: 'user',
    completedFiles: 2n,
    completedBytes: 10n,
    discoveredFiles: 3n,
    discoveredBytes: 20n,
  }))
  await run1.close()
  traces.length = 0

  return { memory, store, parent, traces, diagnostics, paused }
}

describe('FSA settlement against persisted IndexedDB file state', () => {
  it('preserves stored completed counts and safe-name metadata when a resumed folder re-pauses before discovery visits prior files', async () => {
    const { store, paused, diagnostics, traces } = await seedPausedFolderWithSafeRename('fsa-repause')

    const run2 = await reopenFSASession({
      task: paused,
      store,
      allowPermissionPrompt: false,
      diagnostics,
    })
    const repaused = await run2.settle(settled('paused', {
      pauseReason: 'interrupted',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 0n,
      discoveredBytes: 0n,
    }))
    await run2.close()

    expect(repaused.lifecycle).toMatchObject({
      kind: 'paused',
      reason: 'interrupted',
      completedFiles: 2n,
      completedBytes: 10n,
      retainedBytes: 12n,
      discoveredFiles: 3n,
      discoveredBytes: 20n,
      discoveryComplete: false,
    })
    expect(repaused.safeRenamed?.count).toBe(1)
    expect(traces).toContainEqual(expect.objectContaining({
      eventName: 'settlement',
      payload: expect.objectContaining({
        backend: 'file_system_access',
        task_id: 'fsa-repause',
        job_status: 'paused',
        lifecycle: 'paused',
        completed_files: '2',
      }),
    }))
  })

  it('uses stored completed counts instead of undercounting partial.savedFiles on resumed runs', async () => {
    const { store, paused } = await seedPausedFolderWithSafeRename('fsa-partial-count')

    const run2 = await reopenFSASession({ task: paused, store, allowPermissionPrompt: false })
    const partial = await run2.settle(settled('partial', {
      completedFiles: 1n,
      completedBytes: 4n,
      discoveredFiles: 2n,
      discoveredBytes: 14n,
      failedFiles: 1n,
    }))
    await run2.close()

    expect(partial.lifecycle).toMatchObject({
      kind: 'partial',
      reason: 'failures',
      savedFiles: 2n,
      failedFiles: 1n,
    })
    expect(partial.safeRenamed?.count).toBe(1)
  })

  it.each([
    {
      label: 'stopped',
      result: settled('stopped'),
      expectedReason: 'stopped',
      expectedFailedFiles: 0n,
    },
    {
      label: 'job-level failure with zero per-file failure counters',
      result: settled('failed', { error: new Error('markDiscoveryComplete failed') }),
      expectedReason: 'failures',
      expectedFailedFiles: 0n,
    },
    {
      label: 'catalog capacity limit failure',
      result: settled('failed', {
        failedDirectories: 1n,
        error: new CatalogPageStoreError('resource-limit', 'cache full'),
      }),
      expectedReason: 'browser-catalog-limit',
      expectedFailedFiles: 1n,
    },
    {
      label: 'source-changed',
      result: settled('source-changed', {
        failedFiles: 1n,
        sourceRevisionFailures: {
          shareInstance: 's1',
          count: 1n,
          files: [{ entryId: 'f-unfinished', path: ['sub', 'partial.bin'], sourcePath: ['Bundle', 'sub', 'partial.bin'], reason: 'source-changed' }],
        },
      }),
      expectedReason: 'failures',
      expectedFailedFiles: 1n,
    },
    {
      label: 'source-busy',
      result: settled('source-busy', {
        failedFiles: 1n,
        sourceRevisionFailures: {
          shareInstance: 's1',
          count: 1n,
          files: [{ entryId: 'f-unfinished', path: ['sub', 'partial.bin'], sourcePath: ['Bundle', 'sub', 'partial.bin'], reason: 'source-busy' }],
        },
      }),
      expectedReason: 'failures',
      expectedFailedFiles: 1n,
    },
  ] as const)(
    'promotes resumed $label with prior completed files to partial, cleans unfinished files, and preserves safe-name restoration',
    async ({ result, expectedReason, expectedFailedFiles }) => {
      const taskId = `fsa-promote-${result.status}-${expectedReason}`
      const { store, parent, paused, diagnostics, traces } = await seedPausedFolderWithSafeRename(taskId)

      const run2 = await reopenFSASession({
        task: paused,
        store,
        allowPermissionPrompt: false,
        diagnostics,
      })
      const terminal = await run2.settle(result)
      await run2.close()

      expect(terminal.lifecycle).toMatchObject({
        kind: 'partial',
        reason: expectedReason,
        savedFiles: 2n,
        failedFiles: expectedFailedFiles,
      })
      if (terminal.lifecycle.kind === 'partial') {
        expect(terminal.lifecycle.cleanupPending).toBeUndefined()
      }
      expect(terminal.safeRenamed?.count).toBe(1)
      if (result.sourceRevisionFailures !== undefined) {
        expect(terminal.sourceRevisionFailures).toEqual(result.sourceRevisionFailures)
      }

      // Unfinished file and its now-empty parent directory are cleaned up; completed files survive on disk.
      const bundleDir = parent.children.get('Bundle') as FakeDirectoryHandle
      expect(bundleDir.children.has('a.bin')).toBe(true)
      expect(bundleDir.children.has('sub')).toBe(false)

      // Safe-name restoration remains usable for the completed file saved in the prior run.
      const plan = await loadFSARestorePlan(store, taskId, 'posix')
      expect(plan.restorable.map(step => step.toName)).toEqual(['notes:2026.txt'])
      const exported = await writeFSARestoreArtifact(store, taskId, 'posix')
      expect(exported.fileName).toBe('windshare-restore-names.sh')

      expect(traces).toContainEqual(expect.objectContaining({
        eventName: 'settlement',
        payload: expect.objectContaining({
          backend: 'file_system_access',
          task_id: taskId,
          job_status: result.status,
          lifecycle: 'partial',
          completed_files: '2',
        }),
      }))
    },
  )

  it.each(['paused', 'stopped', 'failed'] as const)(
    'keeps a committed single-file FSA task saved across a late %s settlement and preserves safe-name restoration',
    async lateStatus => {
      const taskId = `fsa-single-${lateStatus}`
      const memory = new MemoryReceiveStore()
      const store = memory as unknown as ReceiveStore
      const parent = new FakeDirectoryHandle('Downloads', memory.events, name => name.includes(':'))

      const session = await openNewFSASession({
        task: {
          taskId,
          selection: SPEC,
          selectionKey: `sel-${taskId}`,
          artifact: { kind: 'directory-tree', name: 'report:final.txt', singleFileId: 'f1' },
          lifecycle: initialReceiveState(taskId),
        },
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        store,
      })

      const writer = await session.transferOutput.openFile({
        fileId: 'f1',
        path: ['report:final.txt'],
        sourcePath: ['report:final.txt'],
        revision: 'r1',
        exactSize: 4n,
      }, SIGNAL)
      await writer.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
      await writer.commit([byteRange(0n, 4n)])

      const record = await session.settle(settled(lateStatus, {
        ...(lateStatus === 'paused' ? { pauseReason: 'user' as const } : {}),
        ...(lateStatus === 'failed' ? { error: new Error('late post-commit failure') } : {}),
      }))
      await session.close()

      expect(record.lifecycle).toMatchObject({
        kind: 'saved',
        savedFiles: 1n,
        savedBytes: 4n,
      })
      expect(record.safeRenamed?.count).toBe(1)
      const plan = await loadFSARestorePlan(store, taskId, 'posix')
      expect(plan.restorable.map(step => step.toName)).toEqual(['report:final.txt'])
    },
  )

  it('falls back to failed when partial status arrives with zero persisted completed files', () => {
    const lifecycle = nextFSALifecycle(
      initialReceiveState('empty-partial'),
      settled('partial', { error: new Error('uncommitted failure') }),
      {
        singleFile: false,
        stored: summarizeStoredTaskFiles([]),
      },
    )
    expect(lifecycle).toMatchObject({
      kind: 'failed',
      reason: 'error',
      detail: 'uncommitted failure',
      cleanupPending: true,
    })
  })

  it('summarizes persisted file rows while excluding ZIP directory placeholders', () => {
    const rows: ReceiveFileRecord[] = [
      {
        id: 't:dir',
        taskId: 't',
        fileId: 'dir',
        revision: 'r',
        path: ['docs'],
        size: 0n,
        completedRanges: [],
        status: 'completed',
        zipEntry: {
          sequence: 0,
          kind: 'directory',
          localHeaderOffset: 0n,
          payloadOffset: 0n,
          descriptorOffset: 0n,
          endOffset: 0n,
          crcRanges: [],
        },
      },
      {
        id: 't:f1',
        taskId: 't',
        fileId: 'f1',
        revision: 'r1',
        path: ['docs', 'a.bin'],
        size: 10n,
        completedRanges: [[0n, 10n]],
        status: 'completed',
      },
      {
        id: 't:f2',
        taskId: 't',
        fileId: 'f2',
        revision: 'r2',
        path: ['docs', 'b.bin'],
        size: 20n,
        completedRanges: [[0n, 5n]],
        status: 'receiving',
      },
    ]

    expect(summarizeStoredTaskFiles(rows)).toEqual({
      fileCount: 2n,
      totalBytes: 30n,
      completedFiles: 1n,
      completedBytes: 10n,
      retainedBytes: 15n,
      hasUnfinished: true,
    })
  })
})
