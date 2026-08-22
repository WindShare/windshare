import { describe, expect, it, vi } from 'vitest'

import { deriveArtifactChoiceIdentity, type ReceiveIntent } from '../../src/transfer/intent'

import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import {
  openOriginPrivateWorkspaceNamespace,
  reopenOriginPrivateWorkspaceNamespace,
} from '../../src/output/origin-private/namespace'
import type {
  OriginPrivatePackageContinuationBackend,
  OriginPrivateWorkspaceBackend,
} from '../../src/output/origin-private/session'
import type { OriginPrivatePackageStore } from '../../src/output/origin-private/package-store'
import { originPrivatePackageHandleId } from '../../src/output/origin-private/package-store'
import {
  AuthorityOwnedReceiveOperationMutationPort,
  PersistedReceiveOperationCleanupExecutor,
  PersistedReceiveOperationDeadlineElapsedError,
  PersistedReceiveOperationNeedsAttentionError,
  PersistedReceiveOperationReopenAuthority,
} from '../../src/output/resume/reopen-authority'
import {
  admitWorkspaceBudget,
  type WorkspaceCapacitySnapshot,
} from '../../src/output/workspace/budget'
import { createOriginalFileArtifactVerificationReceipt } from '../../src/output/workspace/receipts'
import { sealWorkspaceZipPreparation } from '../../src/output/workspace/preparation'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  receiveOperationHandleRecord,
  receiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import { WORKSPACE_HANDLE_PACKAGE_OBJECT } from '../../src/output/workspace/stages'
import { identity } from './planning/fixture'
import {
  MemoryDirectoryHandle,
  MemoryLockManager,
  MemoryOperationRepository,
  MemoryRepositoryState,
  bytesFilled,
  directTreeIntent,
  memoryStorage,
  reopenAuthority,
  requiredDescriptor,
  retainedCleanupBackend,
  resumableReceive,
  seedFSAOperationBinding,
  seedWorkspaceAdmission,
  seedWorkspacePackage,
  seedWorkspaceZipAdmission,
  workspaceIntent,
  workspaceZipIntent,
  workspaceZipPreparationInput,
} from './resume-reopen-authority-fixture'

const ENTERED_AT = 10_000
const WORKSPACE_CAPACITY = Object.freeze({
  jobLimitBytes: 1_000_000n,
  processLimitBytes: 2_000_000n,
  otherActiveJobPeakBytes: 0n,
  estimatedQuotaBytes: 3_000_000n,
  currentUsageBytes: 0n,
  minimumReserveBytes: 0n,
  verifiedAlreadyOwnedBytes: 0n,
}) satisfies WorkspaceCapacitySnapshot

