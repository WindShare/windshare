import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type EnvironmentOffers,
  type OfferedArtifactChoice,
  type SelectionProjectionV1,
} from '../../src/output/planning'
import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MS } from '../../src/output/portable/browser-download'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import { TransferJob } from '../../src/transfer/v2-job'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type {
  V2PlanExecutionAuthority,
} from '../../src/transfer/output-session'
import {
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  deriveArtifactChoiceIdentity,
  type SelectionSpec,
} from '../../src/transfer/intent'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { OriginPrivateWorkspaceBudgetAuthority } from '../../src/output/origin-private/admission'
import { openOriginPrivateRetainedArtifactBackend } from '../../src/output/origin-private/session'
import { openOriginPrivateWorkspaceNamespace } from '../../src/output/origin-private/namespace'
import { openOriginPrivateProgressiveZipBackend } from '../../src/output/origin-private/progressive-backend'
import { RECEIVE_RECORD_RECEIPT } from '../../src/output/workspace/records'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from '../../src/ui/v2-browser-receive-composition'
import type {
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
} from '../../src/ui/v2-receive-runtime'
import {
  WorkspaceOperationStages,
  type WorkspaceStageTraceEvent,
} from '../../src/output/workspace/stages'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity as catalogIdentity,
  identityText as catalogIdentityText,
  readerFixture,
} from '../transfer/v2-job-fixture'
import {
  DURABLE_FIXTURE_INITIAL_TIME,
  durableIdentities,
  originPrivateClaim,
  readDurableLifecycle,
} from './durable-output-fixture'

const PRODUCT_ZIP_FILE_BYTES = 68n
const PRODUCT_FIXTURE_CHUNK_BYTES = 2
const PRODUCT_FIXTURE_LANE_COUNT = 1
const EMPTY_MATERIALIZATION_SUMMARY = Object.freeze({
  entryCount: 0n,
  fileCount: 0n,
  directoryCount: 0n,
  rawBytes: 0n,
})

export interface FreshProgressiveZipAdmissionProof {
  readonly lifecycle: string
  readonly contentRequests: string
  readonly traceNames: readonly string[]
  readonly receiptCount: number
  readonly manifestPageCount: number
  readonly objectHandlePresent: boolean
}

export interface ProductProgressiveZipAdmissionProof {
  readonly admission: string
  readonly lifecycle: string
  readonly traceNames: readonly string[]
  readonly cleanup: string
}

export interface TransferJobProgressiveZipProof {
  readonly worker: string
  readonly lifecycle: string
  readonly workspaceTraceNames: readonly string[]
  readonly transferTraceNames: readonly string[]
  readonly evidence: Readonly<{
    readonly admittedFilePaths: readonly string[]
    readonly admissionCount: number
    readonly discoveryCompleteCalls: number
  }>
  readonly cleanup: string
}

