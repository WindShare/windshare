import { describe, expect, it, vi } from 'vitest'
import { admitRetainedAction, actionsForState } from '../../src/application/receive/admission'
import { ExecutionState, WorkspaceTask } from '../../src/application/receive/execution-state'
import { loadRetainedTasks, retainedTaskEnvironment } from '../../src/application/receive/retained'
import { ReceiveSessionRunner } from '../../src/application/receive/session-runner'
import { byteRange } from '../../src/content/geometry'
import type { BrowserOutputWindow, OutputSessionHandle } from '../../src/output/backends/contract'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import type { FrozenSelectionPolicy } from '../../src/catalog/selection'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { loadFSARestorePlan, writeFSARestoreArtifact } from '../../src/output/backends/fsa-restore'
import { deterministicSafeComponent } from '../../src/output/backends/safe-name'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { FSARestoreTargetMissingError } from '../../src/output/model/safe-name-restore'
import type { ReceiveStore } from '../../src/output/store/database'
import { acquireTaskLock, TaskBusyError } from '../../src/output/store/tab-lock'
import { FileDestinationConflictError } from '../../src/transfer/fault'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState, transitionReceiveState, type SafeRenamedSummary } from '../../src/transfer/state'
import { presentTask } from '../../src/ui/tasks/present'
import { taskFixture } from '../../src/ui/tasks/fixtures'
import { FakeDirectoryHandle, FakeFileHandle, MemoryReceiveStore } from './fsa-fixture'

const SIGNAL = new AbortController().signal

function createFakeLockManager(): LockManager {
  const held = new Set<string>()
  return {
    request: async <T>(name: string, options: LockOptions, callback: LockGrantedCallback<T>) => {
      if (options.ifAvailable && held.has(name)) {
        return callback(null)
      }
      held.add(name)
      try {
        return await callback({ name, mode: options.mode ?? 'exclusive' })
      } finally {
        held.delete(name)
      }
    },
    query: async () => ({ held: [], pending: [] }),
  } as unknown as LockManager
}

function settled(status: SettledTransferJobResult['status'], overrides: Partial<SettledTransferJobResult> = {}): SettledTransferJobResult {
  return {
    status,
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 0n,
    discoveredBytes: 0n,
    discoveryComplete: true,
    failedFiles: 0n,
    failedDirectories: 0n,
    ...overrides,
  }
}

