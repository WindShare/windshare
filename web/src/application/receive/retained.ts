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
import { diagnosticBackendName, emitOutputTrace, type OutputDiagnosticsPorts, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveStore } from '../../output/store/database'
import { acquireTaskLock, isTaskLockedElsewhere, TaskBusyError } from '../../output/store/tab-lock'
import { completedTuplesBytes, type ReceiveTaskRecord } from '../../output/store/types'
import { classifyNativeError, storagePauseReason } from '../../transfer/fault'
import type { JoinedBrowserShare } from '../browser-share/gateway'
import { closeOutputSession } from './session-opener'
import { transitionReceiveState } from '../../transfer/state'
import {
  actionsForState,
  admitRetainedAction,
  canForgetState,
  canSavePartialState,
  type RetainedAdmissionContext,
} from './admission'
import type { ReceiveAction, RetainedTaskSnapshot } from './contracts'

export interface RetainedTaskEnvironment {
  readonly store: ReceiveStore
  readonly signal?: AbortSignal
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly locks?: LockManager
  readonly outputTrace?: OutputTraceSource
}

export function retainedTaskEnvironment(
  store: ReceiveStore,
  { windowPort, opfsEnv, outputTrace }: Pick<RetainedTaskEnvironment, 'windowPort' | 'opfsEnv' | 'outputTrace'>,
  signal?: AbortSignal,
): RetainedTaskEnvironment {
  const locks = windowPort.navigator?.locks
  return {
    store, windowPort,
    ...(signal === undefined ? {} : { signal }),
    ...(locks === undefined ? {} : { locks }),
    ...(opfsEnv === undefined ? {} : { opfsEnv }),
    ...(outputTrace === undefined ? {} : { outputTrace }),
  }
}

export type WorkspaceRetainedMatch =
  | Readonly<{ kind: 'local-complete'; task: RetainedTaskSnapshot }>
  | Readonly<{
      kind: 'banner'
      bannerKind: 'continue' | 'ready-to-save'
      task: RetainedTaskSnapshot
      action: 'continue' | 'save'
    }>
  | Readonly<{ kind: 'pending-continuation' }>
  | Readonly<{ kind: 'none' }>

export interface WorkspaceRetainedMatchInput {
  readonly tasks: readonly RetainedTaskSnapshot[]
  readonly shareInstance: string | null
  readonly selectionKey: string | null
  readonly admission: RetainedAdmissionContext
  readonly isIgnored?: (task: RetainedTaskSnapshot) => boolean
}

function byNewestCreatedDesc(left: RetainedTaskSnapshot, right: RetainedTaskSnapshot): number {
  return (right.display?.createdAtMilliseconds ?? 0) - (left.display?.createdAtMilliseconds ?? 0)
}

function isCompleteLocalRetained(
  task: RetainedTaskSnapshot,
  admission: RetainedAdmissionContext,
): boolean {
  const { lifecycle } = task
  if (lifecycle.contentWarning !== undefined) return false
  const hasLocalArtifact = lifecycle.kind === 'ready-to-save' ||
    (lifecycle.kind === 'download-started' && lifecycle.retained)
  if (!hasLocalArtifact) return false
  const save = admitRetainedAction(task, 'save', admission)
  // Transient local-slot contention disables the button in place without invalidating the local copy.
  return save.allowed || save.reason === 'busy'
}

function retainedBannerMatch(
  task: RetainedTaskSnapshot,
  admission: RetainedAdmissionContext,
): Extract<WorkspaceRetainedMatch, { kind: 'banner' }> | null {
  if (task.lifecycle.kind === 'ready-to-save' && admitRetainedAction(task, 'save', admission).allowed) {
    return Object.freeze({ kind: 'banner', bannerKind: 'ready-to-save', task, action: 'save' })
  }
  if (task.lifecycle.kind === 'paused' && admitRetainedAction(task, 'continue', admission).allowed) {
    return Object.freeze({ kind: 'banner', bannerKind: 'continue', task, action: 'continue' })
  }
  return null
}

function isTransientlyBlockedContinuation(
  task: RetainedTaskSnapshot,
  admission: RetainedAdmissionContext,
): boolean {
  if (task.lifecycle.kind !== 'paused') return false
  const result = admitRetainedAction(task, 'continue', admission)
  return !result.allowed && (result.reason === 'busy' || result.reason === 'downloading')
}

