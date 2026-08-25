import { describe, expect, it } from 'vitest'

import {
  FSARootMutationBusyError,
  acquireFSARootMutationLease,
} from '../../src/output/browser/namespace-mutation'
import type { BrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import {
  FSAResourceOwner,
  type FSAOwnedOutputSession,
} from '../../src/ui/browser-receive/fsa-resource-owner'
import {
  MemoryDirectory,
  MemoryLockManager,
} from '../output/file-system-access-lifecycle-fixture'

describe('FSA background resource ownership', () => {
  it('retains the Web Lock through held output close and releases every authority once in order', async () => {
    const parent = new MemoryDirectory('downloads')
    const parentHandle = parent as unknown as FileSystemDirectoryHandle
    const locks = new MemoryLockManager()
    const rootLease = await acquireFSARootMutationLease(parentHandle, locks)
    const closeGate = deferred<void>()
    const operationReleaseStarted = deferred<void>()
    const releaseOperation = deferred<void>()
    const order: string[] = []
    const outputSession: FSAOwnedOutputSession = {
      repairProjection: undefined,
      subscribeRepairProjectionActivation: () => () => undefined,
      closeForTerminalSettlement: async () => {
        order.push('output-close-started')
        await closeGate.promise
        order.push('output-close-finished')
      },
      releaseRootLease: async () => {
        order.push('root-release')
        await rootLease.release()
      },
    }
    const operationLease = operationLeaseFixture(async () => {
      order.push('operation-release-started')
      operationReleaseStarted.resolve()
      await releaseOperation.promise
      order.push('operation-release-finished')
    })
    const repository = {
      close: () => { order.push('repository-close') },
    }
    const owner = new FSAResourceOwner({ outputSession, operationLease, repository })

    const firstClose = owner.close()
    const secondClose = owner.close()
    expect(secondClose).toBe(firstClose)
    await Promise.resolve()
    expect(order).toEqual(['output-close-started'])
    await expect(acquireFSARootMutationLease(parentHandle, locks))
      .rejects.toBeInstanceOf(FSARootMutationBusyError)

    closeGate.resolve()
    await operationReleaseStarted.promise
    expect(order).toEqual([
      'output-close-started',
      'output-close-finished',
      'operation-release-started',
    ])
    await expect(acquireFSARootMutationLease(parentHandle, locks))
      .rejects.toBeInstanceOf(FSARootMutationBusyError)

    releaseOperation.resolve()
    await firstClose
    expect(order).toEqual([
      'output-close-started',
      'output-close-finished',
      'operation-release-started',
      'operation-release-finished',
      'repository-close',
      'root-release',
    ])
    expect(locks.releaseCount).toBe(1)

    const contender = await acquireFSARootMutationLease(parentHandle, locks)
    await contender.release()
    expect(locks.releaseCount).toBe(2)
    await owner.close()
    expect(order).toHaveLength(6)
  })

  it('orders reopened operation authority closure before output-owned Web Lock release', async () => {
    const order: string[] = []
    const owner = new FSAResourceOwner({
      outputSession: outputSessionFixture(order),
      closeOperationAuthority: async () => { order.push('operation-authority-close') },
    })

    await owner.close()

    expect(order).toEqual([
      'output-close',
      'operation-authority-close',
      'root-release',
    ])
  })
})

function operationLeaseFixture(release: () => void | Promise<void>): BrowserReceiveOperationLease {
  return Object.freeze({
    operationId: 'operation',
    leaseId: 'lease',
    acquiredAt: 0,
    heartbeat: () => Promise.reject(new Error('heartbeat is outside this test')),
    release: async () => { await release() },
  })
}

function outputSessionFixture(order: string[]): FSAOwnedOutputSession {
  return Object.freeze({
    repairProjection: undefined,
    subscribeRepairProjectionActivation: () => () => undefined,
    closeForTerminalSettlement: async () => { order.push('output-close') },
    releaseRootLease: async () => { order.push('root-release') },
  })
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
}
