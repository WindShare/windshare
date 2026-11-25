import { admitRetainedAction, retainedTaskReadiness } from '../../src/application/receive/admission'
import { executeRetainedTaskAction, loadRetainedTasks } from '../../src/application/receive/retained'
import { byteRange } from '../../src/content/geometry'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { browserOPFSEnvironment, openNewOPFSSession, reopenOPFSSession } from '../../src/output/backends/opfs'
import { ReceiveStore } from '../../src/output/store/database'
import { initialReceiveState } from '../../src/transfer/state'

const ENCODER = new TextEncoder()
const OPFS_DONE_PAYLOAD = ENCODER.encode('done-before-process-exit')
const OPFS_PART_PREFIX = ENCODER.encode('first-part')
const OPFS_PART_SUFFIX = ENCODER.encode('|second-part')
const OPFS_PART_UNCOMMITTED = ENCODER.encode('xxxx')
const OPFS_FRESH_PAYLOAD = ENCODER.encode('fresh-after-process-exit')
const OPFS_SINGLE_PAYLOAD = ENCODER.encode('single-file-committed-before-process-exit')

const FSA_DONE_BYTES = new Uint8Array([10, 20, 30, 40])
const FSA_PART_PREFIX = new Uint8Array([1, 2, 3, 4])
const FSA_PART_SUFFIX = new Uint8Array([5, 6, 7, 8])
const FSA_PART_UNCOMMITTED = new Uint8Array([9, 9])
const FSA_DRAFT_BYTES = new Uint8Array([11, 12, 13, 14, 15, 16])

export interface ProcessCrashIds {
  readonly databaseName: string
  readonly opfsTaskId: string
  readonly opfsSingleTaskId: string
  readonly fsaTaskId: string
  readonly fsaSandbox: string
}