function resolveSelectionRetainedMatch(
  shareTasks: readonly RetainedTaskSnapshot[],
  selectionKey: string,
  admission: RetainedAdmissionContext,
): WorkspaceRetainedMatch {
  let selectionBanner: Extract<WorkspaceRetainedMatch, { kind: 'banner' }> | null = null
  for (const task of shareTasks) {
    if (task.selectionKey !== selectionKey) continue
    if (isCompleteLocalRetained(task, admission)) {
      return Object.freeze({ kind: 'local-complete', task })
    }
    if (task.lifecycle.kind === 'paused') {
      const banner = retainedBannerMatch(task, admission)
      if (banner !== null) return selectionBanner ?? banner
      if (isTransientlyBlockedContinuation(task, admission)) {
        return selectionBanner ?? Object.freeze({ kind: 'pending-continuation' })
      }
      continue
    }
    selectionBanner ??= retainedBannerMatch(task, admission)
  }
  return selectionBanner ?? Object.freeze({ kind: 'none' })
}

/**
 * Single source of truth for matching persisted tasks to an idle share workspace.
 * Exact selection matches take precedence over share-wide banners:
 * - A complete local OPFS copy promotes directly so remote saving folds behind it.
 * - A newer continuable attempt for the same selection supersedes older local copies.
 * - Transient busy/downloading contention suppresses fallback so the workspace stays stable.
 * - Share-wide banners only appear when the current selection has no retained match.
 */
export function resolveWorkspaceRetainedMatch(input: WorkspaceRetainedMatchInput): WorkspaceRetainedMatch {
  const { tasks, shareInstance, selectionKey, admission, isIgnored } = input
  if (shareInstance === null) return Object.freeze({ kind: 'none' })

  const shareTasks = tasks
    .filter(task => task.shareInstance === shareInstance && isIgnored?.(task) !== true)
    .sort(byNewestCreatedDesc)
  if (shareTasks.length === 0) return Object.freeze({ kind: 'none' })

  if (selectionKey !== null) {
    const selectionMatch = resolveSelectionRetainedMatch(shareTasks, selectionKey, admission)
    if (selectionMatch.kind !== 'none') return selectionMatch
  }

  for (const task of shareTasks) {
    const banner = retainedBannerMatch(task, admission)
    if (banner !== null) return banner
  }
  return Object.freeze({ kind: 'none' })
}

/**
 * Loads all persisted tasks from `ReceiveStore`, normalizing any abandoned `downloading`
 * task whose tab crashed (no active Web Lock) into `paused (interrupted)`.
 */
export async function loadRetainedTasks(
  env: RetainedTaskEnvironment,
  isLocalTask: (taskId: string) => boolean = () => false,
): Promise<readonly RetainedTaskSnapshot[]> {
  const records = await env.store.listTasks()
  return resolveRetainedCandidates(env, records, isLocalTask)
}

/** Normalizes abandoned records and checks cross-tab locks only for the given candidate subset. */
export async function resolveRetainedCandidates(
  env: RetainedTaskEnvironment,
  records: readonly ReceiveTaskRecord[],
  isLocalTask: (taskId: string) => boolean,
): Promise<readonly RetainedTaskSnapshot[]> {
  const snapshots: RetainedTaskSnapshot[] = []
  for (const record of records) {
    env.signal?.throwIfAborted()
    try {
      const normalized = await normalizeAbandonedTask(record, env, isLocalTask)
      if (normalized === undefined) continue
      const activeElsewhere = !isLocalTask(normalized.taskId) && await isTaskLockedElsewhere(normalized.taskId, env.locks)
      snapshots.push(retainedSnapshot(normalized, activeElsewhere))
    } catch (error) {
      env.signal?.throwIfAborted()
      traceInventoryFailure(env, record, 'read_failed', error)
    }
  }
  return Object.freeze(snapshots)
}

