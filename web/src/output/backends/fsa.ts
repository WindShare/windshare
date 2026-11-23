import type { ByteRange } from '../../content/geometry'
import { bigintToSafeNumber } from '../../content/geometry'
import {
  FaultScope,
  OutputFaultCode,
  TransferFaultError,
  isPermissionDeniedError,
  outputFault,
} from '../../transfer/fault'
import type { TransferJobResult } from '../../transfer/job'
import type {
  TransferDirectoryRequest,
  TransferFileRequest,
  TransferOutputFile,
  TransferOutputSession,
} from '../../transfer/output-port'
import { transitionReceiveState } from '../../transfer/state'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import { acquireTaskLock, type TaskLock } from '../store/tab-lock'
import {
  completedTuplesBytes,
  fileRecordId,
  fromRangeTuples,
  maxRangeEnd,
  toRangeTuples,
  type FSATargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import type { BrowserOutputWindow, OutputSessionHandle, StartBackendTaskInput } from './contract'
import {
  cleanupFSATaskEntries,
  pruneEmptySubdirectories,
  readFSADiskFingerprint,
  removeUncommittedEmptyPlaceholder,
} from './fsa-cleanup'
import { SafeNameRegistry, decideCollisionName, deterministicSafeComponent } from './safe-name'

const FSA_MAX_CONCURRENT_FILES = 4
const MAX_COLLISION_ATTEMPTS = 1_000

/**
 * Starts the OS directory picker synchronously inside the user's click handler.
 * Never await anything before calling this function.
 */
export function startFSADirectoryPicker(
  windowPort: BrowserOutputWindow,
): Promise<FileSystemDirectoryHandle> {
  if (typeof windowPort.showDirectoryPicker !== 'function') {
    throw new DOMException('Folder picker is unavailable in this browser', 'NotSupportedError')
  }
  return windowPort.showDirectoryPicker({ mode: 'readwrite' })
}

type PermissionHandle = FileSystemDirectoryHandle & {
  queryPermission?: (desc: { mode: 'read' | 'readwrite' }) => Promise<'granted' | 'denied' | 'prompt'>
  requestPermission?: (desc: { mode: 'read' | 'readwrite' }) => Promise<'granted' | 'denied' | 'prompt'>
}

export async function ensureFSAPermission(
  handle: FileSystemDirectoryHandle,
  allowPrompt: boolean,
): Promise<'granted' | 'prompt' | 'denied'> {
  const permHandle = handle as PermissionHandle
  if (typeof permHandle.queryPermission !== 'function') return 'granted'
  const current = await permHandle.queryPermission({ mode: 'readwrite' })
  if (current === 'granted') return 'granted'
  if (!allowPrompt || typeof permHandle.requestPermission !== 'function') return current
  return permHandle.requestPermission({ mode: 'readwrite' })
}

export async function openNewFSASession(input: Readonly<{
  task: StartBackendTaskInput
  parentHandle: FileSystemDirectoryHandle
  store: ReceiveStore
  locks?: LockManager
  diagnostics?: OutputDiagnosticsPorts
}>): Promise<OutputSessionHandle> {
  const lock = await acquireTaskLock(input.task.taskId, input.locks)
  try {
    const singleFile = input.task.artifact.singleFileId !== undefined
    const safeNames = new SafeNameRegistry()
    const { reservedName, collisionIndex, rootHandle } = await reserveFSATarget(
      input.parentHandle,
      input.task.artifact.name,
      singleFile,
      safeNames,
    )
    const target: FSATargetRecord = Object.freeze({
      kind: 'fsa',
      parentHandle: input.parentHandle,
      reservedName,
      collisionIndex,
      singleFile,
    })
    const display = input.task.display === undefined
      ? undefined
      : Object.freeze({ ...input.task.display, destinationLabel: input.parentHandle.name })
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
      backend: 'fsa',
      lifecycle: downloading,
      target,
      ...(display === undefined ? {} : { display }),
      discoveryComplete: false,
      updatedAt: Date.now(),
    })
    await input.store.putTask(record)
    emitOutputTrace(input.diagnostics?.trace, 'output_reservation', {
      backend: 'file_system_access',
      transition: 'acquired',
    })
    return new FSASessionHandle(record, rootHandle, input.store, lock, safeNames, input.diagnostics)
  } catch (error) {
    await lock.release()
    throw error
  }
}

