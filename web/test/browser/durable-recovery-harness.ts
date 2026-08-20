import type { ReceiveIntent } from '../../src/transfer/intent'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import {
  openOriginPrivateRetainedArtifactBackend,
  openOriginPrivateWorkspaceBackend,
} from '../../src/output/origin-private/session'
import {
  openOriginPrivateWorkspaceNamespace,
  originPrivateWorkspaceRootHandleId,
} from '../../src/output/origin-private/namespace'
import { OriginPrivatePackageWorkflow } from '../../src/output/origin-private/workflow'
import type { PackagedArtifactV1 } from '../../src/output/workspace/aggregate'
import { createSingleFileWorkspaceBudget } from '../../src/output/workspace/budget'
import { recoverAbandonedOperation } from '../../src/output/workspace/recovery'
import { recoverWorkspaceActivationCandidates } from '../../src/output/workspace/activation-recovery'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import {
  createBrowserReceiveOperationMutationPort,
} from '../../src/output/resume/reopen-authority'
import {
  WorkspaceOperationStages,
  type WorkspaceStageTraceEvent,
} from '../../src/output/workspace/stages'
import {
  DURABLE_FIXTURE_CAPACITY_BYTES,
  DURABLE_FIXTURE_INITIAL_TIME,
  durableIdentities,
  durableIntent,
  originPrivateClaim,
  readDurableLifecycle,
  workspaceBudgetAuthority,
} from './durable-output-fixture'

const FILE_BYTES = Uint8Array.of(1, 2, 3, 4, 5)
const CHECKPOINT_PREFIX_BYTES = 3
const FRESH_PAGE_RECOVERY_TIME = 1_500
const RECOVERY_TIME = 3_000
const RETRY_TIME = 4_000
const ACTIVE_SIGNAL = new AbortController().signal

export interface DurableReceiveFixture {
  readonly key: string
  readonly checkpointDatabaseName: string
  readonly admissionDatabaseName: string
  readonly durableMetadataBytes: string
}

export interface DurablePackageFixture extends DurableReceiveFixture {
  readonly rawOwnedObjectId: string
  readonly package: Readonly<{
    sealedMaterializationDigest: string
    artifactSpecDigest: string
    packageOwnedObjectId: string
    exactBytes: string
    artifactReceiptDigest: string
    layoutDigest: string
    digest: string
  }>
  readonly originalExpiry: number
}

export interface FreshPageWorkspaceResumeFixture {
  readonly key: string
  readonly checkpointDatabaseName: string
  readonly admissionDatabaseName: string
}

export interface FreshPageWorkspaceResumeCut {
  readonly fixture: FreshPageWorkspaceResumeFixture
  readonly lifecycle: string
}

export interface FreshPageWorkspaceResumeProof {
  readonly lifecycle: string
  readonly admittedContentReopened: boolean
  readonly cleanup: string
}

export interface WorkspaceActivationReloadCut {
  readonly key: string
  readonly databaseName: string
  readonly operationId: string
  readonly candidateCount: number
}

export interface WorkspaceActivationReloadProof {
  readonly candidateCount: number
  readonly promotedHandlePresent: boolean
  readonly lifecycle: string
  readonly retainedContinuation: string | null
}

export interface ReceiveCrashCutResult {
  readonly fixture: DurableReceiveFixture
  readonly ranges: readonly string[]
  readonly lifecycle: string
  readonly contentRequests: string
}

export interface RecoveredPackageResult {
  readonly fixture: DurablePackageFixture
  readonly recoveredRanges: readonly string[]
  readonly packageBytes: readonly number[]
  readonly recoveryDecision: string
  readonly lifecycle: string
  readonly contentRequests: string
  readonly packageSeals: number
  readonly publicationAttempts: number
}

export interface PublicationRetryResult {
  readonly packageBytes: readonly number[]
  readonly packageDigest: string
  readonly originalExpiry: number
  readonly restoredExpiry: number
  readonly contentRequests: string
  readonly packageSeals: number
  readonly publicationAttempts: number
  readonly cleanup: string
}

