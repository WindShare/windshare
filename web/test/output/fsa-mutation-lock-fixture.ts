import {
  acquireFSARootMutationLease as acquireRootLease,
  FSAHandleIdentityRegistry,
  type BrowserLockHandle,
  type BrowserLockManagerRuntime,
  type BrowserMutationLockOptions,
  type FSAHandleIdentityResolver,
} from '../../src/output/browser/namespace-mutation'
import type {
  FSAHandleIdentityRecord,
  FSAHandleIdentityStore,
} from '../../src/output/browser/mutation-coordination/handle-identity'
import type { PerformanceSummaryObservations } from '../../src/output/diagnostics/performance-summary'

export class MemoryFSAHandleIdentityStore implements FSAHandleIdentityStore {
  readonly records: FSAHandleIdentityRecord[] = []

  async readAll(): Promise<readonly FSAHandleIdentityRecord[]> {
    return [...this.records]
  }

  async insert(handle: FileSystemHandle): Promise<string> {
    const identity = String(this.records.length + 1)
    this.records.push({ identity, handle })
    return identity
  }
}

interface LockRequest {
  readonly options: BrowserMutationLockOptions
  readonly callback: (lock: BrowserLockHandle | null) => Promise<void>
  readonly resolve: () => void
  readonly reject: (error: unknown) => void
}

export class MemoryMutationLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Map<string, BrowserMutationLockOptions[]>()
  readonly #queued = new Map<string, LockRequest[]>()

  request(
    name: string,
    options: BrowserMutationLockOptions,
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void> {
    return new Promise((resolve, reject) => {
      const request = { options, callback, resolve, reject }
      const queue = this.#queued.get(name) ?? []
      if (queue.length === 0 && this.#available(name, options)) {
        this.#start(name, request)
      } else if (options.ifAvailable) {
        callback(null).then(resolve, reject)
      } else {
        queue.push(request)
        this.#queued.set(name, queue)
      }
    })
  }

  #available(name: string, options: BrowserMutationLockOptions): boolean {
    const held = this.#held.get(name) ?? []
    return held.length === 0 || (
      options.mode === 'shared' && held.every(lock => lock.mode === 'shared')
    )
  }

  #start(name: string, request: LockRequest): void {
    const held = this.#held.get(name) ?? []
    held.push(request.options)
    this.#held.set(name, held)
    const release = () => {
      held.splice(held.indexOf(request.options), 1)
      const queue = this.#queued.get(name) ?? []
      while (queue[0] !== undefined && this.#available(name, queue[0].options)) {
        const next = queue.shift()!
        this.#start(name, next)
      }
    }
    request.callback({ name }).then(
      () => { release(); request.resolve() },
      error => { release(); request.reject(error) },
    )
  }
}

const identitiesByManager = new WeakMap<BrowserLockManagerRuntime, FSAHandleIdentityResolver>()

export function memoryFSAIdentities(manager: BrowserLockManagerRuntime): FSAHandleIdentityResolver {
  let identities = identitiesByManager.get(manager)
  if (identities === undefined) {
    identities = new FSAHandleIdentityRegistry({
      store: new MemoryFSAHandleIdentityStore(),
      manager: new MemoryMutationLockManager(),
    })
    identitiesByManager.set(manager, identities)
  }
  return identities
}

export function acquireFSARootMutationLease(
  parent: FileSystemDirectoryHandle,
  manager: BrowserLockManagerRuntime,
  maximumActiveWriters?: number,
  performance?: PerformanceSummaryObservations,
) {
  return acquireRootLease(parent, manager, maximumActiveWriters, performance, memoryFSAIdentities(manager))
}
