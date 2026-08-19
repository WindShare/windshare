import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  bindMaterialization,
  offerArtifacts,
  type SelectionProjectionV1,
} from '../../src/output/planning'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import { TransferJob } from '../../src/transfer/v2-job'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type {
  ExactPreparationEvidence,
  V2PlanExecutionAuthority,
} from '../../src/transfer/output-session'
import {
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  type OriginalFileArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../src/output/origin-private/admission'
import {
  openOriginPrivateRetainedArtifactBackend,
  openOriginPrivateWorkspaceBackend,
} from '../../src/output/origin-private/session'
import {
  openOriginPrivateWorkspaceNamespace,
} from '../../src/output/origin-private/namespace'
import { OriginPrivatePackageWorkflow } from '../../src/output/origin-private/workflow'
import type { PackagedArtifactV1 } from '../../src/output/workspace/aggregate'
import { createSingleFileWorkspaceBudget } from '../../src/output/workspace/budget'
import { sealWorkspaceZipPreparation } from '../../src/output/workspace/preparation'
import { RECEIVE_RECORD_RECEIPT } from '../../src/output/workspace/records'
import { recoverAbandonedOperation } from '../../src/output/workspace/recovery'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import {
  createBrowserReceiveOperationMutationPort,
} from '../../src/output/resume/reopen-authority'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from '../../src/ui/v2-browser-receive-composition'
import {
  WORKSPACE_HANDLE_ZIP_LAYOUT,
  WorkspaceOperationStages,
  type WorkspaceStageTraceEvent,
  workspaceZipLayoutHandleId,
} from '../../src/output/workspace/stages'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity as catalogIdentity,
  identityText as catalogIdentityText,
  readerFixture,
} from '../transfer/v2-job-fixture'

const FILE_BYTES = Uint8Array.of(1, 2, 3, 4, 5)
const PRODUCT_ZIP_FILE_BYTES = 68n
const PRODUCT_FIXTURE_CHUNK_BYTES = 2
const PRODUCT_FIXTURE_LANE_COUNT = 1
const EMPTY_MATERIALIZATION_SUMMARY = Object.freeze({ entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n })
const CHECKPOINT_PREFIX_BYTES = 3
const INITIAL_TIME = 1_000
const FRESH_PAGE_RECOVERY_TIME = 1_500
const RECOVERY_TIME = 3_000
const RETRY_TIME = 4_000
const CLAIM_LEASE_MILLISECONDS = 1_000
const CLAIM_HEARTBEAT_MILLISECONDS = 500
const CAPACITY_BYTES = 1_000_000_000n
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

export interface FreshPreparedZipAdmissionProof {
  readonly lifecycle: string
  readonly contentRequests: string
  readonly traceNames: readonly string[]
  readonly receiptCount: number
  readonly manifestPageCount: number
  readonly layoutHandlePresent: boolean
}

export interface ProductPreparedZipAdmissionProof {
  readonly admission: string
  readonly lifecycle: string
  readonly traceNames: readonly string[]
  readonly cleanup: string
}

