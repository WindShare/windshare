import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { FILE_SYSTEM_ACCESS_BACKEND } from '../../src/output/capability/contract'
import { BrowserFileSystemTree } from '../../src/output/browser/filesystem-tree'
import {
  browserFileSystemRootMutationLockName,
  BrowserFileSystemMutationClosedError,
  type BrowserLockHandle,
  type BrowserLockManagerRuntime,
} from '../../src/output/browser/namespace-mutation'
import {
  acquireBrowserFileSystemAccessSessionLease,
  acquireBrowserOutputSessionLease,
  BrowserOutputSessionBusyError,
} from '../../src/output/browser/session-lease'
import type { DurableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'

const ROOT_IDENTITY = opaqueIdentity(0x31)
const OTHER_ROOT_IDENTITY = opaqueIdentity(0x32)
const FIRST_INTENT = opaqueIdentity(0x41)
const SECOND_INTENT = opaqueIdentity(0x42)
const FIRST_BINDING = binding(FIRST_INTENT, ROOT_IDENTITY)
const SECOND_BINDING = binding(SECOND_INTENT, ROOT_IDENTITY)

describe('browser File System Access namespace mutation authority', () => {
  it('settles a barrier race across intents with one root owner and typed busy', async () => {
    const manager = new DeterministicLockManager()
    const gate = manager.gate(
      browserFileSystemRootMutationLockName(FIRST_BINDING),
      2,
    )
    const first = acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    const second = acquireBrowserFileSystemAccessSessionLease(SECOND_BINDING, manager)

    await gate.reached
    gate.open()
    const [firstResult, secondResult] = await Promise.allSettled([first, second])

    expect(firstResult.status).toBe('fulfilled')
    expect(secondResult.status).toBe('rejected')
    if (firstResult.status !== 'fulfilled' || secondResult.status !== 'rejected') return
    expect(secondResult.reason).toBeInstanceOf(BrowserOutputSessionBusyError)
    expect(secondResult.reason).toMatchObject({
      name: 'InvalidStateError',
      scope: 'file-system-root',
      binding: SECOND_BINDING,
    })

    // A failed root acquisition must not strand the loser's intent lock.
    const secondIntentOnly = await acquireBrowserOutputSessionLease(SECOND_BINDING, manager)
    await secondIntentOnly.release()
    await firstResult.value.release()

    const recovered = await acquireBrowserFileSystemAccessSessionLease(SECOND_BINDING, manager)
    await recovered.release()
  })

  it('keeps same-intent opener exclusivity distinct from the shared-root lock', async () => {
    const manager = new DeterministicLockManager()
    const first = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)

    await expect(acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager))
      .rejects.toMatchObject({
        name: 'InvalidStateError',
        scope: 'intent-namespace',
        binding: FIRST_BINDING,
      })

    await first.release()
  })

  it('drains accepted mutations before release and refuses late mutation', async () => {
    const manager = new DeterministicLockManager()
    const lease = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    const firstStarted = deferred<void>()
    const finishFirst = deferred<void>()
    const order: string[] = []
    const first = lease.mutations.mutate('create-file', ['first.bin'], async () => {
      order.push('first-started')
      firstStarted.resolve()
      await finishFirst.promise
      order.push('first-finished')
    })
    await firstStarted.promise
    const second = lease.mutations.mutate('remove-file', ['second.bin'], async () => {
      order.push('second')
    })
    const release = lease.release()

    await expect(lease.mutations.mutate('ensure-directory', ['late'], async () => undefined))
      .rejects.toBeInstanceOf(BrowserFileSystemMutationClosedError)
    expect(order).toEqual(['first-started'])

    finishFirst.resolve()
    await Promise.all([first, second, release])
    expect(order).toEqual(['first-started', 'first-finished', 'second'])

    const reopened = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    await reopened.release()
  })

  it('binds tree authority to intent, backend, and root', async () => {
    const manager = new DeterministicLockManager()
    const lease = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    const root = new MemoryDirectoryHandle('root')

    expect(() => BrowserFileSystemTree.forSharedRoot({
      root: root.handle,
      handles: new MemoryHandleRepository(SECOND_BINDING),
      mutations: lease.mutations,
      randomIdentity: () => opaqueIdentity(0x51),
    })).toThrow('does not match the output namespace')

    await lease.release()
  })

  it('preserves no-replace and denies cross-intent removal after lease recovery', async () => {
    const manager = new DeterministicLockManager()
    const root = new MemoryDirectoryHandle('root')
    const firstRepository = new MemoryHandleRepository(FIRST_BINDING)
    const firstLease = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    const firstTree = BrowserFileSystemTree.forSharedRoot({
      root: root.handle,
      handles: firstRepository,
      mutations: firstLease.mutations,
      randomIdentity: identitySequence(0x61),
    })
    const firstFile = await firstTree.createFileExclusive(['shared.bin'])
    const firstDirectory = await firstTree.ensureDirectory(['shared-directory'])
    expect(firstDirectory.created).toBe(true)
    const originalFileHandle = root.entry('shared.bin')
    const originalDirectoryHandle = root.entry('shared-directory')
    await firstLease.release()

    const secondRepository = new MemoryHandleRepository(SECOND_BINDING)
    const secondLease = await acquireBrowserFileSystemAccessSessionLease(SECOND_BINDING, manager)
    const secondTree = BrowserFileSystemTree.forSharedRoot({
      root: root.handle,
      handles: secondRepository,
      mutations: secondLease.mutations,
      randomIdentity: identitySequence(0x71),
    })

    await expect(secondTree.createFileExclusive(['shared.bin']))
      .rejects.toMatchObject({ name: 'InvalidModificationError' })
    const secondDirectory = await secondTree.ensureDirectory(['shared-directory'])
    expect(secondDirectory.created).toBe(false)
    await expect(secondTree.removeFile(['shared.bin'], firstFile.identity))
      .rejects.toMatchObject({ name: 'InvalidStateError' })
    await expect(secondTree.removeDirectory(['shared-directory'], firstDirectory.identity))
      .rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(root.entry('shared.bin')).toBe(originalFileHandle)
    expect(root.entry('shared-directory')).toBe(originalDirectoryHandle)

    await secondLease.release()
  })

  it('does not collide independent physical roots', async () => {
    const manager = new DeterministicLockManager()
    expect(browserFileSystemRootMutationLockName(FIRST_BINDING)).not.toBe(
      browserFileSystemRootMutationLockName(binding(SECOND_INTENT, OTHER_ROOT_IDENTITY)),
    )
    const first = await acquireBrowserFileSystemAccessSessionLease(FIRST_BINDING, manager)
    const second = await acquireBrowserFileSystemAccessSessionLease(
      binding(SECOND_INTENT, OTHER_ROOT_IDENTITY),
      manager,
    )
    await Promise.all([first.release(), second.release()])
  })
})

