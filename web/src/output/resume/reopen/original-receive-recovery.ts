import { IndexedDbFileCheckpointRepository } from '../../browser/indexeddb-repository'
import { emitOutputTrace, outputTraceEvent } from '../../diagnostics'
import { OriginPrivatePackageStore } from '../../origin-private/package-store'
import { OriginPrivateWorkspaceRoot } from '../../origin-private/workspace-root'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import type { WorkspaceBudgetV1 } from '../../workspace/budget'
import { recoverAbandonedOperation } from '../../workspace/recovery'
import type { ReceiveLifecycleState } from '../../workspace/state'
import { originalCheckpointBinding, originalReceiveCheckpointSummary,
  readOriginalReceiveCheckpoint } from '../../origin-private/recovery/receive-checkpoint'
import type { WorkspaceContinuationInput } from './workspace-continuation-authority'

/** Recover durable evidence before admission so a rejected retry cannot strand an active lifecycle. */
export async function recoverOriginalFileReceive(input: {
  readonly authority: WorkspaceContinuationInput
  readonly budget: WorkspaceBudgetV1
  readonly now: number
  readonly checkpointDatabaseName?: string
}): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-receive'; payloadKind: 'file-set' }>> {
  const { authority, budget } = input
  const intent = authority.snapshot.operation.receiveIntent
  if (authority.snapshot.lifecycle.kind !== 'receiving' || budget.evidence.kind !== 'single-file') {
    throw new TypeError('Interrupted original recovery requires an admitted single-file receive')
  }
  const binding = originalCheckpointBinding(intent)
  const checkpoints = await IndexedDbFileCheckpointRepository.open(binding, input.checkpointDatabaseName)
  try {
    const checkpoint = await readOriginalReceiveCheckpoint(intent, budget.evidence.catalogSize, checkpoints)
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
    if (checkpoint !== undefined) {
      const store = new OriginPrivatePackageStore({
        root, operationRepository: authority.repository, checkpointHandles: checkpoints,
      })
      const file = await store.readOwnedFile(checkpoint.ownedObjectId)
      // Uncommitted tails may survive a crash; only committed ranges authorize reused bytes.
      if (checkpoint.verifiedRanges.some(range => range.end > BigInt(file.size)) ||
          BigInt(file.size) > checkpoint.exactSize) {
        throw new TargetOwnershipUnknownError('checkpoint', intent.operationId)
      }
    }
    const summary = await originalReceiveCheckpointSummary(intent, checkpoint)
    const recovered = recoverAbandonedOperation(authority.snapshot.lifecycle, {
      kind: 'verified-receive',
      ...summary,
      selectionFacts: {
        discoveredFileCount: 1n,
        discoveredBytes: budget.evidence.catalogSize,
        discovery: 'complete',
      },
      lastVerifiedRecordDigest: checkpoint?.checksum ?? authority.snapshot.operationRecord.digest,
    }, { planKind: 'workspace-then-publish', nowMilliseconds: input.now }).state
    if (recovered.kind !== 'resumable-receive' || recovered.payloadKind !== 'file-set') {
      throw new TypeError('Verified original recovery did not produce a receive continuation')
    }
    await authority.repository.commitTransition({
      operationId: intent.operationId,
      expectedLifecycleGeneration: authority.snapshot.lifecycle.generation,
      expectedLeaseId: authority.lease.leaseId,
      lifecycle: recovered,
    })
    emitOutputTrace(authority.diagnostics?.trace, () => outputTraceEvent('reopen', {
      backend: 'origin_private',
      transition: 'receive_recovered',
      operation_id: intent.operationId,
      lifecycle_generation: recovered.generation.toString(),
      checkpoint_count: checkpoint === undefined ? '0' : '1',
      verified_bytes: recovered.retainedBytes.toString(),
    }))
    return recovered
  } finally {
    checkpoints.close()
  }
}