export async function createWorkspaceActivationReloadCut(
  key: string,
): Promise<WorkspaceActivationReloadCut> {
  const ids = await durableIdentities(key)
  const intent = await durableIntent(ids)
  const databaseName = `windshare-workspace-activation-${key}`
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  let markerCutReached = false
  try {
    await openOriginPrivateWorkspaceNamespace({
      receiveIntent: intent,
      repository,
      onActivationTransition: transition => {
        if (transition !== 'marker-written') return
        markerCutReached = true
        throw new Error('simulated page loss after durable workspace marker')
      },
    })
  } catch {
    // The marker cut deliberately interrupts before handle promotion.
  }
  if (!markerCutReached) {
    repository.close()
    throw new TypeError('workspace activation reload cut did not reach the marker boundary')
  }
  const candidateCount = (await repository.listWorkspaceActivationCandidates()).length
  repository.close()
  return Object.freeze({ key, databaseName, operationId: intent.operationId, candidateCount })
}

export async function recoverWorkspaceActivationReloadCut(
  cut: WorkspaceActivationReloadCut,
): Promise<WorkspaceActivationReloadProof> {
  const repository = await IndexedDbReceiveOperationRepository.open(cut.databaseName)
  try {
    await recoverWorkspaceActivationCandidates({ repository })
    const [candidates, root, lifecycleRecord] = await Promise.all([
      repository.listWorkspaceActivationCandidates(),
      repository.readHandle(originPrivateWorkspaceRootHandleId(cut.operationId)),
      repository.readLifecycle(cut.operationId),
    ])
    if (lifecycleRecord === undefined) {
      throw new TypeError('promoted workspace activation omitted its lifecycle')
    }
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    return Object.freeze({
      candidateCount: candidates.length,
      promotedHandlePresent: root !== undefined,
      lifecycle: lifecycle.kind,
      retainedContinuation: receiveOperationResumeDescriptor(lifecycle, Date.now())?.continuation ?? null,
    })
  } finally {
    repository.close()
  }
}

export async function createFreshPageWorkspaceResumeCut(
  key: string,
): Promise<FreshPageWorkspaceResumeCut> {
  const ids = await durableIdentities(key)
  const intent = await durableIntent(ids)
  const checkpointDatabaseName = `windshare-w3c-resume-${key}`
  const admissionDatabaseName = `windshare-w3c-resume-admission-${key}`
  const repository = await IndexedDbReceiveOperationRepository.open(checkpointDatabaseName)
  const namespace = await openOriginPrivateWorkspaceNamespace({
    receiveIntent: intent,
    repository,
    randomOwnedObjectId: () => ids.rootOwnedObjectId,
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    clock: { now: () => DURABLE_FIXTURE_INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x51),
  })
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => DURABLE_FIXTURE_INITIAL_TIME,
    contentRequests: { count: () => 0n },
  })
  const rejectionBackend = await openOriginPrivateRetainedArtifactBackend({
    receiveIntent: intent,
    operationRepository: repository,
    namespace,
    checkpointDatabaseName,
  })
  const authority = await workspaceBudgetAuthority({
    operationId: intent.operationId,
    databaseName: admissionDatabaseName,
    now: DURABLE_FIXTURE_INITIAL_TIME,
    token: `${key}-resume-cut`,
  })
  const admission = await stages.admitSingleFile({
    fileId: ids.fileId,
    containingDirectoryId: ids.directoryId,
    generation: ids.generation,
    catalogSize: BigInt(FILE_BYTES.byteLength),
    authority,
    durableMetadataBytesExcludingAdmissionRecords: 0n,
    rejectionCleanup: await rejectionBackend.cleanup.cleanupRequest(),
  })
  await rejectionBackend.close()
  if (admission.kind !== 'accepted') {
    throw new Error(`fresh-page workspace admission failed: ${admission.reason}`)
  }
  const claim = originPrivateClaim(admission.content.claim)
  const paused = await stages.pauseReceive({
    checkpointSetDigest: ids.expiryReceiptDigest,
    completedFileCount: 0n,
    completedBytes: 0n,
  })
  ;(globalThis as Record<string, unknown>).__windshareW3cFreshResumeCut = Object.freeze({
    repository,
    lease,
    claim,
  })
  return Object.freeze({
    fixture: Object.freeze({ key, checkpointDatabaseName, admissionDatabaseName }),
    lifecycle: paused.kind,
  })
}