describe('persisted receive operation reopen authority', () => {
  it('reopens DirectTree from a fresh repository instance and resumes under a fresh lease', async () => {
    const state = new MemoryRepositoryState()
    const intent = await directTreeIntent()
    const parent = new MemoryDirectoryHandle('downloads')
    await seedFSAOperationBinding(new MemoryOperationRepository(state), intent, parent.asHandle())
    const lifecycle = resumableReceive(intent, 4n)
    await state.seedLifecycle(lifecycle)
    const staleLeaseId = identity(80)
    state.lease = receiveOperationLeaseRecord({
      operationId: intent.operationId,
      leaseId: staleLeaseId,
      acquiredAt: ENTERED_AT - 1,
    })
    const descriptor = requiredDescriptor(lifecycle, ENTERED_AT + 1)
    let repositoryInstances = 0
    const trace = vi.fn()
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => {
        repositoryInstances += 1
        return new MemoryOperationRepository(state)
      },
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(81),
      },
      trace,
    })

    const reopened = await authority.reopen(descriptor, 'continue')

    expect(repositoryInstances).toBe(1)
    expect(reopened).toMatchObject({
      kind: 'direct-tree',
      intent: { operationId: intent.operationId, digest: intent.digest },
      lifecycle: { kind: 'receiving', generation: 5n },
      binding: { parent: parent.asHandle() },
    })
    expect(reopened.lease.leaseId).not.toBe(staleLeaseId)
    expect(reopened.lifecycle).toMatchObject({ activeLeaseId: reopened.lease.leaseId })
    if (reopened.kind !== 'direct-tree') throw new Error('test reopened the wrong route')
    expect(reopened.receiveAdmissionFallback).toEqual(lifecycle)
    expect(trace).toHaveBeenCalledWith(expect.objectContaining({
      name: 'receive.operation.reopen_authorized',
      operation_id: intent.operationId,
      lease_id: reopened.lease.leaseId,
    }))

    await reopened.close()
    expect(state.lease).toBeUndefined()
  })

  it('delegates fresh-page DirectTree discard and preserves PartialDirectory semantics', async () => {
    const state = new MemoryRepositoryState()
    const intent = await directTreeIntent()
    const parent = new MemoryDirectoryHandle('downloads')
    await seedFSAOperationBinding(new MemoryOperationRepository(state), intent, parent.asHandle())
    const lifecycle = resumableReceive(intent, 5n)
    await state.seedLifecycle(lifecycle)
    const receiptDigest = identity(84, 32)
    const discardDirectTree = vi.fn(async ({ operation }) => {
      expect(operation).toMatchObject({
        kind: 'direct-tree',
        intent: { operationId: intent.operationId, digest: intent.digest },
        lifecycle: { kind: 'resumable-receive', generation: lifecycle.generation },
      })
      return Object.freeze({
        lifecycle: Object.freeze({
          operationId: intent.operationId,
          receiveIntentDigest: intent.digest,
          generation: lifecycle.generation + 1n,
          kind: 'partial-directory' as const,
          reason: 'stopped' as const,
          successCount: 1n,
          failureCount: 0n,
          receiptDigest,
        }),
        receiptDigest,
      })
    })
    const mutation = new AuthorityOwnedReceiveOperationMutationPort({
      reopen: reopenAuthority(state, ENTERED_AT + 1, 85),
      cleanup: new PersistedReceiveOperationCleanupExecutor({ discardDirectTree }),
    })

    await expect(mutation.discard(requiredDescriptor(lifecycle, ENTERED_AT + 1)))
      .resolves.toEqual({ kind: 'partial-directory', receiptDigest })
    expect(discardDirectTree).toHaveBeenCalledOnce()
    expect(state.lease).toBeUndefined()
  })

  it('reopens the exact origin-private namespace without creating a replacement', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    const initial = await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(71, 32),
    })
    const lifecycle = resumableReceive(intent, 6n)
    await state.seedLifecycle(lifecycle)
    const admission = await seedWorkspaceAdmission(state, intent)
    const releaseBudget = vi.fn(async () => undefined)
    const reclaimWorkspaceBudget = vi.fn(async (input) => {
      expect(input.budget.digest).toBe(admission.budget.digest)
      expect(input.receipt.digest).toBe(admission.receipt.digest)
      const accepted = admitWorkspaceBudget(input.budget, WORKSPACE_CAPACITY)
      if (accepted.kind !== 'accepted') throw new Error('test recovery budget was rejected')
      return Object.freeze({
        kind: 'accepted' as const,
        claim: Object.freeze({
          budgetDigest: input.budget.digest,
          capacity: WORKSPACE_CAPACITY,
          admission: accepted,
          release: releaseBudget,
        }),
      })
    })
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(72),
      },
      reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
        ...input,
        storage,
      }),
      reclaimWorkspaceBudget,
    })

    const reopened = await authority.reopen(
      requiredDescriptor(lifecycle, ENTERED_AT + 1),
      'continue',
    )

    expect(reopened).toMatchObject({
      kind: 'workspace',
      intent: { operationId: intent.operationId, digest: intent.digest },
      lifecycle: { kind: 'receiving', generation: 7n },
      namespace: {
        root: initial.root,
        rootHandleId: initial.rootHandleId,
        rootOwnedObjectId: initial.rootOwnedObjectId,
      },
    })
    expect(storageRoot.creationCount).toBe(1)
    if (reopened.kind !== 'workspace') throw new Error('workspace reopen changed kind')
    expect(reopened.receiveAdmissionFallback).toEqual(lifecycle)
    expect(reopened.admittedContent).toMatchObject({
      budget: { digest: admission.budget.digest },
      admissionReceipt: { digest: admission.receipt.digest },
      claim: { budgetDigest: admission.budget.digest },
    })
    expect(reclaimWorkspaceBudget).toHaveBeenCalledTimes(1)
    await reopened.close()
    expect(releaseBudget).toHaveBeenCalledTimes(1)
  })

  it('reopens the exact persisted ZIP preparation before advancing fresh-page receive', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceZipIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(90, 32),
    })
    const preparation = await sealWorkspaceZipPreparation(workspaceZipPreparationInput(intent))
    const admission = await seedWorkspaceZipAdmission(state, intent, preparation)
    const lifecycle = resumableReceive(intent, 7n)
    await state.seedLifecycle(lifecycle)
    const accepted = admitWorkspaceBudget(admission.budget, WORKSPACE_CAPACITY)
    if (accepted.kind !== 'accepted') throw new Error('test recovery budget was rejected')
    const closeBackend = vi.fn(async () => undefined)
    const backend = Object.freeze({ close: closeBackend }) as unknown as OriginPrivateWorkspaceBackend
    const openWorkspaceReceiveBackend = vi.fn(async (input) => {
      expect(input).toMatchObject({
        receiveIntent: { operationId: intent.operationId, digest: intent.digest },
        namespace: { operationId: intent.operationId },
        contentGate: { preparationManifestDigest: preparation.manifest.digest },
      })
      return backend
    })
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(91),
      },
      reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
        ...input,
        storage,
      }),
      reclaimWorkspaceBudget: async () => Object.freeze({
        kind: 'accepted',
        claim: Object.freeze({
          budgetDigest: admission.budget.digest,
          capacity: WORKSPACE_CAPACITY,
          admission: accepted,
          readmit: async () => accepted,
          release: async () => undefined,
        }),
      }),
      openWorkspaceReceiveBackend,
    })

    const reopened = await authority.reopen(
      requiredDescriptor(lifecycle, ENTERED_AT + 1),
      'continue',
    )

    expect(reopened).toMatchObject({
      kind: 'workspace',
      lifecycle: { kind: 'receiving', generation: 8n },
      preparation: {
        manifest: { digest: preparation.manifest.digest },
        zipLayout: { digest: preparation.zipLayout.digest },
      },
    })
    if (reopened.kind !== 'workspace' || reopened.receiveContinuation === undefined) {
      throw new Error('workspace receive continuation is missing')
    }
    await expect(reopened.receiveContinuation.openBackend()).resolves.toBe(backend)
    await reopened.close()
    expect(openWorkspaceReceiveBackend).toHaveBeenCalledOnce()
    expect(closeBackend).toHaveBeenCalledOnce()
  })

})

