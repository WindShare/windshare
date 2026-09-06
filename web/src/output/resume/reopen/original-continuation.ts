import { IndexedDbFileCheckpointRepository } from '../../browser/indexeddb-repository'
import { OriginPrivatePackageStore } from '../../origin-private/package-store'
import { OriginPrivateWorkspaceRoot } from '../../origin-private/workspace-root'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import type { WorkspaceBudgetV1 } from '../../workspace/budget'
import type { WorkspaceOperationStages } from '../../workspace/stages'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import { originalCheckpointBinding, readCompletedOriginalFile } from '../original-checkpoint'
import { persistReceiveResume } from './persistence'
import type { WorkspaceContinuationInput } from './workspace-continuation-authority'

/** Rebuild only the sole admitted original from a final checkpoint, before granting package authority. */
export async function sealCompletedOriginalFile(input: {
  readonly authority: WorkspaceContinuationInput
  readonly stages: WorkspaceOperationStages
  readonly budget: WorkspaceBudgetV1
  readonly now: number
  readonly checkpointDatabaseName?: string
}) {
  const { authority, stages, budget } = input
  const intent = authority.snapshot.operation.receiveIntent
  const binding = originalCheckpointBinding(intent)
  const checkpoints = await IndexedDbFileCheckpointRepository.open(binding, input.checkpointDatabaseName)
  try {
    const proof = await readCompletedOriginalFile(intent, budget, checkpoints)
    if (proof === undefined || budget.evidence.kind !== 'single-file') {
      throw new DOMException('Completed original-file recovery authority is unavailable', 'InvalidStateError')
    }
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBindingDigest: budget.workspaceBindingDigest,
      authorityRef: binding.authorityRef,
      workspaceRootHandleId: authority.target.namespace.rootHandleId,
      workspaceRootHandle: authority.target.namespace.root,
      repository: authority.repository,
    })
    await root.authorize()
    const packages = new OriginPrivatePackageStore({
      root, operationRepository: authority.repository, checkpointHandles: checkpoints,
    })
    // Metadata alone cannot authorize a missing or replaced OPFS object.
    const file = await packages.readOwnedFile(proof.ownedObjectId)
    if (BigInt(file.size) !== proof.exactSize) throw new TargetOwnershipUnknownError('commit', intent.operationId)
    await persistReceiveResume(authority.repository, authority.snapshot, authority.lease, input.now)
    await stages.sealMaterialization({
      transferJobId: intent.operationId,
      generations: [{
        directoryId: budget.evidence.containingDirectoryId,
        generation: budget.evidence.generation,
      }],
      entries: [{
        kind: 'file',
        artifactPath: proof.canonicalPath,
        fileId: proof.fileId,
        fileRevision: proof.fileRevision,
        exactSize: proof.exactSize,
        ownedObjectId: proof.ownedObjectId,
        checkpoint: {
          recordId: proof.recordId,
          recordDigest: proof.recordDigest,
          checkpointGeneration: proof.checkpointGeneration,
        },
      }],
      checkpoints: { readFinalCheckpoint: (recordId, generation) => checkpoints.finalCheckpointProof(recordId, generation) },
    })
    const record = await authority.repository.readLifecycle(intent.operationId)
    const lifecycle = record === undefined ? undefined : await decodeStoredReceiveLifecycleState(record)
    if (lifecycle?.kind !== 'materialization-sealed') throw new TypeError('Original recovery did not seal materialization')
    return lifecycle
  } finally {
    checkpoints.close()
  }
}
