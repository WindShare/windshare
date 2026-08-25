import { encodeBase64Url } from '../../crypto/bytes'
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
const LEGACY_FSA_MAXIMUM_ACTIVE_WRITERS = 1

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

/**
 * This temporary root-wide port keeps the current tree compileable until authenticated
 * parent authorities replace it. New scheduler code must use the narrower kind model.
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
  maximumActiveWriters: number = LEGACY_FSA_MAXIMUM_ACTIVE_WRITERS,
  performance?: PerformanceSummaryObservations,
): Promise<FSARootMutationLease> {
  const lockName = await fsaRootMutationLockName(parent)
  const rootParent = Symbol(lockName) as FSAParentMutationIdentity
  const scheduler = createFSAOperationMutationScheduler({
    rootParent,
    maximumActiveWriters,
    ...(performance === undefined ? {} : { performance }),
  })
  const authority = new SerializedFSARootMutationAuthority(scheduler, rootParent, performance)
  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => { releaseResolve = resolve })
  const completion = manager.request(
    lockName,
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
    scheduler,
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
    case 'settle-operation':
    case 'remove-entry':
      return
  }
  throw new TypeError('FSA namespace mutation kind is invalid')
}
