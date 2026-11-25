import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import { discardFSATask, openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { retainsFSAReservationClaim } from '../../src/output/backends/fsa-reservation'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveTaskRecord } from '../../src/output/store/types'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState, transitionReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, FakeFileHandle, MemoryReceiveStore } from './fsa-fixture'

const pausedEmpty: SettledTransferJobResult = {
  status: 'paused',
  pauseReason: 'user',
  completedFiles: 0n,
  completedBytes: 0n,
  discoveredFiles: 0n,
  discoveredBytes: 0n,
  discoveryComplete: false,
  failedFiles: 0n,
  failedDirectories: 0n,
}

function folderTask(taskId: string, name = 'Photos') {
  return {
    taskId,
    selection: { shareInstance: `share-${taskId}`, syntheticRoot: 'root', defaultSelected: true, rules: [] },
    selectionKey: `sel-${taskId}`,
    artifact: { kind: 'directory-tree' as const, name },
    lifecycle: initialReceiveState(taskId),
  }
}

function singleFileTask(taskId: string, name: string) {
  return {
    taskId,
    selection: { shareInstance: `share-${taskId}`, syntheticRoot: 'root', defaultSelected: true, rules: [] },
    selectionKey: `sel-${taskId}`,
    artifact: { kind: 'directory-tree' as const, name, singleFileId: `file-${taskId}` },
    lifecycle: initialReceiveState(taskId),
  }
}

function reservedNameOf(record: ReceiveTaskRecord | undefined): string {
  return record?.target.kind === 'fsa' ? record.target.reservedName : ''
}

function makeBlockingLockManager(): LockManager {
  const queues = new Map<string, Promise<void>>()
  const held = new Set<string>()
  return {
    request: async (
      name: string,
      options: LockOptions,
      callback: (lock: Lock | null) => Promise<unknown>,
    ) => {
      if (options.ifAvailable && held.has(name)) return callback(null)
      const previous = queues.get(name) ?? Promise.resolve()
      let release!: () => void
      const current = new Promise<void>(resolve => { release = resolve })
      queues.set(name, current)
      await previous
      held.add(name)
      try {
        return await callback({ name, mode: options.mode ?? 'exclusive' })
      } finally {
        held.delete(name)
        release()
      }
    },
  } as unknown as LockManager
}

