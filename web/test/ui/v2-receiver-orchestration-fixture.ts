import { vi } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy, type V2FrozenSelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2ConnectivityActivation } from '../../src/connectivity/v2-receiver-policy'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createIncidentScopeIssuer,
  type FailureFact,
  type FailureFactRelation,
  type IncidentScopeKind,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { recordOutputException, type OutputFailureSinks } from '../../src/output/diagnostics'
import type {
  CandidateMaterializationBinding,
  EnvironmentOffers,
  OfferedArtifactChoice,
  ResolvedArtifactAction,
} from '../../src/output/planning'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
  type ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import {
  createManagedAtomicReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { V2PlanExecutionAuthority } from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
  type TransferWorkerSettlement,
} from '../../src/transfer/outcome'
import type { TransferJobResult } from '../../src/transfer/v2-job'
import {
  createAuthenticatedProjectionEvidence,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import type { V2ReceiverIncidentPort } from '../../src/ui/controller/contracts'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from '../../src/ui/v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2ArtifactPresentationAuthority,
  V2LifecycleMutation,
  V2RouteCommitInput,
  V2RouteCommitResult,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../../src/ui/v2-receive-runtime'
import type { V2ProtocolGenerationListener } from '../../src/receiver/v2-supervisor'
import {
  environment,
  handoffTarget,
  managedTarget,
  workspaceOffer,
} from '../output/planning/fixture'

const ROOT_ID = identityText(1)
export const FILE_ID = identityText(2)
const SESSION_ID = identityText(3)

export const MANAGED_ENVIRONMENT = environment({ targets: [managedTarget()] })
export const WORKSPACE_ENVIRONMENT = environment({
  targets: [handoffTarget()],
  workspace: workspaceOffer(),
})
export const NO_DESTINATION_ENVIRONMENT = environment()

const FILE_ENTRY: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identityBytes(2),
  idText: FILE_ID,
  name: 'report.txt',
  expectedSize: 128n,
})

export function resetOrchestrationTestEnvironment(): void {
  vi.useRealTimers()
  vi.unstubAllGlobals()
}

export class FakeReceiveComposition implements V2ReceiveCompositionPort {
  readonly startedChoices: OfferedArtifactChoice[] = []
  readonly startedAuthorities: FakeStartedAuthority[] = []
  readonly authorityStartStacks: boolean[] = []
  readonly retainedSignals: AbortSignal[] = []
  readonly retainedActionCalls: Array<Readonly<{
    operation: V2RetainedReceiveOperation
    action: V2RetainedReceiveAction
    signal: AbortSignal
  }>> = []
  readonly retainedInventories: V2RetainedReceiveInventory[] = []
  retainedOperations: readonly V2RetainedReceiveOperation[] = Object.freeze([])
  retainedGate: PromiseLike<V2RetainedReceiveInventory> | undefined
  retainedActionGate: PromiseLike<V2RetainedReceiveActionResult> | undefined
  readonly retained = Object.freeze({
    list: (signal: AbortSignal): PromiseLike<V2RetainedReceiveInventory> => {
      this.retainedSignals.push(signal)
      if (this.retainedGate !== undefined) return this.retainedGate
      const inventory = retainedInventory(
        this.retainedOperations,
        (operation, action, actionSignal) => {
          this.retainedActionCalls.push(Object.freeze({
            operation,
            action,
            signal: actionSignal,
          }))
          return this.retainedActionGate ?? Promise.resolve(Object.freeze({ kind: 'completed' }))
        },
      )
      this.retainedInventories.push(inventory)
      return Promise.resolve(inventory)
    },
  })
  readonly #fallback: EnvironmentOffers
  readonly #environmentQueue: Array<EnvironmentOffers | PromiseLike<EnvironmentOffers>>
  environmentCalls = 0
  clickStack: () => boolean = () => true
  authorityReady: Promise<void> | undefined

  constructor(
    fallback: EnvironmentOffers,
    environmentQueue: Array<EnvironmentOffers | PromiseLike<EnvironmentOffers>> = [],
  ) {
    this.#fallback = fallback
    this.#environmentQueue = [...environmentQueue]
  }

  environment(): EnvironmentOffers | PromiseLike<EnvironmentOffers> {
    this.environmentCalls += 1
    return this.#environmentQueue.shift() ?? this.#fallback
  }

