import { ByteRangeSet, byteRange } from '../../content/geometry'
import {
  type AutomaticCheckpointTrigger,
  type OutputCheckpointCost,
  type OutputCheckpointCostBudget,
  type OutputFileOwnership,
  type OutputSourceIdentity,
  VerifiedFinalOutputFile,
  outputCheckpointCost,
} from '../../transfer/output-session'
import {
  emitOutputTrace,
  observePerformance,
  outputTraceEvent,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  recordOutputException,
  type OutputDiagnosticsPorts,
  type PerformanceCheckpointCost,
} from '../diagnostics'
import { createFinalizedFileMaterializationRecords } from '../materialization-ledger/journal'
import type { MaterializationLedgerBindingV1 } from '../materialization-ledger/model'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  type FileCheckpointPhase,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import { finalFileCheckpointProof, type FileCheckpointJournal } from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentAutomaticCheckpointResult,
  PersistentByteRange,
  PersistentFileRecoveryPolicy,
  PersistentFileTransactionPort,
  PersistentFinalFileCommit,
  PersistentTreeFile,
  PersistentTemporarySpacePurpose,
  PersistentWriterPreflight,
  SemanticPersistentOutputJournal,
} from './contracts'
import { PersistentOutputError, TargetOwnershipUnknownError } from './errors'
import {
  PersistentRecoveryPreflightError,
  persistentCheckpointDeclineReason,
} from './recovery'
import { runPersistentOutputStage, type PersistentOutputStageScope } from './stage-diagnostics'

type PersistentTransactionFailureStage =
  | 'output_write'
  | 'output_commit'
  | 'checkpoint'
  | 'cleanup'

type PersistentTransactionTraceInput =
  | Readonly<{
      eventName: 'output_write'
      transition: 'transaction_failed' | 'transaction_committed' | 'commit_failed'
    }>
  | Readonly<{ eventName: 'checkpoint'; transition: 'failed' }>
  | Readonly<{ eventName: 'cleanup'; transition: 'failed' }>

type PersistentTransactionState = 'active' | 'paused' | 'retired' | 'committed'

export class PersistentFileTransaction implements PersistentFileTransactionPort {
  readonly revision: OpenedFileRevision
  readonly ownedObjectId: string
  readonly #handle: PersistentTreeFile
  readonly #checkpoints: FileCheckpointJournal
  readonly #semantic: SemanticPersistentOutputJournal | undefined
  readonly #ledgerBinding: MaterializationLedgerBindingV1 | undefined
  readonly #ownership: OutputFileOwnership
  readonly #source: OutputSourceIdentity
  readonly #recovery: PersistentFileRecoveryPolicy
  readonly #onClose: (transaction: PersistentFileTransaction) => void
  readonly #onOwnershipUnknown: (error: TargetOwnershipUnknownError) => void
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #stageScope: PersistentOutputStageScope | undefined
  readonly #initialDurable: readonly PersistentByteRange[]
  #checkpoint: FileCheckpointV2
  #ranges: ByteRangeSet
  #tail: Promise<unknown> = Promise.resolve()
  #finalCommit: PersistentFinalFileCommit | undefined
  #state: PersistentTransactionState = 'active'
  #writerOpen = false
  #nextPreservingOpenApproved = false
  #cumulativeWriteAmplificationBytes = 0n
  #observedDurableBytes: bigint
  #released = false