interface LockGate {
  readonly reached: Promise<void>
  open(): void
}

interface PendingLockRequest {
  readonly name: string
  readonly options: { readonly mode: 'exclusive'; readonly ifAvailable?: true }
  readonly callback: (lock: BrowserLockHandle | null) => Promise<void>
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
}

class DeterministicLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Set<string>()
  readonly #waiting = new Map<string, PendingLockRequest[]>()
  #gate: {
    readonly name: string
    readonly count: number
    readonly reached: ReturnType<typeof deferred<void>>
    readonly pending: PendingLockRequest[]
  } | undefined

  gate(name: string, count: number): LockGate {
    if (this.#gate !== undefined) throw new Error('A lock gate is already active')
    const reached = deferred<void>()
    this.#gate = { name, count, reached, pending: [] }
    return Object.freeze({
      reached: reached.promise,
      open: () => {
        const gate = this.#gate
        if (gate === undefined || gate.name !== name) throw new Error('Lock gate is not active')
        this.#gate = undefined
        for (const request of gate.pending) this.#start(request)
      },
    })
  }

  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable?: true },
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const request = { name, options, callback, resolve, reject }
      const gate = this.#gate
      if (gate !== undefined && gate.name === name) {
        gate.pending.push(request)
        if (gate.pending.length === gate.count) gate.reached.resolve()
        return
      }
      this.#start(request)
    })
  }

  #start(request: PendingLockRequest): void {
    if (this.#held.has(request.name)) {
      if (request.options.ifAvailable === true) {
        request.callback(null).then(request.resolve, request.reject)
        return
      }
      const waiting = this.#waiting.get(request.name) ?? []
      waiting.push(request)
      this.#waiting.set(request.name, waiting)
      return
    }
    this.#held.add(request.name)
    request.callback(Object.freeze({ name: request.name })).then(
      () => {
        this.#held.delete(request.name)
        request.resolve()
        this.#startNext(request.name)
      },
      (error: unknown) => {
        this.#held.delete(request.name)
        request.reject(error)
        this.#startNext(request.name)
      },
    )
  }

  #startNext(name: string): void {
    const waiting = this.#waiting.get(name)
    const next = waiting?.shift()
    if (waiting?.length === 0) this.#waiting.delete(name)
    if (next !== undefined) this.#start(next)
  }
}

