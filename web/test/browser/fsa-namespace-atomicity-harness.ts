import { encodeBase64Url } from '../../src/crypto/bytes'
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
  IndexedDbCompatibleNameLedger,
} from '../../src/output/browser/indexeddb-compatible-name-ledger'
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
import {
  CompatibleNameCoordinator,
  prepareCompatibleNameRootRepair,
  type CompatibleNameActivationLedger,
  type CompatibleNameRootRepairFactory,
} from '../../src/output/file-system-access/compatible-name/coordinator'
import { compatibleNameRestorationPairCandidate } from '../../src/output/file-system-access/compatible-name/naming'
import { decodeCompatibleNameSidecar } from '../../src/output/file-system-access/compatible-name/sidecar-codec'

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

const NATIVE_SCHEDULER_WRITER_LIMIT = 2
const heldNativeDrains = new Map<string, HeldNativeDrain>()

export interface NativeMutationSchedulingProof {
  readonly sameParentMutationStartedBeforeWriterClose: boolean
  readonly independentParentCreatedWhileSameParentDrained: boolean
  readonly laterWriterAdmittedDuringMutation: boolean
  readonly eventOrder: readonly string[]
  readonly sameParentFileBytes: readonly number[]
  readonly independentParentFileBytes: readonly number[]
  readonly peakActiveWriters: number
}

