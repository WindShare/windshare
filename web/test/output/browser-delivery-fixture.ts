import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createBrowserSavePolicy, createBrowserDeliveryRecord, advanceBrowserDeliveryRecord,
  type BrowserSavePolicyV1,
} from '../../src/output/browser-delivery'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE, FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2, type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'

export function deliveryIdentity(byte: number, width = 32): string {
  return encodeBase64Url(new Uint8Array(width).fill(byte))
}

export function deliveryPolicy(preference: 'automatic' | 'direct' = 'automatic'): BrowserSavePolicyV1 {
  const target = {
    operationId: deliveryIdentity(1, 16), receiveIntentDigest: deliveryIdentity(2),
    materializationBindingDigest: deliveryIdentity(3), materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: deliveryIdentity(4),
  }
  return createBrowserSavePolicy({
    operationId: target.operationId, receiveIntentDigest: target.receiveIntentDigest, target, preference,
    ...(preference === 'direct' ? {} : {
      staging: {
        operationId: deliveryIdentity(5, 16), receiveIntentDigest: deliveryIdentity(6),
        materializationBindingDigest: deliveryIdentity(7), materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
        authorityRef: deliveryIdentity(8),
      },
    }),
  })
}

export function deliveryFixture(preference: 'automatic' | 'direct' = 'automatic') {
  const policy = deliveryPolicy(preference)
  const source = {
    fileId: deliveryIdentity(9, 16), fileRevision: deliveryIdentity(10, 16),
    canonicalPath: ['nested', 'data.bin'], exactSize: 8n,
  }
  const initial = createBrowserDeliveryRecord({
    policy, source, materializationRelativePath: source.canonicalPath, placement: preference === 'direct' ? 'direct' : 'staged', placementReason: 'fixture',
  })
  const checkpoint = (placement: 'direct' | 'staged', end = source.exactSize): FileCheckpointV2 =>
    newFileCheckpointV2({
      ...(placement === 'direct' ? policy.target : policy.staging!),
      ...source, canonicalPath: placement === 'staged' ? [source.fileId] : source.canonicalPath, ownedObjectId: deliveryIdentity(placement === 'direct' ? 11 : 12),
      stateGeneration: 1n, checkpointGeneration: 1n,
      verifiedRanges: end === 0n ? [] : [{ start: 0n, end }],
      phase: FILE_CHECKPOINT_PHASE_ACTIVE, commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
  const target = checkpoint('direct')
  const stage = preference === 'direct' ? undefined : checkpoint('staged')
  const advance = (previous: typeof initial, state: typeof initial.state) =>
    advanceBrowserDeliveryRecord(policy, previous, state)
  return { policy, source, initial, target, stage, checkpoint, advance }
}
