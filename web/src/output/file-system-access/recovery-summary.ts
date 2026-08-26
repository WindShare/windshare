import {
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  decodeFileCheckpointV2,
  encodeFileCheckpointV2,
  fileCheckpointIsComplete,
  validateFileCheckpoint,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  snapshotRecoverySelectionFacts,
  type ReceiveLifecycleState,
} from '../workspace/state'
import type { FileCheckpointJournal } from '../persistence/journal'
import { scanAllFSAFileCheckpoints } from './checkpoint-repository'
import {
  fsaCheckpointSetDigest,
  type DirectTreeIntent,
} from './settlement-proof'

const MAXIMUM_RECOVERY_SUMMARY_VALUE = 0xffff_ffff_ffff_ffffn

type ResumableFileSetLifecycle = Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive'; payloadKind: 'file-set' }
>

export interface FSARecoveryCheckpointSnapshot {
  readonly lifecycleGeneration: bigint
  readonly checkpointSetDigest: string
  readonly checkpoints: readonly FileCheckpointV2[]
}

export interface RecoverySummary {
  readonly lifecycleGeneration: bigint
  readonly checkpointSetDigest: string
  readonly discoveredFileCount: bigint
  readonly discoveredBytes: bigint
  readonly discovery: 'complete' | 'known-so-far'
  readonly completedFileCount: bigint
  readonly completedBytes: bigint
  readonly incompleteFileCount: bigint
  readonly verifiedPartialFileCount: bigint
  readonly verifiedPartialBytes: bigint
  readonly unstartedFileCount: bigint
  readonly unstartedBytes: bigint
  readonly preservingRemainingBytes: bigint
  readonly restartRemainingBytes: bigint
  readonly restartRedownloadBytes: bigint
  readonly maximumPreservingTemporaryBytes: bigint
}

export async function createFSARecoveryCheckpointSnapshot(
  intent: DirectTreeIntent,
  lifecycleGeneration: bigint,
  checkpoints: readonly FileCheckpointV2[],
): Promise<FSARecoveryCheckpointSnapshot> {
  if (typeof lifecycleGeneration !== 'bigint' || lifecycleGeneration <= 0n ||
      lifecycleGeneration > MAXIMUM_RECOVERY_SUMMARY_VALUE || !Array.isArray(checkpoints)) {
    throw new TypeError('FSA recovery checkpoint snapshot coordinates are invalid')
  }
  // Re-encoding gives the snapshot its own immutable values and prevents later storage mutation
  // from changing facts after the digest has been calculated.
  const exactCheckpoints = Object.freeze(checkpoints.map(checkpoint => {
    validateFileCheckpoint(checkpoint)
    return decodeFileCheckpointV2(encodeFileCheckpointV2(checkpoint))
  }))
  return Object.freeze({
    lifecycleGeneration,
    checkpointSetDigest: await fsaCheckpointSetDigest(intent, exactCheckpoints),
    checkpoints: exactCheckpoints,
  })
}

