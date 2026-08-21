import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import type { NamedContainerEntryReservation } from '../../transfer/intent'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import { fsaOwnedDirectoryHandleId } from './indexeddb-root-binding'

const FSA_DIRECTORY_LOCATOR_DOMAIN = 'windshare/fsa-directory-locator/v1'

export async function fsaDirectoryHandleId(
  reservation: NamedContainerEntryReservation,
  path: readonly string[],
): Promise<string> {
  const encodedPath = path.map(segment => `${segment.length}:${segment}`).join('/')
  const material = new TextEncoder().encode(
    `${FSA_DIRECTORY_LOCATOR_DOMAIN}\0${reservation.digest}\0${encodedPath}`,
  )
  const digest = encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', material)))
  return fsaOwnedDirectoryHandleId(reservation.operationId, digest)
}

export function snapshotRelativePath(path: readonly string[], allowEmpty: boolean): readonly string[] {
  if (allowEmpty && path.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(path)
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