export interface TransferJobPreparedZipProof {
  readonly worker: string
  readonly lifecycle: string
  readonly workspaceTraceNames: readonly string[]
  readonly transferTraceNames: readonly string[]
  readonly evidence: Readonly<{
    readonly entryCount: string
    readonly fileCount: string
    readonly directoryCount: string
    readonly selectedRawBytes: string
    readonly generationCount: number
    readonly entries: readonly Readonly<{
      readonly kind: 'directory' | 'file'
      readonly role?: string
      readonly sourceSegmentCount: number
      readonly artifactSegmentCount: number
      readonly modifiedTimePresent: boolean
    }>[]
  }>
  readonly cleanup: string
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

export async function proveFreshPreparedZipAdmission(
  key: string,
): Promise<FreshPreparedZipAdmissionProof> {
  const ids = await durableIdentities(key)
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const workspace = await createWorkspaceBinding({
    operationId: ids.operationId,
    workspaceId: ids.workspaceId,
    artifact,
    repositoryRef: ids.repositoryRef,
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: ids.shareInstance,
      syntheticRoot: ids.syntheticRoot,
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  const repository = await IndexedDbReceiveOperationRepository.open()
  const namespace = await openOriginPrivateWorkspaceNamespace({
    receiveIntent: intent,
    repository,
    randomOwnedObjectId: () => ids.rootOwnedObjectId,
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    clock: { now: () => INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x41),
  })
  const traces: WorkspaceStageTraceEvent[] = []
  let contentRequests = 0n
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => INITIAL_TIME,
    contentRequests: { count: () => contentRequests },
    onTrace: (event) => traces.push(event),
  })
  let cleanupBackend: Awaited<ReturnType<
    typeof openOriginPrivateRetainedArtifactBackend
  >> | undefined
  let claim: OriginPrivateWorkspaceBudgetClaim | undefined
  try {
    const preparationId = await identity(key, 'preparation', 16)
    const rootGeneration = await identity(key, 'root-generation', 16)
    const modifiedTime = Object.freeze({
      seconds: 1_700_000_000n,
      nanoseconds: 123_000_000,
      precision: 3 as const,
      milliseconds: 1_700_000_000_123n,
    })
    await stages.beginReceive(preparationId)
    const preparation = await sealWorkspaceZipPreparation({
      receiveIntent: intent,
      preparationId,
      generations: [
        {
          directoryId: ids.syntheticRoot,
          generation: rootGeneration,
        },
        {
          directoryId: ids.directoryId,
          generation: ids.generation,
        },
      ],
      entries: [
        {
          kind: 'directory',
          sourcePath: [],
          artifactPath: [artifact.layout.name],
          directoryId: ids.syntheticRoot,
          generation: rootGeneration,
          role: 'result-root',
        },
        {
          kind: 'directory',
          sourcePath: ['micro-share'],
          artifactPath: [artifact.layout.name, 'micro-share'],
          directoryId: ids.directoryId,
          generation: ids.generation,
          role: 'necessary-ancestor',
          modifiedTime,
        },
        {
          kind: 'file',
          sourcePath: ['micro-share', 'pixel.png'],
          artifactPath: [artifact.layout.name, 'micro-share', 'pixel.png'],
          fileId: ids.fileId,
          containingDirectoryId: ids.directoryId,
          generation: ids.generation,
          exactSize: PRODUCT_ZIP_FILE_BYTES,
          modifiedTime,
        },
      ],
    })
    const authority = await OriginPrivateWorkspaceBudgetAuthority.open(intent.operationId, {
      estimate: () => navigator.storage.estimate(),
    })
    const admission = await stages.admitPreparedZip({
      preparation,
      authority,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: {
        targets: Object.freeze([]),
        port: Object.freeze({
          removeOwnedObject: async () => Object.freeze({ kind: 'ownership-unknown' as const }),
          removeFileCheckpoints: async () => Object.freeze({
            kind: 'ownership-unknown' as const,
          }),
        }),
      },
    })
    if (admission.kind !== 'accepted') {
      throw new DOMException(
        `prepared ZIP admission rejected: ${admission.reason}`,
        'QuotaExceededError',
      )
    }
    claim = originPrivateClaim(admission.content.claim)
    cleanupBackend = await openOriginPrivateRetainedArtifactBackend({
      receiveIntent: intent,
      operationRepository: repository,
      namespace,
    })
    const [lifecycle, receipts, pages, layoutHandle] = await Promise.all([
      readLifecycle(repository, intent.operationId),
      repository.listRecords(intent.operationId, RECEIVE_RECORD_RECEIPT),
      repository.listManifestPages(intent.operationId),
      repository.readHandle(workspaceZipLayoutHandleId(intent.operationId, preparationId)),
    ])
    if (layoutHandle === undefined || layoutHandle.kind !== WORKSPACE_HANDLE_ZIP_LAYOUT) {
      throw new Error('prepared ZIP admission omitted its durable layout handle')
    }
    return Object.freeze({
      lifecycle: lifecycle.kind,
      contentRequests: contentRequests.toString(),
      traceNames: Object.freeze(traces.map((event) => event.name)),
      receiptCount: receipts.length,
      manifestPageCount: pages.length,
      layoutHandlePresent: true,
    })
  } finally {
    await claim?.release().catch(() => undefined)
    if (cleanupBackend !== undefined) {
      await stages.discard(await cleanupBackend.cleanup.cleanupRequest()).catch(() => undefined)
      await cleanupBackend.close().catch(() => undefined)
    }
    await lease.release().catch(() => undefined)
    repository.close()
    contentRequests = 0n
  }
}

export async function proveProductPreparedZipAdmission(
  key: string,
): Promise<ProductPreparedZipAdmissionProof> {
  const ids = await durableIdentities(key)
  const selection = await createSelectionSpec({
    shareInstance: ids.shareInstance,
    syntheticRoot: ids.syntheticRoot,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projection = productZipProjection(selection.digest, ids.directoryId)
  const traces: WorkspaceStageTraceEvent[] = []
  const composition = createBrowserReceiveComposition(window as BrowserReceiveWindow, {
    onTrace: (event) => traces.push(event),
  })
  const signal = new AbortController().signal
  const environment = await composition.environment(signal)
  const offered = await offerArtifacts(
    projection,
    Object.freeze({ kind: 'complete' as const }),
    environment,
  )
  if (offered.kind !== 'artifact-actions') {
    throw new TypeError('product composition did not offer workspace ZIP')
  }
  const action = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.plan.kind === 'workspace-then-publish' && candidate.artifact?.kind === 'zip-archive')
  if (action === undefined) throw new TypeError('product composition omitted workspace ZIP')

  const started = await composition.startArtifactAuthority(action)
  let runtime: Awaited<ReturnType<typeof started.finalize>> | undefined
  try {
    runtime = await started.finalize(async acquired => {
      const bound = await bindMaterialization({
        selection,
        chosenAction: action,
        currentProjection: projection,
        currentDiscovery: Object.freeze({ kind: 'complete' as const }),
        currentEnvironment: environment,
        acquired,
      })
      if (bound.kind !== 'bound') throw new TypeError('product workspace ZIP did not freeze')
      return bound.intent
    }, signal)
    const intent = runtime.intent
    if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive') {
      throw new TypeError('product binding changed the workspace ZIP route')
    }
    const rootGeneration = await identity(key, 'product-root-generation', 16)
    const modifiedTime = Object.freeze({
      seconds: 1_700_000_000n,
      nanoseconds: 123_000_000,
      precision: 3 as const,
      milliseconds: 1_700_000_000_123n,
    })
    const workspaceIntent = intent as Parameters<
      typeof runtime.plans.prepareWorkspaceZip
    >[0]
    const admission = await runtime.plans.prepareWorkspaceZip(workspaceIntent, Object.freeze({
      generations: Object.freeze([
        Object.freeze({ directoryId: ids.syntheticRoot, generation: rootGeneration }),
        Object.freeze({ directoryId: ids.directoryId, generation: ids.generation }),
      ]),
      entries: Object.freeze([
        Object.freeze({
          kind: 'directory' as const,
          sourcePath: Object.freeze([]),
          artifactPath: Object.freeze([intent.artifact.layout.name]),
          directoryId: ids.syntheticRoot,
          generation: rootGeneration,
          role: 'result-root' as const,
        }),
        Object.freeze({
          kind: 'directory' as const,
          sourcePath: Object.freeze(['micro-share']),
          artifactPath: Object.freeze([intent.artifact.layout.name, 'micro-share']),
          directoryId: ids.directoryId,
          generation: ids.generation,
          role: 'necessary-ancestor' as const,
          modifiedTime,
        }),
        Object.freeze({
          kind: 'file' as const,
          sourcePath: Object.freeze(['micro-share', 'pixel.png']),
          artifactPath: Object.freeze([intent.artifact.layout.name, 'micro-share', 'pixel.png']),
          fileId: ids.fileId,
          containingDirectoryId: ids.directoryId,
          generation: ids.generation,
          exactSize: PRODUCT_ZIP_FILE_BYTES,
          modifiedTime,
        }),
      ]),
      entryCount: 3n,
      fileCount: 1n,
      directoryCount: 2n,
      selectedRawBytes: PRODUCT_ZIP_FILE_BYTES,
    }), signal)
    if (admission.kind !== 'accepted') {
      throw new DOMException('product workspace ZIP was rejected', 'QuotaExceededError')
    }
    const paused = await admission.execution.pause(Object.freeze({
      worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
      materialization: EMPTY_MATERIALIZATION_SUMMARY,
      reason: 'product admission proof completed',
    }), signal)
    const discarded = await runtime.startLifecycleAction('discard', paused)
    return Object.freeze({
      admission: admission.kind,
      lifecycle: 'receiving',
      traceNames: Object.freeze(traces.map(event => event.name)),
      cleanup: discarded.lifecycle.kind,
    })
  } finally {
    if (runtime !== undefined) {
      await Promise.resolve(runtime.detach()).catch(() => undefined)
    }
  }
}

export async function proveTransferJobPreparedZip(): Promise<TransferJobPreparedZipProof> {
  const selection = await createSelectionSpec({
    shareInstance: catalogIdentityText(1),
    syntheticRoot: catalogIdentityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const modifiedTime = Object.freeze({
    seconds: 1_700_000_000n,
    nanoseconds: 123_456_789,
    precision: 3 as const,
    milliseconds: 1_700_000_000_123n,
  })
  const directory = Object.freeze({
    ...directoryEntry(catalogIdentity(30), 'micro-share'),
    modifiedTime,
  })
  const file = Object.freeze({
    ...fileEntry(catalogIdentity(20), 'pixel.png', PRODUCT_ZIP_FILE_BYTES),
    modifiedTime,
  })
  const projection = productZipProjection(selection.digest, directory.idText)
  const workspaceTraces: WorkspaceStageTraceEvent[] = []
  const transferTraceNames: string[] = []
  const composition = createBrowserReceiveComposition(window as BrowserReceiveWindow, {
    onTrace: event => workspaceTraces.push(event),
  })
  const signal = new AbortController().signal
  const environment = await composition.environment(signal)
  const offered = await offerArtifacts(
    projection,
    Object.freeze({ kind: 'complete' as const }),
    environment,
  )
  if (offered.kind !== 'artifact-actions') {
    throw new TypeError('product composition did not offer workspace ZIP')
  }
  const action = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.plan.kind === 'workspace-then-publish' && candidate.artifact?.kind === 'zip-archive')
  if (action === undefined) throw new TypeError('product composition omitted workspace ZIP')

  const started = await composition.startArtifactAuthority(action)
  let runtime: Awaited<ReturnType<typeof started.finalize>> | undefined
  try {
    runtime = await started.finalize(async acquired => {
      const bound = await bindMaterialization({
        selection,
        chosenAction: action,
        currentProjection: projection,
        currentDiscovery: Object.freeze({ kind: 'complete' as const }),
        currentEnvironment: environment,
        acquired,
      })
      if (bound.kind !== 'bound') throw new TypeError('product workspace ZIP did not freeze')
      return bound.intent
    }, signal)
    const catalog = catalogFixture([
      {
        id: catalogIdentity(2),
        entries: [directory],
        generation: catalogIdentity(90),
      },
      {
        id: directory.id,
        entries: [file],
        generation: catalogIdentity(91),
      },
    ])
    const readers = readerFixture([file])
    let observedEvidence: ExactPreparationEvidence | undefined
    const prepareWorkspaceZip: V2PlanExecutionAuthority['prepareWorkspaceZip'] = async (
      intent,
      evidence,
      preparationSignal,
    ) => {
      observedEvidence = evidence
      return runtime!.plans.prepareWorkspaceZip(intent, evidence, preparationSignal)
    }
    const plans: V2PlanExecutionAuthority = Object.freeze({
      ...runtime.plans,
      prepareWorkspaceZip,
    })
    const result = await new TransferJob({
      descriptor: {
        shareInstance: catalogIdentity(1),
        syntheticRoot: catalogIdentity(2),
        syntheticRootId: catalogIdentityText(2),
        chunkSize: PRODUCT_FIXTURE_CHUNK_BYTES,
      } as never,
      catalog: catalog.catalog,
      selection: new V2SelectionPolicy(true),
      intent: runtime.intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      lanes: { size: PRODUCT_FIXTURE_LANE_COUNT },
      transferJobId: runtime.transferJobId,
      trace: {
        current: event => transferTraceNames.push(
          event.name === 'receive_transition' ? event.transition : event.name,
        ),
      },
    }).run(signal)
    const evidence = observedEvidence
    if (evidence === undefined) {
      throw new TypeError('TransferJob did not expose exact preparation evidence')
    }
    const discarded = await runtime.startLifecycleAction('discard', result.lifecycle)
    return Object.freeze({
      worker: result.worker.status,
      lifecycle: result.lifecycle.kind,
      workspaceTraceNames: Object.freeze(workspaceTraces.map(event => event.name)),
      transferTraceNames: Object.freeze(transferTraceNames),
      evidence: summarizePreparationEvidence(evidence),
      cleanup: discarded.lifecycle.kind,
    })
  } finally {
    if (runtime !== undefined) {
      await Promise.resolve(runtime.detach()).catch(() => undefined)
    }
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
    clock: { now: () => INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x51),
  })
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => INITIAL_TIME,
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
    now: INITIAL_TIME,
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
  const stable = await readLifecycle(descriptorRepository, intent.operationId)
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
    estimateWorkspaceStorage: async () => ({ usage: 0, quota: Number(CAPACITY_BYTES) }),
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
    clock: { now: () => INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x21),
  })
  let contentRequests = 0n
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => INITIAL_TIME,
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
    now: INITIAL_TIME,
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
  const lifecycle = await readLifecycle(repository, intent.operationId)
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
  const observed = await readLifecycle(repository, intent.operationId)
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

