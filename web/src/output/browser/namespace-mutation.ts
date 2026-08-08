import { encodeBase64Url } from '../../crypto/bytes'
import { FILE_SYSTEM_ACCESS_BACKEND } from '../capability/contract'
import {
  durableCheckpointNamespaceIdentity,
  type DurableCheckpointNamespaceIdentity,
} from '../persistence/namespace'

const FILE_SYSTEM_ROOT_LOCK_DOMAIN = 'windshare-output-root'

export interface BrowserLockHandle {
  readonly name: string
}

export interface BrowserLockManagerRuntime {
  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable?: true },
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void>
}

export type BrowserOutputSessionBusyScope = 'intent-namespace' | 'file-system-root'

export class BrowserOutputSessionBusyError extends DOMException {
  readonly scope: BrowserOutputSessionBusyScope
  readonly binding: DurableCheckpointNamespaceIdentity

  constructor(
    scope: BrowserOutputSessionBusyScope,
    binding: DurableCheckpointNamespaceIdentity,
  ) {
    super(busyMessage(scope), 'InvalidStateError')
    this.scope = scope
    this.binding = durableCheckpointNamespaceIdentity(binding)
  }
}

export class BrowserFileSystemMutationClosedError extends DOMException {
  readonly binding: DurableCheckpointNamespaceIdentity

  constructor(binding: DurableCheckpointNamespaceIdentity) {
    super('The File System Access root mutation authority is closed', 'InvalidStateError')
    this.binding = durableCheckpointNamespaceIdentity(binding)
  }
}

export type BrowserFileSystemMutationKind =
  | 'ensure-directory'
  | 'create-file'
  | 'remove-file'
  | 'remove-directory'

export interface BrowserFileSystemMutationAuthority {
  readonly binding: DurableCheckpointNamespaceIdentity
  mutate<T>(
    kind: BrowserFileSystemMutationKind,
    path: readonly string[],
    operation: () => Promise<T>,
  ): Promise<T>
}

export interface BrowserFileSystemMutationLease {
  readonly authority: BrowserFileSystemMutationAuthority
  release(): Promise<void>
}

/**
 * The intent digest is deliberately absent: two intents may have independent
 * journals while still targeting the same mutable picker root.
 */
export function browserFileSystemRootMutationLockName(
  input: DurableCheckpointNamespaceIdentity,
): string {
  const binding = requireFileSystemAccessBinding(input)
  const rootDomain = `${binding.backend}\0${binding.rootIdentity}`
  return `${FILE_SYSTEM_ROOT_LOCK_DOMAIN}:${encodeBase64Url(new TextEncoder().encode(rootDomain))}`
}

export async function acquireBrowserFileSystemMutationLease(
  input: DurableCheckpointNamespaceIdentity,
  manager: BrowserLockManagerRuntime,
): Promise<BrowserFileSystemMutationLease> {
  const binding = requireFileSystemAccessBinding(input)
  const authority = new SerializedFileSystemMutationAuthority(binding)
  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => { releaseResolve = resolve })
  const completion = manager.request(
    browserFileSystemRootMutationLockName(binding),
    { mode: 'exclusive', ifAvailable: true },
    async (lock) => {
      if (lock === null) {
        acquiredReject(new BrowserOutputSessionBusyError('file-system-root', binding))
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
        // Draining accepted mutations before releasing the cross-realm lock keeps
        // a late cleanup from crossing into the next intent's ownership window.
        await authority.close()
        releaseResolve()
        await completion
      })()
      return releasePromise
    },
  })
}

class SerializedFileSystemMutationAuthority implements BrowserFileSystemMutationAuthority {
  readonly binding: DurableCheckpointNamespaceIdentity
  #accepting = true
  #tail: Promise<void> = Promise.resolve()

  constructor(binding: DurableCheckpointNamespaceIdentity) {
    this.binding = binding
  }

  async mutate<T>(
    kind: BrowserFileSystemMutationKind,
    path: readonly string[],
    operation: () => Promise<T>,
  ): Promise<T> {
    requireMutation(kind, path)
    if (!this.#accepting) throw new BrowserFileSystemMutationClosedError(this.binding)

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

function requireFileSystemAccessBinding(
  input: DurableCheckpointNamespaceIdentity,
): DurableCheckpointNamespaceIdentity {
  const binding = durableCheckpointNamespaceIdentity(input)
  if (binding.backend !== FILE_SYSTEM_ACCESS_BACKEND) {
    throw new TypeError('File System Access mutation authority requires the File System Access backend')
  }
  return binding
}

function requireMutation(
  kind: BrowserFileSystemMutationKind,
  path: readonly string[],
): void {
  if (kind !== 'ensure-directory' && kind !== 'create-file' &&
      kind !== 'remove-file' && kind !== 'remove-directory') {
    throw new TypeError('File System Access mutation kind is invalid')
  }
  if (path.length === 0 || path.some((segment) => typeof segment !== 'string' || segment.length === 0)) {
    throw new TypeError('File System Access mutation path is invalid')
  }
}

function busyMessage(scope: BrowserOutputSessionBusyScope): string {
  return scope === 'intent-namespace'
    ? 'This checkpoint namespace is already active in another page'
    : 'This File System Access root is already active in another transfer'
}
