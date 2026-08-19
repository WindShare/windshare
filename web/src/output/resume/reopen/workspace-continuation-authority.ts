import type { BrowserReceiveOperationLease } from '../../browser/session-lease'
import type { OutputDiagnosticsPorts } from '../../diagnostics'
import type { OriginPrivateStorageEstimate } from '../../origin-private/admission'
import { OriginPrivateWorkspaceBudgetOwnershipError } from '../../origin-private/admission-authority'
import {
  openOriginPrivateWorkspaceBackend,
  type OriginPrivateWorkspaceBackend,
} from '../../origin-private/session'
import type { PersistentTreeTrace } from '../../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import type { PreparationAdmissionReceiptV1 } from '../../workspace/receipts'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import type { ReceiveLifecycleState } from '../../workspace/state'
import {
  WorkspaceOperationStages,
  type AdmittedWorkspaceContent,
  type WorkspaceContentRequestCounter,
} from '../../workspace/stages'
import type { SealedWorkspaceZipPreparationV1 } from '../../workspace/preparation'
import {
  readWorkspacePackageCleanupAuthority,
  reopenWorkspacePackageContinuation,
  reopenWorkspacePreparationAuthority,
  type OpenOriginPrivatePackageContinuation,
} from '../workspace-continuation'
import {
  PersistedWorkspaceBudgetReclaimRejectedError,
  type PersistedReopenSnapshot,
  type PersistedWorkspaceBudgetReclaim,
  type ReopenLifecycleAuthority,
  type ReopenResources,
  type ReopenedReceiveTarget,
  type ReopenedWorkspaceReceiveContinuation,
} from './model'
import {
  persistReceiveResume,
  readPersistedWorkspaceAdmission,
  requireOriginPrivateBudgetClaim,
} from './persistence'

export interface WorkspaceContinuationAuthorityOptions {
  readonly openWorkspaceStages: typeof WorkspaceOperationStages.open
  readonly reclaimWorkspaceBudget: PersistedWorkspaceBudgetReclaim
  readonly estimateWorkspaceStorage: () => Promise<OriginPrivateStorageEstimate>
  readonly workspaceBudgetDatabaseName?: string
  readonly checkpointDatabaseName?: string
  readonly openWorkspacePackageContinuation?: OpenOriginPrivatePackageContinuation
  readonly openWorkspaceReceiveBackend: typeof openOriginPrivateWorkspaceBackend
  readonly contentRequests: WorkspaceContentRequestCounter
  readonly now: () => number
  readonly ownershipAttention: (input: WorkspaceOwnershipAttentionInput) => Promise<never>
}

export interface WorkspaceOwnershipAttentionInput {
  readonly repository: ReceiveOperationRepository
  readonly snapshot: PersistedReopenSnapshot
  readonly lease: BrowserReceiveOperationLease
}

export interface WorkspaceContinuationInput {
  readonly repository: ReceiveOperationRepository
  readonly snapshot: PersistedReopenSnapshot
  readonly lease: BrowserReceiveOperationLease
  readonly target: Extract<ReopenedReceiveTarget, { kind: 'workspace' }>
  readonly resources: ReopenResources
  readonly diagnostics?: OutputDiagnosticsPorts
}

/**
 * Progresses only an already-fenced workspace target. Repository acquisition,
 * lease ownership, deadlines, and cross-backend lifecycle selection remain with
 * the enclosing reopen authority.
 */
export class WorkspaceContinuationAuthority {
  readonly #openWorkspaceStages: typeof WorkspaceOperationStages.open
  readonly #reclaimWorkspaceBudget: PersistedWorkspaceBudgetReclaim
  readonly #estimateWorkspaceStorage: () => Promise<OriginPrivateStorageEstimate>
  readonly #workspaceBudgetDatabaseName: string | undefined
  readonly #checkpointDatabaseName: string | undefined
  readonly #openWorkspacePackageContinuation: OpenOriginPrivatePackageContinuation | undefined
  readonly #openWorkspaceReceiveBackend: typeof openOriginPrivateWorkspaceBackend
  readonly #contentRequests: WorkspaceContentRequestCounter
  readonly #now: () => number
  readonly #ownershipAttention: WorkspaceContinuationAuthorityOptions['ownershipAttention']