export async function reopenFreshPageWorkspaceResume(
  fixture: FreshPageWorkspaceResumeFixture,
): Promise<FreshPageWorkspaceResumeProof> {
  const descriptorRepository = await IndexedDbReceiveOperationRepository.open(
    fixture.checkpointDatabaseName,
  )
  const ids = await durableIdentities(fixture.key)
  const intent = await durableIntent(ids)
  const stable = await readDurableLifecycle(descriptorRepository, intent.operationId)
  const descriptor = receiveOperationResumeDescriptor(stable, FRESH_PAGE_RECOVERY_TIME)
  descriptorRepository.close()
  if (descriptor === undefined || descriptor.continuation !== 'resume-receive') {
    throw new TypeError('fresh-page workspace did not retain resume authority')
  }

  const mutations = createBrowserReceiveOperationMutationPort({
    checkpointDatabaseName: fixture.checkpointDatabaseName,
    clock: { now: () => FRESH_PAGE_RECOVERY_TIME },
    leaseOptions: {
      clock: { now: () => FRESH_PAGE_RECOVERY_TIME },
      randomBytes: () => new Uint8Array(16).fill(0x61),
    },
    workspaceBudgetDatabaseName: fixture.admissionDatabaseName,
    estimateWorkspaceStorage: async () => ({
      usage: 0,
      quota: Number(DURABLE_FIXTURE_CAPACITY_BYTES),
    }),
  })
  const mutation = await mutations.resume(descriptor)
  if (mutation.kind !== 'continuation' ||
      mutation.continuation.kind !== 'workspace-receive') {
    throw new TypeError('fresh-page workspace resume did not return its operation authority')
  }
  const reopened = mutation.continuation.operation
  const lifecycle = reopened.lifecycle.kind
  let backend: Awaited<ReturnType<typeof openOriginPrivateWorkspaceBackend>> | undefined
  try {
    backend = await reopened.receiveContinuation.openBackend()
    const cleanup = await reopened.stages.discard(await backend.cleanup.cleanupRequest())
    return Object.freeze({
      lifecycle,
      admittedContentReopened: true,
      cleanup: cleanup.kind,
    })
  } finally {
    await backend?.close().catch(() => undefined)
    await reopened.close().catch(() => undefined)
    await Promise.all([
      deleteDatabase(fixture.checkpointDatabaseName),
      deleteDatabase(fixture.admissionDatabaseName),
    ])
  }
}

