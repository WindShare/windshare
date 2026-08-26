import type { FinalFileCheckpointProof } from '../../output/persistence/journal'
import type { PersistentFileTransactionPort } from '../../output/persistent-tree/contracts'
import { PersistentPreservingWriterOpenError } from '../../output/persistent-tree/recovery'
import {
  TransferPauseRequestedError,
  VerifiedDurableRanges,
  type AutomaticCheckpointResult,
  type AutomaticCheckpointTrigger,
  type FileRetirementDisposition,
  type OpenedOutputRevision,
  type OutputFileOwnership,
  type OutputFileTransaction,
  type VerifiedFinalOutputFile,
} from '../output-session'

export interface PersistentOutputTransactionNamespace {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
}

/** Operation-level pause with the exact preserving open that can be retried. */
export class PersistentWriterOpenPauseRequestedError extends TransferPauseRequestedError {
  readonly materializationRelativePath: readonly string[]
  readonly cost: PersistentPreservingWriterOpenError['cost']
  readonly purpose: PersistentPreservingWriterOpenError['purpose']

  constructor(cause: PersistentPreservingWriterOpenError) {
    super(
      `Persistent output preserving writer failed for ${cause.materializationRelativePath.join('/')}`,
      { cause },
    )
    this.name = 'PersistentWriterOpenPauseRequestedError'
    this.materializationRelativePath = Object.freeze([...cause.materializationRelativePath])
    this.cost = cause.cost
    this.purpose = cause.purpose
  }
}

export class PersistentOutputTransaction implements OutputFileTransaction {
  readonly #transaction: PersistentFileTransactionPort
  readonly #revision: OpenedOutputRevision
  readonly #ownership: OutputFileOwnership
  readonly #namespace: PersistentOutputTransactionNamespace
  readonly #isolated: boolean
  readonly #releaseMutation: () => void
  readonly #recordProof: (proof: FinalFileCheckpointProof) => void
  #settled = false

  constructor(input: Readonly<{
    transaction: PersistentFileTransactionPort
    revision: OpenedOutputRevision
    ownership: OutputFileOwnership
    checkpointNamespace: PersistentOutputTransactionNamespace
    isolated: boolean
    releaseMutation(): void
    recordProof(proof: FinalFileCheckpointProof): void
  }>) {
    this.#transaction = input.transaction
    this.#revision = input.revision
    this.#ownership = input.ownership
    this.#namespace = input.checkpointNamespace
    this.#isolated = input.isolated
    this.#releaseMutation = input.releaseMutation
    this.#recordProof = input.recordProof
  }

  async writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    try {
      await this.#transaction.writeRange(offset, data, signal)
    } catch (cause) {
      if (cause instanceof PersistentPreservingWriterOpenError) throw resumableWriterOpenPause(cause)
      throw cause
    }
  }

  async automaticCheckpoint(
    trigger: AutomaticCheckpointTrigger,
    signal: AbortSignal,
  ): Promise<AutomaticCheckpointResult> {
    signal.throwIfAborted()
    if (this.#settled) throw new Error('persistent output transaction is already settled')
    let result: Awaited<ReturnType<PersistentFileTransactionPort['automaticCheckpoint']>>
    try {
      result = await this.#transaction.automaticCheckpoint(trigger, signal)
    } catch (cause) {
      if (cause instanceof PersistentPreservingWriterOpenError) throw resumableWriterOpenPause(cause)
      throw cause
    }
    if (result.kind === 'deferred') {
      return Object.freeze({ kind: 'deferred' as const, reason: result.reason })
    }
    if (result.kind === 'finished') {
      return Object.freeze({ kind: 'finished' as const, reason: result.reason })
    }
    return Object.freeze({
      kind: 'advanced' as const,
      durable: verifiedPersistentRanges(
        this.#ownership,
        this.#revision,
        result.durableRanges,
      ),
    })
  }

  async commit(signal: AbortSignal): Promise<VerifiedFinalOutputFile> {
    if (this.#settled) throw new Error('persistent output transaction is already settled')
    const commit = await this.#transaction.commit(signal)
    requireMatchingFinalProof(
      this.#revision,
      this.#ownership,
      this.#namespace,
      commit.checkpointProof,
    )
    this.#recordProof(commit.checkpointProof)
    this.#settle()
    return commit.finalOutput
  }

  async retire(reason: unknown): Promise<FileRetirementDisposition> {
    if (!this.#settled) {
      await this.#transaction.retire(reason)
      this.#settle()
    }
    return this.#isolated ? 'FileIsolated' : 'JobOutputCompromised'
  }

  async pause(reason: unknown): Promise<VerifiedDurableRanges> {
    if (this.#settled) throw new Error('persistent output transaction is already settled')
    const durable = verifiedPersistentRanges(
      this.#ownership,
      this.#revision,
      await this.#transaction.pause(reason),
    )
    this.#settle()
    return durable
  }

  #settle(): void {
    if (this.#settled) return
    this.#settled = true
    this.#releaseMutation()
  }
}

function resumableWriterOpenPause(
  cause: PersistentPreservingWriterOpenError,
): PersistentWriterOpenPauseRequestedError {
  return new PersistentWriterOpenPauseRequestedError(cause)
}

export function verifiedPersistentRanges(
  ownership: OutputFileOwnership,
  revision: OpenedOutputRevision,
  ranges: readonly Readonly<{ start: bigint; end: bigint }>[],
): VerifiedDurableRanges {
  return new VerifiedDurableRanges(ownership, revision, revision.exactSize, ranges)
}

function requireMatchingFinalProof(
  revision: OpenedOutputRevision,
  ownership: OutputFileOwnership,
  namespace: PersistentOutputTransactionNamespace,
  proof: FinalFileCheckpointProof,
): void {
  if (proof.operationId !== namespace.operationId ||
      proof.receiveIntentDigest !== namespace.receiveIntentDigest ||
      proof.materializationBindingDigest !== namespace.materializationBindingDigest ||
      proof.fileId !== revision.fileId || proof.fileRevision !== revision.fileRevision ||
      proof.exactSize !== revision.exactSize || proof.ownedObjectId !== ownership.ownedFileIdentity ||
      !samePath(proof.canonicalPath, ownership.canonicalPath) || proof.complete !== true) {
    throw new TypeError('final checkpoint proof escaped its output transaction')
  }
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}
