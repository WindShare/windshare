import {
  PERFORMANCE_CLAIM_PHASES_V1,
  type PerformanceClaimPhaseV1,
} from '../../diagnostics/trace/transfer-payload'
import {
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from './performance-summary'

export interface PerformanceClaimPhaseSample {
  readonly memberCount: number
  readonly queueMilliseconds: number
  readonly runMilliseconds: number
  readonly activeMilliseconds: bigint
  readonly overlapMilliseconds: bigint
  readonly maximumActive: number
  readonly activeAtCompletion: number
}

export type PerformanceClaimPhaseSamples = Readonly<Record<
  PerformanceClaimPhaseV1,
  PerformanceClaimPhaseSample
>>

export interface PerformanceClaimActiveObservation {
  finish(): void
}

export interface PerformanceClaimPhaseObservation {
  beginActive(): PerformanceClaimActiveObservation | undefined
  finish(): void
}

export interface PerformanceClaimBatchTimeline {
  beginPhase(
    phase: PerformanceClaimPhaseV1,
    memberCount: number,
  ): PerformanceClaimPhaseObservation | undefined
  complete(): PerformanceClaimBatchTimelineResult | undefined
}

export interface PerformanceClaimBatchTimelineResult {
  readonly completedAtMilliseconds: number
  readonly phases: PerformanceClaimPhaseSamples
}

interface ActivePhaseState {
  readonly phase: PerformanceClaimPhaseV1
  readonly memberCount: number
  readonly queuedMilliseconds: number
  readonly startedAtMilliseconds: number
  lastActiveAtMilliseconds: number
  active: number
  maximumActive: number
  activeMilliseconds: bigint
  overlapMilliseconds: bigint
  finished: boolean
}

export function createPerformanceClaimBatchTimeline(
  performance: PerformanceSummaryObservations | undefined,
  startedAtMilliseconds: number | undefined,
): PerformanceClaimBatchTimeline | undefined {
  if (performance === undefined || startedAtMilliseconds === undefined) return undefined
  return new ClaimBatchTimeline(performance, startedAtMilliseconds)
}

class ClaimBatchTimeline implements PerformanceClaimBatchTimeline {
  readonly #performance: PerformanceSummaryObservations
  readonly #samples = new Map<PerformanceClaimPhaseV1, PerformanceClaimPhaseSample>()
  #cursorMilliseconds: number
  #nextPhaseIndex = 0
  #current: ActivePhaseState | undefined
  #disabled = false

  constructor(performance: PerformanceSummaryObservations, startedAtMilliseconds: number) {
    this.#performance = performance
    this.#cursorMilliseconds = startedAtMilliseconds
  }

  beginPhase(
    phase: PerformanceClaimPhaseV1,
    memberCount: number,
  ): PerformanceClaimPhaseObservation | undefined {
    if (this.#disabled || this.#current !== undefined ||
        PERFORMANCE_CLAIM_PHASES_V1[this.#nextPhaseIndex] !== phase ||
        !Number.isSafeInteger(memberCount) || memberCount < 0) {
      this.#disabled = true
      return undefined
    }
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const queuedMilliseconds = performanceElapsedMilliseconds(
      this.#cursorMilliseconds,
      startedAtMilliseconds,
    )
    if (startedAtMilliseconds === undefined || queuedMilliseconds === undefined) {
      this.#disabled = true
      return undefined
    }
    const state: ActivePhaseState = {
      phase,
      memberCount,
      queuedMilliseconds,
      startedAtMilliseconds,
      lastActiveAtMilliseconds: startedAtMilliseconds,
      active: 0,
      maximumActive: 0,
      activeMilliseconds: 0n,
      overlapMilliseconds: 0n,
      finished: false,
    }
    this.#current = state
    return Object.freeze({
      beginActive: () => this.#beginActive(state),
      finish: () => this.#finishPhase(state),
    })
  }

  complete(): PerformanceClaimBatchTimelineResult | undefined {
    if (this.#current?.phase === 'installation') this.#finishPhase(this.#current)
    if (this.#disabled || this.#current !== undefined ||
        this.#nextPhaseIndex !== PERFORMANCE_CLAIM_PHASES_V1.length) return undefined
    return Object.freeze({
      completedAtMilliseconds: this.#cursorMilliseconds,
      phases: Object.freeze(Object.fromEntries(PERFORMANCE_CLAIM_PHASES_V1.map(phase => [
        phase,
        this.#samples.get(phase)!,
      ])) as PerformanceClaimPhaseSamples),
    })
  }

  #beginActive(state: ActivePhaseState): PerformanceClaimActiveObservation | undefined {
    if (this.#disabled || state.finished || this.#current !== state) return undefined
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    if (startedAtMilliseconds === undefined || !this.#advanceActive(state, startedAtMilliseconds)) {
      this.#disabled = true
      return undefined
    }
    state.active += 1
    state.maximumActive = Math.max(state.maximumActive, state.active)
    let finished = false
    return Object.freeze({
      finish: () => {
        if (finished || this.#disabled) return
        finished = true
        const completedAtMilliseconds = performanceNowMilliseconds(this.#performance)
        if (completedAtMilliseconds === undefined ||
            !this.#advanceActive(state, completedAtMilliseconds) || state.active === 0) {
          this.#disabled = true
          return
        }
        state.active -= 1
      },
    })
  }

  #finishPhase(state: ActivePhaseState): void {
    if (this.#disabled || state.finished || this.#current !== state) return
    const completedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    if (completedAtMilliseconds === undefined ||
        !this.#advanceActive(state, completedAtMilliseconds) || state.active !== 0) {
      this.#disabled = true
      return
    }
    const runMilliseconds = performanceElapsedMilliseconds(
      state.startedAtMilliseconds,
      completedAtMilliseconds,
    )
    if (runMilliseconds === undefined) {
      this.#disabled = true
      return
    }
    state.finished = true
    this.#samples.set(state.phase, Object.freeze({
      memberCount: state.memberCount,
      queueMilliseconds: state.queuedMilliseconds,
      runMilliseconds,
      activeMilliseconds: state.activeMilliseconds,
      overlapMilliseconds: state.overlapMilliseconds,
      maximumActive: state.maximumActive,
      activeAtCompletion: state.active,
    }))
    this.#cursorMilliseconds = completedAtMilliseconds
    this.#nextPhaseIndex += 1
    this.#current = undefined
  }

  #advanceActive(state: ActivePhaseState, atMilliseconds: number): boolean {
    const next = Math.max(state.lastActiveAtMilliseconds, atMilliseconds)
    const elapsed = BigInt(next - state.lastActiveAtMilliseconds)
    state.activeMilliseconds += BigInt(state.active) * elapsed
    if (state.active >= 2) state.overlapMilliseconds += elapsed
    state.lastActiveAtMilliseconds = next
    return Number.isSafeInteger(next) && next >= 0
  }
}