  startArtifactAuthority(
    offered: OfferedArtifactChoice,
  ): V2ArtifactPresentationAuthority {
    this.startedChoices.push(offered)
    this.authorityStartStacks.push(this.clickStack())
    const authority = new FakeStartedAuthority(
      offered,
      intent => new FakeBoundRuntime(intent),
      this.authorityReady,
    )
    this.startedAuthorities.push(authority)
    return authority
  }
}

export class FakeStartedAuthority implements V2ArtifactPresentationAuthority {
  readonly ready: Promise<void>
  readonly releaseReasons: unknown[] = []
  readonly commitActions: ResolvedArtifactAction[] = []
  readonly #offered: OfferedArtifactChoice
  readonly #runtime: (intent: ReceiveIntent) => FakeBoundRuntime
  runtime: FakeBoundRuntime | undefined

  constructor(
    offered: OfferedArtifactChoice,
    runtime: (intent: ReceiveIntent) => FakeBoundRuntime,
    ready: Promise<void> = Promise.resolve(),
  ) {
    this.#offered = offered
    this.#runtime = runtime
    this.ready = ready
  }

  async commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult> {
    input.signal.throwIfAborted()
    this.commitActions.push(input.action)
    const candidate = await candidateBindingForTest(input.action, this.#offered.suggestedName)
    input.signal.throwIfAborted()
    const bound = await input.freezeAtFence(candidate)
    input.signal.throwIfAborted()
    this.runtime = this.#runtime(bound.intent)
    return Object.freeze({ kind: 'bound-operation', operation: this.runtime })
  }

  release(reason: unknown): void {
    this.releaseReasons.push(reason)
  }
}

class FakeTransferControlError extends DOMException {
  readonly control: V2ActiveReceiveControl

  constructor(control: V2ActiveReceiveControl) {
    super(`${control} requested`, 'AbortError')
    this.control = control
  }
}

export class FakeBoundRuntime implements V2BoundReceiveOperation {
  readonly plans = Object.freeze({}) as V2PlanExecutionAuthority
  readonly transferJobId = identityText(76)
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls: readonly V2ActiveReceiveControl[] = Object.freeze(['pause', 'stop'])
  readonly initialWorkspaceUsage: WorkspaceUsage = Object.freeze({
    ownedBytes: 512n,
    maximumBytes: 4_096n,
  })
  readonly interruptions: Array<{ control: V2ActiveReceiveControl; inClickStack: boolean }> = []
  readonly lifecycleActions: Array<{ action: LifecycleUserAction; inClickStack: boolean }> = []
  readonly expiryObservations: ReceiveLifecycleState[] = []
  readonly detachments: unknown[] = []
  readonly admissionFailures: unknown[] = []
  currentOutputFailures: OutputFailureSinks | undefined
  expiryFailure: unknown
  detachFailure: unknown
  controlStack: () => boolean = () => true
  lifecycleActionStack: () => boolean = () => true
  lastControl: V2ActiveReceiveControl | undefined
  admissionSettlementError: unknown
  nextLifecycleAction: (
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ) => V2LifecycleMutation = defaultLifecycleAction

  constructor(
    intent: ReceiveIntent = {} as ReceiveIntent,
    lifecycle?: ReceiveLifecycleState,
  ) {
    this.intent = intent
    this.lifecycle = lifecycle ?? initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    })
  }

  bindOutputFailures(failures: OutputFailureSinks | undefined) {
    this.currentOutputFailures = failures
    return Object.freeze({
      revoke: () => {
        if (this.currentOutputFailures === failures) this.currentOutputFailures = undefined
      },
    })
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    this.lastControl = control
    this.interruptions.push({ control, inClickStack: this.controlStack() })
    transfer.abort(new FakeTransferControlError(control))
  }

  startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): V2LifecycleMutation {
    this.lifecycleActions.push({ action, inClickStack: this.lifecycleActionStack() })
    return this.nextLifecycleAction(action, lifecycle)
  }

  async observeExpiry(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    this.expiryObservations.push(lifecycle)
    if (this.expiryFailure !== undefined) {
      recordOutputException(this.currentOutputFailures?.checkpoint, this.expiryFailure)
      throw this.expiryFailure
    }
    const expiresAt = lifecycleDeadlineForTest(lifecycle)
    return {
      lifecycle: next(lifecycle, {
        kind: 'expired',
        priorStableState: stableKindForExpiry(lifecycle),
        expiresAt,
        cleanupState: 'cleanup-pending',
        expiryReceiptDigest: identityText(73, 32),
      }),
      workspaceUsage: this.initialWorkspaceUsage,
    }
  }

  resolveWorkspaceUsage(): WorkspaceUsage {
    return this.initialWorkspaceUsage
  }

  settleTransferAdmissionFailure(reason: unknown): V2LifecycleMutation {
    this.admissionFailures.push(reason)
    if (this.admissionSettlementError !== undefined) throw this.admissionSettlementError
    if (this.lastControl === 'pause') {
      return {
        lifecycle: next(this.lifecycle, {
          kind: 'resumable-receive',
          checkpointSetDigest: identityText(74, 32),
          completedFileCount: 0n,
          completedBytes: 0n,
          expiresAt: Date.now() + 60_000,
        }),
        workspaceUsage: this.initialWorkspaceUsage,
      }
    }
    return {
      lifecycle: next(this.lifecycle, {
        kind: 'restart-required',
        reason: 'direct-atomic-rolled-back',
        receiptDigest: identityText(75, 32),
      }),
    }
  }

  detach(): void {
    this.detachments.push('detached')
    if (this.detachFailure !== undefined) {
      recordOutputException(this.currentOutputFailures?.cleanup, this.detachFailure)
      throw this.detachFailure
    }
  }
}

