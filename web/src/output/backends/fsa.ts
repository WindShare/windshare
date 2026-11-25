import {
  FaultScope,
  OutputFaultCode,
  TransferFaultError,
  classifyNativeError,
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
import { transitionReceiveState, type SafeRenamedSummary } from '../../transfer/state'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import { acquireTaskLock, withFSAReservationLock, type TaskLock } from '../store/tab-lock'
import {
  fileRecordId, summarizeStoredTaskFiles,
  type FSADiskFingerprint,
  type FSAReservedFile,
  type FSATargetRecord,
  type ReceiveFileRecord,
  type ReceiveTaskRecord,
} from '../store/types'
import {
  loadLockedReopenableTask,
  mergeSourceOmittedEntries,
  restoreReopenedTaskRecord,
  type BrowserOutputWindow,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from './contract'
import {
  cleanupFSATaskEntries, isFSATypeMismatch, isMissingFSAEntry, isUncommittedEmptyPlaceholder,
  matchesFSADiskFingerprint, readFSADiskFingerprint, sameFSAEntry, settleFSATerminalCleanup,
} from './fsa-cleanup'
import { destinationChanged, FSAOutputFile, verifyExistingFSAFile, type FSAFileOrigin } from './fsa-file'
import {
  buildFSATargetRecord, collectFSATopLevelClaims, ensureFSAPermission, openFSADirectory, openFSAFile,
  openOrCreateFSAFile, reserveFSATarget, retainsFSAReservationClaim, rollbackFSATaskTarget,
  rollbackReservedTarget, type FSADirectory,
} from './fsa-reservation'
import { nextFSALifecycle } from './fsa-settlement'
import {
  SafeNameRegistry, applySafeRenamedFields, collectSafeRenamedFromRecords,
  mergeRenamedDirectories,
} from './safe-name'

type FSATaskRecord = ReceiveTaskRecord & { readonly target: FSATargetRecord }

const FSA_MAX_CONCURRENT_FILES = 4

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
    const reservedAtMs = Date.now()
    const { record, rootHandle, reserved } = await withFSAReservationLock(async () => {
      const claimedNames = await collectFSATopLevelClaims(input.store, input.parentHandle, input.task.taskId)
      const allocated = await reserveFSATarget(
        input.parentHandle,
        input.task.artifact.name,
        singleFile,
        safeNames,
        input.task.taskId,
        claimedNames,
      )
      try {
        const target = buildFSATargetRecord(input.parentHandle, allocated, reservedAtMs)
        const rootHandle = allocated.entry.kind === 'directory' ? allocated.entry.handle : input.parentHandle
        const display = input.task.display === undefined
          ? undefined
          : Object.freeze({ ...input.task.display, destinationLabel: input.parentHandle.name })
        const downloading = transitionReceiveState(input.task.lifecycle, { kind: 'downloading' })
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
          updatedAt: Date.now(),
        })
        // Persist inside the reservation lock so concurrent opens observe freshly claimed names
        // and failed writes roll back before another tab can claim the same slot.
        await input.store.putTask(record)
        return { record, rootHandle, reserved: allocated }
      } catch (error) {
        await rollbackReservedTarget(input.parentHandle, allocated).catch(() => undefined)
        throw error
      }
    }, input.locks)
    emitOutputTrace(input.diagnostics?.trace, 'output_reservation', {
      backend: 'file_system_access',
      task_id: input.task.taskId,
      transition: 'acquired',
      collision_index: reserved.collisionIndex,
      ...(reserved.skippedClaimedNames > 0 ? { skipped_claimed_names: reserved.skippedClaimedNames } : {}),
    })
    return new FSASessionHandle(record, rootHandle, input.store, lock, safeNames, input.locks, input.diagnostics)
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
  const traceReopenFailed = (error: unknown) => emitOutputTrace(input.diagnostics?.trace, 'reopen', {
    backend: 'file_system_access', task_id: input.task.taskId, transition: 'failed', error_class: classifyNativeError(error),
  })
  let lock: TaskLock
  try {
    lock = await acquireTaskLock(input.task.taskId, input.locks)
  } catch (error) {
    traceReopenFailed(error)
    throw error
  }
  try {
    // Capture the upper bound once the task lock proves no previous session is still allocating placeholders.
    const reopenedAtMs = Date.now()
    const current = await loadLockedReopenableTask(input.store, input.task.taskId, 'fsa', input.signal)
    const perm = await ensureFSAPermission(current.target.parentHandle, input.allowPermissionPrompt, input.signal)
    input.signal?.throwIfAborted()
    if (perm !== 'granted') {
      throw new TransferFaultError(
        outputFault(FaultScope.Job, OutputFaultCode.PermissionDenied),
        'Folder permission is required to continue saving',
      )
    }
    const target = current.target
    const rootHandle = target.singleFile
      ? target.parentHandle
      : await target.parentHandle.getDirectoryHandle(target.reservedName, { create: true })
    const downloading = transitionReceiveState(current.lifecycle, { kind: 'downloading' })
    const updated: FSATaskRecord = Object.freeze({ ...current, target, lifecycle: downloading, updatedAt: Date.now() })
    const priorFiles = await input.store.listTaskFiles(updated.taskId)
    input.signal?.throwIfAborted()
    await input.store.putTask(updated)
    const safeNames = new SafeNameRegistry()
    safeNames.seedFromPrior(updated, priorFiles, updated.safeRenamedDirectories)
    emitOutputTrace(input.diagnostics?.trace, 'reopen', {
      backend: 'file_system_access', task_id: updated.taskId, transition: 'authorized',
    })
    return new FSASessionHandle(
      updated, rootHandle, input.store, lock, safeNames, input.locks, input.diagnostics, priorFiles, reopenedAtMs, current,
    )
  } catch (error) {
    traceReopenFailed(error)
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
    if (current.target.kind === 'fsa' && retainsFSAReservationClaim(current.lifecycle)) {
      const files = await store.listTaskFiles(task.taskId)
      const unfinished = files.some(file => file.status !== 'completed')
      // Partial results own their saved root directory even if no file rows remain;
      // other claim-retaining states own an empty reserved directory or 0-byte file placeholder.
      const hasReservedPlaceholder = files.length === 0 && current.lifecycle.kind !== 'partial'
      if (unfinished || hasReservedPlaceholder) {
        const permission = await ensureFSAPermission(current.target.parentHandle, true, signal)
        signal?.throwIfAborted()
        if (permission === 'granted') {
          await cleanupFSATaskEntries(task.taskId, current.target, files, { store, diagnostics, beginRemoval })
        } else {
          emitOutputTrace(diagnostics?.trace, 'cleanup', {
            backend: 'file_system_access',
            task_id: task.taskId,
            transition: 'permission_denied',
          })
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
  readonly reopenedFrom?: ReceiveTaskRecord
  #record: FSATaskRecord
  readonly #rootHandle: FileSystemDirectoryHandle
  readonly #store: ReceiveStore
  readonly #lock: TaskLock
  readonly #safeNames: SafeNameRegistry
  readonly #locks: LockManager | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #reopenedAtMs: number | undefined
  readonly #files: FSAFileRecords
  readonly #dirCache = new Map<string, Promise<FSADirectory>>()
  readonly #createdDirectories = new Set<FileSystemDirectoryHandle>()
  readonly #fileDirectories = new Set<FileSystemDirectoryHandle>()
  readonly #reclaimedPlaceholders: FileSystemFileHandle[] = []
  #reservedPlaceholderActive = false
  #outputTouched = false
  #rolledBack = false
  #reclaimTail: Promise<void> = Promise.resolve()

  constructor(
    record: FSATaskRecord,
    rootHandle: FileSystemDirectoryHandle,
    store: ReceiveStore,
    lock: TaskLock,
    safeNames: SafeNameRegistry,
    locks?: LockManager,
    diagnostics?: OutputDiagnosticsPorts,
    priorFiles: readonly ReceiveFileRecord[] = [],
    reopenedAtMs?: number,
    reopenedFrom?: ReceiveTaskRecord,
  ) {
    this.taskId = record.taskId
    if (reopenedFrom !== undefined) this.reopenedFrom = reopenedFrom
    this.#record = record
    this.#rootHandle = rootHandle
    this.#store = store
    this.#lock = lock
    this.#safeNames = safeNames
    this.#locks = locks
    this.#diagnostics = diagnostics
    this.#reopenedAtMs = reopenedAtMs
    this.#files = new FSAFileRecords(store, priorFiles)
    this.transferOutput = Object.freeze({
      sessionId: record.taskId,
      capabilities: Object.freeze({
        // FileSystemWritableFileStream has no in-place flush; reopening with
        // keepExistingData copies the full file prefix on non-CoW filesystems,
        // so only explicit cuts and sufficiently large outage checkpoints justify reopening.
        maxConcurrentFiles: FSA_MAX_CONCURRENT_FILES,
        resumable: true,
        partialOutput: true,
      }),
      ensureDirectory: (request: TransferDirectoryRequest, signal: AbortSignal) => this.#ensureDirectory(request, signal),
      ...(this.#files.completedAtOpen.size > 0
        ? { checkCompleted: (fileId: string, signal: AbortSignal) => this.#checkCompleted(fileId, signal) }
        : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
      safeRenamedSummary: () => this.#safeNames.summary(),
    })
  }

  get record(): ReceiveTaskRecord {
    return this.#record
  }

  subscribeSafeRenamed(listener: (summary: SafeRenamedSummary) => void): () => void {
    return this.#safeNames.subscribe(listener)
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    this.#outputTouched = true
    const files = await this.#store.listTaskFiles(this.taskId)
    const stored = summarizeStoredTaskFiles(files)

    const nextLifecycle = nextFSALifecycle(this.#record.lifecycle, result, {
      singleFile: this.#record.target.singleFile,
      stored,
    })
    const candidateDirs = mergeRenamedDirectories(this.#record.safeRenamedDirectories, this.#safeNames.renamedDirectories())
    const initialSafe = nextLifecycle.kind === 'failed'
      ? { summary: undefined, directories: undefined }
      : collectSafeRenamedFromRecords(this.#record, files, candidateDirs)
    const { sourceOmittedEntries: previousOmitted, ...baseRecord } = this.#record
    const sourceOmittedEntries = mergeSourceOmittedEntries(
      result.sourceOmittedEntries,
      previousOmitted,
      result.discoveryComplete,
    )
    let nextRecord = applySafeRenamedFields(Object.freeze({
      ...baseRecord,
      lifecycle: nextLifecycle,
      ...(sourceOmittedEntries === undefined ? {} : { sourceOmittedEntries }),
      ...(result.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: result.sourceRevisionFailures }),
      updatedAt: Date.now(),
    }), initialSafe)
    await this.#store.putTask(nextRecord)
    this.#record = nextRecord
    if (nextLifecycle.kind === 'failed' || nextLifecycle.kind === 'partial') {
      // Failed runs and user-stopped runs remove all task-created empty scaffolding;
      // failure-settled partial runs preserve explicitly selected empty folders and
      // prune only abandoned file parents. Non-recursive removal protects saved files.
      const pruneAllCreated =
        nextLifecycle.kind === 'failed' ||
        (nextLifecycle.kind === 'partial' && nextLifecycle.reason === 'stopped')
      const directories = pruneAllCreated ? [...this.#createdDirectories] : [...this.#fileDirectories]
      nextRecord = await settleFSATerminalCleanup({
        record: nextRecord,
        rootHandle: this.#rootHandle,
        files,
        initialSafe,
        store: {
          putTask: row => this.#store.putTask(row),
          deleteFiles: ids => this.#files.deleteFiles(ids),
        },
        diagnostics: this.#diagnostics,
        directories,
      })
      this.#record = nextRecord
    }
    emitOutputTrace(this.#diagnostics?.trace, 'settlement', {
      backend: 'file_system_access',
      task_id: this.taskId,
      job_status: result.status,
      lifecycle: nextRecord.lifecycle.kind,
      completed_files: String(stored.completedFiles),
      failed_files: String(result.failedFiles + result.failedDirectories),
      ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
    })
    return nextRecord
  }

  async rollbackUnactivated(): Promise<void> {
    if (this.#rolledBack || this.#outputTouched) return
    this.#rolledBack = true
    if (this.reopenedFrom !== undefined) {
      this.#record = await restoreReopenedTaskRecord(this.reopenedFrom, this.#store) as FSATaskRecord
      emitOutputTrace(this.#diagnostics?.trace, 'reopen', {
        backend: 'file_system_access', task_id: this.taskId, transition: 'rolled_back',
      })
      return
    }
    const removedPlaceholder = await rollbackFSATaskTarget(this.#record.target)
    await this.#store.deleteTask(this.taskId)
    emitOutputTrace(this.#diagnostics?.trace, 'output_reservation', {
      backend: 'file_system_access', task_id: this.taskId, transition: 'rolled_back',
      removed_placeholder: removedPlaceholder,
    })
  }

  async close(): Promise<void> {
    await this.#lock.release()
  }

  async #ensureDirectory(request: TransferDirectoryRequest, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#outputTouched = true
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
    this.#outputTouched = true
    const knownRecord = this.#files.find(this.taskId, request.fileId)
    const parent = await this.#resolveDirectory(request.path.slice(0, -1), signal, knownRecord?.fsaPath)
    const target = this.#record.target
    const leaf = knownRecord?.fsaPath?.at(-1) ?? knownRecord?.handle?.name ??
      (target.singleFile ? target.reservedName : request.path.at(-1)!)
    const locatedHandle = knownRecord === undefined ? undefined : await this.#locateRecordedFile(parent, leaf, knownRecord)
    signal.throwIfAborted()
    const opened = locatedHandle !== undefined && knownRecord !== undefined
      ? { kind: 'recorded' as const, handle: locatedHandle, record: knownRecord }
      : await this.#openUnrecordedFile(parent, request, leaf, Date.now())
    const physicalPath = [...parent.physicalPath, opened.handle.name]
    const displayPath = this.#displayPath(physicalPath)
    const origin: FSAFileOrigin = opened.kind === 'recorded'
      ? { kind: 'recorded', record: await this.#verifyFile(opened.record, opened.handle, displayPath) }
      : opened.origin
    if (parent.created) this.#fileDirectories.add(parent.handle)
    return new FSAOutputFile({
      taskId: this.taskId, request, parentDir: parent.handle, handle: opened.handle, physicalPath,
      displayPath, signal, store: this.#files, origin, diagnostics: this.#diagnostics,
    })
  }

  async #openUnrecordedFile(
    parent: FSADirectory,
    request: TransferFileRequest,
    leaf: string,
    openedAtMs: number,
  ): Promise<
    | Readonly<{ kind: 'recorded'; handle: FileSystemFileHandle; record: ReceiveFileRecord }>
    | Readonly<{ kind: 'placeholder'; handle: FileSystemFileHandle; origin: Exclude<FSAFileOrigin, { kind: 'recorded' }> }>
  > {
    const target = this.#record.target
    const entry = target.singleFile
      ? await this.#openSingleFileEntry(parent.handle, target.reservedName, request.fileId)
      : await openFSAFile(parent.handle, request.path, leaf, this.#safeNames, this.taskId)
    const candidatePath = this.#displayPath([...parent.physicalPath, entry.handle.name])
    const releaseReserved = () => { this.#reservedPlaceholderActive = false }
    if (entry.created) {
      if (!target.singleFile) {
        return { kind: 'placeholder', handle: entry.handle, origin: { kind: 'fresh-placeholder', openedAtMs } }
      }
      const fingerprint = await this.#refreshReservedFile(entry.handle, candidatePath)
      this.#reservedPlaceholderActive = true
      return {
        kind: 'placeholder',
        handle: entry.handle,
        origin: { kind: 'reserved-placeholder', fingerprint, onReleased: releaseReserved },
      }
    }
    // A repeated discovery can hit a completed file; a crash or a user edit can occupy the same name.
    // Resolve that rare case from IndexedDB without retaining a file ID for every ordinary success.
    const record = await this.#store.getFile(this.taskId, request.fileId)
    if (record !== undefined) return { kind: 'recorded', handle: entry.handle, record }
    if (target.singleFile) {
      const fingerprint = await this.#claimReservedFile(entry.handle, target.reservedFile, request.fileId, candidatePath)
      return {
        kind: 'placeholder',
        handle: entry.handle,
        origin: { kind: 'reserved-placeholder', fingerprint, onReleased: releaseReserved },
      }
    }
    const fingerprint = await this.#reclaimUncommittedPlaceholder(entry.handle, target.reservedAtMs, request.fileId, candidatePath)
    return {
      kind: 'placeholder',
      handle: entry.handle,
      origin: { kind: 'reclaimed-placeholder', fingerprint },
    }
  }

  async #openSingleFileEntry(
    parent: FileSystemDirectoryHandle,
    reservedName: string,
    fileId: string,
  ): Promise<{ handle: FileSystemFileHandle; created: boolean }> {
    try {
      return { handle: await parent.getFileHandle(reservedName), created: false }
    } catch (error) {
      if (isFSATypeMismatch(error)) throw this.#singleFileTypeMismatch(reservedName, fileId, error)
      if (!isMissingFSAEntry(error)) throw error
    }
    // Single-file reservations are negotiated once at openNewFSASession; recreate the
    // exact reserved slot under the reservation lock without child-scoped safe renaming.
    return withFSAReservationLock(async () => {
      try {
        return await openOrCreateFSAFile(parent, reservedName)
      } catch (error) {
        if (isFSATypeMismatch(error)) throw this.#singleFileTypeMismatch(reservedName, fileId, error)
        throw error
      }
    }, this.#locks)
  }

  #singleFileTypeMismatch(reservedName: string, fileId: string, cause: unknown): TransferFaultError {
    emitOutputTrace(this.#diagnostics?.trace, 'output_reservation', {
      backend: 'file_system_access',
      task_id: this.taskId,
      file_id: fileId,
      transition: 'single_file_type_mismatch',
    })
    return destinationChanged(this.#displayPath([reservedName]), cause)
  }

  #claimReservedFile(
    handle: FileSystemFileHandle,
    reserved: FSAReservedFile,
    fileId: string,
    displayPath: string,
  ): Promise<FSADiskFingerprint> {
    return this.#serializePlaceholderClaim(async () => {
      if (this.#reservedPlaceholderActive || !await this.#matchesReservedFile(handle, reserved, displayPath)) {
        throw destinationChanged(displayPath)
      }
      this.#reservedPlaceholderActive = true
      if (this.#reopenedAtMs !== undefined) {
        emitOutputTrace(this.#diagnostics?.trace, 'reopen', {
          backend: 'file_system_access', task_id: this.taskId, file_id: fileId, transition: 'placeholder_reclaimed',
        })
      }
      return reserved.fingerprint
    })
  }

  #reclaimUncommittedPlaceholder(
    handle: FileSystemFileHandle, reservedAtMs: number, fileId: string, displayPath: string,
  ): Promise<FSADiskFingerprint> {
    if (this.#reopenedAtMs === undefined) return Promise.reject(destinationChanged(displayPath))
    const reopenedAtMs = this.#reopenedAtMs
    return this.#serializePlaceholderClaim(async () => {
      for (const prior of this.#reclaimedPlaceholders) {
        if (await sameFSAEntry(prior, handle)) throw destinationChanged(displayPath)
      }
      const disk = await readFSADiskFingerprint(handle).catch((cause: unknown) => {
        throw destinationChanged(displayPath, cause)
      })
      if (!isUncommittedEmptyPlaceholder(disk, reservedAtMs, reopenedAtMs)) {
        throw destinationChanged(displayPath)
      }
      this.#reclaimedPlaceholders.push(handle)
      emitOutputTrace(this.#diagnostics?.trace, 'reopen', {
        backend: 'file_system_access', task_id: this.taskId, file_id: fileId, transition: 'placeholder_reclaimed',
      })
      // Preserve the exact disk fingerprint so first write, 0-byte commit, and abort cleanup can verify it.
      return disk
    })
  }

  async #serializePlaceholderClaim<T>(work: () => Promise<T>): Promise<T> {
    // Serialize claim checks so concurrent openFile calls cannot reclaim the same entry twice.
    const previous = this.#reclaimTail
    let release!: () => void
    this.#reclaimTail = new Promise<void>(resolve => { release = resolve })
    await previous
    try {
      return await work()
    } finally {
      release()
    }
  }

  async #matchesReservedFile(
    handle: FileSystemFileHandle,
    reserved: FSAReservedFile,
    displayPath: string,
  ): Promise<boolean> {
    if (!await sameFSAEntry(handle, reserved.handle)) return false
    const disk = await readFSADiskFingerprint(handle).catch((cause: unknown) => {
      throw destinationChanged(displayPath, cause)
    })
    return matchesFSADiskFingerprint(disk, reserved.fingerprint)
  }

  async #refreshReservedFile(handle: FileSystemFileHandle, displayPath: string): Promise<FSADiskFingerprint> {
    const target = this.#record.target
    if (!target.singleFile) throw new TypeError('Cannot refresh reserved file on a directory FSA task')
    if (handle.name !== target.reservedName) throw destinationChanged(displayPath)
    const fingerprint = await readFSADiskFingerprint(handle).catch((cause: unknown) => {
      throw destinationChanged(displayPath, cause)
    })
    if (fingerprint.size !== 0) throw destinationChanged(displayPath)
    const nextRecord: FSATaskRecord = Object.freeze({
      ...this.#record,
      target: Object.freeze({
        ...target,
        reservedFile: Object.freeze({ handle, fingerprint }),
      }),
      updatedAt: Date.now(),
    })
    await this.#store.putTask(nextRecord)
    this.#record = nextRecord
    return fingerprint
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
    const result = await openFSADirectory(parent, prefix, name, this.#safeNames, this.taskId)
    if (result.kind === 'conflict') {
      emitOutputTrace(this.#diagnostics?.trace, 'output_reservation', {
        backend: 'file_system_access',
        task_id: this.taskId,
        transition: 'directory_collision',
        depth: result.physicalPath.length,
      })
      throw destinationChanged(this.#displayPath(result.physicalPath))
    }
    if (result.directory.created) this.#createdDirectories.add(result.directory.handle)
    return result.directory
  }
}