function summarizePreparationEvidence(
  evidence: ExactPreparationEvidence,
): TransferJobPreparedZipProof['evidence'] {
  return Object.freeze({
    entryCount: evidence.entryCount.toString(),
    fileCount: evidence.fileCount.toString(),
    directoryCount: evidence.directoryCount.toString(),
    selectedRawBytes: evidence.selectedRawBytes.toString(),
    generationCount: evidence.generations.length,
    entries: Object.freeze(evidence.entries.map(entry => Object.freeze({
      kind: entry.kind,
      ...(entry.kind === 'directory' ? { role: entry.role } : {}),
      sourceSegmentCount: entry.sourcePath.length,
      artifactSegmentCount: entry.artifactPath.length,
      modifiedTimePresent: entry.modifiedTime !== undefined,
    }))),
  })
}

function productZipProjection(
  selectionDigest: string,
  directoryId: string,
): SelectionProjectionV1 {
  const selectedRoot = Object.freeze({
    kind: 'directory' as const,
    directoryId,
    sourcePath: 'micro-share',
    portableName: 'micro-share',
  })
  return Object.freeze({
    version: 1 as const,
    epoch: nextProjectionEpoch(0n),
    selectionDigest,
    selectedRoots: Object.freeze([selectedRoot]),
    selectedRootCountLowerBound: 1,
    selectedRootsTruncated: false,
    generations: Object.freeze([]),
    metrics: Object.freeze({
      fileCountLowerBound: 1,
      directoryCountLowerBound: 1,
      byteCountLowerBound: PRODUCT_ZIP_FILE_BYTES,
    }),
    unsettledTargets: Object.freeze([]),
    proof: Object.freeze({
      kind: 'tree' as const,
      selectedRoots: Object.freeze([selectedRoot]),
      selectedRootCountLowerBound: 1,
      selectedRootsTruncated: false,
      layoutBasis: Object.freeze({ kind: 'synthetic-selection' as const }),
    }),
  })
}

