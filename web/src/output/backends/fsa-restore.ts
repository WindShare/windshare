import {
  FaultScope,
  OutputFaultCode,
  TransferFaultError,
  isPermissionDeniedError,
  outputFault,
} from '../../transfer/fault'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import {
  FSARestoreTargetMissingError,
  type RestorePlatform,
  type SafeNameRestoreExportResult,
  type SafeNameRestorePlan,
} from '../model/safe-name-restore'
import { decideCollisionName } from '../names/policy'
import { buildSafeNameRestorePlan, renderRestoreArtifact } from '../names/restore-plan'
import type { ReceiveStore } from '../store/database'
import { acquireTaskLock, withFSAReservationLock } from '../store/tab-lock'
import type { FSADiskFingerprint } from '../store/types'
import { isMissingFSAEntry, readFSADiskFingerprint, removeVerifiedFSALeaf } from './fsa-cleanup'
import { ensureFSAPermission, entryExists } from './fsa-reservation'

const MAX_ARTIFACT_COLLISION_ATTEMPTS = 1_000
const TEXT_ENCODER = new TextEncoder()

export async function loadFSARestorePlan(
  store: Pick<ReceiveStore, 'getTask' | 'listTaskFiles'>,
  taskId: string,
  platform: RestorePlatform,
): Promise<SafeNameRestorePlan> {
  const task = await store.getTask(taskId)
  if (task === undefined) {
    throw new DOMException('This download record no longer exists', 'NotFoundError')
  }
  if (task.target.kind !== 'fsa' || (task.lifecycle.kind !== 'saved' && task.lifecycle.kind !== 'partial')) {
    return buildSafeNameRestorePlan({ task, files: [], platform })
  }
  const files = await store.listTaskFiles(taskId)
  return buildSafeNameRestorePlan({ task, files, platform })
}

export async function writeFSARestoreArtifact(
  store: Pick<ReceiveStore, 'getTask' | 'listTaskFiles'>,
  taskId: string,
  platform: RestorePlatform,
  locks?: LockManager,
  diagnostics?: OutputDiagnosticsPorts,
  signal?: AbortSignal,
): Promise<SafeNameRestoreExportResult> {
  const lock = await acquireTaskLock(taskId, locks)
  try {
    signal?.throwIfAborted()
    const task = await store.getTask(taskId)
    signal?.throwIfAborted()
    if (task === undefined) {
      throw new DOMException('This download record no longer exists', 'NotFoundError')
    }
    if (task.target.kind !== 'fsa') {
      throw new DOMException('Filename restoration is only available for folder downloads', 'InvalidStateError')
    }
    if (task.lifecycle.kind !== 'saved' && task.lifecycle.kind !== 'partial') {
      throw new DOMException('Filename restoration is only available after a download finishes or stops', 'InvalidStateError')
    }

    // Request permission immediately after the O(1) task lookup while the click's user activation is still fresh.
    const permission = await ensureFSAPermission(task.target.parentHandle, true, signal)
    signal?.throwIfAborted()
    if (permission !== 'granted') {
      throw new TransferFaultError(
        outputFault(FaultScope.Job, OutputFaultCode.PermissionDenied),
        'Folder permission is required to save the filename restoration file',
      )
    }

    const files = await store.listTaskFiles(taskId)
    signal?.throwIfAborted()
    const plan = buildSafeNameRestorePlan({ task, files, platform })
    if (plan.restorable.length === 0) {
      throw new DOMException('No restorable filenames were found for this download', 'InvalidStateError')
    }

    const targetDir = await resolveRestoreTargetDirectory(task.target, plan)
    const artifact = renderRestoreArtifact(plan, platform)
    const { fileName, usedFallback } = await writeArtifactWithPolicyFallback(
      targetDir,
      artifact.suggestedName,
      artifact.fallbackName,
      artifact.content,
      locks,
      signal,
    )

    emitOutputTrace(diagnostics?.trace, 'publication', {
      backend: 'file_system_access',
      task_id: taskId,
      operation: 'safe_name_restore_export',
      platform,
      working_directory: plan.workingDirectory,
      restorable_count: plan.restorable.length,
      unsupported_count: plan.unsupported.length,
      used_fallback: usedFallback,
    })

    return Object.freeze({
      fileName,
      runCommand: artifact.runCommand(fileName),
      directoryLabel: plan.targetFolderLabel,
    })
  } finally {
    await lock.release()
  }
}

