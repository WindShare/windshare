import type { BrowserReceiveOperationLease } from '../../output/browser/session-lease'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../output/origin-private/admission'
import type { OriginPrivateWorkspaceNamespace } from '../../output/origin-private/namespace'
import {
  openOriginPrivateWorkspaceBackend,
  type OriginPrivateWorkspaceBackend,
} from '../../output/origin-private/session'
import type { AuthorityOwnedReceiveOperationContinuation } from '../../output/resume/reopen-authority'
import { DEFAULT_OPFS_JOB_WORKSPACE_LIMIT } from '../../output/workspace/budget'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import {
  type AdmittedWorkspaceContent,
  type WorkspaceOperationStages,
  type WorkspaceStageTraceListener,
} from '../../output/workspace/stages'
import { sealWorkspaceZipPreparation } from '../../output/workspace/preparation'
import { createPersistentWorkspaceExecution } from '../../transfer/settlement/persistent-execution'
import type { V2ExecutionAdmissionLifecycle } from '../../transfer/settlement/v2-plan-authority'
import {
  createOperationID,
  createOutputSessionID,
  createTransferJobID,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  TransferPauseRequestedError,
  outputSessionIdentity,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type ExecutionAdmissionResult,
  type V2PlanExecutionAuthority,
  type WorkspaceExecution,
} from '../../transfer/output-session'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from '../v2-lifecycle-presentation'
import type { V2BoundReceiveOperation, V2LifecycleMutation } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import {
  WorkspaceExecutionAdmissionSettlement,
  type WorkspaceReceiveAdmissionFallback,
} from './workspace-admission'
import {
  WorkspaceReceivePackaging,
  type WorkspaceContinuationPort,
} from './workspace-packaging'
import { workspacePlanAuthority } from './workspace-publication'
import {
  readLifecycle,
  requireMatchingSingleFileAdmission,
  requirePreparation,
  requireSameIntent,
  unavailableRoute,
} from './shared'

