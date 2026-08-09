import type { ReceiveIntent } from '../../transfer/intent'
import {
  verifyFSAOperationBinding,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import { IndexedDbReceiveOperationRepository } from '../browser/indexeddb-repository'
import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
  type BrowserReceiveOperationLeaseOptions,
} from '../browser/session-lease'
import {
  discardReopenedFileSystemAccessOutput,
  type FreshPageFileSystemAccessDiscardResult,
} from '../file-system-access/fresh-page-discard'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateStorageEstimate,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../origin-private/admission'
import { OriginPrivateWorkspaceBudgetOwnershipError } from '../origin-private/admission-authority'
import {
  reopenOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../origin-private/namespace'
import {
  openOriginPrivateWorkspaceBackend,
  openOriginPrivateRetainedArtifactBackend,
  type OriginPrivatePackageContinuationBackend,
  type OriginPrivateRetainedArtifactBackend,
  type OriginPrivateWorkspaceBackend,
} from '../origin-private/session'
import type { PersistentTreeTrace } from '../persistent-tree/contracts'
import { OriginPrivateWorkspaceRoot } from '../origin-private/workspace-root'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { reduceReceiveLifecycle } from '../workspace/lifecycle'
import {
  RECEIVE_RECORD_OPERATION,
  RECEIVE_RECORD_RECEIPT,
  RECEIVE_RECORD_RESERVATION,
  RECEIVE_RECORD_WORKSPACE_BINDING,
  createPersistedReceiveRecord,
  decodeStoredReceiveOperation,
  operationRecordId,
  storedReceiveOperationRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationV1,
} from '../workspace/records'
import {
  createExpiryReceipt,
  decodePreparationAdmissionAuthority,
  persistedReceiptRecord,
  type ExpiryReceiptV1,
  type PreparationAdmissionReceiptV1,
} from '../workspace/receipts'
import type { ReceiveOperationRepository } from '../workspace/repository'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../workspace/state-codec'
import {
  lifecycleDeadline,
  type PlanKind,
  type ReceiveLifecycleState,
} from '../workspace/state'
import {
  WorkspaceOperationStages,
  type AdmittedWorkspaceContent,
  type WorkspaceBudgetClaim,
  type WorkspaceBudgetClaimResult,
  type WorkspaceContentRequestCounter,
} from '../workspace/stages'
import type { SealedWorkspaceZipPreparationV1 } from '../workspace/preparation'
import {
  readWorkspacePackageCleanupAuthority,
  reopenWorkspacePackageContinuation,
  reopenWorkspacePreparationAuthority,
  type OpenOriginPrivatePackageContinuation,
  type ReopenedWorkspacePackageContinuation,
} from './workspace-continuation'
import {
  receiveOperationResumeDescriptor,
  type ReceiveOperationResumeDescriptor,
} from './descriptor'
import type {
  ReceiveOperationDiscardResult,
  ReceiveOperationMutationPort,
} from './authority'

export type PersistedReceiveOperationReopenPurpose = 'continue' | 'cleanup'

export type PersistedReceiveOperationReopenTraceEvent =
  | Readonly<{
      name: 'receive.operation.reopen_authorized'
      operation_id: string
      receive_intent_digest: string
      lifecycle_generation: bigint
      continuation: ReceiveOperationResumeDescriptor['continuation']
      lease_id: string
    }>
  | Readonly<{
      name: 'receive.operation.expired'
      operation_id: string
      prior_stable_state: StableLifecycleKind
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.needs_attention'
      operation_id: string
      prior_state: ReceiveLifecycleState['kind']
      needs_attention_reason: 'target-ownership-unknown'
    }>

export type PersistedReceiveOperationReopenTrace = (
  event: PersistedReceiveOperationReopenTraceEvent,
) => void

export interface ReopenedReceiveOperationBase {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly lease: BrowserReceiveOperationLease
  readonly repository: ReceiveOperationRepository
  close(): Promise<void>
}

export interface ReopenedDirectTreeOperation extends ReopenedReceiveOperationBase {
  readonly kind: 'direct-tree'
  readonly binding: PersistedFSAOperationBinding
}

export interface ReopenedWorkspaceOperation extends ReopenedReceiveOperationBase {
  readonly kind: 'workspace'
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly stages: WorkspaceOperationStages
  /** Present only when resume-receive reclaimed the exact durable admission authority. */
  readonly admittedContent?: AdmittedWorkspaceContent
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly receiveContinuation?: ReopenedWorkspaceReceiveContinuation
  readonly packageContinuation?: ReopenedWorkspacePackageContinuation
}

export interface ReopenedWorkspaceReceiveContinuation {
  readonly preparation?: SealedWorkspaceZipPreparationV1
  openBackend(options?: { readonly onTrace?: PersistentTreeTrace }): Promise<OriginPrivateWorkspaceBackend>
}

export type ReopenedReceiveOperation =
  | ReopenedDirectTreeOperation
  | ReopenedWorkspaceOperation

export class PersistedReceiveOperationDeadlineElapsedError extends DOMException {
  readonly state: Extract<ReceiveLifecycleState, { kind: 'expired' }>
  readonly receipt: ExpiryReceiptV1

  constructor(
    state: Extract<ReceiveLifecycleState, { kind: 'expired' }>,
    receipt: ExpiryReceiptV1,
  ) {
    super('Receive operation retention deadline elapsed before reopen', 'InvalidStateError')
    this.state = state
    this.receipt = receipt
  }
}

export class PersistedReceiveOperationNeedsAttentionError extends DOMException {
  readonly state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>

  constructor(state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>) {
    super('Receive operation ownership requires attention', 'InvalidStateError')
    this.state = state
  }
}

export class PersistedWorkspaceBudgetReclaimRejectedError extends DOMException {
  readonly budgetDigest: string
  readonly reason: Extract<
    WorkspaceBudgetClaimResult,
    { kind: 'rejected' }
  >['admission']['reason']

  constructor(result: Extract<WorkspaceBudgetClaimResult, { kind: 'rejected' }>) {
    super('Retained workspace no longer fits the current storage budget', 'QuotaExceededError')
    this.budgetDigest = result.admission.budgetDigest
    this.reason = result.admission.reason
  }
}

type LeaseOptions = Omit<BrowserReceiveOperationLeaseOptions, 'acquireTransition'>

export interface PersistedWorkspaceBudgetReclaimInput {
  readonly intent: ReceiveIntent
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly repository: ReceiveOperationRepository
  readonly operationLease: BrowserReceiveOperationLease
  readonly budget: AdmittedWorkspaceContent['budget']
  readonly receipt: PreparationAdmissionReceiptV1
  readonly estimate: () => Promise<OriginPrivateStorageEstimate>
  readonly now: () => number
  readonly databaseName?: string
}

export type PersistedWorkspaceBudgetReclaim = (
  input: PersistedWorkspaceBudgetReclaimInput,
) => Promise<WorkspaceBudgetClaimResult>

export interface PersistedReceiveOperationReopenAuthorityOptions {
  readonly repositoryFactory: () => Promise<ReceiveOperationRepository>
  readonly clock?: { now(): number }
  readonly leaseOptions?: LeaseOptions
  readonly acquireLease?: typeof acquireBrowserReceiveOperationLease
  readonly verifyDirectTreeBinding?: typeof verifyFSAOperationBinding
  readonly reopenWorkspaceNamespace?: typeof reopenOriginPrivateWorkspaceNamespace
  readonly openWorkspaceStages?: typeof WorkspaceOperationStages.open
  readonly reclaimWorkspaceBudget?: PersistedWorkspaceBudgetReclaim
  readonly estimateWorkspaceStorage?: () => Promise<OriginPrivateStorageEstimate>
  readonly workspaceBudgetDatabaseName?: string
  readonly checkpointDatabaseName?: string
  readonly openWorkspacePackageContinuation?: OpenOriginPrivatePackageContinuation
  readonly openWorkspaceReceiveBackend?: typeof openOriginPrivateWorkspaceBackend
  readonly contentRequests?: WorkspaceContentRequestCounter
  readonly trace?: PersistedReceiveOperationReopenTrace
}

interface PersistedReopenSnapshot {
  readonly operation: ReceiveOperationV1
  readonly operationRecord: PersistedReceiveRecord
  readonly bindingRecord: PersistedReceiveRecord
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
}

type ReopenedReceiveTarget =
  | Readonly<{
      kind: 'direct-tree'
      binding: PersistedFSAOperationBinding
    }>
  | Readonly<{
      kind: 'workspace'
      namespace: OriginPrivateWorkspaceNamespace
    }>

interface ReopenResources {
  lease?: BrowserReceiveOperationLease
  reclaimedClaim?: WorkspaceBudgetClaim
  packageBackend?: OriginPrivatePackageContinuationBackend
  receiveBackend?: OriginPrivateWorkspaceBackend
  receiveBackendOpening?: Promise<OriginPrivateWorkspaceBackend>
  closed?: boolean
}

interface ReopenLifecycleAuthority {
  readonly lifecycle: ReceiveLifecycleState
  readonly stages?: WorkspaceOperationStages
  readonly admittedContent?: AdmittedWorkspaceContent
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly packageContinuation?: ReopenedWorkspacePackageContinuation
  readonly receiveContinuation?: ReopenedWorkspaceReceiveContinuation
}

/**
 * Turns an inventory projection into one operation-scoped authority. The caller
 * never supplies an intent, binding, handle, repository row, or lifecycle state.
 */
export class PersistedReceiveOperationReopenAuthority {
  readonly #repositoryFactory: () => Promise<ReceiveOperationRepository>
  readonly #clock: { now(): number }
  readonly #leaseOptions: LeaseOptions
  readonly #acquireLease: typeof acquireBrowserReceiveOperationLease
  readonly #verifyDirectTreeBinding: typeof verifyFSAOperationBinding
  readonly #reopenWorkspaceNamespace: typeof reopenOriginPrivateWorkspaceNamespace
  readonly #openWorkspaceStages: typeof WorkspaceOperationStages.open
  readonly #reclaimWorkspaceBudget: PersistedWorkspaceBudgetReclaim
  readonly #estimateWorkspaceStorage: () => Promise<OriginPrivateStorageEstimate>
  readonly #workspaceBudgetDatabaseName: string | undefined
  readonly #checkpointDatabaseName: string | undefined
  readonly #openWorkspacePackageContinuation: OpenOriginPrivatePackageContinuation | undefined
  readonly #openWorkspaceReceiveBackend: typeof openOriginPrivateWorkspaceBackend
  readonly #contentRequests: WorkspaceContentRequestCounter
  readonly #trace: PersistedReceiveOperationReopenTrace | undefined

  constructor(options: PersistedReceiveOperationReopenAuthorityOptions) {
    this.#repositoryFactory = options.repositoryFactory
    this.#clock = options.clock ?? SYSTEM_CLOCK
    this.#leaseOptions = options.leaseOptions ?? {}
    this.#acquireLease = options.acquireLease ?? acquireBrowserReceiveOperationLease
    this.#verifyDirectTreeBinding = options.verifyDirectTreeBinding ?? verifyFSAOperationBinding
    this.#reopenWorkspaceNamespace = options.reopenWorkspaceNamespace ??
      reopenOriginPrivateWorkspaceNamespace
    this.#openWorkspaceStages = options.openWorkspaceStages ?? WorkspaceOperationStages.open
    this.#reclaimWorkspaceBudget = options.reclaimWorkspaceBudget ??
      reclaimOriginPrivateWorkspaceBudget
    this.#estimateWorkspaceStorage = options.estimateWorkspaceStorage ??
      estimateOriginPrivateStorage
    this.#workspaceBudgetDatabaseName = options.workspaceBudgetDatabaseName
    this.#checkpointDatabaseName = options.checkpointDatabaseName
    this.#openWorkspacePackageContinuation = options.openWorkspacePackageContinuation
    this.#openWorkspaceReceiveBackend = options.openWorkspaceReceiveBackend ??
      openOriginPrivateWorkspaceBackend
    this.#contentRequests = options.contentRequests ?? ZERO_CONTENT_REQUESTS
    this.#trace = options.trace
  }

  async reopen(
    descriptor: ReceiveOperationResumeDescriptor,
    purpose: PersistedReceiveOperationReopenPurpose,
  ): Promise<ReopenedReceiveOperation> {
    const repository = await this.#repositoryFactory()
    const resources: ReopenResources = {}
    try {
      const { snapshot, lease } = await this.#acquireSnapshotAuthority(
        repository,
        descriptor,
        purpose,
        resources,
      )
      const target = await this.#reopenTarget(repository, snapshot, lease, descriptor)
      const lifecycleAuthority = await this.#advanceLifecycle({
        repository,
        snapshot,
        lease,
        target,
        descriptor,
        purpose,
        resources,
      })
      const operation = await this.#createReopenedOperation({
        repository,
        snapshot,
        lease,
        target,
        lifecycleAuthority,
        resources,
      })
      this.#emit(Object.freeze({
        name: 'receive.operation.reopen_authorized',
        operation_id: descriptor.operationId,
        receive_intent_digest: descriptor.receiveIntentDigest,
        lifecycle_generation: lifecycleAuthority.lifecycle.generation,
        continuation: descriptor.continuation,
        lease_id: lease.leaseId,
      }))
      return operation
    } catch (error) {
      return closeAfterFailure(
        repository,
        resources.lease,
        resources.reclaimedClaim,
        resources.packageBackend,
        error,
      )
    }
  }

  async #acquireSnapshotAuthority(
    repository: ReceiveOperationRepository,
    descriptor: ReceiveOperationResumeDescriptor,
    purpose: PersistedReceiveOperationReopenPurpose,
    resources: ReopenResources,
  ): Promise<Readonly<{
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
  }>> {
    let snapshot = await this.#readSnapshot(repository, descriptor)
    await assertDescriptorAuthority(descriptor, snapshot, this.#now(), purpose)
    const lease = await this.#acquireLease(repository, descriptor.operationId, {
      ...this.#leaseOptions,
      acquireTransition: {
        expectedLifecycleGeneration: snapshot.lifecycle.generation,
        records: [snapshot.operationRecord, snapshot.bindingRecord],
      },
    })
    resources.lease = lease

    // The acquisition transaction fences generation, lease replacement, and both
    // immutable records. This reread prevents a caller from observing a pre-fence cut.
    snapshot = await this.#readSnapshot(repository, descriptor)
    await assertDescriptorAuthority(descriptor, snapshot, this.#now(), purpose)
    return Object.freeze({ snapshot, lease })
  }

  async #reopenTarget(
    repository: ReceiveOperationRepository,
    snapshot: PersistedReopenSnapshot,
    lease: BrowserReceiveOperationLease,
    descriptor: ReceiveOperationResumeDescriptor,
  ): Promise<ReopenedReceiveTarget> {
    try {
      if (snapshot.operation.receiveIntent.plan.kind === 'direct-tree') {
        const binding = await this.#verifyDirectTreeBinding({
          repository,
          intent: snapshot.operation.receiveIntent,
        })
        return Object.freeze({ kind: 'direct-tree', binding })
      }
      const namespace = await this.#reopenWorkspaceNamespace({
        repository,
        receiveIntent: snapshot.operation.receiveIntent,
      })
      return Object.freeze({ kind: 'workspace', namespace })
    } catch (error) {
      if (!(error instanceof TargetOwnershipUnknownError)) throw error
      return this.#throwOwnershipAttention(repository, snapshot, lease, descriptor.operationId)
    }
  }

  async #advanceLifecycle(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
    target: ReopenedReceiveTarget
    descriptor: ReceiveOperationResumeDescriptor
    purpose: PersistedReceiveOperationReopenPurpose
    resources: ReopenResources
  }>): Promise<ReopenLifecycleAuthority> {
    const observedAt = this.#now()
    const deadline = lifecycleDeadline(input.snapshot.lifecycle)
    if (deadline !== undefined && observedAt >= deadline) {
      return Object.freeze({
        lifecycle: await this.#expire(input, observedAt),
      })
    }
    if (input.purpose !== 'continue') {
      return Object.freeze({ lifecycle: input.snapshot.lifecycle })
    }
    if (input.descriptor.continuation === 'resume-receive') {
      return this.#resumeReceive(input, observedAt)
    }
    if (input.descriptor.continuation === 'resume-package') {
      return this.#resumePackage(input)
    }
    return Object.freeze({ lifecycle: input.snapshot.lifecycle })
  }

  async #expire(
    input: Readonly<{
      repository: ReceiveOperationRepository
      snapshot: PersistedReopenSnapshot
      lease: BrowserReceiveOperationLease
      descriptor: ReceiveOperationResumeDescriptor
      purpose: PersistedReceiveOperationReopenPurpose
    }>,
    observedAt: number,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'expired' }>> {
    const expired = await persistExpiry(
      input.repository,
      input.snapshot,
      input.lease,
      observedAt,
    )
    this.#emit(Object.freeze({
      name: 'receive.operation.expired',
      operation_id: input.descriptor.operationId,
      prior_stable_state: expired.receipt.priorStableState,
      expires_at_ms: expired.receipt.expiresAt,
    }))
    if (input.purpose === 'continue') {
      throw new PersistedReceiveOperationDeadlineElapsedError(expired.state, expired.receipt)
    }
    return expired.state
  }

  async #resumeReceive(
    input: Readonly<{
      repository: ReceiveOperationRepository
      snapshot: PersistedReopenSnapshot
      lease: BrowserReceiveOperationLease
      target: ReopenedReceiveTarget
      descriptor: ReceiveOperationResumeDescriptor
      resources: ReopenResources
    }>,
    observedAt: number,
  ): Promise<ReopenLifecycleAuthority> {
    if (input.target.kind === 'direct-tree') {
      return Object.freeze({
        lifecycle: await persistReceiveResume(
          input.repository,
          input.snapshot,
          input.lease,
          observedAt,
        ),
      })
    }
    return this.#resumeWorkspace(Object.freeze({ ...input, target: input.target }), observedAt)
  }

  async #resumeWorkspace(
    input: Readonly<{
      repository: ReceiveOperationRepository
      snapshot: PersistedReopenSnapshot
      lease: BrowserReceiveOperationLease
      target: Extract<ReopenedReceiveTarget, { kind: 'workspace' }>
      descriptor: ReceiveOperationResumeDescriptor
      resources: ReopenResources
    }>,
    observedAt: number,
  ): Promise<ReopenLifecycleAuthority> {
    const stages = await this.#openStages(input.repository, input.snapshot, input.lease)
    const admission = await this.#reclaimWorkspaceAdmission(input)
    let preparation: SealedWorkspaceZipPreparationV1 | undefined
    try {
      preparation = await reopenWorkspacePreparationAuthority({
        repository: input.repository,
        intent: input.snapshot.operation.receiveIntent,
        admissionReceipt: admission.receipt,
      })
    } catch {
      return this.#throwOwnershipAttention(
        input.repository,
        input.snapshot,
        input.lease,
        input.descriptor.operationId,
      )
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
      repository: input.repository,
      snapshot: input.snapshot,
      target: input.target,
      admittedContent,
      resources: input.resources,
      ...(preparation === undefined ? {} : { preparation }),
    })
    return Object.freeze({
      lifecycle,
      stages,
      admittedContent,
      receiveContinuation,
      ...(preparation === undefined ? {} : { preparation }),
    })
  }

  #workspaceReceiveContinuation(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    target: Extract<ReopenedReceiveTarget, { kind: 'workspace' }>
    admittedContent: AdmittedWorkspaceContent
    preparation?: SealedWorkspaceZipPreparationV1
    resources: ReopenResources
  }>): ReopenedWorkspaceReceiveContinuation {
    return Object.freeze({
      ...(input.preparation === undefined ? {} : { preparation: input.preparation }),
      openBackend: (options?: { readonly onTrace?: PersistentTreeTrace }) => {
        if (input.resources.closed === true) {
          throw new DOMException('Receive continuation authority is closed', 'InvalidStateError')
        }
        const budgetClaim = requireOriginPrivateBudgetClaim(
          input.admittedContent.claim,
          input.snapshot.operation.operationId,
        )
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

  async #resumePackage(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
    target: ReopenedReceiveTarget
    descriptor: ReceiveOperationResumeDescriptor
    resources: ReopenResources
  }>): Promise<ReopenLifecycleAuthority> {
    if (input.target.kind !== 'workspace' || input.snapshot.lifecycle.kind !== 'resumable-package') {
      throw new TypeError('package continuation requires a stable workspace operation')
    }
    const workspaceInput = Object.freeze({ ...input, target: input.target })
    const stages = await this.#openStages(input.repository, input.snapshot, input.lease)
    const admission = await this.#reclaimWorkspaceAdmission(workspaceInput)
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
      return this.#throwOwnershipAttention(
        input.repository,
        input.snapshot,
        input.lease,
        input.descriptor.operationId,
      )
    }
  }

  async #reclaimWorkspaceAdmission(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
    target: Extract<ReopenedReceiveTarget, { kind: 'workspace' }>
    descriptor: ReceiveOperationResumeDescriptor
    resources: ReopenResources
  }>): Promise<Readonly<{
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
        now: () => this.#now(),
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
      return this.#throwOwnershipAttention(
        input.repository,
        input.snapshot,
        input.lease,
        input.descriptor.operationId,
      )
    }
  }

  async #throwOwnershipAttention(
    repository: ReceiveOperationRepository,
    snapshot: PersistedReopenSnapshot,
    lease: BrowserReceiveOperationLease,
    operationId: string,
  ): Promise<never> {
    const attention = await persistOwnershipAttention(repository, snapshot, lease, this.#now())
    this.#emit(Object.freeze({
      name: 'receive.operation.needs_attention',
      operation_id: operationId,
      prior_state: snapshot.lifecycle.kind,
      needs_attention_reason: 'target-ownership-unknown',
    }))
    throw new PersistedReceiveOperationNeedsAttentionError(attention)
  }

  async #createReopenedOperation(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
    target: ReopenedReceiveTarget
    lifecycleAuthority: ReopenLifecycleAuthority
    resources: ReopenResources
  }>): Promise<ReopenedReceiveOperation> {
    const base = {
      intent: input.snapshot.operation.receiveIntent,
      lifecycle: input.lifecycleAuthority.lifecycle,
      lease: input.lease,
      repository: input.repository,
      close: closeAuthority(
        input.repository,
        input.lease,
        input.resources,
      ),
    }
    if (input.target.kind === 'direct-tree') {
      return Object.freeze({ ...base, ...input.target })
    }
    const stages = input.lifecycleAuthority.stages ??
      await this.#openStages(input.repository, input.snapshot, input.lease)
    return Object.freeze({
      ...base,
      ...input.target,
      stages,
      ...(input.lifecycleAuthority.admittedContent === undefined
        ? {}
        : { admittedContent: input.lifecycleAuthority.admittedContent }),
      ...(input.lifecycleAuthority.preparation === undefined
        ? {}
        : { preparation: input.lifecycleAuthority.preparation }),
      ...(input.lifecycleAuthority.receiveContinuation === undefined
        ? {}
        : { receiveContinuation: input.lifecycleAuthority.receiveContinuation }),
      ...(input.lifecycleAuthority.packageContinuation === undefined
        ? {}
        : { packageContinuation: input.lifecycleAuthority.packageContinuation }),
    })
  }

  #openStages(
    repository: ReceiveOperationRepository,
    snapshot: PersistedReopenSnapshot,
    lease: BrowserReceiveOperationLease,
  ): Promise<WorkspaceOperationStages> {
    return this.#openWorkspaceStages({
      repository,
      receiveIntent: snapshot.operation.receiveIntent,
      leaseId: lease.leaseId,
      clock: () => this.#now(),
      contentRequests: this.#contentRequests,
    })
  }

  async #readSnapshot(
    repository: ReceiveOperationRepository,
    descriptor: ReceiveOperationResumeDescriptor,
  ): Promise<PersistedReopenSnapshot> {
    const [operationRecord, lifecycleRecord] = await Promise.all([
      repository.readRecord(operationRecordId(descriptor.operationId, RECEIVE_RECORD_OPERATION)),
      repository.readLifecycle(descriptor.operationId),
    ])
    if (operationRecord === undefined || lifecycleRecord === undefined) {
      throw new TypeError('persisted receive operation authority is incomplete')
    }
    const [operation, lifecycle] = await Promise.all([
      decodeStoredReceiveOperation(operationRecord),
      Promise.resolve(decodeStoredReceiveLifecycleState(lifecycleRecord)),
    ])
    const bindingRecord = await expectedBindingRecord(operation.receiveIntent)
    const storedBinding = await repository.readRecord(bindingRecord.id)
    if (!samePersistedRecord(bindingRecord, storedBinding)) {
      throw new TypeError('persisted receive plan binding changed its canonical authority')
    }
    return Object.freeze({
      operation,
      operationRecord: storedReceiveOperationRecord(operation),
      bindingRecord,
      lifecycle,
      lifecycleRecord,
    })
  }

  #now(): number {
    const value = this.#clock.now()
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new TypeError('receive reopen clock must be a non-negative safe integer')
    }
    return value
  }

  #emit(event: PersistedReceiveOperationReopenTraceEvent): void {
    try {
      this.#trace?.(event)
    } catch {
      // Durable ownership decisions remain authoritative when telemetry is unavailable.
    }
  }
}