async function resolveRestoreTargetDirectory(
  target: Extract<Awaited<ReturnType<ReceiveStore['getTask']>> extends infer T ? T extends { target: infer U } ? U : never : never, { kind: 'fsa' }>,
  plan: SafeNameRestorePlan,
): Promise<FileSystemDirectoryHandle> {
  if (plan.workingDirectory === 'parent-folder') {
    return target.parentHandle
  }
  try {
    return await target.parentHandle.getDirectoryHandle(target.reservedName)
  } catch (error) {
    if (isMissingFSAEntry(error) || (error instanceof DOMException && error.name === 'TypeMismatchError')) {
      throw new FSARestoreTargetMissingError(plan.targetFolderLabel, { cause: error })
    }
    if (isPermissionDeniedError(error)) {
      throw new TransferFaultError(
        outputFault(FaultScope.Job, OutputFaultCode.PermissionDenied),
        'Folder permission is required to access the destination folder',
        { cause: error },
      )
    }
    throw error
  }
}

async function writeArtifactWithPolicyFallback(
  dir: FileSystemDirectoryHandle,
  suggestedName: string,
  fallbackName: string | null,
  content: string,
  locks: LockManager | undefined,
  signal?: AbortSignal,
): Promise<{ readonly fileName: string; readonly usedFallback: boolean }> {
  try {
    const fileName = await writeUniqueTextFile(dir, suggestedName, content, locks, signal)
    return { fileName, usedFallback: false }
  } catch (error) {
    if (fallbackName === null || !isNameOrPolicyRejection(error, signal)) {
      throw error
    }
    const fileName = await writeUniqueTextFile(dir, fallbackName, content, locks, signal)
    return { fileName, usedFallback: true }
  }
}

interface ReservedArtifactFile {
  readonly fileName: string
  readonly handle: FileSystemFileHandle
  readonly fingerprint: FSADiskFingerprint
}

async function writeUniqueTextFile(
  dir: FileSystemDirectoryHandle,
  suggestedName: string,
  content: string,
  locks: LockManager | undefined,
  signal?: AbortSignal,
): Promise<string> {
  const reserved = await reserveUniqueArtifactFile(dir, suggestedName, locks, signal)
  let writer: FileSystemWritableFileStream | undefined
  try {
    signal?.throwIfAborted()
    writer = await reserved.handle.createWritable()
    signal?.throwIfAborted()
    await writer.write({ type: 'write', position: 0, data: TEXT_ENCODER.encode(content) })
    await writer.close()
    return reserved.fileName
  } catch (error) {
    if (writer !== undefined) {
      await writer.abort(error).catch(() => undefined)
    }
    // Verify identity and untouched 0-byte fingerprint so rollback never deletes a concurrently replaced file.
    await removeVerifiedFSALeaf(dir, reserved.fileName, reserved.handle, reserved.fingerprint).catch(() => undefined)
    throw error
  }
}

/**
 * Serializes artifact filename allocation across tasks (and with top-level download reservations)
 * because File System Access lacks atomic `O_EXCL` creation. Only placeholder reservation holds
 * the global lock; streaming the payload runs outside it so slow disk flushes cannot block new downloads.
 */
async function reserveUniqueArtifactFile(
  dir: FileSystemDirectoryHandle,
  suggestedName: string,
  locks: LockManager | undefined,
  signal?: AbortSignal,
): Promise<ReservedArtifactFile> {
  signal?.throwIfAborted()
  return withFSAReservationLock(async () => {
    for (let collisionIndex = 0; collisionIndex < MAX_ARTIFACT_COLLISION_ATTEMPTS; collisionIndex += 1) {
      signal?.throwIfAborted()
      const candidate = decideCollisionName(suggestedName, 'file', collisionIndex)
      const reserved = await tryReserveArtifactCandidate(dir, candidate, signal)
      if (reserved !== 'exists') return reserved
    }
    throw new TransferFaultError(
      outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
      'Could not allocate a unique filename in the destination folder',
    )
  }, locks)
}

async function tryReserveArtifactCandidate(
  dir: FileSystemDirectoryHandle,
  candidate: string,
  signal?: AbortSignal,
): Promise<ReservedArtifactFile | 'exists'> {
  const state = await entryExists(dir, candidate)
  if (state === 'exists') return 'exists'
  if (state === 'invalid-name') {
    throw new TypeError(`Browser rejected filename "${candidate}"`)
  }
  signal?.throwIfAborted()
  let handle: FileSystemFileHandle
  try {
    handle = await dir.getFileHandle(candidate, { create: true })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'TypeMismatchError') return 'exists'
    throw error
  }
  const fingerprint = await readFSADiskFingerprint(handle)
  // An external writer can populate the same path after entryExists; never adopt or overwrite non-empty bytes.
  if (fingerprint.size !== 0) return 'exists'
  return { fileName: candidate, handle, fingerprint }
}

function isNameOrPolicyRejection(error: unknown, signal?: AbortSignal): boolean {
  if (error instanceof TypeError) return true
  if (!(error instanceof DOMException)) return false
  if (error.name === 'InvalidStateError' || error.name === 'SecurityError') return true
  if (error.name === 'AbortError') return !(signal?.aborted ?? false)
  return false
}