type DurableIntent = ReceiveIntent & { readonly artifact: OriginalFileArtifact }

async function durableIntent(ids: DurableIdentities): Promise<DurableIntent> {
  const artifact = await createOriginalFileArtifact({
    fileId: ids.fileId,
    sourcePath: 'root/browser-file.bin',
    suggestedName: 'browser-file.bin',
  })
  const workspace = await createWorkspaceBinding({
    operationId: ids.operationId,
    workspaceId: ids.workspaceId,
    artifact,
    repositoryRef: ids.repositoryRef,
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: ids.shareInstance,
      syntheticRoot: ids.syntheticRoot,
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  if (intent.artifact.kind !== 'original-file') throw new TypeError('durable fixture artifact changed')
  return intent as DurableIntent
}

interface DurableIdentities {
  readonly operationId: string
  readonly workspaceId: string
  readonly repositoryRef: string
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly fileId: string
  readonly fileRevision: string
  readonly directoryId: string
  readonly generation: string
  readonly rootOwnedObjectId: string
  readonly transferJobId: string
  readonly expiryReceiptDigest: string
  readonly firstPublicationAttemptId: string
  readonly secondPublicationAttemptId: string
}

async function durableIdentities(key: string): Promise<DurableIdentities> {
  if (!/^[A-Za-z0-9-]{1,80}$/u.test(key)) throw new TypeError('durable fixture key is invalid')
  return Object.freeze({
    operationId: await identity(key, 'operation', 16),
    workspaceId: await identity(key, 'workspace', 16),
    repositoryRef: await identity(key, 'repository', 32),
    shareInstance: await identity(key, 'share', 16),
    syntheticRoot: await identity(key, 'selection-root', 16),
    fileId: await identity(key, 'file', 16),
    fileRevision: await identity(key, 'revision', 16),
    directoryId: await identity(key, 'directory', 16),
    generation: await identity(key, 'generation', 16),
    rootOwnedObjectId: await identity(key, 'workspace-root-object', 32),
    transferJobId: await identity(key, 'transfer-job', 16),
    expiryReceiptDigest: await identity(key, 'expiry-receipt', 32),
    firstPublicationAttemptId: await identity(key, 'publication-one', 16),
    secondPublicationAttemptId: await identity(key, 'publication-two', 16),
  })
}

async function identity(key: string, label: string, width: 16 | 32): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(`windshare/w3c-test/${key}/${label}`),
  ))
  return encodeBase64Url(digest.slice(0, width))
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

