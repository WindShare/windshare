import type { BrowserReceiveOperationLease } from '../browser/session-lease'
import type { BrowserLockManagerRuntime } from '../browser/namespace-mutation'
import type { PersistedFSAOperationBinding } from '../browser/indexeddb-root-binding'
import {
  fileCheckpointDigest,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU64,
  canonicalU8,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../workspace/lifecycle'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  createPersistedReceiveRecord,
  operationRecordId,
  validatePersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import type { ReceiveOperationRepository } from '../workspace/repository'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import {
  validateReceiveIntent,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  openFreshPageFileSystemAccessDiscard,
  type FSAFileCheckpointRepositoryFactory,
  type FSAFreshPageDiscardCut,
  type FreshPageFileSystemAccessDiscardSession,
} from './session'
import { fsaCheckpointSetDigest } from './settlement'

const FSA_FRESH_PAGE_CLEANUP_RECEIPT = 14
const FSA_FRESH_PAGE_PARTIAL_RECEIPT = 15
const CONSUMED_AUTHORITIES = new WeakSet<object>()

type DirectTreeIntent = ReceiveIntent & Readonly<{
  plan: DirectTreePlan
  artifact: DirectoryTreeArtifact
}>

type FreshPageDiscardLifecycle =
  | Extract<ReceiveLifecycleState, { readonly kind: 'resumable-receive' }>
  | Extract<ReceiveLifecycleState, { readonly kind: 'expired' }>

export interface ReopenedFileSystemAccessDiscardOperation {
  readonly kind: 'direct-tree'
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly binding: PersistedFSAOperationBinding
  readonly lease: Pick<BrowserReceiveOperationLease, 'operationId' | 'leaseId'>
  readonly repository: ReceiveOperationRepository
  close(): Promise<void>
}

export type FreshPageFileSystemAccessDiscardResult =
  | Readonly<{
      lifecycle:
        | Extract<ReceiveLifecycleState, { readonly kind: 'partial-directory' }>
        | Extract<ReceiveLifecycleState, { readonly kind: 'discarded' }>
        | Extract<ReceiveLifecycleState, { readonly kind: 'expired' }>
      receiptDigest: string
    }>
  | Readonly<{
      lifecycle: Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>
    }>

export type FSAFreshPageDiscardTraceEvent = Readonly<{
  name: 'receive.fsa.fresh_discard.completed'
  operation_id: string
  receive_intent_digest: string
  outcome: 'partial-directory' | 'discarded' | 'expired' | 'needs-attention'
  completed_file_count: bigint
  completed_bytes: bigint
  removed_object_count: bigint
  needs_attention_reason?: 'target-ownership-unknown' | 'cleanup-unknown'
}>

export interface DiscardReopenedFileSystemAccessOutputOptions {
  readonly operation: ReopenedFileSystemAccessDiscardOperation
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly clock?: () => number
  readonly trace?: (event: FSAFreshPageDiscardTraceEvent) => void
}

/**
 * Consumes W3-C's single reopened operation authority. Presentation cannot
 * provide handles, checkpoint ranges, worker summaries, or a cleanup verdict;
 * every such fact is reread while both operation and parent locks are alive.
 */
export async function discardReopenedFileSystemAccessOutput(
  options: DiscardReopenedFileSystemAccessOutputOptions,
): Promise<FreshPageFileSystemAccessDiscardResult> {
  if (CONSUMED_AUTHORITIES.has(options.operation)) {
    throw new DOMException('Fresh-page FSA discard authority was already consumed', 'InvalidStateError')
  }
  CONSUMED_AUTHORITIES.add(options.operation)

  const authority = await FreshPageDiscardLifecycleAuthority.create(options)
  return new FreshPageDiscardExecution(options, authority).run()
}

class FreshPageDiscardExecution {
  readonly #options: DiscardReopenedFileSystemAccessOutputOptions
  readonly #authority: FreshPageDiscardLifecycleAuthority
  #session: FreshPageFileSystemAccessDiscardSession | undefined
  #result: FreshPageFileSystemAccessDiscardResult | undefined
  #failure: unknown
  #authorized = false

  constructor(
    options: DiscardReopenedFileSystemAccessOutputOptions,
    authority: FreshPageDiscardLifecycleAuthority,
  ) {
    this.#options = options
    this.#authority = authority
  }

  async run(): Promise<FreshPageFileSystemAccessDiscardResult> {
    try {
      await this.#execute()
    } catch (cause) {
      await this.#captureFailure(cause)
    }
    await this.#closeSession()
    await this.#closeOperation()
    if (this.#failure !== undefined) throw this.#failure
    if (this.#result === undefined) {
      throw new TypeError('Fresh-page FSA discard produced no durable result')
    }
    return this.#result
  }

  async #execute(): Promise<void> {
    await this.#authority.verifyInitialAuthority()
    this.#authorized = true
    this.#session = await openFreshPageFileSystemAccessDiscard({
      intent: this.#authority.intent,
      binding: this.#options.operation.binding,
      operationRepository: this.#options.operation.repository,
      ...(this.#options.lockManager === undefined ? {} : { lockManager: this.#options.lockManager }),
      ...(this.#options.checkpointRepositoryFactory === undefined
        ? {}
        : { checkpointRepositoryFactory: this.#options.checkpointRepositoryFactory }),
      ...(this.#options.databaseName === undefined
        ? {}
        : { databaseName: this.#options.databaseName }),
    })
    if (!this.#session.usesOperationRepository(this.#options.operation.repository)) {
      throw new TypeError('Fresh-page FSA discard split its repository authority')
    }
    let cut: FSAFreshPageDiscardCut
    try {
      cut = await this.#session.discardOwnedUnfinishedObjects()
    } catch (cause) {
      throw cleanupObservationUnknown(this.#authority.intent.operationId, cause)
    }
    this.#result = await this.#authority.commitDiscard(cut)
  }

  async #captureFailure(cause: unknown): Promise<void> {
    if (cause instanceof TargetOwnershipUnknownError && this.#authorized) {
      try {
        this.#result = await this.#authority.recordUnknown(cause)
      } catch (attentionFailure) {
        this.#failure = new AggregateError(
          [cause, attentionFailure],
          'Fresh-page FSA discard and NeedsAttention persistence both failed',
        )
      }
      return
    }
    this.#failure = cause
  }

  async #closeSession(): Promise<void> {
    if (this.#session === undefined) return
    try {
      await this.#session.close()
    } catch (closeFailure) {
      await this.#captureCloseFailure(closeFailure)
    }
  }

  async #captureCloseFailure(closeFailure: unknown): Promise<void> {
    if (!this.#authorized) {
      this.#failure = combineFailures(this.#failure, closeFailure)
      return
    }
    try {
      this.#result = await this.#authority.recordUnknown(new TargetOwnershipUnknownError(
        'cleanup',
        this.#authority.intent.operationId,
        { cause: closeFailure },
      ))
    } catch (attentionFailure) {
      this.#failure = combineFailures(
        this.#failure,
        new AggregateError(
          [closeFailure, attentionFailure],
          'Fresh-page FSA root close and NeedsAttention persistence both failed',
        ),
      )
    }
  }

  async #closeOperation(): Promise<void> {
    try {
      await this.#options.operation.close()
    } catch (closeFailure) {
      this.#failure = combineFailures(this.#failure, closeFailure)
    }
  }
}

