import {
  FaultScope,
  OutputFaultCode,
  TransferFaultError,
  isPermissionDeniedError,
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
  completedTuplesBytes,
  type FSATargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import type { BrowserOutputWindow, OutputSessionHandle, StartBackendTaskInput } from './contract'
import {
  cleanupFSATaskEntries,
  pruneEmptySubdirectories,
} from './fsa-cleanup'
import { FSAOutputFile, verifyExistingFSAFile } from './fsa-file'
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
    const priorCompleted = await loadCompletedFSARecords(input.store, updated.taskId)
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
      priorCompleted,
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
  readonly #priorCompleted: ReadonlyMap<string, ReceiveFileRecord>
  readonly #dirCache = new Map<string, FileSystemDirectoryHandle>()

  constructor(
    record: ReceiveTaskRecord,
    rootHandle: FileSystemDirectoryHandle,
    store: ReceiveStore,
    lock: TaskLock,
    safeNames: SafeNameRegistry,
    diagnostics?: OutputDiagnosticsPorts,
    priorCompleted: ReadonlyMap<string, ReceiveFileRecord> = new Map(),
  ) {
    this.taskId = record.taskId
    this.#record = record
    this.#rootHandle = rootHandle
    this.#store = store
    this.#lock = lock
    this.#safeNames = safeNames
    this.#diagnostics = diagnostics
    this.#priorCompleted = priorCompleted
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
      ...(priorCompleted.size > 0
        ? { checkCompleted: (fileId: string, signal: AbortSignal) => this.#checkCompleted(fileId, signal) }
        : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
      markDiscoveryComplete: async () => {
        this.#record = Object.freeze({ ...this.#record, discoveryComplete: true, updatedAt: Date.now() })
        await this.#store.putTask(this.#record)
      },
      safeRenamedSummary: () => this.#safeNames.summary(),
    })
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    if (result.status === 'stopped' && this.#record.target.kind === 'fsa' && !this.#record.target.singleFile) {
      await pruneEmptySubdirectories(this.#rootHandle, [...this.#dirCache.values()]).catch(() => undefined)
    }
    const files = await this.#store.listTaskFiles(this.taskId)
    let retainedBytes = 0n
    for (const file of files) retainedBytes += completedTuplesBytes(file.completedRanges)

    const nextLifecycle = nextFSALifecycle(this.#record.lifecycle, result, retainedBytes)
    if ((nextLifecycle.kind === 'failed' || nextLifecycle.kind === 'partial') &&
        this.#record.target.kind === 'fsa' && files.some(file => file.status !== 'completed')) {
      await cleanupFSATaskEntries(this.taskId, this.#record.target, files, this.#diagnostics)
    }
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

  async #checkCompleted(fileId: string, signal: AbortSignal): Promise<CompletedOutputFile | undefined> {
    signal.throwIfAborted()
    const existing = this.#priorCompleted.get(fileId)
    if (existing === undefined) return undefined
    await verifyExistingFSAFile(existing)
    return Object.freeze({ size: existing.size })
  }

  async #openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> {
    signal.throwIfAborted()
    const existing = await this.#store.getFile(this.taskId, request.fileId)
    if (existing !== undefined) await verifyExistingFSAFile(existing)

    const parentPath = request.path.slice(0, -1)
    const leafName = this.#record.target.kind === 'fsa' && this.#record.target.singleFile
      ? this.#record.target.reservedName
      : request.path.at(-1)!
    const parentDir = parentPath.length === 0
      ? this.#rootHandle
      : await this.#resolveDirectory(parentPath, signal)

    const openedAtMs = Date.now()
    const { handle, physicalName } = existing?.handle !== undefined
      ? { handle: existing.handle, physicalName: existing.physicalName ?? existing.handle.name }
      : await createOrOpenFSAFile(parentDir, request.path, leafName, this.#safeNames)

    return new FSAOutputFile({
      taskId: this.taskId,
      request,
      parentDir,
      handle,
      physicalName,
      openedAtMs,
      store: this.#store,
      existing,
      diagnostics: this.#diagnostics,
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

async function loadCompletedFSARecords(
  store: ReceiveStore,
  taskId: string,
): Promise<ReadonlyMap<string, ReceiveFileRecord>> {
  const files = await store.listTaskFiles(taskId)
  const completed = new Map<string, ReceiveFileRecord>()
  for (const file of files) {
    if (file.status === 'completed') completed.set(file.fileId, file)
  }
  return completed
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
