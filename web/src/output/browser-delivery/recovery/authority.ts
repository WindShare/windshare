import { fileCheckpointIsComplete, type FileCheckpointV2 } from '../../persistence/checkpoint'
import { sameDurableCheckpointNamespace } from '../../persistence/namespace'
import { createFSARecoveryCheckpointSnapshot, deriveFSARecoverySummary } from '../../file-system-access/recovery-summary'
import type { DirectTreeIntent } from '../../file-system-access/settlement-proof'
import { nextReceiveLifecycleState, type ReceiveLifecycleState } from '../../workspace/state'
import { validateBrowserDeliveryRecord } from '../records'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from '../model'

export type LocalFileSetLifecycle = Extract<ReceiveLifecycleState, { kind: 'resumable-receive'; payloadKind: 'file-set' }>

/** The old digest must still be reconstructible from exact pre-mutation cuts; unrelated changes never become authorized. */
export async function deriveBrowserDeliveryLifecycle(input: {
  intent: DirectTreeIntent
  lifecycle: LocalFileSetLifecycle
  policy: BrowserSavePolicyV1
  records: readonly BrowserDeliveryRecordV1[]
  checkpoints: readonly FileCheckpointV2[]
}): Promise<LocalFileSetLifecycle> {
  const { intent, lifecycle, policy, checkpoints } = input
  if (policy.operationId !== intent.operationId || policy.receiveIntentDigest !== intent.digest) {
    throw new TypeError('Local delivery policy belongs to another lifecycle intent')
  }
  const snapshot = await createFSARecoveryCheckpointSnapshot(intent, lifecycle.generation, checkpoints)
  if (snapshot.checkpointSetDigest === lifecycle.checkpointSetDigest) {
    await deriveFSARecoverySummary({ intent, lifecycle, snapshot })
    return lifecycle
  }
  const baseline = new Map(checkpoints.map(checkpoint => [checkpoint.fileId, checkpoint]))
  if (baseline.size !== checkpoints.length) throw new TypeError('Local delivery repeats target file authority')
  let authorized = 0
  for (const inputRecord of input.records) {
    const record = validateBrowserDeliveryRecord(policy, inputRecord)
    const mutation = record.localMutation
    if (mutation === undefined || mutation.lifecycleGeneration !== lifecycle.generation) continue
    if (mutation.checkpointSetDigest !== lifecycle.checkpointSetDigest) {
      throw new TypeError('Local delivery baseline belongs to another checkpoint set')
    }
    const current = baseline.get(record.fileId)
    validateCurrentTarget(policy, record, current)
    if (mutation.priorTargetCheckpoint === undefined) baseline.delete(record.fileId)
    else baseline.set(record.fileId, mutation.priorTargetCheckpoint)
    authorized += 1
  }
  if (authorized === 0) throw new TypeError('Changed target checkpoints lack durable local-delivery authorization')
  const oldSnapshot = await createFSARecoveryCheckpointSnapshot(intent, lifecycle.generation, [...baseline.values()])
  await deriveFSARecoverySummary({ intent, lifecycle, snapshot: oldSnapshot })
  const completed = checkpoints.filter(fileCheckpointIsComplete)
  const next = nextReceiveLifecycleState(lifecycle, {
    ...lifecycle, checkpointSetDigest: snapshot.checkpointSetDigest,
    completedFileCount: BigInt(completed.length),
    completedBytes: completed.reduce((sum, checkpoint) => sum + checkpoint.exactSize, 0n),
  }) as LocalFileSetLifecycle
  await deriveFSARecoverySummary({
    intent, lifecycle: next, snapshot: { ...snapshot, lifecycleGeneration: next.generation },
  })
  return next
}

function validateCurrentTarget(
  policy: BrowserSavePolicyV1, record: BrowserDeliveryRecordV1, current: FileCheckpointV2 | undefined,
): void {
  const prior = record.localMutation?.priorTargetCheckpoint
  if (current === undefined) {
    if (prior !== undefined) throw new TypeError('Local delivery lost its previously owned target checkpoint')
    return
  }
  if (!sameDurableCheckpointNamespace(policy.target, current) ||
      current.fileRevision !== record.source.fileRevision || current.exactSize !== record.source.exactSize ||
      JSON.stringify(current.canonicalPath) !== JSON.stringify(record.materializationRelativePath) ||
      (prior !== undefined && (current.recordId !== prior.recordId || current.ownedObjectId !== prior.ownedObjectId ||
        current.checkpointGeneration < prior.checkpointGeneration || current.stateGeneration < prior.stateGeneration))) {
    throw new TypeError('Local delivery changed target ownership or authenticated source')
  }
}
