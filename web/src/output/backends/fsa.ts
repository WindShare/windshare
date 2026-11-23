import {
  FaultScope,
  OutputFaultCode,
  TransferFaultError,
  outputFault,
} from '../../transfer/fault'
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
import type { ReceiveStore } from '../store/database'
import { acquireTaskLock, type TaskLock } from '../store/tab-lock'
import {
  completedTuplesBytes, fileRecordId,
  type FSATargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import type { BrowserOutputWindow, OutputSessionHandle, StartBackendTaskInput } from './contract'
import { cleanupFSATaskEntries, isMissingFSAEntry, sameFSAEntry } from './fsa-cleanup'
import { destinationChanged, FSAOutputFile, verifyExistingFSAFile } from './fsa-file'
import { SafeNameRegistry, decideCollisionName, deterministicSafeComponent } from './safe-name'

type FSATaskRecord = ReceiveTaskRecord & { readonly target: FSATargetRecord }

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
  signal?: AbortSignal,
): Promise<'granted' | 'prompt' | 'denied'> {
  signal?.throwIfAborted()
  const permHandle = handle as PermissionHandle
  if (typeof permHandle.queryPermission !== 'function') return 'granted'
  const current = await permHandle.queryPermission({ mode: 'readwrite' })
  signal?.throwIfAborted()
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
    const record: FSATaskRecord = Object.freeze({
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
  signal?: AbortSignal
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
    const perm = await ensureFSAPermission(input.task.target.parentHandle, input.allowPermissionPrompt, input.signal)
    input.signal?.throwIfAborted()
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
    const updated: FSATaskRecord = Object.freeze({
      ...input.task, target,
      lifecycle: downloading,
      updatedAt: Date.now(),
    })
    const priorFiles = await input.store.listTaskFiles(updated.taskId)
    input.signal?.throwIfAborted()
    await input.store.putTask(updated)
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'file_system_access',
      transition: 'authorized',
    })
    return new FSASessionHandle(
      updated,
      rootHandle,
      input.store,
      lock,
      new SafeNameRegistry(),
      input.diagnostics,
      priorFiles,
    )
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
  signal?: AbortSignal,
): Promise<void> {
  const lock = await acquireTaskLock(task.taskId, locks)
  let removing = false
  const beginRemoval = () => {
    if (!removing) signal?.throwIfAborted()
    removing = true
  }
  try {
    signal?.throwIfAborted()
    const current = await store.getTask(task.taskId)
    if (current === undefined) return
    if (current.target.kind === 'fsa') {
      const files = await store.listTaskFiles(task.taskId)
      const unfinished = files.some(file => file.status !== 'completed')
      if (unfinished || (current.lifecycle.kind === 'paused' && !current.target.singleFile && files.length === 0)) {
        const permission = await ensureFSAPermission(current.target.parentHandle, unfinished, signal)
        signal?.throwIfAborted()
        if (permission === 'granted') {
          await cleanupFSATaskEntries(task.taskId, current.target, files, { store, diagnostics, beginRemoval })
        } else if (unfinished) {
          throw new TransferFaultError(outputFault(FaultScope.Job, OutputFaultCode.PermissionDenied),
            'Allow folder access to discard the unfinished files.')
        }
      }
    }
    beginRemoval()
    await store.deleteTask(task.taskId)
  } finally {
    await lock.release()
  }
}

interface FSADirectory {
  readonly handle: FileSystemDirectoryHandle
  readonly physicalPath: readonly string[]
  readonly created: boolean
}

/** While the task lock is held, unfinished rows can be reused directly after each successful IndexedDB transaction. */
class FSAFileRecords {
  readonly completedAtOpen = new Map<string, ReceiveFileRecord>()
  readonly #unfinished = new Map<string, ReceiveFileRecord>()
  readonly #store: ReceiveStore

  constructor(store: ReceiveStore, prior: readonly ReceiveFileRecord[]) {
    this.#store = store
    for (const file of prior) {
      if (file.status === 'completed') this.completedAtOpen.set(file.fileId, file)
      else this.#unfinished.set(file.id, file)
    }
  }

