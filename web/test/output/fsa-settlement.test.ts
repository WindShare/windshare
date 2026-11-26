import { describe, expect, it, vi } from 'vitest'
import { loadRetainedTasks, retainedTaskEnvironment } from '../../src/application/receive/retained'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import { CatalogPageStoreError } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { loadFSARestorePlan, writeFSARestoreArtifact } from '../../src/output/backends/fsa-restore'
import { nextFSALifecycle } from '../../src/output/backends/fsa-settlement'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import { summarizeStoredTaskFiles, type ReceiveFileRecord } from '../../src/output/store/types'
import { FileDestinationConflictError, OutputFaultCode } from '../../src/transfer/fault'
import { TransferJob, type SettledTransferJobResult, type TransferJobFacts } from '../../src/transfer/job'
import { initialReceiveState, type ReceivePartialReason } from '../../src/transfer/state'
import { FakeDirectoryHandle, FakeFileHandle, MemoryReceiveStore } from './fsa-fixture'

const SIGNAL = new AbortController().signal
const SPEC = Object.freeze({ shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] })

function settled(
  status: SettledTransferJobResult['status'],
  overrides: Partial<TransferJobFacts> & { readonly partialReason?: ReceivePartialReason } = {},
): SettledTransferJobResult {
  const { partialReason, ...facts } = overrides
  const base: TransferJobFacts = {
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 0n,
    discoveredBytes: 0n,
    discoveryComplete: false,
    failedFiles: 0n,
    failedDirectories: 0n,
    ...facts,
  }
  return status === 'partial'
    ? { ...base, status, partialReason: partialReason ?? 'failures' }
    : { ...base, status }
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
      result: settled('failed', { error: new Error('job-level output failure') }),
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
      settled('partial', { partialReason: 'failures', error: new Error('uncommitted failure') }),
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

    const stoppedEmpty = nextFSALifecycle(
      initialReceiveState('empty-stopped-partial'),
      settled('partial', { partialReason: 'stopped' }),
      {
        singleFile: false,
        stored: summarizeStoredTaskFiles([]),
      },
    )
    expect(stoppedEmpty).toMatchObject({
      kind: 'failed',
      reason: 'cancelled',
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
      completedFiles: 1n,
      completedBytes: 10n,
      retainedBytes: 15n,
      hasUnfinished: true,
    })
  })
})

