import { byteRange } from '../../src/content/geometry'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { ReceiveStore } from '../../src/output/store/database'
import { initialReceiveState } from '../../src/transfer/state'
import { deleteDatabase } from './receive-storage-harness'

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
    const survivingNames = await listDirectoryKeys(reservedDir)

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

export async function verifyFSAConcurrentSingleFileAllocationOverNativeHandles(databaseName: string) {
  const store = await ReceiveStore.open(databaseName)
  const root = await navigator.storage.getDirectory()
  const sandboxName = `fsa-concurrent-${crypto.randomUUID()}`
  const parent = await root.getDirectoryHandle(sandboxName, { create: true })
  const taskA = `fsa-a-${crypto.randomUUID()}`
  const taskB = `fsa-b-${crypto.randomUUID()}`
  const payloadA = new TextEncoder().encode('report-from-task-a')
  const payloadB = new TextEncoder().encode('report-from-task-b-longer')

  try {
    const [sessionA, sessionB] = await Promise.all([
      openNewFSASession({
        task: {
          taskId: taskA,
          selection: { shareInstance: 'share-a', syntheticRoot: 'root', defaultSelected: true, rules: [] },
          selectionKey: 'sel-a',
          artifact: { kind: 'directory-tree', name: 'report.bin', singleFileId: 'f-a' },
          lifecycle: initialReceiveState(taskA),
        },
        parentHandle: parent,
        store,
      }),
      openNewFSASession({
        task: {
          taskId: taskB,
          selection: { shareInstance: 'share-b', syntheticRoot: 'root', defaultSelected: true, rules: [] },
          selectionKey: 'sel-b',
          artifact: { kind: 'directory-tree', name: 'report.bin', singleFileId: 'f-b' },
          lifecycle: initialReceiveState(taskB),
        },
        parentHandle: parent,
        store,
      }),
    ])

    const [writerA, writerB] = await Promise.all([
      sessionA.transferOutput.openFile({
        fileId: 'f-a', path: ['report.bin'], sourcePath: ['report.bin'], revision: 'r-a', exactSize: BigInt(payloadA.byteLength),
      }, new AbortController().signal),
      sessionB.transferOutput.openFile({
        fileId: 'f-b', path: ['report.bin'], sourcePath: ['report.bin'], revision: 'r-b', exactSize: BigInt(payloadB.byteLength),
      }, new AbortController().signal),
    ])

    await Promise.all([
      writerA.writeAt(0n, payloadA).then(() => writerA.commit([byteRange(0n, BigInt(payloadA.byteLength))])),
      writerB.writeAt(0n, payloadB).then(() => writerB.commit([byteRange(0n, BigInt(payloadB.byteLength))])),
    ])

    const [settledA, settledB] = await Promise.all([
      sessionA.settle({
        status: 'completed', completedFiles: 1n, completedBytes: BigInt(payloadA.byteLength),
        discoveredFiles: 1n, discoveredBytes: BigInt(payloadA.byteLength), discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
      }),
      sessionB.settle({
        status: 'completed', completedFiles: 1n, completedBytes: BigInt(payloadB.byteLength),
        discoveredFiles: 1n, discoveredBytes: BigInt(payloadB.byteLength), discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
      }),
    ])
    await Promise.all([sessionA.close(), sessionB.close()])

    const names = await listDirectoryKeys(parent)
    const contents = await Promise.all(names.map(async name =>
      await (await (await parent.getFileHandle(name)).getFile()).text()))

    return {
      lifecycleKinds: [settledA.lifecycle.kind, settledB.lifecycle.kind],
      names,
      contents: contents.sort(),
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
