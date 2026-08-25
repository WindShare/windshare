import type { PerformanceFilePipelineStageV1 } from '../../diagnostics/trace/transfer-payload'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from './performance-summary'

export interface PerformanceFilePipelineObservation {
  transition(stage: PerformanceFilePipelineStageV1): void
  close(): void
}

export interface PerformanceRevisionOpenObservation {
  finish(succeeded: boolean): void
}

export function createPerformanceFilePipelineObservation(
  observations: PerformanceSummaryObservations | undefined,
): PerformanceFilePipelineObservation | undefined {
  const startedAtMilliseconds = performanceNowMilliseconds(observations)
  if (observations === undefined || startedAtMilliseconds === undefined) return undefined
  let stage: PerformanceFilePipelineStageV1 | undefined = 'idle_no_ready_file'
  observePerformance(observations, summary => summary.observeFilePipelineTransition({
    to: 'idle_no_ready_file',
    atMilliseconds: startedAtMilliseconds,
  }))
  return Object.freeze({
    transition: (next: PerformanceFilePipelineStageV1) => {
      if (stage === undefined || stage === next) return
      const atMilliseconds = performanceNowMilliseconds(observations)
      if (atMilliseconds === undefined) return
      const previous = stage
      observePerformance(observations, summary => summary.observeFilePipelineTransition({
        from: previous,
        to: next,
        atMilliseconds,
      }))
      stage = next
    },
    close: () => {
      if (stage === undefined) return
      const atMilliseconds = performanceNowMilliseconds(observations)
      if (atMilliseconds === undefined) return
      const previous = stage
      observePerformance(observations, summary => summary.observeFilePipelineTransition({
        from: previous,
        atMilliseconds,
      }))
      stage = undefined
    },
  })
}

export function beginPerformanceRevisionOpen(
  observations: PerformanceSummaryObservations | undefined,
  waitMilliseconds: number | undefined,
): PerformanceRevisionOpenObservation | undefined {
  const startedAtMilliseconds = performanceNowMilliseconds(observations)
  if (observations === undefined || waitMilliseconds === undefined ||
      startedAtMilliseconds === undefined) return undefined
  observePerformance(observations, summary =>
    summary.observeRevisionOpenStarted(startedAtMilliseconds))
  let finished = false
  return Object.freeze({
    finish: (succeeded: boolean) => {
      if (finished) return
      finished = true
      const completedAtMilliseconds = performanceNowMilliseconds(observations)
      const runMilliseconds = performanceElapsedMilliseconds(
        startedAtMilliseconds,
        completedAtMilliseconds,
      )
      if (completedAtMilliseconds === undefined || runMilliseconds === undefined) return
      observePerformance(observations, summary => summary.observeRevisionOpenFinished({
        atMilliseconds: completedAtMilliseconds,
        waitMilliseconds,
        runMilliseconds,
        succeeded,
      }))
    },
  })
}