export async function reopenFSASession(input: Readonly<{
  task: ReceiveTaskRecord
  store: ReceiveStore
  locks?: LockManager
  allowPermissionPrompt: boolean
  diagnostics?: OutputDiagnosticsPorts
}>): Promise<OutputSessionHandle> {
  if (input.task.target.kind !== 'fsa') {
    throw new TypeError('Cannot reopen a non-FSA task with the FSA backend')
  }
  let lock: TaskLock
  try {
    lock = await acquireTaskLock(input.task.taskId, input.locks)
  } catch (error) {
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'file_system_access',
      transition: 'failed',
    })
    throw error
  }
  try {
    const perm = await ensureFSAPermission(input.task.target.parentHandle, input.allowPermissionPrompt)
    if (perm !== 'granted') {
      throw new TransferFaultError(
        outputFault(FaultScope.Job, OutputFaultCode.PermissionDenied),
        'Folder permission is required to continue saving',
      )
    }
    const target = input.task.target
    const rootHandle = target.singleFile
      ? target.parentHandle
      : await target.parentHandle.getDirectoryHandle(target.reservedName, { create: true })
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
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'file_system_access',
      transition: 'authorized',
    })
    return new FSASessionHandle(updated, rootHandle, input.store, lock, new SafeNameRegistry(), input.diagnostics)
  } catch (error) {
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'file_system_access',
      transition: 'failed',
    })
    await lock.release()
    throw error
  }
}

export async function discardFSATask(
  task: ReceiveTaskRecord,
  store: ReceiveStore,
  locks?: LockManager,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  if (task.target.kind !== 'fsa') {
    await store.deleteTask(task.taskId)
    return
  }
  const lock = await acquireTaskLock(task.taskId, locks)
  try {
    const files = await store.listTaskFiles(task.taskId)
    const perm = await ensureFSAPermission(task.target.parentHandle, true).catch(() => 'denied' as const)
    if (perm === 'granted') {
      await cleanupFSATaskEntries(task.taskId, task.target, files, diagnostics)
    }
    await store.deleteTask(task.taskId)
  } finally {
    await lock.release()
  }
}

class FSASessionHandle implements OutputSessionHandle {
  readonly taskId: string
  readonly backend = 'fsa' as const
  readonly transferOutput: TransferOutputSession
  #record: ReceiveTaskRecord
  readonly #rootHandle: FileSystemDirectoryHandle
  readonly #store: ReceiveStore
  readonly #lock: TaskLock
  readonly #safeNames: SafeNameRegistry
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #dirCache = new Map<string, FileSystemDirectoryHandle>()

  constructor(
    record: ReceiveTaskRecord,
    rootHandle: FileSystemDirectoryHandle,
    store: ReceiveStore,
    lock: TaskLock,
    safeNames: SafeNameRegistry,
    diagnostics?: OutputDiagnosticsPorts,
  ) {
    this.taskId = record.taskId
    this.#record = record
    this.#rootHandle = rootHandle
    this.#store = store
    this.#lock = lock
    this.#safeNames = safeNames
    this.#diagnostics = diagnostics
    this.transferOutput = Object.freeze({
      sessionId: record.taskId,
      capabilities: Object.freeze({
        // FileSystemWritableFileStream has no in-place flush; reopening with
        // keepExistingData copies the full file prefix on non-CoW filesystems,
        // so FSA commits on pause, lease retry, and completion instead.
        maxConcurrentFiles: FSA_MAX_CONCURRENT_FILES,
      }),
      ensureDirectory: (request: TransferDirectoryRequest, signal: AbortSignal) => this.#ensureDirectory(request, signal),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
      markDiscoveryComplete: async () => {
        this.#record = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(this.#record)
      },
      safeRenamedSummary: () => this.#safeNames.summary(),
    })
  }