class FreshPageDiscardLifecycleAuthority {
  readonly intent: DirectTreeIntent
  readonly #operation: ReopenedFileSystemAccessDiscardOperation
  readonly #repository: ReceiveOperationRepository
  readonly #leaseId: string
  readonly #clock: () => number
  readonly #trace: DiscardReopenedFileSystemAccessOutputOptions['trace']
  #initial: VerifiedLifecycle | undefined

  private constructor(input: Readonly<{
    intent: DirectTreeIntent
    operation: ReopenedFileSystemAccessDiscardOperation
    leaseId: string
    clock: () => number
    trace?: (event: FSAFreshPageDiscardTraceEvent) => void
  }>) {
    this.intent = input.intent
    this.#operation = input.operation
    this.#repository = input.operation.repository
    this.#leaseId = input.leaseId
    this.#clock = input.clock
    this.#trace = input.trace
  }

  static async create(
    options: DiscardReopenedFileSystemAccessOutputOptions,
  ): Promise<FreshPageDiscardLifecycleAuthority> {
    const intent = await requireDirectTreeIntent(options.operation.intent)
    if (options.operation.kind !== 'direct-tree' ||
        options.operation.binding.intent.operationId !== intent.operationId ||
        options.operation.binding.intent.digest !== intent.digest ||
        options.operation.binding.reservation.digest !== intent.plan.reservation.digest ||
        options.operation.lease.operationId !== intent.operationId) {
      throw new TypeError('Fresh-page FSA discard received a recombined operation authority')
    }
    return new FreshPageDiscardLifecycleAuthority({
      intent,
      operation: options.operation,
      leaseId: snapshotIdentity(options.operation.lease.leaseId, 16, 'lifecycle lease ID'),
      clock: options.clock ?? Date.now,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
  }

  async verifyInitialAuthority(): Promise<void> {
    const current = await this.#lifecycle()
    const expected = await storedReceiveLifecycleState(this.#operation.lifecycle)
    if (current.record.digest !== expected.digest) {
      throw new DOMException('Fresh-page FSA lifecycle generation is stale', 'InvalidStateError')
    }
    const lifecycle = this.#requireDiscardLifecycle(current.state)
    await this.#verifyReferencedReceipt(lifecycle)
    this.#initial = current
  }

  async commitDiscard(cut: FSAFreshPageDiscardCut): Promise<FreshPageFileSystemAccessDiscardResult> {
    const current = await this.#lifecycle()
    if (this.#initial === undefined || current.record.digest !== this.#initial.record.digest) {
      throw new DOMException('Fresh-page FSA lifecycle changed before cleanup commit', 'InvalidStateError')
    }
    const lifecycle = this.#requireDiscardLifecycle(current.state)
    const checkpointSetDigest = await fsaCheckpointSetDigest(this.intent, cut.committedCheckpoints)
    const completedFileCount = BigInt(cut.successfulCheckpoints.length)
    const completedBytes = cut.successfulCheckpoints.reduce(
      (total, checkpoint) => total + checkpoint.exactSize,
      0n,
    )
    if (lifecycle.kind === 'resumable-receive' &&
        (lifecycle.checkpointSetDigest !== checkpointSetDigest ||
         lifecycle.completedFileCount !== completedFileCount ||
         lifecycle.completedBytes !== completedBytes)) {
      throw new TargetOwnershipUnknownError('cleanup', this.intent.operationId)
    }

    const cleanupReceipt = await freshPageCleanupReceipt({
      intent: this.intent,
      lifecycle: current,
      cut,
      checkpointSetDigest,
      completedFileCount,
      completedBytes,
    })
    const records: PersistedReceiveRecord[] = [cleanupReceipt]
    let receiptDigest = cleanupReceipt.digest
    let next: ReceiveLifecycleState
    if (lifecycle.kind === 'resumable-receive') {
      if (completedFileCount !== 0n) {
        const partialReceipt = await freshPagePartialReceipt({
          intent: this.intent,
          lifecycle: current,
          cleanupReceiptDigest: cleanupReceipt.digest,
          successfulCheckpoints: cut.successfulCheckpoints,
          completedBytes,
        })
        records.push(partialReceipt)
        receiptDigest = partialReceipt.digest
      }
      next = this.#reduce(lifecycle, {
        kind: 'stop-requested',
        successCount: completedFileCount,
        // Stopping unfinished work is not a manufactured worker failure.
        failureCount: 0n,
        receiptDigest,
        cleanupReceiptDigest: cleanupReceipt.digest,
        expectedGeneration: lifecycle.generation,
        leaseId: this.#leaseId,
      })
    } else {
      next = this.#reduce(lifecycle, {
        kind: 'cleanup-verified',
        cleanupReceiptDigest: cleanupReceipt.digest,
        expectedGeneration: lifecycle.generation,
        leaseId: this.#leaseId,
      })
    }