export function retainedSnapshot(record: ReceiveTaskRecord, activeElsewhere: boolean): RetainedTaskSnapshot {
  const canSavePartial = canSavePartialState(record.lifecycle, record.backend, record.target.kind === 'opfs' && record.target.isZip)
  return Object.freeze({
    taskId: record.taskId, shareInstance: record.shareInstance, selectionKey: record.selectionKey,
    backend: record.backend, lifecycle: record.lifecycle, activeElsewhere, canSavePartial,
    ...(record.display === undefined ? {} : { display: record.display }),
    ...(record.safeRenamed === undefined ? {} : { safeRenamed: record.safeRenamed }),
    ...(record.sourceRevisionFailures === undefined ? {} : { sourceRevisionFailures: record.sourceRevisionFailures }),
    actions: actionsForState(record.lifecycle, record.backend, canSavePartial),
  })
}

function traceInventoryFailure(env: RetainedTaskEnvironment, record: ReceiveTaskRecord, transition: string, error: unknown): void {
  emitOutputTrace(env.outputTrace, 'continuation', {
    task_id: record.taskId, backend: diagnosticBackendName(record.backend), transition, error_class: classifyNativeError(error),
  })
}

type RetainedActionOutcome = Readonly<{ kind: 'completed'; updated?: ReceiveTaskRecord }>

async function readRetainedTask(env: RetainedTaskEnvironment, taskId: string): Promise<ReceiveTaskRecord> {
  const record = await env.store.getTask(taskId)
  env.signal?.throwIfAborted()
  if (record === undefined) throw new DOMException('This download record no longer exists', 'NotFoundError')
  return record
}

export async function executeRetainedTaskAction(
  env: RetainedTaskEnvironment,
  taskId: string,
  action: Exclude<ReceiveAction, 'continue'>,
): Promise<RetainedActionOutcome> {
  if (action === 'forget') return forgetRetainedTask(env, taskId)
  const record = await readRetainedTask(env, taskId)
  switch (action) {
    case 'discard':
      return removeRetainedTask(env, record)
    case 'save':
      return saveRetainedTask(env, record)
    case 'save-partial':
      return exportPartialRetainedTask(env, record)
    case 'pause':
    case 'stop':
      throw new DOMException('Only an active download can be paused or stopped', 'InvalidStateError')
  }
}

function retainedDiagnostics(env: RetainedTaskEnvironment, record: ReceiveTaskRecord): OutputDiagnosticsPorts {
  return { backend: diagnosticBackendName(record.backend), ...(env.outputTrace === undefined ? {} : { trace: env.outputTrace }) }
}

async function forgetRetainedTask(env: RetainedTaskEnvironment, taskId: string): Promise<RetainedActionOutcome> {
  const lock = await acquireTaskLock(taskId, env.locks)
  try {
    const record = await readRetainedTask(env, taskId)
    if (!canForgetState(record.lifecycle, record.backend)) {
      throw new DOMException('Remove retained download data explicitly instead of forgetting history', 'InvalidStateError')
    }
    await env.store.deleteTask(taskId)
    if (!actionsForState(record.lifecycle, record.backend).includes('forget')) {
      emitOutputTrace(env.outputTrace, 'cleanup', {
        backend: diagnosticBackendName(record.backend),
        task_id: taskId,
        transition: 'forgotten_without_disk_cleanup',
      })
    }
    return { kind: 'completed' }
  } finally {
    await lock.release()
  }
}

async function removeRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend === 'opfs') {
    await deleteOPFSTask(record, env.store, resolveOPFSEnv(env), env.signal)
  } else if (record.backend === 'fsa') {
    await discardFSATask(record, env.store, env.locks, retainedDiagnostics(env, record), env.signal)
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
  const updated = await saveRetainedOPFSTask(record, env.store, resolveOPFSEnv(env), env.windowPort, env.signal)
  return { kind: 'completed', updated }
}

async function exportPartialRetainedTask(
  env: RetainedTaskEnvironment,
  record: ReceiveTaskRecord,
): Promise<RetainedActionOutcome> {
  if (record.backend !== 'opfs') {
    throw new DOMException('Partial ZIP export is only available for browser-stored ZIP downloads', 'InvalidStateError')
  }
  await exportPartialOPFSZip(record, env.store, resolveOPFSEnv(env), env.windowPort, env.signal)
  return { kind: 'completed' }
}

