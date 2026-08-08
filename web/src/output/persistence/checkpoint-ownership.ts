import { encodeBase64Url } from '../../crypto/bytes'
import {
  CheckpointCursor,
  checkpointChecksumPayload,
  checkpointConcatBytes,
  checkpointEqualBytes,
  checkpointFramedText,
} from './checkpoint-codec'
import { FileCheckpointError } from './checkpoint-lifecycle'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MAX_BACKEND_BYTES,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBackend,
  identityBytes,
  type CheckpointIdentity,
} from './checkpoint-model'

export const FILE_CHECKPOINT_OWNERSHIP_DOMAIN = 'windshare/file-checkpoint-ownership/v1' as const
export const FILE_CHECKPOINT_CERTIFICATION_LINUX_EXT4_PROCESS_RESTART =
  'linux/ext4/process-restart/v2' as const
export const FILE_CHECKPOINT_CERTIFICATION_WINDOWS_NTFS_PROCESS_RESTART =
  'windows/ntfs/process-restart/v1' as const
export const FILE_CHECKPOINT_CALLER_PROVIDED_CONTAINER = 'caller-provided-container' as const
export const FILE_CHECKPOINT_AUTHORITY_CREATED_ROOT = 'authority-created-root' as const
export const FILE_CHECKPOINT_MAX_CERTIFICATION_BYTES = 128

const OWNERSHIP_CHECKSUM_BYTES = 32
const OWNERSHIP_MARKER_MAX_BYTES = 128
const OWNERSHIP_NAMESPACE_MAX_BYTES = 256
const ROOT_DISPOSITION_MAX_BYTES = 128

export type FileCheckpointCertification =
  | typeof FILE_CHECKPOINT_CERTIFICATION_LINUX_EXT4_PROCESS_RESTART
  | typeof FILE_CHECKPOINT_CERTIFICATION_WINDOWS_NTFS_PROCESS_RESTART

export type FileCheckpointRootOpenDisposition =
  | typeof FILE_CHECKPOINT_CALLER_PROVIDED_CONTAINER
  | typeof FILE_CHECKPOINT_AUTHORITY_CREATED_ROOT

export interface FileCheckpointOwnership {
  readonly marker: typeof FILE_CHECKPOINT_OWNERSHIP_MARKER
  readonly namespace: typeof FILE_CHECKPOINT_NAMESPACE
  readonly backend: string
  readonly certification: FileCheckpointCertification
  readonly rootIdentity: string
  readonly rootOpenDisposition: FileCheckpointRootOpenDisposition
}

export interface FileCheckpointOwnershipSpec {
  readonly backend: string
  readonly certification: FileCheckpointCertification | string
  readonly rootIdentity: CheckpointIdentity
  readonly rootOpenDisposition: FileCheckpointRootOpenDisposition | string
}

export function newFileCheckpointOwnership(
  spec: FileCheckpointOwnershipSpec,
): FileCheckpointOwnership {
  try {
    const backend = canonicalFileCheckpointBackend(spec.backend)
    const certification = canonicalCheckpointCertification(spec.certification)
    const rootIdentity = encodeBase64Url(identityBytes(
      spec.rootIdentity,
      FILE_CHECKPOINT_ID_BYTES,
      'root identity',
    ))
    const rootOpenDisposition = canonicalRootOpenDisposition(spec.rootOpenDisposition)
    return Object.freeze({
      marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
      namespace: FILE_CHECKPOINT_NAMESPACE,
      backend,
      certification,
      rootIdentity,
      rootOpenDisposition,
    })
  } catch (error) {
    if (error instanceof FileCheckpointError && error.code === 'ownership') throw error
    throw new FileCheckpointError('ownership', 'checkpoint ownership is invalid')
  }
}

export function canonicalFileCheckpointOwnershipBytes(
  ownership: FileCheckpointOwnership,
): Uint8Array<ArrayBuffer> {
  if (ownership.marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
      ownership.namespace !== FILE_CHECKPOINT_NAMESPACE) {
    throw new FileCheckpointError('ownership', 'checkpoint ownership marker is invalid')
  }
  const canonical = newFileCheckpointOwnership(ownership)
  return checkpointConcatBytes([
    checkpointFramedText(FILE_CHECKPOINT_OWNERSHIP_DOMAIN),
    checkpointFramedText(canonical.marker),
    checkpointFramedText(canonical.namespace),
    checkpointFramedText(canonical.backend),
    checkpointFramedText(canonical.certification),
    identityBytes(canonical.rootIdentity, FILE_CHECKPOINT_ID_BYTES, 'root identity'),
    checkpointFramedText(canonical.rootOpenDisposition),
  ])
}