export interface ReceiveOperationOwnedCleanupExecutor {
  cleanup(operation: ReopenedReceiveOperation): Promise<ReceiveOperationDiscardResult>
}

export interface PersistedReceiveOperationCleanupExecutorOptions {
  readonly checkpointDatabaseName?: string
  readonly discardDirectTree?: typeof discardReopenedFileSystemAccessOutput
  readonly openWorkspaceBackend?: typeof openOriginPrivateRetainedArtifactBackend
}

/** Each plan owner derives its own physical inventory before this seam projects the durable result. */
export class PersistedReceiveOperationCleanupExecutor
implements ReceiveOperationOwnedCleanupExecutor {
  readonly #checkpointDatabaseName: string | undefined
  readonly #discardDirectTree: typeof discardReopenedFileSystemAccessOutput
  readonly #openWorkspaceBackend: typeof openOriginPrivateRetainedArtifactBackend

  constructor(options: PersistedReceiveOperationCleanupExecutorOptions = {}) {
    this.#checkpointDatabaseName = options.checkpointDatabaseName
    this.#discardDirectTree = options.discardDirectTree ?? discardReopenedFileSystemAccessOutput
    this.#openWorkspaceBackend = options.openWorkspaceBackend ?? openOriginPrivateRetainedArtifactBackend
  }

  async cleanup(operation: ReopenedReceiveOperation): Promise<ReceiveOperationDiscardResult> {
    if (operation.kind === 'direct-tree') {
      return projectDirectTreeDiscard(await this.#discardDirectTree({
        operation,
        ...(this.#checkpointDatabaseName === undefined
          ? {}
          : { databaseName: this.#checkpointDatabaseName }),
      }))
    }
    let backend: OriginPrivateRetainedArtifactBackend | undefined
    try {
      backend = await this.#openWorkspaceBackend({
        receiveIntent: operation.intent,
        operationRepository: operation.repository,
        namespace: operation.namespace,
        ...(this.#checkpointDatabaseName === undefined
          ? {}
          : { checkpointDatabaseName: this.#checkpointDatabaseName }),
      })
      const request = await backend.cleanup.cleanupRequest()
      const result = operation.lifecycle.kind === 'expired' ||
          (operation.lifecycle.kind === 'published' && operation.lifecycle.cleanupState === 'cleanup-pending')
        ? await operation.stages.retryTerminalCleanup(request)
        : await operation.stages.discard(request)
      if (result.kind === 'retryable-failure') {
        throw new DOMException('Owned workspace cleanup must be retried', 'OperationError')
      }
      if (result.kind === 'needs-attention') {
        return Object.freeze({ kind: 'needs-attention', reason: 'cleanup-unknown' })
      }
      if (result.state.kind === 'discarded') {
        return Object.freeze({
          kind: 'discarded',
          cleanupReceiptDigest: result.receipt.digest,
        })
      }
      if (result.state.kind === 'published' || result.state.kind === 'expired') {
        return Object.freeze({
          kind: 'cleanup-completed',
          terminalState: result.state.kind,
          cleanupReceiptDigest: result.receipt.digest,
        })
      }
      throw new TypeError('workspace cleanup returned a non-terminal lifecycle')
    } finally {
      await backend?.close()
    }
  }
}