export class FakeJoinedShare {
  readonly descriptor = Object.freeze({
    shareInstance: identityBytes(10),
    shareInstanceId: identityText(10),
    syntheticRoot: identityBytes(1),
    syntheticRootId: ROOT_ID,
  })
  readonly recoveryIdentity = 'test-share'
  protocolSessionId = SESSION_ID
  readonly selection: V2SelectionPolicy
  readonly projectionRequests: Array<{ readonly signal: AbortSignal }> = []
  readonly transferRuns: FakeTransferRun[] = []
  readonly #projectionGates: Array<PromiseLike<void> | undefined>
  readonly #generationListeners = new Set<V2ProtocolGenerationListener>()
  closeCount = 0

  constructor(defaultSelected: boolean, projectionGates: Array<PromiseLike<void> | undefined> = []) {
    this.selection = new V2SelectionPolicy(defaultSelected)
    this.#projectionGates = [...projectionGates]
  }

  rootDirectory(): V2BrowseDirectory {
    return Object.freeze({
      id: identityBytes(1),
      idText: ROOT_ID,
      name: 'Shared files',
      path: Object.freeze([]),
      ancestry: Object.freeze([ROOT_ID]),
    })
  }

  async page(directory: V2BrowseDirectory) {
    return Object.freeze({
      directory,
      pageIndex: 0,
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      entries: Object.freeze([FILE_ENTRY]),
    })
  }

  subscribeCatalogScanProgress(): () => void {
    return () => undefined
  }

  get protocolSessionIdentity(): string {
    return this.protocolSessionId
  }

  subscribeProtocolGeneration(listener: V2ProtocolGenerationListener): () => void {
    this.#generationListeners.add(listener)
    return () => this.#generationListeners.delete(listener)
  }

  replaceProtocolSession(protocolSessionId: string): void {
    this.protocolSessionId = protocolSessionId
    for (const listener of this.#generationListeners) {
      listener(Object.freeze({
        generationId: 2,
        protocolSessionId,
        protocolSessionIdentity: protocolSessionId as unknown as Parameters<
          V2ProtocolGenerationListener
        >[0]['protocolSessionIdentity'],
      }))
    }
  }

  projectionSource(selection: V2FrozenSelectionPolicy): AuthenticatedDiscoverySource {
    const gate = this.#projectionGates.shift()
    const requests = this.projectionRequests
    return Object.freeze({
      discover: async function* (request: AuthenticatedDiscoveryRequest) {
        requests.push({ signal: request.signal })
        await gate
        const selected = selection.selected(FILE_ENTRY, [ROOT_ID])
        yield createAuthenticatedProjectionEvidence({
          generations: [{
            directoryId: ROOT_ID,
            generation: identityText(20 + requests.length),
          }],
          metrics: {
            fileCountLowerBound: selected ? 1 : 0,
            directoryCountLowerBound: 0,
            byteCountLowerBound: selected ? FILE_ENTRY.expectedSize : 0n,
          },
          ...(selected
            ? {
                representativeFile: {
                  fileId: FILE_ID,
                  sourcePath: FILE_ENTRY.name,
                  portableName: FILE_ENTRY.name,
                },
              }
            : {}),
          settledTargets: request.unsettledTargets,
        })
        return Object.freeze({ settledTargets: request.unsettledTargets })
      },
    })
  }

