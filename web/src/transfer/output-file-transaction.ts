import type {
  BeginOutputFileResult,
  OutputFile,
  OutputFileOwnership,
  OutputFileTransaction,
  OutputSessionIdentity,
  OutputSourceIdentity,
  VerifiedDurableRanges,
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
  session: OutputSessionIdentity,
): BoundOutputFileTransaction {
  const durableRanges = requireOutputBinding(begun.durableRanges, file, session)
  return Object.freeze({
    transaction: new SourceBoundOutputTransaction(
      begun.transaction,
      file,
      session,
      durableRanges.ownership,
    ),
    durableRanges,
  })
}

class SourceBoundOutputTransaction implements OutputFileTransaction {
  readonly #transaction: OutputFileTransaction
  readonly #file: OutputFile
  readonly #session: OutputSessionIdentity
  readonly #ownership: OutputFileOwnership

  constructor(
    transaction: OutputFileTransaction,
    file: OutputFile,
    session: OutputSessionIdentity,
    ownership: OutputFileOwnership,
  ) {
    this.#transaction = transaction
    this.#file = file
    this.#session = session
    this.#ownership = ownership
  }

  writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    const end = offset + BigInt(data.byteLength)
    if (offset < 0n || end > this.#file.exactSize) {
      throw new RangeError('output write range exceeds its file transaction')
    }
    return this.#transaction.writeRange(offset, data)
  }

  async checkpoint(): Promise<VerifiedDurableRanges> {
    return requireOutputBinding(
      await this.#transaction.checkpoint(),
      this.#file,
      this.#session,
      this.#ownership,
    )
  }

  commit(): Promise<void> {
    return this.#transaction.commit()
  }

  abort(reason: unknown): Promise<'FileIsolated' | 'JobOutputCompromised'> {
    return this.#transaction.abort(reason)
  }
}

function requireOutputBinding(
  durableRanges: VerifiedDurableRanges,
  file: OutputFile,
  session: OutputSessionIdentity,
  ownership?: OutputFileOwnership,
): VerifiedDurableRanges {
  if (!sameOutputSource(durableRanges.source, file.source) ||
      durableRanges.fileSize !== file.exactSize ||
      durableRanges.ownership.backend !== session.backend ||
      durableRanges.ownership.outputSessionId !== session.outputSessionId ||
      !samePath(durableRanges.ownership.canonicalPath, file.path) ||
      (ownership !== undefined && !sameOutputOwnership(durableRanges.ownership, ownership))) {
    throw new Error('output durable ranges belong to a different output or source revision')
  }
  return durableRanges
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