export async function createOriginPrivateReceiveCrashCut(
  key: string,
): Promise<ReceiveCrashCutResult> {
  const ids = await durableIdentities(key)
  const intent = await durableIntent(ids)
  const checkpointDatabaseName = `windshare-w3c-${key}`
  const admissionDatabaseName = `windshare-w3c-admission-${key}`
  const repository = await IndexedDbReceiveOperationRepository.open(checkpointDatabaseName)
  const namespace = await openOriginPrivateWorkspaceNamespace({
    receiveIntent: intent,
    repository,
    randomOwnedObjectId: () => ids.rootOwnedObjectId,
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    clock: { now: () => DURABLE_FIXTURE_INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x21),
  })
  let contentRequests = 0n
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => DURABLE_FIXTURE_INITIAL_TIME,
    contentRequests: { count: () => contentRequests },
  })
  const rejectionBackend = await openOriginPrivateRetainedArtifactBackend({
    receiveIntent: intent,
    operationRepository: repository,
    namespace,
    checkpointDatabaseName,
  })
  const authority = await workspaceBudgetAuthority({
    operationId: intent.operationId,
    databaseName: admissionDatabaseName,
    now: DURABLE_FIXTURE_INITIAL_TIME,
    token: `${key}-initial`,
  })
  const admission = await stages.admitSingleFile({
    fileId: ids.fileId,
    containingDirectoryId: ids.directoryId,
    generation: ids.generation,
    catalogSize: BigInt(FILE_BYTES.byteLength),
    authority,
    durableMetadataBytesExcludingAdmissionRecords: 0n,
    rejectionCleanup: await rejectionBackend.cleanup.cleanupRequest(),
  })
  await rejectionBackend.close()
  if (admission.kind !== 'accepted') throw new Error(`test workspace admission failed: ${admission.reason}`)
  const claim = originPrivateClaim(admission.content.claim)
  const backend = await openOriginPrivateWorkspaceBackend({
    receiveIntent: intent,
    operationRepository: repository,
    namespace,
    contentGate: admission.content.gate,
    budgetClaim: claim,
    checkpointDatabaseName,
  })
  const transaction = await backend.materialization.beginFile({
    artifactPath: [intent.artifact.suggestedName],
    openRevision: async () => {
      contentRequests += 1n
      return Object.freeze({
        fileId: ids.fileId,
        fileRevision: ids.fileRevision,
        exactSize: BigInt(FILE_BYTES.byteLength),
      })
    },
  })
  await transaction.writeRange(0n, FILE_BYTES.subarray(0, CHECKPOINT_PREFIX_BYTES), ACTIVE_SIGNAL)
  const ranges = await transaction.checkpoint(ACTIVE_SIGNAL)
  await transaction.close()
  const lifecycle = await readDurableLifecycle(repository, intent.operationId)
  ;(globalThis as Record<string, unknown>).__windshareW3cCrashCut = {
    repository,
    backend,
    lease,
    claim,
  }
  return Object.freeze({
    fixture: Object.freeze({
      key,
      checkpointDatabaseName,
      admissionDatabaseName,
      durableMetadataBytes: admission.content.budget.durableMetadataBytes.toString(),
    }),
    ranges: ranges.map(rangeText),
    lifecycle: lifecycle.kind,
    contentRequests: contentRequests.toString(),
  })
}