  async settle(result: TransferJobResult): Promise<ReceiveTaskRecord> {
    if (result.status === 'stopped' && this.#record.target.kind === 'fsa' && !this.#record.target.singleFile) {
      await pruneEmptySubdirectories(this.#rootHandle, [...this.#dirCache.values()]).catch(() => undefined)
    }
    const files = await this.#store.listTaskFiles(this.taskId)
    let retainedBytes = 0n
    for (const file of files) retainedBytes += completedTuplesBytes(file.completedRanges)

    const nextLifecycle = nextFSALifecycle(this.#record.lifecycle, result, retainedBytes)
    const safeRenamed = this.#safeNames.summary() ?? this.#record.safeRenamed
    const nextRecord: ReceiveTaskRecord = Object.freeze({
      ...this.#record,
      lifecycle: nextLifecycle,
      discoveryComplete: result.discoveryComplete || this.#record.discoveryComplete,
      ...(safeRenamed === undefined ? {} : { safeRenamed }),
      ...(result.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: result.sourceRevisionFailures }),
      updatedAt: Date.now(),
    })
    this.#record = nextRecord
    await this.#store.putTask(nextRecord)
    return nextRecord
  }

  async close(): Promise<void> {
    await this.#lock.release()
  }

  async #ensureDirectory(request: TransferDirectoryRequest, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await this.#resolveDirectory(request.path, signal)
  }

  async #openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> {
    signal.throwIfAborted()
    const existing = await this.#store.getFile(this.taskId, request.fileId)
    const verified = existing === undefined ? undefined : await verifyExistingFSAFile(existing)
    if (verified !== undefined && verified.status === 'completed') {
      return completedFSAFile(verified)
    }

    const parentPath = request.path.slice(0, -1)
    const leafName = this.#record.target.kind === 'fsa' && this.#record.target.singleFile
      ? this.#record.target.reservedName
      : request.path.at(-1)!
    const parentDir = parentPath.length === 0
      ? this.#rootHandle
      : await this.#resolveDirectory(parentPath, signal)

    const openedAtMs = Date.now()
    const { handle, physicalName } = verified?.handle !== undefined
      ? { handle: verified.handle, physicalName: verified.physicalName ?? verified.handle.name }
      : await createOrOpenFSAFile(parentDir, request.path, leafName, this.#safeNames)

    const initialRanges = verified === undefined ? [] : fromRangeTuples(verified.completedRanges)
    return new FSAOutputFile({
      taskId: this.taskId,
      request,
      parentDir,
      handle,
      physicalName,
      openedAtMs,
      freshUnpersisted: existing === undefined,
      store: this.#store,
      initialRanges,
      ...(verified?.revision === undefined ? {} : { savedRevision: verified.revision }),
      ...(verified?.sourceMetadata === undefined ? {} : { savedSourceMetadata: verified.sourceMetadata }),
      ...(verified?.size === undefined ? {} : { savedSize: verified.size }),
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
  }

  async #resolveDirectory(path: readonly string[], signal: AbortSignal): Promise<FileSystemDirectoryHandle> {
    let current = this.#rootHandle
    const prefix: string[] = []
    for (const segment of path) {
      signal.throwIfAborted()
      prefix.push(segment)
      const cacheKey = JSON.stringify(prefix)
      const cached = this.#dirCache.get(cacheKey)
      if (cached !== undefined) {
        current = cached
        continue
      }
      const existingName = this.#safeNames.physicalComponent(prefix, 'directory')
      let next: FileSystemDirectoryHandle
      try {
        next = await current.getDirectoryHandle(existingName ?? segment, { create: true })
      } catch (error) {
        if (isPermissionDeniedError(error)) throw error
        const safeName = await deterministicSafeComponent({ logicalPath: prefix, entryKind: 'directory' })
        this.#safeNames.record(prefix, 'directory', safeName)
        next = await current.getDirectoryHandle(safeName, { create: true })
      }
      this.#dirCache.set(cacheKey, next)
      current = next
    }
    return current
  }
}

class FSAOutputFile implements TransferOutputFile {
  readonly completedRanges: readonly ByteRange[]
  readonly alreadyCompleted = false
  readonly savedRevision?: string
  readonly savedSourceMetadata?: string
  readonly savedSize?: bigint
  readonly #taskId: string
  readonly #request: TransferFileRequest
  readonly #parentDir: FileSystemDirectoryHandle
  readonly #handle: FileSystemFileHandle
  readonly #physicalName: string
  readonly #openedAtMs: number
  readonly #store: ReceiveStore
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #freshUnpersisted: boolean
  #writer: FileSystemWritableFileStream | undefined
  #writerDirty = false
  #hasCommittedData: boolean