  beginDownloadConnectivity(): V2ConnectivityActivation {
    return {
      routes: Object.freeze({}) as never,
      close: () => undefined,
    }
  }

  transferJob(plans: V2PlanExecutionAuthority, intent: ReceiveIntent) {
    const run = new FakeTransferRun(plans, intent)
    this.transferRuns.push(run)
    return Object.freeze({ run: (signal?: AbortSignal) => run.start(signal) })
  }

  async close(): Promise<void> {
    this.closeCount += 1
  }
}

export class FakeTransferRun {
  readonly plans: V2PlanExecutionAuthority
  readonly intent: ReceiveIntent
  readonly #result = deferred<TransferJobResult>()
  signal: AbortSignal | undefined

  constructor(plans: V2PlanExecutionAuthority, intent: ReceiveIntent) {
    this.plans = plans
    this.intent = intent
  }

  start(signal?: AbortSignal): Promise<TransferJobResult> {
    this.signal = signal
    signal?.addEventListener('abort', () => {
      const reason = signal.reason
      if (!(reason instanceof FakeTransferControlError)) {
        this.#result.reject(reason)
        return
      }
      const lifecycle = initialReceiveLifecycleState({
        operationId: this.intent.operationId,
        receiveIntentDigest: this.intent.digest,
      })
      this.resolve(reason.control === 'pause'
        ? next(lifecycle, {
            kind: 'resumable-receive',
            checkpointSetDigest: identityText(74, 32),
            completedFileCount: 0n,
            completedBytes: 0n,
            expiresAt: Date.now() + 60_000,
          })
        : next(lifecycle, {
            kind: 'restart-required',
            reason: 'direct-atomic-rolled-back',
            receiptDigest: identityText(75, 32),
          }), reason)
    }, { once: true })
    return this.#result.promise
  }

  resolve(
    lifecycle: ReceiveLifecycleState,
    abortReason?: unknown,
    worker: TransferWorkerSettlement = abortReason === undefined
      ? transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
      : transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
  ): void {
    this.#result.resolve({
      worker,
      lifecycle,
      measure: Object.freeze({}) as never,
      transferJobId: identityText(76),
      intent: this.intent,
      ...(abortReason === undefined ? {} : { abortReason }),
    })
  }

  reject(reason: unknown): void {
    this.#result.reject(reason)
  }
}

export class FakeGateway {
  readonly #joined: FakeJoinedShare[]
  joinCount = 0

  constructor(joined: FakeJoinedShare[]) {
    this.#joined = joined
  }

  async join(): Promise<V2JoinedBrowserShare> {
    const joined = this.#joined[this.joinCount]
    this.joinCount += 1
    if (joined === undefined) throw new Error('unexpected join')
    return joined as unknown as V2JoinedBrowserShare
  }
}

export function controllerFor(
  joined: FakeJoinedShare,
  receive: V2ReceiveCompositionPort,
  incidents?: V2ReceiverIncidentPort,
): V2ReceiverController {
  const gateway = new FakeGateway([joined])
  const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
    receive,
    ...(incidents === undefined ? {} : { incidents }),
  })
  controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
  return controller
}

export interface RecordingIncidents {
  readonly port: V2ReceiverIncidentPort
  readonly facts: Array<Readonly<{
    scopeKind: IncidentScopeKind
    scopeSequence: bigint
    fact: FailureFact
    relation: FailureFactRelation
  }>>
  readonly decisions: PresentationDecision[]
  readonly order: string[]
}

export function recordingIncidents(): RecordingIncidents {
  const issuer = createIncidentScopeIssuer()
  const facts: RecordingIncidents['facts'] = []
  const decisions: PresentationDecision[] = []
  const order: string[] = []
  return {
    facts,
    decisions,
    order,
    port: {
      openScope: kind => issuer.open(kind, {
        factRecorded: observation => {
          facts.push({
            scopeKind: kind,
            scopeSequence: observation.ref.scope.scopeSequence,
            fact: observation.fact,
            relation: observation.relation,
          })
          order.push(observation.fact.kind === 'native_output_failure'
            ? `native:${observation.fact.stage}:${observation.relation}`
            : `fact:${observation.fact.kind}:${observation.relation}`)
        },
      }),
      submitDecision: (_scope, decision) => {
        decisions.push(decision)
        order.push(`decision:${decision.kind}`)
      },
    },
  }
}

