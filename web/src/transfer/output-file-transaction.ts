import { ByteRangeSet, byteRange } from '../content/geometry'
import {
  snapshotOpenedOutputRevision,
  VerifiedDurableRanges,
  VerifiedFinalOutputFile,
} from './output-session'
import type {
  AutomaticCheckpointResult,
  AutomaticCheckpointTrigger,
  BeginOutputFileResult,
  FileRetirementDisposition,
  OutputFile,
  OutputFileOwnership,
  OutputFileTransaction,
  OutputSession,
  OutputSessionIdentity,
  OutputSourceIdentity,
} from './output-session'

export interface BoundOutputFileTransaction {
  readonly transaction: OutputFileTransaction
  readonly initialDurable: VerifiedDurableRanges
}

/**
 * Keeps resume identity checks on every checkpoint, because validating only
 * BeginFile would let a backend accidentally rebind later have-state.
 */
export function bindOutputFileTransaction(
  begun: BeginOutputFileResult,
  file: OutputFile,
  session: Pick<OutputSession, 'identity' | 'capabilities'>,
): BoundOutputFileTransaction {
  try {
    const transaction = ownOutputFileTransaction(begun)
    const revision = snapshotOpenedOutputRevision(begun.revision)
    if (!sameOutputSource(revision, file.source) || revision.exactSize !== file.exactSize) {
      throw new OutputCheckpointContractError(
        'output transaction opened a different authenticated source revision',
      )
    }
    const initialDurable = requireOutputBinding(begun.durableRanges, file, session.identity)
    const durable = session.capabilities.durability !== 'None'
    if (!durable && !initialDurable.asRangeSet().empty) {
      throw new OutputCheckpointContractError('transient output cannot claim durable resumable ranges')
    }
    if (!session.capabilities.randomWrite) requireCanonicalPrefix(initialDurable.asRangeSet())
    return Object.freeze({
      transaction: new SourceBoundOutputTransaction(
        transaction,
        file,
        session.identity,
        initialDurable.ownership,
        initialDurable,
        durable,
        session.capabilities.randomWrite,
      ),
      initialDurable,
    })
  } catch (cause) {
    if (cause instanceof OutputTransactionContractError) throw cause
    throw new OutputTransactionContractError('output transaction binding validation failed', { cause })
  }
}

/** Takes settlement ownership before any resume evidence is interpreted. */
export function ownOutputFileTransaction(begun: BeginOutputFileResult): OutputFileTransaction {
  try {
    const candidate = (begun as unknown as { readonly transaction?: unknown } | null)?.transaction
    if (typeof candidate !== 'object' || candidate === null ||
        !hasFunction(candidate, 'writeRange') || !hasFunction(candidate, 'automaticCheckpoint') ||
         !hasFunction(candidate, 'commit') || !hasFunction(candidate, 'retire') ||
         !hasFunction(candidate, 'pause')) {
      throw new OutputTransactionContractError('BeginFile did not return a complete output transaction')
    }
    return candidate as OutputFileTransaction
  } catch (cause) {
    if (cause instanceof OutputTransactionContractError) throw cause
    throw new OutputTransactionContractError('BeginFile transaction ownership could not be established', { cause })
  }
}

export async function retireOutputFileTransaction(
  transaction: OutputFileTransaction,
  reason: unknown,
): Promise<FileRetirementDisposition> {
  const disposition = await transaction.retire(reason)
  if (disposition !== 'FileIsolated' && disposition !== 'JobOutputCompromised') {
    throw new OutputTransactionContractError('output transaction returned an invalid retirement disposition')
  }
  return disposition
}

class SourceBoundOutputTransaction implements OutputFileTransaction {
  readonly #transaction: OutputFileTransaction
  readonly #file: OutputFile
  readonly #session: OutputSessionIdentity
  readonly #ownership: OutputFileOwnership
  readonly #durable: boolean
  readonly #positioned: boolean
  #durableRanges: VerifiedDurableRanges
  #pendingRanges: ByteRangeSet
  #nextSequentialOffset: bigint
  #automaticCheckpointFinished = false
  #state: 'open' | 'committing' | 'pausing' | 'retiring' | 'committed' | 'paused' | 'retired' | 'failed' = 'open'

  constructor(
    transaction: OutputFileTransaction,
    file: OutputFile,
    session: OutputSessionIdentity,
    ownership: OutputFileOwnership,
    durableRanges: VerifiedDurableRanges,
    durable: boolean,
    positioned: boolean,
  ) {
    this.#transaction = transaction
    this.#file = file
    this.#session = session
    this.#ownership = ownership
    this.#durable = durable
    this.#positioned = positioned
    this.#durableRanges = durableRanges
    this.#pendingRanges = new ByteRangeSet(file.exactSize, [])
    this.#nextSequentialOffset = positioned ? 0n : requireCanonicalPrefix(durableRanges.asRangeSet())
  }

