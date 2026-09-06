import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type {
  PersistentAutomaticCheckpointResult,
  PersistentByteRange,
} from './contracts'
import {
  checkpointPerformanceCost,
  durableByteAdvance,
  rangeBytes,
} from './file-transaction-calculations'

export class PersistentCheckpointObservation {
  readonly #performance: OutputDiagnosticsPorts['performance']
  #observedDurableBytes: bigint

  constructor(
    performance: OutputDiagnosticsPorts['performance'],
    initialDurableRanges: readonly PersistentByteRange[],
  ) {
    this.#performance = performance
    this.#observedDurableBytes = rangeBytes(initialDurableRanges)
  }

  async observeAutomatic(
    operation: () => Promise<PersistentAutomaticCheckpointResult>,
  ): Promise<PersistentAutomaticCheckpointResult> {
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const result = await operation()
    const durableBytes = result.kind === 'advanced'
      ? rangeBytes(result.durableRanges)
      : this.#observedDurableBytes
    const elapsedMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      performanceNowMilliseconds(this.#performance),
    )
    if (elapsedMilliseconds !== undefined) {
      const cost = result.kind === 'advanced' ? result.cost : result.estimate
      observePerformance(this.#performance, summary => {
        summary.observeCheckpoint({
          trigger: 'automatic',
          decision: result.kind === 'advanced' ? 'advanced' : 'declined',
          cost: checkpointPerformanceCost(cost),
          elapsedMilliseconds,
          estimatedCopyBytes: cost.prefixCopyBytes,
        })
        if (result.kind === 'advanced') {
          summary.observeByteTransition(
            'durable',
            durableByteAdvance(this.#observedDurableBytes, durableBytes),
          )
        }
      })
    }
    this.#observedDurableBytes = durableBytes
    return result
  }

  async observeForcedPause(
    operation: () => Promise<readonly PersistentByteRange[]>,
  ): Promise<readonly PersistentByteRange[]> {
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const durable = await operation()
    const durableBytes = rangeBytes(durable)
    const elapsedMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      performanceNowMilliseconds(this.#performance),
    )
    if (elapsedMilliseconds !== undefined) {
      observePerformance(this.#performance, summary => {
        summary.observeCheckpoint({
          trigger: 'forced_pause',
          decision: 'advanced',
          cost: 'constant',
          elapsedMilliseconds,
        })
        summary.observeByteTransition(
          'durable',
          durableByteAdvance(this.#observedDurableBytes, durableBytes),
        )
      })
    }
    this.#observedDurableBytes = durableBytes
    return durable
  }

  observeFinal(durableRanges: readonly PersistentByteRange[], exactSize: bigint): void {
    const durableBytes = rangeBytes(durableRanges)
    observePerformance(this.#performance, summary => {
      summary.observeByteTransition(
        'durable',
        durableByteAdvance(this.#observedDurableBytes, durableBytes),
      )
      summary.observeByteTransition('final', exactSize)
    })
    this.#observedDurableBytes = durableBytes
  }
}