  find(taskId: string, fileId: string): ReceiveFileRecord | undefined {
    return this.#unfinished.get(fileRecordId(taskId, fileId)) ?? this.completedAtOpen.get(fileId)
  }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    await this.#store.putFile(record)
    if (record.status === 'completed') this.#unfinished.delete(record.id)
    else this.#unfinished.set(record.id, record)
  }

  async deleteFiles(ids: readonly string[]): Promise<void> {
    await this.#store.deleteFiles(ids)
    for (const id of ids) this.#unfinished.delete(id)
  }
}

class FSASessionHandle implements OutputSessionHandle {
  readonly taskId: string
  readonly backend = 'fsa' as const
  readonly transferOutput: TransferOutputSession
  #record: FSATaskRecord
  readonly #rootHandle: FileSystemDirectoryHandle
  readonly #store: ReceiveStore
  readonly #lock: TaskLock
  readonly #safeNames: SafeNameRegistry
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #files: FSAFileRecords
  readonly #dirCache = new Map<string, Promise<FSADirectory>>()
  readonly #createdDirectories = new Set<FileSystemDirectoryHandle>()
  readonly #fileDirectories = new Set<FileSystemDirectoryHandle>()

  constructor(
    record: FSATaskRecord,
    rootHandle: FileSystemDirectoryHandle,
    store: ReceiveStore,
    lock: TaskLock,
    safeNames: SafeNameRegistry,
    diagnostics?: OutputDiagnosticsPorts,
    priorFiles: readonly ReceiveFileRecord[] = [],
  ) {
    this.taskId = record.taskId
    this.#record = record
    this.#rootHandle = rootHandle
    this.#store = store
    this.#lock = lock
    this.#safeNames = safeNames
    this.#diagnostics = diagnostics
    this.#files = new FSAFileRecords(store, priorFiles)
    this.transferOutput = Object.freeze({
      sessionId: record.taskId,
      capabilities: Object.freeze({
        // FileSystemWritableFileStream has no in-place flush; reopening with
        // keepExistingData copies the full file prefix on non-CoW filesystems,
        // so only explicit cuts and sufficiently large outage checkpoints justify reopening.
        maxConcurrentFiles: FSA_MAX_CONCURRENT_FILES,
        resumable: true,
      }),
      ensureDirectory: (request: TransferDirectoryRequest, signal: AbortSignal) => this.#ensureDirectory(request, signal),
      ...(this.#files.completedAtOpen.size > 0
        ? { checkCompleted: (fileId: string, signal: AbortSignal) => this.#checkCompleted(fileId, signal) }
        : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
      markDiscoveryComplete: async () => {
        const record = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(record)
        this.#record = record
      },
      safeRenamedSummary: () => this.#safeNames.summary(),
    })
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    const files = await this.#store.listTaskFiles(this.taskId)
    let retainedBytes = 0n
    for (const file of files) retainedBytes += completedTuplesBytes(file.completedRanges)

    const nextLifecycle = nextFSALifecycle(this.#record.lifecycle, result, retainedBytes)
    const safeRenamed = this.#safeNames.summary() ?? this.#record.safeRenamed
    const nextRecord: FSATaskRecord = Object.freeze({
      ...this.#record,
      lifecycle: nextLifecycle,
      discoveryComplete: result.discoveryComplete || this.#record.discoveryComplete,
      ...(safeRenamed === undefined ? {} : { safeRenamed }),
      ...(result.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: result.sourceRevisionFailures }),
      updatedAt: Date.now(),
    })
    await this.#store.putTask(nextRecord)
    this.#record = nextRecord
    if (nextLifecycle.kind === 'failed' || nextLifecycle.kind === 'partial') {
      // An explicitly selected empty folder is part of a partial result. During cancellation, remove task-created scaffolding.
      const directories = result.status === 'stopped' ? [...this.#createdDirectories] : [...this.#fileDirectories]
      await cleanupFSATaskEntries(this.taskId, nextRecord.target, files, {
        store: this.#files, diagnostics: this.#diagnostics, directories,
      }).catch(() => emitOutputTrace(this.#diagnostics?.trace, 'cleanup', {
        backend: 'file_system_access', task_id: this.taskId, transition: 'deferred',
      }))
    }
    return nextRecord
  }

  async close(): Promise<void> {
    await this.#lock.release()
  }

  async #ensureDirectory(request: TransferDirectoryRequest, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await this.#resolveDirectory(request.path, signal)
  }

  async #checkCompleted(fileId: string, signal: AbortSignal): Promise<CompletedOutputFile | undefined> {
    signal.throwIfAborted()
    const existing = this.#files.completedAtOpen.get(fileId)
    if (existing === undefined) return undefined
    const path = this.#displayPath(existing.fsaPath ?? existing.path)
    if (existing.handle === undefined) throw destinationChanged(path)
    await verifyExistingFSAFile(existing, existing.handle, path)
    return Object.freeze({ size: existing.size })
  }