/** Projects choice costs only after the lifecycle and exact committed snapshot authenticate each other. */
export async function deriveFSARecoverySummary(input: Readonly<{
  intent: DirectTreeIntent
  lifecycle: ResumableFileSetLifecycle
  snapshot: FSARecoveryCheckpointSnapshot
}>): Promise<RecoverySummary> {
  const { intent, lifecycle, snapshot } = input
  if (lifecycle.operationId !== intent.operationId ||
      lifecycle.receiveIntentDigest !== intent.digest ||
      snapshot.lifecycleGeneration !== lifecycle.generation) {
    throw new TypeError('FSA recovery snapshot belongs to another lifecycle generation')
  }
  const selection = snapshotRecoverySelectionFacts(
    lifecycle.selectionFacts,
    lifecycle.completedFileCount,
    lifecycle.completedBytes,
  )
  const recomputedDigest = await fsaCheckpointSetDigest(intent, snapshot.checkpoints)
  if (snapshot.checkpointSetDigest !== recomputedDigest ||
      lifecycle.checkpointSetDigest !== recomputedDigest) {
    throw new TypeError('FSA recovery checkpoint snapshot digest does not match its lifecycle')
  }

  const recordIds = new Set<string>()
  const fileIds = new Set<string>()
  const paths = new Set<string>()
  const ownedObjects = new Set<string>()
  let checkpointFileCount = 0n
  let checkpointExactBytes = 0n
  let completedFileCount = 0n
  let completedBytes = 0n
  let incompleteFileCount = 0n
  let verifiedPartialFileCount = 0n
  let verifiedPartialBytes = 0n
  let maximumPreservingTemporaryBytes = 0n

  for (const checkpoint of snapshot.checkpoints) {
    validateRecoveryCheckpoint(intent, checkpoint)
    const path = JSON.stringify(checkpoint.canonicalPath)
    if (recordIds.has(checkpoint.recordId) || fileIds.has(checkpoint.fileId) ||
        paths.has(path) || ownedObjects.has(checkpoint.ownedObjectId)) {
      throw new TypeError('FSA recovery checkpoint snapshot contains duplicate file authority')
    }
    recordIds.add(checkpoint.recordId)
    fileIds.add(checkpoint.fileId)
    paths.add(path)
    ownedObjects.add(checkpoint.ownedObjectId)
    checkpointFileCount = checkedAdd(checkpointFileCount, 1n, 'checkpoint file count')
    checkpointExactBytes = checkedAdd(
      checkpointExactBytes,
      checkpoint.exactSize,
      'checkpoint exact bytes',
    )
    if (fileCheckpointIsComplete(checkpoint)) {
      completedFileCount = checkedAdd(completedFileCount, 1n, 'completed file count')
      completedBytes = checkedAdd(completedBytes, checkpoint.exactSize, 'completed bytes')
      continue
    }
    incompleteFileCount = checkedAdd(incompleteFileCount, 1n, 'incomplete file count')
    const verifiedBytes = checkpoint.verifiedRanges.reduce(
      (total, range) => checkedAdd(total, range.end - range.start, 'verified partial bytes'),
      0n,
    )
    if (verifiedBytes > 0n) {
      verifiedPartialFileCount = checkedAdd(
        verifiedPartialFileCount,
        1n,
        'verified partial file count',
      )
    }
    verifiedPartialBytes = checkedAdd(
      verifiedPartialBytes,
      verifiedBytes,
      'verified partial bytes',
    )
    const durablePrefixBytes = checkpoint.verifiedRanges.at(-1)?.end ?? 0n
    if (durablePrefixBytes > maximumPreservingTemporaryBytes) {
      maximumPreservingTemporaryBytes = durablePrefixBytes
    }
  }

  if (completedFileCount !== lifecycle.completedFileCount ||
      completedBytes !== lifecycle.completedBytes) {
    throw new TypeError('FSA recovery checkpoint completion totals disagree with the lifecycle')
  }
  if (checkpointFileCount > selection.discoveredFileCount ||
      checkpointExactBytes > selection.discoveredBytes) {
    throw new TypeError('FSA recovery checkpoints exceed the discovered selection')
  }
  const preservingRemainingBytes = checkedSubtract(
    checkedSubtract(selection.discoveredBytes, completedBytes, 'preserving remaining bytes'),
    verifiedPartialBytes,
    'preserving remaining bytes',
  )
  const restartRemainingBytes = checkedSubtract(
    selection.discoveredBytes,
    completedBytes,
    'restart remaining bytes',
  )

  return Object.freeze({
    lifecycleGeneration: lifecycle.generation,
    checkpointSetDigest: lifecycle.checkpointSetDigest,
    discoveredFileCount: selection.discoveredFileCount,
    discoveredBytes: selection.discoveredBytes,
    discovery: selection.discovery === 'complete' ? 'complete' : 'known-so-far',
    completedFileCount,
    completedBytes,
    incompleteFileCount,
    verifiedPartialFileCount,
    verifiedPartialBytes,
    unstartedFileCount: selection.discoveredFileCount - checkpointFileCount,
    unstartedBytes: selection.discoveredBytes - checkpointExactBytes,
    preservingRemainingBytes,
    restartRemainingBytes,
    restartRedownloadBytes: verifiedPartialBytes,
    maximumPreservingTemporaryBytes,
  })
}

export async function readFSARecoverySummary(input: Readonly<{
  intent: DirectTreeIntent
  lifecycle: ResumableFileSetLifecycle
  checkpoints: FileCheckpointJournal
}>): Promise<RecoverySummary> {
  const committed = await scanAllFSAFileCheckpoints(input.checkpoints, 'committed')
  const snapshot = await createFSARecoveryCheckpointSnapshot(
    input.intent,
    input.lifecycle.generation,
    committed,
  )
  return deriveFSARecoverySummary({
    intent: input.intent,
    lifecycle: input.lifecycle,
    snapshot,
  })
}

function validateRecoveryCheckpoint(intent: DirectTreeIntent, checkpoint: FileCheckpointV2): void {
  validateFileCheckpoint(checkpoint)
  if ((checkpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED &&
       checkpoint.commitState !== FILE_CHECKPOINT_COMMIT_PUBLISHED) ||
      checkpoint.materializerKind !== FILE_CHECKPOINT_MATERIALIZER_FSA_TREE ||
      checkpoint.operationId !== intent.operationId ||
      checkpoint.receiveIntentDigest !== intent.digest ||
      checkpoint.materializationBindingDigest !== intent.plan.reservation.digest ||
      checkpoint.authorityRef !== intent.plan.reservation.authorityRef) {
    throw new TypeError('FSA recovery checkpoint does not belong to the committed receive snapshot')
  }
}

function checkedAdd(left: bigint, right: bigint, label: string): bigint {
  const result = left + right
  if (left < 0n || right < 0n || result > MAXIMUM_RECOVERY_SUMMARY_VALUE) {
    throw new TypeError(`${label} exceeds the recovery summary integer domain`)
  }
  return result
}

function checkedSubtract(left: bigint, right: bigint, label: string): bigint {
  if (left < 0n || right < 0n || right > left) {
    throw new TypeError(`${label} underflows the recovery summary integer domain`)
  }
  return left - right
}