    let committed = await this.#commitLifecycle(
      current,
      next,
      records,
      cut.removedDirectoryHandleIds,
    )
    try {
      await cut.retireCheckpoints()
    } catch (cause) {
      committed = await this.#recordUnknownLifecycle(new TargetOwnershipUnknownError(
        'cleanup',
        this.intent.operationId,
        { cause },
      ))
    }
    if (committed.state.kind === 'needs-attention') {
      this.#emit(
        'needs-attention',
        completedFileCount,
        completedBytes,
        cut,
        requireFreshDiscardAttentionReason(committed.state.reason),
      )
      return Object.freeze({ lifecycle: committed.state })
    }
    if (committed.state.kind !== 'partial-directory' && committed.state.kind !== 'discarded' &&
        committed.state.kind !== 'expired') {
      throw new TypeError('Fresh-page FSA discard persisted a non-terminal lifecycle')
    }
    this.#emit(committed.state.kind, completedFileCount, completedBytes, cut)
    return Object.freeze({ lifecycle: committed.state, receiptDigest })
  }

  async recordUnknown(
    cause: TargetOwnershipUnknownError,
  ): Promise<Extract<FreshPageFileSystemAccessDiscardResult, {
    readonly lifecycle: { readonly kind: 'needs-attention' }
  }>> {
    const committed = await this.#recordUnknownLifecycle(cause)
    this.#emit(
      'needs-attention',
      0n,
      0n,
      undefined,
      requireFreshDiscardAttentionReason(committed.state.reason),
    )
    return Object.freeze({ lifecycle: committed.state })
  }

  async #recordUnknownLifecycle(
    cause: TargetOwnershipUnknownError,
  ): Promise<VerifiedNeedsAttention> {
    const current = await this.#lifecycle()
    if (current.state.kind === 'needs-attention') {
      return current as VerifiedNeedsAttention
    }
    const event: LifecycleEvent = cause.stage === 'cleanup'
      ? Object.freeze({
          kind: 'cleanup-unknown',
          lastVerifiedRecordDigest: current.record.digest,
          expectedGeneration: current.state.generation,
          leaseId: this.#leaseId,
        })
      : Object.freeze({
          kind: 'ownership-unknown',
          lastVerifiedRecordDigest: current.record.digest,
          expectedGeneration: current.state.generation,
          leaseId: this.#leaseId,
        })
    const next = this.#reduce(current.state, event)
    const committed = await this.#commitLifecycle(current, next)
    if (committed.state.kind !== 'needs-attention') {
      throw new TypeError('Unknown fresh-page FSA cleanup did not become NeedsAttention')
    }
    return committed as VerifiedNeedsAttention
  }

  async #verifyReferencedReceipt(state: FreshPageDiscardLifecycle): Promise<void> {
    const digest = state.kind === 'resumable-receive'
      ? state.partialReceiptDigest
      : state.expiryReceiptDigest
    if (digest === undefined) return
    const id = operationRecordId(this.intent.operationId, RECEIVE_RECORD_RECEIPT, digest)
    let record: PersistedReceiveRecord | undefined
    try {
      record = await this.#repository.readRecord(id)
      if (record !== undefined) record = await validatePersistedReceiveRecord(record)
    } catch (cause) {
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId, { cause })
    }
    if (record === undefined || record.kind !== RECEIVE_RECORD_RECEIPT ||
        record.operationId !== this.intent.operationId || record.digest !== digest) {
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId)
    }
  }

  async #lifecycle(): Promise<VerifiedLifecycle> {
    let record: PersistedReceiveRecord | undefined
    let lease: Awaited<ReturnType<ReceiveOperationRepository['readLease']>>
    try {
      [record, lease] = await Promise.all([
        this.#repository.readLifecycle(this.intent.operationId),
        this.#repository.readLease(this.intent.operationId),
      ])
    } catch (cause) {
      const error = new DOMException(
        'Fresh-page FSA lifecycle authority is unreadable',
        'InvalidStateError',
      )
      Object.defineProperty(error, 'cause', { value: cause })
      throw error
    }
    if (record === undefined || lease === undefined ||
        lease.operationId !== this.intent.operationId || lease.leaseId !== this.#leaseId) {
      throw new DOMException('Fresh-page FSA lifecycle lease is stale or foreign', 'InvalidStateError')
    }
    let state: ReceiveLifecycleState
    try {
      state = decodeStoredReceiveLifecycleState(record)
    } catch (cause) {
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId, { cause })
    }
    if (state.operationId !== this.intent.operationId ||
        state.receiveIntentDigest !== this.intent.digest ||
        ('activeLeaseId' in state && state.activeLeaseId !== this.#leaseId)) {
      throw new DOMException('Fresh-page FSA lifecycle escaped its operation lease', 'InvalidStateError')
    }
    return Object.freeze({ state, record })
  }

  async #commitLifecycle(
    current: VerifiedLifecycle,
    next: ReceiveLifecycleState,
    records: readonly PersistedReceiveRecord[] = [],
    deleteHandleIds: readonly string[] = [],
  ): Promise<VerifiedLifecycle> {
    const expected = await storedReceiveLifecycleState(next)
    try {
      await this.#repository.commitTransition({
        operationId: this.intent.operationId,
        expectedLifecycleGeneration: current.state.generation,
        expectedLeaseId: this.#leaseId,
        ...(records.length === 0 ? {} : { records }),
        ...(deleteHandleIds.length === 0 ? {} : { deleteHandleIds }),
        lifecycle: next,
      })
    } catch (cause) {
      const observed = await this.#lifecycle().catch(() => undefined)
      if (observed !== undefined && observed.record.digest === expected.digest &&
          await this.#recordsMatch(records) && await this.#handlesAbsent(deleteHandleIds)) {
        return observed
      }
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId, { cause })
    }
    const observed = await this.#lifecycle()
    if (observed.record.digest !== expected.digest || !await this.#recordsMatch(records) ||
        !await this.#handlesAbsent(deleteHandleIds)) {
      throw new TargetOwnershipUnknownError('settlement', this.intent.operationId)
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

  async #handlesAbsent(ids: readonly string[]): Promise<boolean> {
    try {
      for (const id of ids) {
        if (await this.#repository.readHandle(id) !== undefined) return false
      }
      return true
    } catch {
      return false
    }
  }

  #requireDiscardLifecycle(state: ReceiveLifecycleState): FreshPageDiscardLifecycle {
    if (state.kind === 'resumable-receive') {
      if (this.#now() >= state.expiresAt) {
        throw new DOMException(
          'Elapsed DirectTree retention must be persisted as Expired before cleanup',
          'InvalidStateError',
        )
      }
      return state
    }
    if (state.kind === 'expired' && state.priorStableState === 'resumable-receive' &&
        state.cleanupState === 'cleanup-pending') return state
    throw new DOMException('Fresh-page FSA discard requires retained DirectTree cleanup', 'InvalidStateError')
  }

  #reduce(state: ReceiveLifecycleState, event: LifecycleEvent): ReceiveLifecycleState {
    const reduced = reduceReceiveLifecycle(state, event, {
      planKind: 'direct-tree',
      preparationRequired: false,
      activeLeaseId: this.#leaseId,
      nowMilliseconds: this.#now(),
    })
    if (reduced.status !== 'applied' || reduced.state === state) {
      throw new TypeError('Fresh-page FSA lifecycle transition was stale or side-effect free')
    }
    return reduced.state
  }

  #now(): number {
    const value = this.#clock()
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new TypeError('Fresh-page FSA discard clock is invalid')
    }
    return value
  }

  #emit(
    outcome: FSAFreshPageDiscardTraceEvent['outcome'],
    completedFileCount: bigint,
    completedBytes: bigint,
    cut?: FSAFreshPageDiscardCut,
    reason?: 'target-ownership-unknown' | 'cleanup-unknown',
  ): void {
    try {
      this.#trace?.(Object.freeze({
        name: 'receive.fsa.fresh_discard.completed',
        operation_id: this.intent.operationId,
        receive_intent_digest: this.intent.digest,
        outcome,
        completed_file_count: completedFileCount,
        completed_bytes: completedBytes,
        removed_object_count: BigInt(cut?.removedObjectIds.length ?? 0),
        ...(reason === undefined ? {} : { needs_attention_reason: reason }),
      }))
    } catch {
      // The durable lifecycle and receipt remain authoritative when tracing fails.
    }
  }
}