  constructor(input: Readonly<{
    taskId: string
    request: TransferFileRequest
    parentDir: FileSystemDirectoryHandle
    handle: FileSystemFileHandle
    physicalName: string
    openedAtMs: number
    freshUnpersisted: boolean
    store: ReceiveStore
    initialRanges: readonly ByteRange[]
    savedRevision?: string
    savedSourceMetadata?: string
    savedSize?: bigint
    diagnostics?: OutputDiagnosticsPorts
  }>) {
    this.#taskId = input.taskId
    this.#request = input.request
    this.#parentDir = input.parentDir
    this.#handle = input.handle
    this.#physicalName = input.physicalName
    this.#openedAtMs = input.openedAtMs
    this.#freshUnpersisted = input.freshUnpersisted
    this.#store = input.store
    this.#diagnostics = input.diagnostics
    this.completedRanges = input.initialRanges
    this.#hasCommittedData = input.initialRanges.length > 0
    if (input.savedRevision !== undefined) this.savedRevision = input.savedRevision
    if (input.savedSourceMetadata !== undefined) this.savedSourceMetadata = input.savedSourceMetadata
    if (input.savedSize !== undefined) this.savedSize = input.savedSize
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    const writer = await this.#ensureWriter()
    await writer.write({
      type: 'write',
      position: bigintToSafeNumber(offset, 'FSA write offset'),
      data: data.slice(),
    })
    this.#writerDirty = true
  }

  async checkpoint(ranges: readonly ByteRange[]): Promise<void> {
    // Flush physical bytes to disk BEFORE recording ranges in IndexedDB.
    await this.#closeWriter()
    this.#freshUnpersisted = false
    await this.#persist('receiving', ranges)
  }

  async commit(ranges: readonly ByteRange[]): Promise<void> {
    await this.#closeWriter()
    this.#freshUnpersisted = false
    await this.#persist('completed', ranges)
    emitOutputTrace(this.#diagnostics?.trace, 'output_write', {
      backend: 'file_system_access',
      transition: 'transaction_committed',
    })
  }

  async pause(ranges: readonly ByteRange[]): Promise<void> {
    return this.checkpoint(ranges)
  }

  async abort(reason?: unknown): Promise<void> {
    const writer = this.#writer
    this.#writer = undefined
    this.#writerDirty = false
    if (writer !== undefined) {
      await writer.abort(reason).catch(() => undefined)
    }
    if (this.#freshUnpersisted) {
      this.#freshUnpersisted = false
      await removeUncommittedEmptyPlaceholder(
        this.#parentDir,
        this.#physicalName,
        this.#handle,
        this.#openedAtMs,
      )
    }
  }

  async #ensureWriter(): Promise<FileSystemWritableFileStream> {
    if (this.#writer !== undefined) return this.#writer
    this.#writer = await this.#handle.createWritable({ keepExistingData: this.#hasCommittedData })
    return this.#writer
  }

