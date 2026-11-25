import { byteRange } from '../../src/content/geometry'
import {
  browserOPFSEnvironment,
  deleteOPFSTask,
  exportPartialOPFSZip,
  openNewOPFSSession,
  reopenOPFSSession,
  saveRetainedOPFSTask,
} from '../../src/output/backends/opfs'
import { openPortableSession } from '../../src/output/backends/portable'
import { ReceiveStore } from '../../src/output/store/database'
import { acquireTaskLock, isTaskLockedElsewhere, type TaskLock } from '../../src/output/store/tab-lock'
import { fileRecordId } from '../../src/output/store/types'
import { initialReceiveState } from '../../src/transfer/state'

const OPFS_PAYLOAD_A = new TextEncoder().encode('first-file-payload')
const OPFS_PAYLOAD_B = new TextEncoder().encode('second-file-payload!')
const OPFS_TASKS_DIR = 'windshare-tasks'

let heldLock: TaskLock | undefined

export async function verifyUnifiedStore(databaseName: string) {
  const store = await ReceiveStore.open(databaseName)
  try {
    const taskId = `store-${crypto.randomUUID()}`
    await store.putTask({
      taskId,
      shareInstance: 'share-1',
      selectionKey: 'sel-1',
      selection: { shareInstance: 'share-1', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      artifact: { kind: 'original-file', name: 'hello.txt' },
      backend: 'opfs',
      lifecycle: initialReceiveState(taskId),
      target: { kind: 'opfs', objectName: `${taskId}.part`, fileName: 'hello.txt', isZip: false, allocatedLength: 0n },
      updatedAt: Date.now(),
    })
    await store.putFile({
      id: fileRecordId(taskId, 'f1'),
      taskId,
      fileId: 'f1',
      revision: 'r1',
      path: ['hello.txt'],
      size: 5n,
      completedRanges: [[0n, 5n]],
      status: 'completed',
    })

    const tasksBefore = await store.listTasks()
    const filesBefore = await store.listTaskFiles(taskId)
    await store.deleteTask(taskId)
    const tasksAfter = await store.listTasks()
    const filesAfter = await store.listTaskFiles(taskId)
    return {
      tasksBefore: tasksBefore.length,
      filesBefore: filesBefore.length,
      tasksAfter: tasksAfter.length,
      filesAfter: filesAfter.length,
    }
  } finally {
    store.close()
    await deleteDatabase(databaseName)
  }
}

export async function holdCrossTabLock(taskId: string): Promise<boolean> {
  heldLock = await acquireTaskLock(taskId)
  return true
}

export async function releaseCrossTabLock(): Promise<void> {
  await heldLock?.release()
  heldLock = undefined
}

export async function checkCrossTabLock(taskId: string): Promise<{ lockedElsewhere: boolean; acquireError: string | null }> {
  const lockedElsewhere = await isTaskLockedElsewhere(taskId)
  let acquireError: string | null = null
  try {
    const lock = await acquireTaskLock(taskId)
    await lock.release()
  } catch (error) {
    acquireError = error instanceof Error ? error.name : String(error)
  }
  return { lockedElsewhere, acquireError }
}

export async function preparePausedOPFSZip(databaseName: string, taskId: string): Promise<void> {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  try {
    const session = await openNewOPFSSession({
      task: {
        taskId,
        selection: { shareInstance: 'share-opfs', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-opfs',
        artifact: { kind: 'zip-archive', name: 'bundle.zip' },
        lifecycle: initialReceiveState(taskId),
      },
      store,
      env,
      windowPort: window,
    })
    try {
      await session.transferOutput.ensureDirectory?.({ directoryId: 'd1', path: ['folder'] }, new AbortController().signal)
      const f1 = await session.transferOutput.openFile({
        fileId: 'f1', path: ['folder', 'a.txt'], sourcePath: ['folder', 'a.txt'], revision: 'r1',
        exactSize: BigInt(OPFS_PAYLOAD_A.byteLength),
      }, new AbortController().signal)
      await f1.writeAt(0n, OPFS_PAYLOAD_A)
      await f1.commit([byteRange(0n, BigInt(OPFS_PAYLOAD_A.byteLength))])

      const f2 = await session.transferOutput.openFile({
        fileId: 'f2', path: ['folder', 'b.txt'], sourcePath: ['folder', 'b.txt'], revision: 'r2',
        exactSize: BigInt(OPFS_PAYLOAD_B.byteLength),
      }, new AbortController().signal)
      await f2.writeAt(0n, OPFS_PAYLOAD_B.subarray(0, 6))
      await f2.pause([byteRange(0n, 6n)])

      await session.settle({
        status: 'paused',
        pauseReason: 'user',
        completedFiles: 1n,
        completedBytes: BigInt(OPFS_PAYLOAD_A.byteLength),
        discoveredFiles: 2n,
        discoveredBytes: BigInt(OPFS_PAYLOAD_A.byteLength + OPFS_PAYLOAD_B.byteLength),
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      })
    } finally {
      await session.close()
    }
  } finally {
    store.close()
  }
}

export async function exportPausedOPFSZip(databaseName: string, taskId: string): Promise<void> {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  try {
    const task = await store.getTask(taskId)
    if (task === undefined) throw new Error('missing paused OPFS task')
    await exportPartialOPFSZip(task, store, env, window)
  } finally {
    store.close()
  }
}

export async function resumeAndCompleteOPFSZip(databaseName: string, taskId: string) {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  try {
    const paused = await store.getTask(taskId)
    if (paused === undefined) throw new Error('missing paused OPFS task')
    const reopened = await reopenOPFSSession({
      task: paused,
      store,
      env,
      windowPort: window,
    })
    try {
      const f1Completed = await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)
      const f2Again = await reopened.transferOutput.openFile({
        fileId: 'f2', path: ['folder', 'b.txt'], sourcePath: ['folder', 'b.txt'], revision: 'r2',
        exactSize: BigInt(OPFS_PAYLOAD_B.byteLength),
      }, new AbortController().signal)
      await f2Again.writeAt(6n, OPFS_PAYLOAD_B.subarray(6))
      await f2Again.commit([byteRange(0n, BigInt(OPFS_PAYLOAD_B.byteLength))])

      const finished = await reopened.settle({
        status: 'completed',
        completedFiles: 2n,
        completedBytes: BigInt(OPFS_PAYLOAD_A.byteLength + OPFS_PAYLOAD_B.byteLength),
        discoveredFiles: 2n,
        discoveredBytes: BigInt(OPFS_PAYLOAD_A.byteLength + OPFS_PAYLOAD_B.byteLength),
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      })
      return {
        reopenedF1AlreadyCompleted: f1Completed?.size === BigInt(OPFS_PAYLOAD_A.byteLength),
        reopenedF2StartEnd: f2Again.completedRanges.map(r => `${r.start}:${r.end}`),
        finishedKind: finished.lifecycle.kind,
      }
    } finally {
      await reopened.close()
    }
  } finally {
    store.close()
  }
}

export async function saveRetainedOPFSZip(databaseName: string, taskId: string): Promise<string> {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  try {
    const task = await store.getTask(taskId)
    if (task === undefined) throw new Error('missing completed OPFS task')
    const savedAgain = await saveRetainedOPFSTask(task, store, env, window)
    return savedAgain.lifecycle.kind
  } finally {
    store.close()
  }
}

export async function deleteOPFSZipTask(databaseName: string, taskId: string) {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  try {
    const task = await store.getTask(taskId)
    if (task === undefined || task.target.kind !== 'opfs') throw new Error('missing OPFS task to delete')
    const objectName = task.target.objectName
    await deleteOPFSTask(task, store, env)
    const remaining = await store.getTask(taskId)
    const root = await navigator.storage.getDirectory()
    const tasksDir = await root.getDirectoryHandle(OPFS_TASKS_DIR)
    const objectRemoved = await tasksDir.getFileHandle(objectName).then(
      () => false,
      (error: unknown) => error instanceof DOMException && error.name === 'NotFoundError',
    )
    return {
      taskRemoved: remaining === undefined,
      objectRemoved,
    }
  } finally {
    store.close()
  }
}

export async function cleanupOPFSZipTest(databaseName: string, taskId: string): Promise<void> {
  try {
    const root = await navigator.storage.getDirectory()
    const tasksDir = await root.getDirectoryHandle(OPFS_TASKS_DIR)
    await tasksDir.removeEntry(`${taskId}.part`)
  } catch { /* Object was already deleted or never created. */ }
  await deleteDatabase(databaseName)
}

export async function verifyPortableStreamingZip(): Promise<string> {
  const taskId = `portable-${crypto.randomUUID()}`
  const session = openPortableSession({
    task: {
      taskId,
      selection: { shareInstance: 'share-p', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-p',
      artifact: { kind: 'zip-archive', name: 'quick.zip' },
      lifecycle: initialReceiveState(taskId),
    },
    windowPort: window,
  })
  try {
    const file = await session.transferOutput.openFile({
      fileId: 'f1', path: ['note.txt'], sourcePath: ['note.txt'], revision: 'r1', exactSize: 5n,
    }, new AbortController().signal)
    await file.writeAt(0n, new TextEncoder().encode('hello'))
    await file.commit([byteRange(0n, 5n)])
    const settled = await session.settle({
      status: 'completed',
      completedFiles: 1n,
      completedBytes: 5n,
      discoveredFiles: 1n,
      discoveredBytes: 5n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    return settled.lifecycle.kind
  } finally {
    await session.close()
  }
}

export function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
    request.onblocked = () => resolve()
  })
}
