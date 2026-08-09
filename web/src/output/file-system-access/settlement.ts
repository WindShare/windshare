import { fileCheckpointDigest, fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { verifyFSAOperationBinding } from '../browser/indexeddb-root-binding'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU32,
  canonicalU64,
  canonicalU8,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../workspace/lifecycle'
import type { MaterializedManifestEntry } from '../workspace/manifest'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  createPersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import {
  DirectorySettlementKind,
  createDirectoryAdmissionScope,
  snapshotDirectoryAdmission,
  snapshotMaterializationPath,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type DirectorySettlement,
} from '../../transfer/directory-admission'
import { snapshotTransferJobId } from '../../transfer/job/identity'
import {
  validateReceiveIntent,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../transfer/intent'
import type { MaterializationSummary, PlanPauseRequest, PlanSettlementRequest } from '../../transfer/output-session'
import type { CompletedTransferWorkerSettlement, TransferFailure } from '../../transfer/outcome'
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

const FSA_SETTLEMENT_RECEIPT = 12
const FSA_UNOPENED_CLEANUP_RECEIPT = 13

type DirectTreeIntent = ReceiveIntent & Readonly<{
  plan: DirectTreePlan
  artifact: DirectoryTreeArtifact
}>
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

  async #settlementReceipt(
    outcome: 'published' | 'partial-directory' | 'resumable-receive',
    request: PlanPauseRequest | PlanSettlementRequest<CompletedTransferWorkerSettlement>,
    evidence: ObservedSettlementEvidence,
  ): Promise<PersistedReceiveRecord> {
    const bytes = canonicalRecord('windshare/receive-receipt/v1', 1, [
      canonicalU8(FSA_SETTLEMENT_RECEIPT),
      identityFrame(this.#intent.operationId, 16, 'operation ID'),
      identityFrame(this.#intent.digest, 32, 'receive intent digest'),
      identityFrame(this.#intent.plan.reservation.digest, 32, 'reservation digest'),
      identityFrame(this.#transferJobId, 16, 'transfer job ID'),
      canonicalFrame(canonicalU8(settlementOutcomeByte(outcome))),
      identityFrame(evidence.checkpointSetDigest, 32, 'checkpoint set digest'),
      canonicalFrame(canonicalU64(request.materialization.entryCount)),
      canonicalFrame(canonicalU64(request.materialization.fileCount)),
      canonicalFrame(canonicalU64(request.materialization.directoryCount)),
      canonicalFrame(canonicalU64(request.materialization.rawBytes)),
      canonicalFrame(canonicalU8(workerStatusByte(request.worker.status))),
      canonicalFrame(canonicalU64(BigInt(request.worker.failureCount))),
      canonicalFrame(canonicalU64(BigInt(request.worker.omittedFailureCount))),
      canonicalFrame(canonicalU64(BigInt(evidence.entries.length))),
      ...evidence.entries.flatMap(entry => [
        canonicalFrame(canonicalSettlementEntry(entry)),
        ...(entry.kind === 'file'
          ? [identityFrame(entry.checkpoint.recordId, 32, 'checkpoint record ID')]
          : []),
      ]),
      canonicalFrame(canonicalU64(BigInt(evidence.directorySettlements.length))),
      ...evidence.directorySettlements.map(value => canonicalFrame(
        canonicalDirectorySettlement(value),
      )),
      canonicalFrame(canonicalU64(BigInt(request.worker.failures.length))),
      ...request.worker.failures.map(failure => canonicalFrame(canonicalFailure(failure))),
    ])
    return createPersistedReceiveRecord({
      operationId: this.#intent.operationId,
      kind: RECEIVE_RECORD_RECEIPT,
      canonicalBytes: bytes,
    })
  }

  #unopenedCleanupReceipt(): Promise<PersistedReceiveRecord> {
    return createPersistedReceiveRecord({
      operationId: this.#intent.operationId,
      kind: RECEIVE_RECORD_CLEANUP,
      canonicalBytes: canonicalRecord('windshare/receive-receipt/v1', 1, [
        canonicalU8(FSA_UNOPENED_CLEANUP_RECEIPT),
        identityFrame(this.#intent.operationId, 16, 'operation ID'),
        identityFrame(this.#intent.digest, 32, 'receive intent digest'),
        identityFrame(this.#intent.plan.reservation.digest, 32, 'reservation digest'),
        identityFrame(this.#transferJobId, 16, 'transfer job ID'),
        canonicalFrame(canonicalU64(0n)),
      ]),
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

interface ObservedSettlementEvidence {
  readonly entries: readonly MaterializedManifestEntry[]
  readonly directorySettlements: readonly PersistentDirectorySettlementEvidence[]
  readonly checkpoints: readonly FileCheckpointV2[]
  readonly checkpointSetDigest: string
  readonly completedFileCount: bigint
  readonly completedBytes: bigint
}

async function requireDirectTreeIntent(input: ReceiveIntent): Promise<DirectTreeIntent> {
  const intent = await validateReceiveIntent(input)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      intent.plan.reservation.authorityKind !== 'fsa-container' ||
      intent.plan.reservation.guarantees.profile !== 'fsa-tree' ||
      intent.plan.reservation.guarantees.delivery !== 'managed-target' ||
      intent.plan.reservation.guarantees.replacement !== 'coordinated-no-replace' ||
      intent.plan.reservation.guarantees.visibility !== 'prefix-visible') {
    throw new TypeError('FSA settlement requires ManagedTarget CoordinatedNoReplace PrefixVisible')
  }
  return intent as DirectTreeIntent
}

function snapshotEntries(entries: readonly MaterializedManifestEntry[]): readonly MaterializedManifestEntry[] {
  const snapshots = entries.map(entry => {
    canonicalSettlementEntry(entry)
    return Object.freeze({
      ...entry,
      artifactPath: snapshotMaterializationPath(entry.artifactPath),
      ...(entry.kind === 'file'
        ? { checkpoint: Object.freeze({ ...entry.checkpoint }) }
        : {}),
    }) as MaterializedManifestEntry
  }).sort(compareEntries)
  const keys = snapshots.map(entry => JSON.stringify(entry.artifactPath))
  if (new Set(keys).size !== keys.length) {
    throw new TypeError('FSA settlement repeats an artifact path')
  }
  return Object.freeze(snapshots)
}

function snapshotDirectorySettlements(
  values: readonly PersistentDirectorySettlementEvidence[],
  directories: readonly Extract<MaterializedManifestEntry, { kind: 'directory' }>[],
  scope: DirectoryAdmissionScope,
): readonly PersistentDirectorySettlementEvidence[] {
  const byPath = new Map(directories.map(entry => [JSON.stringify(entry.artifactPath), entry]))
  const snapshots = values.map(value => {
    const artifactPath = snapshotMaterializationPath(value.artifactPath)
    const settlement = snapshotSettlement(value.settlement)
    const entry = byPath.get(JSON.stringify(artifactPath))
    if (entry === undefined || settlement.admission.receiveIntentDigest !== scope.receiveIntentDigest ||
        settlement.admission.layoutVersion !== scope.layoutVersion ||
        settlement.admission.layout !== scope.layout ||
        settlement.admission.directoryId !== entry.directoryId ||
        settlement.admission.generation !== entry.generation) {
      throw new TypeError('FSA directory settlement escaped its owned directory evidence')
    }
    return Object.freeze({ artifactPath, settlement })
  }).sort((left, right) => comparePath(left.artifactPath, right.artifactPath))
  const paths = snapshots.map(value => JSON.stringify(value.artifactPath))
  if (new Set(paths).size !== paths.length) {
    throw new TypeError('FSA settlement repeats a directory receipt')
  }
  return Object.freeze(snapshots)
}

function snapshotSettlement(input: DirectorySettlement): DirectorySettlement {
  const admission = snapshotDirectoryAdmission(input.admission)
  switch (input.kind) {
    case DirectorySettlementKind.Finalized:
      return Object.freeze({ kind: input.kind, admission })
    case DirectorySettlementKind.IsolatedFailure:
      return Object.freeze({ kind: input.kind, admission, fault: input.fault })
  }
}

function sameFileEvidence(
  entry: Extract<MaterializedManifestEntry, { kind: 'file' }>,
  checkpoint: FileCheckpointV2,
): boolean {
  return fileCheckpointIsComplete(checkpoint) &&
    checkpoint.operationId.length !== 0 &&
    checkpoint.recordId === entry.checkpoint.recordId &&
    fileCheckpointDigest(checkpoint) === entry.checkpoint.recordDigest &&
    checkpoint.checkpointGeneration === entry.checkpoint.checkpointGeneration &&
    checkpoint.fileId === entry.fileId && checkpoint.fileRevision === entry.fileRevision &&
    checkpoint.exactSize === entry.exactSize && checkpoint.ownedObjectId === entry.ownedObjectId &&
    samePath(checkpoint.canonicalPath, entry.artifactPath)
}

function sameFinalProof(
  proof: FinalFileCheckpointProof,
  entry: Extract<MaterializedManifestEntry, { kind: 'file' }>,
  intent: DirectTreeIntent,
): boolean {
  return proof.operationId === intent.operationId && proof.receiveIntentDigest === intent.digest &&
    proof.materializationBindingDigest === intent.plan.reservation.digest && proof.complete === true &&
    proof.recordId === entry.checkpoint.recordId &&
    proof.recordDigest === entry.checkpoint.recordDigest &&
    proof.checkpointGeneration === entry.checkpoint.checkpointGeneration &&
    proof.fileId === entry.fileId && proof.fileRevision === entry.fileRevision &&
    proof.exactSize === entry.exactSize && proof.ownedObjectId === entry.ownedObjectId &&
    samePath(proof.canonicalPath, entry.artifactPath)
}

function materializationSummary(entries: readonly MaterializedManifestEntry[]): MaterializationSummary {
  const visible = entries.filter(entry => entry.kind === 'file' || entry.artifactPath.length !== 0)
  const files = visible.filter(entry => entry.kind === 'file')
  return Object.freeze({
    entryCount: BigInt(visible.length),
    fileCount: BigInt(files.length),
    directoryCount: BigInt(visible.length - files.length),
    rawBytes: files.reduce((total, entry) => total + entry.exactSize, 0n),
  })
}

function sameSummary(left: MaterializationSummary, right: MaterializationSummary): boolean {
  return left.entryCount === right.entryCount && left.fileCount === right.fileCount &&
    left.directoryCount === right.directoryCount && left.rawBytes === right.rawBytes
}

export async function fsaCheckpointSetDigest(
  intent: DirectTreeIntent,
  checkpoints: readonly FileCheckpointV2[],
): Promise<string> {
  return canonicalDigest(canonicalRecord('windshare/fsa-checkpoint-set/v1', 1, [
    identityFrame(intent.operationId, 16, 'operation ID'),
    identityFrame(intent.digest, 32, 'receive intent digest'),
    identityFrame(intent.plan.reservation.digest, 32, 'reservation digest'),
    canonicalFrame(canonicalU64(BigInt(checkpoints.length))),
    ...checkpoints.map(checkpoint => canonicalFrame(concatCanonicalBytes([
      identityFrame(checkpoint.recordId, 32, 'checkpoint record ID'),
      identityFrame(fileCheckpointDigest(checkpoint), 32, 'checkpoint digest'),
      canonicalFrame(canonicalU64(checkpoint.checkpointGeneration)),
    ]))),
  ]))
}

function canonicalDirectorySettlement(
  value: PersistentDirectorySettlementEvidence,
): CanonicalBytes {
  const admission = value.settlement.admission
  return concatCanonicalBytes([
    canonicalFrame(canonicalSettlementPath(value.artifactPath)),
    canonicalFrame(canonicalU8(
      value.settlement.kind === DirectorySettlementKind.Finalized ? 1 : 2,
    )),
    canonicalFrame(canonicalText(admission.layout)),
    identityFrame(admission.receiveIntentDigest, 32, 'directory receive intent digest'),
    canonicalFrame(canonicalU8(admission.layoutVersion)),
    identityFrame(admission.token, 32, 'directory admission token'),
    identityFrame(admission.directoryId, 16, 'directory ID'),
    identityFrame(admission.generation, 16, 'directory generation'),
    canonicalFrame(canonicalSettlementPath(admission.path)),
    canonicalOptionalIdentity(admission.parentToken, 32, 'parent admission token'),
    canonicalModifiedTime(admission),
  ])
}

function canonicalFailure(failure: TransferFailure): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU8(failure.kind === 'directory' ? 1 : 2),
    canonicalFrame(canonicalIdentity(
      failure.kind === 'directory' ? failure.directoryId : failure.fileId,
      16,
      `${failure.kind} failure identity`,
    )),
  ])
}

function canonicalSettlementEntry(entry: MaterializedManifestEntry): CanonicalBytes {
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalSettlementPath(entry.artifactPath)),
      identityFrame(entry.directoryId, 16, 'directory ID'),
      identityFrame(entry.generation, 16, 'directory generation'),
      identityFrame(entry.ownedObjectId, 32, 'owned object ID'),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalSettlementPath(entry.artifactPath)),
    identityFrame(entry.fileId, 16, 'file ID'),
    identityFrame(entry.fileRevision, 16, 'file revision'),
    canonicalFrame(canonicalU64(entry.exactSize)),
    identityFrame(entry.ownedObjectId, 32, 'owned object ID'),
    identityFrame(entry.checkpoint.recordDigest, 32, 'checkpoint digest'),
    canonicalFrame(canonicalU64(entry.checkpoint.checkpointGeneration)),
  ])
}

function canonicalSettlementPath(path: readonly string[]): CanonicalBytes {
  return path.length === 0 ? canonicalU64(0n) : canonicalPath(path)
}

function canonicalModifiedTime(admission: DirectoryAdmission): CanonicalBytes {
  if (admission.modifiedTime === undefined) return canonicalFrame(canonicalU8(0))
  return canonicalFrame(concatCanonicalBytes([
    canonicalU8(1),
    canonicalFrame(canonicalI64(admission.modifiedTime.seconds)),
    canonicalFrame(canonicalU32(admission.modifiedTime.nanoseconds)),
    canonicalFrame(canonicalU8(admission.modifiedTime.precision)),
  ]))
}

function canonicalOptionalIdentity(
  value: string | undefined,
  width: number,
  label: string,
): CanonicalBytes {
  return canonicalFrame(value === undefined
    ? canonicalU8(0)
    : concatCanonicalBytes([canonicalU8(1), canonicalFrame(canonicalIdentity(value, width, label))]))
}

function identityFrame(value: string, width: number, label: string): CanonicalBytes {
  return canonicalFrame(canonicalIdentity(value, width, label))
}

function settlementOutcomeByte(
  outcome: 'published' | 'partial-directory' | 'resumable-receive',
): number {
  switch (outcome) {
    case 'published': return 1
    case 'partial-directory': return 2
    case 'resumable-receive': return 3
  }
}

function workerStatusByte(status: string): number {
  switch (status) {
    case 'Succeeded': return 1
    case 'CompletedWithErrors': return 2
    case 'Paused': return 3
    default: throw new TypeError('FSA worker settlement status is invalid')
  }
}

function compareEntries(left: MaterializedManifestEntry, right: MaterializedManifestEntry): number {
  const path = comparePath(left.artifactPath, right.artifactPath)
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function comparePath(left: readonly string[], right: readonly string[]): number {
  const limit = Math.min(left.length, right.length)
  for (let index = 0; index < limit; index += 1) {
    if (left[index] === right[index]) continue
    return left[index]! < right[index]! ? -1 : 1
  }
  return left.length - right.length
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