describe('persisted workspace package continuation', () => {

  it('finishes a sealed package from a fresh authority without reopening sender content', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(92, 32),
    })
    const seeded = await seedWorkspacePackage(state, intent)
    const accepted = admitWorkspaceBudget(seeded.admission.budget, WORKSPACE_CAPACITY)
    if (accepted.kind !== 'accepted') throw new Error('test recovery budget was rejected')
    const releaseBudget = vi.fn(async () => undefined)
    const verifyManifestOwnership = vi.fn(async () => undefined)
    const verifyTemporaryCleanup = vi.fn(async () => undefined)
    const closeBackend = vi.fn(async () => undefined)
    const packageOwnedObjectId = identity(93, 32)
    const packageHandleId = originPrivatePackageHandleId(intent.operationId, packageOwnedObjectId)
    const packageHandle = Object.freeze({
      kind: 'file' as const,
      isSameEntry: async () => true,
    }) as unknown as FileSystemFileHandle
    const packages = Object.freeze({
      allocatePackage: async () => Object.freeze({
        ownedObjectId: packageOwnedObjectId,
        handleId: packageHandleId,
        handleRecord: receiveOperationHandleRecord({
          id: packageHandleId,
          operationId: intent.operationId,
          kind: WORKSPACE_HANDLE_PACKAGE_OBJECT,
          authorityRef: intent.plan.kind === 'workspace-then-publish'
            ? intent.plan.workspace.repositoryRef
            : identity(94, 32),
          ownedObjectId: packageOwnedObjectId,
          handle: packageHandle,
        }),
      }),
      promoteOriginalFile: async (input: {
        readonly sealedMaterializationDigest: string
        readonly artifactSpecDigest: string
      }) => createOriginalFileArtifactVerificationReceipt({
        operationId: intent.operationId,
        receiveIntentDigest: intent.digest,
        sealedMaterializationDigest: input.sealedMaterializationDigest,
        artifactSpecDigest: input.artifactSpecDigest,
        packageOwnedObjectId,
        exactBytes: seeded.proof.exactSize,
        finalCheckpointDigest: seeded.proof.recordDigest,
        finalCheckpointGeneration: seeded.proof.checkpointGeneration,
        promotionVerified: true,
      }),
      cleanupUncommittedPackage: async () => Object.freeze({
        schemaVersion: 1 as const,
        operationId: intent.operationId,
        packageOwnedObjectId,
        packageHandleId,
        result: 'removed' as const,
        canonicalBytes: new Uint8Array(),
        digest: identity(95, 32),
      }),
    }) as unknown as OriginPrivatePackageStore
    const backend: OriginPrivatePackageContinuationBackend = Object.freeze({
      packages,
      finalCheckpoints: {
        readFinalCheckpoint: async () => seeded.proof,
        recoverFinalCheckpoint: async () => seeded.proof,
      },
      verifyManifestOwnership,
      verifyTemporaryCleanup,
      close: closeBackend,
    })
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(96),
      },
      reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
        ...input,
        storage,
      }),
      reclaimWorkspaceBudget: async () => Object.freeze({
        kind: 'accepted',
        claim: Object.freeze({
          budgetDigest: seeded.admission.budget.digest,
          capacity: WORKSPACE_CAPACITY,
          admission: accepted,
          readmit: async () => accepted,
          release: releaseBudget,
        }),
      }),
      openWorkspacePackageContinuation: async () => backend,
    })
    const mutation = new AuthorityOwnedReceiveOperationMutationPort({
      reopen: authority,
      cleanup: { cleanup: async () => { throw new Error('cleanup was not requested') } },
    })

    const result = await mutation.resume(requiredDescriptor(seeded.lifecycle, ENTERED_AT + 1))
    expect(result).toMatchObject({
      kind: 'continuation',
      continuation: { kind: 'workspace-package' },
    })
    if (result.kind !== 'continuation' || result.continuation.kind !== 'workspace-package') {
      throw new Error('fresh-page package continuation changed route')
    }
    const packaged = await result.continuation.operation.packageContinuation.execute(
      new AbortController().signal,
    )

    expect(packaged).toMatchObject({ kind: 'sealed', state: { kind: 'waiting-to-save' } })
    expect(await state.lifecycle()).toMatchObject({ kind: 'waiting-to-save' })
    expect(verifyManifestOwnership).toHaveBeenCalledOnce()
    expect(verifyTemporaryCleanup).toHaveBeenCalledOnce()
    await result.continuation.operation.close()
    expect(closeBackend).toHaveBeenCalledOnce()
    expect(releaseBudget).toHaveBeenCalledOnce()
  })

  it('moves a resumable package to NeedsAttention when its cleanup receipt is missing', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(109, 32),
    })
    const seeded = await seedWorkspacePackage(state, intent)
    const cleanup = [...state.records.values()].find((record) =>
      record.digest === seeded.lifecycle.tempCleanupProofDigest)
    if (cleanup === undefined) throw new Error('package cleanup fixture is missing')
    state.records.delete(cleanup.id)
    const accepted = admitWorkspaceBudget(seeded.admission.budget, WORKSPACE_CAPACITY)
    if (accepted.kind !== 'accepted') throw new Error('test recovery budget was rejected')
    const openWorkspacePackageContinuation = vi.fn()
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(110),
      },
      reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
        ...input,
        storage,
      }),
      reclaimWorkspaceBudget: async () => Object.freeze({
        kind: 'accepted',
        claim: Object.freeze({
          budgetDigest: seeded.admission.budget.digest,
          capacity: WORKSPACE_CAPACITY,
          admission: accepted,
          readmit: async () => accepted,
          release: async () => undefined,
        }),
      }),
      openWorkspacePackageContinuation,
    })

    await expect(authority.reopen(
      requiredDescriptor(seeded.lifecycle, ENTERED_AT + 1),
      'continue',
    )).rejects.toBeInstanceOf(PersistedReceiveOperationNeedsAttentionError)
    expect(await state.lifecycle()).toMatchObject({
      kind: 'needs-attention',
      generation: seeded.lifecycle.generation + 1n,
      reason: 'target-ownership-unknown',
    })
    expect(openWorkspacePackageContinuation).not.toHaveBeenCalled()
  })

  it('persists NeedsAttention when a workspace has no unique admission receipt', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(82, 32),
    })
    const lifecycle = resumableReceive(intent, 8n)
    await state.seedLifecycle(lifecycle)
    const reclaimWorkspaceBudget = vi.fn()
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(83),
      },
      reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
        ...input,
        storage,
      }),
      reclaimWorkspaceBudget,
    })

    await expect(authority.reopen(
      requiredDescriptor(lifecycle, ENTERED_AT + 1),
      'continue',
    )).rejects.toBeInstanceOf(PersistedReceiveOperationNeedsAttentionError)

    expect(await state.lifecycle()).toMatchObject({
      kind: 'needs-attention',
      generation: 9n,
      reason: 'target-ownership-unknown',
    })
    expect(reclaimWorkspaceBudget).not.toHaveBeenCalled()
    expect(state.lease).toBeUndefined()
  })

  it('persists NeedsAttention when external binding ownership is unprovable', async () => {
    const state = new MemoryRepositoryState()
    const intent = await directTreeIntent()
    await seedFSAOperationBinding(
      new MemoryOperationRepository(state),
      intent,
      new MemoryDirectoryHandle('downloads').asHandle(),
    )
    const lifecycle = resumableReceive(intent, 8n)
    await state.seedLifecycle(lifecycle)
    const parent = [...state.handles.values()][0]!
    state.handles.set(parent.id, Object.freeze({
      ...parent,
      handle: Object.freeze({ kind: 'file' }),
    }))
    const authority = reopenAuthority(state, ENTERED_AT + 1, 73)

    await expect(authority.reopen(
      requiredDescriptor(lifecycle, ENTERED_AT + 1),
      'continue',
    )).rejects.toBeInstanceOf(PersistedReceiveOperationNeedsAttentionError)

    expect((await state.lifecycle()).kind).toBe('needs-attention')
    expect(await state.lifecycle()).toMatchObject({
      generation: 9n,
      reason: 'target-ownership-unknown',
    })
    expect(state.lease).toBeUndefined()
  })
})