type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' }
>

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
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #closeAuthority: (() => Promise<void>) | undefined
  readonly #packaging: WorkspaceReceivePackaging
  #plans!: V2PlanExecutionAuthority
  #transferJobId: string
  #backend: OriginPrivateWorkspaceBackend | undefined
  #admitted: AdmittedWorkspaceContent | undefined
  #budgetClaim: OriginPrivateWorkspaceBudgetClaim | undefined
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
    diagnostics?: OutputDiagnosticsPorts
    transferJobId: string
    backend?: OriginPrivateWorkspaceBackend
    admitted?: AdmittedWorkspaceContent
    preparation?: Parameters<WorkspaceReceivePackaging['setPreparation']>[0]
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
    this.#diagnostics = input.diagnostics
    this.#closeAuthority = input.closeAuthority
    this.#transferJobId = input.transferJobId
    this.#backend = input.backend
    this.#admitted = input.admitted
    this.#packaging = new WorkspaceReceivePackaging({
      windowPort: input.windowPort,
      intent: input.intent,
      repository: input.repository,
      namespace: input.namespace,
      stages: input.stages,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
      ...(input.preparation === undefined ? {} : { preparation: input.preparation }),
    })
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
    repository: ReceiveOperationRepository
    namespace: OriginPrivateWorkspaceNamespace
    lease: BrowserReceiveOperationLease
    stages: WorkspaceOperationStages
    trace?: WorkspaceStageTraceListener
    diagnostics?: OutputDiagnosticsPorts
  }): Promise<WorkspaceReceiveOperation> {
    const owner = new WorkspaceReceiveOperation({
      windowPort: input.windowPort,
      intent: input.intent,
      repository: input.repository,
      namespace: input.namespace,
      lease: input.lease,
      stages: input.stages,
      ...(input.trace === undefined ? {} : { trace: input.trace }),
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
      transferJobId: createTransferJobID(),
    })
    owner.#plans = await workspacePlanAuthority(input.intent, owner)
    return owner
  }

  static async reopen(input: {
    windowPort: BrowserReceiveWindow
    operation: WorkspaceReceiveContinuation['operation']
    backend: OriginPrivateWorkspaceBackend
    trace?: WorkspaceStageTraceListener
    diagnostics?: OutputDiagnosticsPorts
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
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
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
    return this.#packaging.startLifecycleAction(
      action,
      lifecycle,
      this.#backend,
      this.#continuationPort(),
    )
  }

  async observeExpiry(): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    return this.#packaging.observeExpiry(this.#backend)
  }

  resolveWorkspaceUsage(lifecycle: ReceiveLifecycleState): WorkspaceUsage | null {
    return this.#packaging.resolveWorkspaceUsage(lifecycle)
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
      this.#packaging.sealDigest ?? this.intent.digest,
    )
  }

  async detach(): Promise<void> {
    if (this.#detached) return
    this.#detached = true
    if (this.#closeAuthority !== undefined) {
      await this.#closeAuthority()
      return
    }
    const tasks: {
      readonly promise: Promise<unknown>
      readonly failureIsNative: boolean
    }[] = [{
      promise: this.#lease.release(),
      failureIsNative: true,
    }]
    if (this.#backend !== undefined) {
      // The backend classifies its file/checkpoint cleanup at the native boundary.
      tasks.push({ promise: this.#backend.close(), failureIsNative: false })
    }
    if (this.#budgetClaim !== undefined) {
      tasks.push({ promise: this.#budgetClaim.release(), failureIsNative: true })
    }
    const results = await Promise.allSettled(tasks.map(task => task.promise))
    const failures: unknown[] = []
    let outerFailureObserved = false
    for (const [index, result] of results.entries()) {
      if (result.status !== 'rejected') continue
      failures.push(result.reason)
      if (tasks[index]?.failureIsNative === true) {
        outerFailureObserved = true
        recordOutputException(this.#diagnostics?.failures?.cleanup, result.reason)
      }
    }
    try {
      this.#repository.close()
    } catch (error) {
      failures.push(error)
      outerFailureObserved = true
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    if (failures.length > 0) {
      const failure = new AggregateError(failures, 'Workspace receive resources did not detach')
      if (outerFailureObserved) {
        emitOutputTrace(this.#diagnostics?.trace, () =>
          outputTraceEvent('cleanup', {
            backend: 'origin_private',
            transition: 'failed',
          }))
      }
      throw failure
    }
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('cleanup', {
        backend: 'origin_private',
        transition: 'completed',
      }))
  }

  async admitOriginal(
    intent: ReceiveIntent,
    evidence: ExactSingleFileEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      requireMatchingSingleFileAdmission(this.#admitted, evidence)
      return this.#reopenWorkspaceExecution(intent, { kind: 'single-file', evidence }, signal)
    }
    const budget = await this.#budgetAuthority()
    const result = await this.#stages.admitSingleFile({
      fileId: evidence.fileId,
      containingDirectoryId: evidence.containingDirectoryId,
      generation: evidence.generation,
      catalogSize: evidence.catalogSize,
      authority: budget,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: this.#packaging.cleanupRequest(undefined),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    return this.#openWorkspaceExecution(intent, { kind: 'single-file', evidence }, result.content, signal)
  }

  async prepareZip(
    intent: ReceiveIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      const existing = requirePreparation(this.#packaging.preparation)
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
      return this.#reopenWorkspaceExecution(intent, { kind: 'prepared', evidence }, signal)
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
      rejectionCleanup: this.#packaging.cleanupRequest(undefined),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    this.#packaging.setPreparation(preparation)
    return this.#openWorkspaceExecution(intent, { kind: 'prepared', evidence }, result.content, signal)
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
    const reopened = await this.#stages.reopenAdmittedContent({ budget: admitted.budget, claim })
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
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
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
    const execution = await createPersistentWorkspaceExecution({
      intent: intent as Parameters<typeof createPersistentWorkspaceExecution>[0]['intent'],
      admission: admission as Parameters<typeof createPersistentWorkspaceExecution>[0]['admission'],
      materialization: backend.materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'browser-origin-private-workspace',
        outputSessionId: createOutputSessionID(),
      }),
      settlement: this.#packaging.settlement(
        backend,
        () => this.#transferJobId,
        () => this.#backend,
      ),
      signal,
    } as Parameters<typeof createPersistentWorkspaceExecution>[0])
    this.#admissionSettlement.markExecutionAdmitted()
    return Object.freeze({ kind: 'accepted', execution })
  }

  #continuationPort(): WorkspaceContinuationPort {
    return Object.freeze({
      activeControls: this.activeControls,
      admitted: this.#admitted !== undefined,
      hasReceiveAuthority: this.#closeAuthority !== undefined || this.#budgetClaim !== undefined,
      closeOwnedBackend: async () => {
        if (this.#closeAuthority !== undefined) return
        await this.#backend?.close()
        this.#backend = undefined
      },
      createPlans: () => workspacePlanAuthority(this.intent, this),
      beginContinuation: (
        lifecycle: Extract<ReceiveLifecycleState, { readonly kind: 'resumable-receive' }>,
      ) =>
        this.#admissionSettlement.beginContinuation(lifecycle),
      installTransferAttempt: (
        plans: V2PlanExecutionAuthority,
        transferJobId: string,
      ) => {
        this.#transferJobId = transferJobId
        this.#plans = plans
      },
    })
  }

  #discard(): Promise<V2LifecycleMutation> {
    return this.#packaging.startLifecycleAction(
      'discard',
      this.lifecycle,
      this.#backend,
      this.#continuationPort(),
    )
  }

  #budgetAuthority(): Promise<OriginPrivateWorkspaceBudgetAuthority> {
    return OriginPrivateWorkspaceBudgetAuthority.open(this.intent.operationId, {
      estimate: () => this.#window.navigator.storage.estimate(),
    })
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}
