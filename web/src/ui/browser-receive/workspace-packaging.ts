import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import type { OriginPrivateWorkspaceNamespace } from '../../output/origin-private/namespace'
import type { OriginPrivateWorkspaceBackend } from '../../output/origin-private/session'
import {
  OriginPrivatePackageWorkflow,
  type OriginPrivatePackageAttemptResult,
} from '../../output/origin-private/workflow'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import {
  type WorkspaceCleanupRequest,
  type WorkspaceOperationStages,
} from '../../output/workspace/stages'
import type { SealedWorkspaceZipPreparationV1 } from '../../output/workspace/preparation'
import { DEFAULT_OPFS_JOB_WORKSPACE_LIMIT } from '../../output/workspace/budget'
import { createTransferJobID, type ReceiveIntent } from '../../transfer/intent'
import type {
  PersistentMaterializationSettlementCut,
  PersistentWorkspaceSettlementAuthority,
  WorkspaceMaterializationEvidence,
} from '../../transfer/settlement/persistent-execution'
import type {
  PlanPauseRequest,
  PlanSettlementRequest,
  V2PlanExecutionAuthority,
} from '../../transfer/output-session'
import type { SuccessfulTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from '../v2-lifecycle-presentation'
import type { V2LifecycleMutation } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { NamespaceOnlyCleanupPort, handoffRetainedWorkspacePackage } from './workspace-publication'
import { checkpointSetDigest, requirePreparation, unavailableRoute } from './shared'

const PACKAGE_CLEANUP_RETRY_LIMIT = 3

interface WorkspaceSealBundle {
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>
}

export interface WorkspaceContinuationPort {
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly admitted: boolean
  readonly hasReceiveAuthority: boolean
  closeOwnedBackend(): Promise<void>
  createPlans(): Promise<V2PlanExecutionAuthority>
  beginContinuation(
    lifecycle: Extract<ReceiveLifecycleState, { readonly kind: 'resumable-receive' }>,
  ): void
  installTransferAttempt(plans: V2PlanExecutionAuthority, transferJobId: string): void
}

export class WorkspaceReceivePackaging {
  readonly #window: BrowserReceiveWindow
  readonly #intent: ReceiveIntent
  readonly #repository: ReceiveOperationRepository
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #stages: WorkspaceOperationStages
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #preparation: SealedWorkspaceZipPreparationV1 | undefined
  #sealBundle: WorkspaceSealBundle | undefined
  #packageExactBytes: bigint | undefined

  constructor(input: {
    readonly windowPort: BrowserReceiveWindow
    readonly intent: ReceiveIntent
    readonly repository: ReceiveOperationRepository
    readonly namespace: OriginPrivateWorkspaceNamespace
    readonly stages: WorkspaceOperationStages
    readonly diagnostics?: OutputDiagnosticsPorts
    readonly preparation?: SealedWorkspaceZipPreparationV1
  }) {
    this.#window = input.windowPort
    this.#intent = input.intent
    this.#repository = input.repository
    this.#namespace = input.namespace
    this.#stages = input.stages
    this.#diagnostics = input.diagnostics
    this.#preparation = input.preparation
  }

  get preparation(): SealedWorkspaceZipPreparationV1 | undefined {
    return this.#preparation
  }

  setPreparation(preparation: SealedWorkspaceZipPreparationV1): void {
    this.#preparation = preparation
  }

  get sealDigest(): string | undefined {
    return this.#sealBundle?.sealed.seal.digest
  }

  settlement(
    backend: OriginPrivateWorkspaceBackend,
    currentTransferJobId: () => string,
    currentBackend: () => OriginPrivateWorkspaceBackend | undefined,
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
          checkpointSetDigest: await checkpointSetDigest(this.#intent, cut.evidence),
          completedFileCount: BigInt(files.length),
          completedBytes,
        })
      },
      settle: async (
        request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
        signal: AbortSignal,
      ) => {
        if (request.transferJobId !== currentTransferJobId()) {
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
        const result = await this.#package(this.#requireBackend(currentBackend()), sealed, signal)
        return result.state
      },
    })
  }

  async startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
    backend: OriginPrivateWorkspaceBackend | undefined,
    continuation: WorkspaceContinuationPort,
  ): Promise<V2LifecycleMutation> {
    switch (action) {
      case 'continue':
        return this.#continue(lifecycle, backend, continuation)
      case 'save':
      case 'redownload':
        return this.#handoff(lifecycle, backend)
      case 'discard':
      case 'delete':
        return this.#discard(backend)
      case 'change-location':
        throw unavailableRoute()
    }
  }

  async observeExpiry(
    backend: OriginPrivateWorkspaceBackend | undefined,
  ): Promise<V2LifecycleMutation> {
    const result = await this.#stages.expireIfDue(this.cleanupRequest(backend))
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

  cleanupRequest(backend: OriginPrivateWorkspaceBackend | undefined): WorkspaceCleanupRequest {
    return backend === undefined
      ? Object.freeze({
          targets: Object.freeze([]),
          port: new NamespaceOnlyCleanupPort(this.#namespace, this.#repository, this.#intent),
        })
      : Object.freeze({ targets: Object.freeze([]), port: backend.cleanup })
  }

  async #package(
    backend: OriginPrivateWorkspaceBackend,
    sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>,
    signal: AbortSignal,
    retry = false,
  ): Promise<Exclude<OriginPrivatePackageAttemptResult, { kind: 'cleanup-pending' }>> {
    const workflow = new OriginPrivatePackageWorkflow({
      stages: this.#stages,
      store: backend.packages,
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    let result: OriginPrivatePackageAttemptResult = this.#intent.artifact.kind === 'zip-archive'
      ? await workflow.buildZip({
          receiveIntentDigest: this.#intent.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          layout: requirePreparation(this.#preparation).zipLayout,
          signal,
          retry,
        })
      : await workflow.buildOriginalFile({
          receiveIntentDigest: this.#intent.digest,
          artifactSpecDigest: this.#intent.artifact.digest,
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
      const error = new DOMException('Workspace package cleanup remains pending', 'OperationError')
      recordOutputException(
        this.#diagnostics?.failures?.cleanup,
        error,
        { recoveryDisposition: 'needs_attention' },
      )
      emitOutputTrace(this.#diagnostics?.trace, () =>
        outputTraceEvent('cleanup', {
          backend: 'origin_private',
          transition: 'failed',
        }))
      throw error
    }
    if (result.kind === 'sealed') this.#packageExactBytes = result.package.exactBytes
    return result
  }

  async #continue(
    lifecycle: ReceiveLifecycleState,
    backend: OriginPrivateWorkspaceBackend | undefined,
    continuation: WorkspaceContinuationPort,
  ): Promise<V2LifecycleMutation> {
    if (lifecycle.kind === 'resumable-package') {
      const seal = this.#sealBundle?.sealed
      if (seal === undefined) {
        throw new DOMException('Package continuation proof is unavailable', 'InvalidStateError')
      }
      if (backend === undefined) {
        throw new DOMException('Workspace backend is unavailable', 'InvalidStateError')
      }
      const result = await this.#package(backend, seal, new AbortController().signal, true)
      return Object.freeze({
        lifecycle: result.state,
        workspaceUsage: this.resolveWorkspaceUsage(result.state),
      })
    }
    if (lifecycle.kind !== 'resumable-receive' || !continuation.admitted ||
        !continuation.hasReceiveAuthority) throw unavailableRoute()
    await continuation.closeOwnedBackend()
    const plans = await continuation.createPlans()
    const current = await this.#stages.resumeReceive()
    continuation.beginContinuation(lifecycle)
    continuation.installTransferAttempt(plans, createTransferJobID())
    return Object.freeze({
      lifecycle: current,
      activeControls: continuation.activeControls,
      workspaceUsage: this.resolveWorkspaceUsage(current),
      resumeTransfer: true,
    })
  }

  async #handoff(
    lifecycle: ReceiveLifecycleState,
    backend: OriginPrivateWorkspaceBackend | undefined,
  ): Promise<V2LifecycleMutation> {
    if (lifecycle.kind !== 'waiting-to-save' &&
        !(lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace')) {
      throw unavailableRoute()
    }
    if (backend === undefined) {
      throw new DOMException('Retained package backend is unavailable', 'InvalidStateError')
    }
    const state = await handoffRetainedWorkspacePackage(
      this.#window,
      Object.freeze({ intent: this.#intent, lifecycle, stages: this.#stages }),
      backend,
      this.#diagnostics,
    )
    return Object.freeze({ lifecycle: state, workspaceUsage: this.resolveWorkspaceUsage(state) })
  }

  async #discard(
    backend: OriginPrivateWorkspaceBackend | undefined,
  ): Promise<V2LifecycleMutation> {
    const result = await this.#stages.discard(this.cleanupRequest(backend))
    return Object.freeze({ lifecycle: result.state, workspaceUsage: null })
  }

  #requireBackend(
    backend: OriginPrivateWorkspaceBackend | undefined,
  ): OriginPrivateWorkspaceBackend {
    if (backend === undefined) {
      throw new DOMException('Workspace backend is unavailable', 'InvalidStateError')
    }
    return backend
  }
}
