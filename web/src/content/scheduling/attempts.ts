import type { ContentLane } from './selection'

// One immutable read has a finite physical-attempt budget, including rescue and
// replacement generations. Other blocks' progress never replenishes it.
export const MAXIMUM_BLOCK_ATTEMPTS = 16

export type BlockRetryDecision = Readonly<{
  firstDispatchSequence: number
  attempts: number
  maximumAttempts: number
}> & (
  | Readonly<{
      transition: 'retrying'
      dispatchSequence: number
      laneId: number
      laneEpoch: number
      route: ContentLane['route']
      successfulBlocks: number
    }> & (
      | Readonly<{ reason: 'untried_lane' }>
      | Readonly<{ reason: 'recovered_lane'; successfulBlocksAtFailure: number }>
    )
  | Readonly<{
      transition: 'exhausted'
      reason: 'attempt_limit' | 'no_recovered_lane'
    }>
)

type AttemptState =
  | { readonly kind: 'active' }
  | { readonly kind: 'failed'; readonly successfulBlocks: number }

/** Attempt exclusion expires only on authenticated delivery after that failure. */
export class BlockAttempts {
  readonly #lanes = new Map<ContentLane, AttemptState>()
  readonly #observe: ((decision: BlockRetryDecision) => void) | undefined
  #count = 0
  #firstDispatchSequence = 0

  constructor(observe?: (decision: BlockRetryDecision) => void) {
    this.#observe = observe
  }

  get exhausted(): boolean { return this.#count >= MAXIMUM_BLOCK_ATTEMPTS }

  allows(lane: ContentLane): boolean {
    if (this.exhausted) return false
    const previous = this.#lanes.get(lane)
    return previous === undefined || (previous.kind === 'failed' &&
      lane.performance.successfulBlocks > previous.successfulBlocks)
  }

  started(lane: ContentLane, dispatchSequence: number): void {
    if (!this.allows(lane)) throw new Error('Content attempt was admitted without retry authority')
    const previous = this.#lanes.get(lane)
    this.#lanes.set(lane, { kind: 'active' })
    this.#count++
    if (this.#count === 1) { this.#firstDispatchSequence = dispatchSequence; return }
    this.#emit({
      ...this.#context(), transition: 'retrying', dispatchSequence,
      laneId: lane.lane.id, laneEpoch: lane.laneEpoch, route: lane.route,
      successfulBlocks: lane.performance.successfulBlocks,
      ...(previous?.kind === 'failed'
        ? { reason: 'recovered_lane', successfulBlocksAtFailure: previous.successfulBlocks }
        : { reason: 'untried_lane' }),
    })
  }

  failed(lane: ContentLane): void {
    // Snapshot at failure, not dispatch: a concurrent success that predates the
    // failure cannot justify retrying the same stalled read.
    this.#lanes.set(lane, { kind: 'failed', successfulBlocks: lane.performance.successfulBlocks })
  }

  reportExhausted(): void {
    this.#emit({ ...this.#context(), transition: 'exhausted',
      reason: this.exhausted ? 'attempt_limit' : 'no_recovered_lane' })
  }

  #context() {
    return { firstDispatchSequence: this.#firstDispatchSequence, attempts: this.#count,
      maximumAttempts: MAXIMUM_BLOCK_ATTEMPTS }
  }

  #emit(decision: BlockRetryDecision): void {
    try { this.#observe?.(decision) } catch { /* Retry evidence cannot become transfer authority. */ }
  }
}