interface VerifiedLifecycle {
  readonly state: ReceiveLifecycleState
  readonly record: PersistedReceiveRecord
}

interface VerifiedNeedsAttention extends VerifiedLifecycle {
  readonly state: Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>
}

async function freshPageCleanupReceipt(input: Readonly<{
  intent: DirectTreeIntent
  lifecycle: VerifiedLifecycle
  cut: FSAFreshPageDiscardCut
  checkpointSetDigest: string
  completedFileCount: bigint
  completedBytes: bigint
}>): Promise<PersistedReceiveRecord> {
  const candidateSetDigest = await checkpointEvidenceDigest(
    input.intent,
    'candidate',
    input.cut.candidateCheckpoints,
  )
  return createPersistedReceiveRecord({
    operationId: input.intent.operationId,
    kind: RECEIVE_RECORD_CLEANUP,
    canonicalBytes: canonicalRecord('windshare/receive-receipt/v1', 1, [
      canonicalU8(FSA_FRESH_PAGE_CLEANUP_RECEIPT),
      identityFrame(input.intent.operationId, 16, 'operation ID'),
      identityFrame(input.intent.digest, 32, 'receive intent digest'),
      identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
      identityFrame(input.lifecycle.record.digest, 32, 'prior lifecycle record digest'),
      canonicalFrame(canonicalU64(input.lifecycle.state.generation)),
      identityFrame(input.checkpointSetDigest, 32, 'checkpoint set digest'),
      identityFrame(candidateSetDigest, 32, 'candidate checkpoint set digest'),
      canonicalFrame(canonicalU64(input.completedFileCount)),
      canonicalFrame(canonicalU64(input.completedBytes)),
      canonicalFrame(canonicalU64(BigInt(input.cut.successfulCheckpoints.length))),
      ...input.cut.successfulCheckpoints.map(checkpoint =>
        canonicalFrame(canonicalCheckpointEvidence(checkpoint))),
      canonicalFrame(canonicalU64(BigInt(input.cut.removedObjectIds.length))),
      ...input.cut.removedObjectIds.map(value => identityFrame(value, 32, 'removed object ID')),
      canonicalFrame(canonicalU64(BigInt(input.cut.removedDirectoryHandleIds.length))),
      ...input.cut.removedDirectoryHandleIds.map(value => canonicalFrame(canonicalText(value))),
    ]),
  })
}

