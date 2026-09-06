import { OutputCapacityBlockedError } from '../../transfer/capacity-pressure/drain'

export interface ZipCapacityEntry {
  blocked(cause: unknown): OutputCapacityBlockedError
  finish(): void
}

/** Bounds pressure recovery to the existing file window; no new revisions or payload retries enter it. */
export class ZipCapacityWindow {
  readonly #active = new Map<string, object>()
  readonly #maximumEntries: number
  readonly #trace: ((stage: string, activeEntries: number) => void) | undefined
  #pressure: { cause: unknown; drained: Promise<void>; resolve(): void } | undefined

  constructor(maximumEntries: number, trace?: (stage: string, activeEntries: number) => void) {
    if (!Number.isSafeInteger(maximumEntries) || maximumEntries < 1) {
      throw new RangeError('ZIP active entry window must be a positive integer')
    }
    this.#maximumEntries = maximumEntries
    this.#trace = trace
  }

  enter(entryId: string): ZipCapacityEntry {
    if (this.#pressure !== undefined) throw this.#blockedError()
    if (this.#active.has(entryId)) throw new Error('ZIP entry already has an active transaction')
    if (this.#active.size >= this.#maximumEntries) throw new RangeError('ZIP active entry window exceeded')
    const token = {}
    this.#active.set(entryId, token)
    return {
      blocked: cause => {
        if (this.#active.get(entryId) !== token) throw new Error('ZIP capacity entry is closed')
        if (this.#pressure === undefined) {
          let resolve!: () => void
          const drained = new Promise<void>(done => { resolve = done })
          this.#pressure = { cause, drained, resolve }
          this.#emit('capacity-pressure')
        }
        return this.#blockedError()
      },
      finish: () => {
        if (this.#active.get(entryId) !== token) return
        this.#active.delete(entryId)
        if (this.#active.size === 0 && this.#pressure !== undefined) {
          this.#emit('capacity-drained')
          this.#pressure.resolve()
        }
      },
    }
  }

  async beforeDirectory(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (this.#pressure === undefined) return
    const blocked = this.#blockedError()
    await blocked.waitForDrain(signal)
    throw blocked
  }

  #blockedError(): OutputCapacityBlockedError {
    const pressure = this.#pressure!
    return new OutputCapacityBlockedError(pressure.cause, pressure.drained)
  }

  #emit(stage: string): void {
    try { this.#trace?.(stage, this.#active.size) } catch { /* Observation cannot delay checkpoint settlement. */ }
  }
}
