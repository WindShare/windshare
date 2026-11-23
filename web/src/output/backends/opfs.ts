import type { ByteRange } from '../../content/geometry'
import { storagePauseReason } from '../../transfer/fault'
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
  maxRangeEnd,
  toRangeTuples,
  type OPFSTargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../zip/progressive-zip'
import {
  artifactDownloadMediaType,
  triggerBrowserDownload,
  type BrowserDownloadWindow,
} from './browser-handoff'
import { resolveZipEntryPath, type OutputSessionHandle, type StartBackendTaskInput } from './contract'

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

async function openTasksDirectory(env: OPFSEnvironment): Promise<FileSystemDirectoryHandle> {
  const root = await env.getRootDirectory()
  return root.getDirectoryHandle(OPFS_ROOT_DIR, { create: true })
}

export async function openNewOPFSSession(input: Readonly<{
  task: StartBackendTaskInput
  store: ReceiveStore
  env: OPFSEnvironment
  windowPort: BrowserDownloadWindow
  diagnostics?: OutputDiagnosticsPorts
}>): Promise<OutputSessionHandle> {
  const lock = await acquireTaskLock(input.task.taskId, input.env.locks)
  try {
    const isZip = input.task.artifact.kind === 'zip-archive'
    const objectName = `${input.task.taskId}.part`
    const dir = await openTasksDirectory(input.env)
    const fileHandle = await dir.getFileHandle(objectName, { create: true })
    const nativeFactory = input.env.openNativeObject ?? openNativeObject
    const io = new ObjectCheckpointCoordinator({
      io: await nativeFactory(fileHandle),
      operationId: input.task.taskId,
      objectId: objectName,
    })

    const target: OPFSTargetRecord = Object.freeze({
      kind: 'opfs',
      objectName,
      fileName: input.task.artifact.name,
      isZip,
      allocatedLength: 0n,
    })
    const display = input.task.display === undefined
      ? undefined
      : Object.freeze({
          ...input.task.display,
          destinationLabel: Object.freeze({ kind: 'browser-downloads' as const }),
        })
    const downloading = transitionReceiveState(input.task.lifecycle, {
      kind: 'downloading',
      phase: 'receiving',
    })
    const record: ReceiveTaskRecord = Object.freeze({
      taskId: input.task.taskId,
      shareInstance: input.task.selection.shareInstance,
      selectionKey: input.task.selectionKey,
      selection: input.task.selection,
      artifact: input.task.artifact,
      backend: 'opfs',
      lifecycle: downloading,
      target,
      ...(display === undefined ? {} : { display }),
      discoveryComplete: false,
      updatedAt: Date.now(),
    })
    await input.store.putTask(record)
    const zip = isZip ? await ProgressiveZipWriter.open(record.taskId, input.store, io) : undefined
    emitOutputTrace(input.diagnostics?.trace, 'output_reservation', {
      backend: 'origin_private',
      transition: 'acquired',
    })
    return new OPFSSessionHandle({
      record,
      fileHandle,
      io,
      ...(zip === undefined ? {} : { zip }),
      reopened: false,
      store: input.store,
      lock,
      windowPort: input.windowPort,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
  } catch (error) {
    await lock.release()
    throw error
  }
}

export async function reopenOPFSSession(input: Readonly<{
  task: ReceiveTaskRecord
  store: ReceiveStore
  env: OPFSEnvironment
  windowPort: BrowserDownloadWindow
  diagnostics?: OutputDiagnosticsPorts
}>): Promise<OutputSessionHandle> {
  if (input.task.target.kind !== 'opfs') {
    throw new TypeError('Cannot reopen a non-OPFS task with the OPFS backend')
  }
  let lock: TaskLock
  try {
    lock = await acquireTaskLock(input.task.taskId, input.env.locks)
  } catch (error) {
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'origin_private',
      transition: 'failed',
    })
    throw error
  }
  try {
    const target = input.task.target
    const dir = await openTasksDirectory(input.env)
    const fileHandle = await dir.getFileHandle(target.objectName, { create: true })
    const nativeFactory = input.env.openNativeObject ?? openNativeObject
    const io = new ObjectCheckpointCoordinator({
      io: await nativeFactory(fileHandle),
      operationId: input.task.taskId,
      objectId: target.objectName,
    })
    const downloading = transitionReceiveState(input.task.lifecycle, {
      kind: 'downloading',
      phase: 'receiving',
    })
    const updated: ReceiveTaskRecord = Object.freeze({
      ...input.task,
      lifecycle: downloading,
      updatedAt: Date.now(),
    })
    await input.store.putTask(updated)
    const zip = target.isZip ? await ProgressiveZipWriter.open(updated.taskId, input.store, io) : undefined
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'origin_private',
      transition: 'authorized',
    })
    return new OPFSSessionHandle({
      record: updated,
      fileHandle,
      io,
      ...(zip === undefined ? {} : { zip }),
      reopened: true,
      store: input.store,
      lock,
      windowPort: input.windowPort,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
  } catch (error) {
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'origin_private',
      transition: 'failed',
    })
    await lock.release()
    throw error
  }
}

