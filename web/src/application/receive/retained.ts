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
import { diagnosticBackendName, type OutputDiagnosticsPorts, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveStore } from '../../output/store/database'
import { acquireTaskLock, isTaskLockedElsewhere, TaskBusyError } from '../../output/store/tab-lock'
import { completedTuplesBytes, type ReceiveTaskRecord } from '../../output/store/types'
import { transitionReceiveState } from '../../transfer/state'
import { actionsForState, canSavePartialState } from './admission'
import type { ReceiveAction, RetainedTaskSnapshot } from './contracts'

export interface RetainedTaskEnvironment {
  readonly store: ReceiveStore
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly locks?: LockManager
  readonly outputTrace?: OutputTraceSource
}

export function retainedTaskEnvironment(
  store: ReceiveStore,
  { windowPort, opfsEnv, outputTrace }: Pick<RetainedTaskEnvironment, 'windowPort' | 'opfsEnv' | 'outputTrace'>,
): RetainedTaskEnvironment {
  const locks = windowPort.navigator?.locks
  return {
    store, windowPort,
    ...(locks === undefined ? {} : { locks }),
    ...(opfsEnv === undefined ? {} : { opfsEnv }),
    ...(outputTrace === undefined ? {} : { outputTrace }),
  }
}

/** Only tasks with local results or recoverable progress can replace a fresh selection. */
export function matchingRetainedTask(
  tasks: readonly RetainedTaskSnapshot[], shareInstance: string, selectionKey: string,
): RetainedTaskSnapshot | undefined {
  return tasks.filter(task => task.shareInstance === shareInstance && task.selectionKey === selectionKey &&
    (task.lifecycle.kind === 'paused' || task.lifecycle.kind === 'ready-to-save' ||
      (task.lifecycle.kind === 'download-started' && task.lifecycle.retained) ||
      (task.backend === 'opfs' && task.lifecycle.kind === 'partial')))
    .sort((left, right) =>
      (right.display?.createdAtMilliseconds ?? 0) - (left.display?.createdAtMilliseconds ?? 0))[0]
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
    if (normalized === undefined) continue
    const activeElsewhere = normalized.taskId !== activeLocalTaskId &&
      await isTaskLockedElsewhere(normalized.taskId, env.locks)
    const canSavePartial = canSavePartialState(
      normalized.lifecycle,
      normalized.backend,
      normalized.target.kind === 'opfs' && normalized.target.isZip,
    )
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

function retainedDiagnostics(env: RetainedTaskEnvironment, record: ReceiveTaskRecord): OutputDiagnosticsPorts {
  return { backend: diagnosticBackendName(record.backend), ...(env.outputTrace === undefined ? {} : { trace: env.outputTrace }) }
}

async function forgetRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  // Never silently forget an unfinished or unexported browser-retained task; require explicit discard/delete.
  if (
    record.lifecycle.kind === 'paused' ||
    record.lifecycle.kind === 'ready-to-save' ||
    (record.lifecycle.kind === 'download-started' && record.lifecycle.retained) ||
    (record.backend === 'opfs' && record.lifecycle.kind === 'partial')
  ) {
    throw new DOMException('Remove retained download data explicitly instead of forgetting history', 'InvalidStateError')
  }
  if (record.backend === 'fsa' && (record.lifecycle.kind === 'partial' || record.lifecycle.kind === 'failed')) {
    return removeRetainedTask(env, record)
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
    await discardFSATask(record, env.store, env.locks, retainedDiagnostics(env, record))
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
      diagnostics: retainedDiagnostics(env, record),
    })
    return { kind: 'reopened', task: record, session }
  }
  if (record.backend === 'opfs') {
    const session = await reopenOPFSSession({
      task: record,
      store: env.store,
      env: resolveOPFSEnv(env),
      windowPort: env.windowPort,
      diagnostics: retainedDiagnostics(env, record),
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
): Promise<ReceiveTaskRecord | undefined> {
  if (record.lifecycle.kind !== 'downloading' && record.lifecycle.kind !== 'preparing') {
    return record
  }
  if (record.taskId === activeLocalTaskId) return record
  // Hold the same lock as the writer through the IDB update. A probe alone can race a resume in another tab.
  const lock = await acquireTaskLock(record.taskId, env.locks).catch((error: unknown) => {
    if (error instanceof TaskBusyError) return undefined
    throw error
  })
  if (lock === undefined) return record
  try {
    const current = await env.store.getTask(record.taskId)
    if (current === undefined || (current.lifecycle.kind !== 'preparing' && current.lifecycle.kind !== 'downloading')) {
      return current
    }
    return await pauseFromCheckpoints(current, env)
  } finally {
    await lock.release()
  }
}

async function pauseFromCheckpoints(record: ReceiveTaskRecord, env: RetainedTaskEnvironment): Promise<ReceiveTaskRecord> {
  const files = await env.store.listTaskFiles(record.taskId)
  let completedFiles = 0n
  let completedBytes = 0n
  let retainedBytes = 0n
  let discoveredFiles = 0n
  let discoveredBytes = 0n
  for (const file of files) {
    if (file.zipEntry?.kind === 'directory') continue
    discoveredFiles += 1n
    discoveredBytes += file.size
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
    discoveredFiles,
    discoveredBytes,
    discoveryComplete: record.discoveryComplete,
  })
  const updated: ReceiveTaskRecord = Object.freeze({
    ...record,
    lifecycle: paused,
    updatedAt: Date.now(),
  })
  await env.store.putTask(updated)
  return updated
}
