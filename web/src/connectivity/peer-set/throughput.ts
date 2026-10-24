import { SAMPLE_BYTE_BUDGET, SAMPLE_LIFETIME_MILLISECONDS, type SamplingEvidence } from '../../content/scheduling/sampling'

export const THROUGHPUT_SAMPLE_MS = 1000
const OBSERVATION_MS = 3000
const FAILURE_BACKOFF_MS = 15_000
const IDLE_RETENTION_MS = 10_000
const MINIMUM_USEFUL_GAIN = 1.10
const MINIMUM_OUTPUT_GAIN = 1.05
const MINIMUM_OUTPUT_RATIO = 0.65
const MINIMUM_SETTLED_ATTEMPTS = 4
const MINIMUM_BUSY_MS = 1000

export interface ThroughputPathIdentity {
  readonly pathId: string
  readonly laneId: number
  readonly laneEpoch: number
}
export interface ThroughputPath {
  readonly identity: ThroughputPathIdentity
  readonly optional: boolean
  readonly work: SamplingEvidence
}
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
  readonly paths: readonly ThroughputPath[]
  readonly pending: boolean
}
export interface ThroughputDecision {
  readonly action: 'hold' | 'explore' | 'sample' | 'retire' | 'keep' | 'abandon'
  readonly reason?: string
  readonly path?: ThroughputPathIdentity
  readonly work?: SamplingEvidence
  readonly usefulRate?: number
  readonly outputRate?: number
  readonly baselineUsefulRate?: number
  readonly baselineOutputRate?: number
}
interface Window { at: number; useful: number; output: number }
interface Rates { usefulRate: number; outputRate: number }
interface Baseline { rates: Rates; paths: readonly ThroughputPathIdentity[] }
type Trial =
  | { phase: 'connecting'; baseline: Baseline }
  | { phase: 'sampling'; baseline: Baseline; path: ThroughputPathIdentity; initial: SamplingEvidence; started: number; window: Window }
const HOLD = { action: 'hold' } as const

function samePath(a: ThroughputPathIdentity, b: ThroughputPathIdentity): boolean {
  return a.pathId === b.pathId && a.laneId === b.laneId && a.laneEpoch === b.laneEpoch
}
function samePaths(a: readonly ThroughputPathIdentity[], b: readonly ThroughputPathIdentity[]): boolean {
  return a.length === b.length && a.every(path => b.some(other => samePath(path, other)))
}
function windowAt(now: number, o: ThroughputObservation): Window {
  return { at: now, useful: o.usefulBytes, output: o.outputBytes }
}
function ratesAt(now: number, o: ThroughputObservation, window: Window): Rates {
  const seconds = (now - window.at) / 1000
  return { usefulRate: (o.usefulBytes - window.useful) / seconds, outputRate: (o.outputBytes - window.output) / seconds }
}
function workSince(work: SamplingEvidence, initial: SamplingEvidence): SamplingEvidence {
  return {
    offeredBytes: work.offeredBytes - initial.offeredBytes, settledBytes: work.settledBytes - initial.settledBytes,
    completedBytes: work.completedBytes - initial.completedBytes,
    completedAttempts: work.completedAttempts - initial.completedAttempts, failedAttempts: work.failedAttempts - initial.failedAttempts,
    supersededAttempts: work.supersededAttempts - initial.supersededAttempts, cancelledAttempts: work.cancelledAttempts - initial.cancelledAttempts,
    busyMilliseconds: work.busyMilliseconds - initial.busyMilliseconds,
  }
}
function representative(work: SamplingEvidence): boolean {
  // Multiple settled attempts avoid judging startup from one block; either a
  // busy interval or a full byte sample also accommodates very fast paths.
  return work.completedAttempts + work.failedAttempts + work.supersededAttempts >= MINIMUM_SETTLED_ATTEMPTS &&
    (work.busyMilliseconds >= MINIMUM_BUSY_MS || work.completedBytes >= SAMPLE_BYTE_BUDGET)
}

/** Connectivity owns the experiment; allocation supplies generation-bound work facts. */
export class ThroughputController {
  #window: Window | undefined
  #trial: Trial | undefined
  #eligibleSince: number | undefined
  #idleSince: number | undefined
  #notBefore = -Infinity
  #paths: readonly ThroughputPathIdentity[] = []