function projectDirectTreeDiscard(
  result: FreshPageFileSystemAccessDiscardResult,
): ReceiveOperationDiscardResult {
  if (result.lifecycle.kind === 'needs-attention') {
    if (result.lifecycle.reason === 'publication-unknown') {
      throw new TypeError('DirectTree discard cannot produce publication uncertainty')
    }
    return Object.freeze({ kind: 'needs-attention', reason: result.lifecycle.reason })
  }
  if (!('receiptDigest' in result)) {
    throw new TypeError('DirectTree discard omitted its durable receipt')
  }
  if (result.lifecycle.kind === 'partial-directory') {
    return Object.freeze({ kind: 'partial-directory', receiptDigest: result.receiptDigest })
  }
  if (result.lifecycle.kind === 'discarded') {
    return Object.freeze({ kind: 'discarded', cleanupReceiptDigest: result.receiptDigest })
  }
  return Object.freeze({
    kind: 'cleanup-completed',
    terminalState: 'expired',
    cleanupReceiptDigest: result.receiptDigest,
  })
}

export type AuthorityOwnedReceiveOperationMutationResult =
  | Readonly<{
      kind: 'continuation'
      continuation: AuthorityOwnedReceiveOperationContinuation
    }>
  | Readonly<{ kind: 'retention-cleanup'; result: ReceiveOperationDiscardResult }>

