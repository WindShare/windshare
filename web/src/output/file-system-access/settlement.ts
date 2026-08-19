import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { verifyFSAOperationBinding } from '../browser/indexeddb-root-binding'
import {
  equalCanonicalBytes,
  snapshotIdentity,
} from '../workspace/canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../workspace/lifecycle'
import type { PersistedReceiveRecord } from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import { createDirectoryAdmissionScope, type DirectoryAdmissionScope } from '../../transfer/directory-admission'
import { snapshotTransferJobId } from '../../transfer/job/identity'
import type { ReceiveIntent } from '../../transfer/intent'
import type { PlanPauseRequest, PlanSettlementRequest } from '../../transfer/output-session'
import type { CompletedTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  PersistentDirectTreeSettlementAuthority,
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
} from '../../transfer/settlement/persistent-execution'
import {
  FileSystemAccessOutputSession,
  type FSAFinalSettlementObservation,
} from './session'
import {
  isFSAStableOrTerminal,
  sameReceiveAdmissionFallback,
  snapshotReceiveAdmissionFallback,
  type ReceiveAdmissionFallback,
} from './admission-fallback'
import {
  createFSASettlementReceipt,
  createFSAUnopenedCleanupReceipt,
  requireDirectTreeIntent,
  type DirectTreeIntent,
  type ObservedSettlementEvidence,
} from './settlement-proof'
import { observeFSASettlementEvidence } from './settlement-evidence'

export { fsaCheckpointSetDigest } from './settlement-proof'

export type FSASettlementRepository = Pick<
  ReceiveOperationRepository,
  'commitTransition' | 'readLifecycle' | 'readLease' | 'readRecord' | 'readHandle'
>

export type FSASettlementTraceEvent =
  | Readonly<{
      name: 'receive.fsa.settlement.completed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      outcome: 'published' | 'partial-directory' | 'resumable-receive' | 'discarded' | 'needs-attention'
      checkpoint_count: bigint
      completed_file_count: bigint
      completed_bytes: bigint
      ownership_stage?: TargetOwnershipUnknownError['stage']
    }>
  | Readonly<{
      name: 'receive.fsa.continuation.admission_failed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      restored_checkpoint_set_digest: string
      restored_completed_file_count: bigint
      restored_completed_bytes: bigint
      restored_expires_at_ms: number
    }>

export interface FileSystemAccessOperationSettlementAuthority {
  bindMaterialization(session: FileSystemAccessOutputSession): PersistentDirectTreeSettlementAuthority
  settleExecutionAdmissionFailure(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>>
}

export interface CreateFileSystemAccessSettlementAuthorityOptions {
  readonly intent: ReceiveIntent
  readonly repository: FSASettlementRepository
  readonly lifecycleLeaseId: string
  readonly transferJobId: string
  /** Exact durable state to restore when a continuation cannot admit an execution. */
  readonly admissionFallback?: ReceiveAdmissionFallback
  readonly clock?: () => number
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: (event: FSASettlementTraceEvent) => void
}

/**
 * Owns the reducer and durable receipt cut for one FSA operation. Transfer supplies
 * evidence, but only this authority can turn an observed namespace into lifecycle state.
 */
export async function createFileSystemAccessSettlementAuthority(
  options: CreateFileSystemAccessSettlementAuthorityOptions,
): Promise<FileSystemAccessOperationSettlementAuthority> {
  const intent = await requireDirectTreeIntent(options.intent)
  const admissionFallback = snapshotReceiveAdmissionFallback(intent, options.admissionFallback)
  return new FSAOperationSettlementAuthority({
    intent,
    repository: options.repository,
    lifecycleLeaseId: snapshotIdentity(options.lifecycleLeaseId, 16, 'lifecycle lease ID'),
    transferJobId: snapshotTransferJobId(options.transferJobId),
    ...(admissionFallback === undefined ? {} : { admissionFallback }),
    clock: options.clock ?? Date.now,
    ...(options.diagnostics === undefined
      ? {}
      : { diagnostics: options.diagnostics }),
    ...(options.trace === undefined ? {} : { trace: options.trace }),
    directoryScope: await createDirectoryAdmissionScope(intent),
  })
}

class FSAOperationSettlementAuthority implements FileSystemAccessOperationSettlementAuthority {
  readonly #intent: DirectTreeIntent
  readonly #repository: FSASettlementRepository
  readonly #lifecycleLeaseId: string
  readonly #transferJobId: string
  readonly #clock: () => number
  readonly #trace: CreateFileSystemAccessSettlementAuthorityOptions['trace']
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #directoryScope: DirectoryAdmissionScope
  readonly #admissionFallback: ReceiveAdmissionFallback | undefined
  #materializationBound = false

