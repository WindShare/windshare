import { describe, expect, it } from 'vitest'

import {
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createReceiveIntent,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  fsaOwnedDirectoryHandleId,
  persistFSAOperationBinding,
  persistFSAOwnedDirectory,
  verifyFSAOperationBinding,
} from '../../src/output/browser/indexeddb-root-binding'
import {
  FSARootMutationBusyError,
  acquireFSARootMutationLease,
  fsaRootMutationLockName,
  type BrowserLockManagerRuntime,
} from '../../src/output/browser/namespace-mutation'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import type {
  PersistedReceiveRecord,
  ReceiveOperationHandleRecord,
} from '../../src/output/workspace/records'
import { RECEIVE_RECORD_RESERVATION } from '../../src/output/workspace/records'
import type {
  ReceiveOperationRepository,
  ReceiveOperationTransition,
} from '../../src/output/workspace/repository'
import { identity } from './planning/fixture'

describe('FSA namespace and persisted parent authority', () => {
  it('serializes same-named parents and reports a competing WindShare task as busy', async () => {
    const manager = new MemoryLockManager()
    const firstParent = directoryHandle('shared-parent', 'first')
    const secondParent = directoryHandle('shared-parent', 'second')
    expect(await fsaRootMutationLockName(firstParent)).toBe(
      await fsaRootMutationLockName(secondParent),
    )

    const first = await acquireFSARootMutationLease(firstParent, manager)
    await expect(acquireFSARootMutationLease(secondParent, manager)).rejects.toBeInstanceOf(
      FSARootMutationBusyError,
    )
    await first.release()
    const reopened = await acquireFSARootMutationLease(secondParent, manager)
    await reopened.release()
  })

  it('persists operation, reservation, and parent handle in one verified transition', async () => {
    const repository = new MemoryOperationRepository()
    const parent = directoryHandle('downloads', 'parent-a')
    const intent = await singleFileIntent()

    const persisted = await persistFSAOperationBinding({ repository, intent, parent })
    expect(persisted.reservation.guarantees).toMatchObject({
      profile: 'fsa-tree',
      replacement: 'coordinated-no-replace',
      delivery: 'managed-target',
      visibility: 'prefix-visible',
      rollback: 'none',
    })
    expect(repository.transitions).toHaveLength(1)
    expect(repository.transitions[0]).toMatchObject({ operationId: intent.operationId })
    await expect(verifyFSAOperationBinding({
      repository,
      intent,
      expectedParent: parent,
    })).resolves.toMatchObject({ parent, intent })
  })

  it('never accepts a different parent or rewritten immutable reservation on reopen', async () => {
    const repository = new MemoryOperationRepository()
    const parent = directoryHandle('downloads', 'parent-a')
    const intent = await singleFileIntent()
    const persisted = await persistFSAOperationBinding({ repository, intent, parent })

    await expect(verifyFSAOperationBinding({
      repository,
      intent,
      expectedParent: directoryHandle('downloads', 'parent-b'),
    })).rejects.toBeInstanceOf(TargetOwnershipUnknownError)

    const reservationRecord = [...repository.records.values()].find(
      (record) => record.kind === RECEIVE_RECORD_RESERVATION,
    )
    if (reservationRecord === undefined) throw new Error('reservation fixture is missing')
    repository.records.set(
      reservationRecord.id,
      { bogus: true } as unknown as PersistedReceiveRecord,
    )
    await expect(verifyFSAOperationBinding({
      repository,
      intent: persisted.intent,
    })).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
  })

  it('reopens only the same-operation owned directory handle', async () => {
    const repository = new MemoryOperationRepository()
    const parent = directoryHandle('downloads', 'parent-a')
    const intent = await singleFileIntent()
    const persisted = await persistFSAOperationBinding({ repository, intent, parent })
    const handleId = fsaOwnedDirectoryHandleId(intent.operationId, 'opaque-locator')
    const first = directoryHandle('task-root', 'root-a')

    await persistFSAOwnedDirectory({
      repository,
      reservation: persisted.reservation,
      handleId,
      ownedObjectId: identity(90, 32),
      handle: first,
    })
    await expect(persistFSAOwnedDirectory({
      repository,
      reservation: persisted.reservation,
      handleId,
      ownedObjectId: identity(90, 32),
      handle: directoryHandle('task-root', 'root-b'),
    })).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
  })
})

async function singleFileIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: 'report.bin',
    outputName: 'report.bin',
  })
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(4),
    reservationId: identity(5),
    artifact,
    authorityRef: identity(6, 32),
    reservedName: 'report.bin',
    collisionIndex: 0,
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
}

class MemoryLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Set<string>()

  async request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: { readonly name: string } | null) => Promise<void>,
  ): Promise<void> {
    if (this.#held.has(name)) {
      await callback(null)
      return
    }
    this.#held.add(name)
    try {
      await callback({ name })
    } finally {
      this.#held.delete(name)
    }
  }
}

class MemoryOperationRepository implements ReceiveOperationRepository {
  readonly records = new Map<string, PersistedReceiveRecord>()
  readonly handles = new Map<string, ReceiveOperationHandleRecord>()
  readonly transitions: ReceiveOperationTransition[] = []

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    this.transitions.push(transition)
    for (const record of transition.records ?? []) this.records.set(record.id, record)
    for (const handle of transition.handles ?? []) this.handles.set(handle.id, handle)
    for (const id of transition.deleteRecordIds ?? []) this.records.delete(id)
    for (const id of transition.deleteHandleIds ?? []) this.handles.delete(id)
  }

  async readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return this.records.get(id)
  }

  async readLifecycle(): Promise<PersistedReceiveRecord | undefined> {
    return undefined
  }

  async listRecords(operationId: string): Promise<readonly PersistedReceiveRecord[]> {
    return [...this.records.values()].filter((record) => record.operationId === operationId)
  }

  async listManifestPages(): Promise<readonly []> {
    return []
  }

  async readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return this.handles.get(id) as ReceiveOperationHandleRecord<T> | undefined
  }

  async readLease(): Promise<undefined> {
    return undefined
  }

  close(): void {}
}

function directoryHandle(name: string, token: string): FileSystemDirectoryHandle {
  const handle = {
    kind: 'directory' as const,
    name,
    isSameEntry: async (other: FileSystemHandle) =>
      (other as unknown as { readonly token?: string }).token === token,
    token,
  }
  return handle as unknown as FileSystemDirectoryHandle
}