async function freshPagePartialReceipt(input: Readonly<{
  intent: DirectTreeIntent
  lifecycle: VerifiedLifecycle
  cleanupReceiptDigest: string
  successfulCheckpoints: readonly FileCheckpointV2[]
  completedBytes: bigint
}>): Promise<PersistedReceiveRecord> {
  return createPersistedReceiveRecord({
    operationId: input.intent.operationId,
    kind: RECEIVE_RECORD_RECEIPT,
    canonicalBytes: canonicalRecord('windshare/receive-receipt/v1', 1, [
      canonicalU8(FSA_FRESH_PAGE_PARTIAL_RECEIPT),
      identityFrame(input.intent.operationId, 16, 'operation ID'),
      identityFrame(input.intent.digest, 32, 'receive intent digest'),
      identityFrame(input.intent.plan.reservation.digest, 32, 'reservation digest'),
      identityFrame(input.lifecycle.record.digest, 32, 'prior lifecycle record digest'),
      identityFrame(input.cleanupReceiptDigest, 32, 'cleanup receipt digest'),
      canonicalFrame(canonicalU64(BigInt(input.successfulCheckpoints.length))),
      canonicalFrame(canonicalU64(input.completedBytes)),
      ...input.successfulCheckpoints.map(checkpoint =>
        canonicalFrame(canonicalCheckpointEvidence(checkpoint))),
    ]),
  })
}

