import { afterEach, describe, expect, it, vi } from 'vitest'
import { IndexedDbFileCheckpointRepository } from '../../src/output/browser/indexeddb-repository'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import { OriginPrivatePackageStore } from '../../src/output/origin-private/package-store'
import { OriginPrivateWorkspaceRoot } from '../../src/output/origin-private/workspace-root'
import { openOriginPrivateWorkspaceNamespace, reopenOriginPrivateWorkspaceNamespace } from '../../src/output/origin-private/namespace'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  newFileCheckpointV2, type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import { PersistedReceiveOperationReopenAuthority } from '../../src/output/resume/reopen-authority'
import { admitWorkspaceBudget } from '../../src/output/workspace/budget'
import { WorkspaceOperationStages, type WorkspaceBudgetClaimResult } from '../../src/output/workspace/stages'
import { deriveArtifactChoiceIdentity } from '../../src/transfer/intent'
import { identity } from './planning/fixture'
import {
  MemoryDirectoryHandle, MemoryLockManager, MemoryOperationRepository, MemoryRepositoryState,
  bytesFilled, memoryStorage, requiredDescriptor, seedWorkspaceAdmission, workspaceIntent,
} from './resume-reopen-authority-fixture'

afterEach(() => vi.restoreAllMocks())

const CAPACITY = {
  outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n,
  estimatedQuotaBytes: 3_000_000n, currentUsageBytes: 0n, minimumReserveBytes: 0n, verifiedAlreadyOwnedBytes: 0n,
}
const NOW = 10_000

