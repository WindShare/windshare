import { afterEach, describe, expect, it, vi } from 'vitest'
import { IndexedDbFileCheckpointRepository } from '../../src/output/browser/indexeddb-repository'
import { OriginPrivatePackageStore } from '../../src/output/origin-private/package-store'
import { OriginPrivateWorkspaceRoot } from '../../src/output/origin-private/workspace-root'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  newFileCheckpointV2, type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { finalFileCheckpointProof } from '../../src/output/persistence/journal'
import { readCompletedOriginalFile } from '../../src/output/resume/original-checkpoint'
import { sealCompletedOriginalFile } from '../../src/output/resume/reopen/original-continuation'
import type { WorkspaceContinuationInput } from '../../src/output/resume/reopen/workspace-continuation-authority'
import { receiveOperationLeaseRecord } from '../../src/output/workspace/records'
import { WorkspaceOperationStages } from '../../src/output/workspace/stages'
import { identity } from './planning/fixture'
import {
  MemoryDirectoryHandle, MemoryOperationRepository, MemoryRepositoryState, resumableReceive,
  seedWorkspaceAdmission, workspaceIntent,
} from './resume-reopen-authority-fixture'

afterEach(() => vi.restoreAllMocks())

async function fixture() {
  const state = new MemoryRepositoryState()
  const intent = await workspaceIntent()
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file') throw new Error('fixture')
  const { budget } = await seedWorkspaceAdmission(state, intent)
  const checkpoint = newFileCheckpointV2({
    operationId: intent.operationId, receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    fileId: intent.artifact.fileId, fileRevision: identity(97), canonicalPath: [intent.artifact.suggestedName],
    exactSize: 5n, materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef, ownedObjectId: identity(98, 32),
    stateGeneration: 2n, checkpointGeneration: 3n,
    verifiedRanges: [{ start: 0n, end: 5n }], commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  return { state, intent, budget, checkpoint }
}

function journal(records: readonly FileCheckpointV2[]) {
  return {
    scanCommitted: vi.fn(async () => ({ records })),
    finalCheckpointProof: vi.fn(async () => finalFileCheckpointProof(records[0]!)),
    close: vi.fn(),
  }
}

describe('original-file final checkpoint recovery', () => {
  it('derives local completion from the single admitted file without lifecycle progress counters', async () => {
    const { intent, budget, checkpoint } = await fixture()
    const store = journal([checkpoint])
    expect(await readCompletedOriginalFile(intent, budget, store)).toEqual(finalFileCheckpointProof(checkpoint))
    expect(store.scanCommitted).toHaveBeenCalledWith({ direction: 'ascending', limit: 2 })
  })

  it.each(['missing', 'incomplete', 'ambiguous', 'different-path', 'different-size'] as const)(
    'does not infer local completion from %s authority', async (cut) => {
      const { intent, budget, checkpoint } = await fixture()
      let records: readonly FileCheckpointV2[] = [checkpoint]
      if (cut === 'missing') records = []
      if (cut === 'incomplete') records = [newFileCheckpointV2({ ...checkpoint, verifiedRanges: [{ start: 0n, end: 2n }] })]
      if (cut === 'ambiguous') records = [checkpoint, checkpoint]
      if (cut === 'different-path') records = [newFileCheckpointV2({ ...checkpoint, canonicalPath: ['elsewhere.bin'] })]
      if (cut === 'different-size') records = [newFileCheckpointV2({ ...checkpoint, exactSize: 6n, verifiedRanges: [{ start: 0n, end: 6n }] })]
      expect(await readCompletedOriginalFile(intent, budget, journal(records))).toBeUndefined()
    },
  )

  it.each(['receiving', 'resumable-receive'] as const)(
    'seals the %s crash cut offline using the real admission generation', async (kind) => {
      const { state, intent, budget, checkpoint } = await fixture()
      const paused = resumableReceive(intent, 8n)
      const lifecycle = kind === 'receiving'
        ? { kind, operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 8n, activeLeaseId: identity(43) } as const
        : paused
      await state.seedLifecycle(lifecycle)
      const leaseId = identity(44)
      state.lease = receiveOperationLeaseRecord({ operationId: intent.operationId, leaseId, acquiredAt: 10 })
      const repository = new MemoryOperationRepository(state)
      const stages = await WorkspaceOperationStages.open({
        repository, receiveIntent: intent, leaseId, clock: () => 20, contentRequests: { count: () => 0n },
      })
      const store = journal([checkpoint])
      vi.spyOn(IndexedDbFileCheckpointRepository, 'open').mockResolvedValue(store as unknown as IndexedDbFileCheckpointRepository)
      vi.spyOn(OriginPrivateWorkspaceRoot.prototype, 'authorize').mockResolvedValue(undefined)
      const localRead = vi.spyOn(OriginPrivatePackageStore.prototype, 'readOwnedFile')
        .mockResolvedValue(new File([new Uint8Array(5)], 'file.bin'))
      const authority = {
        repository, snapshot: { lifecycle, operation: { operationId: intent.operationId, receiveIntent: intent } },
        lease: { leaseId }, target: { namespace: { rootHandleId: 'retained-root', root: new MemoryDirectoryHandle('opfs').asHandle() } },
      } as unknown as WorkspaceContinuationInput
      const result = await sealCompletedOriginalFile({ authority, stages, budget, now: 20 })
      expect(result.kind).toBe('materialization-sealed')
      expect(result.generation).toBe(10n)
      expect(localRead).toHaveBeenCalledWith(checkpoint.ownedObjectId)
      expect(store.close).toHaveBeenCalledOnce()
      expect(state.pages.size).toBeGreaterThan(0)
    },
  )

  it('leaves the original recovery lifecycle unchanged if the retained file is truncated', async () => {
    const { state, intent, budget, checkpoint } = await fixture()
    const lifecycle = resumableReceive(intent, 8n)
    await state.seedLifecycle(lifecycle)
    const repository = new MemoryOperationRepository(state)
    vi.spyOn(IndexedDbFileCheckpointRepository, 'open')
      .mockResolvedValue(journal([checkpoint]) as unknown as IndexedDbFileCheckpointRepository)
    vi.spyOn(OriginPrivateWorkspaceRoot.prototype, 'authorize').mockResolvedValue(undefined)
    vi.spyOn(OriginPrivatePackageStore.prototype, 'readOwnedFile')
      .mockResolvedValue(new File([new Uint8Array(2)], 'file.bin'))
    const authority = {
      repository, snapshot: { lifecycle, operation: { operationId: intent.operationId, receiveIntent: intent } },
      lease: { leaseId: identity(44) }, target: { namespace: { rootHandleId: 'retained-root', root: new MemoryDirectoryHandle('opfs').asHandle() } },
    } as unknown as WorkspaceContinuationInput
    const stages = { sealMaterialization: vi.fn() } as unknown as WorkspaceOperationStages
    await expect(sealCompletedOriginalFile({ authority, stages, budget, now: 20 })).rejects.toThrow()
    expect(await state.lifecycle()).toEqual(lifecycle)
    expect(stages.sealMaterialization).not.toHaveBeenCalled()
  })
})