async function checkpointEvidenceDigest(
  intent: DirectTreeIntent,
  label: 'candidate',
  checkpoints: readonly FileCheckpointV2[],
): Promise<string> {
  return canonicalDigest(canonicalRecord(`windshare/fsa-${label}-checkpoint-set/v1`, 1, [
    identityFrame(intent.operationId, 16, 'operation ID'),
    identityFrame(intent.digest, 32, 'receive intent digest'),
    identityFrame(intent.plan.reservation.digest, 32, 'reservation digest'),
    canonicalFrame(canonicalU64(BigInt(checkpoints.length))),
    ...checkpoints.map(checkpoint => canonicalFrame(canonicalCheckpointEvidence(checkpoint))),
  ]))
}

function canonicalCheckpointEvidence(checkpoint: FileCheckpointV2): CanonicalBytes {
  return concatCanonicalBytes([
    identityFrame(checkpoint.recordId, 32, 'checkpoint record ID'),
    identityFrame(fileCheckpointDigest(checkpoint), 32, 'checkpoint digest'),
    canonicalFrame(canonicalU64(checkpoint.checkpointGeneration)),
    identityFrame(checkpoint.fileId, 16, 'file ID'),
    identityFrame(checkpoint.fileRevision, 16, 'file revision'),
    canonicalFrame(canonicalU64(checkpoint.exactSize)),
    identityFrame(checkpoint.ownedObjectId, 32, 'owned object ID'),
    canonicalFrame(canonicalPath(checkpoint.canonicalPath)),
  ])
}

