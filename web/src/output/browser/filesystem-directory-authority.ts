import { encodeBase64Url } from '../../crypto/bytes'
import type { FSANamedContainerEntryReservation } from '../../transfer/intent'
import { OutputDirectoryMutationError } from '../../transfer/output-session'
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
import {
  persistFSAOwnedDirectory,
  readFSAOwnedDirectoryBinding,
  type FSAOperationBindingRepository,
} from './indexeddb-root-binding'
import {
  PathComponentRejectedError,
  inspectFileSystemComponent,
} from './filesystem-component-inspection'
import { requireOwnedObjectId } from './filesystem-file-record'
import type {
  FSAAuthorityCache,
  FSAVerifiedDirectoryAuthority,
} from './mutation-coordination/authority-cache'
import type { FSAOperationMutationScheduler } from './mutation-coordination/model'

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

export type FSADirectoryAdmissionOutcome =
  | Readonly<{
      kind: 'materialized'
      value: Readonly<{ ownedObjectId: string; created: boolean }>
    }>
  | Readonly<{ kind: 'rejected'; rejection: PathComponentRejectedError }>

interface CompatibleDirectoryAdmission {
  hasLateLogicalCollision(path: readonly string[], entryKind: 'directory'): boolean
  hasMapping(path: readonly string[], entryKind: 'directory'): boolean
  commitVerifiedDirectory(path: readonly string[], ownedObjectId: string): Promise<void>
}

/** The cache install follows the existing durable record and exact native identity validation. */
export async function admitFSAOwnedDirectory(input: Readonly<{
  scheduler: FSAOperationMutationScheduler
  authorities: FSAAuthorityCache
  repository: FSAOperationBindingRepository
  reservation: FSANamedContainerEntryReservation
  operationId: string
  canonicalPath: readonly string[]
  parent: FSAVerifiedDirectoryAuthority
  name: string
  handleId: string
  stageScope?: PersistentOutputStageScope
  compatibleNames?: CompatibleDirectoryAdmission
  recordCompatibleTargetCreated: () => Promise<void>
  randomOwnedObjectId: () => string
}>): Promise<FSADirectoryAdmissionOutcome> {
  return input.scheduler.runNamespace(
    [input.parent.schedulerIdentity],
    'create-directory',
    async () => {
      const persisted = await readFSAOwnedDirectoryBinding({
        repository: input.repository,
        reservation: input.reservation,
        handleId: input.handleId,
        diagnosticTarget: 'directory',
        ...(input.stageScope === undefined ? {} : { stageScope: input.stageScope }),
      })
      if (persisted !== undefined) {
        const current = await openDirectoryEntry(
          input.parent.handle,
          input.name,
          input.operationId,
          input.stageScope,
          'fsa.directory.entry.open',
        )
        if (!await verifySameDirectory(
          current,
          persisted.handle,
          input.operationId,
          input.stageScope,
          'fsa.directory.handle.verify',
        )) {
          throw new TargetOwnershipUnknownError('parent-authority', input.operationId)
        }
        input.authorities.installDirectory({
          ...persisted,
          canonicalPath: input.canonicalPath,
          physicalName: input.name,
          handle: current,
        })
        await input.compatibleNames?.commitVerifiedDirectory(
          input.canonicalPath,
          persisted.ownedObjectId,
        )
        return Object.freeze({
          kind: 'materialized',
          value: Object.freeze({ ownedObjectId: persisted.ownedObjectId, created: false }),
        })
      }
      if (input.compatibleNames?.hasLateLogicalCollision(input.canonicalPath, 'directory')) {
        throw new OutputDirectoryMutationError(
          'A logical directory collides with an operation-owned compatible name',
          false,
        )
      }
      let inspection: 'absent' | 'occupied'
      try {
        inspection = await runPersistentOutputStage(
          input.stageScope,
          'fsa.directory.entry.inspect',
          () => inspectFileSystemComponent({
            verifiedParent: input.parent.handle,
            component: input.name,
            expectedKind: 'directory',
            stage: 'fsa.directory.entry.inspect',
            mode: input.compatibleNames?.hasMapping(input.canonicalPath, 'directory') === true
              ? 'diagnostic'
              : 'classify-rejection',
          }),
        )
      } catch (error) {
        if (!(error instanceof PathComponentRejectedError)) throw error
        return Object.freeze({ kind: 'rejected', rejection: error })
      }
      if (inspection === 'occupied') {
        throw new TargetOwnershipUnknownError('namespace-create', input.operationId)
      }
      const created = await runPersistentOutputStage(
        input.stageScope,
        'fsa.directory.entry.create',
        () => input.parent.handle.getDirectoryHandle(input.name, { create: true }),
      )
      await input.recordCompatibleTargetCreated()
      const ownedObjectId = requireOwnedObjectId(input.randomOwnedObjectId())
      const ownedScope = input.stageScope?.withCorrelation({ ownedObjectId })
      const committedBinding = await persistFSAOwnedDirectory({
        repository: input.repository,
        reservation: input.reservation,
        handleId: input.handleId,
        ownedObjectId,
        handle: created,
        diagnosticTarget: 'directory',
        ...(ownedScope === undefined ? {} : { stageScope: ownedScope }),
      })
      const current = await openDirectoryEntry(
        input.parent.handle,
        input.name,
        input.operationId,
        ownedScope,
        'fsa.directory.entry.open',
      )
      if (!await verifySameDirectory(
        current,
        created,
        input.operationId,
        ownedScope,
        'fsa.directory.handle.verify',
      )) {
        throw new TargetOwnershipUnknownError('namespace-create', input.operationId)
      }
      input.authorities.installDirectory({
        ...committedBinding,
        canonicalPath: input.canonicalPath,
        physicalName: input.name,
        handle: current,
      })
      await input.compatibleNames?.commitVerifiedDirectory(input.canonicalPath, ownedObjectId)
      return Object.freeze({
        kind: 'materialized',
        value: Object.freeze({ ownedObjectId, created: true }),
      })
    },
  )
}