  constructor(input: Readonly<{
    intent: DirectTreeIntent
    repository: FSASettlementRepository
    lifecycleLeaseId: string
    transferJobId: string
    admissionFallback?: ReceiveAdmissionFallback
    clock: () => number
    diagnostics?: OutputDiagnosticsPorts
    trace?: (event: FSASettlementTraceEvent) => void
    directoryScope: DirectoryAdmissionScope
  }>) {
    this.#intent = input.intent
    this.#repository = input.repository
    this.#lifecycleLeaseId = input.lifecycleLeaseId
    this.#transferJobId = input.transferJobId
    this.#admissionFallback = input.admissionFallback
    this.#clock = input.clock
    this.#trace = input.trace
    this.#diagnostics = input.diagnostics
    this.#directoryScope = input.directoryScope
  }

  bindMaterialization(
    session: FileSystemAccessOutputSession,
  ): PersistentDirectTreeSettlementAuthority {
    this.#requireIntent(session.intent)
    if (!session.usesOperationRepository(this.#repository)) {
      throw new TypeError('FSA materialization and lifecycle must share one repository authority')
    }
    if (this.#materializationBound) {
      throw new DOMException('FSA settlement authority already has a materialization', 'InvalidStateError')
    }
    this.#materializationBound = true
    const authority: PersistentDirectTreeSettlementAuthority = {
      pause: (request, cut, signal) => this.#pause(session, request, cut, signal),
      settle: (request, cut, signal) => this.#settle(session, request, cut, signal),
    }
    return Object.freeze(authority)
  }

  async settleExecutionAdmissionFailure(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    signal.throwIfAborted()
    this.#requireIntent(intent)
    try {
      await verifyFSAOperationBinding({ repository: this.#repository, intent: this.#intent })
    } catch (cause) {
      const ownershipState = await this.#settleAdmissionOwnershipUnknown(cause)
      if (ownershipState !== undefined) return ownershipState
      throw cause
    }
    const ownershipState = await this.#settleAdmissionOwnershipUnknown(reason)
    if (ownershipState !== undefined) return ownershipState
    const current = await this.#lifecycle()
    if (this.#admissionFallback !== undefined &&
        sameReceiveAdmissionFallback(current.state, this.#admissionFallback)) {
      return current.state
    }
    if (isFSAStableOrTerminal(current.state)) return current.state
    if (current.state.kind === 'receiving' && this.#admissionFallback !== undefined &&
        current.state.generation === this.#admissionFallback.generation + 1n &&
        !this.#materializationBound) {
      const fallback = this.#admissionFallback
      const next = this.#reduce(current.state, {
        kind: 'resume-admission-failed',
        checkpointSetDigest: fallback.checkpointSetDigest,
        completedFileCount: fallback.completedFileCount,
        completedBytes: fallback.completedBytes,
        expiresAt: fallback.expiresAt,
        ...(fallback.partialReceiptDigest === undefined
          ? {}
          : { partialReceiptDigest: fallback.partialReceiptDigest }),
        expectedGeneration: current.state.generation,
        leaseId: this.#lifecycleLeaseId,
      })
      const committed = await this.#commitLifecycle(current, next)
      this.#emitAdmissionFailureRestored(fallback)
      return committed.state
    }
    const safelyUnopened = !this.#materializationBound &&
      (current.state.kind === 'intent-frozen' ||
       (current.state.kind === 'receiving' && this.#admissionFallback === undefined))
    if (!safelyUnopened) {
      return (await this.#recordNeedsAttention()).state
    }
    const receipt = await this.#unopenedCleanupReceipt()
    const next = this.#reduce(current.state, {
      kind: 'cleanup-verified',
      cleanupReceiptDigest: receipt.digest,
      expectedGeneration: current.state.generation,
      leaseId: this.#lifecycleLeaseId,
    })
    const committed = await this.#commitLifecycle(current, next, [receipt])
    this.#emit('discarded', 0n, 0n, 0n)
    return committed.state
  }

  async recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>> {
    if (signal.aborted) {
      // Settlement ambiguity is also a cancellation shield; unknown cannot be retried.
    }
    this.#requireIntent(intent)
    return (await this.#recordNeedsAttention()).state as Extract<
      ReceiveLifecycleState,
      { readonly kind: 'needs-attention' }
    >
  }

  async #pause(
    session: FileSystemAccessOutputSession,
    request: PlanPauseRequest,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    return this.#withMaterializationCut(session, cut, async (observation) => {
      if (signal.aborted) {
        // A requested pause is a cancellation shield: its durable cut must still finish.
      }
      if (request.worker.status !== 'Paused') {
        throw new TypeError('FSA pause requires a paused worker settlement')
      }
      const observed = await observeFSASettlementEvidence({
        intent: this.#intent,
        directoryScope: this.#directoryScope,
        observation,
        evidence: cut.evidence,
        summary: request.materialization,
        requireComplete: false,
      })
      const receipt = await this.#settlementReceipt('resumable-receive', request, observed)
      const current = await this.#lifecycle()
      if (current.state.kind !== 'receiving') {
        throw new TypeError('FSA pause requires Receiving lifecycle state')
      }
      const next = this.#reduce(current.state, {
        kind: 'pause-verified',
        stage: 'receive',
        checkpointSetDigest: observed.checkpointSetDigest,
        completedFileCount: observed.completedFileCount,
        completedBytes: observed.completedBytes,
        partialReceiptDigest: receipt.digest,
        expectedGeneration: current.state.generation,
        leaseId: this.#lifecycleLeaseId,
      })
      const committed = await this.#commitLifecycle(current, next, [receipt])
      this.#emit(
        'resumable-receive',
        BigInt(observed.checkpoints.length),
        observed.completedFileCount,
        observed.completedBytes,
      )
      return committed.state
    })
  }

  async #settle(
    session: FileSystemAccessOutputSession,
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    return this.#withMaterializationCut(session, cut, async (observation) => {
      signal.throwIfAborted()
      if (snapshotTransferJobId(request.transferJobId) !== this.#transferJobId) {
        throw new TypeError('FSA settlement escaped its transfer job')
      }
      const published = request.worker.status === 'Succeeded'
      const observed = await observeFSASettlementEvidence({
        intent: this.#intent,
        directoryScope: this.#directoryScope,
        observation,
        evidence: cut.evidence,
        summary: request.materialization,
        requireComplete: published,
      })
      const outcome = published ? 'published' as const : 'partial-directory' as const
      const receipt = await this.#settlementReceipt(outcome, request, observed)
      let current = await this.#lifecycle()
      if (current.state.kind === 'receiving') {
        const finalizing = this.#reduce(current.state, {
          kind: 'discovery-completed',
          expectedGeneration: current.state.generation,
          leaseId: this.#lifecycleLeaseId,
        })
        current = await this.#commitLifecycle(current, finalizing)
      }
      if (current.state.kind !== 'finalizing-tree') {
        throw new TypeError('FSA completion requires FinalizingTree lifecycle state')
      }
      const next = this.#reduce(current.state, {
        kind: 'tree-finalization-completed',
        outcome,
        receiptDigest: receipt.digest,
        completedFileCount: observed.completedFileCount,
        completedBytes: observed.completedBytes,
        successCount: request.materialization.entryCount,
        failureCount: BigInt(request.worker.failureCount),
        expectedGeneration: current.state.generation,
        leaseId: this.#lifecycleLeaseId,
      })
      let committed = await this.#commitLifecycle(current, next, [receipt])
      try {
        await observation.retireCheckpoints()
      } catch (cause) {
        committed = await this.#recordNeedsAttention(cause)
      }
      this.#emit(
        committed.state.kind === 'needs-attention' ? 'needs-attention' : outcome,
        BigInt(observed.checkpoints.length),
        observed.completedFileCount,
        observed.completedBytes,
      )
      return committed.state
    })
  }

  async #withMaterializationCut(
    session: FileSystemAccessOutputSession,
    cut: PersistentMaterializationSettlementCut<PersistentMaterializationEvidence>,
    operation: (observation: FSAFinalSettlementObservation) => Promise<ReceiveLifecycleState>,
  ): Promise<ReceiveLifecycleState> {
    let result: ReceiveLifecycleState | undefined
    let failure: unknown
    try {
      result = await session.runFinalSettlement(async (observation) => {
        try {
          return await operation(observation)
        } catch (cause) {
          if (!(cause instanceof TargetOwnershipUnknownError)) throw cause
          return (await this.#recordNeedsAttention(cause)).state
        }
      })
    } catch (cause) {
      failure = cause
      if (cause instanceof TargetOwnershipUnknownError) {
        try {
          result = (await this.#recordNeedsAttention(cause, false)).state
          failure = undefined
        } catch (attentionFailure) {
          failure = new AggregateError(
            [cause, attentionFailure],
            'FSA ownership and NeedsAttention persistence both failed',
          )
        }
      }
    }
    try {
      await cut.closeMaterialization()
    } catch (closeFailure) {
      failure = failure === undefined
        ? closeFailure
        : new AggregateError([failure, closeFailure], 'FSA settlement and materialization close failed')
    }
    if (failure !== undefined) throw failure
    if (result === undefined) throw new TypeError('FSA settlement produced no lifecycle state')
    return result
  }

  #settlementReceipt(
    outcome: 'published' | 'partial-directory' | 'resumable-receive',
    request: PlanPauseRequest | PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    evidence: ObservedSettlementEvidence,
  ): Promise<PersistedReceiveRecord> {
    return createFSASettlementReceipt({
      intent: this.#intent,
      transferJobId: this.#transferJobId,
      outcome,
      request,
      evidence,
    })
  }

  #unopenedCleanupReceipt(): Promise<PersistedReceiveRecord> {
    return createFSAUnopenedCleanupReceipt({
      intent: this.#intent,
      transferJobId: this.#transferJobId,
    })
  }

  async #lifecycle(): Promise<VerifiedLifecycle> {
    let record: PersistedReceiveRecord | undefined
    let lease: Awaited<ReturnType<FSASettlementRepository['readLease']>>
    try {
      [record, lease] = await Promise.all([
        this.#repository.readLifecycle(this.#intent.operationId),
        this.#repository.readLease(this.#intent.operationId),
      ])
    } catch (cause) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId, { cause })
    }
    if (record === undefined || lease === undefined ||
        lease.operationId !== this.#intent.operationId || lease.leaseId !== this.#lifecycleLeaseId) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
    }
    let state: ReceiveLifecycleState
    try {
      state = decodeStoredReceiveLifecycleState(record)
    } catch (cause) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId, { cause })
    }
    if (state.operationId !== this.#intent.operationId ||
        state.receiveIntentDigest !== this.#intent.digest ||
        ('activeLeaseId' in state && state.activeLeaseId !== this.#lifecycleLeaseId)) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
    }
    return Object.freeze({ state, record })
  }

  async #commitLifecycle(
    current: VerifiedLifecycle,
    next: ReceiveLifecycleState,
    records: readonly PersistedReceiveRecord[] = [],
  ): Promise<VerifiedLifecycle> {
    const expectedLifecycle = await storedReceiveLifecycleState(next)
    try {
      await this.#repository.commitTransition({
        operationId: this.#intent.operationId,
        expectedLifecycleGeneration: current.state.generation,
        expectedLeaseId: this.#lifecycleLeaseId,
        ...(records.length === 0 ? {} : { records }),
        lifecycle: next,
      })
    } catch (cause) {
      const observed = await this.#lifecycle().catch(() => undefined)
      if (observed !== undefined && observed.record.digest === expectedLifecycle.digest &&
          await this.#recordsMatch(records)) return observed
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId, { cause })
    }
    const observed = await this.#lifecycle()
    if (observed.record.digest !== expectedLifecycle.digest || !await this.#recordsMatch(records)) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
    }
    return observed
  }

  async #recordsMatch(records: readonly PersistedReceiveRecord[]): Promise<boolean> {
    try {
      for (const expected of records) {
        const actual = await this.#repository.readRecord(expected.id)
        if (actual === undefined || actual.digest !== expected.digest ||
            !equalCanonicalBytes(actual.canonicalBytes, expected.canonicalBytes)) return false
      }
      return true
    } catch {
      return false
    }
  }

  async #settleAdmissionOwnershipUnknown(
    reason: unknown,
  ): Promise<ReceiveLifecycleState | undefined> {
    if (!(reason instanceof TargetOwnershipUnknownError)) return undefined
    if (reason.operationId !== null && reason.operationId !== this.#intent.operationId) {
      throw new TypeError('FSA admission ownership evidence belongs to another operation', {
        cause: reason,
      })
    }
    return (await this.#recordNeedsAttention(reason)).state
  }

  async #recordNeedsAttention(
    cause?: unknown,
    classifyCause = true,
  ): Promise<VerifiedLifecycle> {
    if (cause !== undefined && classifyCause) {
      recordOutputException(this.#diagnostics?.failures?.settlement, cause)
    }
    const current = await this.#lifecycle()
    if (current.state.kind === 'needs-attention') return current
    if (cause === undefined) {
      this.#recordReviewedFailure('settlement', 'needs_attention')
    }
    this.#emitOutputSettlement('ownership_unknown')
    const next = this.#reduce(current.state, {
      kind: 'ownership-unknown',
      lastVerifiedRecordDigest: current.record.digest,
      expectedGeneration: current.state.generation,
      leaseId: this.#lifecycleLeaseId,
    })
    const committed = await this.#commitLifecycle(current, next)
    if (committed.state.kind !== 'needs-attention') {
      throw new TypeError('unknown FSA ownership did not become NeedsAttention')
    }
    this.#emit(
      'needs-attention',
      0n,
      0n,
      0n,
      cause instanceof TargetOwnershipUnknownError ? cause.stage : 'settlement',
    )
    return committed
  }

  #reduce(state: ReceiveLifecycleState, event: LifecycleEvent): ReceiveLifecycleState {
    const reduced = reduceReceiveLifecycle(state, event, {
      planKind: 'direct-tree',
      preparationRequired: false,
      activeLeaseId: this.#lifecycleLeaseId,
      nowMilliseconds: this.#now(),
    })
    if (reduced.status !== 'applied' || reduced.state === state) {
      throw new TypeError('FSA lifecycle transition was stale or side-effect free')
    }
    return reduced.state
  }

  #requireIntent(input: ReceiveIntent): void {
    if (input.operationId !== this.#intent.operationId || input.digest !== this.#intent.digest ||
        input.plan.kind !== 'direct-tree' ||
        input.plan.reservation.digest !== this.#intent.plan.reservation.digest) {
      throw new TypeError('FSA settlement authority belongs to another receive intent')
    }
  }

  #now(): number {
    const value = this.#clock()
    if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('FSA settlement clock is invalid')
    return value
  }

  #emit(
    outcome: Extract<
      FSASettlementTraceEvent,
      { name: 'receive.fsa.settlement.completed' }
    >['outcome'],
    checkpointCount: bigint,
    completedFileCount: bigint,
    completedBytes: bigint,
    ownershipStage?: TargetOwnershipUnknownError['stage'],
  ): void {
    this.#emitOutputSettlement('completed', normalizedSettlementOutcome(outcome))
    try {
      this.#trace?.(Object.freeze({
        name: 'receive.fsa.settlement.completed',
        operation_id: this.#intent.operationId,
        receive_intent_digest: this.#intent.digest,
        transfer_job_id: this.#transferJobId,
        outcome,
        checkpoint_count: checkpointCount,
        completed_file_count: completedFileCount,
        completed_bytes: completedBytes,
        ...(ownershipStage === undefined ? {} : { ownership_stage: ownershipStage }),
      }))
    } catch {
      // The persisted reducer state remains authoritative when telemetry is unavailable.
    }
  }

  #emitAdmissionFailureRestored(fallback: ReceiveAdmissionFallback): void {
    this.#recordReviewedFailure('continuation', 'resumable_receive')
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('continuation', {
        backend: 'file_system_access',
        transition: 'admission_failed',
      }))
    try {
      this.#trace?.(Object.freeze({
        name: 'receive.fsa.continuation.admission_failed',
        operation_id: this.#intent.operationId,
        receive_intent_digest: this.#intent.digest,
        transfer_job_id: this.#transferJobId,
        restored_checkpoint_set_digest: fallback.checkpointSetDigest,
        restored_completed_file_count: fallback.completedFileCount,
        restored_completed_bytes: fallback.completedBytes,
        restored_expires_at_ms: fallback.expiresAt,
      }))
    } catch {
      // Durable lifecycle restoration remains authoritative when telemetry is unavailable.
    }
  }

  #recordReviewedFailure(
    stage: 'settlement' | 'continuation',
    recoveryDisposition: 'needs_attention' | 'resumable_receive',
  ): void {
    try {
      const sink = stage === 'settlement'
        ? this.#diagnostics?.failures?.settlement
        : this.#diagnostics?.failures?.continuation
      sink?.record({ nativeClass: 'unknown', recoveryDisposition })
    } catch {
      // Reviewed facts remain observation-only when a custom sink rejects them.
    }
  }

  #emitOutputSettlement(
    transition: 'completed' | 'ownership_unknown',
    outcome?: 'published' | 'partial_directory' | 'resumable_receive' | 'discarded' | 'needs_attention',
  ): void {
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('settlement', {
        backend: 'file_system_access',
        transition,
        ...(outcome === undefined ? {} : { outcome }),
      }))
  }
}

function normalizedSettlementOutcome(
  outcome: Extract<
    FSASettlementTraceEvent,
    { name: 'receive.fsa.settlement.completed' }
  >['outcome'],
): 'published' | 'partial_directory' | 'resumable_receive' | 'discarded' | 'needs_attention' {
  return outcome === 'partial-directory' || outcome === 'resumable-receive' ||
      outcome === 'needs-attention'
    ? outcome.replace('-', '_') as
      | 'partial_directory'
      | 'resumable_receive'
      | 'needs_attention'
    : outcome
}

interface VerifiedLifecycle {
  readonly state: ReceiveLifecycleState
  readonly record: PersistedReceiveRecord
}
