import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  fileTarget,
  parentIdentity,
  probeNativeMutationScheduling,
  type NativeMutationSchedulingProof,
} from './fsa-native-mutation-probe'
import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  deriveArtifactChoiceIdentity,
  type DirectoryTreeArtifact,
} from '../../src/transfer/intent'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  IndexedDbFileCheckpointRepository,
  IndexedDbReceiveOperationRepository,
} from '../../src/output/browser/indexeddb-repository'
import {
  FSARootMutationBusyError,
  acquireFSARootMutationLease,
  type FSARootMutationLease,
} from '../../src/output/browser/namespace-mutation'
import type {
  FSAOperationMutationScheduler,
  FSAParentMutationIdentity,
  FSAWriterLifecycleLease,
} from '../../src/output/browser/mutation-coordination/model'
import {
  createFSAOperationMutationScheduler,
} from '../../src/output/browser/mutation-coordination/scheduler'
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
import { isSafeComponentName } from '../../src/output/file-system-access/safe-name'

export interface FsaNamespaceFixture {
  readonly databaseName: string
  readonly parentName: string
}

export function preservingBrowserRecoveryPolicy() {
  return Object.freeze({ pausedFile: 'preserve' as const })
}

interface HeldTask {
  readonly session: FileSystemAccessOutputSession
  readonly repository: IndexedDbReceiveOperationRepository
}

const heldTasks = new Map<string, HeldTask>()

interface HeldNativeDrain {
  readonly rootLease: FSARootMutationLease
  readonly scheduler: FSAOperationMutationScheduler
  readonly writer: FileSystemWritableFileStream
  readonly writerLease: FSAWriterLifecycleLease
  releasePromise: Promise<void> | undefined
}

const heldNativeDrains = new Map<string, HeldNativeDrain>()

export async function exerciseNativeMutationScheduling(
  fixture: FsaNamespaceFixture,
): Promise<NativeMutationSchedulingProof> {
  await resetFixture(fixture)
  try {
    return await probeNativeMutationScheduling(await parentDirectory(fixture, true))
  } finally {
    await resetFixture(fixture)
  }
}

export interface NativeWriterFailureReleaseProof {
  readonly closeFailureObserved: boolean
  readonly abortFailureObserved: boolean
  readonly successorBytes: readonly number[]
  readonly acquiredWriterLeases: number
  readonly releasedWriterLeases: number
}

export async function exerciseNativeWriterFailureRelease(
  fixture: FsaNamespaceFixture,
): Promise<NativeWriterFailureReleaseProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const identity = parentIdentity('native-failure-parent')
  const scheduler = createFSAOperationMutationScheduler({
    rootParent: identity,
    maximumActiveWriters: 1,
  })
  try {
    const closeFailureObserved = await failSettledNativeWriter(
      scheduler,
      identity,
      await parent.getFileHandle('close-failure.bin', { create: true }),
      'close',
    )
    const abortFailureObserved = await failSettledNativeWriter(
      scheduler,
      identity,
      await parent.getFileHandle('abort-failure.bin', { create: true }),
      'abort',
    )
    const successorHandle = await parent.getFileHandle('successor.bin', { create: true })
    const successorLease = await scheduler.acquireWriter(fileTarget(identity, successorHandle.name))
    const successorWriter = await successorHandle.createWritable()
    try {
      await successorWriter.write(Uint8Array.of(21, 22, 23))
      await successorWriter.close()
    } finally {
      successorLease.release()
    }
    const successor = await successorHandle.getFile()
    const diagnostics = scheduler.diagnostics()
    await scheduler.close()
    return Object.freeze({
      closeFailureObserved,
      abortFailureObserved,
      successorBytes: Object.freeze([...new Uint8Array(await successor.arrayBuffer())]),
      acquiredWriterLeases: diagnostics.acquiredWriterLeases,
      releasedWriterLeases: diagnostics.releasedWriterLeases,
    })
  } finally {
    await scheduler.close().catch(() => undefined)
    await resetFixture(fixture)
  }
}

export async function holdNativeRootThroughWriterDrain(
  fixture: FsaNamespaceFixture,
): Promise<void> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const rootLease = await acquireFSARootMutationLease(parent, undefined, 1)
  const handle = await parent.getFileHandle('held.bin', { create: true })
  const writerLease = await rootLease.scheduler.acquireWriter(
    fileTarget(rootLease.authority.rootParentIdentity, handle.name),
  )
  const writer = await handle.createWritable()
  await writer.write(Uint8Array.of(31, 32))
  heldNativeDrains.set(fixture.databaseName, {
    rootLease,
    scheduler: rootLease.scheduler,
    writer,
    writerLease,
    releasePromise: undefined,
  })
}

