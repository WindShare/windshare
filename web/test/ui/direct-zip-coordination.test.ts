import { describe, expect, it, vi } from 'vitest'

import {
  acquireFSAEntryMutationLease,
  acquireFSAParentNamespaceLease,
  FSAEntryMutationBusyError,
  FSARootMutationBusyError,
  type FSAHandleIdentityResolver,
} from '../../src/output/browser/namespace-mutation'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { BrowserDirectZipCoordination } from '../../src/ui/browser-receive/direct-zip/coordination'
import {
  acquireFSARootMutationLease,
  memoryFSAIdentities,
  MemoryMutationLockManager,
} from '../output/fsa-mutation-lock-fixture'

const OPERATION_ID = 'AQAAAAAAAAAAAAAAAAAAAA'

describe('Direct ZIP coordination lifetime', () => {
  it('drains queued namespace work before releasing target and parent authority', async () => {
    const fixture = coordinationFixture()
    const events: OutputTraceEvent[] = []
    const coordination = await BrowserDirectZipCoordination.open({
      ...fixture, operationId: OPERATION_ID, trace: { current: event => events.push(event) },
    })
    await coordination.claimFile(fixture.file)
    const blocker = await acquireFSAParentNamespaceLease(fixture.parent, fixture.manager, fixture.identities)
    const entered = deferred()
    const finish = deferred()
    const mutation = coordination.mutations.run(async () => {
      entered.resolve()
      await finish.promise
      return 'completed'
    })
    const closing = coordination.close()
    expect(coordination.close()).toBe(closing)
    try {
      await expect(acquireFSARootMutationLease(fixture.parent, fixture.manager))
        .rejects.toBeInstanceOf(FSARootMutationBusyError)
      await expect(acquireFSAEntryMutationLease(fixture.file, fixture.manager, fixture.identities))
        .rejects.toBeInstanceOf(FSAEntryMutationBusyError)
      const lateMutation = vi.fn(async () => undefined)
      await expect(coordination.mutations.run(lateMutation)).rejects.toMatchObject({ name: 'InvalidStateError' })
      await expect(coordination.parentLocks.acquire(fixture.parent)).rejects.toMatchObject({ name: 'InvalidStateError' })
      await expect(coordination.claimFile(fixture.file)).rejects.toMatchObject({ name: 'InvalidStateError' })
      expect(lateMutation).not.toHaveBeenCalled()

      await blocker.release()
      await entered.promise
      await expect(acquireFSAEntryMutationLease(fixture.file, fixture.manager, fixture.identities))
        .rejects.toBeInstanceOf(FSAEntryMutationBusyError)
      finish.resolve()
      await expect(mutation).resolves.toBe('completed')
      await closing
      const releasedScopes = events.flatMap(event =>
        event.eventName === 'direct_zip_coordination' && event.payload.transition === 'released'
          ? [event.payload.scope] : [])
      expect(releasedScopes).toEqual(['namespace', 'target', 'parent_access'])
      await assertAuthoritiesReleased(fixture)
    } finally {
      finish.resolve()
      await blocker.release()
      await mutation.catch(() => undefined)
      await closing
    }
  })

  it('drains a pending target identity lookup and refuses a concurrent target claim', async () => {
    const fixture = coordinationFixture()
    const entered = deferred()
    const finish = deferred()
    const identities: FSAHandleIdentityResolver = {
      resolve: async handle => {
        if (handle.kind === 'file') {
          entered.resolve()
          await finish.promise
        }
        return fixture.identities.resolve(handle)
      },
    }
    const coordination = await BrowserDirectZipCoordination.open({
      ...fixture, identities, operationId: OPERATION_ID,
    })
    const claim = coordination.claimFile(fixture.file)
    await entered.promise
    await expect(coordination.claimFile(fixture.file)).rejects.toMatchObject({ name: 'InvalidStateError' })
    const closing = coordination.close()
    try {
      await expect(acquireFSARootMutationLease(fixture.parent, fixture.manager))
        .rejects.toBeInstanceOf(FSARootMutationBusyError)
      await expect(coordination.parentLocks.acquire(fixture.parent)).rejects.toMatchObject({ name: 'InvalidStateError' })
      await expect(coordination.claimFile(fixture.file)).rejects.toMatchObject({ name: 'InvalidStateError' })
      finish.resolve()
      await claim
      await closing
      await assertAuthoritiesReleased(fixture)
    } finally {
      finish.resolve()
      await claim.catch(() => undefined)
      await closing
    }
  })

  it('releases parent authority when a target lookup fails during close', async () => {
    const fixture = coordinationFixture()
    const entered = deferred()
    const finish = deferred()
    const failure = new DOMException('The target identity lookup failed', 'DataError')
    const identities: FSAHandleIdentityResolver = {
      resolve: async handle => {
        if (handle.kind === 'file') {
          entered.resolve()
          await finish.promise
          throw failure
        }
        return fixture.identities.resolve(handle)
      },
    }
    const coordination = await BrowserDirectZipCoordination.open({
      ...fixture, identities, operationId: OPERATION_ID,
    })
    const claim = coordination.claimFile(fixture.file)
    const rejectedClaim = expect(claim).rejects.toBe(failure)
    await entered.promise
    const closing = coordination.close()
    finish.resolve()
    await rejectedClaim
    await closing
    await assertAuthoritiesReleased(fixture)
  })

  it('releases target and parent authority when queued namespace acquisition fails', async () => {
    const fixture = coordinationFixture()
    const entered = deferred()
    const finish = deferred()
    const failure = new DOMException('The parent identity lookup failed', 'DataError')
    let rejectNamespace = false
    const identities: FSAHandleIdentityResolver = {
      resolve: async handle => {
        if (handle.kind === 'directory' && rejectNamespace) {
          entered.resolve()
          await finish.promise
          throw failure
        }
        return fixture.identities.resolve(handle)
      },
    }
    const coordination = await BrowserDirectZipCoordination.open({
      ...fixture, identities, operationId: OPERATION_ID,
    })
    await coordination.claimFile(fixture.file)
    rejectNamespace = true
    const mutate = vi.fn(async () => undefined)
    const mutation = coordination.mutations.run(mutate)
    const rejectedMutation = expect(mutation).rejects.toBe(failure)
    await entered.promise
    const closing = coordination.close()
    finish.resolve()
    await rejectedMutation
    await closing
    expect(mutate).not.toHaveBeenCalled()
    await assertAuthoritiesReleased(fixture)
  })

  it('drains failed namespace effects and releases their lease before lifetime leases', async () => {
    const fixture = coordinationFixture()
    const coordination = await BrowserDirectZipCoordination.open({ ...fixture, operationId: OPERATION_ID })
    await coordination.claimFile(fixture.file)
    const entered = deferred()
    const finish = deferred()
    const failure = new DOMException('The namespace effect failed', 'InvalidModificationError')
    const mutation = coordination.mutations.run(async () => {
      entered.resolve()
      await finish.promise
      throw failure
    })
    const rejectedMutation = expect(mutation).rejects.toBe(failure)
    await entered.promise
    const closing = coordination.close()
    finish.resolve()
    await rejectedMutation
    await closing
    await assertAuthoritiesReleased(fixture)
  })
})

function coordinationFixture() {
  const manager = new MemoryMutationLockManager()
  return {
    parent: comparableHandle('directory', 'Downloads') as FileSystemDirectoryHandle,
    file: comparableHandle('file', 'archive.zip') as FileSystemFileHandle,
    manager,
    identities: memoryFSAIdentities(manager),
  }
}

function comparableHandle(kind: FileSystemHandleKind, name: string): FileSystemHandle {
  const handle: FileSystemHandle = { kind, name, isSameEntry: async other => other === handle }
  return handle
}

async function assertAuthoritiesReleased(fixture: ReturnType<typeof coordinationFixture>) {
  const root = await acquireFSARootMutationLease(fixture.parent, fixture.manager)
  await root.release()
  const target = await acquireFSAEntryMutationLease(fixture.file, fixture.manager, fixture.identities)
  await target.release()
  const namespace = await acquireFSAParentNamespaceLease(fixture.parent, fixture.manager, fixture.identities)
  await namespace.release()
}

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(complete => { resolve = complete })
  return { promise, resolve }
}
