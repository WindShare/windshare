import { ByteRangeSet, byteRange } from '../../content/geometry'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentByteRange,
  PersistentFileTransactionPort,
  PersistentTreeFile,
} from './contracts'
import { PersistentOutputError, TargetOwnershipUnknownError } from './errors'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from './stage-diagnostics'

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
  | Readonly<{
      eventName: 'checkpoint'
      transition: 'failed'
    }>
  | Readonly<{
      eventName: 'cleanup'
      transition: 'failed'
    }>

export class PersistentFileTransaction implements PersistentFileTransactionPort {
  readonly revision: OpenedFileRevision
  readonly ownedObjectId: string
  readonly #handle: PersistentTreeFile
  readonly #checkpoints: FileCheckpointJournal
  readonly #onClose: (transaction: PersistentFileTransaction) => void
  readonly #onOwnershipUnknown: (error: TargetOwnershipUnknownError) => void
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #stageScope: PersistentOutputStageScope | undefined
  #checkpoint: FileCheckpointV2
  #ranges: ByteRangeSet
  #tail: Promise<unknown> = Promise.resolve()
  #finalProof: FinalFileCheckpointProof | undefined
  #closed = false

  constructor(input: Readonly<{
    revision: OpenedFileRevision
    handle: PersistentTreeFile
    checkpoint: FileCheckpointV2
    checkpoints: FileCheckpointJournal
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
    this.#onClose = input.onClose ?? (() => undefined)
    this.#onOwnershipUnknown = input.onOwnershipUnknown ?? (() => undefined)
    this.#diagnostics = input.diagnostics
    this.#stageScope = input.stageScope
    this.#ranges = new ByteRangeSet(input.revision.exactSize, input.checkpoint.verifiedRanges)
  }

  get verifiedRanges(): readonly PersistentByteRange[] {
    return this.#checkpoint.verifiedRanges
  }

  writeRange(
    offset: bigint,
    data: Uint8Array,
    signal?: AbortSignal,
  ): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue('output_write', async () => {
      throwIfAborted(signal)
      this.#requireMutable()
      const end = offset + BigInt(snapshot.byteLength)
      if (offset < 0n || end < offset || end > this.revision.exactSize) {
        throw new RangeError('Persistent output write exceeds its opened revision')
      }
      if (snapshot.byteLength === 0) return
      await this.#handle.writeAt(offset, snapshot)
      this.#ranges = this.#ranges.union(new ByteRangeSet(
        this.revision.exactSize,
        [byteRange(offset, end)],
      ))
      throwIfAborted(signal)
    })
  }

  checkpoint(signal?: AbortSignal): Promise<readonly PersistentByteRange[]> {
    return this.#enqueue('checkpoint', async () => {
      return this.#checkpointNow(signal)
    })
  }

  commit(signal?: AbortSignal): Promise<FinalFileCheckpointProof> {
    return this.#enqueue('output_commit', async () => {
      throwIfAborted(signal)
      this.#requireOpen()
      if (this.#finalProof !== undefined) {
        await this.#handle.verify('commit')
        return this.#finalProof
      }
      const wanted = byteRange(0n, this.revision.exactSize)
      if (!this.#ranges.covers(wanted)) {
        throw new PersistentOutputError(
          'incomplete-file',
          'A prefix-visible file cannot be finalized with missing ranges',
        )
      }
      await this.#checkpointNow(signal)
      const size = await this.#handle.size()
      if (size !== this.revision.exactSize || !fileCheckpointIsComplete(this.#checkpoint)) {
        throw new PersistentOutputError(
          'incomplete-file',
          'The materialized file does not match its exact opened revision',
        )
      }
      await this.#handle.verify('commit')
      const proof = await this.#checkpoints.finalCheckpointProof(
        this.#checkpoint.recordId,
        this.#checkpoint.checkpointGeneration,
      )
      // The filesystem observation follows the durable checkpoint read, closing the
      // window where an external replacement could masquerade as successful commit.
      await this.#handle.verify('commit')
      this.#finalProof = proof
      this.#trace({
        eventName: 'output_write',
        transition: 'transaction_committed',
      })
      return proof
    })
  }

  close(): Promise<void> {
    return this.#enqueue('cleanup', async () => {
      if (this.#closed) return
      this.#closed = true
      try {
        await this.#handle.close()
      } finally {
        this.#onClose(this)
      }
    })
  }

  async #checkpointNow(signal?: AbortSignal): Promise<readonly PersistentByteRange[]> {
    throwIfAborted(signal)
    this.#requireMutable()
    await this.#handle.flush()
    await this.#handle.verify('checkpoint')
    const actualSize = await this.#handle.size()
    const writtenEnd = this.#ranges.ranges.at(-1)?.end ?? 0n
    if (actualSize < writtenEnd || actualSize > this.revision.exactSize) {
      throw new TargetOwnershipUnknownError('checkpoint', this.#checkpoint.operationId)
    }
    if (sameRanges(this.#ranges.ranges, this.#checkpoint.verifiedRanges)) {
      return this.#checkpoint.verifiedRanges
    }
    const candidate = newFileCheckpointV2({
      ...this.#checkpoint,
      checkpointGeneration: this.#checkpoint.checkpointGeneration + 1n,
      verifiedRanges: this.#ranges.ranges,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    const candidateScope = this.#stageScope?.withCorrelation({
      checkpointRecordId: candidate.recordId,
      checkpointGeneration: candidate.checkpointGeneration,
    })
    await runPersistentOutputStage(
      candidateScope,
      'indexeddb.checkpoint.candidate-stage',
      () => this.#checkpoints.stageCheckpointUpdate(this.#checkpoint, candidate),
    )
    await this.#handle.verify('checkpoint')
    const committed = newFileCheckpointV2({
      ...candidate,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
    const committedScope = candidateScope?.withCorrelation({
      checkpointRecordId: committed.recordId,
      checkpointGeneration: committed.checkpointGeneration,
    })
    await runPersistentOutputStage(
      committedScope,
      'indexeddb.checkpoint.commit',
      () => this.#checkpoints.commitCheckpointCandidate(candidate, committed),
    )
    const verified = await runPersistentOutputStage(
      committedScope,
      'indexeddb.checkpoint.committed-read',
      () => this.#checkpoints.readCommitted(committed.recordId),
    )
    if (verified === undefined || verified.checksum !== committed.checksum ||
        verified.checkpointGeneration !== committed.checkpointGeneration) {
      throw new TargetOwnershipUnknownError('checkpoint', this.#checkpoint.operationId)
    }
    await this.#handle.verify('checkpoint')
    this.#checkpoint = verified
    throwIfAborted(signal)
    return verified.verifiedRanges
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

  #recordFailure(stage: PersistentTransactionFailureStage, error: unknown): void {
    const failures = this.#diagnostics?.failures
    switch (stage) {
      case 'output_write':
        recordOutputException(failures?.outputWrite, error)
        this.#trace({
          eventName: 'output_write',
          transition: 'transaction_failed',
        })
        return
      case 'output_commit':
        recordOutputException(failures?.outputCommit, error)
        this.#trace({
          eventName: 'output_write',
          transition: 'commit_failed',
        })
        return
      case 'checkpoint':
        recordOutputException(failures?.checkpoint, error)
        this.#trace({ eventName: 'checkpoint', transition: 'failed' })
        return
      case 'cleanup':
        recordOutputException(failures?.cleanup, error)
        this.#trace({ eventName: 'cleanup', transition: 'failed' })
        return
      default:
        return
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

  #requireOpen(): void {
    if (this.#closed) {
      throw new PersistentOutputError('output-state', 'Persistent file transaction is closed')
    }
  }

  #requireMutable(): void {
    this.#requireOpen()
    if (this.#finalProof !== undefined) {
      throw new PersistentOutputError('output-state', 'Committed persistent file is immutable')
    }
  }
}

function sameRanges(
  left: readonly PersistentByteRange[],
  right: readonly PersistentByteRange[],
): boolean {
  return left.length === right.length && left.every((range, index) =>
    range.start === right[index]?.start && range.end === right[index]?.end)
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  signal?.throwIfAborted()
}
