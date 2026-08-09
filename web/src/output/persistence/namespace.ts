import { encodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_ID_BYTES,
  OPERATION_ID_BYTES,
  checkpointMaterializerKind,
  identityBytes,
  type CheckpointIdentity,
  type FileCheckpointMaterializerKind,
} from './checkpoint'

export interface DurableCheckpointNamespaceIdentity {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly materializerKind: FileCheckpointMaterializerKind
  readonly authorityRef: string
}

export function durableCheckpointNamespaceIdentity(input: {
  readonly operationId: CheckpointIdentity
  readonly receiveIntentDigest: CheckpointIdentity
  readonly materializationBindingDigest: CheckpointIdentity
  readonly materializerKind: FileCheckpointMaterializerKind | number
  readonly authorityRef: CheckpointIdentity
}): DurableCheckpointNamespaceIdentity {
  return Object.freeze({
    operationId: encodeBase64Url(identityBytes(input.operationId, OPERATION_ID_BYTES, 'operation ID')),
    receiveIntentDigest: encodeBase64Url(identityBytes(
      input.receiveIntentDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    )),
    materializationBindingDigest: encodeBase64Url(identityBytes(
      input.materializationBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    )),
    materializerKind: checkpointMaterializerKind(input.materializerKind),
    authorityRef: encodeBase64Url(identityBytes(
      input.authorityRef,
      FILE_CHECKPOINT_ID_BYTES,
      'authority reference',
    )),
  })
}

export function durableCheckpointNamespaceKey(
  identity: DurableCheckpointNamespaceIdentity,
): string {
  const canonical = durableCheckpointNamespaceIdentity(identity)
  return `windshare/file-checkpoint/v2/${canonical.operationId}`
}

export function sameDurableCheckpointNamespace(
  left: DurableCheckpointNamespaceIdentity,
  right: DurableCheckpointNamespaceIdentity,
): boolean {
  return left.operationId === right.operationId &&
    left.receiveIntentDigest === right.receiveIntentDigest &&
    left.materializationBindingDigest === right.materializationBindingDigest &&
    left.materializerKind === right.materializerKind && left.authorityRef === right.authorityRef
}
