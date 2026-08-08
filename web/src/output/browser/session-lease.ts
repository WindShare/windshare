import { encodeBase64Url } from '../../crypto/bytes'
import {
  durableCheckpointNamespaceKey,
  durableCheckpointNamespaceIdentity,
  type DurableCheckpointNamespaceIdentity,
} from '../persistence/namespace'
import {
  acquireBrowserFileSystemMutationLease,
  BrowserOutputSessionBusyError,
  type BrowserFileSystemMutationAuthority,
  type BrowserLockManagerRuntime,
} from './namespace-mutation'

export { BrowserOutputSessionBusyError } from './namespace-mutation'

export interface BrowserOutputSessionLease {
  release(): Promise<void>
}

export interface BrowserFileSystemAccessSessionLease extends BrowserOutputSessionLease {
  readonly mutations: BrowserFileSystemMutationAuthority
}

/** A browser lock prevents two tabs from publishing competing checkpoint heads. */
export async function acquireBrowserOutputSessionLease(
  input: DurableCheckpointNamespaceIdentity,
  manager: BrowserLockManagerRuntime = browserLockManager(),
): Promise<BrowserOutputSessionLease> {
  const identity = durableCheckpointNamespaceIdentity(input)
  const namespace = durableCheckpointNamespaceKey(identity)
  return acquireBrowserLease(
    `windshare-output:${encodeBase64Url(new TextEncoder().encode(namespace))}`,
    true,
    () => new BrowserOutputSessionBusyError('intent-namespace', identity),
    manager,
  )
}

/**
 * FSA roots are shared mutable capabilities, so their lock omits the intent
 * digest and is held alongside the intent-specific checkpoint lease.
 */
export async function acquireBrowserFileSystemAccessSessionLease(
  input: DurableCheckpointNamespaceIdentity,
  manager: BrowserLockManagerRuntime = browserLockManager(),
): Promise<BrowserFileSystemAccessSessionLease> {
  const identity = durableCheckpointNamespaceIdentity(input)
  const namespaceLease = await acquireBrowserOutputSessionLease(identity, manager)
  try {
    const rootLease = await acquireBrowserFileSystemMutationLease(identity, manager)
    let releasePromise: Promise<void> | undefined
    return Object.freeze({
      mutations: rootLease.authority,
      release: () => {
        releasePromise ??= releaseFileSystemAccessLeases(rootLease, namespaceLease)
        return releasePromise
      },
    })
  } catch (error) {
    try {
      await namespaceLease.release()
    } catch (releaseError) {
      throw new AggregateError(
        [error, releaseError],
        'File System Access root contention could not release the intent lease',
        { cause: releaseError },
      )
    }
    throw error
  }
}

/** Serializes the one-shot legacy-store cleaner across every page in the origin. */
export function acquireBrowserCheckpointCleanupLease(
  databaseName: string,
  manager: BrowserLockManagerRuntime = browserLockManager(),
): Promise<BrowserOutputSessionLease> {
  if (databaseName.length === 0) throw new TypeError('checkpoint database name must not be empty')
  return acquireBrowserLease(
    `windshare-output-cleanup:${encodeBase64Url(new TextEncoder().encode(databaseName))}`,
    false,
    () => new DOMException('Checkpoint cleanup is already active in another page', 'InvalidStateError'),
    manager,
  )
}

async function acquireBrowserLease(
  name: string,
  failWhenUnavailable: boolean,
  unavailable: () => unknown,
  manager: BrowserLockManagerRuntime,
): Promise<BrowserOutputSessionLease> {
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
        acquiredReject(unavailable())
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

async function releaseFileSystemAccessLeases(
  rootLease: BrowserOutputSessionLease,
  namespaceLease: BrowserOutputSessionLease,
): Promise<void> {
  const failures: unknown[] = []
  try {
    await rootLease.release()
  } catch (error) {
    failures.push(error)
  }
  try {
    await namespaceLease.release()
  } catch (error) {
    failures.push(error)
  }
  if (failures.length === 1) throw failures[0]
  if (failures.length > 1) {
    throw new AggregateError(failures, 'File System Access namespace leases could not be released')
  }
}

function browserLockManager(): BrowserLockManagerRuntime {
  const browserNavigator = globalThis.navigator as (Navigator & {
    readonly locks?: BrowserLockManagerRuntime
  }) | undefined
  const manager = browserNavigator?.locks
  if (manager === undefined) {
    throw new DOMException('Persistent output requires the Web Locks API', 'NotSupportedError')
  }
  return manager
}
