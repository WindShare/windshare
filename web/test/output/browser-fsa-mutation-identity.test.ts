import { describe, expect, it, vi } from 'vitest'

import {
  acquireFSAEntryMutationLease,
  acquireFSAParentAccessLease,
  acquireFSAParentNamespaceLease,
  acquireFSARootMutationLease,
  FSAEntryMutationBusyError,
  FSAHandleIdentityRegistry,
  FSARootMutationBusyError,
} from '../../src/output/browser/namespace-mutation'
import type { FSAHandleIdentityStore } from '../../src/output/browser/mutation-coordination/handle-identity'
import {
  MemoryFSAHandleIdentityStore,
  MemoryMutationLockManager,
} from './fsa-mutation-lock-fixture'

describe('FSA physical mutation identity', () => {
  it('assigns one durable identity to concurrent aliases across registry instances', async () => {
    const store = new MemoryFSAHandleIdentityStore()
    const manager = new MemoryMutationLockManager()
    const first = new FSAHandleIdentityRegistry({ store, manager })
    const reloaded = new FSAHandleIdentityRegistry({ store, manager })

    const identities = await Promise.all([
      first.resolve(directory('downloads', 'directory-a')),
      reloaded.resolve(directory('renamed-downloads', 'directory-a')),
      reloaded.resolve(directory('downloads', 'directory-b')),
    ])

    expect(identities[0]).toBe(identities[1])
    expect(identities[0]).not.toBe(identities[2])
    expect(store.records).toHaveLength(2)
  })

  it('allows unrelated same-named directories to hold exclusive legacy leases', async () => {
    const { manager, identities } = fixture()
    const first = await acquireFSARootMutationLease(
      directory('downloads', 'directory-a'), manager, undefined, undefined, identities,
    )
    const second = await acquireFSARootMutationLease(
      directory('downloads', 'directory-b'), manager, undefined, undefined, identities,
    )
    await expect(Promise.all([first.release(), second.release()])).resolves.toEqual([undefined, undefined])
  })

  it('shares parent access and excludes legacy root writers in both directions', async () => {
    const { manager, identities } = fixture()
    const parent = directory('downloads', 'directory-a')
    const first = await acquireFSAParentAccessLease(parent, manager, identities)
    const second = await acquireFSAParentAccessLease(
      directory('alias', 'directory-a'), manager, identities,
    )
    expect(first.name).toBe(second.name)

    await expect(acquireFSARootMutationLease(
      parent, manager, undefined, undefined, identities,
    )).rejects.toBeInstanceOf(FSARootMutationBusyError)
    await first.release()
    await expect(acquireFSARootMutationLease(
      parent, manager, undefined, undefined, identities,
    )).rejects.toBeInstanceOf(FSARootMutationBusyError)
    await second.release()

    const root = await acquireFSARootMutationLease(
      parent, manager, undefined, undefined, identities,
    )
    await expect(acquireFSAParentAccessLease(parent, manager, identities))
      .rejects.toBeInstanceOf(FSARootMutationBusyError)
    await root.release()
    const reopened = await acquireFSAParentAccessLease(parent, manager, identities)
    await reopened.release()
  })

  it('queues a short namespace mutation while unrelated parents and files proceed', async () => {
    const { manager, identities } = fixture()
    const parent = directory('downloads', 'directory-a')
    const access = await acquireFSAParentAccessLease(parent, manager, identities)
    const first = await acquireFSAParentNamespaceLease(parent, manager, identities)
    let entered = false
    const waiting = acquireFSAParentNamespaceLease(
      directory('alias', 'directory-a'), manager, identities,
    ).then(lease => { entered = true; return lease })
    const otherParent = await acquireFSAParentNamespaceLease(
      directory('downloads', 'directory-b'), manager, identities,
    )
    const fileLease = await acquireFSAEntryMutationLease(file('archive.zip', 'file-a'), manager, identities)
    expect(entered).toBe(false)
    await first.release()
    const second = await waiting
    expect(entered).toBe(true)
    await Promise.all([second.release(), otherParent.release(), fileLease.release(), access.release()])
  })

  it('conflicts on one physical file across handle aliases and survives registry reopening', async () => {
    const { store, manager, identities } = fixture()
    const fileLease = await acquireFSAEntryMutationLease(file('archive.zip', 'file-a'), manager, identities)
    const reloaded = new FSAHandleIdentityRegistry({ store, manager })
    await expect(acquireFSAEntryMutationLease(file('alias.zip', 'file-a'), manager, reloaded))
      .rejects.toBeInstanceOf(FSAEntryMutationBusyError)
    const independent = await acquireFSAEntryMutationLease(file('archive.zip', 'file-b'), manager, reloaded)
    await independent.release()
    const firstRelease = fileLease.release()
    expect(fileLease.release()).toBe(firstRelease)
    await firstRelease
    const next = await acquireFSAEntryMutationLease(file('alias.zip', 'file-a'), manager, reloaded)
    expect(next.name).toBe(fileLease.name)
    await next.release()
  })

  it.each(['read', 'compare', 'insert'] as const)(
    'fails closed on %s failure and releases registry coordination for the next attempt',
    async stage => {
      const failure = new DOMException('identity unavailable', 'NotAllowedError')
      const manager = new MemoryMutationLockManager()
      const store = new MemoryFSAHandleIdentityStore()
      const parent = directory('downloads', 'directory-a')
      let failing = true
      const failingStore: FSAHandleIdentityStore = {
        readAll: async () => {
          if (failing && stage === 'read') throw failure
          return store.readAll()
        },
        insert: async handle => {
          if (failing && stage === 'insert') throw failure
          return store.insert(handle)
        },
      }
      if (stage === 'compare') {
        await store.insert(directory('downloads', 'directory-b'))
        const compare = parent.isSameEntry.bind(parent)
        vi.spyOn(parent, 'isSameEntry').mockImplementation(async other => {
          if (failing) throw failure
          return compare(other)
        })
      }
      const identities = new FSAHandleIdentityRegistry({ store: failingStore, manager })
      await expect(acquireFSAParentAccessLease(parent, manager, identities)).rejects.toBe(failure)
      expect(store.records).toHaveLength(stage === 'compare' ? 1 : 0)
      failing = false
      const next = await acquireFSAParentAccessLease(parent, manager, identities)
      await next.release()
    },
  )

  it('rejects handles without physical comparison authority before writing the registry', async () => {
    const { store, identities } = fixture()
    const parent = { kind: 'directory', name: 'downloads' } as FileSystemDirectoryHandle
    await expect(identities.resolve(parent)).rejects.toBeInstanceOf(TypeError)
    expect(store.records).toHaveLength(0)
  })
})

function fixture() {
  const store = new MemoryFSAHandleIdentityStore()
  const manager = new MemoryMutationLockManager()
  return { store, manager, identities: new FSAHandleIdentityRegistry({ store, manager }) }
}

function handle(kind: FileSystemHandleKind, name: string, physicalId: string): FileSystemHandle {
  return {
    kind,
    name,
    physicalId,
    isSameEntry: async (other: FileSystemHandle) =>
      physicalId === (other as FileSystemHandle & { physicalId: string }).physicalId,
  } as FileSystemHandle
}

function directory(name: string, physicalId: string): FileSystemDirectoryHandle {
  return handle('directory', name, physicalId) as FileSystemDirectoryHandle
}

function file(name: string, physicalId: string): FileSystemFileHandle {
  return handle('file', name, physicalId) as FileSystemFileHandle
}