export async function startTransfer(
  controller: V2ReceiverController,
  joined: FakeJoinedShare,
): Promise<void> {
  await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
  controller.chooseArtifact('download-original')
  await waitFor(() => joined.transferRuns.length === 1)
}

export async function candidateBindingForTest(
  action: ResolvedArtifactAction,
  suggestedName: string | null,
): Promise<CandidateMaterializationBinding> {
  const artifact = action.artifact
  switch (action.route.kind) {
    case 'direct-atomic': {
      const reservation = await createManagedAtomicReservation({
        operationId: identityText(40),
        reservationId: identityText(41),
        artifact,
        authorityRef: identityText(42, 32),
        nameAuthority: action.route.target.guarantees.nameAuthority as
          'application-chosen' | 'user-chosen',
        requestedName: suggestedName ?? FILE_ENTRY.name,
        reservedName: suggestedName ?? FILE_ENTRY.name,
        collisionIndex: 0,
      })
      return Object.freeze({
        kind: 'destination-reservation',
        targetRouteId: action.route.target.routeId,
        reservation,
      })
    }
    case 'workspace-then-publish': {
      const workspace = await createWorkspaceBinding({
        operationId: identityText(43),
        workspaceId: identityText(44),
        artifact,
        repositoryRef: identityText(45, 32),
      })
      return Object.freeze({
        kind: 'workspace-binding',
        workspaceRouteId: action.route.workspace.routeId,
        publicationTargetRouteId: action.route.publicationTarget.routeId,
        workspace,
      })
    }
    case 'direct-tree':
    case 'portable-handoff':
      throw new Error(`unsupported test plan ${action.choice.plan.kind}`)
  }
}