describe('TransferJob partialReason and FSA terminal directory cleanup', () => {
  const shareInstance = new Uint8Array(16).fill(1)
  const rootId = new Uint8Array(16).fill(2)
  const rootText = encodeBase64Url(rootId)
  const makeId = (seed: number) => new Uint8Array(16).fill(seed)
  const payload = Uint8Array.of(1, 2, 3, 4)

  const dirEntry = (id: Uint8Array<ArrayBuffer>, name: string): CatalogEntry =>
    Object.freeze({ kind: 'directory', id, idText: encodeBase64Url(id), name })
  const fileEntry = (id: Uint8Array<ArrayBuffer>, name: string): CatalogEntry =>
    Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize: BigInt(payload.byteLength) })

  const makePage = (dirId: Uint8Array<ArrayBuffer>, entries: readonly CatalogEntry[], pageIndex = 0, terminal = true) => {
    const generation = new Uint8Array(16).fill(3)
    const dirText = encodeBase64Url(dirId)
    const committed: CommittedDirectory = Object.freeze({
      directoryId: dirId, directoryIdText: dirText, generation, generationText: encodeBase64Url(generation),
      pageCount: terminal ? pageIndex + 1 : pageIndex + 2, entryCount: entries.length, omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    })
    const page: CatalogPage = Object.freeze({
      shareInstance, directoryId: dirId, directoryIdText: dirText, generation, generationText: committed.generationText,
      pageIndex, terminal, previousCommitment: new Uint8Array(32), entries: Object.freeze([...entries]),
      omittedCount: 0n, objectCommitment: new Uint8Array(32), encodedBytes: 64,
    })
    return { committed, page }
  }

  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance, shareInstanceId: encodeBase64Url(shareInstance), syntheticRoot: rootId, syntheticRootId: rootText,
  }) as unknown as ShareDescriptor

  const openRevision = async (fileId: Uint8Array) => {
    const rev = new Uint8Array(16).fill(70)
    return Object.freeze({
      descriptor: Object.freeze({
        shareInstance, shareInstanceId: descriptor.shareInstanceId, fileId, fileIdText: encodeBase64Url(fileId),
        fileRevision: rev, fileRevisionText: encodeBase64Url(rev), exactSize: BigInt(payload.byteLength),
        geometry: new FileGeometry(BigInt(payload.byteLength), 64n),
      }) as FileRevisionDescriptor,
      leaseId: new Uint8Array(16).fill(99),
      release: async () => undefined,
    })
  }
  const broker: BlockRangeReader = {
    readRange: async function* () { yield Object.freeze({ offset: 0n, data: payload }) },
  }

  it('prunes discovery scaffolding on user stop after saved files even with prior failures', async () => {
    const okDirId = makeId(10)
    const scaffoldDirId = makeId(12)
    const fileOkId = makeId(20)
    const fileFailId = makeId(21)
    const filePendingId = makeId(22)

    const stopPages = new Map([
      [rootText, makePage(rootId, [
        dirEntry(okDirId, 'ok-dir'),
        dirEntry(scaffoldDirId, 'scaffolding'),
        fileEntry(fileOkId, 'ok.bin'),
        fileEntry(fileFailId, 'fail.bin'),
        fileEntry(filePendingId, 'pending.bin'),
      ])],
      [encodeBase64Url(okDirId), makePage(okDirId, [])],
      [encodeBase64Url(scaffoldDirId), makePage(scaffoldDirId, [])],
    ])
    const stopMem = new MemoryReceiveStore()
    const stopParent = new FakeDirectoryHandle('Downloads', stopMem.events)
    const stopSession = await openNewFSASession({
      task: {
        taskId: 'job-stop-after-failure', selection: SPEC, selectionKey: 'sel-stop-job',
        artifact: { kind: 'directory-tree', name: 'StoppedRun' }, lifecycle: initialReceiveState('job-stop-after-failure'),
      },
      parentHandle: stopParent as unknown as FileSystemDirectoryHandle,
      store: stopMem as unknown as ReceiveStore,
    })
    const jobRef: { current?: TransferJob } = {}
    const stopRevisions: RevisionReader = {
      open: async fileId => {
        if (fileId[0] === fileFailId[0]) throw new Error('permanent failure')
        if (fileId[0] === filePendingId[0]) {
          jobRef.current?.stop()
          throw new DOMException('stopped', 'AbortError')
        }
        return openRevision(fileId)
      },
    }
    const stopJob = new TransferJob({
      descriptor,
      catalog: {
        loadDirectory: async (dirId: Uint8Array) => stopPages.get(encodeBase64Url(dirId))!.committed,
        page: async (committed: CommittedDirectory) => stopPages.get(committed.directoryIdText)!.page,
      } as unknown as CatalogClient,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: stopRevisions,
      broker,
      output: {
        ...stopSession.transferOutput,
        capabilities: Object.freeze({ ...stopSession.transferOutput.capabilities, maxConcurrentFiles: 1 }),
      },
    })
    jobRef.current = stopJob
    const stopResult = await stopJob.run()
    expect(stopResult.status).toBe('partial')
    if (stopResult.status !== 'partial') throw new Error('expected partial result')
    expect(stopResult.partialReason).toBe('stopped')
    expect(stopResult.failedFiles).toBe(1n)

    const settledStop = await stopSession.settle(stopResult)
    await stopSession.close()
    expect(settledStop.lifecycle).toMatchObject({ kind: 'partial', reason: 'stopped', savedFiles: 1n, failedFiles: 1n })
    expect([...(stopParent.children.get('StoppedRun') as FakeDirectoryHandle).children.keys()]).toEqual(['ok.bin'])
  })

  it('preserves selected empty dirs on discovery page failure and keeps non-resumable stop as stopped', async () => {
    const emptyDirId = makeId(11)
    const fileOkId = makeId(20)
    const filePendingId = makeId(22)
    const page0 = makePage(rootId, [dirEntry(emptyDirId, 'keep-empty'), fileEntry(fileOkId, 'ok.bin')], 0, false)
    const pageFailMem = new MemoryReceiveStore()
    const pageFailParent = new FakeDirectoryHandle('Downloads', pageFailMem.events)
    const pageFailSession = await openNewFSASession({
      task: {
        taskId: 'job-page-fail', selection: SPEC, selectionKey: 'sel-page-fail',
        artifact: { kind: 'directory-tree', name: 'PageFailRun' }, lifecycle: initialReceiveState('job-page-fail'),
      },
      parentHandle: pageFailParent as unknown as FileSystemDirectoryHandle,
      store: pageFailMem as unknown as ReceiveStore,
    })
    const pageFailResult = await new TransferJob({
      descriptor,
      catalog: {
        loadDirectory: async () => page0.committed,
        page: async (_committed: CommittedDirectory, pageIndex: number) => {
          if (pageIndex === 0) return page0.page
          throw new Error('corrupt catalog page')
        },
      } as unknown as CatalogClient,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: openRevision },
      broker,
      output: {
        ...pageFailSession.transferOutput,
        capabilities: Object.freeze({ ...pageFailSession.transferOutput.capabilities, maxConcurrentFiles: 1 }),
      },
    }).run()
    expect(pageFailResult.status).toBe('partial')
    if (pageFailResult.status !== 'partial') throw new Error('expected partial result')
    expect(pageFailResult.partialReason).toBe('failures')
    expect(pageFailResult.failedFiles).toBe(0n)
    expect(pageFailResult.failedDirectories).toBe(0n)
    const settledPageFail = await pageFailSession.settle(pageFailResult)
    await pageFailSession.close()
    expect(settledPageFail.lifecycle).toMatchObject({ kind: 'partial', reason: 'failures', savedFiles: 1n, failedFiles: 0n })
    expect([...(pageFailParent.children.get('PageFailRun') as FakeDirectoryHandle).children.keys()].sort())
      .toEqual(['keep-empty', 'ok.bin'])

    const portablePage = makePage(rootId, [fileEntry(fileOkId, 'ok.bin'), fileEntry(filePendingId, 'p.bin')])
    const portableRef: { current?: TransferJob } = {}
    const portableJob = new TransferJob({
      descriptor,
      catalog: {
        loadDirectory: async () => portablePage.committed,
        page: async () => portablePage.page,
      } as unknown as CatalogClient,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async fileId => {
          if (fileId[0] === filePendingId[0]) {
            portableRef.current?.stop()
            throw new DOMException('stopped', 'AbortError')
          }
          return openRevision(fileId)
        },
      },
      broker,
      output: {
        sessionId: 'portable-stop',
        capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false },
        openFile: async () => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'skipped',
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    })
    portableRef.current = portableJob
    const portableResult = await portableJob.run()
    expect(portableResult.status).toBe('stopped')
  })
})