  async #closeWriter(): Promise<void> {
    const writer = this.#writer
    const wroteBytes = this.#writerDirty
    this.#writer = undefined
    this.#writerDirty = false
    if (writer !== undefined) {
      await writer.close()
      if (wroteBytes) this.#hasCommittedData = true
    }
  }

  async #persist(status: 'receiving' | 'completed', ranges: readonly ByteRange[]): Promise<void> {
    // Capture local disk identity only for unfinished writes so discard can reject
    // user-edited or swapped entries without adding stat calls to completed files.
    const fsaDiskFingerprint = status === 'receiving'
      ? await readFSADiskFingerprint(this.#handle)
      : undefined
    const row: ReceiveFileRecord = Object.freeze({
      id: fileRecordId(this.#taskId, this.#request.fileId),
      taskId: this.#taskId,
      fileId: this.#request.fileId,
      revision: this.#request.revision,
      ...(this.#request.sourceMetadata === undefined ? {} : { sourceMetadata: this.#request.sourceMetadata }),
      path: this.#request.path,
      physicalName: this.#physicalName,
      size: this.#request.exactSize,
      completedRanges: toRangeTuples(ranges),
      status,
      handle: this.#handle,
      ...(fsaDiskFingerprint === undefined ? {} : { fsaDiskFingerprint }),
    })
    await this.#store.putFile(row)
  }
}

function completedFSAFile(record: ReceiveFileRecord): TransferOutputFile {
  return Object.freeze({
    completedRanges: fromRangeTuples(record.completedRanges),
    alreadyCompleted: true,
    savedRevision: record.revision,
    ...(record.sourceMetadata === undefined ? {} : { savedSourceMetadata: record.sourceMetadata }),
    savedSize: record.size,
    writeAt: () => Promise.resolve(),
    checkpoint: () => Promise.resolve(),
    commit: () => Promise.resolve(),
    pause: () => Promise.resolve(),
    abort: () => Promise.resolve(),
  })
}

/**
 * Lightweight physical verification on resume: ensures the FSA file handle still resolves
 * and its physical size covers every claimed completed range. Never relies on unverified IDB state.
 */
async function verifyExistingFSAFile(record: ReceiveFileRecord): Promise<ReceiveFileRecord | undefined> {
  if (record.handle === undefined) return undefined
  try {
    const file = await record.handle.getFile()
    const physicalSize = BigInt(file.size)
    if (record.status === 'completed') {
      return physicalSize === record.size ? record : undefined
    }
    const needed = maxRangeEnd(record.completedRanges)
    if (physicalSize < needed) return undefined
    return record
  } catch {
    return undefined
  }
}

async function createOrOpenFSAFile(
  parentDir: FileSystemDirectoryHandle,
  logicalPath: readonly string[],
  leafName: string,
  safeNames: SafeNameRegistry,
): Promise<{ readonly handle: FileSystemFileHandle; readonly physicalName: string }> {
  try {
    const handle = await parentDir.getFileHandle(leafName, { create: true })
    return { handle, physicalName: leafName }
  } catch (error) {
    if (isPermissionDeniedError(error)) throw error
    const safeName = await deterministicSafeComponent({ logicalPath, entryKind: 'file' })
    safeNames.record(logicalPath, 'file', safeName)
    const handle = await parentDir.getFileHandle(safeName, { create: true })
    return { handle, physicalName: safeName }
  }
}

async function reserveFSATarget(
  parentHandle: FileSystemDirectoryHandle,
  requestedName: string,
  singleFile: boolean,
  safeNames: SafeNameRegistry,
): Promise<{
  readonly reservedName: string
  readonly collisionIndex: number
  readonly rootHandle: FileSystemDirectoryHandle
}> {
  const kind = singleFile ? 'file' : 'directory'
  for (let index = 0; index < MAX_COLLISION_ATTEMPTS; index++) {
    const candidate = decideCollisionName(requestedName, kind, index)
    const exists = await entryExists(parentHandle, candidate)
    if (exists === 'exists') continue
    if (exists === 'invalid-name') {
      const safe = await tryReserveSafeName(parentHandle, requestedName, kind, singleFile, index, safeNames)
      if (safe !== undefined) return safe
      continue
    }
    const created = await tryCreateTargetRoot(parentHandle, requestedName, candidate, kind, singleFile, index, safeNames)
    return created
  }
  throw new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
    'Could not allocate a unique name in the selected folder',
  )
}

