import {
  acquireBrowserMutationLease,
  browserLockManager,
  type BrowserLockManagerRuntime,
  type FSAMutationLease,
} from './mutation-coordination/web-lock'
import {
  FSAHandleIdentityRegistry,
  type FSAHandleIdentityResolver,
} from './mutation-coordination/handle-identity'
import {
  createFSAOperationMutationScheduler,
} from './mutation-coordination/scheduler'
import type {
  FSAOperationMutationScheduler,
  FSAParentMutationIdentity,
} from './mutation-coordination/model'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../diagnostics/performance-summary'
import type { PerformanceNamespaceKindV1 } from '../../diagnostics/trace/transfer-payload'

const FSA_ROOT_LOCK_DOMAIN = 'windshare/fsa-parent-lock/v1'
const FSA_NAMESPACE_LOCK_DOMAIN = 'windshare/fsa-parent-namespace-lock/v1'
const FSA_ENTRY_LOCK_DOMAIN = 'windshare/fsa-entry-lock/v1'
const LEGACY_FSA_MAXIMUM_ACTIVE_WRITERS = 1

export type {
  BrowserLockHandle,
  BrowserLockManagerRuntime,
  BrowserMutationLockOptions,
  FSAMutationLease,
} from './mutation-coordination/web-lock'
export {
  FSAHandleIdentityRegistry,
  type FSAHandleIdentityResolver,
} from './mutation-coordination/handle-identity'

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

export class FSAEntryMutationBusyError extends DOMException {
  readonly scope = 'fsa-entry' as const

  constructor() {
    super('This file is already being changed by another WindShare task', 'InvalidStateError')
  }
}

/**
 * Tree output retains root authority until its operation-local writer scheduler drains.
 * Single-file output uses shared parent access with explicit namespace and entry leases.
 */
export type FSANamespaceMutationKind =
  | 'reserve-name'
  | 'create-directory'
  | 'create-file'
  | 'settle-operation'
  | 'remove-entry'

export interface FSARootMutationAuthority {
  readonly scheduler: FSAOperationMutationScheduler
  readonly rootParentIdentity: FSAParentMutationIdentity
  readonly performance?: PerformanceSummaryObservations
  registerAuthorityRelease(release: () => void): void
  run<T>(kind: FSANamespaceMutationKind, operation: () => Promise<T>): Promise<T>
}

export interface FSARootMutationLease {
  readonly authority: FSARootMutationAuthority
  readonly scheduler: FSAOperationMutationScheduler
  release(): Promise<void>
}

export async function fsaRootMutationLockName(
  parent: FileSystemDirectoryHandle,
  identities: FSAHandleIdentityResolver = new FSAHandleIdentityRegistry(),
): Promise<string> {
  if (parent.kind !== 'directory') {
    throw new TypeError('FSA parent lock requires a directory authority')
  }
  return `${FSA_ROOT_LOCK_DOMAIN}:${await identities.resolve(parent)}`
}

/** Shared access excludes legacy tree writers while allowing independent file owners. */
export async function acquireFSAParentAccessLease(
  parent: FileSystemDirectoryHandle,
  manager: BrowserLockManagerRuntime = browserLockManager(),
  identities: FSAHandleIdentityResolver = new FSAHandleIdentityRegistry({ manager }),
): Promise<FSAMutationLease> {
  return acquireBrowserMutationLease(
    await fsaRootMutationLockName(parent, identities),
    manager,
    { mode: 'shared', ifAvailable: true },
    () => new FSARootMutationBusyError(),
  )
}

/** Callers hold parent access while briefly serializing name inspection and mutation. */
export async function acquireFSAParentNamespaceLease(
  parent: FileSystemDirectoryHandle,
  manager: BrowserLockManagerRuntime = browserLockManager(),
  identities: FSAHandleIdentityResolver = new FSAHandleIdentityRegistry({ manager }),
): Promise<FSAMutationLease> {
  if (parent.kind !== 'directory') {
    throw new TypeError('FSA namespace lock requires a directory authority')
  }
  return acquireBrowserMutationLease(
    `${FSA_NAMESPACE_LOCK_DOMAIN}:${await identities.resolve(parent)}`,
    manager,
    { mode: 'exclusive' },
    () => new FSARootMutationBusyError(),
  )
}