  async writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#requireOpen('write')
    const end = offset + BigInt(data.byteLength)
    if (data.byteLength === 0 || offset < 0n || end > this.#file.exactSize) {
      throw new RangeError('output write range exceeds its file transaction')
    }
    const written = byteRange(offset, end)
    if (overlapsAny(written, this.#durableRanges.ranges) || overlapsAny(written, this.#pendingRanges.ranges)) {
      throw new OutputCheckpointContractError('output write overlaps bytes already accepted or durable')
    }
    if (!this.#positioned && offset !== this.#nextSequentialOffset) {
      throw new OutputCheckpointContractError('sequential output writes must extend its durable and pending prefix')
    }
    await this.#transaction.writeRange(offset, data, signal)
    // A resolved write is accepted state even if cancellation raced its completion;
    // pause must therefore include it in the forced cut rather than lose ownership truth.
    this.#pendingRanges = this.#pendingRanges.union(new ByteRangeSet(this.#file.exactSize, [written]))
    if (!this.#positioned) this.#nextSequentialOffset = end
  }

  async automaticCheckpoint(
    trigger: AutomaticCheckpointTrigger,
    signal: AbortSignal,
  ): Promise<AutomaticCheckpointResult> {
    signal.throwIfAborted()
    this.#requireOpen('checkpoint')
    if (this.#automaticCheckpointFinished) {
      throw new OutputCheckpointContractError('automatic checkpoint evaluation already finished')
    }
    if (trigger !== 'pending-bytes' && trigger !== 'pending-time') {
      throw new OutputCheckpointContractError('output checkpoint trigger is invalid')
    }
    if (this.#pendingRanges.empty) {
      throw new OutputCheckpointContractError('automatic checkpoint requires accepted pending ranges')
    }
    const result = await this.#transaction.automaticCheckpoint(trigger, signal)
    if (typeof result !== 'object' || result === null) {
      throw new OutputCheckpointContractError('output checkpoint returned a malformed decision')
    }
    if (result.kind === 'deferred') {
      return snapshotAutomaticCheckpointDeferral(result.reason)
    }
    if (result.kind === 'finished') {
      const finished = snapshotAutomaticCheckpointFinish(result.reason)
      this.#automaticCheckpointFinished = true
      return finished
    }
    if (result.kind !== 'advanced') {
      throw new OutputCheckpointContractError('output checkpoint returned an unknown decision')
    }
    if (!this.#durable) {
      throw new OutputCheckpointContractError('transient output cannot advance durable recovery evidence')
    }
    const checkpoint = requireOutputBinding(result.durable, this.#file, this.#session, this.#ownership)
    const expected = this.#acceptedRanges()
    if (!sameRanges(checkpoint.asRangeSet(), expected)) {
      throw new OutputCheckpointContractError(
        'output checkpoint does not equal prior durability plus every accepted pending write',
      )
    }
    this.#durableRanges = checkpoint
    this.#pendingRanges = new ByteRangeSet(this.#file.exactSize, [])
    return Object.freeze({ kind: 'advanced', durable: checkpoint })
  }

  async commit(signal: AbortSignal): Promise<VerifiedFinalOutputFile> {
    signal.throwIfAborted()
    this.#requireOpen('commit')
    if (!coversWholeFile(this.#acceptedRanges())) {
      throw new OutputCheckpointContractError(
        'output transaction cannot commit without complete durable and pending coverage',
      )
    }
    this.#state = 'committing'
    try {
      const proof = requireFinalBinding(
        await this.#transaction.commit(signal),
        this.#file,
        this.#session,
        this.#ownership,
      )
      this.#state = 'committed'
      return proof
    } catch (error) {
      this.#state = 'failed'
      throw error
    }
  }

  async retire(reason: unknown): Promise<'FileIsolated' | 'JobOutputCompromised'> {
    this.#requireOpen('retire')
    this.#state = 'retiring'
    try {
      const disposition = await retireOutputFileTransaction(this.#transaction, reason)
      this.#state = 'retired'
      return disposition
    } catch (error) {
      this.#state = 'failed'
      throw error
    }
  }

  async pause(reason: unknown): Promise<VerifiedDurableRanges> {
    this.#requireOpen('pause')
    this.#state = 'pausing'
    const expected = this.#durable ? this.#acceptedRanges() : this.#durableRanges.asRangeSet()
    try {
      const durable = requireOutputBinding(
        await this.#transaction.pause(reason),
        this.#file,
        this.#session,
        this.#ownership,
      )
      if (!sameRanges(durable.asRangeSet(), expected)) {
        throw new OutputCheckpointContractError(
          'output pause did not return the exact forced durable cut',
        )
      }
      this.#durableRanges = durable
      this.#pendingRanges = new ByteRangeSet(this.#file.exactSize, [])
      this.#state = 'paused'
      return durable
    } catch (error) {
      this.#state = 'failed'
      throw error
    }
  }

  #acceptedRanges(): ByteRangeSet {
    return this.#durableRanges.asRangeSet().union(this.#pendingRanges)
  }

  #requireOpen(operation: string): void {
    if (this.#state !== 'open') {
      throw new OutputCheckpointContractError(
        `output transaction cannot ${operation} after terminal settlement began`,
      )
    }
  }
}

export class OutputTransactionContractError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'OutputTransactionContractError'
  }
}