  constructor(options: WorkspaceContinuationAuthorityOptions) {
    this.#openWorkspaceStages = options.openWorkspaceStages
    this.#reclaimWorkspaceBudget = options.reclaimWorkspaceBudget
    this.#estimateWorkspaceStorage = options.estimateWorkspaceStorage
    this.#workspaceBudgetDatabaseName = options.workspaceBudgetDatabaseName
    this.#checkpointDatabaseName = options.checkpointDatabaseName
    this.#openWorkspacePackageContinuation = options.openWorkspacePackageContinuation
    this.#openWorkspaceReceiveBackend = options.openWorkspaceReceiveBackend
    this.#contentRequests = options.contentRequests
    this.#now = options.now
    this.#ownershipAttention = options.ownershipAttention
  }

  async resumeReceive(
    input: WorkspaceContinuationInput,
    observedAt: number,
    admissionFallback: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
  ): Promise<ReopenLifecycleAuthority> {
    const stages = await this.openStages(
      input.repository,
      input.snapshot,
      input.lease,
      input.diagnostics,
    )
    const admission = await this.#reclaimWorkspaceAdmission(input)
    let preparation: SealedWorkspaceZipPreparationV1 | undefined
    try {
      preparation = await reopenWorkspacePreparationAuthority({
        repository: input.repository,
        intent: input.snapshot.operation.receiveIntent,
        admissionReceipt: admission.receipt,
      })
    } catch {
      return this.#ownershipAttention(input)
    }
    const lifecycle = await persistReceiveResume(
      input.repository,
      input.snapshot,
      input.lease,
      observedAt,
    )
    const claim = input.resources.reclaimedClaim
    if (claim === undefined) throw new TypeError('workspace reopen omitted its budget claim')
    const admittedContent = await stages.reopenAdmittedContent({
      budget: admission.budget,
      claim,
    })
    const receiveContinuation = this.#workspaceReceiveContinuation({
      ...input,
      admittedContent,
      ...(preparation === undefined ? {} : { preparation }),
    })
    return Object.freeze({
      lifecycle,
      receiveAdmissionFallback: admissionFallback,
      stages,
      admittedContent,
      receiveContinuation,
      ...(preparation === undefined ? {} : { preparation }),
    })
  }

  async resumePackage(
    input: WorkspaceContinuationInput,
  ): Promise<ReopenLifecycleAuthority> {
    if (input.snapshot.lifecycle.kind !== 'resumable-package') {
      throw new TypeError('package continuation requires a stable workspace operation')
    }
    const stages = await this.openStages(
      input.repository,
      input.snapshot,
      input.lease,
      input.diagnostics,
    )
    const admission = await this.#reclaimWorkspaceAdmission(input)
    const claim = input.resources.reclaimedClaim
    if (claim === undefined) throw new TypeError('package reopen omitted its budget claim')
    try {
      const admittedContent = await stages.reopenAdmittedPackage({
        budget: admission.budget,
        claim,
      })
      const cleanupReceipt = await readWorkspacePackageCleanupAuthority({
        repository: input.repository,
        intent: input.snapshot.operation.receiveIntent,
        lifecycle: input.snapshot.lifecycle,
      })
      const reopened = await reopenWorkspacePackageContinuation({
        repository: input.repository,
        intent: input.snapshot.operation.receiveIntent,
        lifecycle: input.snapshot.lifecycle,
        namespace: input.target.namespace,
        stages,
        admitted: admittedContent,
        admissionReceipt: admission.receipt,
        cleanupReceipt,
        ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
        ...(this.#checkpointDatabaseName === undefined
          ? {}
          : { checkpointDatabaseName: this.#checkpointDatabaseName }),
        ...(this.#openWorkspacePackageContinuation === undefined
          ? {}
          : { openBackend: this.#openWorkspacePackageContinuation }),
      })
      input.resources.packageBackend = reopened.backend
      return Object.freeze({
        lifecycle: input.snapshot.lifecycle,
        stages,
        admittedContent,
        packageContinuation: reopened.continuation,
      })
    } catch {
      return this.#ownershipAttention(input)
    }
  }

  openStages(
    repository: ReceiveOperationRepository,
    snapshot: PersistedReopenSnapshot,
    lease: BrowserReceiveOperationLease,
    diagnostics?: OutputDiagnosticsPorts,
  ): Promise<WorkspaceOperationStages> {
    return this.#openWorkspaceStages({
      repository,
      receiveIntent: snapshot.operation.receiveIntent,
      leaseId: lease.leaseId,
      clock: this.#now,
      contentRequests: this.#contentRequests,
      ...(diagnostics === undefined ? {} : { diagnostics }),
    })
  }

  async #reclaimWorkspaceAdmission(
    input: WorkspaceContinuationInput,
  ): Promise<Readonly<{
    budget: AdmittedWorkspaceContent['budget']
    receipt: PreparationAdmissionReceiptV1
  }>> {
    try {
      const admission = await readPersistedWorkspaceAdmission(
        input.repository,
        input.snapshot.operation.receiveIntent,
      )
      const claimResult = await this.#reclaimWorkspaceBudget({
        intent: input.snapshot.operation.receiveIntent,
        namespace: input.target.namespace,
        repository: input.repository,
        operationLease: input.lease,
        budget: admission.budget,
        receipt: admission.receipt,
        estimate: this.#estimateWorkspaceStorage,
        now: this.#now,
        ...(this.#workspaceBudgetDatabaseName === undefined
          ? {}
          : { databaseName: this.#workspaceBudgetDatabaseName }),
      })
      if (claimResult.kind === 'rejected') {
        throw new PersistedWorkspaceBudgetReclaimRejectedError(claimResult)
      }
      input.resources.reclaimedClaim = claimResult.claim
      return admission
    } catch (error) {
      if (!(error instanceof TargetOwnershipUnknownError) &&
          !(error instanceof OriginPrivateWorkspaceBudgetOwnershipError)) throw error
      return this.#ownershipAttention(input)
    }
  }

  #workspaceReceiveContinuation(input: WorkspaceContinuationInput & Readonly<{
    admittedContent: AdmittedWorkspaceContent
    preparation?: SealedWorkspaceZipPreparationV1
  }>): ReopenedWorkspaceReceiveContinuation {
    return Object.freeze({
      ...(input.preparation === undefined ? {} : { preparation: input.preparation }),
      openBackend: (options?: {
        readonly onTrace?: PersistentTreeTrace
        readonly diagnostics?: OutputDiagnosticsPorts
      }): Promise<OriginPrivateWorkspaceBackend> => {
        if (input.resources.closed === true) {
          throw new DOMException('Receive continuation authority is closed', 'InvalidStateError')
        }
        const budgetClaim = requireOriginPrivateBudgetClaim(
          input.admittedContent.claim,
          input.snapshot.operation.operationId,
        )
        const diagnostics = options?.diagnostics ?? input.diagnostics
        input.resources.receiveBackendOpening ??= this.#openWorkspaceReceiveBackend({
          receiveIntent: input.snapshot.operation.receiveIntent,
          operationRepository: input.repository,
          namespace: input.target.namespace,
          contentGate: input.admittedContent.gate,
          budgetClaim,
          ...(this.#checkpointDatabaseName === undefined
            ? {}
            : { checkpointDatabaseName: this.#checkpointDatabaseName }),
          ...(options?.onTrace === undefined ? {} : { onTrace: options.onTrace }),
          ...(diagnostics === undefined ? {} : { diagnostics }),
        }).then(async (backend) => {
          if (input.resources.closed === true) {
            await backend.close()
            throw new DOMException('Receive continuation closed while opening', 'InvalidStateError')
          }
          input.resources.receiveBackend = backend
          return backend
        })
        return input.resources.receiveBackendOpening
      },
    })
  }
}
