import { byteRange } from '../../src/content/geometry'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
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
      discoveryComplete: false,
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
    await store.putZipSpoolChunk({
      id: `${taskId}:0`,
      taskId,
      index: 0,
      bytes: new Uint8Array([1, 2, 3]),
    })

    const tasksBefore = await store.listTasks()
    const filesBefore = await store.listTaskFiles(taskId)
    const chunkBefore = await store.getZipSpoolChunk(taskId, 0)
    await store.deleteTask(taskId)
    const tasksAfter = await store.listTasks()
    const filesAfter = await store.listTaskFiles(taskId)
    const chunkAfter = await store.getZipSpoolChunk(taskId, 0)
    return {
      tasksBefore: tasksBefore.length,
      filesBefore: filesBefore.length,
      chunksBefore: chunkBefore === undefined ? 0 : 1,
      tasksAfter: tasksAfter.length,
      filesAfter: filesAfter.length,
      chunksAfter: chunkAfter === undefined ? 0 : 1,
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

export async function verifyOPFSProgressiveZipLifecycle(databaseName: string) {
  const store = await ReceiveStore.open(databaseName)
  const env = browserOPFSEnvironment()
  const taskId = `opfs-${crypto.randomUUID()}`
  const downloads: Array<{ name: string; blob: Blob }> = []
  const captureWindow = createCaptureDownloadWindow(downloads)
  const payloadA = new TextEncoder().encode('first-file-payload')
  const payloadB = new TextEncoder().encode('second-file-payload!')

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
      windowPort: captureWindow,
    })

    await session.transferOutput.ensureDirectory?.({ directoryId: 'd1', path: ['folder'] }, new AbortController().signal)
    const f1 = await session.transferOutput.openFile({
      fileId: 'f1', path: ['folder', 'a.txt'], sourcePath: ['folder', 'a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    }, new AbortController().signal)
    await f1.writeAt(0n, payloadA)
    await f1.commit([byteRange(0n, BigInt(payloadA.byteLength))])

    const f2 = await session.transferOutput.openFile({
      fileId: 'f2', path: ['folder', 'b.txt'], sourcePath: ['folder', 'b.txt'], revision: 'r2', exactSize: BigInt(payloadB.byteLength),
    }, new AbortController().signal)
    await f2.writeAt(0n, payloadB.subarray(0, 6))
    await f2.pause([byteRange(0n, 6n)])

    const paused = await session.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 1n,
      completedBytes: BigInt(payloadA.byteLength),
      discoveredFiles: 2n,
      discoveredBytes: BigInt(payloadA.byteLength + payloadB.byteLength),
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await session.close()

    const partialBlob = await exportPartialOPFSZip(paused, store, env, captureWindow)
    const partialBytes = [...new Uint8Array(await partialBlob.arrayBuffer())]

    const reopened = await reopenOPFSSession({
      task: paused,
      store,
      env,
      windowPort: captureWindow,
    })
    const f1Completed = await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)
    const f2Again = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['folder', 'b.txt'], sourcePath: ['folder', 'b.txt'], revision: 'r2', exactSize: BigInt(payloadB.byteLength),
    }, new AbortController().signal)
    await f2Again.writeAt(6n, payloadB.subarray(6))
    await f2Again.commit([byteRange(0n, BigInt(payloadB.byteLength))])

    const finished = await reopened.settle({
      status: 'completed',
      completedFiles: 2n,
      completedBytes: BigInt(payloadA.byteLength + payloadB.byteLength),
      discoveredFiles: 2n,
      discoveredBytes: BigInt(payloadA.byteLength + payloadB.byteLength),
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await reopened.close()

    // Export retained artifact a second time (Save again) and read the OPFS File before deleting it.
    const savedAgain = await saveRetainedOPFSTask(finished, store, env, captureWindow)
    const lastDownload = downloads.at(-1)
    const finalArchiveBytes = lastDownload === undefined
      ? []
      : [...new Uint8Array(await lastDownload.blob.arrayBuffer())]
    await deleteOPFSTask(savedAgain, store, env)
    const remaining = await store.getTask(taskId)

    return {
      reopenedF1AlreadyCompleted: f1Completed?.size === BigInt(payloadA.byteLength),
      reopenedF2StartEnd: f2Again.completedRanges.map(r => `${r.start}:${r.end}`),
      finishedKind: finished.lifecycle.kind,
      savedAgainKind: savedAgain.lifecycle.kind,
      downloadCount: downloads.length,
      finalArchiveName: lastDownload?.name ?? null,
      finalArchiveBytes,
      partialBytes,
      deletedCleanly: remaining === undefined,
    }
  } finally {
    store.close()
    await deleteDatabase(databaseName)
  }
}

