import type { ByteRange } from '../../content/geometry'
import { catalogCapacityReason, classifyNativeError, storagePauseReason } from '../../transfer/fault'
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
import { hasZipLayout, survivingZipCoverage } from '../zip/stored-members'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../zip/progressive-zip'
import {
  artifactDownloadMediaType,
  triggerBrowserDownload,
  type BrowserDownloadWindow,
} from './browser-handoff'
import { mergeSourceOmittedEntries, resolveZipEntryPath, type OutputSessionHandle, type StartBackendTaskInput } from './contract'

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
  return openOPFSSession(input, record, target, false)
}

export function reopenOPFSSession(
  input: OPFSSessionPorts & { readonly task: ReceiveTaskRecord },
): Promise<OutputSessionHandle> {
  if (input.task.target.kind !== 'opfs') {
    return Promise.reject(new TypeError('Cannot reopen a non-OPFS task with the OPFS backend'))
  }
  return openOPFSSession(input, input.task, input.task.target, true)
}

async function openOPFSSession(
  ports: OPFSSessionPorts, task: ReceiveTaskRecord, target: OPFSTargetRecord, reopened: boolean,
): Promise<OutputSessionHandle> {
  let lock: TaskLock | undefined
  let io: ObjectCheckpointCoordinator | undefined
  let directory: FileSystemDirectoryHandle | undefined
  let fileHandle: FileSystemFileHandle | undefined
  try {
    lock = await acquireTaskLock(task.taskId, ports.env.locks)
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
      try {
        if (await ports.store.getTask(task.taskId) === undefined) await removePrivateObject(directory, target.objectName)
      } catch { /* Preserve the opening error if storage itself is unavailable. */ }
    }
    await lock?.release()
    emitOutputTrace(ports.diagnostics?.trace, reopened ? 'reopen' : 'output_reservation', {
      backend: 'origin_private', task_id: task.taskId, transition: 'failed', error_class: classifyNativeError(error),
    })
    throw error
  }
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
    const dir = await openTasksDirectory(env)
    const handle = await dir.getFileHandle(task.target.objectName)
    const file = await handle.getFile()
    if (BigInt(file.size) !== task.target.sealedSize) {
      throw new DOMException('Retained browser file is missing or incomplete', 'NotFoundError')
    }
    const mediaType = artifactDownloadMediaType(task.artifact.kind)
    signal?.throwIfAborted()
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

  constructor(input: Readonly<{
    record: ReceiveTaskRecord
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
      markDiscoveryComplete: async () => {
        const updated = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(updated)
        this.#record = updated
      },
    })
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    if (result.status === 'completed' && this.#zip !== undefined) {
      return this.#settleCompletedZip(result)
    }

    const { files, truncatedLength } = await this.#closeIncompleteSession(result.status)
    if (this.#zip === undefined && result.status !== 'paused') {
      // The file commit already truncated and flushed. Its durable row also survives a late stop.
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
      const pauseReason = storagePauseReason(error)
      if (pauseReason !== undefined) return this.#settleIncomplete('paused', { ...result, pauseReason, error })
      return this.#settleIncomplete('failed', { ...result, error })
    }
    await this.#closeIO()
    return this.#publishCompleted(size, result)
  }

  async #closeIncompleteSession(
    status: SettledTransferJobResult['status'],
  ): Promise<Readonly<{ files: readonly ReceiveFileRecord[]; truncatedLength?: bigint }>> {
    if (this.#zip === undefined || status === 'paused' || status === 'completed') {
      await this.#closeIO()
      return { files: await this.#store.listTaskFiles(this.taskId) }
    }
    try {
      const files = await this.#store.listTaskFiles(this.taskId)
      const truncatedLength = hasCompletedZipFile(files) ? await this.#truncateUnusedZipTail(files) : undefined
      return truncatedLength === undefined ? { files } : { files, truncatedLength }
    } finally {
      await this.#closeIO()
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
    const target: OPFSTargetRecord = Object.freeze({
      ...(this.#record.target as OPFSTargetRecord),
      allocatedLength: this.#zip?.allocatedLength ?? sealedSize,
      sealedSize,
    })

    let nextLifecycle = transitionReceiveState(this.#record.lifecycle, {
      kind: 'ready-to-save',
      size: sealedSize,
      fileName: target.fileName,
      contentWarning: result.contentWarning ?? null,
    })
    const { sourceOmittedEntries: previousOmitted, ...baseRecord } = this.#record
    const sourceOmittedEntries = mergeSourceOmittedEntries(
      result.sourceOmittedEntries,
      previousOmitted,
      result.discoveryComplete,
    )
    // A late stop can leave a complete file for the user to save at their discretion.
    if (result.status === 'completed' && file !== undefined) {
      try {
        const mediaType = artifactDownloadMediaType(this.#record.artifact.kind)
        triggerBrowserDownload(this.#windowPort, file, target.fileName, mediaType)
        nextLifecycle = transitionReceiveState(nextLifecycle, {
          kind: 'download-started', retained: true, fileName: target.fileName,
        })
        emitOutputTrace(this.#diagnostics?.trace, 'publication', {
          backend: 'origin_private', task_id: this.taskId, transition: 'committed',
          ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
        })
      } catch { /* Downloads can retry a handoff that needs a fresh user gesture. */ }
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

    const target: OPFSTargetRecord = Object.freeze({
      ...(this.#record.target as OPFSTargetRecord),
      allocatedLength: truncatedLength ?? this.#zip?.allocatedLength ?? (this.#record.target as OPFSTargetRecord).allocatedLength,
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

  #closeIO(): Promise<void> {
    this.#closingIO ??= this.#io.close().catch(() => undefined)
    return this.#closingIO
  }
}

function hasCompletedZipFile(files: readonly ReceiveFileRecord[]): boolean {
  return files.some(file => file.status === 'completed' && file.zipEntry?.kind !== 'directory')
}

function completedZipTailOffset(files: readonly ReceiveFileRecord[]): bigint {
  let tail = 0n
  for (const file of files) {
    if (file.status !== 'completed' || !hasZipLayout(file)) continue
    // Empty files and directories regenerate headers without reading bytes from the OPFS object.
    if (file.zipEntry.kind !== 'file' || file.size === 0n) continue
    if (file.zipEntry.endOffset > tail) tail = file.zipEntry.endOffset
  }
  return tail
}

function survivingFileCoverage(row: ReceiveFileRecord, physicalSize: bigint): ReceiveFileRecord {
  if (row.status === 'completed' ? physicalSize === row.size : maxRangeEnd(row.completedRanges) <= physicalSize) return row
  const completedRanges = row.completedRanges.flatMap(([start, end]) =>
    start < physicalSize ? [[start, end < physicalSize ? end : physicalSize] as const] : [])
  return { ...row, status: 'receiving', completedRanges }
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
    storedCompletedBytes: bigint
  }>,
): ReceiveTaskRecord['lifecycle'] {
  const warning = result.contentWarning ?? null
  if (status === 'paused') {
    return transitionReceiveState(current, {
      kind: 'paused',
      reason: result.pauseReason ?? 'user',
      completedFiles: input.storedCompletedFiles,
      completedBytes: input.storedCompletedBytes,
      retainedBytes: input.retainedBytes,
      discoveredFiles: result.discoveredFiles,
      discoveredBytes: result.discoveredBytes,
      discoveryComplete: result.discoveryComplete,
      contentWarning: warning,
    })
  }

  const failedCount = result.failedFiles + result.failedDirectories
  const savedFiles = input.storedCompletedFiles
  if (input.isZip && savedFiles > 0n) {
    // Keep completed OPFS ZIP members exportable via zero-copy `buildPartialZipBlob`
    // instead of sealing in place, which would turn pre-allocated failed slots into dead padding.
    const reason = catalogCapacityReason(result.error) ?? (
      status === 'stopped' || (status === 'partial' && failedCount === 0n)
        ? 'stopped'
        : 'failures'
    )
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
    case 'failed': {
      const reason = catalogCapacityReason(error) ?? 'error'
      return transitionReceiveState(current, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && error instanceof Error ? { detail: error.message } : {}),
      })
    }
  }
}