async function selectedRanking(intent: ReceiveIntent) {
  return Object.freeze([(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id])
}

describe('persisted receive operation lifecycle and cleanup authority', () => {
  it('commits Expired before a continuation can cross the exact 24-hour deadline', async () => {
    const state = new MemoryRepositoryState()
    const intent = await directTreeIntent()
    await seedFSAOperationBinding(
      new MemoryOperationRepository(state),
      intent,
      new MemoryDirectoryHandle('downloads').asHandle(),
    )
    const lifecycle = resumableReceive(intent, 10n)
    await state.seedLifecycle(lifecycle)
    const descriptor = requiredDescriptor(lifecycle, ENTERED_AT + 1)
    const authority = reopenAuthority(state, lifecycle.expiresAt, 74)

    await expect(authority.reopen(descriptor, 'continue'))
      .rejects.toBeInstanceOf(PersistedReceiveOperationDeadlineElapsedError)

    expect(await state.lifecycle()).toMatchObject({
      kind: 'expired',
      generation: 11n,
      expiresAt: lifecycle.expiresAt,
      cleanupState: 'cleanup-pending',
    })
    expect([...state.records.values()].filter((record) =>
      record.kind === RECEIVE_RECORD_RECEIPT)).toHaveLength(1)
    expect(state.lease).toBeUndefined()
  })

  it('rejects a concurrent lifecycle advance at the acquisition generation fence', async () => {
    const state = new MemoryRepositoryState()
    const intent = await directTreeIntent()
    await seedFSAOperationBinding(
      new MemoryOperationRepository(state),
      intent,
      new MemoryDirectoryHandle('downloads').asHandle(),
    )
    const lifecycle = resumableReceive(intent, 12n)
    await state.seedLifecycle(lifecycle)
    const verifyBinding = vi.fn()
    const authority = new PersistedReceiveOperationReopenAuthority({
      repositoryFactory: async () => new MemoryOperationRepository(state),
      clock: { now: () => ENTERED_AT + 1 },
      leaseOptions: {
        manager: new MemoryLockManager(),
        randomBytes: bytesFilled(75),
      },
      acquireLease: async (repository, operationId, options) => {
        await state.seedLifecycle(Object.freeze({ ...lifecycle, generation: 13n }))
        return acquireBrowserReceiveOperationLease(repository, operationId, options)
      },
      verifyDirectTreeBinding: verifyBinding,
    })

    await expect(authority.reopen(
      requiredDescriptor(lifecycle, ENTERED_AT + 1),
      'continue',
    )).rejects.toThrow('concurrent')
    expect(verifyBinding).not.toHaveBeenCalled()
    expect(state.lease).toBeUndefined()
  })

  it('persists a workspace discard receipt through the output-owned cleanup executor', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(76, 32),
    })
    const lifecycle = resumableReceive(intent, 14n)
    await state.seedLifecycle(lifecycle)
    const backend = retainedCleanupBackend('clean')
    const mutation = new AuthorityOwnedReceiveOperationMutationPort({
      reopen: new PersistedReceiveOperationReopenAuthority({
        repositoryFactory: async () => new MemoryOperationRepository(state),
        clock: { now: () => ENTERED_AT + 1 },
        leaseOptions: {
          manager: new MemoryLockManager(),
          randomBytes: bytesFilled(78),
        },
        reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
          ...input,
          storage,
        }),
      }),
      cleanup: new PersistedReceiveOperationCleanupExecutor({
        openWorkspaceBackend: async () => backend,
      }),
    })

    const result = await mutation.discard(requiredDescriptor(lifecycle, ENTERED_AT + 1))

    expect(result).toMatchObject({ kind: 'discarded' })
    expect(await state.lifecycle()).toMatchObject({
      kind: 'discarded',
      cleanupReceiptDigest: result.kind === 'discarded'
        ? result.cleanupReceiptDigest
        : undefined,
    })
    expect([...state.records.values()].filter((record) =>
      record.kind === RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(backend.close).toHaveBeenCalledOnce()
    expect(state.lease).toBeUndefined()
  })

  it('persists NeedsAttention and never claims clean when workspace cleanup ownership is unknown', async () => {
    const state = new MemoryRepositoryState()
    const intent = await workspaceIntent()
    const storageRoot = new MemoryDirectoryHandle('opfs')
    const storage = memoryStorage(storageRoot)
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      preClickRanking: await selectedRanking(intent),
      repository: new MemoryOperationRepository(state),
      storage,
      randomOwnedObjectId: () => identity(79, 32),
    })
    const lifecycle = resumableReceive(intent, 16n)
    await state.seedLifecycle(lifecycle)
    const backend = retainedCleanupBackend('ownership-unknown')
    const mutation = new AuthorityOwnedReceiveOperationMutationPort({
      reopen: new PersistedReceiveOperationReopenAuthority({
        repositoryFactory: async () => new MemoryOperationRepository(state),
        clock: { now: () => ENTERED_AT + 1 },
        leaseOptions: {
          manager: new MemoryLockManager(),
          randomBytes: bytesFilled(80),
        },
        reopenWorkspaceNamespace: async (input) => reopenOriginPrivateWorkspaceNamespace({
          ...input,
          storage,
        }),
      }),
      cleanup: new PersistedReceiveOperationCleanupExecutor({
        openWorkspaceBackend: async () => backend,
      }),
    })

    await expect(mutation.discard(requiredDescriptor(lifecycle, ENTERED_AT + 1)))
      .resolves.toEqual({ kind: 'needs-attention', reason: 'cleanup-unknown' })
    expect(await state.lifecycle()).toMatchObject({
      kind: 'needs-attention',
      generation: 17n,
      reason: 'cleanup-unknown',
    })
    expect([...state.records.values()].filter((record) =>
      record.kind === RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(state.lease).toBeUndefined()
  })
})