export async function proveFreshProgressiveZipAdmission(
  key: string,
): Promise<FreshProgressiveZipAdmissionProof> {
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
    preClickRanking: [(await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)).id],
    repository,
    randomOwnedObjectId: () => ids.rootOwnedObjectId,
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    clock: { now: () => DURABLE_FIXTURE_INITIAL_TIME },
    randomBytes: () => new Uint8Array(16).fill(0x41),
  })
  const traces: WorkspaceStageTraceEvent[] = []
  let contentRequests = 0n
  const stages = await WorkspaceOperationStages.open({
    repository,
    receiveIntent: intent,
    leaseId: lease.leaseId,
    clock: () => DURABLE_FIXTURE_INITIAL_TIME,
    contentRequests: { count: () => contentRequests },
    onTrace: event => traces.push(event),
  })
  let cleanupBackend: Awaited<ReturnType<
    typeof openOriginPrivateRetainedArtifactBackend
  >> | undefined
  let claim: ReturnType<typeof originPrivateClaim> | undefined
  try {
    const authority = await OriginPrivateWorkspaceBudgetAuthority.open(intent.operationId, {
      estimate: () => navigator.storage.estimate(),
    })
    const admission = await stages.progressive.admit(authority)
    claim = originPrivateClaim(admission.claim)
    cleanupBackend = await openOriginPrivateRetainedArtifactBackend({
      receiveIntent: intent,
      operationRepository: repository,
      namespace,
    })
    const progressive = await openOriginPrivateProgressiveZipBackend({
      receiveIntent: intent, operationRepository: repository, namespace,
      contentGate: admission.gate, budgetClaim: claim,
    })
    const checkpoint = await progressive.store.readCheckpoint()
    const objectHandle = await repository.readHandle(progressive.object.handleId)
    await progressive.close()
    if (checkpoint?.entryCount !== 0n || checkpoint.discoveryComplete) {
      throw new Error('Progressive admission prematurely consumed selection discovery')
    }
    const [lifecycle, receipts, pages] = await Promise.all([
      readDurableLifecycle(repository, intent.operationId),
      repository.listRecords(intent.operationId, RECEIVE_RECORD_RECEIPT),
      repository.listManifestPages(intent.operationId),
    ])
    return Object.freeze({
      lifecycle: lifecycle.kind,
      contentRequests: contentRequests.toString(),
      traceNames: Object.freeze(traces.map(event => event.name)),
      receiptCount: receipts.length,
      manifestPageCount: pages.length,
      objectHandlePresent: objectHandle !== undefined,
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

export async function proveProductProgressiveZipAdmission(
  key: string,
): Promise<ProductProgressiveZipAdmissionProof> {
  const ids = await durableIdentities(key)
  const selection = await createSelectionSpec({
    shareInstance: ids.shareInstance,
    syntheticRoot: ids.syntheticRoot,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projection = productZipProjection(selection.digest, ids.directoryId)
  const traces: WorkspaceStageTraceEvent[] = []
  const composition = createBrowserReceiveComposition(window as BrowserReceiveWindow, {
    onTrace: event => traces.push(event),
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
  const choice = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.route.kind === 'workspace-then-publish' &&
    candidate.choice.artifactKind === 'zip-archive')
  if (choice === undefined) throw new TypeError('product composition omitted workspace ZIP')

  let runtime: V2BoundReceiveOperation | undefined
  try {
    runtime = await commitProductionChoice(
      composition,
      selection,
      projection,
      environment,
      choice,
      signal,
    )
    const intent = runtime.intent
    if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'zip-archive') {
      throw new TypeError('product binding changed the workspace ZIP route')
    }
    const workspaceIntent = intent as Parameters<typeof runtime.plans.openWorkspaceZip>[0]
    const admission = await runtime.plans.openWorkspaceZip(workspaceIntent, signal)
    if (admission.kind !== 'accepted') {
      throw new DOMException('product workspace ZIP was rejected', 'QuotaExceededError')
    }
    const paused = await admission.execution.pause(Object.freeze({
      worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
      materialization: EMPTY_MATERIALIZATION_SUMMARY,
      selectionFacts: Object.freeze({
        discoveredFileCount: 1n,
        discoveredBytes: PRODUCT_ZIP_FILE_BYTES,
        discovery: 'complete',
      }),
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

export async function proveTransferJobProgressiveZip(): Promise<TransferJobProgressiveZipProof> {
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
  const handoff = controlledHandoffLease()
  const composition = createBrowserReceiveComposition(handoff.windowPort, {
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
  const choice = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.route.kind === 'workspace-then-publish' &&
    candidate.choice.artifactKind === 'zip-archive')
  if (choice === undefined) throw new TypeError('product composition omitted workspace ZIP')

  let runtime: V2BoundReceiveOperation | undefined
  try {
    runtime = await commitProductionChoice(
      composition,
      selection,
      projection,
      environment,
      choice,
      signal,
    )
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
    const evidence = { admittedFilePaths: [] as string[], admissionCount: 0, discoveryCompleteCalls: 0 }
    const openWorkspaceZip: V2PlanExecutionAuthority['openWorkspaceZip'] = async (intent, openSignal) => {
      const admitted = await runtime!.plans.openWorkspaceZip(intent, openSignal)
      if (admitted.kind !== 'accepted') return admitted
      evidence.admissionCount++
      const execution = admitted.execution
      return { kind: 'accepted', execution: {
        ...execution,
        output: {
          ...execution.output,
          beginFile: (request, fileSignal) => {
            evidence.admittedFilePaths.push(request.sourceAuthenticationPath.join('/'))
            return execution.output.beginFile(request, fileSignal)
          },
        },
        discoveryComplete: async discoverySignal => {
          evidence.discoveryCompleteCalls++
          await execution.discoveryComplete!(discoverySignal)
        },
      } }
    }
    const plans: V2PlanExecutionAuthority = Object.freeze({ ...runtime.plans, openWorkspaceZip })
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
      revisionCapacity: {
        generation: {
          waitForProtocolSessionReplacement: (_identity, waitSignal) =>
            waitUntilAborted(waitSignal),
        },
      },
      transferJobId: runtime.transferJobId,
      trace: {
        current: event => transferTraceNames.push(
          event.name === 'receive_transition' ? event.transition : event.name,
        ),
      },
    }).run(signal)
    handoff.expire()
    const discarded = await runtime.startLifecycleAction('discard', result.lifecycle)
    return Object.freeze({
      worker: result.worker.status,
      lifecycle: result.lifecycle.kind,
      workspaceTraceNames: Object.freeze(workspaceTraces.map(event => event.name)),
      transferTraceNames: Object.freeze(transferTraceNames),
      evidence,
      cleanup: discarded.lifecycle.kind,
    })
  } finally {
    handoff.expire()
    if (runtime !== undefined) {
      await Promise.resolve(runtime.detach()).catch(() => undefined)
    }
  }
}

function controlledHandoffLease() {
  const leases = new Map<number, () => void>()
  const schedule: Window['setTimeout'] = (handler, milliseconds, ...arguments_) => {
    if (milliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MS || typeof handler !== 'function') {
      return window.setTimeout(handler, milliseconds, ...arguments_)
    }
    const release = handler as () => void
    const timer = window.setTimeout(() => {
      leases.delete(timer)
      release()
    }, milliseconds)
    leases.set(timer, release)
    return timer
  }
  const cancel: Window['clearTimeout'] = timer => {
    if (timer !== undefined) leases.delete(timer)
    window.clearTimeout(timer)
  }
  const windowPort = new Proxy(window, {
    get(target, property): unknown {
      if (property === 'setTimeout') return schedule
      if (property === 'clearTimeout') return cancel
      return Reflect.get(target, property, target)
    },
  }) as BrowserReceiveWindow
  return {
    windowPort,
    expire() {
      // This fixture tests reception and owned cleanup. Simulating the browser URL
      // deadline runs the real revocation/reader-release callback without a minute wait.
      for (const [timer, release] of leases) {
        window.clearTimeout(timer)
        leases.delete(timer)
        release()
      }
    },
  }
}

function waitUntilAborted(signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((_resolve, reject) => {
    const abort = () => reject(signal.reason ?? new DOMException('wait aborted', 'AbortError'))
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
}

export async function commitProductionChoice(
  composition: V2ReceiveCompositionPort,
  selection: SelectionSpec,
  projection: SelectionProjectionV1,
  environment: EnvironmentOffers,
  offered: OfferedArtifactChoice,
  signal: AbortSignal,
  display?: import('../../src/output/workspace/operation-display').ReceiveOperationDisplay,
): Promise<V2BoundReceiveOperation> {
  const resolution = await reconcileArtifactChoice({
    choice: offered.choice,
    preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: selection.digest,
    projection,
    discovery: Object.freeze({ kind: 'complete' }),
    environment,
    previousObservation: null,
  })
  if (resolution.kind !== 'resolved') {
    throw new TypeError('product workspace choice did not resolve')
  }
  const authority = composition.startArtifactAuthority(offered, [offered.choice.choiceId], undefined, display)
  await authority.ready
  const committed = await authority.commit({
    action: resolution.action,
    signal,
    freezeAtFence: candidate => bindReceiveIntent({
      selection,
      action: resolution.action,
      candidate,
    }),
  })
  if (committed.kind === 'bound-operation') return committed.operation
  if (committed.kind === 'retryable-precut') {
    throw new DOMException('Product workspace commit was interrupted before persistence', 'AbortError')
  }
  try {
    await committed.authority.settleActivationFailure(committed.cause)
  } finally {
    await committed.authority.detach()
  }
  throw committed.cause
}

export function productZipProjection(
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
