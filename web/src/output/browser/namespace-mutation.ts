import { encodeBase64Url } from '../../crypto/bytes'

const FSA_ROOT_LOCK_DOMAIN = 'windshare/fsa-parent-lock/v1'

export interface BrowserLockHandle {
  readonly name: string
}

export interface BrowserLockManagerRuntime {
  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void>
}

export class FSARootMutationBusyError extends DOMException {
  readonly scope = 'fsa-parent' as const

  constructor() {
    super('This directory is already being changed by another WindShare task', 'InvalidStateError')
  }
}

export class FSARootMutationClosedError extends DOMException {
  constructor() {
    super('The File System Access mutation authority is closed', 'InvalidStateError')
  }
}

export type FSANamespaceMutationKind =
  | 'reserve-name'
  | 'create-directory'
  | 'create-file'
  | 'open-writer'
  | 'commit-file'
  | 'settle-operation'
  | 'remove-entry'

export interface FSARootMutationAuthority {
  run<T>(kind: FSANamespaceMutationKind, operation: () => Promise<T>): Promise<T>
}

export interface FSARootMutationLease {
  readonly authority: FSARootMutationAuthority
  release(): Promise<void>
}

/**
 * FSA does not expose a stable filesystem identifier before persistence. Hashing the
 * picker-visible leaf intentionally over-serializes same-named parents: false sharing
 * is harmless, whereas separate locks for one parent would invalidate no-replace.
 */
export async function fsaRootMutationLockName(
  parent: FileSystemDirectoryHandle,
): Promise<string> {
  if (parent.kind !== 'directory' || typeof parent.name !== 'string') {
    throw new TypeError('FSA root lock requires a named directory authority')
  }
  const material = new TextEncoder().encode(`${FSA_ROOT_LOCK_DOMAIN}\0${parent.name}`)
  const digest = await crypto.subtle.digest('SHA-256', material)
  return `${FSA_ROOT_LOCK_DOMAIN}:${encodeBase64Url(new Uint8Array(digest))}`
}

export async function acquireFSARootMutationLease(
  parent: FileSystemDirectoryHandle,
  manager: BrowserLockManagerRuntime = browserLockManager(),
): Promise<FSARootMutationLease> {
  const authority = new SerializedFSARootMutationAuthority()
  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => { releaseResolve = resolve })
  const completion = manager.request(
    await fsaRootMutationLockName(parent),
    { mode: 'exclusive', ifAvailable: true },
    async (lock) => {
      if (lock === null) {
        acquiredReject(new FSARootMutationBusyError())
        return
      }
      acquiredResolve()
      await held
    },
  )
  completion.then(undefined, acquiredReject)
  await acquired

  let releasePromise: Promise<void> | undefined
  return Object.freeze({
    authority,
    release: () => {
      releasePromise ??= (async () => {
        await authority.close()
        releaseResolve()
        await completion
      })()
      return releasePromise
    },
  })
}

class SerializedFSARootMutationAuthority implements FSARootMutationAuthority {
  #accepting = true
  #tail: Promise<void> = Promise.resolve()

  async run<T>(
    kind: FSANamespaceMutationKind,
    operation: () => Promise<T>,
  ): Promise<T> {
    requireMutationKind(kind)
    if (!this.#accepting) throw new FSARootMutationClosedError()
    const predecessor = this.#tail
    let finish!: () => void
    const current = new Promise<void>((resolve) => { finish = resolve })
    this.#tail = predecessor.then(() => current)
    await predecessor
    try {
      return await operation()
    } finally {
      finish()
    }
  }

  async close(): Promise<void> {
    this.#accepting = false
    await this.#tail
  }
}

function browserLockManager(): BrowserLockManagerRuntime {
  const manager = globalThis.navigator?.locks
  if (manager === undefined) {
    throw new DOMException('Web Locks are required for coordinated FSA output', 'NotSupportedError')
  }
  return manager as BrowserLockManagerRuntime
}

function requireMutationKind(kind: FSANamespaceMutationKind): void {
  switch (kind) {
    case 'reserve-name':
    case 'create-directory':
    case 'create-file':
    case 'open-writer':
    case 'commit-file':
    case 'settle-operation':
    case 'remove-entry':
      return
  }
  throw new TypeError('FSA namespace mutation kind is invalid')
}