export async function recoverReceiveAndSealPackage(
  fixture: DurableReceiveFixture,
): Promise<RecoveredPackageResult> {
  const ids = await durableIdentities(fixture.key)
  const intent = await durableIntent(ids)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.checkpointDatabaseName)
  const namespace = await openOriginPrivateWorkspaceNamespace({ receiveIntent: intent, repository })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    clock: { now: () => RECOVERY_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x31),
  })
  const budget = await createSingleFileWorkspaceBudget({
    receiveIntent: intent,
    fileId: ids.fileId,
    containingDirectoryId: ids.directoryId,
    generation: ids.generation,
    catalogSize: BigInt(FILE_BYTES.byteLength),
    durableMetadataBytes: BigInt(fixture.durableMetadataBytes),
  })
  const authority = await workspaceBudgetAuthority({
    operationId: intent.operationId,
    databaseName: fixture.admissionDatabaseName,
    now: RECOVERY_TIME,
    token: `${fixture.key}-recovered`,
  })
  const claimResult = await authority.claim(budget)
  if (claimResult.kind !== 'accepted') throw new Error('recovered workspace budget was rejected')
  const claim = originPrivateClaim(claimResult.claim)
  let contentRequests = 0n
  const traces: WorkspaceStageTraceEvent[] = []
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => RECOVERY_TIME,
    contentRequests: { count: () => contentRequests },
    onTrace: (event) => traces.push(event),
  })
  const reopened = await stages.reopenAdmittedContent({ budget, claim })
  const backend = await openOriginPrivateWorkspaceBackend({
    receiveIntent: intent,
    operationRepository: repository,
    namespace,
    contentGate: reopened.gate,
    budgetClaim: claim,
    checkpointDatabaseName: fixture.checkpointDatabaseName,
  })
  const transaction = await backend.materialization.beginFile({
    artifactPath: [intent.artifact.suggestedName],
    openRevision: async () => {
      contentRequests += 1n
      return Object.freeze({
        fileId: ids.fileId,
        fileRevision: ids.fileRevision,
        exactSize: BigInt(FILE_BYTES.byteLength),
      })
    },
  })
  const recoveredRanges = transaction.verifiedRanges.map(rangeText)
  const checkpointRepository = await import('../../src/output/browser/indexeddb-repository')
    .then((module) => module.IndexedDbFileCheckpointRepository.open(
      checkpointBinding(intent),
      fixture.checkpointDatabaseName,
    ))
  const committed = await checkpointRepository.scanCommitted({ direction: 'ascending' })
  checkpointRepository.close()
  const checkpoint = committed.records[0]
  if (checkpoint === undefined || committed.records.length !== 1) {
    throw new Error('recovered workspace lacks its unique checkpoint')
  }
  const observed = await readDurableLifecycle(repository, intent.operationId)
  const recovery = recoverAbandonedOperation(observed, {
    kind: 'verified-receive',
    checkpointSetDigest: checkpoint.checksum,
    completedFileCount: 0n,
    completedBytes: 0n,
    lastVerifiedRecordDigest: checkpoint.checksum,
  }, {
    planKind: 'workspace-then-publish',
    nowMilliseconds: RECOVERY_TIME,
    expiryReceiptDigest: ids.expiryReceiptDigest,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: observed.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: recovery.state,
  })
  await stages.resumeReceive()
  await transaction.writeRange(
    BigInt(CHECKPOINT_PREFIX_BYTES),
    FILE_BYTES.subarray(CHECKPOINT_PREFIX_BYTES),
    ACTIVE_SIGNAL,
  )
  const proof = await transaction.commit(ACTIVE_SIGNAL)
  await transaction.close()
  const sealed = await stages.sealMaterialization({
    transferJobId: ids.transferJobId,
    generations: [{ directoryId: ids.directoryId, generation: ids.generation }],
    entries: [{
      kind: 'file',
      artifactPath: [intent.artifact.suggestedName],
      fileId: proof.fileId,
      fileRevision: proof.fileRevision,
      exactSize: proof.exactSize,
      ownedObjectId: proof.ownedObjectId,
      checkpoint: {
        recordId: proof.recordId,
        recordDigest: proof.recordDigest,
        checkpointGeneration: proof.checkpointGeneration,
      },
    }],
    checkpoints: backend.finalCheckpoints,
  })
  const workflow = new OriginPrivatePackageWorkflow({ stages, store: backend.packages })
  const packaged = await workflow.buildOriginalFile({
    receiveIntentDigest: intent.digest,
    artifactSpecDigest: intent.artifact.digest,
    sealedMaterialization: sealed.seal,
    materializedManifest: sealed.manifest,
    signal: ACTIVE_SIGNAL,
  })
  if (packaged.kind !== 'sealed') throw new Error(`package did not seal: ${packaged.kind}`)
  const packageBlob = await backend.packagedArtifacts.readPackagedArtifact(packaged.package)
  const attempt = await stages.startHandoff({
    package: packaged.package,
    publicationAttemptId: ids.firstPublicationAttemptId,
    suggestedName: intent.artifact.suggestedName,
    packagedFileSupported: true,
  })
  const waiting = await stages.recordHandoffNotStarted({
    package: packaged.package,
    attempt,
    reason: 'user-cancelled',
  })
  if (waiting.kind !== 'waiting-to-save') throw new Error('handoff cancellation lost WaitingToSave')
  await backend.close()
  await claim.release()
  await lease.release()
  repository.close()

  return Object.freeze({
    fixture: Object.freeze({
      ...fixture,
      rawOwnedObjectId: proof.ownedObjectId,
      package: serializedPackage(packaged.package),
      originalExpiry: waiting.expiresAt,
    }),
    recoveredRanges,
    packageBytes: Object.freeze([...new Uint8Array(await packageBlob.arrayBuffer())]),
    recoveryDecision: recovery.decision,
    lifecycle: waiting.kind,
    contentRequests: contentRequests.toString(),
    packageSeals: traces.filter((event) => event.name === 'receive.package.sealed').length,
    publicationAttempts: traces.filter((event) => event.name === 'receive.publication.started').length,
  })
}

