import type { BrowserOutputWindow, OutputSessionHandle } from '../../output/backends/contract'
import { discardFSATask, reopenFSASession } from '../../output/backends/fsa'
import {
  browserOPFSEnvironment,
  deleteOPFSTask,
  exportPartialOPFSZip,
  reopenOPFSSession,
  saveRetainedOPFSTask,
  type OPFSEnvironment,
} from '../../output/backends/opfs'
import type { OutputDiagnosticsPorts } from '../../output/diagnostics'
import type { ReceiveStore } from '../../output/store/database'
import { isTaskLockedElsewhere } from '../../output/store/tab-lock'
import { completedTuplesBytes, type ReceiveTaskRecord } from '../../output/store/types'
import { transitionReceiveState } from '../../transfer/state'
import { actionsForState } from './admission'
import type { ReceiveAction, RetainedTaskSnapshot } from './contracts'

export interface RetainedTaskEnvironment {
  readonly store: ReceiveStore
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly locks?: LockManager
  readonly diagnostics?: OutputDiagnosticsPorts
}

/**
 * Loads all persisted tasks from `ReceiveStore`, normalizing any abandoned `downloading`
 * task whose tab crashed (no active Web Lock) into `paused (interrupted)`.
 */
export async function loadRetainedTasks(
  env: RetainedTaskEnvironment,
  activeLocalTaskId: string | null,
): Promise<readonly RetainedTaskSnapshot[]> {
  const records = await env.store.listTasks()
  const snapshots: RetainedTaskSnapshot[] = []
  for (const record of records) {
    const normalized = await normalizeAbandonedTask(record, env, activeLocalTaskId)
    const activeElsewhere = normalized.taskId !== activeLocalTaskId &&
      await isTaskLockedElsewhere(normalized.taskId, env.locks)
    const canSavePartial = normalized.backend === 'opfs' &&
      normalized.target.kind === 'opfs' &&
      normalized.target.isZip &&
      normalized.lifecycle.kind === 'paused' &&
      normalized.lifecycle.completedFiles > 0n
    const actions = actionsForState(normalized.lifecycle, normalized.backend, canSavePartial)
    snapshots.push(Object.freeze({
      taskId: normalized.taskId,
      shareInstance: normalized.shareInstance,
      selectionKey: normalized.selectionKey,
      backend: normalized.backend,
      lifecycle: normalized.lifecycle,
      ...(normalized.display === undefined ? {} : { display: normalized.display }),
      ...(normalized.safeRenamed === undefined ? {} : { safeRenamed: normalized.safeRenamed }),
      ...(normalized.sourceRevisionFailures === undefined
        ? {}
        : { sourceRevisionFailures: normalized.sourceRevisionFailures }),
      actions,
      activeElsewhere,
      canSavePartial,
    }))
  }
  return Object.freeze(snapshots)
}

export type RetainedActionOutcome =
  | Readonly<{ kind: 'completed'; updated?: ReceiveTaskRecord }>
  | Readonly<{ kind: 'reopened'; task: ReceiveTaskRecord; session: OutputSessionHandle }>

export async function executeRetainedTaskAction(
  env: RetainedTaskEnvironment,
  taskId: string,
  action: ReceiveAction,
): Promise<RetainedActionOutcome> {
  const record = await env.store.getTask(taskId)
  if (record === undefined) {
    throw new DOMException('This download record no longer exists', 'NotFoundError')
  }

  switch (action) {
    case 'forget':
      return forgetRetainedTask(env, record)
    case 'discard':
      return removeRetainedTask(env, record)
    case 'save':
      return saveRetainedTask(env, record)
    case 'save-partial':
      return exportPartialRetainedTask(env, record)
    case 'continue':
      return continueRetainedTask(env, record)
    case 'pause':
    case 'stop':
      throw new DOMException('Only an active download can be paused or stopped', 'InvalidStateError')
  }
}

