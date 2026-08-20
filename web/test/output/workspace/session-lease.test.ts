import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  BrowserReceiveOperationBusyError,
  acquireBrowserReceiveOperationLease,
  type BrowserLockManagerRuntime,
} from '../../../src/output/browser/session-lease'
import type {
  ManifestPageRecord,
  PersistedReceiveRecord,
  ReceiveOperationHandleRecord,
  ReceiveOperationLeaseRecord,
} from '../../../src/output/workspace/records'
import { RECEIVE_RECORD_OPERATION } from '../../../src/output/workspace/records'
import type {
  ReceiveOperationRepository,
  ReceiveOperationTransition,
} from '../../../src/output/workspace/repository'
import { initialReceiveLifecycleState } from '../../../src/output/workspace/state'

describe('browser receive operation lease', () => {
  it('atomically replaces an abandoned durable lease, heartbeats, and releases', async () => {
    const operationId = identity(16, 1)
    const repository = new MemoryRepository({
      id: `windshare/receive-operation/v1/${operationId}/lease`,
      schemaVersion: 1,
      operationId,
      leaseId: identity(16, 2),
      acquiredAt: 100,
      heartbeatAt: 100,
    })
    let now = 200
    const lease = await acquireBrowserReceiveOperationLease(repository, operationId, {
      manager: lockManager(true),
      clock: { now: () => now },
      randomBytes: (length) => new Uint8Array(length).fill(3),
    })

    expect(repository.transitions[0]).toEqual(expect.objectContaining({
      expectedLeaseId: identity(16, 2),
      lease: expect.objectContaining({ kind: 'put' }),
    }))
    now = 250
    await expect(lease.heartbeat()).resolves.toEqual(expect.objectContaining({
      acquiredAt: 200,
      heartbeatAt: 250,
    }))
    expect(repository.transitions[1]).not.toHaveProperty('lifecycle')
    await lease.release()
    expect(repository.lease).toBeUndefined()
    expect(repository.transitions.at(-1)).toEqual(expect.objectContaining({
      expectedLeaseId: lease.leaseId,
      lease: { kind: 'delete', leaseId: lease.leaseId },
    }))
  })

  it('fails without writing when another browser context holds the Web Lock', async () => {
    const repository = new MemoryRepository()
    await expect(acquireBrowserReceiveOperationLease(
      repository,
      identity(16, 1),
      { manager: lockManager(false) },
    )).rejects.toBeInstanceOf(BrowserReceiveOperationBusyError)
    expect(repository.transitions).toEqual([])
  })

  it('joins initial records, handles, lifecycle, and lease in one acquisition transition', async () => {
    const operationId = identity(16, 4)
    const repository = new MemoryRepository()
    const record = {
      id: `windshare/receive-operation/v1/${operationId}/fixture`,
      schemaVersion: 1 as const,
      operationId,
      kind: RECEIVE_RECORD_OPERATION,
      digest: identity(32, 5),
      canonicalBytes: Uint8Array.of(1),
    }
    const handle = {
      id: `windshare/receive-operation/v1/${operationId}/handle`,
      schemaVersion: 1 as const,
      operationId,
      kind: 1,
      authorityRef: identity(32, 6),
      handle: Object.freeze({ kind: 'directory' }),
    }
    const lifecycle = initialReceiveLifecycleState({
      operationId,
      receiveIntentDigest: identity(32, 7),
    })

    const lease = await acquireBrowserReceiveOperationLease(repository, operationId, {
      manager: lockManager(true),
      clock: { now: () => 300 },
      randomBytes: length => new Uint8Array(length).fill(8),
      acquireTransition: { records: [record], handles: [handle], lifecycle },
    })

    expect(repository.transitions).toHaveLength(1)
    expect(repository.transitions[0]).toMatchObject({
      operationId,
      records: [record],
      handles: [handle],
      lifecycle,
      lease: { kind: 'put', record: expect.objectContaining({ leaseId: lease.leaseId }) },
    })
    await lease.release()
  })
})

class MemoryRepository implements ReceiveOperationRepository {
  lease: ReceiveOperationLeaseRecord | undefined
  readonly transitions: ReceiveOperationTransition[] = []

  constructor(lease?: ReceiveOperationLeaseRecord) {
    this.lease = lease
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (transition.expectedLeaseId !== undefined &&
        this.lease?.leaseId !== transition.expectedLeaseId) {
      throw new DOMException('lease changed', 'InvalidStateError')
    }
    if (transition.lease?.kind === 'put') this.lease = transition.lease.record
    if (transition.lease?.kind === 'delete') this.lease = undefined
    this.transitions.push(transition)
  }

  async readRecord(): Promise<PersistedReceiveRecord | undefined> {
    return undefined
  }

  async readLifecycle(): Promise<PersistedReceiveRecord | undefined> {
    return undefined
  }

  async listRecords(): Promise<readonly PersistedReceiveRecord[]> {
    return []
  }

  async listManifestPages(): Promise<readonly ManifestPageRecord[]> {
    return []
  }

  async readHandle<T>(): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return undefined
  }

  async readLease(): Promise<ReceiveOperationLeaseRecord | undefined> {
    return this.lease
  }

  close(): void {}
}

function lockManager(available: boolean): BrowserLockManagerRuntime {
  return {
    request: async (name, _options, callback) =>
      callback(available ? { name } : null),
  }
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
