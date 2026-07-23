import { ByteRangeSet, byteRange } from '../../content/geometry'
import type {
  FileAbortDisposition,
  OutputFile,
  OutputFileTransaction,
  VerifiedDurableRanges,
} from '../../transfer/output-session'
import type { PersistentTreeFile } from './contracts'
import { PersistentOutputError } from './errors'
import type { PersistedFileRecord } from '../persistence/journal'
import type { PersistentTreeOutputSession } from './session'

export class PersistentFileTransaction implements OutputFileTransaction {
  readonly #session: PersistentTreeOutputSession
  readonly #file: OutputFile
  readonly #handle: PersistentTreeFile
  #ranges: ByteRangeSet
  #generation: bigint
  #tail: Promise<unknown> = Promise.resolve()
  #settled = false

  constructor(
    session: PersistentTreeOutputSession,
    file: OutputFile,
    handle: PersistentTreeFile,
    record: PersistedFileRecord,
  ) {
    this.#session = session
    this.#file = file
    this.#handle = handle
    this.#ranges = new ByteRangeSet(file.exactSize, record.durableRanges)
    this.#generation = record.generation
  }

  writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      this.#requireActive()
      const end = offset + BigInt(snapshot.byteLength)
      if (offset < 0n || end > this.#file.exactSize) {
        throw new RangeError('Persistent output write exceeds its file')
      }
      await this.#handle.writeAt(offset, snapshot)
      this.#ranges = this.#ranges.union(new ByteRangeSet(
        this.#file.exactSize,
        [byteRange(offset, end)],
      ))
      await this.#session.noteDataWritten(this.#file)
    })
  }

  checkpoint(): Promise<VerifiedDurableRanges> {
    return this.#enqueue(async () => {
      this.#requireActive()
      return this.#session.checkpointFile(this, this.#file, this.#handle, this.#ranges)
    })
  }

  commit(): Promise<void> {
    return this.#enqueue(async () => {
      this.#requireActive()
      await this.#session.checkpointFile(this, this.#file, this.#handle, this.#ranges)
      await this.#session.commitFile(this, this.#file, this.#handle, this.#ranges)
      this.#settled = true
    })
  }

  abort(): Promise<FileAbortDisposition> {
    return this.#enqueue(async () => {
      if (this.#settled) return 'FileIsolated'
      const disposition = await this.#session.abortFile(
        this,
        this.#file,
        this.#handle,
      )
      this.#settled = true
      return disposition
    })
  }

  suspend(): Promise<void> {
    return this.#enqueue(async () => {
      if (this.#settled) return
      await this.#session.suspendFile(this, this.#file, this.#handle)
      this.#settled = true
    })
  }

  nextGeneration(): bigint {
    this.#generation += 1n
    return this.#generation
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
