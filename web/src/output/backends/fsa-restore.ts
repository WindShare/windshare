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
import { acquireTaskLock } from '../store/tab-lock'
import { ensureFSAPermission } from './fsa'
import { isMissingFSAEntry } from './fsa-cleanup'
import { entryExists } from './fsa-reservation'

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
  signal?: AbortSignal,
): Promise<{ readonly fileName: string; readonly usedFallback: boolean }> {
  try {
    const fileName = await writeUniqueTextFile(dir, suggestedName, content, signal)
    return { fileName, usedFallback: false }
  } catch (error) {
    if (fallbackName === null || !isNameOrPolicyRejection(error, signal)) {
      throw error
    }
    const fileName = await writeUniqueTextFile(dir, fallbackName, content, signal)
    return { fileName, usedFallback: true }
  }
}

async function writeUniqueTextFile(
  dir: FileSystemDirectoryHandle,
  suggestedName: string,
  content: string,
  signal?: AbortSignal,
): Promise<string> {
  const allocated = await allocateUniqueFileName(dir, suggestedName, signal)
  let created = false
  let writer: FileSystemWritableFileStream | undefined
  try {
    signal?.throwIfAborted()
    const handle = await dir.getFileHandle(allocated, { create: true })
    created = true
    signal?.throwIfAborted()
    writer = await handle.createWritable()
    signal?.throwIfAborted()
    await writer.write({ type: 'write', position: 0, data: TEXT_ENCODER.encode(content) })
    await writer.close()
    return allocated
  } catch (error) {
    if (writer !== undefined) {
      await writer.abort(error).catch(() => undefined)
    }
    if (created) {
      await dir.removeEntry(allocated).catch(() => undefined)
    }
    throw error
  }
}

async function allocateUniqueFileName(
  dir: FileSystemDirectoryHandle,
  suggestedName: string,
  signal?: AbortSignal,
): Promise<string> {
  for (let collisionIndex = 0; collisionIndex < MAX_ARTIFACT_COLLISION_ATTEMPTS; collisionIndex += 1) {
    signal?.throwIfAborted()
    const candidate = decideCollisionName(suggestedName, 'file', collisionIndex)
    const state = await entryExists(dir, candidate)
    if (state === 'missing') return candidate
    if (state === 'invalid-name') {
      throw new TypeError(`Browser rejected filename "${candidate}"`)
    }
  }
  throw new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.TargetCollision),
    'Could not allocate a unique filename in the destination folder',
  )
}

function isNameOrPolicyRejection(error: unknown, signal?: AbortSignal): boolean {
  if (error instanceof TypeError) return true
  if (!(error instanceof DOMException)) return false
  if (error.name === 'InvalidStateError' || error.name === 'SecurityError') return true
  if (error.name === 'AbortError') return !(signal?.aborted ?? false)
  return false
}
