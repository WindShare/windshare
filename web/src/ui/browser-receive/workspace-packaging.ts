import type { OutputDiagnosticsPorts } from '../../output/diagnostics'
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
import { checkpointSetDigest, unavailableRoute } from './shared'

interface WorkspaceSealBundle {
  readonly sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>
}

export interface WorkspaceContinuationPort {
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly admitted: boolean
  readonly hasReceiveAuthority: boolean
  closeOwnedBackend(): Promise<void>
  createPlans(): Promise<V2PlanExecutionAuthority>
  beginContinuation(
    lifecycle: Extract<ReceiveLifecycleState, {
      readonly kind: 'resumable-receive'
      readonly payloadKind: 'file-set'
    }>,
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
  #sealBundle: WorkspaceSealBundle | undefined
  #packageExactBytes: bigint | undefined

  constructor(input: {
    readonly windowPort: BrowserReceiveWindow
    readonly intent: ReceiveIntent
    readonly repository: ReceiveOperationRepository
    readonly namespace: OriginPrivateWorkspaceNamespace
    readonly stages: WorkspaceOperationStages
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.#window = input.windowPort
    this.#intent = input.intent
    this.#repository = input.repository
    this.#namespace = input.namespace
    this.#stages = input.stages
    this.#diagnostics = input.diagnostics
  }

  setPackageExactBytes(exactBytes: bigint): void {
    this.#packageExactBytes = exactBytes
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
        request: PlanPauseRequest,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
      ) => {
        await cut.closeMaterialization()
        const files = cut.evidence.entries.filter(entry => entry.kind === 'file')
        const completedBytes = files.reduce((total, entry) => total + entry.exactSize, 0n)
        return this.#stages.pauseReceive({
          checkpointSetDigest: await checkpointSetDigest(this.#intent, cut.evidence),
          completedFileCount: BigInt(files.length),
          completedBytes,
          selectionFacts: request.selectionFacts,
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
        })
        this.#sealBundle = Object.freeze({
          sealed,
        })
        const ownedBackend = this.#requireBackend(currentBackend())
        const result = await this.#package(ownedBackend, sealed, signal)
        return result.state.kind === 'waiting-to-save'
          ? (await this.#handoff(result.state, ownedBackend)).lifecycle
          : result.state
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
    if (lifecycle.kind === 'resumable-receive' && lifecycle.payloadKind !== 'direct-zip') {
      ownedBytes = lifecycle.payloadKind === 'opfs-zip' ? lifecycle.occupiedBytes : lifecycle.completedBytes
    }
    else if (this.#packageExactBytes !== undefined &&
        (lifecycle.kind === 'waiting-to-save' ||
         (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace'))) {
      ownedBytes = this.#packageExactBytes
    } else if (this.#sealBundle !== undefined) {
      ownedBytes = this.#sealBundle.sealed.manifest.rawBytes
    }
    return Object.freeze({ ownedBytes })
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
  ): Promise<OriginPrivatePackageAttemptResult> {
    const workflow = new OriginPrivatePackageWorkflow({
      stages: this.#stages,
      store: backend.packages,
      ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
    })
    const result = await workflow.buildOriginalFile({
          receiveIntentDigest: this.#intent.digest,
          artifactSpecDigest: this.#intent.artifact.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          signal,
        })
    this.#packageExactBytes = result.package.exactBytes
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
      const result = await this.#package(backend, seal, new AbortController().signal)
      return Object.freeze({
        lifecycle: result.state,
        workspaceUsage: this.resolveWorkspaceUsage(result.state),
      })
    }
    if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set' ||
        !continuation.admitted ||
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
