import {
  verifyFSAOperationBinding,
} from '../../browser/indexeddb-root-binding'
import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../browser/session-lease'
import type { OriginPrivateStorageEstimate } from '../../origin-private/admission'
import { OriginPrivateWorkspaceBudgetOwnershipError } from '../../origin-private/admission-authority'
import {
  reopenOriginPrivateWorkspaceNamespace,
} from '../../origin-private/namespace'
import { openOriginPrivateWorkspaceBackend } from '../../origin-private/session'
import type { PersistentTreeTrace } from '../../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import {
  RECEIVE_RECORD_OPERATION,
  decodeStoredReceiveOperation,
  operationRecordId,
  storedReceiveOperationRecord,
} from '../../workspace/records'
import type { PreparationAdmissionReceiptV1 } from '../../workspace/receipts'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import { lifecycleDeadline, type ReceiveLifecycleState } from '../../workspace/state'
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
import type { ReceiveOperationResumeDescriptor } from '../descriptor'
import {
  PersistedReceiveOperationDeadlineElapsedError,
  PersistedReceiveOperationNeedsAttentionError,
  PersistedWorkspaceBudgetReclaimRejectedError,
  type LeaseOptions,
  type PersistedReceiveOperationReopenAuthorityOptions,
  type PersistedReceiveOperationReopenPurpose,
  type PersistedReceiveOperationReopenTrace,
  type PersistedReceiveOperationReopenTraceEvent,
  type PersistedReopenSnapshot,
  type PersistedWorkspaceBudgetReclaim,
  type ReopenLifecycleAuthority,
  type ReopenResources,
  type ReopenedReceiveOperation,
  type ReopenedReceiveTarget,
  type ReopenedWorkspaceReceiveContinuation,
} from './model'
import {
  SYSTEM_CLOCK,
  ZERO_CONTENT_REQUESTS,
  assertDescriptorAuthority,
  closeAfterFailure,
  closeAuthority,
  estimateOriginPrivateStorage,
  expectedBindingRecord,
  persistExpiry,
  persistOwnershipAttention,
  persistReceiveResume,
  readPersistedWorkspaceAdmission,
  reclaimOriginPrivateWorkspaceBudget,
  requireOriginPrivateBudgetClaim,
  samePersistedRecord,
} from './persistence'

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
