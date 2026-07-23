import type { OutputSessionIdentity } from '../../transfer/output-session'

interface LockHandle {
  readonly name: string
}

interface LockManagerRuntime {
  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: LockHandle | null) => Promise<void>,
  ): Promise<void>
}

export interface BrowserOutputSessionLease {
  release(): Promise<void>
}

/** A browser lock prevents two tabs from publishing competing checkpoint heads. */
export async function acquireBrowserOutputSessionLease(
  identity: OutputSessionIdentity,
): Promise<BrowserOutputSessionLease> {
  const manager = (navigator as Navigator & { readonly locks?: LockManagerRuntime }).locks
  if (manager === undefined) {
    throw new DOMException('Persistent output requires the Web Locks API', 'NotSupportedError')
  }

  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => { releaseResolve = resolve })
  const completion = manager.request(
    `windshare-output:${identity.backend}:${identity.outputSessionId}`,
    { mode: 'exclusive', ifAvailable: true },
    async (lock) => {
      if (lock === null) {
        acquiredReject(new DOMException(
          'This output session is already active in another page',
          'InvalidStateError',
        ))
        return
      }
      acquiredResolve()
      await held
    },
  )
  completion.then(undefined, acquiredReject)
  await acquired

  let released = false
  return Object.freeze({
    release: async () => {
      if (released) return
      released = true
      releaseResolve()
      await completion
    },
  })
}
