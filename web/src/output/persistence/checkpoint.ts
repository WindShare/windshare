// This facade prevents persistence callers from depending on codec and ownership internals.
export * from './checkpoint-lifecycle'

export {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MAX_BACKEND_BYTES,
  FILE_CHECKPOINT_MAX_FILE_SIZE,
  FILE_CHECKPOINT_MAX_PATH_BYTES,
  FILE_CHECKPOINT_MAX_RANGES,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_V1_SCHEMA_VERSION,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  canonicalFileCheckpointBackend,
  canonicalizeFileCheckpointRanges,
  checkpointIdentityEqual,
  identityBytes,
} from './checkpoint-model'
export type {
  CheckpointIdentity,
  CheckpointRange,
  FileCheckpointRange,
  FileCheckpointSpec,
  FileCheckpointV1,
} from './checkpoint-model'

export {
  FILE_CHECKPOINT_CHECKSUM_DOMAIN,
  FILE_CHECKPOINT_MAGIC,
  FILE_CHECKPOINT_RECORD_DOMAIN,
  canonicalFileCheckpointBytes,
  decodeFileCheckpointV1,
  deriveCheckpointIdentity,
  encodeFileCheckpointV1,
  newFileCheckpointV1,
  selectVerifiedCheckpoint,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
} from './checkpoint-codec'

export {
  FILE_CHECKPOINT_AUTHORITY_CREATED_ROOT,
  FILE_CHECKPOINT_CALLER_PROVIDED_CONTAINER,
  FILE_CHECKPOINT_CERTIFICATION_LINUX_EXT4_PROCESS_RESTART,
  FILE_CHECKPOINT_CERTIFICATION_WINDOWS_NTFS_PROCESS_RESTART,
  FILE_CHECKPOINT_MAX_CERTIFICATION_BYTES,
  FILE_CHECKPOINT_OWNERSHIP_DOMAIN,
  canonicalFileCheckpointOwnershipBytes,
  decodeFileCheckpointOwnership,
  encodeFileCheckpointOwnership,
  newFileCheckpointOwnership,
} from './checkpoint-ownership'
export type {
  FileCheckpointCertification,
  FileCheckpointOwnership,
  FileCheckpointOwnershipSpec,
  FileCheckpointRootOpenDisposition,
} from './checkpoint-ownership'
