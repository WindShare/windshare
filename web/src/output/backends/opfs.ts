import type { ByteRange } from '../../content/geometry'
import { classifyNativeError, storagePauseReason } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import type {
  CompletedOutputFile,
  TransferDirectoryRequest,
  TransferFileRequest,
  TransferOutputFile,
  TransferOutputSession,
} from '../../transfer/output-port'
import { transitionReceiveState } from '../../transfer/state'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import { openNativeObject } from '../native-object/client'
import type { NativeObjectFactory } from '../native-object/contracts'
import { ObjectCheckpointCoordinator } from '../native-object/coordinator'
import type { ReceiveStore } from '../store/database'
import { acquireTaskLock, type TaskLock } from '../store/tab-lock'
import {
  completedTuplesBytes,
  fileRecordId,
  fromRangeTuples,
  toRangeTuples,
  type OPFSTargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import { hasZipLayout, survivingZipCoverage } from '../zip/stored-members'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../zip/progressive-zip'
import {
  artifactDownloadMediaType,
  triggerBrowserDownload,
  type BrowserDownloadWindow,
} from './browser-handoff'
import {
  loadLockedReopenableTask,
  mergeSourceOmittedEntries,
  resolveZipEntryPath,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from './contract'
import {
  completedZipTailOffset,
  hasCompletedZipFile,
  isPersistedCompletedRecord,
  nextOPFSIncompleteLifecycle,
  readyToSaveLifecycle,
  survivingFileCoverage,
  type IncompleteOPFSJobStatus,
} from './opfs-settlement'

const OPFS_ROOT_DIR = 'windshare-tasks'
const OPFS_MAX_CONCURRENT_FILES = 4
const OPFS_CHECKPOINT_INTERVAL_BYTES = 16n * 1024n * 1024n
const OPFS_CHECKPOINT_INTERVAL_MS = 10_000

export interface OPFSEnvironment {
  readonly getRootDirectory: () => Promise<FileSystemDirectoryHandle>
  readonly openNativeObject?: NativeObjectFactory
  readonly locks?: LockManager
}

export function browserOPFSEnvironment(
  storage: StorageManager | undefined = globalThis.navigator?.storage,
  locks: LockManager | undefined = globalThis.navigator?.locks,
): OPFSEnvironment {
  if (typeof storage?.getDirectory !== 'function') {
    throw new DOMException('Origin Private File System is unavailable', 'NotSupportedError')
  }
  return Object.freeze({
    getRootDirectory: () => storage.getDirectory(),
    openNativeObject,
    ...(locks === undefined ? {} : { locks }),
  })
}

async function openTasksDirectory(env: OPFSEnvironment, create = false): Promise<FileSystemDirectoryHandle> {
  const root = await env.getRootDirectory()
  return root.getDirectoryHandle(OPFS_ROOT_DIR, { create })
}

interface OPFSSessionPorts {
  readonly store: ReceiveStore
  readonly env: OPFSEnvironment
  readonly windowPort: BrowserDownloadWindow
  readonly diagnostics?: OutputDiagnosticsPorts
}

export function openNewOPFSSession(
  input: OPFSSessionPorts & { readonly task: StartBackendTaskInput },
): Promise<OutputSessionHandle> {
  const { task } = input
  const target: OPFSTargetRecord = {
    kind: 'opfs', objectName: `${task.taskId}.part`, fileName: task.artifact.name,
    isZip: task.artifact.kind === 'zip-archive', allocatedLength: 0n,
  }
  const record: ReceiveTaskRecord = {
    taskId: task.taskId, shareInstance: task.selection.shareInstance, selectionKey: task.selectionKey,
    selection: task.selection, artifact: task.artifact, backend: 'opfs', lifecycle: task.lifecycle, target,
    ...(task.display === undefined ? {} : {
      display: { ...task.display, destinationLabel: { kind: 'browser-downloads' } },
    }),
    discoveryComplete: false, updatedAt: Date.now(),
  }
  return openOPFSSession(input, record, false)
}

export function reopenOPFSSession(
  input: OPFSSessionPorts & { readonly task: ReceiveTaskRecord; readonly signal?: AbortSignal },
): Promise<OutputSessionHandle> {
  if (input.task.target.kind !== 'opfs') {
    return Promise.reject(new TypeError('Cannot reopen a non-OPFS task with the OPFS backend'))
  }
  return openOPFSSession(input, input.task, true, input.signal)
}

async function openOPFSSession(
  ports: OPFSSessionPorts, initialTask: ReceiveTaskRecord, reopened: boolean, signal?: AbortSignal,
): Promise<OutputSessionHandle> {
  let lock: TaskLock | undefined
  let io: ObjectCheckpointCoordinator | undefined
  let directory: FileSystemDirectoryHandle | undefined
  let fileHandle: FileSystemFileHandle | undefined
  try {
    lock = await acquireTaskLock(initialTask.taskId, ports.env.locks)
    const task = reopened
      ? await loadLockedReopenableTask(ports.store, initialTask.taskId, 'opfs', signal)
      : initialTask
    const target = task.target as OPFSTargetRecord
    directory = await openTasksDirectory(ports.env, true)
    fileHandle = await directory.getFileHandle(target.objectName, { create: true })
    const nativeFactory = ports.env.openNativeObject ?? openNativeObject
    io = new ObjectCheckpointCoordinator({
      io: await nativeFactory(fileHandle), operationId: task.taskId, objectId: target.objectName,
    })
    const zip = target.isZip ? await ProgressiveZipWriter.open(task.taskId, ports.store, io, ports.diagnostics?.trace) : undefined
    const record: ReceiveTaskRecord = Object.freeze({
      ...task,
      lifecycle: transitionReceiveState(task.lifecycle, { kind: 'downloading', phase: 'receiving' }),
      updatedAt: Date.now(),
    })
    const session = new OPFSSessionHandle({
      ...ports, record, fileHandle, directory, io, zip, reopened, lock,
      ...(reopened ? { reopenedFrom: task } : {}),
    })
    await ports.store.putTask(record)
    emitOutputTrace(ports.diagnostics?.trace, reopened ? 'reopen' : 'output_reservation', {
      backend: 'origin_private', task_id: task.taskId, transition: reopened ? 'authorized' : 'acquired',
    })
    return session
  } catch (error) {
    // A successor holding the Web Lock must be able to acquire the exclusive SyncAccessHandle.
    await io?.close().catch(() => undefined)
    if (!reopened && directory !== undefined && fileHandle !== undefined) {
      await cleanupUncommittedOPFSObject(ports.store, directory, initialTask)
    }
    await lock?.release()
    emitOutputTrace(ports.diagnostics?.trace, reopened ? 'reopen' : 'output_reservation', {
      backend: 'origin_private', task_id: initialTask.taskId, transition: 'failed', error_class: classifyNativeError(error),
    })
    throw error
  }
}

async function cleanupUncommittedOPFSObject(
  store: ReceiveStore,
  directory: FileSystemDirectoryHandle,
  task: ReceiveTaskRecord,
): Promise<void> {
  try {
    if (await store.getTask(task.taskId) === undefined) {
      await removePrivateObject(directory, (task.target as OPFSTargetRecord).objectName)
    }
  } catch { /* Preserve the opening error if storage itself is unavailable. */ }
}

/** Exports a completed retained OPFS task to the user's browser Downloads folder (works offline). */
export async function saveRetainedOPFSTask(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  env: OPFSEnvironment,
  windowPort: BrowserDownloadWindow,
  signal?: AbortSignal,
): Promise<ReceiveTaskRecord> {
  if (task.target.kind !== 'opfs') {
    throw new TypeError('Only OPFS tasks can be exported from browser storage')
  }
  const lock = await acquireTaskLock(task.taskId, env.locks)
  try {
    signal?.throwIfAborted()
    const current = await store.getTask(task.taskId)
    signal?.throwIfAborted()
    if (current === undefined) throw new DOMException('This download record no longer exists', 'NotFoundError')
    if (current.target.kind !== 'opfs') throw new TypeError('Only OPFS tasks can be exported from browser storage')
    const dir = await openTasksDirectory(env)
    const handle = await dir.getFileHandle(current.target.objectName)
    const file = await handle.getFile()
    if (BigInt(file.size) !== current.target.sealedSize) {
      throw new DOMException('Retained browser file is missing or incomplete', 'NotFoundError')
    }
    const mediaType = artifactDownloadMediaType(current.artifact.kind)
    signal?.throwIfAborted()
    triggerBrowserDownload(windowPort, file, current.target.fileName, mediaType)
    const nextLifecycle = transitionReceiveState(current.lifecycle, {
      kind: 'download-started',
      retained: true,
      fileName: current.target.fileName,
    })
    const updated: ReceiveTaskRecord = Object.freeze({
      ...current,
      lifecycle: nextLifecycle,
      updatedAt: Date.now(),
    })
    await store.putTask(updated)
    return updated
  } finally {
    await lock.release()
  }
}

/** Exports all completed files from a retained OPFS progressive ZIP without modifying its stored state. */
export async function exportPartialOPFSZip(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  env: OPFSEnvironment,
  windowPort: BrowserDownloadWindow,
  signal?: AbortSignal,
): Promise<Blob> {
  if (task.target.kind !== 'opfs' || !task.target.isZip) {
    throw new TypeError('Partial ZIP export requires a retained OPFS ZIP task')
  }
  const lock = await acquireTaskLock(task.taskId, env.locks)
  try {
    const dir = await openTasksDirectory(env)
    const handle = await dir.getFileHandle(task.target.objectName)
    const file = await handle.getFile()
    const files = await store.listTaskFiles(task.taskId)
    const blob = await buildPartialZipBlob(file, files, signal === undefined ? undefined : { signal })
    const partialName = task.target.fileName.replace(/\.zip$/iu, '') + '-partial.zip'
    signal?.throwIfAborted()
    triggerBrowserDownload(windowPort, blob, partialName, 'application/zip')
    return blob
  } finally {
    await lock.release()
  }
}

function isNotFound(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'NotFoundError'
}

async function removePrivateObject(directory: FileSystemDirectoryHandle, name: string): Promise<boolean> {
  try {
    await directory.removeEntry(name)
    return true
  } catch (error) {
    if (isNotFound(error)) return false
    throw error
  }
}

export async function deleteOPFSTask(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  env: OPFSEnvironment,
  signal?: AbortSignal,
): Promise<void> {
  if (task.target.kind !== 'opfs') throw new TypeError('Only OPFS tasks own browser storage objects')
  const lock = await acquireTaskLock(task.taskId, env.locks)
  try {
    const dir = await openTasksDirectory(env).catch((error: unknown) => {
      if (isNotFound(error)) return undefined
      throw error
    })
    signal?.throwIfAborted()
    const removed = dir !== undefined && await removePrivateObject(dir, task.target.objectName)
    // Once deletion starts reclaiming bytes, finish removing the matching metadata.
    if (!removed) signal?.throwIfAborted()
    await store.deleteTask(task.taskId)
  } finally {
    await lock.release()
  }
}

class OPFSSessionHandle implements OutputSessionHandle {
  readonly taskId: string
  readonly backend = 'opfs' as const
  readonly transferOutput: TransferOutputSession
  readonly reopenedFrom?: ReceiveTaskRecord
  #record: ReceiveTaskRecord
  readonly #directory: FileSystemDirectoryHandle
  readonly #fileHandle: FileSystemFileHandle
  readonly #io: ObjectCheckpointCoordinator
  readonly #zip: ProgressiveZipWriter | undefined
  readonly #store: ReceiveStore
  readonly #lock: TaskLock
  readonly #windowPort: BrowserDownloadWindow
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #closingIO: Promise<void> | undefined
  #taskWriteTail: Promise<unknown> = Promise.resolve()

  constructor(input: Readonly<{
    record: ReceiveTaskRecord
    reopenedFrom?: ReceiveTaskRecord
    directory: FileSystemDirectoryHandle
    fileHandle: FileSystemFileHandle
    io: ObjectCheckpointCoordinator
    zip: ProgressiveZipWriter | undefined
    reopened: boolean
    store: ReceiveStore
    lock: TaskLock
    windowPort: BrowserDownloadWindow
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.taskId = input.record.taskId
    if (input.reopenedFrom !== undefined) this.reopenedFrom = input.reopenedFrom
    this.#record = input.record
    this.#directory = input.directory
    this.#fileHandle = input.fileHandle
    this.#io = input.io
    this.#zip = input.zip
    this.#store = input.store
    this.#lock = input.lock
    this.#windowPort = input.windowPort
    this.#diagnostics = input.diagnostics
    this.transferOutput = Object.freeze({
      sessionId: input.record.taskId,
      capabilities: Object.freeze({
        maxConcurrentFiles: OPFS_MAX_CONCURRENT_FILES,
        resumable: true,
        periodicCheckpoint: Object.freeze({
          intervalBytes: OPFS_CHECKPOINT_INTERVAL_BYTES,
          intervalMilliseconds: OPFS_CHECKPOINT_INTERVAL_MS,
        }),
      }),
      ...(input.zip === undefined ? {} : {
        ensureDirectory: async (request: TransferDirectoryRequest) => {
          const zipPath = resolveZipEntryPath(input.record.artifact, request.path)
          if (zipPath === undefined) return
          await input.zip!.ensureDirectory(request.directoryId, zipPath, request.modifiedTimeMilliseconds)
        },
      }),
      ...(input.reopened ? {
        checkCompleted: (fileId: string, signal: AbortSignal) => this.#checkCompleted(fileId, signal),
      } : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
      markDiscoveryComplete: () => this.#serializeTaskWrite(async () => {
        // Single-file commit may already have promoted this task to `ready-to-save`.
        if (this.#record.discoveryComplete || this.#record.lifecycle.kind !== 'downloading') return
        const updated = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(updated)
        this.#record = updated
      }),
    })
  }

  get record(): ReceiveTaskRecord {
    return this.#record
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    if (result.status === 'completed' && this.#zip !== undefined) {
      return this.#settleCompletedZip(result)
    }

    const { files, truncatedLength } = await this.#closeIncompleteSession(result.status)
    if (this.#zip === undefined) {
      // Single-file commit already truncated, flushed, and sealed `ready-to-save`; a late pause or stop
      // must not downgrade a complete local file back to an online-only paused state.
      const committed = files.find(file => file.status === 'completed')
      if (committed !== undefined) return this.#publishCompleted(committed.size, result)
    }
    if (result.status === 'completed') {
      return this.#settleIncomplete('failed', { ...result, error: new Error('Missing committed browser file') }, files)
    }
    return this.#settleIncomplete(result.status, result, files, truncatedLength)
  }

  async #settleCompletedZip(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    let size: bigint
    try {
      size = await this.#zip!.finalize()
    } catch (error) {
      await this.#closeIO()
      await this.#taskWriteTail
      const pauseReason = storagePauseReason(error)
      if (pauseReason !== undefined) return this.#settleIncomplete('paused', { ...result, pauseReason, error })
      return this.#settleIncomplete('failed', { ...result, error })
    }
    await this.#closeIO()
    await this.#taskWriteTail
    return this.#publishCompleted(size, result)
  }

  async #closeIncompleteSession(
    status: SettledTransferJobResult['status'],
  ): Promise<Readonly<{ files: readonly ReceiveFileRecord[]; truncatedLength?: bigint }>> {
    if (this.#zip === undefined || status === 'paused' || status === 'completed') {
      await this.#closeIO()
      await this.#taskWriteTail
      return { files: await this.#store.listTaskFiles(this.taskId) }
    }
    try {
      const files = await this.#store.listTaskFiles(this.taskId)
      const truncatedLength = hasCompletedZipFile(files) ? await this.#truncateUnusedZipTail(files) : undefined
      return truncatedLength === undefined ? { files } : { files, truncatedLength }
    } finally {
      await this.#closeIO()
      await this.#taskWriteTail
    }
  }

  async close(): Promise<void> {
    await this.#closeIO()
    await this.#lock.release()
  }

  async #publishCompleted(sealedSize: bigint, result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    let file: File | undefined
    try {
      file = await this.#fileHandle.getFile()
    } catch (error) {
      if (isNotFound(error)) return this.#interruptTruncatedOutput(0n, result)
      // A transient failure to read a committed object can be retried from Downloads.
    }
    if (file !== undefined && BigInt(file.size) !== sealedSize) {
      return this.#interruptTruncatedOutput(BigInt(file.size), result)
    }
    const currentTarget = this.#record.target as OPFSTargetRecord
    const target: OPFSTargetRecord = Object.freeze({
      ...currentTarget,
      allocatedLength: this.#zip?.allocatedLength ?? sealedSize,
      sealedSize,
    })

    let nextLifecycle = readyToSaveLifecycle(this.#record.lifecycle, sealedSize, target.fileName, result.contentWarning)
    const { sourceOmittedEntries: previousOmitted, ...baseRecord } = this.#record
    const sourceOmittedEntries = mergeSourceOmittedEntries(
      result.sourceOmittedEntries,
      previousOmitted,
      result.discoveryComplete,
    )
    // A late stop or pause leaves a complete file in `ready-to-save` for the user to export on demand.
    if (result.status === 'completed' && file !== undefined) {
      nextLifecycle = this.#handoffCompletedFile(file, target.fileName, nextLifecycle, sourceOmittedEntries)
    }

    if (isPersistedCompletedRecord(this.#record, target, nextLifecycle, sourceOmittedEntries)) {
      return this.#record
    }

    const updated: ReceiveTaskRecord = Object.freeze({
      ...baseRecord,
      target,
      lifecycle: nextLifecycle,
      ...(sourceOmittedEntries === undefined ? {} : { sourceOmittedEntries }),
      discoveryComplete: true,
      updatedAt: Date.now(),
    })
    await this.#store.putTask(updated)
    this.#record = updated
    return updated
  }

  #handoffCompletedFile(
    file: File,
    fileName: string,
    lifecycle: ReceiveTaskRecord['lifecycle'],
    sourceOmittedEntries: bigint | undefined,
  ): ReceiveTaskRecord['lifecycle'] {
    try {
      const mediaType = artifactDownloadMediaType(this.#record.artifact.kind)
      triggerBrowserDownload(this.#windowPort, file, fileName, mediaType)
      emitOutputTrace(this.#diagnostics?.trace, 'publication', {
        backend: 'origin_private', task_id: this.taskId, transition: 'committed',
        ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
      })
      return transitionReceiveState(lifecycle, { kind: 'download-started', retained: true, fileName })
    } catch {
      /* Downloads can retry a handoff that needs a fresh user gesture. */
      return lifecycle
    }
  }

  async #interruptTruncatedOutput(physicalSize: bigint, result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    emitOutputTrace(this.#diagnostics?.trace, 'publication', {
      backend: 'origin_private', task_id: this.taskId, transition: 'incomplete', physical_bytes: String(physicalSize),
    })
    const files = await this.#store.listTaskFiles(this.taskId)
    const verified: ReceiveFileRecord[] = []
    for (const row of files) {
      const recovered = hasZipLayout(row) ? survivingZipCoverage(row, physicalSize) : survivingFileCoverage(row, physicalSize)
      if (recovered !== row) await this.#store.putFile(recovered)
      verified.push(recovered)
    }
    return this.#settleIncomplete('paused', { ...result, pauseReason: 'interrupted' }, verified)
  }

  async #settleIncomplete(
    status: IncompleteOPFSJobStatus,
    result: SettledTransferJobResult,
    knownFiles?: readonly ReceiveFileRecord[],
    truncatedLength?: bigint,
  ): Promise<ReceiveTaskRecord> {
    const files = knownFiles ?? await this.#store.listTaskFiles(this.taskId)
    let retainedBytes = 0n
    let storedCompletedFiles = 0n
    let storedCompletedBytes = 0n
    for (const file of files) {
      retainedBytes += completedTuplesBytes(file.completedRanges)
      if (file.status === 'completed' && file.zipEntry?.kind !== 'directory') {
        storedCompletedFiles += 1n
        storedCompletedBytes += file.size
      }
    }

    const nextLifecycle = nextOPFSIncompleteLifecycle(this.#record.lifecycle, status, result, {
      isZip: this.#zip !== undefined,
      retainedBytes,
      storedCompletedFiles, storedCompletedBytes,
    })

    const currentTarget = this.#record.target as OPFSTargetRecord
    // Strip any prior `sealedSize` when settling incomplete (e.g. truncated output recovery).
    const target: OPFSTargetRecord = Object.freeze({
      kind: 'opfs',
      objectName: currentTarget.objectName,
      fileName: currentTarget.fileName,
      isZip: currentTarget.isZip,
      allocatedLength: truncatedLength ?? this.#zip?.allocatedLength ?? currentTarget.allocatedLength,
    })
    const { sourceOmittedEntries: previousOmitted, ...baseRecord } = this.#record
    const sourceOmittedEntries = mergeSourceOmittedEntries(
      result.sourceOmittedEntries,
      previousOmitted,
      result.discoveryComplete,
    )
    const updated: ReceiveTaskRecord = Object.freeze({
      ...baseRecord,
      target,
      lifecycle: nextLifecycle,
      discoveryComplete: result.discoveryComplete || this.#record.discoveryComplete,
      ...(sourceOmittedEntries === undefined ? {} : { sourceOmittedEntries }),
      ...(result.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: result.sourceRevisionFailures }),
      updatedAt: Date.now(),
    })
    await this.#store.putTask(updated)
    this.#record = updated
    if (nextLifecycle.kind === 'failed' && storedCompletedFiles === 0n) {
      // Persist the terminal decision before reclaiming bytes; Discard retries any interrupted removal.
      try {
        await removePrivateObject(this.#directory, target.objectName)
      } catch (error) {
        emitOutputTrace(this.#diagnostics?.trace, 'cleanup', {
          backend: 'origin_private', task_id: this.taskId, transition: 'failed',
          error_class: classifyNativeError(error),
        })
      }
    }
    emitOutputTrace(this.#diagnostics?.trace, 'settlement', {
      backend: 'origin_private',
      task_id: this.taskId,
      job_status: status,
      lifecycle: nextLifecycle.kind,
      completed_files: String(storedCompletedFiles),
      failed_files: String(result.failedFiles + result.failedDirectories),
      ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
    })
    return updated
  }

  async #checkCompleted(fileId: string, signal: AbortSignal): Promise<CompletedOutputFile | undefined> {
    signal.throwIfAborted()
    if (this.#zip !== undefined) {
      return this.#zip.completedFile(fileId)
    }
    const existing = await this.#store.getFile(this.taskId, fileId)
    if (existing === undefined || existing.status !== 'completed') return undefined
    const physicalSize = await this.#io.size()
    return physicalSize === existing.size ? Object.freeze({ size: existing.size }) : undefined
  }

  async #openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> {
    signal.throwIfAborted()
    if (this.#zip !== undefined) {
      return this.#openZipFile(request)
    }
    return this.#openSingleFile(request)
  }

  async #openZipFile(request: TransferFileRequest): Promise<TransferOutputFile> {
    const zip = this.#zip!
    const zipPath = resolveZipEntryPath(this.#record.artifact, request.path)
    if (zipPath === undefined) {
      throw new TypeError('ZIP file entry requires a non-empty path')
    }
    const opened = await zip.openFile({
      fileId: request.fileId,
      path: zipPath,
      revision: request.revision,
      ...(request.sourceMetadata === undefined ? {} : { sourceMetadata: request.sourceMetadata }),
      exactSize: request.exactSize,
      ...(request.modifiedTimeMilliseconds === undefined
        ? {}
        : { modifiedTimeMilliseconds: request.modifiedTimeMilliseconds }),
    })
    return Object.freeze({
      completedRanges: opened.completedRanges,
      savedRevision: opened.record.revision,
      ...(opened.record.sourceMetadata === undefined
        ? {}
        : { savedSourceMetadata: opened.record.sourceMetadata }),
      savedSize: opened.record.size,
      writeAt: (offset: bigint, data: Uint8Array) => zip.writeFileSlice(opened, offset, data),
      checkpoint: (ranges: readonly ByteRange[]) => zip.checkpointFile(opened, ranges).then(() => 'saved' as const),
      commit: (ranges: readonly ByteRange[]) => zip.commitFile(opened, ranges),
      pause: (ranges: readonly ByteRange[]) => zip.checkpointFile(opened, ranges),
      abort: () => Promise.resolve(),
    })
  }

  async #openSingleFile(request: TransferFileRequest): Promise<TransferOutputFile> {
    const existing = await this.#store.getFile(this.taskId, request.fileId)
    const validExisting = existing === undefined ? undefined : survivingFileCoverage(existing, await this.#io.size())
    if (validExisting !== undefined && validExisting !== existing) await this.#store.putFile(validExisting)

    const buildFileRow = (status: 'receiving' | 'completed', ranges: readonly ByteRange[]): ReceiveFileRecord =>
      Object.freeze({
        id: fileRecordId(this.taskId, request.fileId),
        taskId: this.taskId,
        fileId: request.fileId,
        revision: request.revision,
        ...(request.sourceMetadata === undefined ? {} : { sourceMetadata: request.sourceMetadata }),
        path: request.path,
        size: request.exactSize,
        completedRanges: toRangeTuples(ranges),
        status,
      })

    const persistReceiving = (ranges: readonly ByteRange[]) => {
      const row = buildFileRow('receiving', ranges)
      // Coordinator flushes the SyncAccessHandle BEFORE writing `completedRanges` to IDB.
      return this.#io.checkpoint('opfs-file-receiving', () => this.#store.putFile(row))
    }

    const commitCompleted = (ranges: readonly ByteRange[]) => {
      const row = buildFileRow('completed', ranges)
      // Coordinator flushes the truncated SyncAccessHandle BEFORE atomically sealing both the
      // file row and task record in one IndexedDB transaction so a crash before settle() still
      // leaves a locally exportable `ready-to-save` task instead of an online-only `paused` task.
      return this.#io.checkpoint('opfs-file-completed', () =>
        this.#serializeTaskWrite(async () => {
          const currentTarget = this.#record.target as OPFSTargetRecord
          const target: OPFSTargetRecord = Object.freeze({
            ...currentTarget,
            allocatedLength: request.exactSize,
            sealedSize: request.exactSize,
          })
          const now = Date.now()
          const readyTask: ReceiveTaskRecord = Object.freeze({
            ...this.#record,
            target,
            lifecycle: transitionReceiveState(this.#record.lifecycle, {
              kind: 'ready-to-save',
              size: request.exactSize,
              fileName: target.fileName,
              contentWarning: null,
            }, now),
            discoveryComplete: true,
            updatedAt: now,
          })
          await this.#store.putTaskAndFile(readyTask, row)
          this.#record = readyTask
          emitOutputTrace(this.#diagnostics?.trace, 'publication', {
            backend: 'origin_private',
            task_id: this.taskId,
            transition: 'ready_to_save',
            sealed_bytes: String(request.exactSize),
          })
        }))
    }

    return Object.freeze({
      completedRanges: validExisting === undefined ? Object.freeze([]) : fromRangeTuples(validExisting.completedRanges),
      ...(validExisting === undefined ? {} : {
        savedRevision: validExisting.revision,
        ...(validExisting.sourceMetadata === undefined ? {} : { savedSourceMetadata: validExisting.sourceMetadata }),
        savedSize: validExisting.size,
      }),
      writeAt: (offset: bigint, data: Uint8Array) => this.#io.writeAt(offset, data),
      checkpoint: (ranges: readonly ByteRange[]) => persistReceiving(ranges).then(() => 'saved' as const),
      commit: async (ranges: readonly ByteRange[]) => {
        await this.#io.truncate(request.exactSize)
        await commitCompleted(ranges)
      },
      pause: (ranges: readonly ByteRange[]) => persistReceiving(ranges),
      abort: () => Promise.resolve(),
    })
  }

  async #truncateUnusedZipTail(files: readonly ReceiveFileRecord[]): Promise<bigint | undefined> {
    if (this.#zip === undefined || this.#io.failed) return undefined
    const retainedEnd = completedZipTailOffset(files)
    try {
      const physicalSize = await this.#io.size()
      if (retainedEnd >= physicalSize) return undefined
      await this.#io.truncate(retainedEnd)
      await this.#io.flush()
      emitOutputTrace(this.#diagnostics?.trace, 'cleanup', {
        backend: 'origin_private',
        task_id: this.taskId,
        transition: 'tail_truncated',
        reclaimed_bytes: String(physicalSize - retainedEnd),
        retained_bytes: String(retainedEnd),
      })
      return retainedEnd
    } catch (error) {
      emitOutputTrace(this.#diagnostics?.trace, 'cleanup', {
        backend: 'origin_private',
        task_id: this.taskId,
        transition: 'tail_truncate_failed',
        error_class: classifyNativeError(error),
      })
      return undefined
    }
  }

  #serializeTaskWrite<T>(write: () => Promise<T>): Promise<T> {
    const result = this.#taskWriteTail.then(write)
    this.#taskWriteTail = result.catch(() => undefined)
    return result
  }

  #closeIO(): Promise<void> {
    this.#closingIO ??= this.#io.close().catch(() => undefined)
    return this.#closingIO
  }
}