async function requireDirectTreeIntent(input: ReceiveIntent): Promise<DirectTreeIntent> {
  const intent = await validateReceiveIntent(input)
  if (intent.plan.kind !== 'direct-tree' || intent.artifact.kind !== 'directory-tree' ||
      intent.plan.reservation.authorityKind !== 'fsa-container' ||
      intent.plan.reservation.guarantees.profile !== 'fsa-tree' ||
      intent.plan.reservation.guarantees.delivery !== 'managed-target' ||
      intent.plan.reservation.guarantees.replacement !== 'coordinated-no-replace' ||
      intent.plan.reservation.guarantees.visibility !== 'prefix-visible') {
    throw new TypeError('Fresh-page FSA discard requires the frozen FSA DirectTree guarantees')
  }
  return intent as DirectTreeIntent
}

function cleanupObservationUnknown(
  operationId: string,
  cause: unknown,
): TargetOwnershipUnknownError {
  return cause instanceof TargetOwnershipUnknownError
    ? cause
    : new TargetOwnershipUnknownError('cleanup', operationId, { cause })
}

function combineFailures(existing: unknown, next: unknown): unknown {
  return existing === undefined
    ? next
    : new AggregateError([existing, next], 'Fresh-page FSA discard could not close its authorities')
}

function requireFreshDiscardAttentionReason(
  reason: Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>['reason'],
): 'target-ownership-unknown' | 'cleanup-unknown' {
  if (reason === 'target-ownership-unknown' || reason === 'cleanup-unknown') return reason
  throw new TypeError('Fresh-page FSA discard produced an unrelated NeedsAttention reason')
}

function identityFrame(value: string, width: number, label: string): CanonicalBytes {
  return canonicalFrame(canonicalIdentity(value, width, label))
}