describe('FSA safe-name live notification, single source of truth, resume, and crash recovery', () => {
  it('emits live subscribeSafeRenamed immediately on first file or directory rename before settle', async () => {
    const memory = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', memory.events, ['empty.cfg', 'app.dll'])
    const session = await openNewFSASession({
      task: {
        taskId: 'live-task',
        selectionKey: 'sel',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'Toolkit' },
        lifecycle: initialReceiveState('live-task'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: memory as unknown as ReceiveStore,
    })

    const observed: SafeRenamedSummary[] = []
    const unsubscribe = session.subscribeSafeRenamed?.(summary => observed.push(summary))
    expect(unsubscribe).toBeTypeOf('function')

    await session.transferOutput.ensureDirectory?.({ directoryId: 'd1', path: ['empty.cfg'] }, SIGNAL)
    expect(observed).toHaveLength(1)
    expect(observed[0]!.count).toBe(1)

    const writer = await session.transferOutput.openFile({
      fileId: 'f1', path: ['app.dll'], sourcePath: ['Toolkit', 'app.dll'], revision: 'rev1', exactSize: 4n,
    }, SIGNAL)
    expect(observed).toHaveLength(2)
    expect(observed[1]!.count).toBe(2)

    await writer.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    await writer.commit([byteRange(0n, 4n)])
    unsubscribe?.()
    await session.close()
  })

  it('preserves renames across pause/resume without overwrites and drops pruned/unfinished ghost counts on partial or failed settle', async () => {
    const memory = new MemoryReceiveStore()
    const store = memory as unknown as ReceiveStore
    const parent = new FakeDirectoryHandle('Downloads', memory.events, ['a.cfg', 'b.dll', 'part.dll', 'empty.cfg', 'abandoned.cfg'])

    const run1 = await openNewFSASession({
      task: {
        taskId: 'resume-safe',
        selectionKey: 'sel',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'App' },
        lifecycle: initialReceiveState('resume-safe'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })

    // Run 1: completed a.cfg + empty renamed dir empty.cfg + unfinished renamed file part.dll under renamed dir abandoned.cfg
    await run1.transferOutput.ensureDirectory?.({ directoryId: 'd-empty', path: ['empty.cfg'] }, SIGNAL)
    const fA = await run1.transferOutput.openFile({
      fileId: 'fa', path: ['a.cfg'], sourcePath: ['App', 'a.cfg'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await fA.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    await fA.commit([byteRange(0n, 4n)])

    const fPart = await run1.transferOutput.openFile({
      fileId: 'fpart', path: ['abandoned.cfg', 'part.dll'], sourcePath: ['App', 'abandoned.cfg', 'part.dll'], revision: 'r2', exactSize: 8n,
    }, SIGNAL)
    await fPart.writeAt(0n, Uint8Array.of(5, 6))
    await fPart.pause([byteRange(0n, 2n)])

    const pausedTask = await run1.settle(settled('paused', {
      pauseReason: 'user', completedFiles: 1n, completedBytes: 4n, discoveredFiles: 2n, discoveredBytes: 12n,
    }))
    await run1.close()

    // Persisted paused record excludes unfinished part.dll (counts completed a.cfg + retained dirs empty.cfg & abandoned.cfg).
    expect(pausedTask.safeRenamed?.count).toBe(3)
    expect(pausedTask.safeRenamedDirectories?.map(d => d.logicalPath)).toEqual([['empty.cfg'], ['abandoned.cfg']])

    // Run 2 reopen: live registry seeds completed + receiving + dirs without gap, then adds b.dll, while part.dll fails & is cleaned up.
    const run2 = await reopenFSASession({ task: pausedTask, store, allowPermissionPrompt: false })
    expect(run2.transferOutput.safeRenamedSummary?.()?.count).toBe(4)

    const liveEvents: number[] = []
    run2.subscribeSafeRenamed?.(summary => liveEvents.push(summary.count))

    const fB = await run2.transferOutput.openFile({
      fileId: 'fb', path: ['b.dll'], sourcePath: ['App', 'b.dll'], revision: 'r3', exactSize: 4n,
    }, SIGNAL)
    expect(liveEvents).toEqual([5])
    await fB.writeAt(0n, Uint8Array.of(7, 8, 9, 10))
    await fB.commit([byteRange(0n, 4n)])

    // Partial settlement cleans unfinished part.dll and prunes its now-empty Run-1 parent abandoned.cfg, then reconciles putTask.
    const partialTask = await run2.settle(settled('partial', {
      completedFiles: 2n, completedBytes: 8n, discoveredFiles: 3n, discoveredBytes: 20n, failedFiles: 1n,
    }))
    await run2.close()

    expect(partialTask.lifecycle.kind).toBe('partial')
    expect(partialTask.safeRenamed?.count).toBe(3)
    expect(partialTask.safeRenamedDirectories?.map(d => d.logicalPath)).toEqual([['empty.cfg']])

    const plan = await loadFSARestorePlan(store, 'resume-safe', 'windows')
    expect(plan.restorable.map(s => s.logicalPath.join('/'))).toEqual(['a.cfg', 'b.dll', 'empty.cfg'])
    expect(plan.unsupported).toEqual([])
  })

  it('keeps putTask before cleanup on failed settle, strips safeRenamed on failure, and distinguishes single-file collisionIndex', async () => {
    const memory = new MemoryReceiveStore()
    const store = memory as unknown as ReceiveStore
    const parent = new FakeDirectoryHandle('Downloads', memory.events, ['tool.dll'])
    parent.putUserFile('report.pdf', Uint8Array.of(9))

    // Failed task must not persist safeRenamed and must not clean disk if putTask throws.
    const failSession = await openNewFSASession({
      task: {
        taskId: 'fail-safe',
        selectionKey: 'sel-fail',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'FailDir' },
        lifecycle: initialReceiveState('fail-safe'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })
    const writer = await failSession.transferOutput.openFile({
      fileId: 'f1', path: ['tool.dll'], sourcePath: ['FailDir', 'tool.dll'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await writer.writeAt(0n, Uint8Array.of(1, 2))
    await writer.pause([byteRange(0n, 2n)])
    vi.spyOn(memory, 'putTask').mockRejectedValueOnce(new Error('idb full'))
    await expect(failSession.settle(settled('failed'))).rejects.toThrow('idb full')
    expect(parent.children.has('FailDir')).toBe(true)

    const failedRecord = await failSession.settle(settled('failed'))
    await failSession.close()
    expect(failedRecord.safeRenamed).toBeUndefined()
    expect(failedRecord.safeRenamedDirectories).toBeUndefined()

    // Single file with collisionIndex=1 ("report (1).pdf") must NOT be misreported as safe-renamed across resume.
    const singleSession = await openNewFSASession({
      task: {
        taskId: 'single-collision',
        selectionKey: 'sel-single',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'report.pdf', singleFileId: 'sf1' },
        lifecycle: initialReceiveState('single-collision'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })
    const sf1 = await singleSession.transferOutput.openFile({
      fileId: 'sf1', path: ['report.pdf'], sourcePath: ['report.pdf'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await sf1.writeAt(0n, Uint8Array.of(1, 2))
    await sf1.pause([byteRange(0n, 2n)])
    const pausedSingle = await singleSession.settle(settled('paused', { pauseReason: 'user' }))
    await singleSession.close()
    expect(pausedSingle.safeRenamed).toBeUndefined()

    const resumedSingle = await reopenFSASession({ task: pausedSingle, store, allowPermissionPrompt: false })
    expect(resumedSingle.transferOutput.safeRenamedSummary?.()).toBeUndefined()
    const sf2 = await resumedSingle.transferOutput.openFile({
      fileId: 'sf1', path: ['report.pdf'], sourcePath: ['report.pdf'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await sf2.writeAt(2n, Uint8Array.of(3, 4))
    await sf2.commit([byteRange(0n, 4n)])
    const savedSingle = await resumedSingle.settle(settled('completed', { completedFiles: 1n, completedBytes: 4n }))
    await resumedSingle.close()
    expect(savedSingle.safeRenamed).toBeUndefined()
    expect((await loadFSARestorePlan(store, 'single-collision', 'windows')).totalCount).toBe(0)
  })

  it('reconstructs safeRenamed during crash recovery in pauseFromCheckpoints', async () => {
    const memory = new MemoryReceiveStore()
    const store = memory as unknown as ReceiveStore
    const parent = new FakeDirectoryHandle('Downloads', memory.events, ['pyvenv.cfg'])
    const session = await openNewFSASession({
      task: {
        taskId: 'crashed-fsa',
        selectionKey: 'sel-crash',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'Env' },
        lifecycle: initialReceiveState('crashed-fsa'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })
    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['pyvenv.cfg'], sourcePath: ['Env', 'pyvenv.cfg'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await file.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    await file.commit([byteRange(0n, 4n)])
    // Abrupt crash: close lock without calling settle(), leaving task in 'downloading' without safeRenamed.
    await session.close()
    expect((await store.getTask('crashed-fsa'))?.safeRenamed).toBeUndefined()

    const snapshots = await loadRetainedTasks(retainedTaskEnvironment(store, { windowPort: {} as BrowserOutputWindow }))
    expect(snapshots).toHaveLength(1)
    expect(snapshots[0]!.lifecycle.kind).toBe('paused')
    expect(snapshots[0]!.safeRenamed?.count).toBe(1)
    expect(snapshots[0]!.safeRenamed?.sample[0]!.logicalPath).toEqual(['pyvenv.cfg'])
  })

  it('isolates re-shared safe directory names across tasks in both discovery orders and restores only the renamed directory', async () => {
    const priorSafeName = await deterministicSafeComponent({
      taskId: 'prior-task',
      logicalPath: ['empty.cfg'],
      entryKind: 'directory',
    })

    for (const order of ['legal-first', 'rejected-first'] as const) {
      const memory = new MemoryReceiveStore()
      const store = memory as unknown as ReceiveStore
      const parent = new FakeDirectoryHandle('Downloads', memory.events, ['empty.cfg'])
      const taskId = `reshare-${order}`
      const session = await openNewFSASession({
        task: {
          taskId,
          selectionKey: `sel-${order}`,
          selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
          artifact: { kind: 'directory-tree', name: 'Bundle' },
          lifecycle: initialReceiveState(taskId),
        },
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        store,
      })

      const writeDirFile = async (dir: string, fileId: string, fileName: string, payload: number) => {
        await session.transferOutput.ensureDirectory?.({ directoryId: `d-${dir}`, path: [dir] }, SIGNAL)
        const writer = await session.transferOutput.openFile({
          fileId, path: [dir, fileName], sourcePath: ['Bundle', dir, fileName], revision: `rev-${fileId}`, exactSize: 1n,
        }, SIGNAL)
        await writer.writeAt(0n, Uint8Array.of(payload))
        await writer.commit([byteRange(0n, 1n)])
      }

      if (order === 'legal-first') {
        await writeDirFile(priorSafeName, 'f-legal', 'legal.txt', 1)
        await writeDirFile('empty.cfg', 'f-rej', 'rejected.txt', 2)
      } else {
        await writeDirFile('empty.cfg', 'f-rej', 'rejected.txt', 2)
        await writeDirFile(priorSafeName, 'f-legal', 'legal.txt', 1)
      }

      const saved = await session.settle(settled('completed', { completedFiles: 2n, completedBytes: 2n }))
      await session.close()

      const newSafeName = await deterministicSafeComponent({ taskId, logicalPath: ['empty.cfg'], entryKind: 'directory' })
      expect(newSafeName).not.toBe(priorSafeName)
      const root = parent.children.get('Bundle') as FakeDirectoryHandle
      expect([...root.children.keys()].sort()).toEqual([newSafeName, priorSafeName].sort())
      expect([...(root.children.get(priorSafeName) as FakeDirectoryHandle).children.keys()]).toEqual(['legal.txt'])
      expect([...(root.children.get(newSafeName) as FakeDirectoryHandle).children.keys()]).toEqual(['rejected.txt'])
      expect(saved.safeRenamed?.count).toBe(1)

      const plan = await loadFSARestorePlan(store, taskId, 'windows')
      expect(plan.restorable.map(step => [step.fromName, step.toName])).toEqual([[newSafeName, 'empty.cfg']])
    }
  })

  it('rejects same-task physical directory collisions explicitly across both orders, pause/resume, while preserving crash reuse', async () => {
    // 1. Both discovery orders in a single session reject collision without merging files or polluting restore plan.
    for (const order of ['legal-first', 'rejected-first'] as const) {
      const memory = new MemoryReceiveStore()
      const store = memory as unknown as ReceiveStore
      const parent = new FakeDirectoryHandle('Downloads', memory.events, ['empty.cfg'])
      const taskId = `collide-${order}`
      const collidingName = await deterministicSafeComponent({
        taskId,
        logicalPath: ['empty.cfg'],
        entryKind: 'directory',
      })
      const traces: OutputTraceEvent[] = []
      const session = await openNewFSASession({
        task: {
          taskId,
          selectionKey: `sel-${order}`,
          selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
          artifact: { kind: 'directory-tree', name: 'Pkg' },
          lifecycle: initialReceiveState(taskId),
        },
        parentHandle: parent as unknown as FileSystemDirectoryHandle,
        store,
        diagnostics: { backend: 'file_system_access', trace: { current: event => traces.push(event) } },
      })

      if (order === 'legal-first') {
        const legalFile = await session.transferOutput.openFile({
          fileId: 'f-legal', path: [collidingName, 'keep.txt'], sourcePath: ['Pkg', collidingName, 'keep.txt'], revision: 'r1', exactSize: 1n,
        }, SIGNAL)
        await legalFile.writeAt(0n, Uint8Array.of(7))
        await legalFile.commit([byteRange(0n, 1n)])

        await expect(session.transferOutput.ensureDirectory?.({ directoryId: 'd-rej', path: ['empty.cfg'] }, SIGNAL))
          .rejects.toBeInstanceOf(FileDestinationConflictError)
        await expect(session.transferOutput.openFile({
          fileId: 'f-rej', path: ['empty.cfg', 'bad.txt'], sourcePath: ['Pkg', 'empty.cfg', 'bad.txt'], revision: 'r2', exactSize: 1n,
        }, SIGNAL)).rejects.toBeInstanceOf(FileDestinationConflictError)

        const partial = await session.settle(settled('partial', { completedFiles: 1n, completedBytes: 1n, failedFiles: 1n, failedDirectories: 1n }))
        await session.close()
        const dir = (parent.children.get('Pkg') as FakeDirectoryHandle).children.get(collidingName) as FakeDirectoryHandle
        expect([...dir.children.keys()]).toEqual(['keep.txt'])
        expect(partial.safeRenamed).toBeUndefined()
      } else {
        const rejFile = await session.transferOutput.openFile({
          fileId: 'f-rej', path: ['empty.cfg', 'from-rej.txt'], sourcePath: ['Pkg', 'empty.cfg', 'from-rej.txt'], revision: 'r1', exactSize: 1n,
        }, SIGNAL)
        await rejFile.writeAt(0n, Uint8Array.of(9))
        await rejFile.commit([byteRange(0n, 1n)])

        await expect(session.transferOutput.ensureDirectory?.({ directoryId: 'd-legal', path: [collidingName] }, SIGNAL))
          .rejects.toBeInstanceOf(FileDestinationConflictError)
        await expect(session.transferOutput.openFile({
          fileId: 'f-legal', path: [collidingName, 'other.txt'], sourcePath: ['Pkg', collidingName, 'other.txt'], revision: 'r2', exactSize: 1n,
        }, SIGNAL)).rejects.toBeInstanceOf(FileDestinationConflictError)

        await session.settle(settled('partial', { completedFiles: 1n, completedBytes: 1n, failedFiles: 1n, failedDirectories: 1n }))
        await session.close()
        const dir = (parent.children.get('Pkg') as FakeDirectoryHandle).children.get(collidingName) as FakeDirectoryHandle
        expect([...dir.children.keys()]).toEqual(['from-rej.txt'])
        const plan = await loadFSARestorePlan(store, taskId, 'windows')
        expect(plan.restorable.map(step => [step.fromName, step.toName])).toEqual([[collidingName, 'empty.cfg']])
      }

      const collisionTrace = traces.find(event => event.payload.transition === 'directory_collision')
      expect(collisionTrace).toBeDefined()
      expect(JSON.stringify(collisionTrace!.payload)).not.toContain('empty.cfg')
      expect(JSON.stringify(collisionTrace!.payload)).not.toContain(collidingName)
    }

    // 2. Across pause/resume: seeded unrenamed legal directory still blocks colliding safe rename on reopen.
    const pauseMem = new MemoryReceiveStore()
    const pauseStore = pauseMem as unknown as ReceiveStore
    const pauseParent = new FakeDirectoryHandle('Downloads', pauseMem.events, ['empty.cfg'])
    const pauseSafe = await deterministicSafeComponent({ taskId: 'pause-collide', logicalPath: ['empty.cfg'], entryKind: 'directory' })
    const run1 = await openNewFSASession({
      task: {
        taskId: 'pause-collide',
        selectionKey: 'sel-pause',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'PausePkg' },
        lifecycle: initialReceiveState('pause-collide'),
      },
      parentHandle: pauseParent as unknown as FileSystemDirectoryHandle,
      store: pauseStore,
    })
    const f1 = await run1.transferOutput.openFile({
      fileId: 'f1', path: [pauseSafe, 'a.txt'], sourcePath: ['PausePkg', pauseSafe, 'a.txt'], revision: 'r1', exactSize: 1n,
    }, SIGNAL)
    await f1.writeAt(0n, Uint8Array.of(1))
    await f1.commit([byteRange(0n, 1n)])
    const paused = await run1.settle(settled('paused', { pauseReason: 'user', completedFiles: 1n, completedBytes: 1n }))
    await run1.close()

    const run2 = await reopenFSASession({ task: paused, store: pauseStore, allowPermissionPrompt: false })
    await expect(run2.transferOutput.ensureDirectory?.({ directoryId: 'd-rej', path: ['empty.cfg'] }, SIGNAL))
      .rejects.toBeInstanceOf(FileDestinationConflictError)
    await run2.close()

    // 3. Crash before settle: reopening the same task seamlessly reuses its own pre-crash safe directory.
    const crashMem = new MemoryReceiveStore()
    const crashStore = crashMem as unknown as ReceiveStore
    const crashParent = new FakeDirectoryHandle('Downloads', crashMem.events, ['empty.cfg'])
    const crashRun1 = await openNewFSASession({
      task: {
        taskId: 'crash-reuse',
        selectionKey: 'sel-crash-reuse',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'CrashPkg' },
        lifecycle: initialReceiveState('crash-reuse'),
      },
      parentHandle: crashParent as unknown as FileSystemDirectoryHandle,
      store: crashStore,
    })
    await crashRun1.transferOutput.ensureDirectory?.({ directoryId: 'd1', path: ['empty.cfg'] }, SIGNAL)
    await crashRun1.close() // abrupt crash without settle()

    const recovered = (await loadRetainedTasks(retainedTaskEnvironment(crashStore, { windowPort: {} as BrowserOutputWindow })))[0]!
    const crashTask = (await crashStore.getTask(recovered.taskId))!
    const crashRun2 = await reopenFSASession({ task: crashTask, store: crashStore, allowPermissionPrompt: false })
    const afterCrash = await crashRun2.transferOutput.openFile({
      fileId: 'f-after', path: ['empty.cfg', 'ok.txt'], sourcePath: ['CrashPkg', 'empty.cfg', 'ok.txt'], revision: 'r1', exactSize: 1n,
    }, SIGNAL)
    await afterCrash.writeAt(0n, Uint8Array.of(3))
    await afterCrash.commit([byteRange(0n, 1n)])
    await crashRun2.settle(settled('completed', { completedFiles: 1n, completedBytes: 1n }))
    await crashRun2.close()

    const crashSafe = await deterministicSafeComponent({ taskId: 'crash-reuse', logicalPath: ['empty.cfg'], entryKind: 'directory' })
    const crashRoot = crashParent.children.get('CrashPkg') as FakeDirectoryHandle
    expect([...crashRoot.children.keys()]).toEqual([crashSafe])
    expect([...(crashRoot.children.get(crashSafe) as FakeDirectoryHandle).children.keys()]).toEqual(['ok.txt'])
  })
})

describe('writeFSARestoreArtifact concurrency, user-activation order, and POSIX fallback', () => {
  async function createSavedTaskWithRename(rejectNames: readonly string[] = ['app.dll']) {
    const memory = new MemoryReceiveStore()
    const store = memory as unknown as ReceiveStore
    const parent = new FakeDirectoryHandle('Downloads', memory.events, rejectNames)
    const session = await openNewFSASession({
      task: {
        taskId: 'export-task',
        selectionKey: 'sel-exp',
        selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
        artifact: { kind: 'directory-tree', name: 'Bundle' },
        lifecycle: initialReceiveState('export-task'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })
    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['app.dll'], sourcePath: ['Bundle', 'app.dll'], revision: 'r1', exactSize: 4n,
    }, SIGNAL)
    await file.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    await file.commit([byteRange(0n, 4n)])
    const saved = await session.settle(settled('completed', { completedFiles: 1n, completedBytes: 4n }))
    await session.close()
    const root = parent.children.get('Bundle') as FakeDirectoryHandle
    return { memory, store, parent, root, saved }
  }

  it('acquires task lock, prompts permission BEFORE listTaskFiles, avoids overwrite, and emits trace without filenames', async () => {
    const { memory, store, parent, root } = await createSavedTaskWithRename()
    const locks = createFakeLockManager()
    const heldLock = await acquireTaskLock('export-task', locks)
    await expect(writeFSARestoreArtifact(store, 'export-task', 'windows', locks)).rejects.toBeInstanceOf(TaskBusyError)
    await heldLock.release()

    const order: string[] = []
    vi.spyOn(parent, 'queryPermission').mockImplementation(async () => {
      order.push('permission')
      return 'granted'
    })
    const origList = memory.listTaskFiles.bind(memory)
    vi.spyOn(memory, 'listTaskFiles').mockImplementation(async id => {
      order.push('listTaskFiles')
      return origList(id)
    })

    // Pre-existing manifest forces collision-safe "(1)" suffix without overwriting.
    root.putUserFile('windshare-restore-names.json', new TextEncoder().encode('existing'))
    const traces: OutputTraceEvent[] = []
    const result = await writeFSARestoreArtifact(store, 'export-task', 'windows', locks, {
      backend: 'file_system_access',
      trace: { current: event => traces.push(event) },
    })

    expect(order).toEqual(['permission', 'listTaskFiles'])
    expect(result.fileName).toBe('windshare-restore-names (1).json')
    expect(result.directoryLabel).toBe('Bundle')
    expect(result.runCommand).toContain('windshare-restore-names (1).json')
    expect(new TextDecoder().decode((root.children.get('windshare-restore-names.json') as FakeFileHandle).bytes)).toBe('existing')
    const writtenJson = JSON.parse(new TextDecoder().decode((root.children.get('windshare-restore-names (1).json') as FakeFileHandle).bytes))
    expect(writtenJson.restorable[0].toName).toBe('app.dll')

    expect(traces).toHaveLength(1)
    expect(traces[0]!.eventName).toBe('publication')
    expect(traces[0]!.payload).toMatchObject({
      operation: 'safe_name_restore_export',
      platform: 'windows',
      restorable_count: 1,
      used_fallback: false,
    })
    expect(JSON.stringify(traces[0]!.payload)).not.toContain('app.dll')
  })

  it('rejects paused/downloading tasks and maps missing destination folder to FSARestoreTargetMissingError', async () => {
    const { memory, store, parent, saved } = await createSavedTaskWithRename()
    const paused = {
      ...saved,
      lifecycle: transitionReceiveState(initialReceiveState('export-task'), {
        kind: 'paused', reason: 'user', completedFiles: 1n, completedBytes: 4n,
        retainedBytes: 4n, discoveredFiles: 2n, discoveredBytes: 8n, discoveryComplete: true,
      }),
    }
    await memory.putTask(paused)
    await expect(writeFSARestoreArtifact(store, 'export-task', 'windows')).rejects.toMatchObject({ name: 'InvalidStateError' })

    await memory.putTask(saved)
    await parent.removeEntry('Bundle', { recursive: true })
    await expect(writeFSARestoreArtifact(store, 'export-task', 'windows')).rejects.toBeInstanceOf(FSARestoreTargetMissingError)
  })

  it('cleans 0-byte .sh placeholder and falls back to .txt ONLY on policy/name errors, never on quota/permission/self-abort', async () => {
    // 1. Policy error during writer.close() -> abort writer + remove 0-byte .sh + fallback to .txt
    const { store, root } = await createSavedTaskWithRename()
    const abortSpy = vi.fn(async () => undefined)
    const origGetFile = root.getFileHandle.bind(root)
    vi.spyOn(root, 'getFileHandle').mockImplementation(async (name, options) => {
      const handle = await origGetFile(name, options)
      if (name === 'windshare-restore-names.sh' && options?.create) {
        const origCreate = handle.createWritable.bind(handle)
        vi.spyOn(handle, 'createWritable').mockImplementationOnce(async () => {
          const stream = await origCreate()
          return {
            ...stream,
            close: async () => { throw new DOMException('policy blocked', 'InvalidStateError') },
            abort: abortSpy,
          }
        })
      }
      return handle
    })

    const fallback = await writeFSARestoreArtifact(store, 'export-task', 'posix')
    expect(abortSpy).toHaveBeenCalledOnce()
    expect(root.children.has('windshare-restore-names.sh')).toBe(false)
    expect(root.children.has('windshare-restore-names.txt')).toBe(true)
    expect(fallback.fileName).toBe('windshare-restore-names.txt')
    expect(fallback.runCommand).toBe('sh ./windshare-restore-names.txt')

    // 2. Real QuotaExceededError / NotAllowedError must propagate WITHOUT falling back to .txt.
    for (const errName of ['QuotaExceededError', 'NotAllowedError'] as const) {
      const fresh = await createSavedTaskWithRename()
      const orig = fresh.root.getFileHandle.bind(fresh.root)
      vi.spyOn(fresh.root, 'getFileHandle').mockImplementation(async (name, options) => {
        const handle = await orig(name, options)
        if (name === 'windshare-restore-names.sh' && options?.create) {
          vi.spyOn(handle, 'createWritable').mockRejectedValueOnce(new DOMException('real error', errName))
        }
        return handle
      })
      await expect(writeFSARestoreArtifact(fresh.store, 'export-task', 'posix')).rejects.toMatchObject({ name: errName })
      expect(fresh.root.children.has('windshare-restore-names.sh')).toBe(false)
      expect(fresh.root.children.has('windshare-restore-names.txt')).toBe(false)
    }

    // 3. Self-aborted AbortError must NOT fall back to .txt.
    const selfAbort = await createSavedTaskWithRename()
    const controller = new AbortController()
    const origGet = selfAbort.root.getFileHandle.bind(selfAbort.root)
    vi.spyOn(selfAbort.root, 'getFileHandle').mockImplementation(async (name, options) => {
      const handle = await origGet(name, options)
      if (name === 'windshare-restore-names.sh' && options?.create) {
        vi.spyOn(handle, 'createWritable').mockImplementationOnce(async () => {
          controller.abort(new DOMException('cancelled', 'AbortError'))
          throw controller.signal.reason
        })
      }
      return handle
    })
    await expect(writeFSARestoreArtifact(selfAbort.store, 'export-task', 'posix', undefined, undefined, controller.signal))
      .rejects.toMatchObject({ name: 'AbortError' })
    expect(selfAbort.root.children.has('windshare-restore-names.txt')).toBe(false)
  })
})

describe('WorkspaceTask live observation and TaskPresentation safeNameRestore state derivation', () => {
  it('tracks count changes cleanly and derives safeNameRestore from facts rather than TaskStage', () => {
    const workspace = new WorkspaceTask()
    expect(workspace.observeSafeRenamed(null)).toBe(false)
    const one = { count: 1, sample: [{ logicalPath: ['a.dll'], physicalName: 'a-dll.windshare-11111111' }] }
    expect(workspace.observeSafeRenamed(one)).toBe(true)
    expect(workspace.observeSafeRenamed(one)).toBe(false)
    expect(workspace.observeSafeRenamed({ ...one, count: 2 })).toBe(true)

    const pausedNeedsAction = presentTask(taskFixture({
      backend: 'fsa',
      lifecycle: transitionReceiveState(initialReceiveState('t1'), {
        kind: 'paused', reason: 'permission-needed', completedFiles: 1n, completedBytes: 4n,
        retainedBytes: 4n, discoveredFiles: 2n, discoveredBytes: 8n, discoveryComplete: true,
      }),
      safeRenamed: one,
    }), 'en')
    expect(pausedNeedsAction.stage).toBe('needs-action')
    expect(pausedNeedsAction.safeNameRestore).toBe('paused')

    const active = presentTask(taskFixture({ backend: 'fsa', safeRenamed: one }), 'en')
    expect(active.safeNameRestore).toBe('active')

    const saved = presentTask(taskFixture({
      backend: 'fsa',
      lifecycle: transitionReceiveState(initialReceiveState('t2'), { kind: 'saved', savedFiles: 1n, savedBytes: 4n }),
      safeRenamed: one,
    }), 'en')
    expect(saved.safeNameRestore).toBe('available')

    const none = presentTask(taskFixture({ backend: 'fsa', safeRenamed: null }), 'en')
    expect(none.safeNameRestore).toBe('none')
  })

  it('mutually excludes export-restore with forget/discard in ExecutionState and pushes live safeRenamed via ReceiveSessionRunner without byte progress', async () => {
    const execution = new ExecutionState()
    const token = execution.claimLocalAction('t1', 'export-restore')
    expect(token).not.toBeNull()
    expect(execution.pendingRetained).toEqual({ taskId: 't1', action: 'export-restore' })
    expect(execution.claimLocalAction('t1', 'forget')).toBeNull()
    expect(execution.claimLocalAction('t1', 'discard')).toBeNull()

    const savedState = transitionReceiveState(initialReceiveState('t1'), { kind: 'saved', savedFiles: 1n, savedBytes: 4n })
    const admission = admitRetainedAction({
      taskId: 't1', shareInstance: 's1', selectionKey: 'k1', backend: 'fsa', lifecycle: savedState,
      actions: actionsForState(savedState, 'fsa'), activeElsewhere: false, canSavePartial: false,
    }, 'forget', {
      currentShareInstance: 's1', hasActiveDownload: false, hasPendingAction: execution.pendingRetained !== null,
      connectionUnavailable: false, shareEnded: false,
    })
    expect(admission).toEqual({ allowed: false, reason: 'busy' })
    execution.releaseLocalIfOwned(token!)
    expect(execution.pendingRetained).toBeNull()

    // Live safeRenamed push through ReceiveSessionRunner even when zero progress callbacks occur.
    vi.useFakeTimers()
    try {
      const workspace = new WorkspaceTask()
      const publishedCounts: Array<number | null> = []
      const runner = new ReceiveSessionRunner({
        windowPort: {},
        execution,
        workspace,
        isDisposed: () => false,
        publish: () => publishedCounts.push(workspace.safeRenamed?.count ?? null),
        onSettledRecord: () => undefined,
        onSessionFinished: () => undefined,
        reportError: () => undefined,
        dispatchAsyncError: () => undefined,
      })

      let emitSafe!: (summary: SafeRenamedSummary) => void
      let resolveJob!: (result: SettledTransferJobResult) => void
      const unsubscribed = vi.fn()
      const initialSummary: SafeRenamedSummary = { count: 1, sample: [{ logicalPath: ['root.cfg'], physicalName: 'root-cfg.windshare-11111111' }] }
      const updatedSummary: SafeRenamedSummary = {
        count: 2,
        sample: [...initialSummary.sample, { logicalPath: ['a.dll'], physicalName: 'a-dll.windshare-22222222' }],
      }
      const session: OutputSessionHandle = {
        taskId: 'live-runner',
        backend: 'fsa',
        transferOutput: {
          sessionId: 'live-runner',
          capabilities: { maxConcurrentFiles: 4, resumable: true },
          openFile: async () => { throw new Error('unused') },
          safeRenamedSummary: () => initialSummary,
        },
        subscribeSafeRenamed: listener => {
          emitSafe = listener
          return unsubscribed
        },
        settle: async () => ({
          taskId: 'live-runner', shareInstance: 's1', selectionKey: 'k1',
          selection: { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] },
          artifact: { kind: 'directory-tree', name: 'Root' }, backend: 'fsa',
          lifecycle: transitionReceiveState(initialReceiveState('live-runner'), { kind: 'saved', savedFiles: 1n, savedBytes: 4n }),
          target: { kind: 'fsa', parentHandle: { name: 'Downloads' } as FileSystemDirectoryHandle, reservedName: 'Root', collisionIndex: 0, singleFile: false, reservedAtMs: 0 },
          safeRenamed: updatedSummary, discoveryComplete: true, updatedAt: Date.now(),
        }),
        close: async () => undefined,
      }
      const joined = {
        descriptor: { shareInstanceId: 's1' },
        beginDownloadConnectivity: () => ({ close: () => undefined }),
        createTransferJob: () => ({ run: () => new Promise<SettledTransferJobResult>(r => { resolveJob = r }), pause: () => undefined, stop: () => undefined, detach: () => undefined }),
        observeConnection: () => () => undefined,
        subscribeConnection: () => () => undefined,
      } as unknown as JoinedBrowserShare

      const launchToken = execution.claimLaunch('opening', 'fsa:tree', 'Root')!
      const running = runner.run({
        transferToken: launchToken,
        joined,
        session,
        selection: {} as FrozenSelectionPolicy,
        activeSelectionKey: 'k1',
        rootPath: [],
        singleFileId: null,
        display: null,
        initialLifecycle: initialReceiveState('live-runner'),
      })
      // Initial publish synchronously reflects root/seeded summary = 1.
      expect(publishedCounts).toContain(1)
      emitSafe(updatedSummary)
      await vi.advanceTimersByTimeAsync(120)
      expect(publishedCounts).toContain(2)

      resolveJob(settled('completed', { completedFiles: 1n, completedBytes: 4n }))
      await running
      expect(unsubscribed).toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })
})
