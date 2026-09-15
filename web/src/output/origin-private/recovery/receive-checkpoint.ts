import type { ReceiveIntent } from '../../../transfer/intent'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_PHASE_ACTIVE, FILE_CHECKPOINT_PHASE_PAUSED,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE, FileCheckpointError,
  fileCheckpointIsComplete, validateFileCheckpoint, type FileCheckpointV2,
} from '../../persistence/checkpoint'
import type { FileCheckpointJournal } from '../../persistence/journal'
import { durableCheckpointNamespaceIdentity, sameDurableCheckpointNamespace } from '../../persistence/namespace'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import { canonicalDigest, canonicalFrame, canonicalRecord, canonicalText } from '../../workspace/canonical'

const ORIGINAL_CHECKPOINT_AUTHORITY_BOUND = 2
const RECEIVE_CHECKPOINT_SET_DOMAIN = 'windshare/original-receive-checkpoint-set'
const RECEIVE_CHECKPOINT_SET_VERSION = 1

export interface OriginalReceiveCheckpointSummary {
  readonly checkpointSetDigest: string
  readonly completedFileCount: bigint
  readonly completedBytes: bigint
  readonly retainedBytes: bigint
}

export function originalCheckpointBinding(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file') {
    throw new TypeError('Original checkpoint recovery requires its workspace intent')
  }
  return durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
}

/** Pause and reload share the same bounded metadata authority, including incomplete files. */
export async function readOriginalReceiveCheckpoint(
  intent: ReceiveIntent,
  expectedSize: bigint,
  checkpoints: Pick<FileCheckpointJournal, 'scanCommitted'>,
): Promise<FileCheckpointV2 | undefined> {
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
    if (intent.artifact.kind !== 'original-file' || !sameDurableCheckpointNamespace(record, binding) ||
        record.fileId !== intent.artifact.fileId || record.exactSize !== expectedSize ||
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

export async function originalReceiveCheckpointSummary(
  intent: ReceiveIntent,
  checkpoint: FileCheckpointV2 | undefined,
): Promise<OriginalReceiveCheckpointSummary> {
  const checkpointSetDigest = await canonicalDigest(canonicalRecord(
    RECEIVE_CHECKPOINT_SET_DOMAIN, RECEIVE_CHECKPOINT_SET_VERSION, [
      canonicalFrame(canonicalText(intent.digest)),
      canonicalFrame(canonicalText(checkpoint?.checksum ?? '')),
    ],
  ))
  const complete = checkpoint !== undefined && fileCheckpointIsComplete(checkpoint)
  return Object.freeze({
    checkpointSetDigest,
    completedFileCount: complete ? 1n : 0n,
    completedBytes: complete ? checkpoint.exactSize : 0n,
    retainedBytes: checkpoint?.verifiedRanges.reduce((sum, range) => sum + range.end - range.start, 0n) ?? 0n,
  })
}