  async #openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> {
    signal.throwIfAborted()
    let record = this.#files.find(this.taskId, request.fileId)
    const parent = await this.#resolveDirectory(request.path.slice(0, -1), signal, record?.fsaPath)
    const target = this.#record.target
    const leaf = record?.fsaPath?.at(-1) ?? record?.handle?.name ??
      (target.singleFile ? target.reservedName : request.path.at(-1)!)
    let handle: FileSystemFileHandle | undefined
    if (record !== undefined) {
      handle = await this.#locateRecordedFile(parent, leaf, record)
      if (handle === undefined) record = undefined
    }
    signal.throwIfAborted()
    const openedAtMs = Date.now()
    if (handle === undefined) {
      const entry = await openFSAFile(parent.handle, request.path, leaf, this.#safeNames)
      handle = entry.handle
      if (!entry.created) {
        // A repeated discovery can hit a completed file; a crash or a user edit can occupy the same name.
        // Resolve that rare case from IndexedDB without retaining a file ID for every ordinary success.
        record = await this.#store.getFile(this.taskId, request.fileId)
        if (record === undefined) throw destinationChanged(this.#displayPath([...parent.physicalPath, handle.name]))
      }
    }
    const physicalPath = [...parent.physicalPath, handle.name]
    const displayPath = this.#displayPath(physicalPath)
    const existing = record === undefined ? undefined : await this.#verifyFile(record, handle, displayPath)
    if (parent.created) this.#fileDirectories.add(parent.handle)
    return new FSAOutputFile({
      taskId: this.taskId, request, parentDir: parent.handle, handle, physicalPath,
      displayPath, openedAtMs, signal, store: this.#files, existing, diagnostics: this.#diagnostics,
    })
  }

  async #locateRecordedFile(parent: FSADirectory, leaf: string, record: ReceiveFileRecord): Promise<FileSystemFileHandle | undefined> {
    try {
      return await parent.handle.getFileHandle(leaf)
    } catch (error) {
      if (!isMissingFSAEntry(error) || record.status === 'completed') {
        throw destinationChanged(this.#displayPath([...parent.physicalPath, leaf]), error)
      }
      // Moving an unverified file aside lets the user restart at byte zero.
      await this.#files.deleteFiles([record.id])
      return undefined
    }
  }

  async #verifyFile(record: ReceiveFileRecord, handle: FileSystemFileHandle, path: string): Promise<ReceiveFileRecord> {
    if (record.handle === undefined || !await sameFSAEntry(handle, record.handle)) throw destinationChanged(path)
    const existing = await verifyExistingFSAFile(record, handle, path)
    if (record.fsaPendingPublish !== undefined) await this.#files.putFile(existing)
    return existing
  }

