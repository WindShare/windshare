export const THROUGHPUT_SAMPLE_MS = 1000
const OBSERVATION_MS = 3000
const SETTLING_MS = 1000
const COOLDOWN_MS = 15_000
const IDLE_RETENTION_MS = 10_000
const MINIMUM_USEFUL_GAIN = 1.10
const MINIMUM_OUTPUT_GAIN = 1.05
const MINIMUM_OUTPUT_RATIO = 0.65

export interface ThroughputObservation {
  readonly inFlightBytes: number
  readonly queuedBytes: number
  readonly networkWaitingBytes: number
  readonly unfinishedBytes: number
  readonly capacityWaitingBytes: number
  readonly usefulBytes: number
  readonly outputBytes: number
  readonly remainingBytes: number
  readonly remainingKnown: boolean
  readonly paths: number
  readonly optionalPaths: number
  readonly pending: boolean
}
export interface ThroughputDecision {
  readonly action: 'hold' | 'explore' | 'retire' | 'keep'
  readonly reason?: string
  readonly usefulRate?: number
  readonly outputRate?: number
  readonly baselineUsefulRate?: number
  readonly baselineOutputRate?: number
}
interface Window { at: number; useful: number; output: number }
interface Rates { usefulRate: number; outputRate: number }
interface Trial extends Rates { paths: number; admittedAt?: number }
const HOLD = { action: 'hold' } as const

/** Uses unique authenticated winners and consumed output, never added-path traffic alone. */
export class ThroughputController {
  #window: Window | undefined
  #trial: Trial | undefined
  #eligibleSince: number | undefined
  #idleSince: number | undefined
  #notBefore = -Infinity
  #lastPaths = 0

  step(now: number, o: ThroughputObservation, maximumPaths: number): ThroughputDecision {
    // Equal progress rates can still reflect a saturated consumer window.
    const windowHasMoreWork = o.queuedBytes === 0 && o.inFlightBytes > 0 && o.remainingBytes > o.inFlightBytes
    const busy = (o.networkWaitingBytes > 0 || windowHasMoreWork) && o.unfinishedBytes > 0 && o.paths > 0
    if (this.#observeDemand(now, o, busy)) return { action: 'retire', reason: 'no-network-demand' }
    if (this.#window === undefined || o.usefulBytes < this.#window.useful || o.outputBytes < this.#window.output) {
      this.#reset(now, o)
      return HOLD
    }
    if (!this.#readyToMeasure(now, o)) return HOLD
    const elapsed = now - this.#window.at
    if (elapsed < OBSERVATION_MS) return HOLD
    const rates = {
      usefulRate: (o.usefulBytes - this.#window.useful) * 1000 / elapsed,
      outputRate: (o.outputBytes - this.#window.output) * 1000 / elapsed,
    }
    this.#reset(now, o)
    if (this.#trial !== undefined) return this.#finishTrial(now, rates)
    return this.#considerGrowth(now, o, maximumPaths, busy, rates)
  }

  #reset(now: number, o: ThroughputObservation): void {
    this.#window = { at: now, useful: o.usefulBytes, output: o.outputBytes }
    this.#lastPaths = o.paths
  }

  #observeDemand(now: number, o: ThroughputObservation, busy: boolean): boolean {
    if (busy) {
      this.#idleSince = undefined
      this.#eligibleSince ??= now
      return false
    }
    this.#eligibleSince = undefined
    this.#idleSince ??= now
    if (o.optionalPaths === 0 || o.pending || now - this.#idleSince < IDLE_RETENTION_MS) return false
    this.#trial = undefined
    this.#notBefore = now + COOLDOWN_MS
    this.#idleSince = now
    this.#reset(now, o)
    return true
  }

  #readyToMeasure(now: number, o: ThroughputObservation): boolean {
    const trial = this.#trial
    if (trial === undefined) {
      if (o.paths === this.#lastPaths) return true
    } else if (o.paths <= trial.paths) {
      if (o.pending) return false
      this.#trial = undefined
      this.#notBefore = now + COOLDOWN_MS
    } else if (trial.admittedAt === undefined) {
      trial.admittedAt = now
    } else if (now - trial.admittedAt >= SETTLING_MS) {
      return true
    }
    this.#reset(now, o)
    return false
  }

  #finishTrial(now: number, rates: Rates): ThroughputDecision {
    const trial = this.#trial!
    this.#trial = undefined
    this.#notBefore = now + COOLDOWN_MS
    const improved = rates.usefulRate >= trial.usefulRate * MINIMUM_USEFUL_GAIN &&
      rates.outputRate >= trial.outputRate * MINIMUM_OUTPUT_GAIN
    return {
      action: improved ? 'keep' : 'retire',
      reason: improved ? 'aggregate-progress-improved' : 'no-aggregate-gain',
      ...rates, baselineUsefulRate: trial.usefulRate, baselineOutputRate: trial.outputRate,
    }
  }

  #considerGrowth(now: number, o: ThroughputObservation, maximumPaths: number, busy: boolean, rates: Rates): ThroughputDecision {
    const outputLimited = rates.outputRate < rates.usefulRate * MINIMUM_OUTPUT_RATIO
    if (o.optionalPaths > 0 && outputLimited && o.capacityWaitingBytes > 0 && !o.pending) {
      this.#notBefore = now + COOLDOWN_MS
      return { action: 'retire', reason: 'consumer-pressure', ...rates }
    }
    if (!busy || this.#eligibleSince === undefined || now - this.#eligibleSince < OBSERVATION_MS ||
      now < this.#notBefore || o.pending || o.paths >= maximumPaths ||
      rates.usefulRate <= 0 || rates.outputRate <= 0 || outputLimited) return HOLD
    if (o.remainingKnown && o.remainingBytes < rates.usefulRate * (OBSERVATION_MS + SETTLING_MS) / 1000) return HOLD
    this.#trial = { paths: o.paths, ...rates }
    return { action: 'explore', reason: 'sustained-network-demand', ...rates }
  }
}