export async function startActiveProcessCrashCut(ids: ProcessCrashIds) {
  const store = await ReceiveStore.open(ids.databaseName)
  const opfsEnv = browserOPFSEnvironment()
  const root = await navigator.storage.getDirectory()
  const parent = await root.getDirectoryHandle(ids.fsaSandbox, { create: true })

  // 1. Active OPFS progressive ZIP with committed, checkpointed+dirty, and uncommitted members.
  const opfsSession = await openNewOPFSSession({
    task: {
      taskId: ids.opfsTaskId,
      selection: { shareInstance: 'share-proc-opfs', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-proc-opfs',
      artifact: { kind: 'zip-archive', name: 'bundle.zip' },
      lifecycle: initialReceiveState(ids.opfsTaskId),
    },
    store,
    env: opfsEnv,
    windowPort: window,
  })
  await opfsSession.transferOutput.ensureDirectory?.(
    { directoryId: 'd1', path: ['docs'] },
    new AbortController().signal,
  )
  const opfsDone = await opfsSession.transferOutput.openFile({
    fileId: 'f1', path: ['docs', 'done.txt'], sourcePath: ['docs', 'done.txt'], revision: 'r1',
    exactSize: BigInt(OPFS_DONE_PAYLOAD.byteLength),
  }, new AbortController().signal)
  await opfsDone.writeAt(0n, OPFS_DONE_PAYLOAD)
  await opfsDone.commit([byteRange(0n, BigInt(OPFS_DONE_PAYLOAD.byteLength))])

  const opfsPart = await opfsSession.transferOutput.openFile({
    fileId: 'f2', path: ['docs', 'part.txt'], sourcePath: ['docs', 'part.txt'], revision: 'r2',
    exactSize: BigInt(OPFS_PART_PREFIX.byteLength + OPFS_PART_SUFFIX.byteLength),
  }, new AbortController().signal)
  await opfsPart.writeAt(0n, OPFS_PART_PREFIX)
  await opfsPart.checkpoint([byteRange(0n, BigInt(OPFS_PART_PREFIX.byteLength))], 'periodic')
  await opfsPart.writeAt(BigInt(OPFS_PART_PREFIX.byteLength), OPFS_PART_UNCOMMITTED)

  const opfsFresh = await opfsSession.transferOutput.openFile({
    fileId: 'f3', path: ['docs', 'uncommitted.txt'], sourcePath: ['docs', 'uncommitted.txt'], revision: 'r3',
    exactSize: BigInt(OPFS_FRESH_PAYLOAD.byteLength),
  }, new AbortController().signal)
  await opfsFresh.writeAt(0n, OPFS_PART_UNCOMMITTED)

  // 2. Active FSA directory tree with committed, checkpointed+dirty, and uncommitted 0-byte placeholder.
  const fsaSession = await openNewFSASession({
    task: {
      taskId: ids.fsaTaskId,
      selection: { shareInstance: 'share-proc-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-proc-fsa',
      artifact: { kind: 'directory-tree', name: 'Project' },
      lifecycle: initialReceiveState(ids.fsaTaskId),
    },
    parentHandle: parent,
    store,
  })
  const fsaDone = await fsaSession.transferOutput.openFile({
    fileId: 'f1', path: ['done.bin'], sourcePath: ['Project', 'done.bin'], revision: 'r1',
    exactSize: BigInt(FSA_DONE_BYTES.byteLength),
  }, new AbortController().signal)
  await fsaDone.writeAt(0n, FSA_DONE_BYTES)
  await fsaDone.commit([byteRange(0n, BigInt(FSA_DONE_BYTES.byteLength))])

  const fsaPart = await fsaSession.transferOutput.openFile({
    fileId: 'f2', path: ['part.bin'], sourcePath: ['Project', 'part.bin'], revision: 'r2',
    exactSize: BigInt(FSA_PART_PREFIX.byteLength + FSA_PART_SUFFIX.byteLength),
  }, new AbortController().signal)
  await fsaPart.writeAt(0n, FSA_PART_PREFIX)
  await fsaPart.checkpoint([byteRange(0n, BigInt(FSA_PART_PREFIX.byteLength))], 'disconnect')
  await fsaPart.writeAt(BigInt(FSA_PART_PREFIX.byteLength), FSA_PART_UNCOMMITTED)

  const fsaDraft = await fsaSession.transferOutput.openFile({
    fileId: 'f3', path: ['draft.bin'], sourcePath: ['Project', 'draft.bin'], revision: 'r3',
    exactSize: BigInt(FSA_DRAFT_BYTES.byteLength),
  }, new AbortController().signal)
  await fsaDraft.writeAt(0n, FSA_DRAFT_BYTES.subarray(0, 3))

  // 3. Committed single-file OPFS interrupted before settle()/publication runs.
  const opfsSingleSession = await openNewOPFSSession({
    task: {
      taskId: ids.opfsSingleTaskId,
      selection: { shareInstance: 'share-proc-single', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-proc-single',
      artifact: {
        kind: 'original-file',
        name: 'committed-single.txt',
        singleFileId: 'single-f1',
        singleFilePath: ['committed-single.txt'],
        singleFileSize: BigInt(OPFS_SINGLE_PAYLOAD.byteLength),
      },
      lifecycle: initialReceiveState(ids.opfsSingleTaskId),
    },
    store,
    env: opfsEnv,
    windowPort: window,
  })
  const opfsSingle = await opfsSingleSession.transferOutput.openFile({
    fileId: 'single-f1', path: ['committed-single.txt'], sourcePath: ['committed-single.txt'], revision: 'r-single',
    exactSize: BigInt(OPFS_SINGLE_PAYLOAD.byteLength),
  }, new AbortController().signal)
  await opfsSingle.writeAt(0n, OPFS_SINGLE_PAYLOAD)
  await opfsSingle.commit([byteRange(0n, BigInt(OPFS_SINGLE_PAYLOAD.byteLength))])

  const opfsRow = await store.getTask(ids.opfsTaskId)
  const opfsSingleRow = await store.getTask(ids.opfsSingleTaskId)
  const fsaRow = await store.getTask(ids.fsaTaskId)
  // Intentionally leave all sessions, SyncAccessHandle workers, writable streams, and Web Locks open
  // so closing the persistent BrowserContext severs the entire Chromium process abruptly.
  return {
    opfsLifecycle: opfsRow?.lifecycle.kind ?? null,
    opfsSingleLifecycle: opfsSingleRow?.lifecycle.kind ?? null,
    fsaLifecycle: fsaRow?.lifecycle.kind ?? null,
  }
}

export async function recoverAfterBrowserProcessRestart(ids: ProcessCrashIds) {
  const store = await ReceiveStore.open(ids.databaseName)
  const opfsEnv = browserOPFSEnvironment()
  try {
    const retainedEnv = { store, windowPort: window, opfsEnv }
    const retained = await loadRetainedTasks(retainedEnv)
    const opfsRetained = retained.find(task => task.taskId === ids.opfsTaskId)
    const opfsSingleRetained = retained.find(task => task.taskId === ids.opfsSingleTaskId)
    const fsaRetained = retained.find(task => task.taskId === ids.fsaTaskId)
    if (opfsRetained === undefined || opfsSingleRetained === undefined || fsaRetained === undefined) {
      throw new Error('Expected all abandoned tasks to be recovered by loadRetainedTasks')
    }

    const opfsSingleReadiness = retainedTaskReadiness(opfsSingleRetained, null)
    const opfsSingleOfflineSave = admitRetainedAction(opfsSingleRetained, 'save', {
      currentShareInstance: null,
      hasActiveDownload: false,
      hasPendingAction: false,
      connectionBlock: 'shareEnded',
    })
    const opfsSingleExported = await executeRetainedTaskAction(retainedEnv, ids.opfsSingleTaskId, 'save')
    const opfsSummary = await resumeAndCompleteOPFS(ids.opfsTaskId, store, opfsEnv)
    const fsaSummary = await resumeAndCompleteFSA(ids.fsaTaskId, ids.fsaSandbox, store)

    return {
      normalizedOpfs: summarizePausedSnapshot(opfsRetained),
      normalizedOpfsSingle: {
        activeElsewhere: opfsSingleRetained.activeElsewhere,
        kind: opfsSingleRetained.lifecycle.kind,
        actions: [...opfsSingleRetained.actions],
        readiness: opfsSingleReadiness,
        offlineSaveAllowed: opfsSingleOfflineSave.allowed,
        exportedLifecycle: opfsSingleExported.updated?.lifecycle.kind ?? null,
      },
      normalizedFsa: summarizePausedSnapshot(fsaRetained),
      opfs: opfsSummary,
      fsa: fsaSummary,
    }
  } finally {
    store.close()
  }
}

async function resumeAndCompleteOPFS(
  taskId: string,
  store: ReceiveStore,
  opfsEnv: ReturnType<typeof browserOPFSEnvironment>,
) {
  const paused = await store.getTask(taskId)
  if (paused === undefined) throw new Error('Missing normalized OPFS task')

  const reopened = await reopenOPFSSession({ task: paused, store, env: opfsEnv, windowPort: window })
  try {
    const f1Completed = await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)
    const f2Size = BigInt(OPFS_PART_PREFIX.byteLength + OPFS_PART_SUFFIX.byteLength)
    const f2 = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['docs', 'part.txt'], sourcePath: ['docs', 'part.txt'], revision: 'r2',
      exactSize: f2Size,
    }, new AbortController().signal)
    const f2Ranges = f2.completedRanges.map(range => `${range.start}:${range.end}`)
    await f2.writeAt(BigInt(OPFS_PART_PREFIX.byteLength), OPFS_PART_SUFFIX)
    await f2.commit([byteRange(0n, f2Size)])

    const f3Size = BigInt(OPFS_FRESH_PAYLOAD.byteLength)
    const f3 = await reopened.transferOutput.openFile({
      fileId: 'f3', path: ['docs', 'uncommitted.txt'], sourcePath: ['docs', 'uncommitted.txt'], revision: 'r3',
      exactSize: f3Size,
    }, new AbortController().signal)
    const f3Ranges = f3.completedRanges.map(range => `${range.start}:${range.end}`)
    await f3.writeAt(0n, OPFS_FRESH_PAYLOAD)
    await f3.commit([byteRange(0n, f3Size)])

    const totalBytes = BigInt(OPFS_DONE_PAYLOAD.byteLength) + f2Size + f3Size
    const settled = await reopened.settle({
      status: 'completed',
      completedFiles: 3n,
      completedBytes: totalBytes,
      discoveredFiles: 3n,
      discoveredBytes: totalBytes,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    return {
      f1CompletedSize: f1Completed === undefined ? null : Number(f1Completed.size),
      f2Ranges,
      f3Ranges,
      settledLifecycle: settled.lifecycle.kind,
    }
  } finally {
    await reopened.close()
  }
}

async function resumeAndCompleteFSA(taskId: string, sandboxName: string, store: ReceiveStore) {
  const paused = await store.getTask(taskId)
  if (paused === undefined) throw new Error('Missing normalized FSA task')

  const reopened = await reopenFSASession({ task: paused, store, allowPermissionPrompt: false })
  try {
    const f1Completed = await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)
    const f2Size = BigInt(FSA_PART_PREFIX.byteLength + FSA_PART_SUFFIX.byteLength)
    const f2 = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['part.bin'], sourcePath: ['Project', 'part.bin'], revision: 'r2',
      exactSize: f2Size,
    }, new AbortController().signal)
    const f2Ranges = f2.completedRanges.map(range => `${range.start}:${range.end}`)
    await f2.writeAt(BigInt(FSA_PART_PREFIX.byteLength), FSA_PART_SUFFIX)
    await f2.commit([byteRange(0n, f2Size)])

    const f3Size = BigInt(FSA_DRAFT_BYTES.byteLength)
    const f3 = await reopened.transferOutput.openFile({
      fileId: 'f3', path: ['draft.bin'], sourcePath: ['Project', 'draft.bin'], revision: 'r3',
      exactSize: f3Size,
    }, new AbortController().signal)
    const f3Ranges = f3.completedRanges.map(range => `${range.start}:${range.end}`)
    await f3.writeAt(0n, FSA_DRAFT_BYTES)
    await f3.commit([byteRange(0n, f3Size)])

    const totalBytes = BigInt(FSA_DONE_BYTES.byteLength) + f2Size + f3Size
    const settled = await reopened.settle({
      status: 'completed',
      completedFiles: 3n,
      completedBytes: totalBytes,
      discoveredFiles: 3n,
      discoveredBytes: totalBytes,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })

    const root = await navigator.storage.getDirectory()
    const project = await (await root.getDirectoryHandle(sandboxName)).getDirectoryHandle('Project')
    return {
      f1CompletedSize: f1Completed === undefined ? null : Number(f1Completed.size),
      f2Ranges,
      f3Ranges,
      settledLifecycle: settled.lifecycle.kind,
      doneBytes: await readHandleBytes(project, 'done.bin'),
      partBytes: await readHandleBytes(project, 'part.bin'),
      draftBytes: await readHandleBytes(project, 'draft.bin'),
    }
  } finally {
    await reopened.close()
  }
}

async function readHandleBytes(dir: FileSystemDirectoryHandle, name: string): Promise<number[]> {
  const file = await (await dir.getFileHandle(name)).getFile()
  return [...new Uint8Array(await file.arrayBuffer())]
}

function summarizePausedSnapshot(task: Awaited<ReturnType<typeof loadRetainedTasks>>[number]) {
  return {
    activeElsewhere: task.activeElsewhere,
    kind: task.lifecycle.kind,
    reason: task.lifecycle.kind === 'paused' ? task.lifecycle.reason : null,
    completedFiles: task.lifecycle.kind === 'paused' ? Number(task.lifecycle.completedFiles) : null,
    retainedBytes: task.lifecycle.kind === 'paused' ? Number(task.lifecycle.retainedBytes) : null,
  }
}