export type AuthorityOwnedReceiveOperationContinuation =
  | Readonly<{
      kind: 'direct-tree-receive'
      operation: ReopenedDirectTreeOperation
    }>
  | Readonly<{
      kind: 'workspace-receive'
      operation: ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'receiving' }>
        readonly admittedContent: AdmittedWorkspaceContent
        readonly receiveContinuation: ReopenedWorkspaceReceiveContinuation
      }
    }>
  | Readonly<{
      kind: 'workspace-package'
      operation: ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
        readonly packageContinuation: ReopenedWorkspacePackageContinuation
      }
    }>
  | Readonly<{
      kind: 'workspace-retained'
      operation: ReopenedWorkspaceOperation
    }>

/**
 * Presentation can consume a descriptor but cannot provide an intent, binding, or
 * cleanup result. The output-owned executor is the only component allowed to turn
 * ownership evidence into a discard receipt.
 */
export class AuthorityOwnedReceiveOperationMutationPort
implements ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> {
  readonly #reopen: PersistedReceiveOperationReopenAuthority
  readonly #cleanup: ReceiveOperationOwnedCleanupExecutor

  constructor(input: {
    readonly reopen: PersistedReceiveOperationReopenAuthority
    readonly cleanup: ReceiveOperationOwnedCleanupExecutor
  }) {
    this.#reopen = input.reopen
    this.#cleanup = input.cleanup
  }

  async resume(
    descriptor: ReceiveOperationResumeDescriptor,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    const operation = await this.#reopen.reopen(descriptor, 'continue')
    return Object.freeze({
      kind: 'continuation',
      continuation: classifyReopenedContinuation(operation),
    })
  }

  async expire(
    descriptor: ReceiveOperationResumeDescriptor,
  ): Promise<AuthorityOwnedReceiveOperationMutationResult> {
    const operation = await this.#reopen.reopen(descriptor, 'cleanup')
    try {
      return Object.freeze({
        kind: 'retention-cleanup',
        result: await this.#cleanup.cleanup(operation),
      })
    } finally {
      await operation.close()
    }
  }

  async discard(
    descriptor: ReceiveOperationResumeDescriptor,
  ): Promise<ReceiveOperationDiscardResult> {
    const operation = await this.#reopen.reopen(descriptor, 'cleanup')
    try {
      return await this.#cleanup.cleanup(operation)
    } finally {
      await operation.close()
    }
  }
}

