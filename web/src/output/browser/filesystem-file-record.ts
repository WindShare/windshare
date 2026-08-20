import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import { FILE_CHECKPOINT_ID_BYTES, identityBytes } from '../persistence/checkpoint'
import type { PersistentHandleRecord } from '../persistence/journal'
import type { OpenedFileRevision } from '../persistent-tree/contracts'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { errorNamed, fileSystemHandle } from './filesystem-failure-facts'

export const FSA_FILE_HANDLE_KIND = 1
const FSA_FILE_HANDLE_DOMAIN = 'windshare/fsa-file-handle/v1'

export type FSAFileIdentityStage =
  | 'parent-authority'
  | 'namespace-create'
  | 'writer-open'
  | 'checkpoint'
  | 'commit'

export function fileHandleRecord(
  reservation: NamedContainerEntryReservation,
  ownedObjectId: string,
  handle: FileSystemFileHandle,
): PersistentHandleRecord {
  return Object.freeze({
    id: fileHandleId(reservation.operationId, ownedObjectId),
    operationId: reservation.operationId,
    kind: FSA_FILE_HANDLE_KIND,
    authorityRef: reservation.authorityRef,
    ownedObjectId,
    handle,
  })
}

export function fileHandleId(operationId: string, ownedObjectId: string): string {
  return `${FSA_FILE_HANDLE_DOMAIN}/${operationId}/${ownedObjectId}`
}

export function recordOwnsFile(
  record: PersistentHandleRecord,
  reservation: NamedContainerEntryReservation,
  ownedObjectId: string,
): boolean {
  return record.operationId === reservation.operationId &&
    record.kind === FSA_FILE_HANDLE_KIND &&
    record.authorityRef === reservation.authorityRef &&
    record.ownedObjectId === ownedObjectId
}

export async function sameFileHandleRecord(
  actual: PersistentHandleRecord | undefined,
  expected: PersistentHandleRecord,
  stageScope?: PersistentOutputStageScope,
): Promise<boolean> {
  const actualHandle = actual === undefined
    ? undefined
    : requireFileHandle(actual.handle, expected.operationId, 'namespace-create')
  const expectedHandle = requireFileHandle(
    expected.handle,
    expected.operationId,
    'namespace-create',
  )
  return actual !== undefined && actual.id === expected.id &&
    actual.operationId === expected.operationId && actual.kind === expected.kind &&
    actual.authorityRef === expected.authorityRef &&
    actual.ownedObjectId === expected.ownedObjectId && actualHandle !== undefined &&
    await runPersistentOutputStage(
      stageScope,
      'fsa.file.handle.verify',
      () => actualHandle.isSameEntry(expectedHandle),
    )
}

export function requireFileHandle(
  value: unknown,
  operationId: string,
  stage: FSAFileIdentityStage,
): FileSystemFileHandle {
  try {
    const handle = fileSystemHandle(value)
    if (handle.kind !== 'file') throw new TypeError('Persisted FSA handle is not a file')
    return handle as FileSystemFileHandle
  } catch (cause) {
    if (cause instanceof TargetOwnershipUnknownError) throw cause
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}

export function requireOpenedRevision(revision: OpenedFileRevision): void {
  identityBytes(revision.fileId, 16, 'file ID')
  identityBytes(revision.fileRevision, 16, 'file revision')
  if (typeof revision.exactSize !== 'bigint' || revision.exactSize < 0n ||
      revision.exactSize > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('Opened file revision has an invalid exact size')
  }
}

export function requireOwnedObjectId(value: string): string {
  return encodeBase64Url(identityBytes(value, FILE_CHECKPOINT_ID_BYTES, 'owned object ID'))
}

export function createOwnedObjectId(): string {
  const value = new Uint8Array(FILE_CHECKPOINT_ID_BYTES)
  crypto.getRandomValues(value)
  return requireOwnedObjectId(encodeBase64Url(value))
}

export async function namespaceEntryExists(
  parent: FileSystemDirectoryHandle,
  name: string,
): Promise<boolean> {
  try {
    await parent.getFileHandle(name)
    return true
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError') && !errorNamed(error, 'TypeMismatchError')) throw error
    if (errorNamed(error, 'TypeMismatchError')) return true
  }
  try {
    await parent.getDirectoryHandle(name)
    return true
  } catch (error) {
    if (errorNamed(error, 'NotFoundError')) return false
    if (errorNamed(error, 'TypeMismatchError')) return true
    throw error
  }
}

export async function sameEntry(
  left: FileSystemHandle,
  right: FileSystemHandle,
  operationId: string,
  stage: FSAFileIdentityStage,
): Promise<boolean> {
  try {
    return await left.isSameEntry(right)
  } catch (cause) {
    throw new TargetOwnershipUnknownError(stage, operationId, { cause })
  }
}
