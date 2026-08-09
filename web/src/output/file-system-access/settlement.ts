import { fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { verifyFSAOperationBinding } from '../browser/indexeddb-root-binding'
import {
  equalCanonicalBytes,
  snapshotIdentity,
} from '../workspace/canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../workspace/lifecycle'
import type { MaterializedManifestEntry } from '../workspace/manifest'
import type { PersistedReceiveRecord } from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import {
  DirectorySettlementKind,
  createDirectoryAdmissionScope,
  type DirectoryAdmissionScope,
} from '../../transfer/directory-admission'
import { snapshotTransferJobId } from '../../transfer/job/identity'
import type { ReceiveIntent } from '../../transfer/intent'
import type {
  MaterializationSummary,
  PlanPauseRequest,
  PlanSettlementRequest,
} from '../../transfer/output-session'
import type { CompletedTransferWorkerSettlement } from '../../transfer/outcome'
import type {
  PersistentDirectTreeSettlementAuthority,
  PersistentDirectorySettlementEvidence,
  PersistentMaterializationEvidence,
  PersistentMaterializationSettlementCut,
} from '../../transfer/settlement/persistent-execution'
import {
  FileSystemAccessOutputSession,
  type FSAFinalSettlementObservation,
} from './session'
import {
  createFSASettlementReceipt,
  createFSAUnopenedCleanupReceipt,
  fsaCheckpointSetDigest,
  materializationSummary,
  requireDirectTreeIntent,
  sameFileEvidence,
  sameFinalProof,
  sameSummary,
  snapshotDirectorySettlements,
  snapshotEntries,
  type DirectTreeIntent,
  type ObservedSettlementEvidence,
} from './settlement-proof'

export { fsaCheckpointSetDigest } from './settlement-proof'

type MaterializedFileEntry = Extract<MaterializedManifestEntry, { kind: 'file' }>
type MaterializedDirectoryEntry = Extract<MaterializedManifestEntry, { kind: 'directory' }>

export type FSASettlementRepository = Pick<
  ReceiveOperationRepository,
  'commitTransition' | 'readLifecycle' | 'readLease' | 'readRecord' | 'readHandle'
>

export type FSASettlementTraceEvent = Readonly<{
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

export interface FileSystemAccessOperationSettlementAuthority {
  bindMaterialization(session: FileSystemAccessOutputSession): PersistentDirectTreeSettlementAuthority
  abortUnopened(
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
  readonly clock?: () => number
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
  return new FSAOperationSettlementAuthority({
    intent,
    repository: options.repository,
    lifecycleLeaseId: snapshotIdentity(options.lifecycleLeaseId, 16, 'lifecycle lease ID'),
    transferJobId: snapshotTransferJobId(options.transferJobId),
    clock: options.clock ?? Date.now,
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
  readonly #directoryScope: DirectoryAdmissionScope
  #materializationBound = false

  constructor(input: Readonly<{
    intent: DirectTreeIntent
    repository: FSASettlementRepository
    lifecycleLeaseId: string
    transferJobId: string
    clock: () => number
    trace?: (event: FSASettlementTraceEvent) => void
    directoryScope: DirectoryAdmissionScope
  }>) {
    this.#intent = input.intent
    this.#repository = input.repository
    this.#lifecycleLeaseId = input.lifecycleLeaseId
    this.#transferJobId = input.transferJobId
    this.#clock = input.clock
    this.#trace = input.trace
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

  async abortUnopened(
    intent: ReceiveIntent,
    _reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    signal.throwIfAborted()
    this.#requireIntent(intent)
    try {
      await verifyFSAOperationBinding({ repository: this.#repository, intent: this.#intent })
    } catch (cause) {
      if (!(cause instanceof TargetOwnershipUnknownError)) throw cause
      return (await this.#recordNeedsAttention(cause)).state
    }
    const current = await this.#lifecycle()
    if (current.state.kind !== 'intent-frozen') {
      throw new TypeError('unopened FSA abort requires IntentFrozen lifecycle state')
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
      const observed = await this.#observe(observation, cut.evidence, request.materialization, false)
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
      const observed = await this.#observe(
        observation,
        cut.evidence,
        request.materialization,
        published,
      )
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
          result = (await this.#recordNeedsAttention(cause)).state
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

  async #observe(
    authority: FSAFinalSettlementObservation,
    evidence: PersistentMaterializationEvidence,
    summary: MaterializationSummary,
    requireComplete: boolean,
  ): Promise<ObservedSettlementEvidence> {
    await authority.verifyOperationBinding()
    const candidates = await authority.candidateCheckpoints()
    if (candidates.length !== 0) {
      throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
    }
    const checkpoints = await authority.committedCheckpoints()
    const entries = snapshotEntries(evidence.entries)
    const fileEntries = entries.filter(
      (entry): entry is MaterializedFileEntry => entry.kind === 'file',
    )
    const directoryEntries = entries.filter(
      (entry): entry is MaterializedDirectoryEntry => entry.kind === 'directory',
    )
    await this.#verifyCheckpointEvidence(authority, checkpoints, fileEntries, requireComplete)
    const directorySettlements = await this.#verifyDirectoryEvidence(
      authority,
      evidence.directorySettlements,
      directoryEntries,
      requireComplete,
    )
    this.#validateLayout(directoryEntries, requireComplete)
    const measured = materializationSummary(entries)
    if (!sameSummary(measured, summary)) {
      throw new TypeError('FSA settlement summary differs from owned evidence')
    }
    const checkpointSetDigest = await fsaCheckpointSetDigest(this.#intent, checkpoints)
    return Object.freeze({
      entries,
      directorySettlements,
      checkpoints,
      checkpointSetDigest,
      completedFileCount: BigInt(fileEntries.length),
      completedBytes: fileEntries.reduce((total, entry) => total + entry.exactSize, 0n),
    })
  }

  async #verifyCheckpointEvidence(
    authority: FSAFinalSettlementObservation,
    checkpoints: readonly FileCheckpointV2[],
    fileEntries: readonly MaterializedFileEntry[],
    requireComplete: boolean,
  ): Promise<void> {
    const checkpointById = new Map(checkpoints.map(record => [record.recordId, record]))
    const fileByCheckpoint = new Map(fileEntries.map(entry => [entry.checkpoint.recordId, entry]))
    if (fileByCheckpoint.size !== fileEntries.length) {
      throw new TypeError('FSA settlement repeats a final checkpoint')
    }

    for (const checkpoint of checkpoints) {
      await authority.verifyCheckpointFile(checkpoint)
      if (fileCheckpointIsComplete(checkpoint) && !fileByCheckpoint.has(checkpoint.recordId)) {
        throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
      }
      if (requireComplete && !fileCheckpointIsComplete(checkpoint)) {
        throw new TypeError('published FSA settlement contains an incomplete checkpoint')
      }
    }
    for (const entry of fileEntries) {
      const checkpoint = checkpointById.get(entry.checkpoint.recordId)
      if (checkpoint === undefined || !sameFileEvidence(entry, checkpoint)) {
        throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
      }
      let proof: FinalFileCheckpointProof
      try {
        proof = await authority.finalCheckpointProof(
          entry.checkpoint.recordId,
          entry.checkpoint.checkpointGeneration,
        )
      } catch (cause) {
        throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId, { cause })
      }
      if (!sameFinalProof(proof, entry, this.#intent)) {
        throw new TargetOwnershipUnknownError('settlement', this.#intent.operationId)
      }
    }
  }

  async #verifyDirectoryEvidence(
    authority: FSAFinalSettlementObservation,
    supplied: readonly PersistentDirectorySettlementEvidence[],
    directoryEntries: readonly MaterializedDirectoryEntry[],
    requireComplete: boolean,
  ): Promise<readonly PersistentDirectorySettlementEvidence[]> {
    for (const entry of directoryEntries) {
      await authority.verifyDirectory(entry.artifactPath, entry.ownedObjectId)
    }
    const directorySettlements = snapshotDirectorySettlements(
      supplied,
      directoryEntries,
      this.#directoryScope,
    )
    if (requireComplete && (directorySettlements.length !== directoryEntries.length ||
        directorySettlements.some(value => value.settlement.kind !== DirectorySettlementKind.Finalized))) {
      throw new TypeError('published FSA settlement lacks finalized directory evidence')
    }
    return directorySettlements
  }

  #validateLayout(
    directories: readonly MaterializedDirectoryEntry[],
    requireComplete: boolean,
  ): void {
    if (this.#intent.artifact.layout.kind === 'single-file') {
      if (directories.length !== 0) {
        throw new TypeError('single-file FSA settlement contains an extra result root')
      }
      return
    }
    const roots = directories.filter(entry => entry.artifactPath.length === 0)
    if (roots.length > 1 || (requireComplete && roots.length !== 1) ||
        roots.some(entry => entry.directoryId !== this.#intent.syntheticRoot)) {
      throw new TypeError('FSA result-root settlement has invalid root evidence')
    }
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

  async #recordNeedsAttention(cause?: unknown): Promise<VerifiedLifecycle> {
    const current = await this.#lifecycle()
    if (current.state.kind === 'needs-attention') return current
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
    outcome: FSASettlementTraceEvent['outcome'],
    checkpointCount: bigint,
    completedFileCount: bigint,
    completedBytes: bigint,
    ownershipStage?: TargetOwnershipUnknownError['stage'],
  ): void {
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
}

interface VerifiedLifecycle {
  readonly state: ReceiveLifecycleState
  readonly record: PersistedReceiveRecord
}