export async function beginHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<{
  readonly schedulerState: string
  readonly lateWriterRejection: string
  readonly lateNamespaceRejection: string
  readonly rootReleasePending: boolean
}> {
  const held = requireHeldNativeDrain(fixture)
  const schedulerClose = held.scheduler.close()
  let rootReleased = false
  held.releasePromise = Promise.all([schedulerClose, held.rootLease.release()])
    .then(() => { rootReleased = true })
  const lateWriterRejection = await rejectionName(
    held.scheduler.acquireWriter(fileTarget(held.rootLease.authority.rootParentIdentity, 'late.bin')),
  )
  const lateNamespaceRejection = await rejectionName(held.scheduler.runRootNamespace(
    'create-file',
    async () => undefined,
  ))
  return Object.freeze({
    schedulerState: held.scheduler.diagnostics().state,
    lateWriterRejection,
    lateNamespaceRejection,
    rootReleasePending: !rootReleased,
  })
}

export async function probeCompetingNativeRoot(
  fixture: FsaNamespaceFixture,
): Promise<{ readonly busy: boolean; readonly scope: string | null }> {
  const parent = await parentDirectory(fixture, false)
  try {
    const lease = await acquireFSARootMutationLease(parent)
    await lease.release()
    return { busy: false, scope: null }
  } catch (error) {
    return error instanceof FSARootMutationBusyError
      ? { busy: true, scope: error.scope }
      : { busy: false, scope: error instanceof Error ? error.name : 'Error' }
  }
}

export async function finishHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void> {
  const held = requireHeldNativeDrain(fixture)
  try {
    await held.writer.close()
  } finally {
    held.writerLease.release()
  }
  await held.releasePromise
}

export async function cleanupHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void> {
  const held = heldNativeDrains.get(fixture.databaseName)
  heldNativeDrains.delete(fixture.databaseName)
  if (held !== undefined) {
    await held.writer.abort().catch(() => undefined)
    held.writerLease.release()
    await held.rootLease.release().catch(() => undefined)
  }
  await resetFixture(fixture)
}

export interface SafeNameRefusalProof {
  readonly scope: 'file' | 'directory' | 'result-root'
  readonly expectedKind: 'file' | 'directory'
  readonly rejectionCallCount: number
  readonly rejectedEntriesBefore: readonly string[]
  readonly contentBlockRequestCountAtRefusal: number
  readonly logicalEntryAbsent: boolean
  readonly safeComponentName: boolean
  readonly summaryCount: number
  readonly extraEntriesCount: number
}

export interface OrdinarySafeNameDormancyProof {
  readonly summaryPresent: boolean
  readonly unexpectedLookupCount: number
  readonly outputNames: readonly string[]
  readonly bothRevisionsAdmittedBeforeRelease: boolean
}

export interface SafeNameScenarioClosureProof {
  readonly refusals: readonly SafeNameRefusalProof[]
  readonly ordinary: OrdinarySafeNameDormancyProof
}

export async function exerciseSafeNameScenarioClosure(
  fixture: FsaNamespaceFixture,
): Promise<SafeNameScenarioClosureProof> {
  const refusals: SafeNameRefusalProof[] = []
  refusals.push(await exerciseDescendantRefusal(
    scopedFixture(fixture, 'file'),
    'file',
    'portable-file.bin',
    71,
  ))
  refusals.push(await exerciseDescendantRefusal(
    scopedFixture(fixture, 'directory'),
    'directory',
    'portable-directory',
    81,
  ))
  refusals.push(await exerciseResultRootRefusal(scopedFixture(fixture, 'root'), 91))
  const ordinary = await exerciseOrdinarySafeNameDormancy(scopedFixture(fixture, 'ordinary'))
  return Object.freeze({ refusals: Object.freeze(refusals), ordinary })
}