async function tryReserveSafeName(
  parentHandle: FileSystemDirectoryHandle,
  requestedName: string,
  kind: 'file' | 'directory',
  singleFile: boolean,
  collisionIndex: number,
  safeNames: SafeNameRegistry,
) {
  const safeName = await deterministicSafeComponent({
    logicalPath: [requestedName],
    entryKind: kind,
    collisionIndex,
  })
  if (await entryExists(parentHandle, safeName) === 'exists') return undefined
  safeNames.record([requestedName], kind, safeName)
  const rootHandle = singleFile
    ? parentHandle
    : await parentHandle.getDirectoryHandle(safeName, { create: true })
  return { reservedName: safeName, collisionIndex, rootHandle }
}

async function tryCreateTargetRoot(
  parentHandle: FileSystemDirectoryHandle,
  requestedName: string,
  candidate: string,
  kind: 'file' | 'directory',
  singleFile: boolean,
  collisionIndex: number,
  safeNames: SafeNameRegistry,
) {
  try {
    const rootHandle = singleFile
      ? parentHandle
      : await parentHandle.getDirectoryHandle(candidate, { create: true })
    return { reservedName: candidate, collisionIndex, rootHandle }
  } catch (error) {
    if (isPermissionDeniedError(error)) throw error
    const safeName = await deterministicSafeComponent({
      logicalPath: [requestedName],
      entryKind: kind,
      collisionIndex,
    })
    safeNames.record([requestedName], kind, safeName)
    const rootHandle = singleFile
      ? parentHandle
      : await parentHandle.getDirectoryHandle(safeName, { create: true })
    return { reservedName: safeName, collisionIndex, rootHandle }
  }
}

async function entryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<'missing' | 'exists' | 'invalid-name'> {
  const fileProbe = await probeHandle(() => parent.getFileHandle(name))
  if (fileProbe !== 'missing') return fileProbe
  return probeHandle(() => parent.getDirectoryHandle(name))
}

async function probeHandle(open: () => Promise<unknown>): Promise<'missing' | 'exists' | 'invalid-name'> {
  try {
    await open()
    return 'exists'
  } catch (error) {
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    if (error instanceof TypeError) return 'invalid-name'
    if (isPermissionDeniedError(error)) throw error
    return 'missing'
  }
}

function nextFSALifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  result: TransferJobResult,
  retainedBytes: bigint,
): ReceiveTaskRecord['lifecycle'] {
  const warning = result.contentWarning ?? null
  switch (result.status) {
    case 'completed':
      return transitionReceiveState(current, {
        kind: 'saved',
        savedFiles: result.completedFiles,
        savedBytes: result.completedBytes,
        contentWarning: warning,
      })
    case 'paused':
      return transitionReceiveState(current, {
        kind: 'paused',
        reason: result.pauseReason ?? 'user',
        completedFiles: result.completedFiles,
        completedBytes: result.completedBytes,
        retainedBytes,
        discoveredFiles: result.discoveredFiles,
        discoveredBytes: result.discoveredBytes,
        discoveryComplete: result.discoveryComplete,
        contentWarning: warning,
      })
    case 'partial':
      return transitionReceiveState(current, {
        kind: 'partial',
        reason: result.failedFiles > 0n || result.failedDirectories > 0n ? 'failures' : 'stopped',
        savedFiles: result.completedFiles,
        failedFiles: result.failedFiles + result.failedDirectories,
        contentWarning: warning,
      })
    case 'stopped':
      return transitionReceiveState(current, { kind: 'failed', reason: 'cancelled' })
    case 'source-changed':
      return transitionReceiveState(current, { kind: 'failed', reason: 'source-changed' })
    case 'failed':
      return transitionReceiveState(current, {
        kind: 'failed',
        reason: 'error',
        ...(result.error instanceof Error ? { detail: result.error.message } : {}),
      })
  }
}