export async function verifyFSABackendOverNativeHandles(databaseName: string) {
  const store = await ReceiveStore.open(databaseName)
  const root = await navigator.storage.getDirectory()
  const sandboxName = `fsa-sandbox-${crypto.randomUUID()}`
  const parent = await root.getDirectoryHandle(sandboxName, { create: true })
  const taskId = `fsa-${crypto.randomUUID()}`

  try {
    await parent.getDirectoryHandle('Album', { create: true })
    const session = await openNewFSASession({
      task: {
        taskId,
        selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-fsa',
        artifact: { kind: 'directory-tree', name: 'Album' },
        lifecycle: initialReceiveState(taskId),
      },
      parentHandle: parent,
      store,
    })

    const f1 = await session.transferOutput.openFile({
      fileId: 'f1', path: ['pic.bin'], sourcePath: ['Album', 'pic.bin'], revision: 'r1', exactSize: 4n,
    }, new AbortController().signal)
    await f1.writeAt(0n, new Uint8Array([11, 22, 33, 44]))
    await f1.commit([byteRange(0n, 4n)])

    const f2 = await session.transferOutput.openFile({
      fileId: 'f2', path: ['clip.bin'], sourcePath: ['Album', 'clip.bin'], revision: 'r2', exactSize: 8n,
    }, new AbortController().signal)
    await f2.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await f2.pause([byteRange(0n, 4n)])

    const paused = await session.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 1n,
      completedBytes: 4n,
      discoveredFiles: 2n,
      discoveredBytes: 12n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await session.close()

    const reopened = await reopenFSASession({ task: paused, store, allowPermissionPrompt: false })
    const resumedF1 = await reopened.transferOutput.checkCompleted?.('f1', new AbortController().signal)
    const resumedF2 = await reopened.transferOutput.openFile({
      fileId: 'f2', path: ['clip.bin'], sourcePath: ['Album', 'clip.bin'], revision: 'r2', exactSize: 8n,
    }, new AbortController().signal)
    await reopened.close()

    await discardFSATask(paused, store)
    const reservedDir = await parent.getDirectoryHandle('Album (1)')
    const survivingNames: string[] = []
    for await (const name of (reservedDir as FileSystemDirectoryHandle & { keys(): AsyncIterableIterator<string> }).keys()) {
      survivingNames.push(name)
    }

    return {
      resumedF1AlreadyCompleted: resumedF1?.size === 4n,
      resumedF2Ranges: resumedF2.completedRanges.map(r => `${r.start}:${r.end}`),
      survivingNames,
      taskDeleted: (await store.getTask(taskId)) === undefined,
    }
  } finally {
    store.close()
    await root.removeEntry(sandboxName, { recursive: true }).catch(() => undefined)
    await deleteDatabase(databaseName)
  }
}

export async function startUncommittedFSAFirstWrite(databaseName: string, sandboxName: string, taskId: string) {
  const store = await ReceiveStore.open(databaseName)
  const root = await navigator.storage.getDirectory()
  const parent = await root.getDirectoryHandle(sandboxName, { create: true })
  const session = await openNewFSASession({
    task: {
      taskId,
      selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-placeholder',
      artifact: { kind: 'directory-tree', name: 'Drafts' },
      lifecycle: initialReceiveState(taskId),
    },
    parentHandle: parent,
    store,
  })
  const file = await session.transferOutput.openFile({
    fileId: 'f1', path: ['report.bin'], sourcePath: ['Drafts', 'report.bin'], revision: 'r1', exactSize: 8n,
  }, new AbortController().signal)
  await file.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
  // Intentionally leave the session, writable stream, and Web Lock open for the page close to sever.
  return { taskId }
}

