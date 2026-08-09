import { encodeBase64Url } from '../../crypto/bytes'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_V2_SCHEMA_VERSION,
  OPERATION_ID_BYTES,
  checkpointMaterializerKind,
  identityBytes,
  type CheckpointIdentity,
  type FileCheckpointMaterializerKind,
} from './checkpoint-model'

export interface CertifiedFileCheckpointOwnership {
  readonly schemaVersion: typeof FILE_CHECKPOINT_V2_SCHEMA_VERSION
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly operationId: string
  readonly materializerKind: FileCheckpointMaterializerKind
  readonly authorityRef: string
  readonly repositoryRef: string
}

export function certifiedFileCheckpointOwnership(input: {
  readonly operationId: CheckpointIdentity
  readonly materializerKind: FileCheckpointMaterializerKind | number
  readonly authorityRef: CheckpointIdentity
  readonly repositoryRef: CheckpointIdentity
}): CertifiedFileCheckpointOwnership {
  return Object.freeze({
    schemaVersion: FILE_CHECKPOINT_V2_SCHEMA_VERSION,
    marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
    namespace: FILE_CHECKPOINT_NAMESPACE,
    operationId: encodeBase64Url(identityBytes(input.operationId, OPERATION_ID_BYTES, 'operation ID')),
    materializerKind: checkpointMaterializerKind(input.materializerKind),
    authorityRef: encodeBase64Url(identityBytes(
      input.authorityRef,
      FILE_CHECKPOINT_ID_BYTES,
      'authority reference',
    )),
    repositoryRef: encodeBase64Url(identityBytes(
      input.repositoryRef,
      FILE_CHECKPOINT_ID_BYTES,
      'repository reference',
    )),
  })
}

export function sameFileCheckpointOwnership(
  left: CertifiedFileCheckpointOwnership,
  right: CertifiedFileCheckpointOwnership,
): boolean {
  return left.schemaVersion === right.schemaVersion &&
    left.marker === right.marker && left.namespace === right.namespace &&
    left.operationId === right.operationId &&
    left.materializerKind === right.materializerKind &&
    left.authorityRef === right.authorityRef && left.repositoryRef === right.repositoryRef
}