  constructor(input: Readonly<{
    revision: OpenedFileRevision
    handle: PersistentTreeFile
    checkpoint: FileCheckpointV2
    checkpoints: FileCheckpointJournal
    semantic?: SemanticPersistentOutputJournal
    ledgerBinding?: MaterializationLedgerBindingV1
    ownership: OutputFileOwnership
    source: OutputSourceIdentity
    recovery: PersistentFileRecoveryPolicy
    onClose?: (transaction: PersistentFileTransaction) => void
    onOwnershipUnknown?: (error: TargetOwnershipUnknownError) => void
    diagnostics?: OutputDiagnosticsPorts
    stageScope?: PersistentOutputStageScope
  }>) {
    this.revision = Object.freeze({ ...input.revision })
    this.ownedObjectId = input.handle.ownedObjectId
    this.#handle = input.handle
    this.#checkpoint = input.checkpoint
    this.#checkpoints = input.checkpoints
    this.#semantic = input.semantic
    this.#ledgerBinding = input.ledgerBinding
    this.#ownership = Object.freeze({
      ...input.ownership,
      canonicalPath: Object.freeze([...input.ownership.canonicalPath]),
    })
    this.#source = Object.freeze({ ...input.source })
    this.#recovery = input.recovery
    this.#onClose = input.onClose ?? (() => undefined)
    this.#onOwnershipUnknown = input.onOwnershipUnknown ?? (() => undefined)
    this.#diagnostics = input.diagnostics
    this.#stageScope = input.stageScope
    this.#initialDurable = Object.freeze(input.checkpoint.verifiedRanges.map(range =>
      Object.freeze({ ...range })))
    this.#ranges = new ByteRangeSet(input.revision.exactSize, input.checkpoint.verifiedRanges)
    this.#observedDurableBytes = rangeBytes(input.checkpoint.verifiedRanges)
  }

  get initialDurableRanges(): readonly PersistentByteRange[] {
    return this.#initialDurable
  }

  get verifiedRanges(): readonly PersistentByteRange[] {
    return this.#checkpoint.verifiedRanges
  }

  writeRange(offset: bigint, data: Uint8Array, signal?: AbortSignal): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue('output_write', async () => {
      throwIfAborted(signal)
      this.#requireActive()
      const end = offset + BigInt(snapshot.byteLength)
      if (offset < 0n || end < offset || end > this.revision.exactSize) {
        throw new RangeError('Persistent output write exceeds its opened revision')
      }
      if (snapshot.byteLength === 0) return
      await this.#ensureWriter()
      await this.#handle.writeAt(offset, snapshot)
      // Native resolution is the irrevocable acceptance boundary. Cancellation
      // arriving afterward is observed by the next transfer/control operation so
      // its pause cut includes exactly the range reported as accepted here.
      this.#ranges = this.#ranges.union(new ByteRangeSet(
        this.revision.exactSize,
        [byteRange(offset, end)],
      ))
      observePerformance(this.#diagnostics?.performance, summary =>
        summary.observeByteTransition('pending', BigInt(snapshot.byteLength)))
    })
  }

  automaticCheckpoint(
    _trigger: AutomaticCheckpointTrigger,
    budget: OutputCheckpointCostBudget,
    signal?: AbortSignal,
  ): Promise<PersistentAutomaticCheckpointResult> {
    return this.#enqueue('checkpoint', () => this.#observeAutomaticCheckpoint(async () => {
      throwIfAborted(signal)
      this.#requireActive()
      if (sameRanges(this.#ranges.ranges, this.#checkpoint.verifiedRanges)) {
        return Object.freeze({
          kind: 'advanced' as const,
          durableRanges: this.#checkpoint.verifiedRanges,
          cost: zeroCheckpointCost(),
        })
      }
      const preflight = this.#handle.checkpointPreflight?.(
        writtenBoundary(this.#ranges.ranges),
        this.#cumulativeWriteAmplificationBytes,
      )
      if (preflight === undefined) {
        return Object.freeze({
          kind: 'declined' as const,
          reason: 'cost-evidence-unavailable' as const,
          estimate: zeroCheckpointCost(),
        })
      }
      const cost = outputCheckpointCost(preflight.cost)
      const reason = persistentCheckpointDeclineReason(cost, budget)
      if (reason !== undefined) {
        return Object.freeze({ kind: 'declined' as const, reason, estimate: cost })
      }
      if (preflight.space === 'requires-user-confirmation' &&
          !(await this.#confirmTemporarySpace(preflight, 'automatic-checkpoint'))) {
        return Object.freeze({
          kind: 'declined' as const,
          reason: 'temporary-space-confirmation-required' as const,
          estimate: cost,
        })
      }
      const durable = await this.#commitDurableCut(FILE_CHECKPOINT_PHASE_ACTIVE, signal)
      this.#cumulativeWriteAmplificationBytes = cost.cumulativeWriteAmplificationBytes
      this.#nextPreservingOpenApproved = true
      return Object.freeze({
        kind: 'advanced' as const,
        durableRanges: durable,
        cost,
      })
    }))
  }

  checkpoint(signal?: AbortSignal): Promise<readonly PersistentByteRange[]> {
    return this.#enqueue(
      'checkpoint',
      () => {
        this.#requireActive()
        return this.#commitDurableCut(FILE_CHECKPOINT_PHASE_ACTIVE, signal)
      },
    )
  }

  commit(signal?: AbortSignal): Promise<PersistentFinalFileCommit> {
    return this.#enqueue('output_commit', async () => {
      throwIfAborted(signal)
      if (this.#finalCommit !== undefined) return this.#finalCommit
      this.#requireActive()
      if (!this.#ranges.covers(byteRange(0n, this.revision.exactSize))) {
        throw new PersistentOutputError(
          'incomplete-file',
          'A prefix-visible file cannot be finalized with missing ranges',
        )
      }

      const alreadyDurable = sameRanges(this.#ranges.ranges, this.#checkpoint.verifiedRanges)
      if (alreadyDurable && fileCheckpointIsComplete(this.#checkpoint)) {
        const recovered = await this.#recoverFinalCommit(this.#checkpoint)
        if (recovered !== undefined) return this.#finishCommit(recovered)
      }

      if (!alreadyDurable || !fileCheckpointIsComplete(this.#checkpoint) ||
          this.revision.exactSize === 0n) {
        if (this.revision.exactSize !== 0n) {
          await this.#ensureWriter()
          await this.#closeWriter()
        }
        const size = await this.#handle.size()
        if (size !== this.revision.exactSize) {
          throw new PersistentOutputError(
            'incomplete-file',
            'The materialized file does not match its exact opened revision',
          )
        }
        await this.#handle.verify('commit')
      }

      const finalCheckpoint = nextCheckpoint(
        this.#checkpoint,
        this.#ranges.ranges,
        FILE_CHECKPOINT_PHASE_ACTIVE,
        true,
      )
      const finalOutput = new VerifiedFinalOutputFile(
        this.#ownership,
        this.#source,
        this.revision.exactSize,
      )
      if (this.#semantic !== undefined && this.#ledgerBinding !== undefined) {
        await this.#commitSemanticFinal(
          finalCheckpoint,
          finalOutput,
          this.#semantic,
          this.#ledgerBinding,
        )
      } else {
        await this.#commitLegacyCheckpoint(finalCheckpoint)
        this.#checkpoint = finalCheckpoint
      }
      throwIfAborted(signal)
      const checkpointProof = finalFileCheckpointProof(this.#checkpoint)
      return this.#finishCommit(Object.freeze({
        ...checkpointProof,
        checkpointProof,
        finalOutput,
      }))
    })
  }

  pause(reason?: unknown): Promise<readonly PersistentByteRange[]> {
    return this.#enqueue('checkpoint', async () => {
      if (this.#state === 'paused') return this.#checkpoint.verifiedRanges
      this.#requireActive()
      try {
        const durable = await this.#observeForcedPauseCheckpoint(
          () => this.#commitDurableCut(FILE_CHECKPOINT_PHASE_PAUSED),
        )
        this.#state = 'paused'
        return durable
      } finally {
        this.#release()
      }
    }).catch((error: unknown) => {
      if (reason instanceof Error && error instanceof Error && error.cause === undefined) {
        Object.defineProperty(error, 'cause', { value: reason, configurable: true })
      }
      throw error
    })
  }

  retire(reason?: unknown): Promise<void> {
    return this.#enqueue('cleanup', async () => {
      if (this.#state === 'retired' || this.#state === 'paused' || this.#state === 'committed') return
      if (this.#state !== 'active') {
        throw new PersistentOutputError('output-state', 'Persistent file transaction is settled')
      }
      this.#state = 'retired'
      try {
        if (this.#writerOpen) {
          if (this.#handle.abort !== undefined) await this.#handle.abort(reason)
          else await this.#handle.close()
          this.#writerOpen = false
        }
      } finally {
        this.#release()
      }
    })
  }

  close(): Promise<void> {
    if (this.#state === 'retired' || this.#state === 'paused' || this.#state === 'committed') {
      return Promise.resolve()
    }
    return this.retire()
  }

  async #commitDurableCut(
    phase: FileCheckpointPhase,
    signal?: AbortSignal,
  ): Promise<readonly PersistentByteRange[]> {
    throwIfAborted(signal)
    const rangesChanged = !sameRanges(this.#ranges.ranges, this.#checkpoint.verifiedRanges)
    // A rejected native write may have opened or partially mutated a private writer
    // without producing accepted range evidence. Abort that unowned state before the
    // pause cut so the writer lease cannot outlive the transaction.
    if (!rangesChanged && this.#writerOpen) await this.#abortWriter()
    if (rangesChanged) {
      await this.#closeWriter()
      const actualSize = await this.#handle.size()
      const writtenEnd = writtenBoundary(this.#ranges.ranges)
      if (actualSize < writtenEnd || actualSize > this.revision.exactSize) {
        throw new TargetOwnershipUnknownError('checkpoint', this.#checkpoint.operationId)
      }
      await this.#handle.verify('checkpoint')
    }
    if (!rangesChanged && phase === this.#checkpoint.phase) {
      return this.#checkpoint.verifiedRanges
    }
    const durable = nextCheckpoint(
      this.#checkpoint,
      this.#ranges.ranges,
      phase,
      this.#semantic === undefined && !rangesChanged && phase !== this.#checkpoint.phase,
    )
    if (this.#semantic !== undefined) {
      await runPersistentOutputStage(
        this.#stageScopeFor(durable),
        phase === FILE_CHECKPOINT_PHASE_PAUSED
          ? 'indexeddb.checkpoint.pause-commit'
          : 'indexeddb.checkpoint.durable-commit',
        () => this.#semantic!.commitDurableCut(this.#checkpoint, durable),
      )
    } else {
      await this.#commitLegacyCheckpoint(durable)
    }
    this.#checkpoint = durable
    throwIfAborted(signal)
    return durable.verifiedRanges
  }

  async #observeAutomaticCheckpoint(
    operation: () => Promise<PersistentAutomaticCheckpointResult>,
  ): Promise<PersistentAutomaticCheckpointResult> {
    const startedAtMilliseconds = performanceNowMilliseconds(this.#diagnostics?.performance)
    const result = await operation()
    const durableBytes = result.kind === 'advanced'
      ? rangeBytes(result.durableRanges)
      : this.#observedDurableBytes
    const elapsedMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      performanceNowMilliseconds(this.#diagnostics?.performance),
    )
    if (elapsedMilliseconds !== undefined) {
      const cost = result.kind === 'advanced' ? result.cost : result.estimate
      observePerformance(this.#diagnostics?.performance, summary => {
        summary.observeCheckpoint({
          trigger: 'automatic',
          decision: result.kind,
          cost: checkpointPerformanceCost(cost),
          elapsedMilliseconds,
          estimatedCopyBytes: cost.prefixCopyBytes,
        })
        if (result.kind === 'advanced') {
          summary.observeByteTransition(
            'durable',
            durableByteAdvance(this.#observedDurableBytes, durableBytes),
          )
        }
      })
    }
    this.#observedDurableBytes = durableBytes
    return result
  }

  async #observeForcedPauseCheckpoint(
    operation: () => Promise<readonly PersistentByteRange[]>,
  ): Promise<readonly PersistentByteRange[]> {
    const startedAtMilliseconds = performanceNowMilliseconds(this.#diagnostics?.performance)
    const durable = await operation()
    const durableBytes = rangeBytes(durable)
    const elapsedMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      performanceNowMilliseconds(this.#diagnostics?.performance),
    )
    if (elapsedMilliseconds !== undefined) {
      observePerformance(this.#diagnostics?.performance, summary => {
        summary.observeCheckpoint({
          trigger: 'forced_pause',
          decision: 'advanced',
          cost: 'constant',
          elapsedMilliseconds,
        })
        summary.observeByteTransition(
          'durable',
          durableByteAdvance(this.#observedDurableBytes, durableBytes),
        )
      })
    }
    this.#observedDurableBytes = durableBytes
    return durable
  }

  async #commitLegacyCheckpoint(committed: FileCheckpointV2): Promise<void> {
    const candidate = newFileCheckpointV2({
      ...committed,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    const scope = this.#stageScopeFor(committed)
    await runPersistentOutputStage(
      scope,
      'indexeddb.checkpoint.candidate-stage',
      () => this.#checkpoints.stageCheckpointUpdate(this.#checkpoint, candidate),
    )
    await runPersistentOutputStage(
      scope,
      'indexeddb.checkpoint.commit',
      () => this.#checkpoints.commitCheckpointCandidate(candidate, committed),
    )
  }

  async #commitSemanticFinal(
    finalCheckpoint: FileCheckpointV2,
    finalOutput: VerifiedFinalOutputFile,
    semantic: SemanticPersistentOutputJournal,
    ledgerBinding: MaterializationLedgerBindingV1,
  ): Promise<void> {
    const records = await createFinalizedFileMaterializationRecords({
      binding: ledgerBinding,
      finalOutput,
      finalCheckpoint,
    })
    const finalStartedAtMilliseconds = performanceNowMilliseconds(
      this.#diagnostics?.performance,
    )
    const receipt = await runPersistentOutputStage(
      this.#stageScopeFor(finalCheckpoint),
      'indexeddb.checkpoint.final-commit',
      () => semantic.commitFinalFile({
        binding: ledgerBinding,
        expectedCommittedCheckpoint: this.#checkpoint,
        records,
        expectedPersistedOwnedFileIdentity: this.ownedObjectId,
      }),
    )
    const finalElapsedMilliseconds = performanceElapsedMilliseconds(
      finalStartedAtMilliseconds,
      performanceNowMilliseconds(this.#diagnostics?.performance),
    )
    if (finalElapsedMilliseconds !== undefined) {
      observePerformance(this.#diagnostics?.performance, summary => {
        summary.observeFinalTransaction(finalElapsedMilliseconds)
        if (receipt.classification === 'insert') {
          summary.observeLedger({ transition: 'entry' })
        }
      })
    }
    this.#checkpoint = receipt.finalCheckpoint
  }

  async #recoverFinalCommit(
    checkpoint: FileCheckpointV2,
  ): Promise<PersistentFinalFileCommit | undefined> {
    if (this.#semantic === undefined || this.#ledgerBinding === undefined) return undefined
    const finalOutput = new VerifiedFinalOutputFile(
      this.#ownership,
      this.#source,
      this.revision.exactSize,
    )
    const records = await createFinalizedFileMaterializationRecords({
      binding: this.#ledgerBinding,
      finalOutput,
      finalCheckpoint: checkpoint,
    })
    const persisted = await this.#semantic.readMaterializationFinalProof(
      this.#ledgerBinding,
      records.finalProof.proofId,
    )
    if (persisted?.proofDigest !== records.finalProof.proofDigest) return undefined
    return Object.freeze({
      ...finalFileCheckpointProof(checkpoint),
      checkpointProof: finalFileCheckpointProof(checkpoint),
      finalOutput,
    })
  }

  async #ensureWriter(): Promise<void> {
    if (this.#writerOpen) return
    const durablePrefix = writtenBoundary(this.#checkpoint.verifiedRanges)
    const mode = durablePrefix === 0n ? 'truncate' as const : 'preserve' as const
    if (mode === 'preserve' && !this.#nextPreservingOpenApproved) {
      await this.#approvePreservingWriter(durablePrefix)
    }
    await this.#handle.openWriter?.(mode)
    this.#writerOpen = true
    this.#nextPreservingOpenApproved = false
  }

  async #approvePreservingWriter(durablePrefix: bigint): Promise<void> {
    const preflight = this.#handle.checkpointPreflight?.(
      durablePrefix,
      this.#cumulativeWriteAmplificationBytes,
    )
    const budget = this.#recovery.costBudget
    if (preflight === undefined || budget === undefined) {
      throw new PersistentRecoveryPreflightError({
        reason: 'cost-evidence-unavailable',
        preflight: preflight ?? unavailablePreflight(durablePrefix),
        ...(budget === undefined ? {} : { budget }),
      })
    }
    const reason = persistentCheckpointDeclineReason(preflight.cost, budget)
    if (reason !== undefined) {
      throw new PersistentRecoveryPreflightError({ reason, preflight, budget })
    }
    if (preflight.space === 'requires-user-confirmation' &&
        !(await this.#confirmTemporarySpace(preflight, 'paused-file-recovery'))) {
      throw new PersistentRecoveryPreflightError({
        reason: 'space-confirmation-required',
        preflight,
        budget,
      })
    }
    this.#cumulativeWriteAmplificationBytes = preflight.cost.cumulativeWriteAmplificationBytes
  }

  async #confirmTemporarySpace(
    preflight: PersistentWriterPreflight,
    purpose: PersistentTemporarySpacePurpose,
  ): Promise<boolean> {
    if (this.#recovery.confirmTemporarySpace === undefined) return false
    return this.#recovery.confirmTemporarySpace(preflight, purpose)
  }

  async #closeWriter(): Promise<void> {
    if (!this.#writerOpen) return
    await this.#handle.flush()
    this.#writerOpen = false
  }

  async #abortWriter(): Promise<void> {
    if (!this.#writerOpen) return
    if (this.#handle.abort !== undefined) await this.#handle.abort()
    else await this.#handle.close()
    this.#writerOpen = false
  }

  #finishCommit(commit: PersistentFinalFileCommit): PersistentFinalFileCommit {
    this.#finalCommit = commit
    this.#state = 'committed'
    this.#release()
    const durableBytes = rangeBytes(this.#checkpoint.verifiedRanges)
    observePerformance(this.#diagnostics?.performance, summary => {
      summary.observeByteTransition(
        'durable',
        durableByteAdvance(this.#observedDurableBytes, durableBytes),
      )
      summary.observeByteTransition('final', this.revision.exactSize)
    })
    this.#observedDurableBytes = durableBytes
    this.#trace({ eventName: 'output_write', transition: 'transaction_committed' })
    return commit
  }

  #stageScopeFor(checkpoint: FileCheckpointV2): PersistentOutputStageScope | undefined {
    return this.#stageScope?.withCorrelation({
      checkpointRecordId: checkpoint.recordId,
      checkpointGeneration: checkpoint.checkpointGeneration,
    })
  }

  #enqueue<T>(
    stage: PersistentTransactionFailureStage,
    operation: () => Promise<T>,
  ): Promise<T> {
    const observed = async (): Promise<T> => {
      try {
        return await operation()
      } catch (error) {
        this.#recordFailure(stage, error)
        if (error instanceof TargetOwnershipUnknownError) this.#onOwnershipUnknown(error)
        throw error
      }
    }
    const result = this.#tail.then(observed, observed)
    this.#tail = result
    return result
  }

  #release(): void {
    if (this.#released) return
    this.#released = true
    this.#onClose(this)
  }

  #recordFailure(stage: PersistentTransactionFailureStage, error: unknown): void {
    const failures = this.#diagnostics?.failures
    switch (stage) {
      case 'output_write':
        recordOutputException(failures?.outputWrite, error)
        this.#trace({ eventName: 'output_write', transition: 'transaction_failed' })
        return
      case 'output_commit':
        recordOutputException(failures?.outputCommit, error)
        this.#trace({ eventName: 'output_write', transition: 'commit_failed' })
        return
      case 'checkpoint':
        recordOutputException(failures?.checkpoint, error)
        this.#trace({ eventName: 'checkpoint', transition: 'failed' })
        return
      case 'cleanup':
        recordOutputException(failures?.cleanup, error)
        this.#trace({ eventName: 'cleanup', transition: 'failed' })
    }
  }

  #trace(input: PersistentTransactionTraceInput): void {
    const diagnostics = this.#diagnostics
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent(input.eventName, {
        backend: diagnostics?.backend === 'file_system_access'
          ? 'file_system_access'
          : 'origin_private',
        transition: input.transition,
      }))
  }

  #requireActive(): void {
    if (this.#state !== 'active') {
      throw new PersistentOutputError('output-state', 'Persistent file transaction is settled')
    }
  }
}

