import { ByteRangeSet, byteRange } from '../content/geometry'
import { VerifiedDurableRanges } from './output-session'
import type {
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
  readonly durableRanges: VerifiedDurableRanges
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
    const durableRanges = requireOutputBinding(begun.durableRanges, file, session.identity)
    const resumable = session.capabilities.durability !== 'None' && session.capabilities.randomWrite
    if (!resumable && !durableRanges.asRangeSet().empty) {
      throw new OutputCheckpointContractError('transient output cannot claim durable resumable ranges')
    }
    return Object.freeze({
      transaction: new SourceBoundOutputTransaction(
        transaction,
        file,
        session.identity,
        durableRanges.ownership,
        durableRanges,
        resumable,
      ),
      durableRanges,
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
        !hasFunction(candidate, 'writeRange') || !hasFunction(candidate, 'checkpoint') ||
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
  readonly #resumable: boolean
  #durableRanges: VerifiedDurableRanges
  #pendingRanges: ByteRangeSet
  #streamOffset = 0n

  constructor(
    transaction: OutputFileTransaction,
    file: OutputFile,
    session: OutputSessionIdentity,
    ownership: OutputFileOwnership,
    durableRanges: VerifiedDurableRanges,
    resumable: boolean,
  ) {
    this.#transaction = transaction
    this.#file = file
    this.#session = session
    this.#ownership = ownership
    this.#resumable = resumable
    this.#durableRanges = durableRanges
    this.#pendingRanges = new ByteRangeSet(file.exactSize, [])
  }

  async writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    const end = offset + BigInt(data.byteLength)
    if (data.byteLength === 0 || offset < 0n || end > this.#file.exactSize) {
      throw new RangeError('output write range exceeds its file transaction')
    }
    const written = byteRange(offset, end)
    if (this.#resumable) {
      if (overlapsAny(written, this.#durableRanges.ranges) || overlapsAny(written, this.#pendingRanges.ranges)) {
        throw new OutputCheckpointContractError('output write overlaps bytes already supplied or durable')
      }
    } else if (offset !== this.#streamOffset) {
      throw new OutputCheckpointContractError('transient output writes must be contiguous and sequential')
    }
    await this.#transaction.writeRange(offset, data, signal)
    signal.throwIfAborted()
    if (this.#resumable) {
      this.#pendingRanges = this.#pendingRanges.union(new ByteRangeSet(this.#file.exactSize, [written]))
    } else {
      this.#streamOffset = end
    }
  }

  async checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    signal.throwIfAborted()
    const checkpoint = requireOutputBinding(
      await this.#transaction.checkpoint(signal),
      this.#file,
      this.#session,
      this.#ownership,
    )
    const next = checkpoint.asRangeSet()
    const expected = this.#resumable
      ? this.#durableRanges.asRangeSet().union(this.#pendingRanges)
      : this.#durableRanges.asRangeSet()
    if (!sameRanges(next, expected)) {
      throw new OutputCheckpointContractError(
        'output checkpoint does not equal prior durability plus every completed write',
      )
    }
    this.#durableRanges = checkpoint
    if (this.#resumable) this.#pendingRanges = new ByteRangeSet(this.#file.exactSize, [])
    return checkpoint
  }

  async commit(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    const complete = this.#resumable
      ? this.#pendingRanges.empty && this.#durableRanges.covers(byteRange(0n, this.#file.exactSize))
      : this.#streamOffset === this.#file.exactSize
    if (!complete) {
      throw new OutputCheckpointContractError(
        'output transaction cannot commit without verified whole-file durability',
      )
    }
    return this.#transaction.commit(signal)
  }

  async retire(reason: unknown): Promise<'FileIsolated' | 'JobOutputCompromised'> {
    return retireOutputFileTransaction(this.#transaction, reason)
  }

  pause(reason: unknown): Promise<void> {
    return this.#transaction.pause(reason)
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
      !samePath(durableRanges.ownership.canonicalPath, file.path) ||
      (ownership !== undefined && !sameOutputOwnership(durableRanges.ownership, ownership))) {
    throw new OutputTransactionContractError(
      'output durable ranges belong to a different output or source revision',
    )
  }
  return durableRanges
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