function classifyReopenedContinuation(
  operation: ReopenedReceiveOperation,
): AuthorityOwnedReceiveOperationContinuation {
  if (operation.kind === 'direct-tree') {
    return Object.freeze({ kind: 'direct-tree-receive', operation })
  }
  if (operation.lifecycle.kind === 'receiving' && operation.admittedContent !== undefined &&
      operation.receiveContinuation !== undefined) {
    return Object.freeze({
      kind: 'workspace-receive',
      operation: operation as ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'receiving' }>
        readonly admittedContent: AdmittedWorkspaceContent
        readonly receiveContinuation: ReopenedWorkspaceReceiveContinuation
      },
    })
  }
  if (operation.lifecycle.kind === 'resumable-package' &&
      operation.packageContinuation !== undefined) {
    return Object.freeze({
      kind: 'workspace-package',
      operation: operation as ReopenedWorkspaceOperation & {
        readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
        readonly packageContinuation: ReopenedWorkspacePackageContinuation
      },
    })
  }
  return Object.freeze({ kind: 'workspace-retained', operation })
}

export function createPersistedReceiveOperationMutationPort(
  options: PersistedReceiveOperationReopenAuthorityOptions &
  PersistedReceiveOperationCleanupExecutorOptions,
): AuthorityOwnedReceiveOperationMutationPort {
  return new AuthorityOwnedReceiveOperationMutationPort({
    reopen: new PersistedReceiveOperationReopenAuthority(options),
    cleanup: new PersistedReceiveOperationCleanupExecutor(options),
  })
}