async function exerciseDescendantRefusal(
  fixture: FsaNamespaceFixture,
  expectedKind: 'file' | 'directory',
  logicalComponent: string,
  seed: number,
): Promise<SafeNameRefusalProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  let session: FileSystemAccessOutputSession | undefined
  let interceptor: NativeLookupInterceptor | undefined
  let contentBlockRequestCount = 0
  const cause = new TypeError('injected exact native refusal')
  try {
    session = await bindTask(
      fixture,
      parent,
      repository,
      await resultRootArtifact(),
      seed,
      true,
    )
    const root = await parent.getDirectoryHandle(session.reservation.physicalName)
    interceptor = installNativeLookupInterceptor({
      parent: root,
      rejection: { kind: expectedKind, cause },
      contentBlockRequestCount: () => contentBlockRequestCount,
    })

    if (expectedKind === 'file') {
      const transaction = await session.beginFile({
        materializationRelativePath: [logicalComponent],
        openRevision: async () => ({
          fileId: identity(seed + 1),
          fileRevision: identity(seed + 2),
          exactSize: 2n,
        }),
      })
      contentBlockRequestCount += 1
      await transaction.writeRange(0n, Uint8Array.of(4, 2))
      await transaction.commit()
    } else {
      await session.ensureDirectory([logicalComponent])
    }

    const summary = session.safeRenamedSummary()
    const renamed = summary?.sample.find(entry => entry.logicalPath.length === 1 &&
      entry.logicalPath[0] === logicalComponent)
    if (renamed === undefined) throw new TypeError('safe-renamed entry was not recorded')
    const rejection = interceptor.calls.filter(call => call.rejected)
    if (rejection.length !== 1 || rejection[0]?.name !== logicalComponent) {
      throw new TypeError('fault injection missed the awaited logical-component lookup')
    }
    const names = await entryNames(root)
    return Object.freeze({
      scope: expectedKind,
      expectedKind,
      rejectionCallCount: rejection.length,
      rejectedEntriesBefore: rejection[0]?.entriesBefore ?? Object.freeze([]),
      contentBlockRequestCountAtRefusal: rejection[0]?.contentBlockRequestCount ?? -1,
      logicalEntryAbsent: !names.includes(logicalComponent),
      safeComponentName: isSafeComponentName(renamed.physicalName),
      summaryCount: summary?.count ?? 0,
      extraEntriesCount: names.filter(name => name !== renamed.physicalName).length,
    })
  } finally {
    interceptor?.restore()
    await session?.close().catch(() => undefined)
    repository.close()
    await resetFixture(fixture)
  }
}

async function exerciseResultRootRefusal(
  fixture: FsaNamespaceFixture,
  seed: number,
): Promise<SafeNameRefusalProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const logicalComponent = 'photos'
  const cause = new TypeError('injected exact native refusal')
  let session: FileSystemAccessOutputSession | undefined
  const interceptor = installNativeLookupInterceptor({
    parent,
    rejection: { kind: 'directory', cause },
    contentBlockRequestCount: () => 0,
  })
  try {
    session = await bindTask(
      fixture,
      parent,
      repository,
      await resultRootArtifact(),
      seed,
      true,
    )
    const summary = session.safeRenamedSummary()
    const physicalName = session.reservation.physicalName
    const rejection = interceptor.calls.filter(call => call.rejected)
    if (rejection.length !== 1 || rejection[0]?.name !== logicalComponent) {
      throw new TypeError('fault injection missed the awaited result-root lookup')
    }
    const names = await entryNames(parent)
    return Object.freeze({
      scope: 'result-root',
      expectedKind: 'directory',
      rejectionCallCount: rejection.length,
      rejectedEntriesBefore: rejection[0]?.entriesBefore ?? Object.freeze([]),
      contentBlockRequestCountAtRefusal: rejection[0]?.contentBlockRequestCount ?? -1,
      logicalEntryAbsent: !names.includes(logicalComponent),
      safeComponentName: isSafeComponentName(physicalName),
      summaryCount: summary?.count ?? 0,
      extraEntriesCount: names.filter(name => name !== physicalName).length,
    })
  } finally {
    interceptor.restore()
    await session?.close().catch(() => undefined)
    repository.close()
    await resetFixture(fixture)
  }
}

