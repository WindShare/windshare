import { V2SessionRuntimeError } from '../../session/v2-runtime-types'
import { V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS } from '../v2-flow'
import { linkAbortSignals } from './deadlines'

export type BlockReceivePhase = 'awaiting_first_fragment' | 'receiving_fragments'

export class V2BlockInactivityTimeoutError extends V2SessionRuntimeError {
  readonly phase: BlockReceivePhase
  readonly waitedMilliseconds: number
  readonly queueProgress: number

  constructor(phase: BlockReceivePhase, waitedMilliseconds: number, queueProgress: number, options?: ErrorOptions) {
    super('lane', `Block receive made no authenticated progress (${phase})`, options)
    this.name = 'V2BlockInactivityTimeoutError'
    this.phase = phase
    this.waitedMilliseconds = waitedMilliseconds
    this.queueProgress = queueProgress
  }
}

/** One content lane's finite, ordered set of outstanding response waits. */
export class BlockResponseQueue {
  readonly #pending = new Set<BlockResponseWait>()

  begin(signal: AbortSignal): BlockResponseWait {
    const wait = new BlockResponseWait(this.#pending, signal)
    this.#pending.add(wait)
    return wait
  }
}

export class BlockResponseWait {
  readonly #pending: Set<BlockResponseWait>
  readonly #timeout = new AbortController()
  readonly #linked: ReturnType<typeof linkAbortSignals>
  readonly #started = performance.now()
  #deadline = this.#started + V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS
  #phase: BlockReceivePhase = 'awaiting_first_fragment'
  #queueProgress = 0
  #reading = true
  #timer: ReturnType<typeof globalThis.setTimeout>

  constructor(pending: Set<BlockResponseWait>, signal: AbortSignal) {
    this.#pending = pending
    this.#linked = linkAbortSignals(signal, this.#timeout.signal)
    this.#timer = globalThis.setTimeout(() => this.#checkDeadline(), V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS)
  }

  get signal(): AbortSignal { return this.#linked.signal }

  // Only accepted, unique fragments earn time. Later admissions cannot keep an
  // older request alive, and another block cannot conceal a stalled assembly.
  progress(): void {
    if (!this.#pending.has(this) || this.signal.aborted) return
    const now = performance.now()
    this.#phase = 'receiving_fragments'
    this.#deadline = now + V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS
    let preceding = true
    for (const wait of this.#pending) {
      if (wait === this) preceding = false
      else if (!preceding && wait.#phase === 'awaiting_first_fragment' &&
        !wait.signal.aborted && now < wait.#deadline) {
        wait.#deadline = now + V2_FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS
        wait.#queueProgress += 1
      }
    }
  }

  // Local assembly/digest verification is not a stalled network read.
  suspend(): void {
    this.#reading = false
    globalThis.clearTimeout(this.#timer)
  }

  resume(): void {
    if (this.#pending.has(this) && !this.#reading && !this.signal.aborted) {
      this.#reading = true
      this.#timer = globalThis.setTimeout(() => this.#checkDeadline(), Math.max(0, Math.ceil(this.#deadline - performance.now())))
    }
  }

  #checkDeadline(): void {
    if (!this.#pending.has(this) || !this.#reading || this.signal.aborted) return
    const now = performance.now()
    const remaining = this.#deadline - now
    if (remaining > 0) {
      // Queue progress extends the deadline without waking the reader.
      this.#timer = globalThis.setTimeout(() => this.#checkDeadline(), Math.ceil(remaining))
      return
    }
    this.#timeout.abort(this.inactivityError())
  }

  inactivityError(options?: ErrorOptions): V2BlockInactivityTimeoutError {
    return new V2BlockInactivityTimeoutError(this.#phase, performance.now() - this.#started, this.#queueProgress, options)
  }

  close(): void {
    globalThis.clearTimeout(this.#timer)
    this.#pending.delete(this)
    this.#linked.close()
  }
}