export type BrowserReceiveOperationMutationPortOptions =
  Omit<PersistedReceiveOperationReopenAuthorityOptions, 'repositoryFactory'> &
  PersistedReceiveOperationCleanupExecutorOptions

/** Production injection seam: inventory/UI supplies only a single-use descriptor. */
export function createBrowserReceiveOperationMutationPort(
  options: BrowserReceiveOperationMutationPortOptions = {},
): AuthorityOwnedReceiveOperationMutationPort {
  return createPersistedReceiveOperationMutationPort({
    ...options,
    repositoryFactory: () => IndexedDbReceiveOperationRepository.open(
      options.checkpointDatabaseName,
    ),
  })
}

async function expectedBindingRecord(intent: ReceiveIntent): Promise<PersistedReceiveRecord> {
  if (intent.plan.kind === 'direct-tree') {
    return createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_RESERVATION,
      canonicalBytes: intent.plan.reservation.canonicalBytes,
    })
  }
  if (intent.plan.kind === 'workspace-then-publish') {
    return createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_WORKSPACE_BINDING,
      canonicalBytes: intent.plan.workspace.canonicalBytes,
    })
  }
  throw new TypeError('persisted reopen supports only DirectTree and Workspace plans')
}

async function assertDescriptorAuthority(
  descriptor: ReceiveOperationResumeDescriptor,
  snapshot: PersistedReopenSnapshot,
  nowMilliseconds: number,
  purpose: PersistedReceiveOperationReopenPurpose,
): Promise<void> {
  const lifecycleProjection = await storedReceiveLifecycleState(descriptor.lifecycle)
  if (descriptor.operationId !== snapshot.operation.operationId ||
      descriptor.receiveIntentDigest !== snapshot.operation.receiveIntentDigest ||
      descriptor.lifecycleGeneration !== snapshot.lifecycle.generation ||
      !samePersistedRecord(snapshot.lifecycleRecord, lifecycleProjection)) {
    throw new DOMException('Receive resume descriptor is stale or foreign', 'InvalidStateError')
  }
  const current = receiveOperationResumeDescriptor(snapshot.lifecycle, nowMilliseconds)
  if (current === undefined) {
    throw new DOMException('Receive lifecycle has no reopen authority', 'InvalidStateError')
  }
  if (current.continuation === 'needs-attention') {
    throw new DOMException('Receive continuation requires explicit owner recovery', 'InvalidStateError')
  }
  if (purpose === 'continue') {
    const crossedDeadline = current.continuation === 'cleanup-expired' &&
      descriptor.expiresAt !== undefined && nowMilliseconds >= descriptor.expiresAt
    if ((!crossedDeadline && current.continuation !== descriptor.continuation) ||
        current.continuation === 'retry-cleanup') {
      throw new DOMException('Receive continuation is stale or inert', 'InvalidStateError')
    }
  }
}