export async function reopenRetainedTask(
  env: RetainedTaskEnvironment, taskId: string, currentShare: () => JoinedBrowserShare | undefined,
): Promise<Readonly<{ joined: JoinedBrowserShare; task: ReceiveTaskRecord; session: OutputSessionHandle }>> {
  const task = await readRetainedTask(env, taskId)
  const joined = currentShare()
  if (joined === undefined || joined.descriptor.shareInstanceId !== task.shareInstance) throw matchingShareRequired()
  if (task.lifecycle.kind !== 'paused' && task.lifecycle.kind !== 'downloading' && task.lifecycle.kind !== 'preparing') {
    throw new DOMException('This download is no longer resumable', 'InvalidStateError')
  }
  const session = await reopenOutput(env, task)
  // The session owns the Web Lock. Restore the previous intent before releasing it if the share changed mid-open.
  if (env.signal?.aborted) return { joined, task, session }
  const current = currentShare()
  if (current?.descriptor.shareInstanceId !== task.shareInstance) {
    try {
      const counts = await readCheckpointCounts(task, env.store)
      const { completedFiles, completedBytes, retainedBytes } = counts
      const lifecycle = task.lifecycle.kind === 'paused'
        ? { ...task.lifecycle, completedFiles, completedBytes, retainedBytes }
        : transitionReceiveState(task.lifecycle, { kind: 'paused', reason: 'interrupted', ...counts })
      await env.store.putTask({ ...task, lifecycle })
    } finally {
      await closeOutputSession(session, env.outputTrace)
    }
    throw matchingShareRequired()
  }
  return { joined: current, task, session }
}

function matchingShareRequired(): DOMException {
  return new DOMException('Open the matching share link to continue this download', 'InvalidStateError')
}

async function reopenOutput(env: RetainedTaskEnvironment, record: ReceiveTaskRecord): Promise<OutputSessionHandle> {
  if (record.backend === 'fsa') {
    return reopenFSASession({
      task: record,
      store: env.store,
      allowPermissionPrompt: true,
      ...(env.signal === undefined ? {} : { signal: env.signal }),
      ...(env.locks === undefined ? {} : { locks: env.locks }),
      diagnostics: retainedDiagnostics(env, record),
    })
  }
  if (record.backend === 'opfs') {
    return reopenOPFSSession({
      task: record,
      store: env.store,
      env: resolveOPFSEnv(env),
      windowPort: env.windowPort,
      diagnostics: retainedDiagnostics(env, record),
    })
  }
  throw new DOMException('This download cannot be resumed', 'NotSupportedError')
}

function resolveOPFSEnv(env: RetainedTaskEnvironment): OPFSEnvironment {
  return env.opfsEnv ?? browserOPFSEnvironment(env.windowPort.navigator?.storage, env.locks)
}

async function normalizeAbandonedTask(
  record: ReceiveTaskRecord,
  env: RetainedTaskEnvironment,
  isLocalTask: (taskId: string) => boolean,
): Promise<ReceiveTaskRecord | undefined> {
  if (record.lifecycle.kind !== 'downloading' && record.lifecycle.kind !== 'preparing') {
    return record
  }
  if (isLocalTask(record.taskId)) return record
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

async function readCheckpointCounts(record: ReceiveTaskRecord, store: ReceiveStore) {
  const files = await store.listTaskFiles(record.taskId)
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
  return { completedFiles, completedBytes, retainedBytes, discoveredFiles, discoveredBytes, discoveryComplete: record.discoveryComplete }
}

async function pauseFromCheckpoints(record: ReceiveTaskRecord, env: RetainedTaskEnvironment): Promise<ReceiveTaskRecord> {
  const checkpoint = await readCheckpointCounts(record, env.store)
  const updatedAt = Date.now()
  const pause = (reason: 'interrupted' | 'storage-full' | 'permission-needed'): ReceiveTaskRecord => ({
    ...record, updatedAt, lifecycle: transitionReceiveState(record.lifecycle, { kind: 'paused', reason, ...checkpoint }, updatedAt),
  })
  const updated = pause('interrupted')
  env.signal?.throwIfAborted()
  try {
    await env.store.putTask(updated)
    return updated
  } catch (error) {
    // The file rows still describe the durable bytes. Discard must remain reachable even when IDB is full.
    traceInventoryFailure(env, record, 'pause_persist_failed', error)
    const reason = storagePauseReason(error)
    return reason === undefined ? updated : pause(reason)
  }
}
