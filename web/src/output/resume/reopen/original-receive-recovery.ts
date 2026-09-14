import { IndexedDbFileCheckpointRepository } from '../../browser/indexeddb-repository'
import { emitOutputTrace, outputTraceEvent } from '../../diagnostics'
import { OriginPrivatePackageStore } from '../../origin-private/package-store'
import { OriginPrivateWorkspaceRoot } from '../../origin-private/workspace-root'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_PHASE_ACTIVE, FILE_CHECKPOINT_PHASE_PAUSED,
  FileCheckpointError, fileCheckpointIsComplete, validateFileCheckpoint,
  type FileCheckpointV2,
} from '../../persistence/checkpoint'
import type { FileCheckpointJournal } from '../../persistence/journal'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import type { WorkspaceBudgetV1 } from '../../workspace/budget'
import { canonicalDigest, canonicalFrame, canonicalRecord, canonicalText } from '../../workspace/canonical'
import { recoverAbandonedOperation } from '../../workspace/recovery'
import type { ReceiveLifecycleState } from '../../workspace/state'
import { originalCheckpointBinding } from '../original-checkpoint'
import type { WorkspaceContinuationInput } from './workspace-continuation-authority'

const ORIGINAL_CHECKPOINT_AUTHORITY_BOUND = 2
const RECEIVE_CHECKPOINT_SET_DOMAIN = 'windshare/original-receive-checkpoint-set'
const RECEIVE_CHECKPOINT_SET_VERSION = 1

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
    const checkpoint = await readReceiveCheckpoint(authority, budget, checkpoints)
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
    const checkpointSetDigest = await canonicalDigest(canonicalRecord(
      RECEIVE_CHECKPOINT_SET_DOMAIN, RECEIVE_CHECKPOINT_SET_VERSION, [
        canonicalFrame(canonicalText(intent.digest)),
        canonicalFrame(canonicalText(checkpoint?.checksum ?? '')),
      ],
    ))
    const complete = checkpoint !== undefined && fileCheckpointIsComplete(checkpoint)
    const recovered = recoverAbandonedOperation(authority.snapshot.lifecycle, {
      kind: 'verified-receive',
      checkpointSetDigest,
      completedFileCount: complete ? 1n : 0n,
      completedBytes: complete ? checkpoint.exactSize : 0n,
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
      verified_bytes: (checkpoint?.verifiedRanges.reduce((sum, range) => sum + range.end - range.start, 0n) ?? 0n).toString(),
    }))
    return recovered
  } finally {
    checkpoints.close()
  }
}

async function readReceiveCheckpoint(
  authority: WorkspaceContinuationInput,
  budget: WorkspaceBudgetV1,
  checkpoints: Pick<FileCheckpointJournal, 'scanCommitted'>,
): Promise<FileCheckpointV2 | undefined> {
  const intent = authority.snapshot.operation.receiveIntent
  const binding = originalCheckpointBinding(intent)
  try {
    const page = await checkpoints.scanCommitted({ direction: 'ascending', limit: ORIGINAL_CHECKPOINT_AUTHORITY_BOUND })
    if (page.nextCursor !== undefined || page.records.length > 1) {
      throw new TypeError('Original receive checkpoint authority is ambiguous')
    }
    const record = page.records[0]
    // Receiving is persisted before the first file opens; no checkpoint is a valid zero-progress cut.
    if (record === undefined) return undefined
    validateFileCheckpoint(record)
    if (intent.artifact.kind !== 'original-file' || budget.evidence.kind !== 'single-file' ||
        record.operationId !== binding.operationId || record.receiveIntentDigest !== binding.receiveIntentDigest ||
        record.materializationBindingDigest !== binding.materializationBindingDigest ||
        record.materializerKind !== binding.materializerKind || record.authorityRef !== binding.authorityRef ||
        record.fileId !== intent.artifact.fileId || record.exactSize !== budget.evidence.catalogSize ||
        record.canonicalPath.length !== 1 || record.canonicalPath[0] !== intent.artifact.suggestedName ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
        (record.phase !== FILE_CHECKPOINT_PHASE_ACTIVE && record.phase !== FILE_CHECKPOINT_PHASE_PAUSED)) {
      throw new TypeError('Original receive checkpoint escaped its admitted file')
    }
    return record
  } catch (cause) {
    if (cause instanceof TypeError || cause instanceof FileCheckpointError) {
      throw new TargetOwnershipUnknownError('checkpoint', intent.operationId, { cause })
    }
    throw cause
  }
}