  #displayPath(path: readonly string[]): string {
    const target = this.#record.target
    return [target.parentHandle.name, ...(target.singleFile ? [] : [target.reservedName]), ...path].join('/')
  }

  async #resolveDirectory(path: readonly string[], signal: AbortSignal, storedPath?: readonly string[]): Promise<FSADirectory> {
    let current: FSADirectory = { handle: this.#rootHandle, physicalPath: [], created: false }
    const prefix: string[] = []
    for (const segment of path) {
      signal.throwIfAborted()
      prefix.push(segment)
      const cacheKey = JSON.stringify(prefix)
      let next = this.#dirCache.get(cacheKey)
      if (next === undefined) {
        const name = storedPath?.[prefix.length - 1] ?? this.#safeNames.physicalComponent(prefix, 'directory') ?? segment
        next = this.#openSubdirectory(current, prefix, name)
        this.#dirCache.set(cacheKey, next)
      }
      try {
        current = await next
      } catch (error) {
        if (this.#dirCache.get(cacheKey) === next) this.#dirCache.delete(cacheKey)
        throw error
      }
    }
    return current
  }

  async #openSubdirectory(parent: FSADirectory, prefix: readonly string[], name: string): Promise<FSADirectory> {
    let next: FSADirectory
    try {
      next = await openDirectory(parent, name)
    } catch (error) {
      if (!requiresSafeComponent(error)) throw error
      const safe = await deterministicSafeComponent({ logicalPath: prefix, entryKind: 'directory' })
      next = await openDirectory(parent, safe)
      this.#safeNames.record(prefix, 'directory', safe)
    }
    if (next.created) this.#createdDirectories.add(next.handle)
    return next
  }
}

function requiresSafeComponent(error: unknown): boolean {
  return error instanceof TypeError || (error instanceof DOMException && error.name === 'TypeMismatchError')
}

async function openDirectory(parent: FSADirectory, name: string): Promise<FSADirectory> {
  const existing = await parent.handle.getDirectoryHandle(name).catch((error: unknown) => {
    if (!isMissingFSAEntry(error)) throw error
  })
  const handle = existing ?? await parent.handle.getDirectoryHandle(name, { create: true })
  return { handle, physicalPath: [...parent.physicalPath, handle.name], created: existing === undefined }
}

async function openFSAFile(
  parent: FileSystemDirectoryHandle, logicalPath: readonly string[], leaf: string, safeNames: SafeNameRegistry,
): Promise<{ handle: FileSystemFileHandle; created: boolean }> {
  const open = async (name: string) => {
    try {
      return { handle: await parent.getFileHandle(name), created: false }
    } catch (error) {
      if (!isMissingFSAEntry(error)) throw error
    }
    return { handle: await parent.getFileHandle(name, { create: true }), created: true }
  }
  try {
    return await open(leaf)
  } catch (error) {
    if (!requiresSafeComponent(error)) throw error
    const name = await deterministicSafeComponent({ logicalPath, entryKind: 'file' })
    const entry = await open(name)
    safeNames.record(logicalPath, 'file', name)
    return entry
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
  for (let collisionIndex = 0; collisionIndex < MAX_COLLISION_ATTEMPTS; collisionIndex++) {
    let reservedName = decideCollisionName(requestedName, kind, collisionIndex)
    let rootHandle = await reserveRootName(parentHandle, reservedName, singleFile)
    if (rootHandle === 'invalid-name') {
      reservedName = await deterministicSafeComponent({ logicalPath: [requestedName], entryKind: kind, collisionIndex })
      rootHandle = await reserveRootName(parentHandle, reservedName, singleFile)
      if (typeof rootHandle !== 'string') safeNames.record([requestedName], kind, reservedName)
    }
    if (typeof rootHandle !== 'string') return { reservedName, collisionIndex, rootHandle }
  }
  throw new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
    'Could not allocate a unique name in the selected folder',
  )
}

async function reserveRootName(
  parent: FileSystemDirectoryHandle, name: string, singleFile: boolean,
): Promise<FileSystemDirectoryHandle | 'exists' | 'invalid-name'> {
  const state = await entryExists(parent, name)
  if (state !== 'missing') return state
  try {
    return singleFile ? parent : await parent.getDirectoryHandle(name, { create: true })
  } catch (error) {
    if (error instanceof TypeError) return 'invalid-name'
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    throw error
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
    if (isMissingFSAEntry(error)) return 'missing'
    throw error
  }
}

function nextFSALifecycle(
  current: ReceiveTaskRecord['lifecycle'],
  result: SettledTransferJobResult,
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