export async function retryRetainedPackagePublication(
  fixture: DurablePackageFixture,
): Promise<PublicationRetryResult> {
  const ids = await durableIdentities(fixture.key)
  const intent = await durableIntent(ids)
  const repository = await durableStep('open operation repository', () =>
    IndexedDbReceiveOperationRepository.open(fixture.checkpointDatabaseName))
  const namespace = await durableStep('open retained OPFS namespace', () =>
    openOriginPrivateWorkspaceNamespace({ receiveIntent: intent, repository }))
  const lease = await durableStep('acquire retained operation lease', () =>
    acquireBrowserReceiveOperationLease(repository, intent.operationId, {
      clock: { now: () => RETRY_TIME },
      randomBytes: () => new Uint8Array(16).fill(0x41),
    }))
  const contentRequests = 0n
  const traces: WorkspaceStageTraceEvent[] = []
  const stages = await durableStep('open retained operation stages', () => WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => RETRY_TIME,
    contentRequests: { count: () => contentRequests },
    onTrace: (event) => traces.push(event),
  }))
  const backend = await durableStep('open retained artifact backend', () =>
    openOriginPrivateRetainedArtifactBackend({
    receiveIntent: intent,
    operationRepository: repository,
    namespace,
    checkpointDatabaseName: fixture.checkpointDatabaseName,
    }))
  const artifact = await durableStep('restore package authority', () => stages.readRetainedPackage())
  if (artifact.digest !== fixture.package.digest) {
    throw new TypeError('repository package authority changed across reload')
  }
  const blob = await durableStep('read retained package artifact', () =>
    backend.packagedArtifacts.readPackagedArtifact(artifact))
  const packageBytes = await durableStep('consume retained package artifact', async () =>
    Object.freeze([...new Uint8Array(await blob.arrayBuffer())]))
  const attempt = await durableStep('start retained package handoff', () => stages.startHandoff({
    package: artifact,
    publicationAttemptId: ids.secondPublicationAttemptId,
    suggestedName: intent.artifact.suggestedName,
    packagedFileSupported: true,
  }))
  const waiting = await durableStep('record retained handoff rejection', () =>
    stages.recordHandoffNotStarted({
    package: artifact,
    attempt,
    reason: 'target-unavailable',
    }))
  if (waiting.kind !== 'waiting-to-save') throw new Error('publication retry lost WaitingToSave')
  const cleanupRequest = await durableStep(
    'derive retained cleanup inventory',
    () => backend.cleanup.cleanupRequest(),
  )
  const cleanup = await durableStep(
    'discard retained workspace',
    () => stages.discard(cleanupRequest),
  )
  await durableStep('close retained artifact backend', () => backend.close())
  await durableStep('release retained operation lease', () => lease.release())
  repository.close()
  await durableStep('delete durable test databases', () => Promise.all([
    deleteDatabase(fixture.checkpointDatabaseName),
    deleteDatabase(fixture.admissionDatabaseName),
  ]).then(() => undefined))
  return Object.freeze({
    packageBytes,
    packageDigest: artifact.digest,
    originalExpiry: fixture.originalExpiry,
    restoredExpiry: waiting.expiresAt,
    contentRequests: contentRequests.toString(),
    packageSeals: traces.filter((event) => event.name === 'receive.package.sealed').length,
    publicationAttempts: traces.filter((event) => event.name === 'receive.publication.started').length,
    cleanup: cleanup.kind,
  })
}

function checkpointBinding(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'workspace-then-publish') throw new TypeError('test intent is not workspace')
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: 3 as const,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
}

function serializedPackage(artifact: PackagedArtifactV1): DurablePackageFixture['package'] {
  return Object.freeze({
    sealedMaterializationDigest: artifact.sealedMaterializationDigest,
    artifactSpecDigest: artifact.artifactSpecDigest,
    packageOwnedObjectId: artifact.packageOwnedObjectId,
    exactBytes: artifact.exactBytes.toString(),
    artifactReceiptDigest: artifact.artifactReceiptDigest,
    layoutDigest: artifact.layoutDigest,
    digest: artifact.digest,
  })
}

function rangeText(range: { readonly start: bigint; readonly end: bigint }): string {
  return `${range.start}:${range.end}`
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('blocked', () => reject(
      new DOMException('test database deletion was blocked', 'InvalidStateError'),
    ), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

async function durableStep<T>(label: string, operation: () => Promise<T>): Promise<T> {
  try {
    return await operation()
  } catch (error) {
    const detail = error instanceof Error ? `${error.name}: ${error.message}` : String(error)
    throw new Error(`${label}: ${detail}`, { cause: error })
  }
}