export async function exerciseNativeMutationScheduling(
  fixture: FsaNamespaceFixture,
): Promise<NativeMutationSchedulingProof> {
  await resetFixture(fixture)
  const root = await parentDirectory(fixture, true)
  const blockedParent = await root.getDirectoryHandle('blocked-parent', { create: true })
  const independentParent = await root.getDirectoryHandle('independent-parent', { create: true })
  const blockedIdentity = parentIdentity('blocked-parent')
  const independentIdentity = parentIdentity('independent-parent')
  const scheduler = createFSAOperationMutationScheduler({
    rootParent: parentIdentity('root'),
    maximumActiveWriters: NATIVE_SCHEDULER_WRITER_LIMIT,
  })
  const activeHandle = await blockedParent.getFileHandle('active.bin', { create: true })
  const activeLease = await scheduler.acquireWriter(blockedIdentity)
  const activeWriter = await activeHandle.createWritable()
  const eventOrder: string[] = []
  const mutationStarted = deferred<void>()
  const releaseMutation = deferred<void>()
  let sameParentMutationStarted = false
  let laterWriterAdmitted = false
  let laterLease: FSAWriterLifecycleLease | undefined
  let laterWriterPromise: Promise<FSAWriterLifecycleLease> | undefined

  try {
    await activeWriter.write(Uint8Array.of(1, 2, 3))
    const sameParentMutation = scheduler.runNamespace(
      [blockedIdentity],
      'create-file',
      async () => {
        sameParentMutationStarted = true
        eventOrder.push('same-parent-mutation')
        const created = await blockedParent.getFileHandle('after-drain.bin', { create: true })
        const writer = await created.createWritable()
        await writer.write(Uint8Array.of(4, 5))
        await writer.close()
        mutationStarted.resolve()
        await releaseMutation.promise
      },
    )
    laterWriterPromise = scheduler.acquireWriter(blockedIdentity).then((lease) => {
      laterLease = lease
      laterWriterAdmitted = true
      eventOrder.push('later-writer')
      return lease
    })

    const independentResult = await scheduler.runNamespace(
      [independentIdentity],
      'create-file',
      async () => {
        eventOrder.push('independent-parent-mutation')
        const created = await independentParent.getFileHandle('independent.bin', { create: true })
        const writer = await created.createWritable()
        await writer.write(Uint8Array.of(8, 9))
        await writer.close()
        return created
      },
    )
    const sameParentMutationStartedBeforeWriterClose = sameParentMutationStarted

    await activeWriter.close()
    activeLease.release()
    await mutationStarted.promise
    const laterWriterAdmittedDuringMutation = laterWriterAdmitted
    releaseMutation.resolve()
    await sameParentMutation
    laterLease = await laterWriterPromise
    const laterHandle = await blockedParent.getFileHandle('later.bin', { create: true })
    const laterNativeWriter = await laterHandle.createWritable()
    await laterNativeWriter.write(Uint8Array.of(6, 7))
    await laterNativeWriter.close()
    laterLease.release()
    laterLease = undefined

    const sameParentFile = await (await blockedParent.getFileHandle('after-drain.bin')).getFile()
    const independentParentFile = await independentResult.getFile()
    const diagnostics = scheduler.diagnostics()
    await scheduler.close()
    return Object.freeze({
      sameParentMutationStartedBeforeWriterClose,
      independentParentCreatedWhileSameParentDrained:
        independentParentFile.size === 2 && !sameParentMutationStartedBeforeWriterClose,
      laterWriterAdmittedDuringMutation,
      eventOrder: Object.freeze(eventOrder),
      sameParentFileBytes: Object.freeze([
        ...new Uint8Array(await sameParentFile.arrayBuffer()),
      ]),
      independentParentFileBytes: Object.freeze([
        ...new Uint8Array(await independentParentFile.arrayBuffer()),
      ]),
      peakActiveWriters: diagnostics.peakActiveWriters,
    })
  } finally {
    releaseMutation.resolve()
    await activeWriter.abort().catch(() => undefined)
    activeLease.release()
    await laterWriterPromise?.catch(() => undefined)
    laterLease?.release()
    await scheduler.close().catch(() => undefined)
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
    const successorLease = await scheduler.acquireWriter(identity)
    const successorHandle = await parent.getFileHandle('successor.bin', { create: true })
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
  const writerLease = await rootLease.scheduler.acquireWriter(rootLease.authority.rootParentIdentity)
  const handle = await parent.getFileHandle('held.bin', { create: true })
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
    held.scheduler.acquireWriter(held.rootLease.authority.rootParentIdentity),
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

export interface CompatibleNameRefusalProof {
  readonly scope: 'file' | 'directory' | 'result-root'
  readonly expectedKind: 'file' | 'directory'
  readonly rejectionCallCount: number
  readonly rejectedEntriesBefore: readonly string[]
  readonly contentBlockRequestCountAtRefusal: number
  readonly logicalEntryAbsent: boolean
  readonly repairActive: boolean
  readonly pairReadyBeforeTarget: boolean
  readonly sidecarCommittedCountBeforeTarget: number
  readonly sidecarCommittedCountAfterCommit: number
  readonly committedMappingKinds: readonly ('file' | 'directory')[]
  readonly rootCausePreserved: boolean | null
}

export interface OrdinaryCompatibleNameDormancyProof {
  readonly repairFactoryCalls: number
  readonly ledgerOpens: number
  readonly repairProjectionPresent: boolean
  readonly projectionActivationCount: number
  readonly unexpectedLookupCount: number
  readonly outputNames: readonly string[]
  readonly bothRevisionsAdmittedBeforeRelease: boolean
}

export interface CompatibleNameScenarioClosureProof {
  readonly refusals: readonly CompatibleNameRefusalProof[]
  readonly ordinary: OrdinaryCompatibleNameDormancyProof
}

export async function exerciseCompatibleNameScenarioClosure(
  fixture: FsaNamespaceFixture,
): Promise<CompatibleNameScenarioClosureProof> {
  const refusals: CompatibleNameRefusalProof[] = []
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
  const ordinary = await exerciseOrdinaryCompatibleNameDormancy(scopedFixture(fixture, 'ordinary'))
  return Object.freeze({ refusals: Object.freeze(refusals), ordinary })
}

export async function exerciseRestorationPairCollisions(fixture: FsaNamespaceFixture) {
  const results = []
  for (const occupiedKind of ['script', 'sidecar'] as const) {
    const scoped = scopedFixture(fixture, occupiedKind)
    await resetFixture(scoped)
    const parent = await parentDirectory(scoped, true)
    const repository = await IndexedDbReceiveOperationRepository.open(scoped.databaseName)
    const session = await bindTask(scoped, parent, repository, await resultRootArtifact(), 111)
    const root = await parent.getDirectoryHandle(session.reservation.physicalName)
    const first = await compatibleNameRestorationPairCandidate({
      operationId: session.intent.operationId, primaryToken: 'aaaaaa', attempt: 0,
    })
    const second = await compatibleNameRestorationPairCandidate({
      operationId: session.intent.operationId, primaryToken: 'aaaaaa', attempt: 1,
    })
    const occupied = await root.getFileHandle(first[occupiedKind], { create: true })
    const writer = await occupied.createWritable()
    await writer.write('unrelated existing file')
    await writer.close()
    const interceptor = installNativeLookupInterceptor({
      parent: root,
      rejection: { kind: 'directory', cause: new TypeError('injected exact native refusal') },
      contentBlockRequestCount: () => 0,
    })
    try {
      await session.ensureDirectory(['mapped-directory'])
      await waitForCompatibleProjection(session, 1)
      const operation = await readCompatibleOperation(scoped, session.intent.operationId)
      const names = await entryNames(root)
      results.push(Object.freeze({
        occupiedKind,
        pairRetriedTogether: operation.header.pair.token === second.token &&
          operation.header.pair.script.physicalName === second.script &&
          operation.header.pair.sidecar.physicalName === second.sidecar,
        unoccupiedFirstPartnerAbsent: !names.includes(first[occupiedKind === 'script' ? 'sidecar' : 'script']),
        occupiedFilePreserved: await (await occupied.getFile()).text() === 'unrelated existing file',
        contentTokenUnchanged: operation.mappings[0]?.token === 'aaaaaa',
        pairOwned: operation.header.pair.script.ownershipState === 'owned' &&
          operation.header.pair.sidecar.ownershipState === 'owned',
      }))
    } finally {
      interceptor.restore()
      await session.close()
      repository.close()
      await resetFixture(scoped)
    }
  }
  return Object.freeze(results)
}

async function exerciseDescendantRefusal(
  fixture: FsaNamespaceFixture,
  expectedKind: 'file' | 'directory',
  logicalComponent: string,
  seed: number,
): Promise<CompatibleNameRefusalProof> {
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
      {
        openCompatibleNameLedger: () => IndexedDbCompatibleNameLedger.open(fixture.databaseName),
      },
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
      const beforeCommit = await readCompatibleOperation(fixture, session.intent.operationId)
      const beforeSidecar = await readCompatibleSidecar(root, beforeCommit.header.pair.sidecar.physicalName)
      if (beforeSidecar.footer.committedCount !== 0) {
        throw new TypeError('uncommitted compatible file was published to the sidecar')
      }
      contentBlockRequestCount += 1
      await transaction.writeRange(0n, Uint8Array.of(4, 2))
      await transaction.commit()
    } else {
      await session.ensureDirectory([logicalComponent])
    }

    await waitForCompatibleProjection(session, 1)
    const operation = await readCompatibleOperation(fixture, session.intent.operationId)
    const sidecar = await readCompatibleSidecar(root, operation.header.pair.sidecar.physicalName)
    const mapping = operation.mappings.find(value => value.logicalPath.length === 1 &&
      value.logicalPath[0] === logicalComponent && value.entryKind === expectedKind)
    if (mapping === undefined) throw new TypeError('compatible mapping was not persisted')
    const rejection = interceptor.calls.filter(call => call.rejected)
    if (rejection.length !== 1 || rejection[0]?.name !== logicalComponent) {
      throw new TypeError('fault injection missed the awaited logical-component lookup')
    }
    const targetCreate = interceptor.calls.find(call => call.create && call.kind === expectedKind &&
      call.name === mapping.physicalComponent)
    return Object.freeze({
      scope: expectedKind,
      expectedKind,
      rejectionCallCount: rejection.length,
      rejectedEntriesBefore: rejection[0]?.entriesBefore ?? Object.freeze([]),
      contentBlockRequestCountAtRefusal: rejection[0]?.contentBlockRequestCount ?? -1,
      logicalEntryAbsent: !(await entryNames(root)).includes(logicalComponent),
      repairActive: session.compatibleNameRepairActive,
      pairReadyBeforeTarget: targetCreate?.activeZeroSidecarsBefore === 1,
      sidecarCommittedCountBeforeTarget: targetCreate?.activeZeroSidecarsBefore === 1 ? 0 : -1,
      sidecarCommittedCountAfterCommit: sidecar.footer.committedCount,
      committedMappingKinds: Object.freeze(operation.mappings
        .filter(value => value.commitState === 'committed')
        .map(value => value.entryKind)),
      rootCausePreserved: null,
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
): Promise<CompatibleNameRefusalProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const logicalComponent = 'photos'
  const cause = new TypeError('injected exact native refusal')
  let rootCausePreserved = false
  let session: FileSystemAccessOutputSession | undefined
  const interceptor = installNativeLookupInterceptor({
    parent,
    rejection: { kind: 'directory', cause },
    contentBlockRequestCount: () => 0,
  })
  const prepareRootRepair: CompatibleNameRootRepairFactory = async input => {
    const prepared = await prepareCompatibleNameRootRepair(input, compatibleNamePreparation())
    rootCausePreserved = prepared.rejection.cause === cause
    return prepared
  }
  try {
    session = await bindTask(
      fixture,
      parent,
      repository,
      await resultRootArtifact(),
      seed,
      true,
      {
        prepareCompatibleNameRootRepair: prepareRootRepair,
        openCompatibleNameLedger: () => IndexedDbCompatibleNameLedger.open(fixture.databaseName),
      },
    )
    await waitForCompatibleProjection(session, 1)
    const operation = await readCompatibleOperation(fixture, session.intent.operationId)
    const sidecar = await readCompatibleSidecar(
      parent,
      operation.header.pair.sidecar.physicalName,
    )
    const mapping = operation.mappings.find(value => value.logicalPath.length === 1 &&
      value.logicalPath[0] === logicalComponent && value.entryKind === 'directory')
    if (mapping === undefined) throw new TypeError('compatible result-root mapping was not persisted')
    const rejection = interceptor.calls.filter(call => call.rejected)
    if (rejection.length !== 1 || rejection[0]?.name !== logicalComponent) {
      throw new TypeError('fault injection missed the awaited result-root lookup')
    }
    const targetCreate = interceptor.calls.find(call => call.create && call.kind === 'directory' &&
      call.name === mapping.physicalComponent)
    return Object.freeze({
      scope: 'result-root',
      expectedKind: 'directory',
      rejectionCallCount: rejection.length,
      rejectedEntriesBefore: rejection[0]?.entriesBefore ?? Object.freeze([]),
      contentBlockRequestCountAtRefusal: rejection[0]?.contentBlockRequestCount ?? -1,
      logicalEntryAbsent: !(await entryNames(parent)).includes(logicalComponent),
      repairActive: session.compatibleNameRepairActive,
      pairReadyBeforeTarget: targetCreate?.activeZeroSidecarsBefore === 1,
      sidecarCommittedCountBeforeTarget: targetCreate?.activeZeroSidecarsBefore === 1 ? 0 : -1,
      sidecarCommittedCountAfterCommit: sidecar.footer.committedCount,
      committedMappingKinds: Object.freeze(operation.mappings
        .filter(value => value.commitState === 'committed')
        .map(value => value.entryKind)),
      rootCausePreserved,
    })
  } finally {
    interceptor.restore()
    await session?.close().catch(() => undefined)
    repository.close()
    await resetFixture(fixture)
  }
}

async function exerciseOrdinaryCompatibleNameDormancy(
  fixture: FsaNamespaceFixture,
): Promise<OrdinaryCompatibleNameDormancyProof> {
  await resetFixture(fixture)
  const parent = await parentDirectory(fixture, true)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  let repairFactoryCalls = 0
  let ledgerOpens = 0
  let projectionActivationCount = 0
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
      {
        prepareCompatibleNameRootRepair: async input => {
          repairFactoryCalls += 1
          return prepareCompatibleNameRootRepair(input, compatibleNamePreparation())
        },
        openCompatibleNameLedger: async () => {
          ledgerOpens += 1
          return IndexedDbCompatibleNameLedger.open(fixture.databaseName)
        },
      },
    )
    const root = await parent.getDirectoryHandle(session.reservation.physicalName)
    interceptor = installNativeLookupInterceptor({ parent: root, contentBlockRequestCount: () => 0 })
    const unsubscribe = session.subscribeRepairProjectionActivation(() => {
      projectionActivationCount += 1
    })
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
    unsubscribe()
    const allowedNames = new Set(['ordinary-a.bin', 'ordinary-b.bin'])
    const repairProjectionPresent = session.repairProjection !== undefined
    const unexpectedLookupCount = interceptor.calls.filter(call => !allowedNames.has(call.name)).length
    const outputNames = Object.freeze(await entryNames(root))
    interceptor.restore()
    interceptor = undefined
    await session.close()
    session = undefined
    return Object.freeze({
      repairFactoryCalls,
      ledgerOpens,
      repairProjectionPresent,
      projectionActivationCount,
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

export interface BindTaskOptions {
  readonly prepareCompatibleNameRootRepair?: CompatibleNameRootRepairFactory
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
}

export async function bindTask(
  fixture: FsaNamespaceFixture,
  parent: FileSystemDirectoryHandle,
  repository: IndexedDbReceiveOperationRepository,
  artifact: DirectoryTreeArtifact,
  seed: number,
  activate = true,
  options: BindTaskOptions = {},
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
    ...(options.prepareCompatibleNameRootRepair === undefined
      ? {}
      : { prepareCompatibleNameRootRepair: options.prepareCompatibleNameRootRepair }),
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
  if (reserved.compatibleNameRepair === undefined) {
    await repository.commitTransition({ operationId: intent.operationId, ...prepared.transition })
  } else {
    await repository.commitFSACompatibleNameBootstrap({
      transition: { operationId: intent.operationId, ...prepared.transition },
      bootstrap: reserved.compatibleNameRepair.bootstrap,
    })
  }
  const binding = await verifyFSAOperationBinding({ repository, intent, expectedParent: parent })
  const openCompatibleNameLedger = options.openCompatibleNameLedger ??
    (() => IndexedDbCompatibleNameLedger.open(fixture.databaseName))
  const compatibleNameCoordinator = reserved.compatibleNameRepair === undefined
    ? undefined
    : await CompatibleNameCoordinator.activate({
        prepared: reserved.compatibleNameRepair,
        mutations: rootLease.authority,
        pairHandles: repository,
        openLedger: openCompatibleNameLedger,
      })
  const session = await assembleNewFileSystemAccessOutput({
    binding,
    operationRepository: repository,
    rootLease,
    databaseName: fixture.databaseName,
    checkpointRepositoryFactory: binding =>
      IndexedDbFileCheckpointRepository.open(binding, fixture.databaseName),
    openCompatibleNameLedger,
    compatibleNamePreparation: compatibleNamePreparation(),
    ...(compatibleNameCoordinator === undefined ? {} : { compatibleNameCoordinator }),
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
  readonly activeZeroSidecarsBefore: number
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
    const activeZeroSidecarsBefore = await countActiveZeroSidecars(receiver)
    const rejected = !rejectionConsumed && options?.create !== true &&
      input.rejection?.kind === kind
    if (rejected) rejectionConsumed = true
    calls.push(Object.freeze({
      kind,
      name,
      create: options?.create === true,
      rejected,
      entriesBefore,
      activeZeroSidecarsBefore,
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

async function countActiveZeroSidecars(parent: FileSystemDirectoryHandle): Promise<number> {
  let count = 0
  for await (const handle of parent.values()) {
    if (handle.kind !== 'file') continue
    try {
      const checkpoint = decodeCompatibleNameSidecar(new Uint8Array(
        await (await (handle as FileSystemFileHandle).getFile()).arrayBuffer(),
      ))
      if (checkpoint.footer.state === 'active' && checkpoint.footer.committedCount === 0 &&
          checkpoint.trailingByteLength === 0) count += 1
    } catch {
      // The immutable script and ordinary files are intentionally not sidecar candidates.
    }
  }
  return count
}

async function readCompatibleOperation(fixture: FsaNamespaceFixture, operationId: string) {
  const ledger = await IndexedDbCompatibleNameLedger.open(fixture.databaseName)
  try {
    const snapshot = await ledger.loadOperation(operationId)
    if (snapshot === undefined) throw new TypeError('compatible-name operation is unavailable')
    return snapshot
  } finally {
    ledger.close()
  }
}

async function readCompatibleSidecar(parent: FileSystemDirectoryHandle, name: string) {
  const handle = await parent.getFileHandle(name)
  return decodeCompatibleNameSidecar(new Uint8Array(await (await handle.getFile()).arrayBuffer()))
}

async function waitForCompatibleProjection(
  session: FileSystemAccessOutputSession,
  committedCount: number,
): Promise<void> {
  const source = session.repairProjection
  if (source === undefined) throw new TypeError('compatible-name repair projection is unavailable')
  let unsubscribe: (() => void) | undefined
  const reached = new Promise<void>(resolve => {
    unsubscribe = source.subscribe(summary => {
      if (summary.committedCount === committedCount && summary.sidecarSync === 'current' &&
          summary.latestObservedFooter?.state === 'active' &&
          summary.latestObservedFooter.committedCount === committedCount) resolve()
    })
  })
  try {
    await reached
  } finally {
    unsubscribe?.()
  }
}

function compatibleNamePreparation() {
  return Object.freeze({
    platform: 'windows',
    randomBits: () => 0,
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
  const lease = await scheduler.acquireWriter(parent)
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

function parentIdentity(description: string): FSAParentMutationIdentity {
  return Symbol(description) as FSAParentMutationIdentity
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
