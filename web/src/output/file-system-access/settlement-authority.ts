import {
  emitOutputTrace,
  observePerformance,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { verifyFSAOperationBinding } from '../browser/indexeddb-root-binding'
import { equalCanonicalBytes } from '../workspace/canonical'
import { MaterializationLedgerEvidenceOutcome } from '../materialization-ledger/evidence'
import { MaterializationLedgerSealPurpose } from '../materialization-ledger/model'
import type { FSATerminalMutationKind } from '../browser/mutation-coordination/model'
import {
  reduceReceiveLifecycle,
  type LifecycleEvent,
} from '../workspace/lifecycle'
import type { PersistedReceiveRecord } from '../workspace/records'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { DirectoryAdmissionScope } from '../../transfer/directory-admission'
import { snapshotTransferJobId } from '../../transfer/job/identity'
import type { ReceiveIntent } from '../../transfer/intent'
import {
  TransferStopRequestedError,
  type PlanPauseRequest,
  type PlanSettlementRequest,
  type PlanStopRequest,
} from '../../transfer/output-session'
import type { CompletedTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  PersistentDirectTreeSettlementAuthority,
  PersistentDirectTreeMaterializationEvidence,
  PersistentMaterializationSettlementCut,
} from '../../transfer/settlement/persistent-execution'
import {
  FileSystemAccessOutputSession,
  type FSAFinalSettlementObservation,
} from './session'
import {
  isFSAStableOrTerminal,
  receiveAdmissionFailureEvent,
  sameReceiveAdmissionFallback,
  type ReceiveAdmissionFallback,
} from './admission-fallback'
import {
  createFSASettlementReceipt,
  createFSAUnopenedCleanupReceipt,
  type DirectTreeIntent,
  type SettlementReceiptEvidence,
} from './settlement-proof'
import { validateSealedFSASettlementEvidence } from './settlement-evidence'
import { normalizedSettlementOutcome, terminalMutationKind } from './settlement-terminal'
import type { RecoverySummary } from './recovery-summary'
import { deriveSettledFSARecoverySummary } from './recovery-settlement'
import { COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION } from './compatible-name/model'
import type {
  CreateFileSystemAccessSettlementAuthorityOptions,
  FileSystemAccessOperationSettlementAuthority,
  FSASettlementRepository,
  FSASettlementTraceEvent,
} from './settlement'

export class FSAOperationSettlementAuthority implements FileSystemAccessOperationSettlementAuthority {
  readonly #intent: DirectTreeIntent
  readonly #repository: FSASettlementRepository
  readonly #lifecycleLeaseId: string
  readonly #transferJobId: string
  readonly #clock: () => number
  readonly #trace: CreateFileSystemAccessSettlementAuthorityOptions['trace']
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #directoryScope: DirectoryAdmissionScope
  readonly #admissionFallback: ReceiveAdmissionFallback | undefined
  #materializationActivationStarted = false
  #boundMaterialization: FileSystemAccessOutputSession | undefined
  #recoverySummary: RecoverySummary | undefined

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
    if (this.#materializationActivationStarted) {
      throw new DOMException('FSA settlement authority already has a materialization', 'InvalidStateError')
    }
    // Binding is the activation fence: from here a failed prepareRoot may have
    // produced namespace effects, even when no usable materialization was returned.
    this.#materializationActivationStarted = true
    this.#boundMaterialization = session
    const authority: PersistentDirectTreeSettlementAuthority = {
      beginTerminal: kind => session.beginTerminal(terminalMutationKind(kind)),
      recoverySummary: () => this.#recoverySummary,
      pause: (request, cut, signal) => this.#pause(session, request, cut, signal),
      stop: (request, cut, signal) => this.#stop(session, request, cut, signal),
      settle: (request, cut, signal) => this.#settle(session, request, cut, signal),
    }
    return Object.freeze(authority)
  }

  #isUnopenedInterruptedContinuation(state: ReceiveLifecycleState): boolean {
    return state.kind === 'receiving' && this.#admissionFallback?.kind === 'receiving' &&
      state.generation === this.#admissionFallback.generation + 1n &&
      !this.#materializationActivationStarted
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
    if (this.#isUnopenedInterruptedContinuation(current.state)) {
      // An interrupted receive has no invented pause receipt to restore. Its durable
      // files/checkpoints remain eligible for the next explicit authority reacquisition.
      return current.state
    }
    if (current.state.kind === 'receiving' && this.#admissionFallback?.kind === 'resumable-receive' &&
        current.state.generation === this.#admissionFallback.generation + 1n &&
        !this.#materializationActivationStarted) {
      const fallback = this.#admissionFallback
      const next = this.#reduce(current.state, receiveAdmissionFailureEvent(
        current.state,
        fallback,
        this.#lifecycleLeaseId,
      ))
      const committed = await this.#commitLifecycle(current, next)
      this.#emitAdmissionFailureRestored(fallback)
      return committed.state
    }
    const safelyUnopened = !this.#materializationActivationStarted &&
      (current.state.kind === 'intent-frozen' ||
       (current.state.kind === 'receiving' && this.#admissionFallback === undefined))
    if (!safelyUnopened) {
      return this.#recordAdmissionNeedsAttention()
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
    cut: PersistentMaterializationSettlementCut<PersistentDirectTreeMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    return this.#withMaterializationCut(session, 'pause-operation', cut, async (observation) => {
      if (signal.aborted) {
        // A requested pause is a cancellation shield: its durable cut must still finish.
      }
      if (request.worker.status !== 'Paused') {
        throw new TypeError('FSA pause requires a paused worker settlement')
      }
      const current = await this.#lifecycle()
      if (current.state.kind !== 'receiving') {
        throw new TypeError('FSA pause requires Receiving lifecycle state')
      }
      await observation.verifyOperationBinding()
      const seal = await observation.sealMaterializationLedger({
        evidence: cut.sealEvidence(),
        sealSequence: current.state.generation + 1n,
        purpose: MaterializationLedgerSealPurpose.ResumableSnapshot,
      })
      const validated = validateSealedFSASettlementEvidence({
        intent: this.#intent,
        directoryScope: this.#directoryScope,
        evidence: cut.evidence,
        seal,
        summary: request.materialization,
        outcome: MaterializationLedgerEvidenceOutcome.Resumable,
      })
      const checkpointEvidence = await observation.resumableCheckpointEvidence()
      const evidence = Object.freeze({ ...validated, ...checkpointEvidence })
      const receipt = await this.#settlementReceipt('resumable-receive', request, evidence)
      const next = this.#reduce(current.state, {
        kind: 'pause-verified',
        stage: 'receive',
        checkpointSetDigest: checkpointEvidence.checkpointSetDigest,
        completedFileCount: validated.fileCount,
        completedBytes: validated.completedBytes,
        selectionFacts: request.selectionFacts,
        partialReceiptDigest: receipt.digest,
        expectedGeneration: current.state.generation,
        leaseId: this.#lifecycleLeaseId,
      })
      const recoverySummary = await deriveSettledFSARecoverySummary({
        intent: this.#intent,
        lifecycle: next,
        checkpointEvidence,
      })
      const committed = await this.#commitLifecycle(current, next, [receipt])
      // UI authority is published only after the exact lifecycle generation is durable.
      this.#recoverySummary = recoverySummary
      this.#emit(
        'resumable-receive',
        checkpointEvidence.checkpointCount,
        validated.fileCount,
        validated.completedBytes,
      )
      return committed.state
    })
  }

  async #settle(
    session: FileSystemAccessOutputSession,
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    cut: PersistentMaterializationSettlementCut<PersistentDirectTreeMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    const footerState = request.worker.status === 'Succeeded' ? 'completed' : 'failed'
    return this.#settleTerminal(session, request, footerState, cut, signal)
  }

  async #stop(
    session: FileSystemAccessOutputSession,
    request: PlanStopRequest,
    cut: PersistentMaterializationSettlementCut<PersistentDirectTreeMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    if (!(request.reason instanceof TransferStopRequestedError) ||
        request.worker.status !== 'Paused') {
      throw new TypeError('FSA Stop requires its typed keep-partial reason and paused worker')
    }
    return this.#settleTerminal(session, request, 'stopped', cut, signal)
  }

  async #settleTerminal(
    session: FileSystemAccessOutputSession,
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement> | PlanStopRequest,
    footerState: 'completed' | 'failed' | 'stopped',
    cut: PersistentMaterializationSettlementCut<PersistentDirectTreeMaterializationEvidence>,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    const terminalKind = footerState === 'stopped' ? 'stop-operation' as const : 'settle-operation' as const
    return this.#withMaterializationCut(session, terminalKind, cut, async (observation) => {
      if (signal.aborted) {
        // Once terminal settlement owns the exclusive cut, cancellation cannot make its
        // durable lifecycle and publication projection disagree.
      }
      if (snapshotTransferJobId(request.transferJobId) !== this.#transferJobId) {
        throw new TypeError('FSA settlement escaped its transfer job')
      }
      const stopped = 'reason' in request
      if ((footerState === 'stopped') !== stopped ||
          (!stopped && (footerState === 'failed') !==
            (request.worker.status === 'CompletedWithErrors'))) {
        throw new TypeError('FSA terminal footer state disagrees with its typed request')
      }
      const published = footerState === 'completed' && request.worker.status === 'Succeeded'
      const outcome = published ? 'published' as const : 'partial-directory' as const
      // A zero-prefix repair has no restorable user namespace. Removing only its
      // verified owned pair here prevents a false repaired result while keeping
      // every ordinary output entry outside this narrowly proven cleanup.
      await observation.removeVerifiedEmptyCompatibleNameRepair()
      const current = await this.#lifecycle()
      if (current.state.kind !== 'receiving') {
        throw new TypeError('FSA completion requires Receiving lifecycle state')
      }
      await observation.verifyOperationBinding()
      const seal = await observation.sealMaterializationLedger({
        evidence: cut.sealEvidence(),
        sealSequence: current.state.generation + 1n,
        purpose: MaterializationLedgerSealPurpose.Terminal,
      })
      const validated = validateSealedFSASettlementEvidence({
        intent: this.#intent,
        directoryScope: this.#directoryScope,
        evidence: cut.evidence,
        seal,
        summary: request.materialization,
        outcome: published
          ? MaterializationLedgerEvidenceOutcome.Published
          : MaterializationLedgerEvidenceOutcome.Partial,
      })
      const evidence = Object.freeze({
        ...validated,
        checkpointCount: validated.fileCount,
      })
      const receipt = await this.#settlementReceipt(outcome, request, evidence)
      const transition = footerState === 'stopped' && stopped
        ? this.#stoppedTransition(current.state, request, receipt.digest)
        : this.#completedTransition(
            current.state,
            request as PlanSettlementRequest<CompletedTransferWorkerSettlement>,
            receipt.digest,
            evidence,
          )
      const next = transition.terminal
      await observation.persistCompatibleNamePendingOutcome(Object.freeze({
        formatVersion: COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION,
        footerState,
        ordinaryLifecycle: next,
        terminalReceipt: receipt,
      }))
      await observation.drainCompatibleNameProjector(footerState)
      const terminalBase = transition.intermediate === undefined
        ? current
        : await this.#commitLifecycle(current, transition.intermediate)
      const committed = await this.#commitLifecycle(terminalBase, next, [receipt])
      observePerformance(this.#diagnostics?.performance, summary => {
        if (committed.state.kind === 'published') summary.markMilestone('published')
        else summary.complete()
      })
      try {
        await observation.retireRecoveryMetadata()
        await observation.clearCompatibleNamePendingOutcome()
      } catch (cleanupFailure) {
        // The receipt and terminal lifecycle are already durable. Leaving metadata for
        // catch-up is safer than reporting a contradictory non-terminal result.
        recordOutputException(this.#diagnostics?.failures?.cleanup, cleanupFailure)
        emitOutputTrace(this.#diagnostics?.trace, () => outputTraceEvent('cleanup', {
          backend: 'file_system_access',
          transition: 'failed',
        }))
      }
      this.#emit(
        outcome,
        evidence.checkpointCount,
        evidence.fileCount,
        evidence.completedBytes,
      )
      return committed.state
    })
  }

  #completedTransition(
    current: ReceiveLifecycleState,
    request: PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    receiptDigest: string,
    evidence: SettlementReceiptEvidence,
  ): Readonly<{
    intermediate: Extract<ReceiveLifecycleState, { kind: 'finalizing-tree' }>
    terminal: Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>
  }> {
    const intermediate = this.#reduce(current, {
      kind: 'discovery-completed',
      expectedGeneration: current.generation,
      leaseId: this.#lifecycleLeaseId,
    })
    if (intermediate.kind !== 'finalizing-tree') {
      throw new TypeError('FSA completion requires FinalizingTree lifecycle state')
    }
    const published = request.worker.status === 'Succeeded'
    const terminal = this.#reduce(intermediate, {
      kind: 'tree-finalization-completed',
      outcome: published ? 'published' : 'partial-directory',
      receiptDigest,
      completedFileCount: evidence.fileCount,
      completedBytes: evidence.completedBytes,
      successCount: request.materialization.entryCount,
      failureCount: BigInt(request.worker.failureCount),
      expectedGeneration: intermediate.generation,
      leaseId: this.#lifecycleLeaseId,
    })
    if (terminal.kind !== 'published' && terminal.kind !== 'partial-directory') {
      throw new TypeError('FSA terminal reduction did not produce an ordinary tree outcome')
    }
    return Object.freeze({ intermediate, terminal })
  }

  #stoppedTransition(
    current: ReceiveLifecycleState,
    request: PlanStopRequest,
    receiptDigest: string,
  ): Readonly<{
    intermediate?: undefined
    terminal: Extract<ReceiveLifecycleState, { kind: 'partial-directory' }>
  }> {
    const terminal = this.#reduce(current, {
      kind: 'stop-requested',
      // The owned DirectTree root is itself retained even when no catalog child committed.
      successCount: request.materialization.entryCount === 0n
        ? 1n
        : request.materialization.entryCount,
      failureCount: BigInt(request.worker.failureCount),
      receiptDigest,
      cleanupReceiptDigest: receiptDigest,
      expectedGeneration: current.generation,
      leaseId: this.#lifecycleLeaseId,
    })
    if (terminal.kind !== 'partial-directory' || terminal.reason !== 'stopped') {
      throw new TypeError('FSA Stop did not retain an ordinary partial result')
    }
    return Object.freeze({ terminal })
  }

  async #withMaterializationCut(
    session: FileSystemAccessOutputSession,
    kind: FSATerminalMutationKind,
    cut: PersistentMaterializationSettlementCut<PersistentDirectTreeMaterializationEvidence>,
    operation: (observation: FSAFinalSettlementObservation) => Promise<ReceiveLifecycleState>,
  ): Promise<ReceiveLifecycleState> {
    let result: ReceiveLifecycleState | undefined
    let failure: unknown
    try {
      result = await session.runFinalSettlement(kind, async (observation) => {
        try {
          return await operation(observation)
        } catch (cause) {
          if (!(cause instanceof TargetOwnershipUnknownError) || session.compatibleNameRepairActive) {
            throw cause
          }
          return (await this.#recordNeedsAttention(cause)).state
        }
      })
    } catch (cause) {
      failure = cause
      if (cause instanceof TargetOwnershipUnknownError && !session.compatibleNameRepairActive) {
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
      // Resource release is a consequence. Once settlement has an initiating failure,
      // cleanup cannot replace its exact object identity with an aggregate wrapper.
      if (failure === undefined) failure = closeFailure
      else recordOutputException(this.#diagnostics?.failures?.cleanup, closeFailure)
    }
    if (failure !== undefined) throw failure
    if (result === undefined) throw new TypeError('FSA settlement produced no lifecycle state')
    return result
  }

  #settlementReceipt(
    outcome: 'published' | 'partial-directory' | 'resumable-receive',
    request: PlanPauseRequest | PlanSettlementRequest<CompletedTransferWorkerSettlement> | PlanStopRequest,
    evidence: SettlementReceiptEvidence,
  ): Promise<PersistedReceiveRecord> {
    return createFSASettlementReceipt({
      intent: this.#intent,
      transferJobId: this.#transferJobId,
      outcome,
      request,
      evidence,
      directoryScope: this.#directoryScope,
    })
  }

  #unopenedCleanupReceipt(): Promise<PersistedReceiveRecord> {
    return createFSAUnopenedCleanupReceipt({
      intent: this.#intent,
      transferJobId: this.#transferJobId,
      directoryScope: this.#directoryScope,
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
    return this.#recordAdmissionNeedsAttention(reason)
  }

  async #recordAdmissionNeedsAttention(reason?: unknown): Promise<ReceiveLifecycleState> {
    try {
      return (await this.#recordNeedsAttention(reason)).state
    } finally {
      // Admission failures return before an execution cut exists. A bound
      // materialization therefore owns final repository and Web Lock cleanup here,
      // even when recording NeedsAttention itself fails.
      await this.#boundMaterialization?.close()
    }
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

  #emitAdmissionFailureRestored(fallback: Extract<ReceiveAdmissionFallback, { kind: 'resumable-receive' }>): void {
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

interface VerifiedLifecycle {
  readonly state: ReceiveLifecycleState
  readonly record: PersistedReceiveRecord
}