export async function retainedWorkspaceIntent(joined: FakeJoinedShare): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createOriginalFileArtifact({
    fileId: FILE_ID,
    sourcePath: FILE_ENTRY.name,
    suggestedName: FILE_ENTRY.name,
  })
  const workspace = await createWorkspaceBinding({
    operationId: identityText(90),
    workspaceId: identityText(91),
    artifact,
    repositoryRef: identityText(92, 32),
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

export function retainedReceiveContinuation(intent: ReceiveIntent): Readonly<{
  operation: V2RetainedReceiveOperation
  runtime: FakeBoundRuntime
}> {
  const initial = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  const retained = next(initial, {
    kind: 'resumable-receive',
    checkpointSetDigest: identityText(93, 32),
    completedFileCount: 1n,
    completedBytes: 64n,
    expiresAt: Date.now() + 60_000,
  })
  const receiving = next(retained, {
    kind: 'receiving',
    activeLeaseId: identityText(94),
  })
  return Object.freeze({
    operation: Object.freeze({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      lifecycleGeneration: retained.generation,
      lifecycle: retained,
      continuation: 'resume-receive',
      actions: Object.freeze(['continue', 'discard'] as const),
    }),
    runtime: new FakeBoundRuntime(intent, receiving),
  })
}

export function retainedOperation(): V2RetainedReceiveOperation {
  const lifecycle: ReceiveLifecycleState = Object.freeze({
    kind: 'needs-attention',
    operationId: identityText(80),
    receiveIntentDigest: identityText(81, 32),
    generation: 7n,
    reason: 'target-ownership-unknown',
    lastVerifiedRecordDigest: identityText(82, 32),
  })
  return Object.freeze({
    operationId: lifecycle.operationId,
    receiveIntentDigest: lifecycle.receiveIntentDigest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation: 'needs-attention',
    actions: Object.freeze([]),
    unavailableReason: 'Ownership needs attention; no automatic action is safe.',
  })
}

export function waitingRetainedOperation(): V2RetainedReceiveOperation {
  const lifecycle: ReceiveLifecycleState = Object.freeze({
    kind: 'waiting-to-save',
    operationId: identityText(83),
    receiveIntentDigest: identityText(84, 32),
    generation: 4n,
    packageDigest: identityText(85, 32),
    expiresAt: Date.now() + 60_000,
  })
  return Object.freeze({
    operationId: lifecycle.operationId,
    receiveIntentDigest: lifecycle.receiveIntentDigest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation: 'save-artifact',
    actions: Object.freeze(['save', 'discard'] as const),
  })
}

export function retainedInventory(
  operations: readonly V2RetainedReceiveOperation[],
  act: V2RetainedReceiveInventory['act'] = () =>
    Promise.resolve(Object.freeze({ kind: 'completed' })),
): V2RetainedReceiveInventory {
  let open = true
  return Object.freeze({
    operations,
    presentationFailures: Object.freeze([]),
    act: (
      operation: V2RetainedReceiveOperation,
      action: V2RetainedReceiveAction,
      signal: AbortSignal,
    ) => {
      if (!open) return Promise.reject(new DOMException('Inventory is closed', 'InvalidStateError'))
      return act(operation, action, signal)
    },
    close: vi.fn(() => {
      open = false
    }),
  })
}

export function stableLifecycle(
  lifecycle: ReceiveLifecycleState,
  kind: 'resumable-receive' | 'waiting-to-save' | 'download-started',
): ReceiveLifecycleState {
  switch (kind) {
    case 'resumable-receive':
      return next(lifecycle, {
        kind,
        checkpointSetDigest: identityText(50, 32),
        completedFileCount: 1n,
        completedBytes: 128n,
        expiresAt: Date.now() + 60_000,
      })
    case 'waiting-to-save':
      return next(lifecycle, {
        kind,
        packageDigest: identityText(51, 32),
        expiresAt: Date.now() + 60_000,
      })
    case 'download-started':
      return next(lifecycle, {
        kind,
        attemptKind: 'workspace',
        attemptId: identityText(52),
        packageDigest: identityText(53, 32),
        retryableUntil: Date.now() + 60_000,
      })
  }
}

function defaultLifecycleAction(
  action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
  lifecycle: ReceiveLifecycleState,
): V2LifecycleMutation {
  switch (action) {
    case 'continue':
      return {
        lifecycle: next(lifecycle, { kind: 'receiving', activeLeaseId: identityText(60) }),
        activeControls: Object.freeze(['pause', 'stop']),
        resumeTransfer: true,
      }
    case 'save':
      return {
        lifecycle: next(lifecycle, {
          kind: 'publishing-managed',
          activeLeaseId: identityText(61),
          packageDigest: identityText(62, 32),
          publicationAttemptId: identityText(63),
        }),
      }
    case 'redownload':
    case 'change-location':
      return {
        lifecycle: next(lifecycle, {
          kind: 'handing-off',
          activeLeaseId: identityText(64),
          attemptKind: 'workspace',
          attemptId: identityText(65),
          packageDigest: identityText(66, 32),
          retainedDeadline: Date.now() + 60_000,
        }),
      }
    case 'discard':
    case 'delete':
      return {
        lifecycle: next(lifecycle, {
          kind: 'discarded',
          cleanupReceiptDigest: identityText(67, 32),
        }),
      }
  }
}

export function next(
  lifecycle: ReceiveLifecycleState,
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  return nextReceiveLifecycleState(lifecycle, payload)
}

function lifecycleDeadlineForTest(lifecycle: ReceiveLifecycleState): number {
  if (lifecycle.kind === 'resumable-receive' || lifecycle.kind === 'resumable-package' ||
      lifecycle.kind === 'waiting-to-save') return lifecycle.expiresAt
  if (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace') {
    return lifecycle.retryableUntil
  }
  throw new Error('test lifecycle does not have a stable deadline')
}

function stableKindForExpiry(
  lifecycle: ReceiveLifecycleState,
): Extract<ReceiveLifecycleState, { kind: 'expired' }>['priorStableState'] {
  if (lifecycle.kind === 'resumable-receive' || lifecycle.kind === 'resumable-package' ||
      lifecycle.kind === 'waiting-to-save') return lifecycle.kind
  if (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace') {
    return lifecycle.kind
  }
  throw new Error('test lifecycle cannot expire')
}

export function missingChoice(): OfferedArtifactChoice {
  throw new Error('authority choice was not captured')
}

export function identityText(seed: number, width = 16): string {
  return encodeBase64Url(identityBytes(seed, width))
}

function identityBytes(seed: number, width = 16): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return value
}

export function traceSource(
  observer: (event: Readonly<{ name: string }>) => void,
) {
  return Object.freeze({
    get current() {
      return observer
    },
  })
}

export interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T | PromiseLike<T>): void
  reject(reason: unknown): void
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, decline) => {
    resolve = accept
    reject = decline
  })
  return { promise, resolve, reject }
}

export async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return
    await new Promise<void>(resolve => setTimeout(resolve, 1))
  }
  throw new Error('timed out waiting for controller state')
}

export async function turns(): Promise<void> {
  for (let index = 0; index < 24; index += 1) await Promise.resolve()
}