export async function resumeFSAPlaceholderAfterPageClose(databaseName: string, sandboxName: string, taskId: string) {
  const store = await ReceiveStore.open(databaseName)
  const root = await navigator.storage.getDirectory()
  try {
    const parent = await root.getDirectoryHandle(sandboxName)
    const dir = await parent.getDirectoryHandle('Drafts')
    const beforeSize = (await (await dir.getFileHandle('report.bin')).getFile()).size
    const beforeRecord = await store.getFile(taskId, 'f1')
    const task = await store.getTask(taskId)
    if (task === undefined) throw new Error('missing persisted task')
    const reopened = await reopenFSASession({ task, store, allowPermissionPrompt: false })
    try {
      const resumed = await reopened.transferOutput.openFile({
        fileId: 'f1', path: ['report.bin'], sourcePath: ['Drafts', 'report.bin'], revision: 'r1', exactSize: 8n,
      }, new AbortController().signal)
      const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])
      await resumed.writeAt(0n, payload)
      await resumed.commit([byteRange(0n, 8n)])
    } finally {
      await reopened.close()
    }
    const finalBytes = [...new Uint8Array(await (await (await dir.getFileHandle('report.bin')).getFile()).arrayBuffer())]
    return {
      beforeSize,
      hadRecordBeforeResume: beforeRecord !== undefined,
      finalBytes,
      completedAfterResume: (await store.getFile(taskId, 'f1'))?.status === 'completed',
    }
  } finally {
    store.close()
    await root.removeEntry(sandboxName, { recursive: true }).catch(() => undefined)
    await deleteDatabase(databaseName)
  }
}

