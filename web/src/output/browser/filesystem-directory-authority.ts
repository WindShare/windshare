import { encodeBase64Url } from '../../crypto/bytes'
import type { FSANamedContainerEntryReservation } from '../../transfer/intent'
import {
  snapshotMaterializationRootRelativePath,
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { fsaOwnedDirectoryHandleId } from './indexeddb-root-binding'

export const FSA_DIRECTORY_LOCATOR_DOMAIN = 'windshare/fsa-directory-locator/v2'

export async function fsaDirectoryHandleId(
  reservation: FSANamedContainerEntryReservation,
  path: readonly string[],
): Promise<string> {
  const relativePath = snapshotMaterializationRootRelativePath(path)
  const encodedPath = relativePath.map(segment => `${segment.length}:${segment}`).join('/')
  const material = new TextEncoder().encode(
    `${FSA_DIRECTORY_LOCATOR_DOMAIN}\0${reservation.digest}\0${encodedPath}`,
  )
  const digest = encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', material)))
  return fsaOwnedDirectoryHandleId(reservation.operationId, digest)
}

export function snapshotRelativePath(
  path: readonly string[],
  allowEmpty: boolean,
): MaterializationRootRelativePath {
  if (!allowEmpty && path.length === 0) {
    throw new TypeError('non-root FSA path is empty')
  }
  return snapshotMaterializationRootRelativePath(path)
}

export async function openDirectoryEntry(
  parent: FileSystemDirectoryHandle,
  name: string,
  operationId: string,
  stageScope: PersistentOutputStageScope | undefined,
  diagnosticStage: Extract<
    PersistentOutputStage,
    'fsa.root.entry.open' | 'fsa.directory.entry.open'
  >,
): Promise<FileSystemDirectoryHandle> {
  try {
    return await runPersistentOutputStage(
      stageScope,
      diagnosticStage,
      () => parent.getDirectoryHandle(name),
    )
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
}

export async function verifySameDirectory(
  left: FileSystemDirectoryHandle,
  right: FileSystemDirectoryHandle,
  operationId: string,
  stageScope: PersistentOutputStageScope | undefined,
  diagnosticStage: Extract<
    PersistentOutputStage,
    'fsa.root.handle.verify' | 'fsa.directory.handle.verify'
  >,
): Promise<boolean> {
  try {
    return await runPersistentOutputStage(
      stageScope,
      diagnosticStage,
      () => left.isSameEntry(right),
    )
  } catch (cause) {
    throw new TargetOwnershipUnknownError('parent-authority', operationId, { cause })
  }
}
