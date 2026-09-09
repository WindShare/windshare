import { reopenProgressiveZipContinuation } from './progressive-continuation'
import { recoverLocalWorkspaceArtifact } from './artifact-recovery'
import { sealCompletedOriginalFile } from './original-continuation'
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
import {
  readWorkspacePackageCleanupAuthority,
  reopenWorkspacePackageContinuation,
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
 * lease ownership and cross-backend lifecycle selection remain with
 * the enclosing reopen authority.
 */
export class WorkspaceContinuationAuthority {
  readonly #options: WorkspaceContinuationAuthorityOptions
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
    this.#options = options
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

  async resumeProgressiveZip(input: WorkspaceContinuationInput, localOnly: boolean, partialExport: boolean): Promise<ReopenLifecycleAuthority> {
    const stages = await this.openStages(input.repository, input.snapshot, input.lease, input.diagnostics)
    return reopenProgressiveZipContinuation(input, this.#options, stages, localOnly, partialExport)
  }

  async recoverArtifact(input: WorkspaceContinuationInput): Promise<ReopenLifecycleAuthority> {
    const stages = await this.openStages(input.repository, input.snapshot, input.lease, input.diagnostics)
    return recoverLocalWorkspaceArtifact(input, stages, this.#checkpointDatabaseName)
  }

  async resumeReceive(
    input: WorkspaceContinuationInput,
    admissionFallback: Extract<ReceiveLifecycleState, {
      kind: 'resumable-receive'
      payloadKind: 'file-set'
    }>,
  ): Promise<ReopenLifecycleAuthority> {
    const stages = await this.openStages(
      input.repository,
      input.snapshot,
      input.lease,
      input.diagnostics,
    )
    const admission = await this.#reclaimWorkspaceAdmission(input)
    const lifecycle = await persistReceiveResume(
      input.repository,
      input.snapshot,
      input.lease,
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
    })
    return Object.freeze({
      lifecycle,
      receiveAdmissionFallback: admissionFallback,
      stages,
      admittedContent,
      receiveContinuation,
    })
  }

  async resumeOriginalFile(input: WorkspaceContinuationInput): Promise<ReopenLifecycleAuthority> {
    const stages = await this.openStages(input.repository, input.snapshot, input.lease, input.diagnostics)
    const admission = await readPersistedWorkspaceAdmission(input.repository, input.snapshot.operation.receiveIntent)
    const lifecycle = await sealCompletedOriginalFile({
      authority: input, stages, budget: admission.budget, now: this.#now(),
      ...(this.#checkpointDatabaseName === undefined ? {} : { checkpointDatabaseName: this.#checkpointDatabaseName }),
    })
    return this.resumePackage({ ...input, snapshot: { ...input.snapshot, lifecycle } })
  }

  async resumePackage(
    input: WorkspaceContinuationInput,
  ): Promise<ReopenLifecycleAuthority> {
    if (input.snapshot.lifecycle.kind !== 'resumable-package' &&
        input.snapshot.lifecycle.kind !== 'materialization-sealed' && input.snapshot.lifecycle.kind !== 'packaging') {
      throw new TypeError('package continuation requires sealed materialization')
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
      return await this.#openPackageContinuation(input, stages, admission, admittedContent)
    } catch {
      return this.#ownershipAttention(input)
    }
  }

  async #openPackageContinuation(
    input: WorkspaceContinuationInput,
    stages: WorkspaceOperationStages,
    admission: Readonly<{ budget: AdmittedWorkspaceContent['budget']; receipt: PreparationAdmissionReceiptV1 }>,
    admittedContent: AdmittedWorkspaceContent,
  ): Promise<ReopenLifecycleAuthority> {
    if (input.snapshot.lifecycle.kind !== 'resumable-package' &&
        input.snapshot.lifecycle.kind !== 'materialization-sealed' && input.snapshot.lifecycle.kind !== 'packaging') {
      throw new TypeError('Package recovery lost sealed materialization')
    }
      const cleanupReceipt = input.snapshot.lifecycle.kind === 'resumable-package'
        ? await readWorkspacePackageCleanupAuthority({
            repository: input.repository,
            intent: input.snapshot.operation.receiveIntent,
            lifecycle: input.snapshot.lifecycle,
          })
        : undefined
      const reopened = await reopenWorkspacePackageContinuation({
        repository: input.repository,
        intent: input.snapshot.operation.receiveIntent,
        lifecycle: input.snapshot.lifecycle,
        namespace: input.target.namespace,
        stages,
        admitted: admittedContent,
        admissionReceipt: admission.receipt,
        ...(cleanupReceipt === undefined ? {} : { cleanupReceipt }),
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
        lifecycle: reopened.lifecycle,
        stages,
        admittedContent,
        packageContinuation: reopened.continuation,
      })
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
  }>): ReopenedWorkspaceReceiveContinuation {
    return Object.freeze({
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
