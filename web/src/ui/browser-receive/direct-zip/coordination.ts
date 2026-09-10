import {
  acquireFSAEntryMutationLease,
  acquireFSAParentAccessLease,
  acquireFSAParentNamespaceLease,
  type BrowserLockManagerRuntime,
  type FSAMutationLease,
  type FSAHandleIdentityResolver,
} from '../../../output/browser/namespace-mutation'
import { emitOutputTrace, outputTraceEvent, type OutputTraceSource } from '../../../output/diagnostics'
import type { DirectZipParentLockPort } from '../../../output/direct-zip/target'
import type { DirectZipNamespaceMutationPort } from './target'

type LockScope = 'parent_access' | 'namespace' | 'target'

interface CoordinationOptions {
  readonly parent: FileSystemDirectoryHandle
  readonly manager: BrowserLockManagerRuntime
  readonly operationId: string
  readonly identities?: FSAHandleIdentityResolver
  readonly trace?: OutputTraceSource
}

/**
 * The shared parent lease coexists with other ZIPs while preserving the existing
 * tree writer's exclusive authority. Only the chosen file stays exclusive.
 */
export class BrowserDirectZipCoordination {
  readonly #parent: FileSystemDirectoryHandle
  readonly #manager: BrowserLockManagerRuntime
  readonly #operationId: string
  readonly #trace: OutputTraceSource | undefined
  readonly #identities: FSAHandleIdentityResolver | undefined
  readonly #pending = new Set<Promise<void>>()
  #accepting = true
  #claiming = false
  #parentAccess!: FSAMutationLease
  #target: FSAMutationLease | undefined
  #closePromise: Promise<void> | undefined

  private constructor(input: CoordinationOptions) {
    this.#parent = input.parent
    this.#manager = input.manager
    this.#operationId = input.operationId
    this.#trace = input.trace
    this.#identities = input.identities
  }

  static async open(input: CoordinationOptions): Promise<BrowserDirectZipCoordination> {
    const coordination = new BrowserDirectZipCoordination(input)
    coordination.#parentAccess = await coordination.#acquire('parent_access', () =>
      acquireFSAParentAccessLease(input.parent, input.manager, input.identities))
    return coordination
  }

  readonly parentLocks: DirectZipParentLockPort<FileSystemDirectoryHandle> = {
    acquire: async parent => {
      const finish = this.#begin()
      try {
        if (!await this.#parent.isSameEntry(parent)) {
          throw new DOMException('ZIP namespace authority belongs to another folder', 'DataError')
        }
        const lease = await this.#acquire('namespace', () =>
          acquireFSAParentNamespaceLease(parent, this.#manager, this.#identities))
        let release: Promise<void> | undefined
        return {
          name: lease.name,
          release: () => {
            release ??= lease.release().finally(finish)
            return release
          },
        }
      } catch (error) {
        finish()
        throw error
      }
    },
  }

  readonly mutations: DirectZipNamespaceMutationPort = {
    run: async operation => {
      const lease = await this.parentLocks.acquire(this.#parent)
      try { return await operation() } finally { await lease.release() }
    },
  }

  async claimFile(file: FileSystemFileHandle): Promise<void> {
    const finish = this.#begin()
    try {
      if (this.#target !== undefined || this.#claiming) {
        throw new DOMException('ZIP target authority is already bound', 'InvalidStateError')
      }
      this.#claiming = true
      try {
        // Bootstrap holds namespace authority; deletion holds target authority before
        // taking namespace authority. A try-only target lease prevents a lock-order cycle.
        this.#target = await this.#acquire('target', () =>
          acquireFSAEntryMutationLease(file, this.#manager, this.#identities))
      } finally { this.#claiming = false }
    } finally { finish() }
  }

  close(): Promise<void> {
    this.#accepting = false
    this.#closePromise ??= (async () => {
      // A queued namespace lease is already admitted work. Keep target and parent
      // protection until that lease and any pending target acquisition have drained.
      await Promise.all(this.#pending)
      try { await this.#target?.release() } finally { await this.#parentAccess.release() }
    })()
    return this.#closePromise
  }

  #begin(): () => void {
    if (!this.#accepting) {
      throw new DOMException('ZIP coordination authority is closed', 'InvalidStateError')
    }
    let resolve!: () => void
    const pending = new Promise<void>(complete => { resolve = complete })
    this.#pending.add(pending)
    return () => { this.#pending.delete(pending); resolve() }
  }

  async #acquire(scope: LockScope, acquire: () => Promise<FSAMutationLease>): Promise<FSAMutationLease> {
    this.#observe(scope, 'waiting')
    let lease: FSAMutationLease
    try {
      lease = await acquire()
    } catch (error) {
      this.#observe(scope, 'failed', undefined, error)
      throw error
    }
    this.#observe(scope, 'acquired', lease.name)
    let release: Promise<void> | undefined
    return {
      name: lease.name,
      release: () => {
        release ??= (async () => {
          try {
            await lease.release()
            this.#observe(scope, 'released', lease.name)
          } catch (error) {
            this.#observe(scope, 'failed', lease.name, error)
            throw error
          }
        })()
        return release
      },
    }
  }

  #observe(scope: LockScope, transition: 'waiting' | 'acquired' | 'released' | 'failed',
    lockName?: string, error?: unknown) {
    emitOutputTrace(this.#trace, () => outputTraceEvent('direct_zip_coordination', {
      operation_id: this.#operationId, scope, transition,
      ...(lockName === undefined ? {} : { lock_name: lockName }),
      ...(error instanceof Error ? { native_error_name: error.name } : {}),
    }))
  }
}
