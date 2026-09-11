export interface BrowserLockHandle {
  readonly name: string
}

export interface BrowserMutationLockOptions {
  readonly mode: 'exclusive' | 'shared'
  readonly ifAvailable?: true
}

export interface BrowserLockManagerRuntime {
  request(
    name: string,
    options: BrowserMutationLockOptions,
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void>
}

export interface FSAMutationLease {
  readonly name: string
  release(): Promise<void>
}

export function browserLockManager(): BrowserLockManagerRuntime {
  const manager = globalThis.navigator?.locks
  if (manager === undefined) {
    throw new DOMException('Web Locks are required for coordinated FSA output', 'NotSupportedError')
  }
  return manager
}

export async function acquireBrowserMutationLease(
  name: string,
  manager: BrowserLockManagerRuntime,
  options: BrowserMutationLockOptions,
  busyError: () => Error,
): Promise<FSAMutationLease> {
  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => { releaseResolve = resolve })
  const completion = manager.request(name, options, async (lock) => {
    if (lock === null) {
      acquiredReject(busyError())
      return
    }
    acquiredResolve()
    await held
  })
  completion.then(undefined, acquiredReject)
  await acquired

  let releasePromise: Promise<void> | undefined
  return Object.freeze({
    name,
    release: () => {
      releasePromise ??= (async () => {
        releaseResolve()
        await completion
      })()
      return releasePromise
    },
  })
}
