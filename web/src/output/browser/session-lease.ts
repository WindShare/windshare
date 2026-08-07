import { encodeBase64Url } from '../../crypto/bytes'
import {
  durableCheckpointNamespaceKey,
  type DurableCheckpointNamespaceIdentity,
} from '../persistence/namespace'

interface LockHandle {
  readonly name: string
}

interface LockManagerRuntime {
  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable?: true },
    callback: (lock: LockHandle | null) => Promise<void>,
  ): Promise<void>
}

export interface BrowserOutputSessionLease {
  release(): Promise<void>
}

/** A browser lock prevents two tabs from publishing competing checkpoint heads. */
export async function acquireBrowserOutputSessionLease(
  identity: DurableCheckpointNamespaceIdentity,
): Promise<BrowserOutputSessionLease> {
  const namespace = durableCheckpointNamespaceKey(identity)
  return acquireBrowserLease(
    `windshare-output:${encodeBase64Url(new TextEncoder().encode(namespace))}`,
    true,
    'This checkpoint namespace is already active in another page',
  )
}

/** Serializes the one-shot legacy-store cleaner across every page in the origin. */
export function acquireBrowserCheckpointCleanupLease(
  databaseName: string,
): Promise<BrowserOutputSessionLease> {
  if (databaseName.length === 0) throw new TypeError('checkpoint database name must not be empty')
  return acquireBrowserLease(
    `windshare-output-cleanup:${encodeBase64Url(new TextEncoder().encode(databaseName))}`,
    false,
    'Checkpoint cleanup is already active in another page',
  )
}

async function acquireBrowserLease(
  name: string,
  failWhenUnavailable: boolean,
  unavailableMessage: string,
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
    name,
    failWhenUnavailable ? { mode: 'exclusive', ifAvailable: true } : { mode: 'exclusive' },
    async (lock) => {
      if (lock === null) {
        acquiredReject(new DOMException(
          unavailableMessage,
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