async function exerciseOrdinarySafeNameDormancy(
  fixture: FsaNamespaceFixture,
): Promise<OrdinarySafeNameDormancyProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  let session: FileSystemAccessOutputSession | undefined
  let interceptor: NativeLookupInterceptor | undefined
  try {
    session = await bindTask(
      fixture,
      parent,
      repository,
      await resultRootArtifact(),
      101,
      true,
    )
    const root = await parent.getDirectoryHandle(session.reservation.physicalName)
    interceptor = installNativeLookupInterceptor({ parent: root, contentBlockRequestCount: () => 0 })
    const admitted = deferred<void>()
    const release = deferred<void>()
    let revisionAdmissions = 0
    const begin = (name: string, fileSeed: number) => session!.beginFile({
      materializationRelativePath: [name],
      openRevision: async () => {
        revisionAdmissions += 1
        if (revisionAdmissions === 2) admitted.resolve()
        await release.promise
        return {
          fileId: identity(fileSeed),
          fileRevision: identity(fileSeed + 1),
          exactSize: 1n,
        }
      },
    })
    const firstPromise = begin('ordinary-a.bin', 201)
    const secondPromise = begin('ordinary-b.bin', 211)
    await admitted.promise
    const bothRevisionsAdmittedBeforeRelease = revisionAdmissions === 2
    release.resolve()
    const [first, second] = await Promise.all([firstPromise, secondPromise])
    await Promise.all([
      first.writeRange(0n, Uint8Array.of(1)).then(() => first.commit()),
      second.writeRange(0n, Uint8Array.of(2)).then(() => second.commit()),
    ])
    const allowedNames = new Set(['ordinary-a.bin', 'ordinary-b.bin'])
    const summaryPresent = session.safeRenamedSummary() !== undefined
    const unexpectedLookupCount = interceptor.calls.filter(call => !allowedNames.has(call.name)).length
    const outputNames = Object.freeze(await entryNames(root))
    interceptor.restore()
    interceptor = undefined
    await session.close()
    session = undefined
    return Object.freeze({
      summaryPresent,
      unexpectedLookupCount,
      outputNames,
      bothRevisionsAdmittedBeforeRelease,
    })
  } finally {
    interceptor?.restore()
    await session?.close().catch(() => undefined)
    repository.close()
    await resetFixture(fixture)
  }
}

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
  const firstRoot = await parent.getDirectoryHandle(first.reservation.physicalName)
  const firstName = first.reservation.physicalName
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
  const reopenedRoot = await parent.getDirectoryHandle(reopened.reservation.physicalName)
  const suffixPersisted = reopened.reservation.physicalName === firstName &&
    reopened.reservation.collisionIndex === firstCollisionIndex
  const rootIdentityPersisted = await reopenedRoot.isSameEntry(firstRoot)
  await reopened.close()

  const second = await bindTask(fixture, parent, reopenRepository, artifact, 20)
  const secondRoot = await parent.getDirectoryHandle(second.reservation.physicalName)
  const newTaskIsolated = second.reservation.physicalName !== firstName &&
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
    materializationRelativePath: [],
    openRevision: async () => {
      revisionOpened = true
      return { fileId: identity(3), fileRevision: identity(33), exactSize: 4n }
    },
  })
  const afterCreation = await entryKinds(parent)
  const revisionOpenedBeforeCreation = revisionOpened && afterCreation.files === 1
  const noExtraRoot = afterCreation.directories === 0 && afterCreation.files === 1
  await transaction.writeRange(0n, Uint8Array.of(1, 2))
  const file = await parent.getFileHandle(session.reservation.physicalName)
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
    materializationRelativePath: [],
    recovery: preservingBrowserRecoveryPolicy(),
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
    await parent.getFileHandle(reopened.reservation.physicalName)
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