export async function acquireFSAEntryMutationLease(
  handle: FileSystemFileHandle,
  manager: BrowserLockManagerRuntime = browserLockManager(),
  identities: FSAHandleIdentityResolver = new FSAHandleIdentityRegistry({ manager }),
): Promise<FSAMutationLease> {
  if (handle.kind !== 'file') {
    throw new TypeError('FSA entry mutation lock requires a file authority')
  }
  return acquireBrowserMutationLease(
    `${FSA_ENTRY_LOCK_DOMAIN}:${await identities.resolve(handle)}`,
    manager,
    { mode: 'exclusive', ifAvailable: true },
    () => new FSAEntryMutationBusyError(),
  )
}

export async function acquireFSARootMutationLease(
  parent: FileSystemDirectoryHandle,
  manager: BrowserLockManagerRuntime = browserLockManager(),
  maximumActiveWriters: number = LEGACY_FSA_MAXIMUM_ACTIVE_WRITERS,
  performance?: PerformanceSummaryObservations,
  identities: FSAHandleIdentityResolver = new FSAHandleIdentityRegistry({ manager }),
): Promise<FSARootMutationLease> {
  const lockName = await fsaRootMutationLockName(parent, identities)
  const rootParent = Symbol(lockName) as FSAParentMutationIdentity
  const scheduler = createFSAOperationMutationScheduler({
    rootParent,
    maximumActiveWriters,
    ...(performance === undefined ? {} : { performance }),
  })
  const authority = new SerializedFSARootMutationAuthority(scheduler, rootParent, performance)
  const lease = await acquireBrowserMutationLease(
    lockName,
    manager,
    { mode: 'exclusive', ifAvailable: true },
    () => new FSARootMutationBusyError(),
  )

  let releasePromise: Promise<void> | undefined
  return Object.freeze({
    authority,
    scheduler,
    release: () => {
      releasePromise ??= (async () => {
        await authority.close()
        await lease.release()
      })()
      return releasePromise
    },
  })
}

class SerializedFSARootMutationAuthority implements FSARootMutationAuthority {
  readonly scheduler: FSAOperationMutationScheduler
  readonly rootParentIdentity: FSAParentMutationIdentity
  readonly performance?: PerformanceSummaryObservations
  readonly #authorityReleases = new Set<() => void>()
  #accepting = true
  #tail: Promise<void> = Promise.resolve()
  #closePromise: Promise<void> | undefined

  constructor(
    scheduler: FSAOperationMutationScheduler,
    rootParentIdentity: FSAParentMutationIdentity,
    performance: PerformanceSummaryObservations | undefined,
  ) {
    this.scheduler = scheduler
    this.rootParentIdentity = rootParentIdentity
    if (performance !== undefined) this.performance = performance
  }

  registerAuthorityRelease(release: () => void): void {
    if (!this.#accepting) throw new FSARootMutationClosedError()
    this.#authorityReleases.add(release)
  }

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
    const queuedAtMilliseconds = performanceNowMilliseconds(this.performance)
    await predecessor
    const startedAtMilliseconds = performanceNowMilliseconds(this.performance)
    let succeeded = false
    try {
      const result = await operation()
      succeeded = true
      return result
    } finally {
      if (succeeded) {
        const completedAtMilliseconds = performanceNowMilliseconds(this.performance)
        const waitMilliseconds = performanceElapsedMilliseconds(
          queuedAtMilliseconds,
          startedAtMilliseconds,
        )
        const runMilliseconds = performanceElapsedMilliseconds(
          startedAtMilliseconds,
          completedAtMilliseconds,
        )
        if (waitMilliseconds !== undefined && runMilliseconds !== undefined) {
          observePerformance(this.performance, summary =>
            summary.observeQueueRun(
              'namespace',
              waitMilliseconds,
              runMilliseconds,
              performanceNamespaceKind(kind),
            ))
        }
      }
      finish()
    }
  }

  close(): Promise<void> {
    if (this.#closePromise !== undefined) return this.#closePromise
    this.#accepting = false
    this.#closePromise = (async () => {
      await this.#tail
      await this.scheduler.close()
      for (const release of this.#authorityReleases) release()
      this.#authorityReleases.clear()
    })()
    return this.#closePromise
  }
}

function performanceNamespaceKind(kind: FSANamespaceMutationKind): PerformanceNamespaceKindV1 {
  switch (kind) {
    case 'reserve-name': return 'reserve_name'
    case 'create-directory': return 'create_directory'
    case 'create-file': return 'create_file'
    case 'settle-operation': return 'settle_operation'
    case 'remove-entry': return 'remove_entry'
  }
}

function requireMutationKind(kind: FSANamespaceMutationKind): void {
  switch (kind) {
    case 'reserve-name':
    case 'create-directory':
    case 'create-file':
    case 'settle-operation':
    case 'remove-entry':
      return
  }
  throw new TypeError('FSA namespace mutation kind is invalid')
}
