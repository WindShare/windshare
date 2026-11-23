import { ByteRangeSet, byteRange, coveredBytes } from '../content/geometry'
import type { CheckpointOutcome, PeriodicCheckpointPolicy, TransferOutputFile } from './output-port'

export interface DisconnectFileCheckpoint {
  readonly outcome: CheckpointOutcome
  readonly savedBytes: bigint
}

/** The ranges and the writer have one owner so a durable cut cannot overtake a write or its accounting. */
export class ActiveFileWrite {
  readonly #writer: TransferOutputFile
  readonly #periodic: PeriodicCheckpointPolicy | undefined
  readonly #failed = new AbortController()
  readonly signal: AbortSignal
  #tail: Promise<unknown> = Promise.resolve()
  #ranges: ByteRangeSet
  #coverage: bigint
  #dirtyBytes = 0n
  #lastCheckpointAt = Date.now()
  #writing = false
  #finishing = false

  constructor(writer: TransferOutputFile, size: bigint, taskSignal: AbortSignal, periodic?: PeriodicCheckpointPolicy) {
    this.#writer = writer
    this.#periodic = periodic
    this.#ranges = new ByteRangeSet(size, writer.completedRanges)
    this.#coverage = coveredBytes(this.#ranges.ranges)
    this.signal = AbortSignal.any([taskSignal, this.#failed.signal])
  }

  get ranges(): ByteRangeSet { return this.#ranges }
  get coverage(): bigint { return this.#coverage }
  get durable(): boolean { return this.#dirtyBytes === 0n && !this.#writing && !this.#finishing && !this.#failed.signal.aborted }
  get failure(): unknown { return this.#failed.signal.reason }

  async write(offset: bigint, data: Uint8Array, onWritten?: (bytes: bigint) => void): Promise<void> {
    this.#writing = true
    try {
      await this.#serialize(async () => {
        this.signal.throwIfAborted()
        const size = this.#ranges.fileSize
        const span = byteRange(offset, offset + BigInt(data.byteLength))
        const next = this.#ranges.union(new ByteRangeSet(size, [span]))
        await this.#writer.writeAt(offset, data)
        this.#ranges = next
        const coverage = coveredBytes(next.ranges)
        const gained = coverage - this.#coverage
        this.#coverage = coverage
        this.#dirtyBytes += gained
        onWritten?.(gained)
        const policy = this.#periodic
        if (policy !== undefined && this.#dirtyBytes > 0n && !this.signal.aborted &&
            (this.#dirtyBytes >= policy.intervalBytes || Date.now() - this.#lastCheckpointAt >= policy.intervalMilliseconds)) {
          await this.#checkpoint('periodic')
        }
      })
    } finally {
      this.#writing = false
    }
  }

  async checkpointOnDisconnect(signal: AbortSignal): Promise<DisconnectFileCheckpoint | undefined> {
    if (this.durable || signal.aborted) return undefined
    if (this.#finishing) return { outcome: 'skipped', savedBytes: 0n }
    return this.#serialize(async () => {
      if (signal.aborted || this.signal.aborted) return undefined
      if (this.#finishing) return { outcome: 'skipped', savedBytes: 0n }
      if (this.#dirtyBytes === 0n) return undefined
      return this.#checkpoint('disconnect')
    })
  }

  commit(): Promise<void> {
    return this.finish(async () => {
      this.signal.throwIfAborted()
      await this.#writer.commit(this.#ranges.ranges)
      this.#dirtyBytes = 0n
      this.#finishing = false
    })
  }

  finish<T>(operation: () => Promise<T>): Promise<T> {
    this.#finishing = true
    return this.#serialize(operation)
  }

  async #checkpoint(reason: 'periodic' | 'disconnect'): Promise<DisconnectFileCheckpoint> {
    const bytes = this.#dirtyBytes
    const outcome = await this.#writer.checkpoint(this.#ranges.ranges, reason)
    if (outcome === 'skipped') return { outcome, savedBytes: 0n }
    this.#dirtyBytes = 0n
    this.#lastCheckpointAt = Date.now()
    return { outcome, savedBytes: bytes }
  }

  #serialize<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation)
    this.#tail = result.catch((cause: unknown) => {
      // Wake a blocked reader without losing the storage error if the broker translates cancellation.
      this.#failed.abort(cause)
    })
    return result
  }
}