async function forgetRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  // Never silently forget an unfinished resumable task; require explicit discard/delete.
  if (
    record.lifecycle.kind === 'paused' ||
    record.lifecycle.kind === 'ready-to-save' ||
    (record.lifecycle.kind === 'download-started' && record.lifecycle.retained)
  ) {
    throw new DOMException('Remove retained download data explicitly instead of forgetting history', 'InvalidStateError')
  }
  await env.store.deleteTask(record.taskId)
  return { kind: 'completed' }
}

async function removeRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend === 'opfs') {
    await deleteOPFSTask(record, env.store, resolveOPFSEnv(env))
  } else if (record.backend === 'fsa') {
    await discardFSATask(record, env.store, env.locks, env.diagnostics)
  } else {
    await env.store.deleteTask(record.taskId)
  }
  return { kind: 'completed' }
}

async function saveRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend !== 'opfs') {
    throw new DOMException('Only browser-stored downloads can be saved again locally', 'InvalidStateError')
  }
  const updated = await saveRetainedOPFSTask(record, env.store, resolveOPFSEnv(env), env.windowPort)
  return { kind: 'completed', updated }
}

async function exportPartialRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend !== 'opfs') {
    throw new DOMException('Partial ZIP export is only available for browser-stored ZIP downloads', 'InvalidStateError')
  }
  await exportPartialOPFSZip(record, env.store, resolveOPFSEnv(env), env.windowPort)
  return { kind: 'completed' }
}

async function continueRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend === 'fsa') {
    const session = await reopenFSASession({
      task: record,
      store: env.store,
      allowPermissionPrompt: true,
      ...(env.locks === undefined ? {} : { locks: env.locks }),
      ...(env.diagnostics === undefined ? {} : { diagnostics: env.diagnostics }),
    })
    return { kind: 'reopened', task: record, session }
  }
  if (record.backend === 'opfs') {
    const session = await reopenOPFSSession({
      task: record,
      store: env.store,
      env: resolveOPFSEnv(env),
      windowPort: env.windowPort,
      ...(env.diagnostics === undefined ? {} : { diagnostics: env.diagnostics }),
    })
    return { kind: 'reopened', task: record, session }
  }
  throw new DOMException('This download cannot be resumed', 'NotSupportedError')
}

function resolveOPFSEnv(env: RetainedTaskEnvironment): OPFSEnvironment {
  return env.opfsEnv ?? browserOPFSEnvironment(env.windowPort.navigator?.storage, env.locks)
}

async function normalizeAbandonedTask(
  record: ReceiveTaskRecord,
  env: RetainedTaskEnvironment,
  activeLocalTaskId: string | null,
): Promise<ReceiveTaskRecord> {
  if (record.lifecycle.kind !== 'downloading' && record.lifecycle.kind !== 'preparing') {
    return record
  }
  if (record.taskId === activeLocalTaskId) return record
  if (await isTaskLockedElsewhere(record.taskId, env.locks)) return record

  const files = await env.store.listTaskFiles(record.taskId)
  let completedFiles = 0n
  let completedBytes = 0n
  let retainedBytes = 0n
  for (const file of files) {
    const bytes = completedTuplesBytes(file.completedRanges)
    retainedBytes += bytes
    if (file.status === 'completed') {
      completedFiles += 1n
      completedBytes += file.size
    }
  }
  const paused = transitionReceiveState(record.lifecycle, {
    kind: 'paused',
    reason: 'interrupted',
    completedFiles,
    completedBytes,
    retainedBytes,
    discoveredFiles: BigInt(files.length),
    discoveredBytes: retainedBytes,
    discoveryComplete: record.discoveryComplete,
  })
  const updated: ReceiveTaskRecord = Object.freeze({
    ...record,
    lifecycle: paused,
    updatedAt: Date.now(),
  })
  await env.store.putTask(updated).catch(() => undefined)
  return updated
}