async function workspaceBudgetAuthority(input: {
  readonly operationId: string
  readonly databaseName: string
  readonly now: number
  readonly token: string
}): Promise<OriginPrivateWorkspaceBudgetAuthority> {
  return OriginPrivateWorkspaceBudgetAuthority.open(input.operationId, {
    estimate: async () => ({ usage: 0, quota: Number(CAPACITY_BYTES) }),
    jobLimitBytes: CAPACITY_BYTES,
    processLimitBytes: CAPACITY_BYTES,
    minimumReserveBytes: 0n,
    databaseName: input.databaseName,
    now: () => input.now,
    leaseMilliseconds: CLAIM_LEASE_MILLISECONDS,
    heartbeatMilliseconds: CLAIM_HEARTBEAT_MILLISECONDS,
    randomToken: () => input.token,
  })
}

async function readLifecycle(
  repository: IndexedDbReceiveOperationRepository,
  operationId: string,
) {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new Error('durable operation lacks lifecycle state')
  return decodeStoredReceiveLifecycleState(record)
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

function originPrivateClaim(claim: {
  readonly budgetDigest: string
  release(): Promise<void>
}): OriginPrivateWorkspaceBudgetClaim {
  if (!('readmit' in claim) || typeof claim.readmit !== 'function') {
    throw new TypeError('origin-private admission did not return a readmission claim')
  }
  return claim as OriginPrivateWorkspaceBudgetClaim
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
