import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  type DirectoryTreeArtifact,
} from '../../src/transfer/intent'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import {
  FSARootMutationBusyError,
  acquireFSARootMutationLease,
} from '../../src/output/browser/namespace-mutation'
import {
  prepareFSAOperationBindingTransition,
  verifyFSAOperationBinding,
} from '../../src/output/browser/indexeddb-root-binding'
import {
  assembleNewFileSystemAccessOutput,
  reopenFileSystemAccessOutput,
  reserveNewFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'

export interface FsaNamespaceFixture {
  readonly databaseName: string
  readonly parentName: string
}

interface HeldTask {
  readonly session: FileSystemAccessOutputSession
  readonly repository: IndexedDbReceiveOperationRepository
}

const heldTasks = new Map<string, HeldTask>()

export async function exerciseTaskRootRestart(
  fixture: FsaNamespaceFixture,
): Promise<{
  readonly firstCollisionIndex: number
  readonly suffixPersisted: boolean
  readonly rootIdentityPersisted: boolean
  readonly newTaskIsolated: boolean
  readonly directoryCount: number
}> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  await parent.getDirectoryHandle('photos', { create: true })
  const artifact = await resultRootArtifact()
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const first = await bindTask(fixture, parent, repository, artifact, 10)
  const firstRoot = await parent.getDirectoryHandle(first.reservation.reservedName)
  const firstName = first.reservation.reservedName
  const firstCollisionIndex = first.reservation.collisionIndex
  const intent = first.intent
  await first.close()
  repository.close()

  const reopenRepository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const reopened = await reopenFileSystemAccessOutput({
    intent,
    operationRepository: reopenRepository,
    databaseName: fixture.databaseName,
  })
  const reopenedRoot = await parent.getDirectoryHandle(reopened.reservation.reservedName)
  const suffixPersisted = reopened.reservation.reservedName === firstName &&
    reopened.reservation.collisionIndex === firstCollisionIndex
  const rootIdentityPersisted = await reopenedRoot.isSameEntry(firstRoot)
  await reopened.close()

  const second = await bindTask(fixture, parent, reopenRepository, artifact, 20)
  const secondRoot = await parent.getDirectoryHandle(second.reservation.reservedName)
  const newTaskIsolated = second.reservation.reservedName !== firstName &&
    !await secondRoot.isSameEntry(firstRoot)
  await second.close()
  reopenRepository.close()
  const directoryCount = (await entryKinds(parent)).directories
  await resetFixture(fixture)
  return Object.freeze({
    firstCollisionIndex,
    suffixPersisted,
    rootIdentityPersisted,
    newTaskIsolated,
    directoryCount,
  })
}

export async function exerciseSingleFileLayout(
  fixture: FsaNamespaceFixture,
): Promise<{
  readonly emptyBeforeRevision: boolean
  readonly revisionOpenedBeforeCreation: boolean
  readonly noExtraRoot: boolean
  readonly prefixVisible: boolean
  readonly restartReusedFile: boolean
  readonly completedBytes: number
}> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const session = await bindTask(fixture, parent, repository, await singleFileArtifact(), 30)
  const emptyBeforeRevision = (await entryKinds(parent)).total === 0
  let revisionOpened = false
  const transaction = await session.beginFile({
    artifactPath: [session.reservation.reservedName],
    openRevision: async () => {
      revisionOpened = true
      return { fileId: identity(3), fileRevision: identity(33), exactSize: 4n }
    },
  })
  const afterCreation = await entryKinds(parent)
  const revisionOpenedBeforeCreation = revisionOpened && afterCreation.files === 1
  const noExtraRoot = afterCreation.directories === 0 && afterCreation.files === 1
  await transaction.writeRange(0n, Uint8Array.of(1, 2))
  const file = await parent.getFileHandle(session.reservation.reservedName)
  // The FSA contract promises namespace visibility while incomplete. Browsers may
  // keep bytes private to the open writable until the checkpoint closes it.
  const prefixVisible = file.kind === 'file'
  await transaction.checkpoint()
  const ownedObjectId = transaction.ownedObjectId
  const intent = session.intent
  await session.close()
  repository.close()

  const reopenRepository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const reopened = await reopenFileSystemAccessOutput({
    intent,
    operationRepository: reopenRepository,
    databaseName: fixture.databaseName,
  })
  const resumed = await reopened.beginFile({
    artifactPath: [reopened.reservation.reservedName],
    openRevision: async () => ({
      fileId: identity(3),
      fileRevision: identity(33),
      exactSize: 4n,
    }),
  })
  const restartReusedFile = resumed.ownedObjectId === ownedObjectId
  await resumed.writeRange(2n, Uint8Array.of(3, 4))
  await resumed.commit()
  const completedBytes = (await (
    await parent.getFileHandle(reopened.reservation.reservedName)
  ).getFile()).size
  await reopened.close()
  reopenRepository.close()
  await resetFixture(fixture)
  return Object.freeze({
    emptyBeforeRevision,
    revisionOpenedBeforeCreation,
    noExtraRoot,
    prefixVisible,
    restartReusedFile,
    completedBytes,
  })
}

