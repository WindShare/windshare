import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../output/origin-private/admission'
import {
  openOriginPrivateWorkspaceNamespace,
  removeOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../../output/origin-private/namespace'
import {
  openOriginPrivateWorkspaceBackend,
  type OriginPrivateWorkspaceBackend,
} from '../../output/origin-private/session'
import {
  OriginPrivatePackageWorkflow,
  type OriginPrivatePackageAttemptResult,
} from '../../output/origin-private/workflow'
import {
  type AcquiredMaterializationAuthority,
  type ArtifactAction,
} from '../../output/planning'
import type {
  AuthorityOwnedReceiveOperationContinuation,
} from '../../output/resume/reopen-authority'
import {
  DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
} from '../../output/workspace/budget'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import {
  WorkspaceOperationStages,
  type AdmittedWorkspaceContent,
  type WorkspaceCleanupRequest,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from '../../output/workspace/stages'
import {
  sealWorkspaceZipPreparation,
  type SealedWorkspaceZipPreparationV1,
} from '../../output/workspace/preparation'
import {
  createPersistentWorkspaceExecution,
  type PersistentMaterializationSettlementCut,
  type PersistentWorkspaceSettlementAuthority,
  type WorkspaceMaterializationEvidence,
} from '../../transfer/settlement/persistent-execution'
import {
  type V2ExecutionAdmissionLifecycle,
} from '../../transfer/settlement/v2-plan-authority'
import {
  createOperationID,
  createOutputSessionID,
  createTransferJobID,
  createWorkspaceBinding,
  createWorkspaceID,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  TransferPauseRequestedError,
  outputSessionIdentity,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type ExecutionAdmissionResult,
  type PlanPauseRequest,
  type PlanSettlementRequest,
  type V2PlanExecutionAuthority,
  type WorkspaceExecution,
} from '../../transfer/output-session'
import type { SuccessfulTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from '../v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import {
  WorkspaceExecutionAdmissionSettlement,
  type WorkspaceReceiveAdmissionFallback,
} from './workspace-admission'
import {
  NamespaceOnlyCleanupPort,
  handoffRetainedWorkspacePackage,
  workspacePlanAuthority,
} from './workspace-publication'
import {
  checkpointSetDigest,
  randomAuthorityReference,
  readLifecycle,
  requireMatchingSingleFileAdmission,
  requirePreparation,
  requireSameIntent,
  requireWorkspaceAction,
  unavailableRoute,
} from './shared'

const PACKAGE_CLEANUP_RETRY_LIMIT = 3
const ZERO_CONTENT_REQUESTS: WorkspaceContentRequestCounter = Object.freeze({ count: () => 0n })
type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' }
>

export class StartedWorkspaceReceive implements V2StartedArtifactAuthority {
  readonly #window: BrowserReceiveWindow
  readonly #action: ArtifactAction
  readonly #trace: WorkspaceStageTraceListener | undefined
  #released = false
  #claimed = false

  constructor(
    windowPort: BrowserReceiveWindow,
    action: ArtifactAction,
    trace: WorkspaceStageTraceListener | undefined,
  ) {
    this.#window = windowPort
    this.#action = action
    this.#trace = trace
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    signal.throwIfAborted()
    const action = requireWorkspaceAction(this.#action)
    const operationId = createOperationID()
    const workspace = await createWorkspaceBinding({
      operationId,
      workspaceId: createWorkspaceID(),
      artifact: action.artifact,
      repositoryRef: randomAuthorityReference(),
    })
    const intent = await freezeIntent(Object.freeze({
      kind: 'workspace-binding',
      workspaceOfferId: action.plan.workspace.id,
      workspace,
    }))
    signal.throwIfAborted()
    this.#requireLive()
    const repository = await IndexedDbReceiveOperationRepository.open()
    let lease: BrowserReceiveOperationLease | undefined
    let namespace: OriginPrivateWorkspaceNamespace | undefined
    try {
      namespace = await openOriginPrivateWorkspaceNamespace({
        receiveIntent: intent,
        repository,
        storage: this.#window.navigator.storage,
      })
      lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId)
      const stages = await WorkspaceOperationStages.open({
        repository,
        receiveIntent: intent,
        leaseId: lease.leaseId,
        clock: Date.now,
        contentRequests: ZERO_CONTENT_REQUESTS,
        ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
      })
      const runtime = await WorkspaceReceiveOperation.create({
        windowPort: this.#window,
        intent,
        repository,
        namespace,
        lease,
        stages,
        ...(this.#trace === undefined ? {} : { trace: this.#trace }),
      })
      signal.throwIfAborted()
      return runtime
    } catch (error) {
      if (namespace !== undefined) {
        await removeOriginPrivateWorkspaceNamespace(namespace, repository).catch(() => undefined)
      }
      await lease?.release().catch(() => undefined)
      repository.close()
      throw error
    }
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) {
      throw new DOMException('Workspace artifact authority was already finalized', 'InvalidStateError')
    }
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }
}

interface WorkspaceSealBundle {
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>
}

export class WorkspaceReceiveOperation implements V2BoundReceiveOperation, V2ExecutionAdmissionLifecycle {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['pause'] as const)
  readonly initialWorkspaceUsage: WorkspaceUsage
  readonly #window: BrowserReceiveWindow
  readonly #repository: ReceiveOperationRepository
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #lease: BrowserReceiveOperationLease
  readonly #stages: WorkspaceOperationStages
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #closeAuthority: (() => Promise<void>) | undefined
  #plans!: V2PlanExecutionAuthority
  #transferJobId: string
  #backend: OriginPrivateWorkspaceBackend | undefined
  #admitted: AdmittedWorkspaceContent | undefined
  #budgetClaim: OriginPrivateWorkspaceBudgetClaim | undefined
  #preparation: SealedWorkspaceZipPreparationV1 | undefined
  #sealBundle: WorkspaceSealBundle | undefined
  #packageExactBytes: bigint | undefined
  readonly #admissionSettlement: WorkspaceExecutionAdmissionSettlement
  #detached = false

  private constructor(input: {
    windowPort: BrowserReceiveWindow
    intent: ReceiveIntent
    lifecycle?: ReceiveLifecycleState
    repository: ReceiveOperationRepository
    namespace: OriginPrivateWorkspaceNamespace
    lease: BrowserReceiveOperationLease
    stages: WorkspaceOperationStages
    trace?: WorkspaceStageTraceListener
    transferJobId: string
    backend?: OriginPrivateWorkspaceBackend
    admitted?: AdmittedWorkspaceContent
    preparation?: SealedWorkspaceZipPreparationV1
    receiveAdmissionFallback?: WorkspaceReceiveAdmissionFallback
    closeAuthority?: () => Promise<void>
  }) {
    this.#window = input.windowPort
    this.intent = input.intent
    this.lifecycle = input.lifecycle ?? initialReceiveLifecycleState({
      operationId: input.intent.operationId,
      receiveIntentDigest: input.intent.digest,
    })
    this.#repository = input.repository
    this.#namespace = input.namespace
    this.#lease = input.lease
    this.#stages = input.stages
    this.#trace = input.trace
    this.#closeAuthority = input.closeAuthority
    this.#transferJobId = input.transferJobId
    this.#backend = input.backend
    this.#admitted = input.admitted
    this.#preparation = input.preparation
    this.initialWorkspaceUsage = Object.freeze({
      ownedBytes: 0n,
      maximumBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
    })
    this.#admissionSettlement = new WorkspaceExecutionAdmissionSettlement({
      operationId: this.intent.operationId,
      currentLifecycle: () => readLifecycle(this.#repository, this.intent.operationId),
      restoreContinuation: fallback => this.#stages.restoreReceiveContinuation(fallback),
      discard: () => this.#discard(),
      recordUnknown: () => this.recordSettlementUnknown(this.intent),
      workspaceUsage: state => this.resolveWorkspaceUsage(state),
    }, input.receiveAdmissionFallback)
  }

  static async create(input: {
    windowPort: BrowserReceiveWindow
    intent: ReceiveIntent
    repository: IndexedDbReceiveOperationRepository
    namespace: OriginPrivateWorkspaceNamespace
    lease: BrowserReceiveOperationLease
    stages: WorkspaceOperationStages
    trace?: WorkspaceStageTraceListener
  }): Promise<WorkspaceReceiveOperation> {
    const owner = new WorkspaceReceiveOperation({
      windowPort: input.windowPort,
      intent: input.intent,
      repository: input.repository,
      namespace: input.namespace,
      lease: input.lease,
      stages: input.stages,
      ...(input.trace === undefined ? {} : { trace: input.trace }),
      transferJobId: createTransferJobID(),
    })
    owner.#plans = await workspacePlanAuthority(input.intent, owner)
    return owner
  }

  static async reopen(input: {
    windowPort: BrowserReceiveWindow
    operation: Extract<WorkspaceReceiveContinuation, { kind: 'workspace-receive' }>['operation']
    backend: OriginPrivateWorkspaceBackend
    trace?: WorkspaceStageTraceListener
  }): Promise<WorkspaceReceiveOperation> {
    const { operation } = input
    const owner = new WorkspaceReceiveOperation({
      windowPort: input.windowPort,
      intent: operation.intent,
      lifecycle: operation.lifecycle,
      repository: operation.repository,
      namespace: operation.namespace,
      lease: operation.lease,
      stages: operation.stages,
      ...(input.trace === undefined ? {} : { trace: input.trace }),
      transferJobId: createTransferJobID(),
      backend: input.backend,
      admitted: operation.admittedContent,
      ...(operation.receiveContinuation.preparation === undefined
        ? {}
        : { preparation: operation.receiveContinuation.preparation }),
      ...(operation.receiveAdmissionFallback === undefined
        ? {}
        : { receiveAdmissionFallback: operation.receiveAdmissionFallback }),
      closeAuthority: () => operation.close(),
    })
    owner.#plans = await workspacePlanAuthority(operation.intent, owner)
    return owner
  }

  get plans(): V2PlanExecutionAuthority {
    return this.#plans
  }

  get transferJobId(): string {
    return this.#transferJobId
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    if (control !== 'pause') throw unavailableRoute()
    transfer.abort(new TransferPauseRequestedError())
  }

  async startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    switch (action) {
      case 'continue':
        return this.#continue(lifecycle)
      case 'save':
      case 'redownload':
        return this.#handoff(lifecycle)
      case 'discard':
      case 'delete':
        return this.#discard()
      case 'change-location':
        throw unavailableRoute()
    }
  }

  async observeExpiry(): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const result = await this.#stages.expireIfDue(this.#cleanupRequest())
    const state = result.kind === 'not-due' ? result.state : result.cleanup.state
    return Object.freeze({ lifecycle: state, workspaceUsage: this.resolveWorkspaceUsage(state) })
  }

  resolveWorkspaceUsage(lifecycle: ReceiveLifecycleState): WorkspaceUsage | null {
    if (lifecycle.kind === 'discarded' ||
        (lifecycle.kind === 'expired' && lifecycle.cleanupState === 'clean')) return null
    let ownedBytes = 0n
    if (lifecycle.kind === 'resumable-receive') ownedBytes = lifecycle.completedBytes
    else if (this.#packageExactBytes !== undefined &&
        (lifecycle.kind === 'waiting-to-save' ||
         (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace'))) {
      ownedBytes = this.#packageExactBytes
    } else if (this.#sealBundle !== undefined) {
      ownedBytes = this.#sealBundle.sealed.manifest.rawBytes
    }
    return Object.freeze({ ownedBytes, maximumBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT })
  }

  settleTransferAdmissionFailure(reason: unknown): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    return this.#admissionSettlement.settle(reason)
  }

  async settleExecutionAdmissionFailure(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    requireSameIntent(this.intent, intent)
    signal.throwIfAborted()
    this.#requireAttached()
    return (await this.#admissionSettlement.settle(reason)).lifecycle
  }

  async recordSettlementUnknown(
    intent: ReceiveIntent,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>> {
    requireSameIntent(this.intent, intent)
    return this.#stages.recordTargetOwnershipUnknown(
      this.#sealBundle?.sealed.seal.digest ?? this.intent.digest,
    )
  }

  async detach(): Promise<void> {
    if (this.#detached) return
    this.#detached = true
    if (this.#closeAuthority !== undefined) {
      await this.#closeAuthority()
      return
    }
    const tasks: Promise<unknown>[] = [this.#lease.release()]
    if (this.#backend !== undefined) tasks.push(this.#backend.close())
    if (this.#budgetClaim !== undefined) tasks.push(this.#budgetClaim.release())
    const results = await Promise.allSettled(tasks)
    this.#repository.close()
    const failures = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    if (failures.length > 0) {
      throw new AggregateError(failures.map(result => result.reason), 'Workspace receive resources did not detach')
    }
  }

  async admitOriginal(
    intent: ReceiveIntent,
    evidence: ExactSingleFileEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      requireMatchingSingleFileAdmission(this.#admitted, evidence)
      return this.#reopenWorkspaceExecution(intent, {
        kind: 'single-file',
        evidence,
      }, signal)
    }
    const budget = await this.#budgetAuthority()
    const result = await this.#stages.admitSingleFile({
      fileId: evidence.fileId,
      containingDirectoryId: evidence.containingDirectoryId,
      generation: evidence.generation,
      catalogSize: evidence.catalogSize,
      authority: budget,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: this.#namespaceCleanupRequest(),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    return this.#openWorkspaceExecution(intent, {
      kind: 'single-file',
      evidence,
    }, result.content, signal)
  }

  async prepareZip(
    intent: ReceiveIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      const existing = requirePreparation(this.#preparation)
      const verified = await sealWorkspaceZipPreparation({
        receiveIntent: intent,
        preparationId: existing.manifest.preparationId,
        generations: evidence.generations,
        entries: evidence.entries,
      })
      if (verified.manifest.digest !== existing.manifest.digest ||
          verified.zipLayout.digest !== existing.zipLayout.digest) {
        throw new TypeError('Workspace ZIP continuation changed its admitted preparation')
      }
      return this.#reopenWorkspaceExecution(intent, {
        kind: 'prepared',
        evidence,
      }, signal)
    }
    const preparationId = createOperationID()
    await this.#stages.beginReceive(preparationId)
    const preparation = await sealWorkspaceZipPreparation({
      receiveIntent: intent,
      preparationId,
      generations: evidence.generations,
      entries: evidence.entries,
    })
    const budget = await this.#budgetAuthority()
    const result = await this.#stages.admitPreparedZip({
      preparation,
      authority: budget,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: this.#namespaceCleanupRequest(),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    this.#preparation = preparation
    return this.#openWorkspaceExecution(intent, {
      kind: 'prepared',
      evidence,
    }, result.content, signal)
  }

  async #reopenWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    const admitted = this.#admitted
    if (admitted === undefined) {
      throw new DOMException('Workspace continuation lost its admission authority', 'InvalidStateError')
    }
    if (this.#closeAuthority !== undefined) {
      const backend = this.#backend
      if (backend === undefined) {
        throw new DOMException('Workspace continuation lost its reopened backend', 'InvalidStateError')
      }
      return this.#createWorkspaceExecution(intent, admission, backend, signal)
    }
    const claim = this.#budgetClaim
    if (claim === undefined) {
      throw new DOMException('Workspace continuation lost its budget authority', 'InvalidStateError')
    }
    const reopened = await this.#stages.reopenAdmittedContent({
      budget: admitted.budget,
      claim,
    })
    return this.#openWorkspaceExecution(intent, admission, reopened, signal)
  }

  async #openWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    content: AdmittedWorkspaceContent,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    this.#admitted = content
    this.#budgetClaim = content.claim as OriginPrivateWorkspaceBudgetClaim
    this.#backend = await openOriginPrivateWorkspaceBackend({
      receiveIntent: intent,
      operationRepository: this.#repository,
      namespace: this.#namespace,
      contentGate: content.gate,
      budgetClaim: this.#budgetClaim,
      ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
    })
    return this.#createWorkspaceExecution(intent, admission, this.#backend, signal)
  }

  async #createWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    backend: OriginPrivateWorkspaceBackend,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    const settlement = this.#workspaceSettlement(backend)
    const execution = await createPersistentWorkspaceExecution({
      intent: intent as Parameters<typeof createPersistentWorkspaceExecution>[0]['intent'],
      admission: admission as Parameters<typeof createPersistentWorkspaceExecution>[0]['admission'],
      materialization: backend.materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'browser-origin-private-workspace',
        outputSessionId: createOutputSessionID(),
      }),
      settlement,
      signal,
    } as Parameters<typeof createPersistentWorkspaceExecution>[0])
    this.#admissionSettlement.markExecutionAdmitted()
    return Object.freeze({ kind: 'accepted', execution })
  }

  #workspaceSettlement(
    backend: OriginPrivateWorkspaceBackend,
  ): PersistentWorkspaceSettlementAuthority {
    return Object.freeze({
      pause: async (
        _request: PlanPauseRequest,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
      ) => {
        await cut.closeMaterialization()
        const files = cut.evidence.entries.filter(entry => entry.kind === 'file')
        const completedBytes = files.reduce((total, entry) => total + entry.exactSize, 0n)
        return this.#stages.pauseReceive({
          checkpointSetDigest: await checkpointSetDigest(this.intent, cut.evidence),
          completedFileCount: BigInt(files.length),
          completedBytes,
        })
      },
      settle: async (
        request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
        signal: AbortSignal,
      ) => {
        if (request.transferJobId !== this.#transferJobId) {
          throw new TypeError('Workspace settlement escaped its active transfer attempt')
        }
        await cut.closeMaterialization()
        const sealed = await this.#stages.sealMaterialization({
          transferJobId: request.transferJobId,
          generations: cut.evidence.generations,
          entries: cut.evidence.entries,
          checkpoints: backend.finalCheckpoints,
          ...(this.#preparation === undefined ? {} : { preparation: this.#preparation }),
        })
        this.#sealBundle = Object.freeze({
          sealed,
          ...(this.#preparation === undefined ? {} : { preparation: this.#preparation }),
        })
        const result = await this.#package(sealed, signal)
        return result.state
      },
    })
  }

  async #package(
    sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>,
    signal: AbortSignal,
    retry = false,
  ): Promise<Exclude<OriginPrivatePackageAttemptResult, { kind: 'cleanup-pending' }>> {
    const backend = this.#backend
    if (backend === undefined) throw new DOMException('Workspace backend is unavailable', 'InvalidStateError')
    const workflow = new OriginPrivatePackageWorkflow({ stages: this.#stages, store: backend.packages })
    let result: OriginPrivatePackageAttemptResult = this.intent.artifact.kind === 'zip-archive'
      ? await workflow.buildZip({
          receiveIntentDigest: this.intent.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          layout: requirePreparation(this.#preparation).zipLayout,
          signal,
          retry,
        })
      : await workflow.buildOriginalFile({
          receiveIntentDigest: this.intent.digest,
          artifactSpecDigest: this.intent.artifact.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          signal,
          retry,
        })
    for (let attempt = 0; result.kind === 'cleanup-pending' &&
        attempt < PACKAGE_CLEANUP_RETRY_LIMIT; attempt += 1) {
      result = await result.retryCleanup()
    }
    if (result.kind === 'cleanup-pending') {
      throw new DOMException('Workspace package cleanup remains pending', 'OperationError')
    }
    if (result.kind === 'sealed') this.#packageExactBytes = result.package.exactBytes
    return result
  }

  async #continue(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    if (lifecycle.kind === 'resumable-package') {
      const seal = this.#sealBundle?.sealed
      if (seal === undefined) throw new DOMException('Package continuation proof is unavailable', 'InvalidStateError')
      const result = await this.#package(seal, new AbortController().signal, true)
      return Object.freeze({
        lifecycle: result.state,
        workspaceUsage: this.resolveWorkspaceUsage(result.state),
      })
    }
    if (lifecycle.kind !== 'resumable-receive' || this.#admitted === undefined ||
        (this.#closeAuthority === undefined && this.#budgetClaim === undefined)) throw unavailableRoute()
    if (this.#closeAuthority === undefined) {
      await this.#backend?.close()
      this.#backend = undefined
    }
    const plans = await workspacePlanAuthority(this.intent, this)
    const current = await this.#stages.resumeReceive()
    this.#admissionSettlement.beginContinuation(lifecycle)
    this.#transferJobId = createTransferJobID()
    this.#plans = plans
    return Object.freeze({
      lifecycle: current,
      activeControls: this.activeControls,
      workspaceUsage: this.resolveWorkspaceUsage(current),
      resumeTransfer: true,
    })
  }

  async #handoff(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    if (lifecycle.kind !== 'waiting-to-save' &&
        !(lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace')) {
      throw unavailableRoute()
    }
    const backend = this.#backend
    if (backend === undefined) throw new DOMException('Retained package backend is unavailable', 'InvalidStateError')
    const state = await handoffRetainedWorkspacePackage(
      this.#window,
      Object.freeze({ intent: this.intent, lifecycle, stages: this.#stages }),
      backend,
    )
    return Object.freeze({ lifecycle: state, workspaceUsage: this.resolveWorkspaceUsage(state) })
  }

  async #discard(): Promise<V2LifecycleMutation> {
    const result = await this.#stages.discard(this.#cleanupRequest())
    return Object.freeze({ lifecycle: result.state, workspaceUsage: null })
  }

  async #budgetAuthority(): Promise<OriginPrivateWorkspaceBudgetAuthority> {
    return OriginPrivateWorkspaceBudgetAuthority.open(this.intent.operationId, {
      estimate: () => this.#window.navigator.storage.estimate(),
    })
  }

  #cleanupRequest(): WorkspaceCleanupRequest {
    return this.#backend === undefined
      ? this.#namespaceCleanupRequest()
      : Object.freeze({ targets: Object.freeze([]), port: this.#backend.cleanup })
  }

  #namespaceCleanupRequest(): WorkspaceCleanupRequest {
    return Object.freeze({
      targets: Object.freeze([]),
      port: new NamespaceOnlyCleanupPort(this.#namespace, this.#repository, this.intent),
    })
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}