describe('FSA cross-task top-level reservation claims', () => {
  it('avoids colliding with a paused task whose directory was deleted externally so discard and resume stay isolated', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const traces: OutputTraceEvent[] = []

    const sessionA = await openNewFSASession({
      task: folderTask('task-a', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const pausedA = await sessionA.settle(pausedEmpty)
    await sessionA.close()

    // User manually deletes the empty reserved directory while task A remains paused in IndexedDB.
    await parent.removeEntry('Photos')
    expect(parent.children.has('Photos')).toBe(false)

    // Starting task B for the same name must skip the claimed slot rather than adopting "Photos".
    const sessionB = await openNewFSASession({
      task: folderTask('task-b', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      diagnostics: { backend: 'file_system_access', trace: { current: event => traces.push(event) } },
    })
    expect(reservedNameOf(sessionB.record)).toBe('Photos (1)')
    expect(parent.children.has('Photos')).toBe(false)
    expect(parent.children.has('Photos (1)')).toBe(true)
    expect(traces).toContainEqual({
      eventName: 'output_reservation',
      payload: {
        backend: 'file_system_access',
        task_id: 'task-b',
        transition: 'acquired',
        collision_index: 1,
        skipped_claimed_names: 1,
      },
    })

    // Discarding task A must not delete task B's newly created empty directory.
    await discardFSATask(pausedA, store as unknown as ReceiveStore)
    expect(parent.children.has('Photos (1)')).toBe(true)
    expect(await store.getTask('task-a')).toBeUndefined()

    // Task B can continue writing normally in its isolated directory.
    const fileB = await sessionB.transferOutput.openFile({
      fileId: 'b1', path: ['b.jpg'], sourcePath: ['Photos', 'b.jpg'], revision: 'rev-b', exactSize: 3n,
    }, new AbortController().signal)
    await fileB.writeAt(0n, Uint8Array.of(4, 5, 6))
    await fileB.commit([byteRange(0n, 3n)])
    await sessionB.close()

    const dirB = parent.children.get('Photos (1)') as FakeDirectoryHandle
    expect([...(dirB.children.get('b.jpg') as FakeFileHandle).bytes]).toEqual([4, 5, 6])
  })

  it('keeps resumed task A and sibling task B in distinct directories when task A directory was deleted', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)

    const sessionA = await openNewFSASession({
      task: folderTask('task-a', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const pausedA = await sessionA.settle(pausedEmpty)
    await sessionA.close()

    await parent.removeEntry('Photos')

    const sessionB = await openNewFSASession({
      task: folderTask('task-b', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(sessionB.record)).toBe('Photos (1)')

    const resumedA = await reopenFSASession({
      task: pausedA,
      store: store as unknown as ReceiveStore,
      allowPermissionPrompt: false,
    })
    const fileA = await resumedA.transferOutput.openFile({
      fileId: 'a1', path: ['a.jpg'], sourcePath: ['Photos', 'a.jpg'], revision: 'rev-a', exactSize: 2n,
    }, new AbortController().signal)
    await fileA.writeAt(0n, Uint8Array.of(1, 2))
    await fileA.commit([byteRange(0n, 2n)])
    await resumedA.close()
    await sessionB.close()

    const dirA = parent.children.get('Photos') as FakeDirectoryHandle
    const dirB = parent.children.get('Photos (1)') as FakeDirectoryHandle
    expect([...(dirA.children.get('a.jpg') as FakeFileHandle).bytes]).toEqual([1, 2])
    expect(dirB.children.size).toBe(0)
  })

  it('deduplicates across case-folded aliases, Unicode forms, and file-versus-folder kinds', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)

    const folderA = await openNewFSASession({
      task: folderTask('dir-claim', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    await folderA.settle(pausedEmpty)
    await folderA.close()
    await parent.removeEntry('Photos')

    // Case-folded single-file task collides with the paused directory claim and advances to "photos (1)".
    const fileB = await openNewFSASession({
      task: singleFileTask('file-claim', 'photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(fileB.record)).toBe('photos (1)')
    await fileB.settle(pausedEmpty)
    await fileB.close()
    await parent.removeEntry('photos (1)')

    // Another folder task requesting "PHOTOS" must skip both case-folded claims in IndexedDB.
    const folderC = await openNewFSASession({
      task: folderTask('dir-claim-2', 'PHOTOS'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(folderC.record)).toBe('PHOTOS (2)')
    await folderC.close()

    // Unicode NFD vs NFC normalization folds to the same collision key.
    const composed = await openNewFSASession({
      task: folderTask('unicode-nfc', 'Caf\u00e9'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    await composed.settle(pausedEmpty)
    await composed.close()
    await parent.removeEntry('Caf\u00e9')

    const decomposed = await openNewFSASession({
      task: folderTask('unicode-nfd', 'Cafe\u0301'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(decomposed.record)).toBe('Cafe\u0301 (1)')
    await decomposed.close()
  })
})

describe('FSA reservation claim lifecycle', () => {
  it('retains claims only for lifecycles that can still reopen or discard the reserved slot', async () => {
    const base = initialReceiveState('state-check')
    expect(retainsFSAReservationClaim(base)).toBe(true)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, { kind: 'downloading', phase: 'receiving' }))).toBe(true)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, {
      kind: 'paused', reason: 'user', completedFiles: 0n, completedBytes: 0n,
      retainedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n, discoveryComplete: false,
    }))).toBe(true)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, {
      kind: 'failed', reason: 'cancelled', cleanupPending: true,
    }))).toBe(true)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, { kind: 'failed', reason: 'cancelled' }))).toBe(false)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, {
      kind: 'partial', reason: 'failures', savedFiles: 1n, failedFiles: 1n, cleanupPending: true,
    }))).toBe(true)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, {
      kind: 'partial', reason: 'failures', savedFiles: 1n, failedFiles: 1n,
    }))).toBe(false)
    expect(retainsFSAReservationClaim(transitionReceiveState(base, {
      kind: 'saved', savedFiles: 1n, savedBytes: 4n,
    }))).toBe(false)

    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)

    // 1. A saved task whose folder was deleted externally releases the name for reuse.
    const savedSession = await openNewFSASession({
      task: folderTask('saved-task', 'ReleaseMe'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    await savedSession.settle({
      status: 'completed', completedFiles: 0n, completedBytes: 0n,
      discoveredFiles: 0n, discoveredBytes: 0n, discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await savedSession.close()
    await parent.removeEntry('ReleaseMe')

    const reusedAfterSaved = await openNewFSASession({
      task: folderTask('after-saved', 'ReleaseMe'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(reusedAfterSaved.record)).toBe('ReleaseMe')
    await reusedAfterSaved.close()

    // 2. A settled partial task without cleanupPending releases the name; cleanupPending retains it.
    const partialClean = await openNewFSASession({
      task: folderTask('partial-clean', 'PartialClean'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const cleanFile = await partialClean.transferOutput.openFile({
      fileId: 'c1', path: ['ok.bin'], sourcePath: ['PartialClean', 'ok.bin'], revision: 'r1', exactSize: 2n,
    }, new AbortController().signal)
    await cleanFile.writeAt(0n, Uint8Array.of(1, 2))
    await cleanFile.commit([byteRange(0n, 2n)])
    const cleanRecord = await partialClean.settle({
      status: 'partial', completedFiles: 1n, completedBytes: 2n,
      discoveredFiles: 2n, discoveredBytes: 4n, discoveryComplete: true, failedFiles: 1n, failedDirectories: 0n,
    })
    await partialClean.close()
    expect(cleanRecord.lifecycle.kind === 'partial' && cleanRecord.lifecycle.cleanupPending).toBeUndefined()
    await parent.removeEntry('PartialClean', { recursive: true })

    const reusedAfterCleanPartial = await openNewFSASession({
      task: folderTask('after-partial-clean', 'PartialClean'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(reusedAfterCleanPartial.record)).toBe('PartialClean')
    await reusedAfterCleanPartial.close()

    const partialPending = await openNewFSASession({
      task: folderTask('partial-pending', 'PartialPending'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const doneFile = await partialPending.transferOutput.openFile({
      fileId: 'p1', path: ['ok.bin'], sourcePath: ['PartialPending', 'ok.bin'], revision: 'r1', exactSize: 2n,
    }, new AbortController().signal)
    await doneFile.writeAt(0n, Uint8Array.of(1, 2))
    await doneFile.commit([byteRange(0n, 2n)])
    const stuckFile = await partialPending.transferOutput.openFile({
      fileId: 'p2', path: ['stuck.bin'], sourcePath: ['PartialPending', 'stuck.bin'], revision: 'r2', exactSize: 4n,
    }, new AbortController().signal)
    await stuckFile.writeAt(0n, Uint8Array.of(9, 8))
    await stuckFile.pause([byteRange(0n, 2n)])
    const pendingDir = parent.children.get('PartialPending') as FakeDirectoryHandle
    vi.spyOn(pendingDir, 'removeEntry').mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'))
    const pendingRecord = await partialPending.settle({
      status: 'partial', completedFiles: 1n, completedBytes: 2n,
      discoveredFiles: 2n, discoveredBytes: 6n, discoveryComplete: true, failedFiles: 1n, failedDirectories: 0n,
    })
    await partialPending.close()
    expect(pendingRecord.lifecycle).toMatchObject({ kind: 'partial', cleanupPending: true })
    await parent.removeEntry('PartialPending', { recursive: true })

    const blockedByPending = await openNewFSASession({
      task: folderTask('after-partial-pending', 'PartialPending'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(blockedByPending.record)).toBe('PartialPending (1)')
    await blockedByPending.close()

    // 3a. A cleanly settled failed task already removed its disk entry, so the name is immediately reusable.
    const failedSession = await openNewFSASession({
      task: folderTask('failed-task', 'RetryFolder'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    const failedRecord = await failedSession.settle({
      status: 'stopped', completedFiles: 0n, completedBytes: 0n,
      discoveredFiles: 0n, discoveredBytes: 0n, discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await failedSession.close()
    expect(parent.children.has('RetryFolder')).toBe(false)
    expect(failedRecord.lifecycle.kind === 'failed' ? failedRecord.lifecycle.cleanupPending : undefined).toBeUndefined()

    const reusedAfterFailed = await openNewFSASession({
      task: folderTask('sibling-after-failed', 'RetryFolder'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(reusedAfterFailed.record)).toBe('RetryFolder')
    // Discarding a stale snapshot of the already-cleaned failed task must never touch the sibling's folder.
    await discardFSATask(failedRecord, store as unknown as ReceiveStore)
    expect(parent.children.has('RetryFolder')).toBe(true)
    await reusedAfterFailed.close()

    // 3b. A failed task whose terminal cleanup was deferred retains its claim until discarded.
    const deferredSession = await openNewFSASession({
      task: folderTask('failed-pending-task', 'DeferredFolder'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    vi.spyOn(parent, 'removeEntry').mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'))
    const deferredRecord = await deferredSession.settle({
      status: 'stopped', completedFiles: 0n, completedBytes: 0n,
      discoveredFiles: 0n, discoveredBytes: 0n, discoveryComplete: false, failedFiles: 0n, failedDirectories: 0n,
    })
    await deferredSession.close()
    expect(deferredRecord.lifecycle).toMatchObject({ kind: 'failed', reason: 'cancelled', cleanupPending: true })
    await parent.removeEntry('DeferredFolder')

    const siblingWhilePending = await openNewFSASession({
      task: folderTask('sibling-while-failed-pending', 'DeferredFolder'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(siblingWhilePending.record)).toBe('DeferredFolder (1)')
    await discardFSATask(deferredRecord, store as unknown as ReceiveStore)
    expect(parent.children.has('DeferredFolder (1)')).toBe(true)
    await siblingWhilePending.close()

    // Once the cleanup-pending failed task is discarded, the original name becomes available again.
    const afterDiscard = await openNewFSASession({
      task: folderTask('after-failed-discard', 'DeferredFolder'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(reservedNameOf(afterDiscard.record)).toBe('DeferredFolder')
    await afterDiscard.close()
  })

  it('scopes claims to the same parent directory and tolerates stale historical parent handles', async () => {
    const store = new MemoryReceiveStore()
    const downloads = new FakeDirectoryHandle('Downloads', store.events)
    const desktop = new FakeDirectoryHandle('Desktop', store.events)

    const otherDirSession = await openNewFSASession({
      task: folderTask('other-parent', 'Photos'),
      parentHandle: desktop as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    await otherDirSession.settle(pausedEmpty)
    await otherDirSession.close()
    await desktop.removeEntry('Photos')

    // A stale record whose handle throws during isSameEntry must not block new allocations.
    const brokenHandle = {
      kind: 'directory',
      name: 'Stale',
      isSameEntry: async () => { throw new DOMException('stale handle', 'NotFoundError') },
    } as unknown as FileSystemDirectoryHandle
    await store.putTask({
      taskId: 'stale-task',
      shareInstance: 'share-stale',
      selectionKey: 'sel-stale',
      selection: { shareInstance: 'share-stale', syntheticRoot: 'root', defaultSelected: true, rules: [] },
      artifact: { kind: 'directory-tree', name: 'Photos' },
      backend: 'fsa',
      lifecycle: transitionReceiveState(initialReceiveState('stale-task'), {
        kind: 'paused', reason: 'user', completedFiles: 0n, completedBytes: 0n,
        retainedBytes: 0n, discoveredFiles: 0n, discoveredBytes: 0n, discoveryComplete: false,
      }),
      target: {
        kind: 'fsa',
        parentHandle: brokenHandle,
        reservedName: 'Photos',
        collisionIndex: 0,
        singleFile: false,
        reservedAtMs: Date.now() - 10_000,
      },
      discoveryComplete: false,
      updatedAt: Date.now() - 10_000,
    })

    const isSameSpy = vi.spyOn(downloads, 'isSameEntry').mockImplementation(async other => {
      if (other === brokenHandle) throw new DOMException('stale handle', 'NotFoundError')
      return other instanceof FakeDirectoryHandle && other.path === downloads.path
    })

    const session = await openNewFSASession({
      task: folderTask('downloads-task', 'Photos'),
      parentHandle: downloads as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })
    expect(isSameSpy).toHaveBeenCalled()
    expect(reservedNameOf(session.record)).toBe('Photos')
    await session.close()
  })

  it('rolls back a failed putTask inside the reservation lock before a waiting task allocates the slot', async () => {
    const store = new MemoryReceiveStore()
    const parent = new FakeDirectoryHandle('Downloads', store.events)
    const locks = makeBlockingLockManager()

    let releaseFirstPut!: () => void
    const firstPutStarted = new Promise<void>(resolve => {
      const originalPut = store.putTask.bind(store)
      vi.spyOn(store, 'putTask').mockImplementation(async record => {
        if (record.taskId === 'failing-open') {
          resolve()
          await new Promise<void>(done => { releaseFirstPut = done })
          throw new DOMException('quota', 'QuotaExceededError')
        }
        return originalPut(record)
      })
    })

    const firstOpen = openNewFSASession({
      task: folderTask('failing-open', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      locks,
    })
    await firstPutStarted

    // While failing-open is paused inside putTask, second-open queues behind the reservation lock.
    const secondOpen = openNewFSASession({
      task: folderTask('second-open', 'Photos'),
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
      locks,
    })
    releaseFirstPut()

    await expect(firstOpen).rejects.toMatchObject({ name: 'QuotaExceededError' })
    const second = await secondOpen
    expect(reservedNameOf(second.record)).toBe('Photos')
    expect(parent.children.has('Photos')).toBe(true)
    expect(parent.children.has('Photos (1)')).toBe(false)
    await second.close()
  })
})