  step(now: number, o: ThroughputObservation, maximumPaths: number): ThroughputDecision {
    const windowHasMoreWork = o.queuedBytes === 0 && o.inFlightBytes > 0 && o.remainingBytes > o.inFlightBytes
    const busy = (o.networkWaitingBytes > 0 || windowHasMoreWork) && o.unfinishedBytes > 0 && o.paths.length > 0
    if (busy) { this.#idleSince = undefined; this.#eligibleSince ??= now }
    else { this.#eligibleSince = undefined; this.#idleSince ??= now }
    if (this.#trial !== undefined) return this.#advanceTrial(now, o, busy)
    const optional = o.paths.filter(path => path.optional).at(-1)
    if (!busy && optional !== undefined && !o.pending && now - this.#idleSince! >= IDLE_RETENTION_MS) {
      this.#notBefore = now + OBSERVATION_MS
      this.#reset(now, o)
      return { action: 'retire', reason: 'no-network-demand', path: optional.identity }
    }
    if (this.#window === undefined || o.usefulBytes < this.#window.useful || o.outputBytes < this.#window.output ||
      !samePaths(this.#paths, o.paths.map(path => path.identity))) {
      this.#reset(now, o)
      return HOLD
    }
    if (now - this.#window.at < OBSERVATION_MS) return HOLD
    const rates = ratesAt(now, o, this.#window)
    this.#reset(now, o)
    const outputLimited = rates.outputRate < rates.usefulRate * MINIMUM_OUTPUT_RATIO
    if (optional !== undefined && outputLimited && o.capacityWaitingBytes > 0 && !o.pending) {
      this.#notBefore = now + OBSERVATION_MS
      return { action: 'retire', reason: 'consumer-pressure', path: optional.identity, ...rates }
    }
    if (!busy || this.#eligibleSince === undefined || now - this.#eligibleSince < OBSERVATION_MS ||
      now < this.#notBefore || o.pending || o.paths.length >= maximumPaths ||
      rates.usefulRate <= 0 || rates.outputRate <= 0 || outputLimited) return HOLD
    if (o.remainingKnown && o.remainingBytes < rates.usefulRate * (OBSERVATION_MS + THROUGHPUT_SAMPLE_MS) / 1000) return HOLD
    this.#trial = { phase: 'connecting', baseline: { rates, paths: this.#paths } }
    return { action: 'explore', reason: 'sustained-network-demand', ...rates }
  }

  #reset(now: number, o: ThroughputObservation): void {
    this.#window = windowAt(now, o)
    this.#paths = o.paths.map(path => path.identity)
  }

  #advanceTrial(now: number, o: ThroughputObservation, busy: boolean): ThroughputDecision {
    const trial = this.#trial!
    if (trial.phase === 'connecting') return this.#awaitTrialPath(now, o, trial.baseline)
    const path = o.paths.find(path => samePath(path.identity, trial.path))
    if (path === undefined) return this.#endTrial(now, o,
      { action: 'abandon', reason: 'trial-path-replaced', path: trial.path }, OBSERVATION_MS)
    if (!samePaths(trial.baseline.paths, o.paths.filter(path => !samePath(path.identity, trial.path)).map(path => path.identity))) {
      return this.#endTrial(now, o, { action: 'retire', reason: 'trial-topology-changed', path: trial.path }, OBSERVATION_MS)
    }
    return this.#measureTrial(now, o, busy, trial, path.work)
  }

  #awaitTrialPath(now: number, o: ThroughputObservation, baseline: Baseline): ThroughputDecision {
    const candidate = o.paths.find(path => path.optional && !baseline.paths.some(old => old.pathId === path.identity.pathId))
    if (candidate === undefined) {
      if (o.pending) return HOLD
      return this.#endTrial(now, o, { action: 'abandon', reason: 'connection-not-admitted' }, FAILURE_BACKOFF_MS)
    }
    this.#trial = { phase: 'sampling', baseline, path: candidate.identity,
      initial: candidate.work, started: now, window: windowAt(now, o) }
    return { action: 'sample', reason: 'path-sampling-started', path: candidate.identity,
      baselineUsefulRate: baseline.rates.usefulRate, baselineOutputRate: baseline.rates.outputRate }
  }

  #measureTrial(now: number, o: ThroughputObservation, busy: boolean,
    trial: Extract<Trial, { phase: 'sampling' }>, evidence: SamplingEvidence): ThroughputDecision {
    const work = workSince(evidence, trial.initial)
    if (Object.values(work).some(value => value < 0) || o.usefulBytes < trial.window.useful || o.outputBytes < trial.window.output) {
      return this.#endTrial(now, o, { action: 'retire', reason: 'trial-observation-reset', path: trial.path }, OBSERVATION_MS)
    }
    if (!busy) return this.#endTrial(now, o, { action: 'retire', reason: 'trial-demand-ended', path: trial.path, work }, OBSERVATION_MS)
    if (work.offeredBytes === 0) trial.window = windowAt(now, o)
    if (now - trial.window.at >= OBSERVATION_MS) {
      const decision = this.#evaluateTrial(now, o, trial, work)
      if (decision !== undefined) return decision
    }
    if (now - trial.started >= SAMPLE_LIFETIME_MILLISECONDS) return this.#endTrial(now, o,
      { action: 'retire', reason: 'insufficient-scheduling-evidence', path: trial.path, work }, OBSERVATION_MS)
    return HOLD
  }

  #evaluateTrial(now: number, o: ThroughputObservation,
    trial: Extract<Trial, { phase: 'sampling' }>, work: SamplingEvidence): ThroughputDecision | undefined {
    const rates = ratesAt(now, o, trial.window)
    if (rates.outputRate < rates.usefulRate * MINIMUM_OUTPUT_RATIO && o.capacityWaitingBytes > 0) {
      return this.#endTrial(now, o, { action: 'retire', reason: 'consumer-pressure', path: trial.path, work, ...rates }, OBSERVATION_MS)
    }
    if (!representative(work)) return undefined
    const improved = rates.usefulRate >= trial.baseline.rates.usefulRate * MINIMUM_USEFUL_GAIN &&
      rates.outputRate >= trial.baseline.rates.outputRate * MINIMUM_OUTPUT_GAIN
    return this.#endTrial(now, o, { action: improved ? 'keep' : 'retire',
      reason: improved ? 'aggregate-progress-improved' : 'no-aggregate-gain', path: trial.path, work, ...rates,
      baselineUsefulRate: trial.baseline.rates.usefulRate, baselineOutputRate: trial.baseline.rates.outputRate },
    improved ? 0 : FAILURE_BACKOFF_MS)
  }

  #endTrial(now: number, o: ThroughputObservation, decision: ThroughputDecision, delay: number): ThroughputDecision {
    const baseline = this.#trial!.baseline.rates
    this.#trial = undefined
    this.#notBefore = now + delay
    this.#reset(now, o)
    return { ...decision, baselineUsefulRate: baseline.usefulRate, baselineOutputRate: baseline.outputRate }
  }
}
