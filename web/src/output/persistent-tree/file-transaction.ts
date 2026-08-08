import { ByteRangeSet, byteRange } from '../../content/geometry'
import type {
  FileRetirementDisposition,
  OutputFile,
  OutputFileTransaction,
  VerifiedDurableRanges,
} from '../../transfer/output-session'
import type { DirectoryFileMutationLease } from '../../transfer/directory-admission-ledger'
import type { PersistentTreeFile } from './contracts'
import { PersistentOutputError } from './errors'
import type { PersistedFileRecord } from '../persistence/journal'
import type { PersistentTreeOutputSession } from './session'

export class PersistentFileTransaction implements OutputFileTransaction {
  readonly #session: PersistentTreeOutputSession
  readonly #file: OutputFile
  readonly #handle: PersistentTreeFile
  readonly #directoryMutation: DirectoryFileMutationLease
  readonly #operation: PersistentFileOperation
  #ranges: ByteRangeSet
  #generation: bigint
  #tail: Promise<unknown> = Promise.resolve()
  #settled = false

  constructor(
    session: PersistentTreeOutputSession,
    file: OutputFile,
    handle: PersistentTreeFile,
    record: PersistedFileRecord,
    directoryMutation: DirectoryFileMutationLease,
  ) {
    this.#session = session
    this.#file = file
    this.#handle = handle
    this.#directoryMutation = directoryMutation
    this.#ranges = new ByteRangeSet(file.exactSize, record.durableRanges)
    this.#generation = record.generation
    const operation: PersistentFileOperation = Object.freeze({
      writeRange: (offset: bigint, data: Uint8Array, signal: AbortSignal) =>
        this.#writeRange(offset, data, signal),
      checkpoint: (signal: AbortSignal) => this.#checkpoint(signal),
      commitCheckpointed: (signal: AbortSignal) => this.#commitCheckpointed(signal),
      retire: (reason: unknown) => this.#retire(reason),
      pause: (reason: unknown) => this.#pause(reason),
      settle: () => this.#settleIfInactive(),
    })
    this.#operation = operation
  }

  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    const snapshot = data.slice()
    return this.runOperation((operation) => operation.writeRange(offset, snapshot, signal))
  }

  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    return this.runOperation((operation) => operation.checkpoint(signal))
  }

  commit(signal: AbortSignal): Promise<void> {
    return this.runOperation(async (operation) => {
      await operation.checkpoint(signal)
      await operation.commitCheckpointed(signal)
      if (!operation.settle()) {
        throw new Error('Committed persistent file remained active')
      }
    })
  }

  retire(reason: unknown): Promise<FileRetirementDisposition> {
    return this.runOperation(async (operation) => {
      try {
        return await operation.retire(reason)
      } finally {
        operation.settle()
      }
    })
  }

  pause(reason: unknown): Promise<void> {
    return this.runOperation(async (operation) => {
      try {
        await operation.pause(reason)
      } finally {
        operation.settle()
      }
    })
  }

  runOperation<T>(operation: (transaction: PersistentFileOperation) => Promise<T>): Promise<T> {
    return this.#session.runOperation(() => this.#enqueue(() => operation(this.#operation)))
  }

  pauseForClose(reason: unknown): Promise<void> {
    return this.#enqueue(async () => {
      try {
        await this.#pause(reason)
      } finally {
        this.#settleIfInactive()
      }
    })
  }

  nextGeneration(): bigint {
    this.#generation += 1n
    return this.#generation
  }

  async #writeRange(offset: bigint, snapshot: Uint8Array, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#requireActive()
    const end = offset + BigInt(snapshot.byteLength)
    if (offset < 0n || end > this.#file.exactSize) {
      throw new RangeError('Persistent output write exceeds its file')
    }
    await this.#handle.writeAt(offset, snapshot)
    signal.throwIfAborted()
    this.#ranges = this.#ranges.union(new ByteRangeSet(
      this.#file.exactSize,
      [byteRange(offset, end)],
    ))
    await this.#session.noteDataWritten(this.#file)
    signal.throwIfAborted()
  }

  async #checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    signal.throwIfAborted()
    this.#requireActive()
    return this.#session.checkpointFile(this, this.#file, this.#handle, this.#ranges, signal)
  }

  async #commitCheckpointed(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#requireActive()
    await this.#session.commitFile(this, this.#file, this.#handle, this.#ranges, signal)
  }

  async #retire(reason: unknown): Promise<FileRetirementDisposition> {
    if (this.#settled) return 'FileIsolated'
    return this.#session.retireFile(this, this.#file, this.#handle, reason)
  }

  async #pause(reason: unknown): Promise<void> {
    if (this.#settled) return
    await this.#session.pauseFile(this, this.#file, this.#handle, reason)
  }

  #settle(): void {
    if (this.#settled) return
    this.#settled = true
    this.#directoryMutation.release()
  }

  #settleIfInactive(): boolean {
    if (this.#settled) return true
    if (this.#session.isFileTransactionActive(this, this.#file)) return false
    this.#settle()
    return true
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation, operation)
    this.#tail = result
    return result
  }

  #requireActive(): void {
    if (this.#settled) {
      throw new PersistentOutputError('output-state', 'Output file transaction is settled')
    }
  }
}

export interface PersistentFileOperation {
  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void>
  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges>
  commitCheckpointed(signal: AbortSignal): Promise<void>
  retire(reason: unknown): Promise<FileRetirementDisposition>
  pause(reason: unknown): Promise<void>
  settle(): boolean
}