export async function verifyFSADiscardSafetyOverNativeHandles(databaseName: string) {
  const store = await ReceiveStore.open(databaseName)
  const root = await navigator.storage.getDirectory()
  const sandboxName = `fsa-discard-safety-${crypto.randomUUID()}`
  const parent = await root.getDirectoryHandle(sandboxName, { create: true })

  try {
    // Case 1: 0 completed files + foreign user file placed in result directory -> preserve user file & directory.
    const foreignTaskId = `fsa-foreign-${crypto.randomUUID()}`
    const foreignSession = await openNewFSASession({
      task: {
        taskId: foreignTaskId,
        selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-foreign',
        artifact: { kind: 'directory-tree', name: 'Trip' },
        lifecycle: initialReceiveState(foreignTaskId),
      },
      parentHandle: parent,
      store,
    })
    const draft = await foreignSession.transferOutput.openFile({
      fileId: 'f1', path: ['sub', 'draft.bin'], sourcePath: ['Trip', 'sub', 'draft.bin'], revision: 'r1', exactSize: 16n,
    }, new AbortController().signal)
    await draft.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await draft.pause([byteRange(0n, 4n)])
    const foreignPaused = await foreignSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 16n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await foreignSession.close()

    const tripDir = await parent.getDirectoryHandle('Trip')
    const userHandle = await tripDir.getFileHandle('user-note.txt', { create: true })
    const userWriter = await userHandle.createWritable()
    await userWriter.write('do-not-delete')
    await userWriter.close()

    await discardFSATask(foreignPaused, store)
    const tripNames = await listDirectoryKeys(tripDir)
    const preservedText = await (await (await tripDir.getFileHandle('user-note.txt')).getFile()).text()

    // Case 2: 0 completed files + no foreign files -> unfinished file, empty subdir, and empty root removed.
    const cleanTaskId = `fsa-clean-${crypto.randomUUID()}`
    const cleanSession = await openNewFSASession({
      task: {
        taskId: cleanTaskId,
        selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-clean',
        artifact: { kind: 'directory-tree', name: 'EmptyAfterDiscard' },
        lifecycle: initialReceiveState(cleanTaskId),
      },
      parentHandle: parent,
      store,
    })
    const part = await cleanSession.transferOutput.openFile({
      fileId: 'f1', path: ['nested', 'part.bin'], sourcePath: ['EmptyAfterDiscard', 'nested', 'part.bin'], revision: 'r1', exactSize: 16n,
    }, new AbortController().signal)
    await part.writeAt(0n, new Uint8Array([9, 8, 7, 6]))
    await part.pause([byteRange(0n, 4n)])
    const cleanPaused = await cleanSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 16n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await cleanSession.close()

    await discardFSATask(cleanPaused, store)
    const cleanRootRemoved = await parent.getDirectoryHandle('EmptyAfterDiscard').then(() => false, () => true)

    // Case 3: Unfinished file externally overwritten at the same path -> preserved on discard.
    const replacedTaskId = `fsa-replaced-${crypto.randomUUID()}`
    const replacedSession = await openNewFSASession({
      task: {
        taskId: replacedTaskId,
        selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-replaced',
        artifact: { kind: 'directory-tree', name: 'Replaced' },
        lifecycle: initialReceiveState(replacedTaskId),
      },
      parentHandle: parent,
      store,
    })
    const clip = await replacedSession.transferOutput.openFile({
      fileId: 'f1', path: ['clip.bin'], sourcePath: ['Replaced', 'clip.bin'], revision: 'r1', exactSize: 16n,
    }, new AbortController().signal)
    await clip.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await clip.pause([byteRange(0n, 4n)])
    const replacedPaused = await replacedSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 16n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await replacedSession.close()

    const replacedDir = await parent.getDirectoryHandle('Replaced')
    const replacedHandle = await replacedDir.getFileHandle('clip.bin')
    const replaceWriter = await replacedHandle.createWritable()
    await replaceWriter.write('user-owned-replacement')
    await replaceWriter.close()

    await discardFSATask(replacedPaused, store)
    const replacedText = await (await (await replacedDir.getFileHandle('clip.bin')).getFile()).text()

    // Case 4: Reserved directory removed and replaced by a same-name regular file -> user file preserved.
    const dirToFileTaskId = `fsa-dir-to-file-${crypto.randomUUID()}`
    const dirToFileSession = await openNewFSASession({
      task: {
        taskId: dirToFileTaskId,
        selection: { shareInstance: 'share-fsa', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'sel-dir-to-file',
        artifact: { kind: 'directory-tree', name: 'Backup' },
        lifecycle: initialReceiveState(dirToFileTaskId),
      },
      parentHandle: parent,
      store,
    })
    const dirToFilePaused = await dirToFileSession.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 0n, completedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n,
      discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await dirToFileSession.close()

    await parent.removeEntry('Backup')
    const userBackupFile = await parent.getFileHandle('Backup', { create: true })
    const backupWriter = await userBackupFile.createWritable()
    await backupWriter.write('user-backup-file')
    await backupWriter.close()

    await discardFSATask(dirToFilePaused, store)
    const dirToFileText = await (await (await parent.getFileHandle('Backup')).getFile()).text()

    return {
      tripNames,
      preservedText,
      cleanRootRemoved,
      replacedText,
      dirToFileText,
      allTasksDeleted: (await store.listTasks()).length === 0,
    }
  } finally {
    store.close()
    await root.removeEntry(sandboxName, { recursive: true }).catch(() => undefined)
    await deleteDatabase(databaseName)
  }
}

async function listDirectoryKeys(dir: FileSystemDirectoryHandle): Promise<string[]> {
  const names: string[] = []
  for await (const name of (dir as FileSystemDirectoryHandle & { keys(): AsyncIterableIterator<string> }).keys()) {
    names.push(name)
  }
  return names.sort()
}

export async function verifyPortableStreamingZip() {
  const downloads: Array<{ name: string; blob: Blob }> = []
  const captureWindow = createCaptureDownloadWindow(downloads)
  const taskId = `portable-${crypto.randomUUID()}`
  const session = openPortableSession({
    task: {
      taskId,
      selection: { shareInstance: 'share-p', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      selectionKey: 'sel-p',
      artifact: { kind: 'zip-archive', name: 'quick.zip' },
      lifecycle: initialReceiveState(taskId),
    },
    windowPort: captureWindow,
  })
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
  await session.close()
  const firstDownload = downloads[0]
  return {
    lifecycleKind: settled.lifecycle.kind,
    downloadName: firstDownload?.name ?? null,
    archiveBytes: firstDownload === undefined ? [] : [...new Uint8Array(await firstDownload.blob.arrayBuffer())],
  }
}

function createCaptureDownloadWindow(
  downloads: Array<{ name: string; blob: Blob }>,
): BrowserOutputWindow {
  const blobs = new Map<string, Blob>()
  let counter = 0
  return {
    URL: {
      createObjectURL: (blob: Blob) => {
        const url = `blob:capture-${++counter}`
        blobs.set(url, blob)
        return url
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => {
        const anchor = {
          href: '',
          download: '',
          rel: '',
          style: { display: '' },
          click: () => {
            const blob = blobs.get(anchor.href)
            if (blob !== undefined) downloads.push({ name: anchor.download, blob })
          },
          remove: () => undefined,
        }
        return anchor as unknown as HTMLAnchorElement
      },
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Document,
  }
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
    request.onblocked = () => resolve()
  })
}