export function encodeFileCheckpointOwnership(
  ownership: FileCheckpointOwnership,
): Uint8Array<ArrayBuffer> {
  const payload = canonicalFileCheckpointOwnershipBytes(ownership)
  return checkpointConcatBytes([payload, checkpointChecksumPayload(payload)])
}

export function decodeFileCheckpointOwnership(encoded: Uint8Array): FileCheckpointOwnership {
  if (encoded.byteLength < OWNERSHIP_CHECKSUM_BYTES + 1) {
    throw new FileCheckpointError('ownership', 'checkpoint ownership marker is truncated')
  }
  const payload = encoded.subarray(0, encoded.byteLength - OWNERSHIP_CHECKSUM_BYTES)
  const supplied = encoded.subarray(encoded.byteLength - OWNERSHIP_CHECKSUM_BYTES)
  const expected = checkpointChecksumPayload(payload)
  if (!checkpointEqualBytes(supplied, expected)) {
    throw new FileCheckpointError('checksum', 'checkpoint ownership checksum is invalid')
  }
  try {
    const cursor = new CheckpointCursor(payload)
    if (cursor.text(FILE_CHECKPOINT_OWNERSHIP_DOMAIN.length) !== FILE_CHECKPOINT_OWNERSHIP_DOMAIN) {
      throw new FileCheckpointError('ownership', 'checkpoint ownership domain is invalid')
    }
    const marker = cursor.text(OWNERSHIP_MARKER_MAX_BYTES)
    const namespace = cursor.text(OWNERSHIP_NAMESPACE_MAX_BYTES)
    const backend = canonicalFileCheckpointBackend(cursor.text(FILE_CHECKPOINT_MAX_BACKEND_BYTES))
    const certification = canonicalCheckpointCertification(
      cursor.text(FILE_CHECKPOINT_MAX_CERTIFICATION_BYTES),
    )
    const root = cursor.fixed(FILE_CHECKPOINT_ID_BYTES, 'root identity')
    const rootOpenDisposition = canonicalRootOpenDisposition(
      cursor.text(ROOT_DISPOSITION_MAX_BYTES),
    )
    if (!cursor.done() || marker !== FILE_CHECKPOINT_OWNERSHIP_MARKER ||
        namespace !== FILE_CHECKPOINT_NAMESPACE) {
      throw new FileCheckpointError('ownership', 'checkpoint ownership marker is invalid')
    }
    const ownership = newFileCheckpointOwnership({
      backend,
      certification,
      rootIdentity: root,
      rootOpenDisposition,
    })
    if (!checkpointEqualBytes(encodeFileCheckpointOwnership(ownership), encoded)) {
      throw new FileCheckpointError(
        'non-canonical',
        'checkpoint ownership encoding is not canonical',
      )
    }
    return ownership
  } catch (error) {
    if (error instanceof FileCheckpointError &&
        (error.code === 'ownership' || error.code === 'non-canonical')) {
      throw error
    }
    throw new FileCheckpointError('ownership', 'checkpoint ownership payload is invalid')
  }
}

function canonicalCheckpointCertification(value: string): FileCheckpointCertification {
  if (value === FILE_CHECKPOINT_CERTIFICATION_LINUX_EXT4_PROCESS_RESTART ||
      value === FILE_CHECKPOINT_CERTIFICATION_WINDOWS_NTFS_PROCESS_RESTART) {
    return value
  }
  throw new FileCheckpointError('ownership', 'checkpoint ownership certification is invalid')
}

function canonicalRootOpenDisposition(value: string): FileCheckpointRootOpenDisposition {
  if (value === FILE_CHECKPOINT_CALLER_PROVIDED_CONTAINER ||
      value === FILE_CHECKPOINT_AUTHORITY_CREATED_ROOT) {
    return value
  }
  throw new FileCheckpointError('ownership', 'checkpoint root-open disposition is invalid')
}
