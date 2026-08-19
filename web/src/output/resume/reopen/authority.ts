import {
  verifyFSAOperationBinding,
} from '../../browser/indexeddb-root-binding'
import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../browser/session-lease'
import {
  reopenOriginPrivateWorkspaceNamespace,
} from '../../origin-private/namespace'
import { openOriginPrivateWorkspaceBackend } from '../../origin-private/session'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
  type OutputFailureSinks,
} from '../../diagnostics'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import {
  RECEIVE_RECORD_OPERATION,
  decodeStoredReceiveOperation,
  operationRecordId,
  storedReceiveOperationRecord,
} from '../../workspace/records'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import { lifecycleDeadline, type ReceiveLifecycleState } from '../../workspace/state'
import { WorkspaceOperationStages } from '../../workspace/stages'
import type { ReceiveOperationResumeDescriptor } from '../descriptor'
import {
  PersistedReceiveOperationDeadlineElapsedError,
  PersistedReceiveOperationNeedsAttentionError,
  type LeaseOptions,
  type PersistedReceiveOperationReopenAuthorityOptions,
  type PersistedReceiveOperationReopenPurpose,
  type PersistedReceiveOperationReopenTrace,
  type PersistedReceiveOperationReopenTraceEvent,
  type PersistedReopenSnapshot,
  type ReopenLifecycleAuthority,
  type ReopenResources,
  type ReopenedReceiveOperation,
  type ReopenedReceiveTarget,
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
  reclaimOriginPrivateWorkspaceBudget,
  samePersistedRecord,
} from './persistence'
import { WorkspaceContinuationAuthority } from './workspace-continuation-authority'

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
  readonly #workspaceContinuation: WorkspaceContinuationAuthority
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #trace: PersistedReceiveOperationReopenTrace | undefined

  constructor(options: PersistedReceiveOperationReopenAuthorityOptions) {
    this.#repositoryFactory = options.repositoryFactory
    this.#clock = options.clock ?? SYSTEM_CLOCK
    this.#leaseOptions = options.leaseOptions ?? {}
    this.#acquireLease = options.acquireLease ?? acquireBrowserReceiveOperationLease
    this.#verifyDirectTreeBinding = options.verifyDirectTreeBinding ?? verifyFSAOperationBinding
    this.#reopenWorkspaceNamespace = options.reopenWorkspaceNamespace ??
      reopenOriginPrivateWorkspaceNamespace
    this.#workspaceContinuation = new WorkspaceContinuationAuthority({
      openWorkspaceStages: options.openWorkspaceStages ?? WorkspaceOperationStages.open,
      reclaimWorkspaceBudget: options.reclaimWorkspaceBudget ??
        reclaimOriginPrivateWorkspaceBudget,
      estimateWorkspaceStorage: options.estimateWorkspaceStorage ??
        estimateOriginPrivateStorage,
      openWorkspaceReceiveBackend: options.openWorkspaceReceiveBackend ??
        openOriginPrivateWorkspaceBackend,
      contentRequests: options.contentRequests ?? ZERO_CONTENT_REQUESTS,
      now: () => this.#now(),
      ownershipAttention: input => this.#throwOwnershipAttention(
        input.repository,
        input.snapshot,
        input.lease,
        input.snapshot.operation.operationId,
      ),
      ...(options.workspaceBudgetDatabaseName === undefined
        ? {}
        : { workspaceBudgetDatabaseName: options.workspaceBudgetDatabaseName }),
      ...(options.checkpointDatabaseName === undefined
        ? {}
        : { checkpointDatabaseName: options.checkpointDatabaseName }),
      ...(options.openWorkspacePackageContinuation === undefined
        ? {}
        : { openWorkspacePackageContinuation: options.openWorkspacePackageContinuation }),
    })
    this.#diagnostics = options.diagnostics
    this.#trace = options.trace
  }

  async reopen(
    descriptor: ReceiveOperationResumeDescriptor,
    purpose: PersistedReceiveOperationReopenPurpose,
    failures?: OutputFailureSinks,
  ): Promise<ReopenedReceiveOperation> {
    const diagnostics = this.#diagnosticsFor(failures)
    this.#emitReviewedReopen('started')
    const repository = await this.#repositoryFactory().catch((error: unknown) => {
      recordOutputException(diagnostics?.failures?.reopen, error)
      this.#emitReviewedReopen('failed')
      throw error
    })
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
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
      const operation = await this.#createReopenedOperation({
        repository,
        snapshot,
        lease,
        target,
        lifecycleAuthority,
        resources,
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
      this.#emitReviewedReopen('authorized')
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
      recordOutputException(diagnostics?.failures?.reopen, error)
      this.#emitReviewedReopen('failed')
      return closeAfterFailure(
        repository,
        resources.lease,
        resources.reclaimedClaim,
        resources.packageBackend,
        error,
        cleanupFailure => this.#observeCleanupFailure(cleanupFailure, diagnostics),
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
    diagnostics?: OutputDiagnosticsPorts
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
      diagnostics?: OutputDiagnosticsPorts
    }>,
    observedAt: number,
  ): Promise<ReopenLifecycleAuthority> {
    if (input.snapshot.lifecycle.kind !== 'resumable-receive') {
      throw new TypeError('receive continuation lost its stable admission fallback')
    }
    if (input.target.kind === 'direct-tree') {
      return Object.freeze({
        lifecycle: await persistReceiveResume(
          input.repository,
          input.snapshot,
          input.lease,
          observedAt,
        ),
        receiveAdmissionFallback: input.snapshot.lifecycle,
      })
    }
    return this.#workspaceContinuation.resumeReceive(
      Object.freeze({ ...input, target: input.target }),
      observedAt,
      input.snapshot.lifecycle,
    )
  }

  async #resumePackage(input: Readonly<{
    repository: ReceiveOperationRepository
    snapshot: PersistedReopenSnapshot
    lease: BrowserReceiveOperationLease
    target: ReopenedReceiveTarget
    descriptor: ReceiveOperationResumeDescriptor
    resources: ReopenResources
    diagnostics?: OutputDiagnosticsPorts
  }>): Promise<ReopenLifecycleAuthority> {
    if (input.target.kind !== 'workspace' || input.snapshot.lifecycle.kind !== 'resumable-package') {
      throw new TypeError('package continuation requires a stable workspace operation')
    }
    return this.#workspaceContinuation.resumePackage(
      Object.freeze({ ...input, target: input.target }),
    )
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
    diagnostics?: OutputDiagnosticsPorts
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
        error => this.#observeCleanupFailure(error, input.diagnostics),
      ),
    }
    if (input.target.kind === 'direct-tree') {
      const fallback = input.lifecycleAuthority.receiveAdmissionFallback
      return Object.freeze({
        ...base,
        ...input.target,
        ...(fallback === undefined ? {} : { receiveAdmissionFallback: fallback }),
      })
    }
    const stages = input.lifecycleAuthority.stages ??
      await this.#workspaceContinuation.openStages(
        input.repository,
        input.snapshot,
        input.lease,
        input.diagnostics,
      )
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
      ...(input.lifecycleAuthority.receiveAdmissionFallback === undefined
        ? {}
        : { receiveAdmissionFallback: input.lifecycleAuthority.receiveAdmissionFallback }),
      ...(input.lifecycleAuthority.packageContinuation === undefined
        ? {}
        : { packageContinuation: input.lifecycleAuthority.packageContinuation }),
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

  #diagnosticsFor(failures: OutputFailureSinks | undefined): OutputDiagnosticsPorts | undefined {
    if (failures === undefined) return this.#diagnostics
    if (this.#diagnostics === undefined) {
      return Object.freeze({ backend: 'origin_private', failures })
    }
    return Object.freeze({ ...this.#diagnostics, failures })
  }

  #now(): number {
    const value = this.#clock.now()
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new TypeError('receive reopen clock must be a non-negative safe integer')
    }
    return value
  }

  #observeCleanupFailure(
    error: unknown,
    diagnostics: OutputDiagnosticsPorts | undefined,
  ): void {
    recordOutputException(
      diagnostics?.failures?.cleanup,
      error,
      { recoveryDisposition: 'needs_attention' },
    )
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent('cleanup', {
        backend: diagnostics?.backend === 'file_system_access'
          ? 'file_system_access'
          : 'origin_private',
        transition: 'failed',
      }))
  }

  #emitReviewedReopen(
    transition: 'started' | 'authorized' | 'failed',
  ): void {
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('reopen', {
        backend: this.#diagnostics?.backend === 'file_system_access'
          ? 'file_system_access'
          : 'origin_private',
        transition,
      }))
  }

  #emit(event: PersistedReceiveOperationReopenTraceEvent): void {
    try {
      this.#trace?.(event)
    } catch {
      // Durable ownership decisions remain authoritative when telemetry is unavailable.
    }
  }
}