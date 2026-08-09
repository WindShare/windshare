import { executeWorkspaceCleanup } from '../cleanup'
import {
  createCleanupReceipt,
  createExpiryReceipt,
  persistedReceiptRecord,
  type CleanupReceiptV1,
  type ExpiryReceiptV1,
} from '../receipts'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_OPERATION,
} from '../records'
import { lifecycleDeadline, type PreparationAdmissionReason, type ReceiveLifecycleState } from '../state'
import {
  mergeHandleIds,
  stableStateKind,
  type WorkspaceCleanupRequest,
  type WorkspaceCleanupResult,
} from './contracts'
import { WorkspaceStageRuntime } from './runtime'

export class WorkspaceCleanupStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) {
    this.runtime = runtime
  }

  async discard(request: WorkspaceCleanupRequest): Promise<WorkspaceCleanupResult> {
    const state = await this.runtime.lifecycle()
    if (state.kind === 'published' || state.kind === 'partial-directory' ||
        state.kind === 'restart-required' || state.kind === 'discarded' ||
        state.kind === 'expired') {
      throw new TypeError('workspace state cannot be discarded')
    }
    this.runtime.requireContinuationUnexpired(state)
    return this.finishCleanup(state, request, [])
  }

  async expireIfDue(request: WorkspaceCleanupRequest): Promise<Readonly<{
    kind: 'not-due'
    state: ReceiveLifecycleState
  }> | Readonly<{
    kind: 'expired'
    expiryReceipt: ExpiryReceiptV1
    cleanup: WorkspaceCleanupResult
  }>> {
    const state = await this.runtime.lifecycle()
    const expiresAt = lifecycleDeadline(state)
    if (expiresAt === undefined) throw new TypeError('workspace state has no stable deadline')
    const now = this.runtime.now()
    if (now < expiresAt) return Object.freeze({ kind: 'not-due', state })
    const priorStableState = stableStateKind(state)
    const expiryReceipt = await createExpiryReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      priorStableState,
      expiresAt,
      cleanupState: 'cleanup-pending',
    })
    const expired = this.runtime.reduceAt(state, this.runtime.event({
      kind: 'expiry-observed',
      expiryReceiptDigest: expiryReceipt.digest,
      cleanupState: 'cleanup-pending',
    }, state), now)
    if (expired.kind !== 'expired') throw new TypeError('elapsed workspace did not become Expired')
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(expiryReceipt)],
      lifecycle: expired,
    })
    this.runtime.emit({
      name: 'receive.operation.expired',
      operation_id: this.runtime.intent.operationId,
      prior_stable_state: expiryReceipt.priorStableState,
      expires_at_ms: expiryReceipt.expiresAt,
    })
    const cleanup = await this.finishCleanup(expired, request, [expiryReceipt.digest])
    return Object.freeze({ kind: 'expired', expiryReceipt, cleanup })
  }

  async retryTerminalCleanup(request: WorkspaceCleanupRequest): Promise<WorkspaceCleanupResult> {
    const state = await this.runtime.lifecycle()
    let keepReceiptDigests: readonly string[] | undefined
    if (state.kind === 'published' && state.cleanupState === 'cleanup-pending') {
      keepReceiptDigests = [state.receiptDigest]
    } else if (state.kind === 'expired' && state.cleanupState === 'cleanup-pending') {
      keepReceiptDigests = [state.expiryReceiptDigest]
    }
    if (keepReceiptDigests === undefined) {
      throw new TypeError('workspace has no retryable terminal cleanup')
    }
    return this.finishCleanup(state, request, keepReceiptDigests)
  }

  async finishCleanup(
    state: ReceiveLifecycleState,
    request: WorkspaceCleanupRequest,
    keepReceiptDigests: readonly string[],
    preparationRejectionReason?: PreparationAdmissionReason,
  ): Promise<WorkspaceCleanupResult> {
    const execution = await executeWorkspaceCleanup({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      targets: request.targets,
      port: request.port,
    })
    const cleanedHandleIds = mergeHandleIds(
      execution.cleanedHandleIds,
      execution.kind === 'clean' ? request.metadataHandleIds ?? [] : [],
    )
    if (execution.kind === 'retryable-failure') {
      await this.#persistPartialCleanup(state, execution, cleanedHandleIds)
      return Object.freeze({ kind: 'retryable-failure', state })
    }
    if (execution.kind === 'ownership-unknown') {
      const receipt = await this.#cleanupReceipt(
        state.generation + 1n,
        execution.removedObjectIds,
        execution.removedCheckpointRecordDigests,
      )
      const next = this.runtime.reduce(state, this.runtime.event({
        kind: 'cleanup-unknown',
        lastVerifiedRecordDigest: receipt.digest,
      }, state))
      if (next.kind !== 'needs-attention') {
        throw new TypeError('unknown workspace cleanup did not require attention')
      }
      await this.runtime.repository.commitTransition({
        operationId: this.runtime.intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.runtime.leaseId,
        records: [await persistedReceiptRecord(receipt)],
        deleteHandleIds: cleanedHandleIds,
        lifecycle: next,
      })
      this.runtime.emit({
        name: 'receive.operation.needs_attention',
        operation_id: this.runtime.intent.operationId,
        prior_state: state.kind,
        needs_attention_reason: 'cleanup-unknown',
      })
      return Object.freeze({ kind: 'needs-attention', receipt, state: next })
    }

    const keep = new Set(keepReceiptDigests)
    const [records, pages] = await Promise.all([
      this.runtime.repository.listRecords(this.runtime.intent.operationId),
      this.runtime.repository.listManifestPages(this.runtime.intent.operationId),
    ])
    const removedRecords = records.filter((record) =>
      record.kind !== RECEIVE_RECORD_OPERATION &&
      record.kind !== RECEIVE_RECORD_LIFECYCLE_STATE &&
      !keep.has(record.digest))
    const receipt = await this.#cleanupReceipt(
      state.generation + 1n,
      execution.removedObjectIds,
      [
        ...removedRecords.map((record) => record.digest),
        ...execution.removedCheckpointRecordDigests,
      ],
    )
    const next = preparationRejectionReason === undefined
      ? this.runtime.reduce(state, this.runtime.event({
          kind: 'cleanup-verified',
          cleanupReceiptDigest: receipt.digest,
        }, state))
      : this.runtime.reduce(state, this.runtime.event({
          kind: 'preparation-rejected',
          reason: preparationRejectionReason,
          cleanupReceiptDigest: receipt.digest,
        }, state))
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(receipt)],
      deleteRecordIds: removedRecords.map((record) => record.id),
      deleteManifestPageIds: pages.map((page) => page.id),
      deleteHandleIds: cleanedHandleIds,
      lifecycle: next,
    })
    this.runtime.emit({
      name: next.kind === 'discarded'
        ? 'receive.operation.discarded'
        : 'receive.operation.cleanup_completed',
      operation_id: this.runtime.intent.operationId,
      cleanup_generation: receipt.cleanupGeneration,
      removed_object_count: BigInt(receipt.removedObjectIds.length),
      removed_record_count: BigInt(receipt.removedRecordDigests.length),
    })
    return Object.freeze({ kind: 'clean', receipt, state: next })
  }

  async #persistPartialCleanup(
    state: ReceiveLifecycleState,
    execution: Readonly<{
      removedObjectIds: readonly string[]
      removedCheckpointRecordDigests: readonly string[]
    }>,
    cleanedHandleIds: readonly string[],
  ): Promise<void> {
    if (execution.removedObjectIds.length === 0 &&
        execution.removedCheckpointRecordDigests.length === 0 &&
        cleanedHandleIds.length === 0) return
    const receipt = await this.#cleanupReceipt(
      state.generation,
      execution.removedObjectIds,
      execution.removedCheckpointRecordDigests,
    )
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(receipt)],
      deleteHandleIds: cleanedHandleIds,
    })
  }

  #cleanupReceipt(
    cleanupGeneration: bigint,
    removedObjectIds: readonly string[],
    removedRecordDigests: readonly string[],
  ): Promise<CleanupReceiptV1> {
    return createCleanupReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      removedObjectIds,
      removedRecordDigests,
      cleanupGeneration,
    })
  }


}
