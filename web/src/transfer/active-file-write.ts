import { ByteRangeSet, byteRange, coveredBytes } from '../content/geometry'
import type { CheckpointOutcome, CheckpointReason, PeriodicCheckpointPolicy, TransferOutputFile } from './output-port'

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
        try {
          await this.#writer.writeAt(offset, data)
        } catch (error) {
          this.#rejectWrite(error)
        }
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

  /** Let any admitted cut finish before deciding whether this writer can still preserve progress. */
  finishWrites(): Promise<unknown> {
    this.#finishing = true
    return this.#tail
  }

  async commit(): Promise<void> {
    await this.finishWrites()
    this.signal.throwIfAborted()
    try {
      await this.#writer.commit(this.#ranges.ranges)
    } catch (error) {
      this.#rejectWrite(error)
    }
    this.#dirtyBytes = 0n
    this.#finishing = false
  }

  async checkpointForRetry(): Promise<void> {
    if (this.#dirtyBytes > 0n) {
      if ((await this.#checkpoint('retry')).outcome !== 'saved') throw new Error('Cannot reopen without a durable file checkpoint')
    } else if (this.#ranges.empty) {
      await this.#writer.abort().catch(() => undefined)
    }
    // Lease retirement can outlive local I/O. The next reader opens against this durable prefix.
    this.#finishing = false
  }

  async pause(reason: unknown): Promise<void> {
    if (this.#dirtyBytes === 0n) {
      // A cancelled reopen can own an unused stream or empty placeholder. Its prior ranges are already durable.
      await this.#writer.abort(reason).catch(() => undefined)
    } else {
      await this.#writer.pause(this.#ranges.ranges)
      this.#dirtyBytes = 0n
    }
    this.#finishing = false
  }

  async #checkpoint(reason: CheckpointReason): Promise<DisconnectFileCheckpoint> {
    const bytes = this.#dirtyBytes
    let outcome: CheckpointOutcome
    try {
      outcome = await this.#writer.checkpoint(this.#ranges.ranges, reason)
    } catch (error) {
      this.#rejectWrite(error)
    }
    if (outcome === 'skipped') return { outcome, savedBytes: 0n }
    this.#dirtyBytes = 0n
    this.#lastCheckpointAt = Date.now()
    return { outcome, savedBytes: bytes }
  }

  #rejectWrite(error: unknown): never {
    // A broker can translate its cancellation, so keep the original I/O failure that woke it.
    if (!this.signal.aborted || error !== this.signal.reason) this.#failed.abort(error)
    throw error
  }

  #serialize<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation)
    this.#tail = result.catch(() => undefined)
    return result
  }
}