export class OutputCheckpointContractError extends OutputTransactionContractError {
  constructor(message: string) {
    super(message)
    this.name = 'OutputCheckpointContractError'
  }
}

function sameRanges(left: ByteRangeSet, right: ByteRangeSet): boolean {
  return left.fileSize === right.fileSize && left.ranges.length === right.ranges.length &&
    left.ranges.every((range, index) => {
      const other = right.ranges[index]
      return other !== undefined && range.start === other.start && range.end === other.end
    })
}

function coversWholeFile(ranges: ByteRangeSet): boolean {
  if (ranges.fileSize === 0n) return ranges.empty
  return ranges.ranges.length === 1 && ranges.ranges[0]?.start === 0n &&
    ranges.ranges[0].end === ranges.fileSize
}

function requireCanonicalPrefix(ranges: ByteRangeSet): bigint {
  if (ranges.empty) return 0n
  const prefix = ranges.ranges[0]
  if (ranges.ranges.length !== 1 || prefix?.start !== 0n) {
    throw new OutputCheckpointContractError(
      'sequential durable output requires one canonical recovery prefix',
    )
  }
  return prefix.end
}

function snapshotAutomaticCheckpointDeferral(
  reason: unknown,
): Extract<AutomaticCheckpointResult, { readonly kind: 'deferred' }> {
  if (reason !== 'capacity-unavailable' && reason !== 'checkpoint-priority') {
    throw new OutputCheckpointContractError('output checkpoint returned an invalid deferral reason')
  }
  return Object.freeze({ kind: 'deferred', reason })
}

function snapshotAutomaticCheckpointFinish(
  reason: unknown,
): Extract<AutomaticCheckpointResult, { readonly kind: 'finished' }> {
  if (reason !== 'prefix-copy-budget' &&
      reason !== 'cumulative-write-amplification-budget' &&
      reason !== 'cost-evidence-unavailable') {
    throw new OutputCheckpointContractError('output checkpoint returned an invalid terminal reason')
  }
  return Object.freeze({ kind: 'finished', reason })
}

function overlapsAny(candidate: { readonly start: bigint; readonly end: bigint }, ranges: readonly {
  readonly start: bigint
  readonly end: bigint
}[]): boolean {
  return ranges.some((range) => candidate.start < range.end && candidate.end > range.start)
}

function requireOutputBinding(
  durableRanges: VerifiedDurableRanges,
  file: OutputFile,
  session: OutputSessionIdentity,
  ownership?: OutputFileOwnership,
): VerifiedDurableRanges {
  if (!(durableRanges instanceof VerifiedDurableRanges) ||
      !sameOutputSource(durableRanges.source, file.source) ||
      durableRanges.fileSize !== file.exactSize ||
      durableRanges.ownership.backend !== session.backend ||
      durableRanges.ownership.outputSessionId !== session.outputSessionId ||
      !samePath(durableRanges.ownership.canonicalPath, file.materializationRelativePath) ||
      (ownership !== undefined && !sameOutputOwnership(durableRanges.ownership, ownership))) {
    throw new OutputTransactionContractError(
      'output durable ranges belong to a different output or source revision',
    )
  }
  return durableRanges
}

function requireFinalBinding(
  proof: VerifiedFinalOutputFile,
  file: OutputFile,
  session: OutputSessionIdentity,
  ownership: OutputFileOwnership,
): VerifiedFinalOutputFile {
  if (!(proof instanceof VerifiedFinalOutputFile) ||
      !sameOutputSource(proof.source, file.source) ||
      proof.fileSize !== file.exactSize ||
      proof.ownership.backend !== session.backend ||
      proof.ownership.outputSessionId !== session.outputSessionId ||
      !samePath(proof.ownership.canonicalPath, file.materializationRelativePath) ||
      !sameOutputOwnership(proof.ownership, ownership)) {
    throw new OutputTransactionContractError(
      'final output proof belongs to a different output or source revision',
    )
  }
  return proof
}

function hasFunction(value: object, property: string): boolean {
  return typeof Reflect.get(value, property) === 'function'
}

function sameOutputSource(
  left: OutputSourceIdentity,
  right: OutputSourceIdentity,
): boolean {
  return left.shareInstance === right.shareInstance &&
    left.fileId === right.fileId &&
    left.fileRevision === right.fileRevision
}

function sameOutputOwnership(
  left: OutputFileOwnership,
  right: OutputFileOwnership,
): boolean {
  return left.backend === right.backend &&
    left.outputSessionId === right.outputSessionId &&
    left.ownedFileIdentity === right.ownedFileIdentity &&
    samePath(left.canonicalPath, right.canonicalPath)
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length &&
    left.every((segment, index) => segment === right[index])
}