class MemoryHandleRepository {
  readonly binding: DurableCheckpointNamespaceIdentity
  readonly #handles = new Map<string, FileSystemHandle>()

  constructor(bindingValue: DurableCheckpointNamespaceIdentity) {
    this.binding = bindingValue
  }

  async putHandle(identity: string, handle: FileSystemHandle): Promise<void> {
    this.#handles.set(identity, handle)
  }

  async getHandle(identity: string): Promise<FileSystemHandle | undefined> {
    return this.#handles.get(identity)
  }

  async deleteHandle(identity: string): Promise<void> {
    this.#handles.delete(identity)
  }
}

type MemoryEntry = MemoryDirectoryHandle | MemoryFileHandle

class MemoryDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #entries = new Map<string, MemoryEntry>()

  constructor(name: string) {
    this.name = name
  }

  get handle(): FileSystemDirectoryHandle {
    return this as unknown as FileSystemDirectoryHandle
  }

  entry(name: string): MemoryEntry | undefined {
    return this.#entries.get(name)
  }

  async getFileHandle(
    name: string,
    options?: { readonly create?: boolean },
  ): Promise<FileSystemFileHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryDirectoryHandle) throw domError('TypeMismatchError')
    if (existing !== undefined) return existing.handle
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryFileHandle(name)
    this.#entries.set(name, created)
    return created.handle
  }

  async getDirectoryHandle(
    name: string,
    options?: { readonly create?: boolean },
  ): Promise<FileSystemDirectoryHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryFileHandle) throw domError('TypeMismatchError')
    if (existing !== undefined) return existing.handle
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryDirectoryHandle(name)
    this.#entries.set(name, created)
    return created.handle
  }

  async removeEntry(name: string): Promise<void> {
    const existing = this.#entries.get(name)
    if (existing === undefined) throw domError('NotFoundError')
    if (existing instanceof MemoryDirectoryHandle && existing.#entries.size > 0) {
      throw domError('InvalidModificationError')
    }
    this.#entries.delete(name)
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this.handle
  }
}

class MemoryFileHandle {
  readonly kind = 'file' as const
  readonly name: string

  constructor(name: string) {
    this.name = name
  }

  get handle(): FileSystemFileHandle {
    return this as unknown as FileSystemFileHandle
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this.handle
  }
}

function binding(
  transferIntentDigest: string,
  rootIdentity: string,
): DurableCheckpointNamespaceIdentity {
  return Object.freeze({
    backend: FILE_SYSTEM_ACCESS_BACKEND,
    transferIntentDigest,
    rootIdentity,
  })
}

function opaqueIdentity(byte: number): string {
  return encodeBase64Url(new Uint8Array(32).fill(byte))
}

function identitySequence(first: number): () => string {
  let next = first
  return () => opaqueIdentity(next++)
}

function domError(name: string): DOMException {
  return new DOMException(name, name)
}

function deferred<T>(): {
  readonly promise: Promise<T>
  resolve(value: T): void
  reject(reason: unknown): void
} {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((onResolve, onReject) => {
    resolve = onResolve
    reject = onReject
  })
  return { promise, resolve, reject }
}
