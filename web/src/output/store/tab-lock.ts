const TASK_LOCK_PREFIX = 'windshare-receive:'

export interface TaskLock {
  readonly name: string
  release(): Promise<void>
}

export class TaskBusyError extends DOMException {
  readonly taskId: string

  constructor(taskId: string) {
    super('This download is already active in another WindShare tab', 'InvalidStateError')
    this.taskId = taskId
  }
}

export function taskLockName(taskId: string): string {
  return `${TASK_LOCK_PREFIX}${taskId}`
}

/**
 * Acquires a non-blocking cross-tab Web Lock for `taskId`.
 * A crashed tab releases its Web Lock automatically without stale IDB lease cleanup.
 */
export async function acquireTaskLock(
  taskId: string,
  locks: LockManager | undefined = globalThis.navigator?.locks,
): Promise<TaskLock> {
  const name = taskLockName(taskId)
  if (locks === undefined) {
    return Object.freeze({ name, release: () => Promise.resolve() })
  }

  let resolveAcquired!: () => void
  let rejectAcquired!: (error: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    resolveAcquired = resolve
    rejectAcquired = reject
  })

  let releaseHold!: () => void
  const hold = new Promise<void>(resolve => { releaseHold = resolve })

  const completion = locks.request(name, { mode: 'exclusive', ifAvailable: true }, async lock => {
    if (lock === null) {
      rejectAcquired(new TaskBusyError(taskId))
      return
    }
    resolveAcquired()
    await hold
  })
  completion.catch(rejectAcquired)
  await acquired

  let releasePromise: Promise<void> | undefined
  return Object.freeze({
    name,
    release: () => {
      releasePromise ??= (async () => {
        releaseHold()
        await completion.catch(() => undefined)
      })()
      return releasePromise
    },
  })
}

export async function isTaskLockedElsewhere(
  taskId: string,
  locks: LockManager | undefined = globalThis.navigator?.locks,
): Promise<boolean> {
  if (locks === undefined) return false
  let busy = false
  await locks.request(taskLockName(taskId), { mode: 'exclusive', ifAvailable: true }, async lock => {
    busy = lock === null
  })
  return busy
}