export async function bindTask(
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
    preClickRanking: [(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id],
  })
  await repository.commitTransition({ operationId: intent.operationId, ...prepared.transition })
  const binding = await verifyFSAOperationBinding({ repository, intent, expectedParent: parent })
  const session = await assembleNewFileSystemAccessOutput({
    binding,
    operationRepository: repository,
    rootLease,
    databaseName: fixture.databaseName,
    checkpointRepositoryFactory: binding =>
      IndexedDbFileCheckpointRepository.open(binding, fixture.databaseName),
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

export async function resultRootArtifact(): Promise<DirectoryTreeArtifact> {
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

export interface NativeLookupCall {
  readonly kind: 'file' | 'directory'
  readonly name: string
  readonly create: boolean
  readonly rejected: boolean
  readonly entriesBefore: readonly string[]
  readonly contentBlockRequestCount: number
}

export interface NativeLookupInterceptor {
  readonly calls: readonly NativeLookupCall[]
  restore(): void
}

export function installNativeLookupInterceptor(input: Readonly<{
  parent: FileSystemDirectoryHandle
  rejection?: Readonly<{
    kind: 'file' | 'directory'
    cause: TypeError
  }>
  contentBlockRequestCount: () => number
}>): NativeLookupInterceptor {
  const prototype = Object.getPrototypeOf(input.parent) as object
  const fileDescriptor = Object.getOwnPropertyDescriptor(prototype, 'getFileHandle')
  const directoryDescriptor = Object.getOwnPropertyDescriptor(prototype, 'getDirectoryHandle')
  if (fileDescriptor === undefined || typeof fileDescriptor.value !== 'function' ||
      directoryDescriptor === undefined || typeof directoryDescriptor.value !== 'function') {
    throw new TypeError('browser FSA directory methods are unavailable for call-locus injection')
  }
  const originalFile = fileDescriptor.value as FileSystemDirectoryHandle['getFileHandle']
  const originalDirectory = directoryDescriptor.value as FileSystemDirectoryHandle['getDirectoryHandle']
  const calls: NativeLookupCall[] = []
  let rejectionConsumed = false
  const intercept = async <T extends FileSystemHandle>(
    receiver: FileSystemDirectoryHandle,
    kind: 'file' | 'directory',
    name: string,
    options: FileSystemGetFileOptions | FileSystemGetDirectoryOptions | undefined,
    nativeLookup: (name: string, options?: FileSystemGetFileOptions | FileSystemGetDirectoryOptions) => Promise<T>,
  ): Promise<T> => {
    if (!await receiver.isSameEntry(input.parent)) return nativeLookup(name, options)
    const entriesBefore = await entryNames(receiver)
    const rejected = !rejectionConsumed && options?.create !== true &&
      input.rejection?.kind === kind
    if (rejected) rejectionConsumed = true
    calls.push(Object.freeze({
      kind,
      name,
      create: options?.create === true,
      rejected,
      entriesBefore,
      contentBlockRequestCount: input.contentBlockRequestCount(),
    }))
    if (rejected) throw input.rejection!.cause
    return nativeLookup(name, options)
  }

  Object.defineProperty(prototype, 'getFileHandle', {
    ...fileDescriptor,
    value: function (
      this: FileSystemDirectoryHandle,
      name: string,
      options?: FileSystemGetFileOptions,
    ): Promise<FileSystemFileHandle> {
      return intercept(
        this,
        'file',
        name,
        options,
        (component, nativeOptions) => Reflect.apply(
          originalFile,
          this,
          [component, nativeOptions as FileSystemGetFileOptions | undefined],
        ) as Promise<FileSystemFileHandle>,
      )
    },
  })
  Object.defineProperty(prototype, 'getDirectoryHandle', {
    ...directoryDescriptor,
    value: function (
      this: FileSystemDirectoryHandle,
      name: string,
      options?: FileSystemGetDirectoryOptions,
    ): Promise<FileSystemDirectoryHandle> {
      return intercept(
        this,
        'directory',
        name,
        options,
        (component, nativeOptions) => Reflect.apply(
          originalDirectory,
          this,
          [component, nativeOptions as FileSystemGetDirectoryOptions | undefined],
        ) as Promise<FileSystemDirectoryHandle>,
      )
    },
  })

  let restored = false
  return Object.freeze({
    calls,
    restore: () => {
      if (restored) return
      restored = true
      Object.defineProperty(prototype, 'getFileHandle', fileDescriptor)
      Object.defineProperty(prototype, 'getDirectoryHandle', directoryDescriptor)
    },
  })
}

function scopedFixture(fixture: FsaNamespaceFixture, scope: string): FsaNamespaceFixture {
  return Object.freeze({
    databaseName: `${fixture.databaseName}-${scope}`,
    parentName: `${fixture.parentName}-${scope}`,
  })
}

async function entryNames(parent: FileSystemDirectoryHandle): Promise<readonly string[]> {
  const names: string[] = []
  for await (const [name] of parent.entries()) names.push(name)
  return Object.freeze(names.sort())
}

async function failSettledNativeWriter(
  scheduler: FSAOperationMutationScheduler,
  parent: FSAParentMutationIdentity,
  handle: FileSystemFileHandle,
  failingOperation: 'close' | 'abort',
): Promise<boolean> {
  const lease = await scheduler.acquireWriter(fileTarget(parent, handle.name))
  const writer = await handle.createWritable()
  try {
    if (failingOperation === 'close') {
      await writer.abort(new DOMException('close failure setup', 'AbortError'))
      return await rejectionName(writer.close()) !== 'resolved'
    }
    const streamWriter = writer.getWriter()
    try {
      const failed = await rejectionName(writer.abort()) !== 'resolved'
      await streamWriter.abort(new DOMException('abort failure cleanup', 'AbortError'))
      return failed
    } finally {
      streamWriter.releaseLock()
    }
  } finally {
    await writer.abort().catch(() => undefined)
    lease.release()
  }
}

function requireHeldNativeDrain(fixture: FsaNamespaceFixture): HeldNativeDrain {
  const held = heldNativeDrains.get(fixture.databaseName)
  if (held === undefined) throw new DOMException('Native FSA drain fixture is not held', 'InvalidStateError')
  return held
}

async function rejectionName(promise: Promise<unknown>): Promise<string> {
  return promise.then(
    () => 'resolved',
    error => error instanceof Error ? error.name : 'Error',
  )
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
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