async function persistReceiveResume(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Extract<ReceiveLifecycleState, { kind: 'receiving' }>> {
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: 'resume-started',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'receiving') {
    throw new TypeError('receive reopen did not enter Receiving')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

async function persistOwnershipAttention(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: 'ownership-unknown',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
    lastVerifiedRecordDigest: snapshot.operationRecord.digest,
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'needs-attention') {
    throw new TypeError('unknown reopen ownership did not become NeedsAttention')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

async function persistExpiry(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Readonly<{
  state: Extract<ReceiveLifecycleState, { kind: 'expired' }>
  receipt: ExpiryReceiptV1
}>> {
  const deadline = lifecycleDeadline(snapshot.lifecycle)
  if (deadline === undefined || nowMilliseconds < deadline) {
    throw new TypeError('receive expiry was requested before its stable deadline')
  }
  const receipt = await createExpiryReceipt({
    operationId: snapshot.operation.operationId,
    receiveIntentDigest: snapshot.operation.receiveIntentDigest,
    priorStableState: stableLifecycleKind(snapshot.lifecycle),
    expiresAt: deadline,
    cleanupState: 'cleanup-pending',
  })
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: 'expiry-observed',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
    expiryReceiptDigest: receipt.digest,
    cleanupState: 'cleanup-pending',
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'expired') {
    throw new TypeError('elapsed receive operation did not become Expired')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    records: [await persistedReceiptRecord(receipt)],
    lifecycle: reduction.state,
  })
  return Object.freeze({ state: reduction.state, receipt })
}

function reducerContext(
  intent: ReceiveIntent,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Readonly<{
  planKind: PlanKind
  preparationRequired: boolean
  activeLeaseId: string
  nowMilliseconds: number
}> {
  return Object.freeze({
    planKind: intent.plan.kind,
    preparationRequired: intent.plan.kind === 'workspace-then-publish' &&
      intent.plan.preparation === 'exact-zip',
    activeLeaseId: lease.leaseId,
    nowMilliseconds,
  })
}

type StableLifecycleKind =
  | 'resumable-receive'
  | 'resumable-package'
  | 'waiting-to-save'
  | 'download-started'

function stableLifecycleKind(state: ReceiveLifecycleState): StableLifecycleKind {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
    case 'download-started': return state.kind
    default: throw new TypeError('receive expiry requires a stable lifecycle')
  }
}

async function readPersistedWorkspaceAdmission(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
): Promise<Readonly<{
  budget: AdmittedWorkspaceContent['budget']
  receipt: PreparationAdmissionReceiptV1
}>> {
  try {
    const records = await repository.listRecords(intent.operationId, RECEIVE_RECORD_RECEIPT)
    const authorities: Array<Readonly<{
      budget: AdmittedWorkspaceContent['budget']
      receipt: PreparationAdmissionReceiptV1
    }>> = []
    for (const record of records) {
      const authority = await decodePreparationAdmissionAuthority(record, intent)
      if (authority !== undefined) authorities.push(authority)
    }
    if (authorities.length !== 1) {
      throw new TypeError('workspace admission authority is missing or ambiguous')
    }
    return authorities[0]!
  } catch (error) {
    throw new TargetOwnershipUnknownError('reservation', intent.operationId, { cause: error })
  }
}

export async function reclaimOriginPrivateWorkspaceBudget(
  input: PersistedWorkspaceBudgetReclaimInput,
): Promise<WorkspaceBudgetClaimResult> {
  if (input.intent.plan.kind !== 'workspace-then-publish') {
    throw new TypeError('workspace budget reclaim requires a workspace receive intent')
  }
  const durableLease = await input.repository.readLease(input.intent.operationId)
  if (durableLease?.leaseId !== input.operationLease.leaseId ||
      input.operationLease.operationId !== input.intent.operationId ||
      input.receipt.operationId !== input.intent.operationId ||
      input.receipt.receiveIntentDigest !== input.intent.digest ||
      input.receipt.workspaceBudgetDigest !== input.budget.digest) {
    throw new OriginPrivateWorkspaceBudgetOwnershipError()
  }
  const root = new OriginPrivateWorkspaceRoot({
    operationId: input.intent.operationId,
    receiveIntentDigest: input.intent.digest,
    workspaceBindingDigest: input.intent.plan.workspace.digest,
    authorityRef: input.intent.plan.workspace.repositoryRef,
    workspaceRootHandleId: input.namespace.rootHandleId,
    workspaceRootHandle: input.namespace.root,
    repository: input.repository,
  })
  const authority = await OriginPrivateWorkspaceBudgetAuthority.open(input.intent.operationId, {
    estimate: input.estimate,
    verifiedAlreadyOwnedBytes: () => root.verifiedAlreadyOwnedBytes(),
    jobLimitBytes: input.receipt.jobLimitBytes,
    processLimitBytes: input.receipt.processLimitBytes,
    minimumReserveBytes: input.receipt.minimumReserveBytes,
    now: input.now,
    ...(input.databaseName === undefined ? {} : { databaseName: input.databaseName }),
  })
  return authority.reclaim(input.budget, input.operationLease)
}

function samePersistedRecord(
  expected: PersistedReceiveRecord,
  actual: PersistedReceiveRecord | undefined,
): boolean {
  if (actual === undefined || expected.id !== actual.id || expected.kind !== actual.kind ||
      expected.operationId !== actual.operationId || expected.digest !== actual.digest ||
      expected.reopenKey !== actual.reopenKey || expected.state !== actual.state ||
      expected.expiresAt !== actual.expiresAt ||
      expected.lifecycleGeneration !== actual.lifecycleGeneration ||
      expected.canonicalBytes.byteLength !== actual.canonicalBytes.byteLength) return false
  return expected.canonicalBytes.every((byte, index) => byte === actual.canonicalBytes[index])
}

function requireOriginPrivateBudgetClaim(
  claim: WorkspaceBudgetClaim,
  operationId: string,
): OriginPrivateWorkspaceBudgetClaim {
  if (!('readmit' in claim) || typeof claim.readmit !== 'function') {
    throw new TargetOwnershipUnknownError('reservation', operationId)
  }
  return claim as OriginPrivateWorkspaceBudgetClaim
}

function closeAuthority(
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  resources: ReopenResources,
): () => Promise<void> {
  let closed: Promise<void> | undefined
  return () => {
    closed ??= (async () => {
      resources.closed = true
      await resources.receiveBackendOpening?.catch(() => undefined)
      const releases = await Promise.allSettled([
        ...(resources.receiveBackend === undefined ? [] : [resources.receiveBackend.close()]),
        ...(resources.packageBackend === undefined ? [] : [resources.packageBackend.close()]),
        ...(resources.reclaimedClaim === undefined ? [] : [resources.reclaimedClaim.release()]),
        lease.release(),
      ])
      repository.close()
      const failures = releases.filter(
        (result): result is PromiseRejectedResult => result.status === 'rejected',
      )
      if (failures.length > 0) {
        throw new AggregateError(
          failures.map((result) => result.reason),
          'Receive reopen authority did not close cleanly',
        )
      }
    })()
    return closed
  }
}

async function closeAfterFailure(
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease | undefined,
  claim: WorkspaceBudgetClaim | undefined,
  packageBackend: OriginPrivatePackageContinuationBackend | undefined,
  failure: unknown,
): Promise<never> {
  const releases = await Promise.allSettled([
    ...(packageBackend === undefined ? [] : [packageBackend.close()]),
    ...(claim === undefined ? [] : [claim.release()]),
    ...(lease === undefined ? [] : [lease.release()]),
  ])
  repository.close()
  const releaseFailures = releases
    .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map((result) => result.reason)
  if (releaseFailures.length > 0) {
    throw new AggregateError(
      [failure, ...releaseFailures],
      'Receive reopen failure could not release its authority',
      { cause: releaseFailures[0] },
    )
  }
  throw failure
}

async function estimateOriginPrivateStorage(): Promise<OriginPrivateStorageEstimate> {
  const storage = globalThis.navigator?.storage
  if (storage === undefined || typeof storage.estimate !== 'function') {
    throw new DOMException('Origin-private storage estimate is unavailable', 'NotSupportedError')
  }
  return storage.estimate()
}

const SYSTEM_CLOCK = Object.freeze({ now: () => Date.now() })
const ZERO_CONTENT_REQUESTS = Object.freeze({ count: () => 0n })