function nextCheckpoint(
  previous: FileCheckpointV2,
  ranges: readonly PersistentByteRange[],
  phase: FileCheckpointPhase,
  forceCheckpointGeneration: boolean,
): FileCheckpointV2 {
  const rangesChanged = !sameRanges(ranges, previous.verifiedRanges)
  return newFileCheckpointV2({
    ...previous,
    stateGeneration: previous.stateGeneration + 1n,
    checkpointGeneration: previous.checkpointGeneration +
      (rangesChanged || forceCheckpointGeneration ? 1n : 0n),
    verifiedRanges: ranges,
    phase,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function writtenBoundary(ranges: readonly PersistentByteRange[]): bigint {
  return ranges.at(-1)?.end ?? 0n
}

function rangeBytes(ranges: readonly PersistentByteRange[]): bigint {
  return ranges.reduce((total, range) => total + (range.end - range.start), 0n)
}

function durableByteAdvance(previous: bigint, current: bigint): bigint {
  return current > previous ? current - previous : 0n
}

function checkpointPerformanceCost(cost: OutputCheckpointCost): PerformanceCheckpointCost {
  if (cost.peakTemporaryBytes > 0n) return 'space_preflight'
  if (cost.prefixCopyBytes > 0n || cost.cumulativeWriteAmplificationBytes > 0n) {
    return 'prefix_copy'
  }
  return 'constant'
}

function sameRanges(
  left: readonly PersistentByteRange[],
  right: readonly PersistentByteRange[],
): boolean {
  return left.length === right.length && left.every((range, index) =>
    range.start === right[index]?.start && range.end === right[index]?.end)
}

function zeroCheckpointCost(): OutputCheckpointCost {
  return Object.freeze({
    prefixCopyBytes: 0n,
    cumulativeWriteAmplificationBytes: 0n,
    peakTemporaryBytes: 0n,
  })
}

function unavailablePreflight(prefix: bigint): PersistentWriterPreflight {
  return Object.freeze({
    cost: Object.freeze({
      prefixCopyBytes: prefix,
      cumulativeWriteAmplificationBytes: prefix,
      peakTemporaryBytes: prefix,
    }),
    space: prefix === 0n ? 'within-modeled-budget' : 'requires-user-confirmation',
  })
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  signal?.throwIfAborted()
}