async function fixture(cut: 'absent' | 'empty' | 'partial' = 'partial') {
  const state = new MemoryRepositoryState()
  const intent = await workspaceIntent()
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file') throw new Error('fixture')
  const storageRoot = new MemoryDirectoryHandle('opfs')
  const storage = memoryStorage(storageRoot)
  await openOriginPrivateWorkspaceNamespace({
    receiveIntent: intent,
    preClickRanking: [(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id],
    repository: new MemoryOperationRepository(state), storage, randomOwnedObjectId: () => identity(71, 32),
  })
  const { budget } = await seedWorkspaceAdmission(state, intent)
  const lifecycle = {
    kind: 'receiving' as const, operationId: intent.operationId, receiveIntentDigest: intent.digest,
    generation: 2n, activeLeaseId: identity(80),
  }
  await state.seedLifecycle(lifecycle)
  const checkpoint = newFileCheckpointV2({
    operationId: intent.operationId, receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    fileId: intent.artifact.fileId, fileRevision: identity(97), canonicalPath: [intent.artifact.suggestedName],
    exactSize: 5n, materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef, ownedObjectId: identity(98, 32),
    stateGeneration: 1n, checkpointGeneration: cut === 'partial' ? 1n : 0n,
    verifiedRanges: cut === 'partial' ? [{ start: 0n, end: 3n }] : [], commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const scanCommitted = vi.fn(async (): Promise<{ records: readonly FileCheckpointV2[]; nextCursor?: string }> =>
    ({ records: cut === 'absent' ? [] : [checkpoint] }))
  const store = { scanCommitted, close: vi.fn() }
  vi.spyOn(IndexedDbFileCheckpointRepository, 'open').mockResolvedValue(store as unknown as IndexedDbFileCheckpointRepository)
  vi.spyOn(OriginPrivateWorkspaceRoot.prototype, 'authorize').mockResolvedValue(undefined)
  const readOwnedFile = vi.spyOn(OriginPrivatePackageStore.prototype, 'readOwnedFile')
    .mockResolvedValue(new File([Uint8Array.of(1, 2, 3, 4)], 'retained.bin'))
  const releaseClaim = vi.fn(async () => undefined)
  const admitted = admitWorkspaceBudget(budget, CAPACITY)
  if (admitted.kind !== 'accepted') throw new Error('fixture budget')
  const claim = { budgetDigest: budget.digest, capacity: CAPACITY, admission: admitted, release: releaseClaim }
  const reclaimWorkspaceBudget = vi.fn(async (): Promise<WorkspaceBudgetClaimResult> => ({ kind: 'accepted', claim }))
  const trace = vi.fn<(event: OutputTraceEvent) => void>()
  const authority = new PersistedReceiveOperationReopenAuthority({
    repositoryFactory: async () => new MemoryOperationRepository(state),
    clock: { now: () => NOW },
    leaseOptions: { manager: new MemoryLockManager(), randomBytes: bytesFilled(81) },
    reopenWorkspaceNamespace: input => reopenOriginPrivateWorkspaceNamespace({ ...input, storage }),
    reclaimWorkspaceBudget,
    outputTrace: { current: trace },
  })
  return { state, intent, budget, lifecycle, checkpoint, store, readOwnedFile, reclaimWorkspaceBudget,
    releaseClaim, authority, trace, storageRoot }
}

describe('interrupted original-file receive recovery', () => {
  it.each(['absent', 'empty', 'partial'] as const)('reopens a %s durable cut with an exact admission fallback', async cut => {
    const f = await fixture(cut)
    const reopened = await f.authority.reopen(requiredDescriptor(f.lifecycle), 'continue')
    try {
      expect(reopened.kind).toBe('workspace')
      if (reopened.kind !== 'workspace') throw new Error('unexpected target')
      expect(reopened.lifecycle).toMatchObject({ kind: 'receiving', generation: 4n, activeLeaseId: reopened.lease.leaseId })
      expect(reopened.receiveAdmissionFallback).toMatchObject({
        kind: 'resumable-receive', payloadKind: 'file-set', generation: 3n,
        completedFileCount: 0n, completedBytes: 0n, retainedBytes: cut === 'partial' ? 3n : 0n,
        selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 5n, discovery: 'complete' },
      })
      expect(reopened.receiveContinuation).toBeDefined()
      expect(f.storageRoot.creationCount).toBe(1)
      expect(f.store.close).toHaveBeenCalledOnce()
      expect(f.readOwnedFile).toHaveBeenCalledTimes(cut === 'absent' ? 0 : 1)
      expect(f.trace).toHaveBeenCalledWith({
        eventName: 'reopen',
        payload: { backend: 'origin_private', transition: 'receive_recovered',
          operation_id: f.intent.operationId, lifecycle_generation: '3',
          checkpoint_count: cut === 'absent' ? '0' : '1', verified_bytes: cut === 'partial' ? '3' : '0' },
      })
      // Recovery summaries must not promote a partly received file to completed output.
      await reopened.stages.restoreReceiveContinuation(reopened.receiveAdmissionFallback!)
      expect(await f.state.lifecycle()).toMatchObject({
        kind: 'resumable-receive', completedFileCount: 0n, retainedBytes: cut === 'partial' ? 3n : 0n, completedBytes: 0n,
        checkpointSetDigest: reopened.receiveAdmissionFallback!.checkpointSetDigest,
      })
    } finally { await reopened.close() }
    expect(f.state.lease).toBeUndefined()
    expect(f.releaseClaim).toHaveBeenCalledOnce()
  })

  it('retains the reconstructed checkpoint when quota admission fails and permits a fresh retry', async () => {
    const f = await fixture()
    const capacity = { ...CAPACITY, estimatedQuotaBytes: 0n }
    const admission = admitWorkspaceBudget(f.budget, capacity)
    if (admission.kind !== 'rejected') throw new Error('fixture must reject quota')
    f.reclaimWorkspaceBudget.mockResolvedValueOnce({ kind: 'rejected', capacity, admission })
    await expect(f.authority.reopen(requiredDescriptor(f.lifecycle), 'continue'))
      .rejects.toThrow('no longer fits the current storage budget')
    const fallback = await f.state.lifecycle()
    expect(fallback).toMatchObject({ kind: 'resumable-receive', generation: 3n })
    expect(f.state.lease).toBeUndefined()
    expect(f.store.close).toHaveBeenCalledOnce()

    const reopened = await f.authority.reopen(requiredDescriptor(fallback), 'continue')
    expect(reopened.lifecycle).toMatchObject({ kind: 'receiving', generation: 4n })
    // A failed admission must reuse the stable recovery cut on retry.
    expect(f.store.scanCommitted).toHaveBeenCalledOnce()
    if (reopened.kind !== 'workspace') throw new Error('unexpected target')
    expect(reopened.receiveAdmissionFallback).toEqual(fallback)
    await reopened.close()
    expect(f.state.lease).toBeUndefined()
  })

  it('restores the recovered checkpoint if content admission fails after entering receiving', async () => {
    const f = await fixture()
    const failure = new DOMException('Content admission interrupted', 'AbortError')
    vi.spyOn(WorkspaceOperationStages.prototype, 'reopenAdmittedContent').mockRejectedValueOnce(failure)
    await expect(f.authority.reopen(requiredDescriptor(f.lifecycle), 'continue')).rejects.toBe(failure)
    const stable = await f.state.lifecycle()
    expect(stable).toMatchObject({ kind: 'resumable-receive', generation: 5n, completedBytes: 0n })
    expect(f.state.lease).toBeUndefined()
    expect(f.releaseClaim).toHaveBeenCalledOnce()
    const retried = await f.authority.reopen(requiredDescriptor(stable), 'continue')
    expect(retried.lifecycle).toMatchObject({ kind: 'receiving', generation: 6n })
    await retried.close()
  })

  it.each(['ambiguous', 'foreign-operation', 'foreign-binding', 'different-file', 'different-path',
    'different-size', 'checksum', 'truncated', 'replaced-object'] as const)(
    'does not authorize resumed content from %s recovery evidence', async fault => {
      const f = await fixture()
      let record = f.checkpoint
      if (fault === 'foreign-operation') record = newFileCheckpointV2({ ...record, operationId: identity(90) })
      if (fault === 'foreign-binding') record = newFileCheckpointV2({ ...record, materializationBindingDigest: identity(91, 32) })
      if (fault === 'different-file') record = newFileCheckpointV2({ ...record, fileId: identity(92) })
      if (fault === 'different-path') record = newFileCheckpointV2({ ...record, canonicalPath: ['foreign.bin'] })
      if (fault === 'different-size') record = newFileCheckpointV2({ ...record, exactSize: 6n })
      if (fault === 'checksum') record = { ...record, checksum: identity(93, 32) }
      f.store.scanCommitted.mockResolvedValue({ records: fault === 'ambiguous' ? [record, record] : [record] })
      if (fault === 'truncated') f.readOwnedFile.mockResolvedValue(new File([Uint8Array.of(1)], 'retained.bin'))
      if (fault === 'replaced-object') f.readOwnedFile.mockRejectedValue(new TargetOwnershipUnknownError('checkpoint', f.intent.operationId))
      await expect(f.authority.reopen(requiredDescriptor(f.lifecycle), 'continue'))
        .rejects.toThrow('ownership requires attention')
      expect(await f.state.lifecycle()).toMatchObject({ kind: 'needs-attention', reason: 'target-ownership-unknown' })
      expect(f.reclaimWorkspaceBudget).not.toHaveBeenCalled()
      expect(f.store.close).toHaveBeenCalledOnce()
      expect(f.state.lease).toBeUndefined()
    },
  )

  it('does not let a failing recovery trace revoke the persisted continuation', async () => {
    const f = await fixture()
    f.trace.mockImplementation(() => { throw new Error('trace sink unavailable') })
    const reopened = await f.authority.reopen(requiredDescriptor(f.lifecycle), 'continue')
    expect(reopened.lifecycle.kind).toBe('receiving')
    await reopened.close()
  })
})