/** Exports a completed retained OPFS task to the user's browser Downloads folder (works offline). */
export async function saveRetainedOPFSTask(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  env: OPFSEnvironment,
  windowPort: BrowserDownloadWindow,
): Promise<ReceiveTaskRecord> {
  if (task.target.kind !== 'opfs') {
    throw new TypeError('Only OPFS tasks can be exported from browser storage')
  }
  const lock = await acquireTaskLock(task.taskId, env.locks)
  try {
    const dir = await openTasksDirectory(env)
    const handle = await dir.getFileHandle(task.target.objectName)
    const file = await handle.getFile()
    if (task.target.sealedSize !== undefined && BigInt(file.size) !== task.target.sealedSize) {
      throw new DOMException('Retained browser file is missing or incomplete', 'NotFoundError')
    }
    const mediaType = artifactDownloadMediaType(task.artifact.kind)
    triggerBrowserDownload(windowPort, file, task.target.fileName, mediaType)
    const nextLifecycle = transitionReceiveState(task.lifecycle, {
      kind: 'download-started',
      retained: true,
      fileName: task.target.fileName,
    })
    const updated: ReceiveTaskRecord = Object.freeze({
      ...task,
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
    const blob = buildPartialZipBlob(file, files)
    const partialName = task.target.fileName.replace(/\.zip$/iu, '') + '-partial.zip'
    triggerBrowserDownload(windowPort, blob, partialName, 'application/zip')
    return blob
  } finally {
    await lock.release()
  }
}

export async function deleteOPFSTask(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  env: OPFSEnvironment,
): Promise<void> {
  const lock = await acquireTaskLock(task.taskId, env.locks)
  try {
    if (task.target.kind === 'opfs') {
      const dir = await openTasksDirectory(env).catch(() => undefined)
      await dir?.removeEntry(task.target.objectName).catch(() => undefined)
    }
    await store.deleteTask(task.taskId)
  } finally {
    await lock.release()
  }
}

class OPFSSessionHandle implements OutputSessionHandle {
  readonly taskId: string
  readonly backend = 'opfs' as const
  readonly transferOutput: TransferOutputSession
  #record: ReceiveTaskRecord
  readonly #fileHandle: FileSystemFileHandle
  readonly #io: ObjectCheckpointCoordinator
  readonly #zip: ProgressiveZipWriter | undefined
  readonly #store: ReceiveStore
  readonly #lock: TaskLock
  readonly #windowPort: BrowserDownloadWindow
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #closed = false

  constructor(input: Readonly<{
    record: ReceiveTaskRecord
    fileHandle: FileSystemFileHandle
    io: ObjectCheckpointCoordinator
    zip?: ProgressiveZipWriter
    reopened: boolean
    store: ReceiveStore
    lock: TaskLock
    windowPort: BrowserDownloadWindow
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.taskId = input.record.taskId
    this.#record = input.record
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
      markDiscoveryComplete: async () => {
        this.#record = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(this.#record)
      },
    })
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    if (result.status === 'completed') {
      return this.#settleCompleted(result)
    }
    // Single-file OPFS can only report `partial` if that file already committed before a late stop/error.
    if (result.status === 'partial' && this.#zip === undefined && result.completedFiles > 0n) {
      return this.#settleCompleted(result)
    }
    await this.#io.flush().catch(() => undefined)
    await this.#closeIO()
    return this.#settleIncomplete(result.status, result)
  }

  async close(): Promise<void> {
    await this.#closeIO()
    await this.#lock.release()
  }

  async #seal(): Promise<bigint> {
    if (this.#zip !== undefined) return this.#zip.finalize()
    await this.#io.flush()
    return this.#io.size()
  }

  async #settleCompleted(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    let sealedSize: bigint
    try {
      sealedSize = await this.#seal()
    } catch (error) {
      // The directory can fail to seal after every member is already durable and exportable.
      await this.#closeIO()
      const pauseReason = storagePauseReason(error)
      if (pauseReason !== undefined) return this.#settleIncomplete('paused', { ...result, pauseReason, error })
      return this.#settleIncomplete('failed', { ...result, error })
    }
    await this.#closeIO()

    const target: OPFSTargetRecord = Object.freeze({
      ...(this.#record.target as OPFSTargetRecord),
      allocatedLength: this.#zip?.allocatedLength ?? sealedSize,
      sealedSize,
    })

    // Try handing off immediately to browser downloads; if blocked or unavailable,
    // keep the task in `ready-to-save` so the user can export it with one click.
    let nextLifecycle = transitionReceiveState(this.#record.lifecycle, {
      kind: 'ready-to-save',
      size: sealedSize,
      fileName: target.fileName,
      contentWarning: result.contentWarning ?? null,
    })
    try {
      const file = await this.#fileHandle.getFile()
      const mediaType = artifactDownloadMediaType(this.#record.artifact.kind)
      triggerBrowserDownload(this.#windowPort, file, target.fileName, mediaType)
      nextLifecycle = transitionReceiveState(nextLifecycle, {
        kind: 'download-started',
        retained: true,
        fileName: target.fileName,
      })
      emitOutputTrace(this.#diagnostics?.trace, 'publication', {
        backend: 'origin_private',
        transition: 'committed',
      })
    } catch {
      // Browser handoff needs a fresh user gesture; `ready-to-save` preserves the result safely.
    }

    const updated: ReceiveTaskRecord = Object.freeze({
      ...this.#record,
      target,
      lifecycle: nextLifecycle,
      discoveryComplete: true,
      updatedAt: Date.now(),
    })
    this.#record = updated
    await this.#store.putTask(updated)
    return updated
  }

  async #settleIncomplete(
    status: IncompleteOPFSJobStatus,
    result: SettledTransferJobResult,
  ): Promise<ReceiveTaskRecord> {
    const files = await this.#store.listTaskFiles(this.taskId)
    let retainedBytes = 0n
    let storedCompletedFiles = 0n
    for (const file of files) {
      retainedBytes += completedTuplesBytes(file.completedRanges)
      if (file.status === 'completed' && file.zipEntry?.kind !== 'directory') {
        storedCompletedFiles += 1n
      }
    }

    const nextLifecycle = nextOPFSIncompleteLifecycle(this.#record.lifecycle, status, result, {
      isZip: this.#zip !== undefined,
      retainedBytes,
      storedCompletedFiles,
    })

    const target: OPFSTargetRecord = Object.freeze({
      ...(this.#record.target as OPFSTargetRecord),
      allocatedLength: this.#zip?.allocatedLength ?? (this.#record.target as OPFSTargetRecord).allocatedLength,
    })
    const updated: ReceiveTaskRecord = Object.freeze({
      ...this.#record,
      target,
      lifecycle: nextLifecycle,
      discoveryComplete: result.discoveryComplete || this.#record.discoveryComplete,
      ...(result.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: result.sourceRevisionFailures }),
      updatedAt: Date.now(),
    })
    this.#record = updated
    await this.#store.putTask(updated)
    emitOutputTrace(this.#diagnostics?.trace, 'settlement', {
      backend: 'origin_private',
      task_id: this.taskId,
      job_status: status,
      lifecycle: nextLifecycle.kind,
      completed_files: String(storedCompletedFiles > result.completedFiles ? storedCompletedFiles : result.completedFiles),
      failed_files: String(result.failedFiles + result.failedDirectories),
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
    return physicalSize >= existing.size ? Object.freeze({ size: existing.size }) : undefined
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
    const physicalSize = await this.#io.size()
    let validExisting: ReceiveFileRecord | undefined
    if (existing !== undefined) {
      const needed = existing.status === 'completed'
        ? existing.size
        : maxRangeEnd(existing.completedRanges)
      if (physicalSize >= needed) validExisting = existing
    }

    const persist = (status: 'receiving' | 'completed', ranges: readonly ByteRange[]) => {
      const row: ReceiveFileRecord = Object.freeze({
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
      // Coordinator flushes the SyncAccessHandle BEFORE writing `completedRanges` to IDB.
      return this.#io.checkpoint(`opfs-file-${status}`, () => this.#store.putFile(row))
    }

    return Object.freeze({
      completedRanges: validExisting === undefined ? Object.freeze([]) : fromRangeTuples(validExisting.completedRanges),
      ...(validExisting === undefined ? {} : {
        savedRevision: validExisting.revision,
        ...(validExisting.sourceMetadata === undefined ? {} : { savedSourceMetadata: validExisting.sourceMetadata }),
        savedSize: validExisting.size,
      }),
      writeAt: (offset: bigint, data: Uint8Array) => this.#io.writeAt(offset, data),
      checkpoint: (ranges: readonly ByteRange[]) => persist('receiving', ranges).then(() => 'saved' as const),
      commit: async (ranges: readonly ByteRange[]) => {
        await this.#io.truncate(request.exactSize)
        await persist('completed', ranges)
      },
      pause: (ranges: readonly ByteRange[]) => persist('receiving', ranges),
      abort: () => Promise.resolve(),
    })
  }

  async #closeIO(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    await this.#io.close().catch(() => undefined)
  }
}

type IncompleteOPFSJobStatus = Exclude<SettledTransferJobResult['status'], 'completed'>

function nextOPFSIncompleteLifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  status: IncompleteOPFSJobStatus,
  result: SettledTransferJobResult,
  input: Readonly<{
    isZip: boolean
    retainedBytes: bigint
    storedCompletedFiles: bigint
  }>,
): ReceiveTaskRecord['lifecycle'] {
  const warning = result.contentWarning ?? null
  const completedFiles = input.storedCompletedFiles > result.completedFiles
    ? input.storedCompletedFiles
    : result.completedFiles
  if (status === 'paused') {
    return transitionReceiveState(current, {
      kind: 'paused',
      reason: result.pauseReason ?? 'user',
      completedFiles,
      completedBytes: result.completedBytes,
      retainedBytes: input.retainedBytes,
      discoveredFiles: result.discoveredFiles,
      discoveredBytes: result.discoveredBytes,
      discoveryComplete: result.discoveryComplete,
      contentWarning: warning,
    })
  }

  const failedCount = result.failedFiles + result.failedDirectories
  const savedFiles = status === 'partial' ? completedFiles : input.storedCompletedFiles
  if (input.isZip && savedFiles > 0n) {
    // Keep completed OPFS ZIP members exportable via zero-copy `buildPartialZipBlob`
    // instead of sealing in place, which would turn pre-allocated failed slots into dead padding.
    const reason = status === 'stopped' || (status === 'partial' && failedCount === 0n)
      ? 'stopped'
      : 'failures'
    return transitionReceiveState(current, {
      kind: 'partial',
      reason,
      savedFiles,
      failedFiles: failedCount,
      contentWarning: warning,
    })
  }

  return failedOPFSLifecycle(current, status, result.error)
}

function failedOPFSLifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  status: Exclude<IncompleteOPFSJobStatus, 'paused'>,
  error: unknown,
): ReceiveTaskRecord['lifecycle'] {
  switch (status) {
    case 'stopped':
      return transitionReceiveState(current, { kind: 'failed', reason: 'cancelled' })
    case 'source-changed':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-changed' })
    case 'partial':
    case 'failed':
      return transitionReceiveState(current, {
        kind: 'failed',
        reason: 'error',
        ...(error instanceof Error ? { detail: error.message } : {}),
      })
  }
}