describe('FSA completed-file verification conflict settlement', () => {
  it.each(['deleted', 'truncated-zero'] as const)(
    'settles a crash-recovered single-file FSA task as failed (not saved) when the committed file is %s before resume',
    async mutation => {
      const taskId = `fsa-single-conflict-${mutation}`
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
          artifact: { kind: 'original-file', name: 'report:final.txt', singleFileId: 'f1' },
          lifecycle: initialReceiveState(taskId),
        },
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        store,
        diagnostics,
      })
      const writer = await run1.transferOutput.openFile({
        fileId: 'f1',
        path: ['report:final.txt'],
        sourcePath: ['report:final.txt'],
        revision: 'r1',
        exactSize: 4n,
      }, SIGNAL)
      await writer.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
      await writer.commit([byteRange(0n, 4n)])
      // Simulate crashing after committing the file row and before settle() persists `saved`.
      await run1.close()

      const recovered = (await loadRetainedTasks(retainedTaskEnvironment(store, { windowPort: {} as BrowserOutputWindow })))[0]!
      expect(recovered.lifecycle.kind).toBe('paused')
      const paused = (await memory.getTask(taskId))!
      const reservedName = paused.target.kind === 'fsa' ? paused.target.reservedName : ''
      const physical = parent.children.get(reservedName) as FakeFileHandle
      if (mutation === 'deleted') {
        await parent.removeEntry(reservedName)
        vi.spyOn(physical, 'getFile').mockRejectedValue(new DOMException('missing', 'NotFoundError'))
      } else {
        // Same timestamp as the initial 0-byte reservation tests that terminal cleanup never deletes an invalidated single file.
        const reservedMtime = paused.target.kind === 'fsa' && paused.target.singleFile
          ? paused.target.reservedFile.fingerprint.lastModified
          : Date.now()
        physical.overwriteExternally(new Uint8Array(0), reservedMtime)
      }

      traces.length = 0
      const run2 = await reopenFSASession({ task: paused, store, allowPermissionPrompt: false, diagnostics })
      let conflictError: unknown
      try {
        await run2.transferOutput.checkCompleted?.('f1', SIGNAL)
      } catch (error) {
        conflictError = error
      }
      expect(conflictError).toBeInstanceOf(FileDestinationConflictError)
      const destinationPath = (conflictError as FileDestinationConflictError).destinationPath

      const terminal = await run2.settle(settled('failed', {
        completedFiles: 0n,
        completedBytes: 0n,
        discoveredFiles: 1n,
        discoveredBytes: 4n,
        discoveryComplete: true,
        failedFiles: 1n,
        error: conflictError,
        contentWarning: {
          selectedFileCount: 1n,
          completedFileCount: 0n,
          missingFiles: [{ path: ['report:final.txt'], reason: 'destination-conflict', destinationPath }],
        },
      }))
      await run2.close()

      expect(terminal.lifecycle).toMatchObject({
        kind: 'failed',
        reason: 'error',
        contentWarning: {
          selectedFileCount: 1n,
          completedFileCount: 0n,
          missingFiles: [{ path: ['report:final.txt'], reason: 'destination-conflict', destinationPath }],
        },
      })
      if (terminal.lifecycle.kind === 'failed') {
        expect(terminal.lifecycle.cleanupPending).toBeUndefined()
      }
      expect(terminal.safeRenamed).toBeUndefined()
      expect(await memory.listTaskFiles(taskId)).toEqual([])
      expect((await loadFSARestorePlan(store, taskId, 'posix')).restorable).toEqual([])
      expect(parent.children.has(reservedName)).toBe(mutation !== 'deleted')
      if (mutation === 'truncated-zero') {
        expect([...physical.bytes]).toEqual([])
      }
      const conflictTrace = traces.find(event => event.payload.transition === 'completed_verification_conflict')
      expect(conflictTrace).toEqual({
        eventName: 'reopen',
        payload: {
          backend: 'file_system_access',
          task_id: taskId,
          file_id: 'f1',
          transition: 'completed_verification_conflict',
        },
      })
      expect(JSON.stringify(conflictTrace!.payload)).not.toContain('report:final.txt')
    },
  )

  it('prevents double-counting, stale safe-name restore steps, and false partial promotion on multi-file FSA resume conflicts', async () => {
    // 1. 2-file folder: safe-renamed file was edited before resume, other file completes -> partial(savedFiles: 1n, failedFiles: 1n).
    const partialCase = await seedPausedFolderWithSafeRename('fsa-multi-conflict-partial')
    const bundleDir = partialCase.parent.children.get('Bundle') as FakeDirectoryHandle
    const renamedRow = (await partialCase.memory.getFile('fsa-multi-conflict-partial', 'f-renamed'))!
    const renamedName = renamedRow.fsaPath![0]!
    const renamedHandle = bundleDir.children.get(renamedName) as FakeFileHandle
    renamedHandle.overwriteExternally(Uint8Array.of(99, 98))

    const run2 = await reopenFSASession({
      task: partialCase.paused,
      store: partialCase.store,
      allowPermissionPrompt: false,
      diagnostics: partialCase.diagnostics,
    })
    await expect(run2.transferOutput.checkCompleted?.('f-renamed', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    // Only `a.bin` (already completed) remains valid; `f-unfinished` also fails.
    const partial = await run2.settle(settled('partial', {
      completedFiles: 1n,
      completedBytes: 4n,
      discoveredFiles: 3n,
      discoveredBytes: 20n,
      discoveryComplete: true,
      failedFiles: 2n,
    }))
    await run2.close()

    expect(partial.lifecycle).toMatchObject({
      kind: 'partial',
      reason: 'failures',
      savedFiles: 1n,
      failedFiles: 2n,
    })
    expect(partial.safeRenamed).toBeUndefined()
    expect((await partialCase.memory.listTaskFiles('fsa-multi-conflict-partial')).map(row => row.fileId)).toEqual(['f-a'])
    expect((await loadFSARestorePlan(partialCase.store, 'fsa-multi-conflict-partial', 'posix')).restorable).toEqual([])
    expect([...renamedHandle.bytes]).toEqual([99, 98])

    // 2. All previously completed files were deleted before resume and remaining files fail -> settles as failed (not partial) and prunes empty root.
    const failedCase = await seedPausedFolderWithSafeRename('fsa-multi-conflict-failed')
    const failedBundle = failedCase.parent.children.get('Bundle') as FakeDirectoryHandle
    const failedRenamedRow = (await failedCase.memory.getFile('fsa-multi-conflict-failed', 'f-renamed'))!
    const failedRenamedName = failedRenamedRow.fsaPath![0]!
    const failedA = failedBundle.children.get('a.bin') as FakeFileHandle
    const failedRenamed = failedBundle.children.get(failedRenamedName) as FakeFileHandle
    await failedBundle.removeEntry('a.bin')
    await failedBundle.removeEntry(failedRenamedName)
    vi.spyOn(failedA, 'getFile').mockRejectedValue(new DOMException('missing', 'NotFoundError'))
    vi.spyOn(failedRenamed, 'getFile').mockRejectedValue(new DOMException('missing', 'NotFoundError'))

    const failedRun = await reopenFSASession({
      task: failedCase.paused,
      store: failedCase.store,
      allowPermissionPrompt: false,
    })
    await expect(failedRun.transferOutput.checkCompleted?.('f-a', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    await expect(failedRun.transferOutput.checkCompleted?.('f-renamed', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    const terminalFailed = await failedRun.settle(settled('failed', {
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 3n,
      discoveredBytes: 20n,
      discoveryComplete: true,
      failedFiles: 3n,
      error: new FileDestinationConflictError('Downloads/Bundle/a.bin'),
    }))
    await failedRun.close()

    expect(terminalFailed.lifecycle).toMatchObject({ kind: 'failed', reason: 'error' })
    if (terminalFailed.lifecycle.kind === 'failed') {
      expect(terminalFailed.lifecycle.cleanupPending).toBeUndefined()
    }
    expect(terminalFailed.safeRenamed).toBeUndefined()
    expect(await failedCase.memory.listTaskFiles('fsa-multi-conflict-failed')).toEqual([])
    expect(failedCase.parent.children.has('Bundle')).toBe(false)
    expect(failedCase.parent.stats.recursiveRemoveCalls).toBe(0)
  })

  it('retains the IndexedDB row across re-pause so 0-byte user truncations are never reclaimed and restored files recover cleanly', async () => {
    // 1. 0-byte user truncation across pause -> resume (conflict + re-pause) -> resume must never reclaim/overwrite the user file.
    const truncCase = await seedPausedFolderWithSafeRename('fsa-repause-zero-byte')
    const bundleDir = truncCase.parent.children.get('Bundle') as FakeDirectoryHandle
    const fileA = bundleDir.children.get('a.bin') as FakeFileHandle
    const writableBefore = fileA.writableCalls.length
    // Truncate to 0 bytes with mtime inside [reservedAtMs, reopenedAtMs], which would otherwise match isUncommittedEmptyPlaceholder.
    fileA.overwriteExternally(new Uint8Array(0), Date.now())

    const run2 = await reopenFSASession({ task: truncCase.paused, store: truncCase.store, allowPermissionPrompt: false })
    await expect(run2.transferOutput.checkCompleted?.('f-a', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    const repaused = await run2.settle(settled('paused', {
      pauseReason: 'interrupted',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 3n,
      discoveredBytes: 20n,
    }))
    await run2.close()

    // Paused counts exclude the invalidated file (`f-renamed` still counts = 1n, 6n bytes), while the IDB row survives re-pause.
    expect(repaused.lifecycle).toMatchObject({
      kind: 'paused',
      reason: 'interrupted',
      completedFiles: 1n,
      completedBytes: 6n,
      retainedBytes: 8n,
    })
    expect((await truncCase.memory.getFile('fsa-repause-zero-byte', 'f-a'))?.status).toBe('completed')

    const run3 = await reopenFSASession({ task: repaused, store: truncCase.store, allowPermissionPrompt: false })
    await expect(run3.transferOutput.checkCompleted?.('f-a', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    await expect(run3.transferOutput.openFile({
      fileId: 'f-a', path: ['a.bin'], sourcePath: ['Bundle', 'a.bin'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)).rejects.toBeInstanceOf(FileDestinationConflictError)
    expect(fileA.writableCalls).toHaveLength(writableBefore)
    expect(bundleDir.children.get('a.bin')).toBe(fileA)
    await run3.close()

    // 2. Temporarily moved file restored before a later resume verifies cleanly without conflict.
    const restoreCase = await seedPausedFolderWithSafeRename('fsa-repause-restored')
    const restoreBundle = restoreCase.parent.children.get('Bundle') as FakeDirectoryHandle
    const restoreA = restoreBundle.children.get('a.bin') as FakeFileHandle
    const missingSpy = vi.spyOn(restoreA, 'getFile').mockRejectedValue(new DOMException('missing', 'NotFoundError'))

    const missingRun = await reopenFSASession({ task: restoreCase.paused, store: restoreCase.store, allowPermissionPrompt: false })
    await expect(missingRun.transferOutput.checkCompleted?.('f-a', SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    const pausedWhileMissing = await missingRun.settle(settled('paused', {
      pauseReason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
    }))
    await missingRun.close()

    missingSpy.mockRestore()
    const restoredRun = await reopenFSASession({ task: pausedWhileMissing, store: restoreCase.store, allowPermissionPrompt: false })
    expect(await restoredRun.transferOutput.checkCompleted?.('f-a', SIGNAL)).toEqual({ size: 4n })
    await restoredRun.close()
  })

  it('does not invalidate completed records when verification fails with PermissionDenied', async () => {
    const { memory, store, parent, paused, diagnostics, traces } = await seedPausedFolderWithSafeRename('fsa-perm-denied')
    const bundleDir = parent.children.get('Bundle') as FakeDirectoryHandle
    const fileA = bundleDir.children.get('a.bin') as FakeFileHandle
    const permSpy = vi.spyOn(fileA, 'getFile').mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'))

    const run2 = await reopenFSASession({ task: paused, store, allowPermissionPrompt: false, diagnostics })
    await expect(run2.transferOutput.checkCompleted?.('f-a', SIGNAL)).rejects.toMatchObject({
      fault: { code: OutputFaultCode.PermissionDenied },
    })
    const repaused = await run2.settle(settled('paused', {
      pauseReason: 'permission-needed',
      completedFiles: 0n,
      completedBytes: 0n,
    }))
    await run2.close()

    expect(permSpy).toHaveBeenCalledOnce()
    expect(repaused.lifecycle).toMatchObject({
      kind: 'paused',
      reason: 'permission-needed',
      completedFiles: 2n,
      completedBytes: 10n,
    })
    expect((await memory.getFile('fsa-perm-denied', 'f-a'))?.status).toBe('completed')
    expect(traces.some(event => event.payload.transition === 'completed_verification_conflict')).toBe(false)

    const run3 = await reopenFSASession({ task: repaused, store, allowPermissionPrompt: false })
    expect(await run3.transferOutput.checkCompleted?.('f-a', SIGNAL)).toEqual({ size: 4n })
    await run3.close()
  })
})
