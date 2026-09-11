import {
  createSelectionSpec, createSingleFileDirectoryTreeArtifact, createFSANamedEntryReservation,
  createReceiveIntent, createDirectTreePlan,
} from '../../src/transfer/intent'
import { createBrowserSavePolicy, createBrowserDeliveryRecord, browserDeliveryStagingPath } from '../../src/output/browser-delivery'
import { requireDirectTreeIntent } from '../../src/output/file-system-access/settlement-proof'
import { createFSARecoveryCheckpointSnapshot } from '../../src/output/file-system-access/recovery-summary'
import { newFileCheckpointV2, FILE_CHECKPOINT_PHASE_PAUSED, FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_MATERIALIZER_FSA_TREE } from '../../src/output/persistence/checkpoint'
import type { LocalFileSetLifecycle } from '../../src/output/browser-delivery/recovery/authority'
import { deliveryIdentity as identity, deliveryPolicy } from './browser-delivery-fixture'

export async function localDeliveryFixture() {
  const fileId = identity(3, 16)
  const artifact = await createSingleFileDirectoryTreeArtifact({ fileId, sourcePath: 'report.bin', outputName: 'report.bin' })
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(4, 16), reservationId: identity(5, 16), artifact, authorityRef: identity(6),
    logicalReservedName: 'report.bin', physicalName: 'report.bin', collisionIndex: 0,
  })
  const intent = await requireDirectTreeIntent(await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(1, 16), syntheticRoot: identity(2, 16),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact, plan: await createDirectTreePlan(artifact, reservation),
  }))
  const policy = createBrowserSavePolicy({
    operationId: intent.operationId, receiveIntentDigest: intent.digest, preference: 'automatic',
    target: {
      operationId: intent.operationId, receiveIntentDigest: intent.digest,
      materializationBindingDigest: reservation.digest, authorityRef: reservation.authorityRef,
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    },
    staging: deliveryPolicy().staging!,
  })
  const source = { fileId, fileRevision: identity(7, 16), canonicalPath: ['report.bin'], exactSize: 8n }
  const initial = createBrowserDeliveryRecord({
    policy, source, materializationRelativePath: [], placement: 'staged', placementReason: 'local-recovery',
  })
  const target = (end = 0n, generation = 1n) => newFileCheckpointV2({
    ...policy.target, ...source, canonicalPath: [], ownedObjectId: identity(8),
    checkpointGeneration: generation, stateGeneration: generation,
    verifiedRanges: end === 0n ? [] : [{ start: 0n, end }],
    phase: end === source.exactSize ? FILE_CHECKPOINT_PHASE_ACTIVE : FILE_CHECKPOINT_PHASE_PAUSED,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const stage = newFileCheckpointV2({
    ...policy.staging!, ...source, canonicalPath: browserDeliveryStagingPath(fileId), ownedObjectId: identity(9),
    checkpointGeneration: 1n, stateGeneration: 1n, verifiedRanges: [{ start: 0n, end: source.exactSize }],
    phase: FILE_CHECKPOINT_PHASE_PAUSED, commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const baseline = target()
  const snapshot = await createFSARecoveryCheckpointSnapshot(intent, 7n, [baseline])
  const lifecycle: LocalFileSetLifecycle = {
    kind: 'resumable-receive', payloadKind: 'file-set', operationId: intent.operationId,
    receiveIntentDigest: intent.digest, generation: 7n, checkpointSetDigest: snapshot.checkpointSetDigest,
    completedFileCount: 0n, completedBytes: 0n,
    selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'complete' },
  }
  return { intent, policy, source, initial, target, stage, baseline, lifecycle }
}