export async function holdTaskRoot(
  fixture: FsaNamespaceFixture,
): Promise<void> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const session = await bindTask(fixture, parent, repository, await resultRootArtifact(), 40)
  heldTasks.set(fixture.databaseName, { session, repository })
}

export async function probeCompetingTask(
  fixture: FsaNamespaceFixture,
): Promise<{ readonly busy: boolean; readonly scope: string | null }> {
  const parent = await parentDirectory(fixture, false)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  try {
    const session = await bindTask(fixture, parent, repository, await resultRootArtifact(), 50)
    await session.close()
    return { busy: false, scope: null }
  } catch (error) {
    return error instanceof FSARootMutationBusyError
      ? { busy: true, scope: error.scope }
      : { busy: false, scope: error instanceof Error ? error.name : 'Error' }
  } finally {
    repository.close()
  }
}

export async function releaseHeldTask(fixture: FsaNamespaceFixture): Promise<void> {
  const held = heldTasks.get(fixture.databaseName)
  if (held === undefined) return
  heldTasks.delete(fixture.databaseName)
  await held.session.close()
  held.repository.close()
  await resetFixture(fixture)
}

export async function exerciseFailedPreExecutionActivation(
  fixture: FsaNamespaceFixture,
): Promise<{ readonly rootAbsentBeforeExecution: boolean; readonly rootAbsentAfterDetach: boolean }> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const session = await bindTask(
    fixture,
    parent,
    repository,
    await resultRootArtifact(),
    60,
    false,
  )
  const rootAbsentBeforeExecution = (await entryKinds(parent)).total === 0
  await session.close()
  const rootAbsentAfterDetach = (await entryKinds(parent)).total === 0
  repository.close()
  await resetFixture(fixture)
  return Object.freeze({ rootAbsentBeforeExecution, rootAbsentAfterDetach })
}

async function bindTask(
  fixture: FsaNamespaceFixture,
  parent: FileSystemDirectoryHandle,
  repository: IndexedDbReceiveOperationRepository,
  artifact: DirectoryTreeArtifact,
  seed: number,
  activate = true,
): Promise<FileSystemAccessOutputSession> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const authority = acquiredParent(parent)
  const rootLease = await acquireFSARootMutationLease(parent)
  const reserved = await reserveNewFileSystemAccessOutput({
    authority,
    artifact,
    rootLease,
    operationId: identity(seed),
    reservationId: identity(seed + 1),
    authorityRef: identity(seed + 2, 32),
  })
  const intent = await createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reserved.reservation),
  })
  const prepared = await prepareFSAOperationBindingTransition({
    repository,
    intent,
    parent,
  })
  await repository.commitTransition({ operationId: intent.operationId, ...prepared.transition })
  const binding = await verifyFSAOperationBinding({ repository, intent, expectedParent: parent })
  const session = await assembleNewFileSystemAccessOutput({
    binding,
    operationRepository: repository,
    rootLease,
    databaseName: fixture.databaseName,
  })
  if (activate) await session.activate()
  return session
}

function acquiredParent(parent: FileSystemDirectoryHandle): AcquiredFSAParentAuthority {
  const offer = fsaParentOffer()
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    targetRouteId: offer.routeId,
    offer,
    parent,
  })
}

async function resultRootArtifact(): Promise<DirectoryTreeArtifact> {
  return createResultRootDirectoryTreeArtifact(
    createCompleteDirectoryResultRoot(identity(70), 'photos'),
  )
}

async function singleFileArtifact(): Promise<DirectoryTreeArtifact> {
  return createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: 'report.bin',
    outputName: 'report.bin',
  })
}

async function parentDirectory(
  fixture: FsaNamespaceFixture,
  create: boolean,
): Promise<FileSystemDirectoryHandle> {
  const root = await originPrivateRoot()
  return root.getDirectoryHandle(fixture.parentName, create ? { create: true } : undefined)
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

async function entryKinds(root: FileSystemDirectoryHandle): Promise<{
  readonly files: number
  readonly directories: number
  readonly total: number
}> {
  let files = 0
  let directories = 0
  for await (const handle of root.values()) {
    if (handle.kind === 'file') files += 1
    else directories += 1
  }
  return { files, directories, total: files + directories }
}

async function resetFixture(fixture: FsaNamespaceFixture): Promise<void> {
  const root = await originPrivateRoot()
  await root.removeEntry(fixture.parentName, { recursive: true }).catch(() => undefined)
  await deleteDatabase(fixture.databaseName)
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error ?? new Error('IndexedDB deletion failed'))
    request.onblocked = () => reject(new Error('IndexedDB deletion was blocked'))
  })
}

function identity(seed: number, width = 16): string {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return encodeBase64Url(value)
}
