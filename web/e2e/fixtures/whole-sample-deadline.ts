export type WholeSampleDeadlinePhase = 'work' | 'cleanup' | 'publication'

export interface WholeSampleDeadlineTiming {
  readonly totalTimeoutMs: number
  readonly teardownReserveMs: number
  readonly evidencePublicationMs: number
  readonly completionMarginMs: number
}

export interface WholeSampleDeadlineClock {
  now(): number
}

export interface WholeSampleDeadlineTimer {
  cancel(): void
}

export interface WholeSampleDeadlineScheduler {
  schedule(callback: () => void, delayMs: number): WholeSampleDeadlineTimer
}

export interface WholeSampleDeadlineDependencies {
  readonly clock?: WholeSampleDeadlineClock
  readonly scheduler?: WholeSampleDeadlineScheduler
}

export class WholeSampleDeadlineExpiredError extends Error {
  readonly phase: WholeSampleDeadlinePhase
  readonly cutoffAtMs: number

  constructor(phase: WholeSampleDeadlinePhase, cutoffAtMs: number) {
    super(`Whole-sample ${phase} phase reached its absolute cutoff at ${cutoffAtMs}ms`)
    this.name = 'WholeSampleDeadlineExpiredError'
    this.phase = phase
    this.cutoffAtMs = cutoffAtMs
  }
}

interface WholeSamplePhaseState {
  readonly phase: WholeSampleDeadlinePhase
  readonly cutoffAtMs: number
  readonly controller: AbortController
  timer: WholeSampleDeadlineTimer | undefined
}

const PERFORMANCE_DEADLINE_CLOCK: WholeSampleDeadlineClock = Object.freeze({
  now: () => performance.now(),
})

const TIMEOUT_DEADLINE_SCHEDULER: WholeSampleDeadlineScheduler = Object.freeze({
  schedule(callback: () => void, delayMs: number) {
    const timer = setTimeout(callback, delayMs)
    return Object.freeze({ cancel: () => clearTimeout(timer) })
  },
})

export class WholeSampleDeadline {
  readonly #clock: WholeSampleDeadlineClock
  readonly #work: WholeSamplePhaseState
  readonly #cleanup: WholeSamplePhaseState
  readonly #publication: WholeSamplePhaseState
  #disposed = false

  readonly workSignal: AbortSignal
  readonly cleanupSignal: AbortSignal
  readonly publicationSignal: AbortSignal

  constructor(
    timing: WholeSampleDeadlineTiming,
    dependencies: WholeSampleDeadlineDependencies = {},
  ) {
    validateWholeSampleDeadlineTiming(timing)
    this.#clock = dependencies.clock ?? PERFORMANCE_DEADLINE_CLOCK
    const scheduler = dependencies.scheduler ?? TIMEOUT_DEADLINE_SCHEDULER
    const startedAt = this.#clock.now()
    if (!Number.isFinite(startedAt)) {
      throw new RangeError('Whole-sample deadline clock must return a finite timestamp')
    }

    const completionCutoff = startedAt + timing.totalTimeoutMs
    if (
      Math.abs(startedAt) > Number.MAX_SAFE_INTEGER ||
      Math.abs(completionCutoff) > Number.MAX_SAFE_INTEGER
    ) {
      throw new RangeError('Whole-sample deadline timestamp exceeds the safe number range')
    }
    const publicationCutoff = completionCutoff - timing.completionMarginMs
    const cleanupCutoff = publicationCutoff - timing.evidencePublicationMs
    const workCutoff = completionCutoff - timing.teardownReserveMs
    this.#work = createWholeSamplePhase('work', workCutoff)
    this.#cleanup = createWholeSamplePhase('cleanup', cleanupCutoff)
    this.#publication = createWholeSamplePhase('publication', publicationCutoff)
    this.workSignal = this.#work.controller.signal
    this.cleanupSignal = this.#cleanup.controller.signal
    this.publicationSignal = this.#publication.controller.signal

    try {
      for (const phase of this.#phases()) {
        phase.timer = scheduler.schedule(
          () => {
            phase.timer = undefined
            this.#expire(phase)
          },
          phase.cutoffAtMs - startedAt,
        )
      }
    } catch (error) {
      this.#cancelTimers()
      throw error
    }
  }

  remainingWork(maximumMs = Number.MAX_SAFE_INTEGER): number {
    return this.#remaining(this.#work, maximumMs)
  }

  remainingCleanup(maximumMs = Number.MAX_SAFE_INTEGER): number {
    return this.#remaining(this.#cleanup, maximumMs)
  }

  remainingPublication(maximumMs = Number.MAX_SAFE_INTEGER): number {
    return this.#remaining(this.#publication, maximumMs)
  }

  runWork<T>(operation: (signal: AbortSignal) => T | PromiseLike<T>): Promise<T> {
    return this.#run(this.#work, operation, false)
  }

  runCleanup<T>(operation: (signal: AbortSignal) => T | PromiseLike<T>): Promise<T> {
    return this.#run(this.#cleanup, operation, true)
  }

  runPublication<T>(operation: (signal: AbortSignal) => T | PromiseLike<T>): Promise<T> {
    return this.#run(this.#publication, operation, false)
  }

  dispose(): void {
    if (this.#disposed) return
    this.#disposed = true
    this.#cancelTimers()
    const reason = new Error('Whole-sample deadline was disposed')
    for (const phase of this.#phases()) {
      if (!phase.controller.signal.aborted) phase.controller.abort(reason)
    }
  }

  #remaining(phase: WholeSamplePhaseState, maximumMs: number): number {
    validateMaximumDeadline(maximumMs)
    const remainingMs = phase.cutoffAtMs - this.#clock.now()
    if (remainingMs <= 0) this.#expire(phase)
    if (phase.controller.signal.aborted) throw phase.controller.signal.reason
    return Math.min(maximumMs, Math.ceil(remainingMs))
  }

  #run<T>(
    phase: WholeSamplePhaseState,
    operation: (signal: AbortSignal) => T | PromiseLike<T>,
    invokeAfterExpiry: boolean,
  ): Promise<T> {
    this.#expireIfElapsed(phase)
    const signal = phase.controller.signal
    if (signal.aborted && !invokeAfterExpiry) return Promise.reject(signal.reason)

    return new Promise<T>((resolveRun, rejectRun) => {
      let settled = false
      const settle = (settlement: () => void): void => {
        if (settled) return
        settled = true
        signal.removeEventListener('abort', rejectAtCutoff)
        settlement()
      }
      const rejectAtCutoff = (): void => {
        settle(() => rejectRun(signal.reason))
      }
      signal.addEventListener('abort', rejectAtCutoff, { once: true })

      let result: Promise<T>
      try {
        // Expired cleanup still owns resources that may require synchronous
        // termination before this scope can safely stop awaiting their promises.
        // Work and publication are rejected before invocation so they cannot
        // create ownership or write evidence after their authoritative cutoffs.
        result = Promise.resolve(operation(signal))
      } catch (error) {
        result = Promise.reject(error)
      }

      // The operation outlives this await when a cutoff wins. Observing both
      // late paths prevents its eventual settlement from escaping as process noise.
      result.then(
        (value) => {
          this.#expireIfElapsed(phase)
          if (signal.aborted) {
            rejectAtCutoff()
            return
          }
          settle(() => resolveRun(value))
        },
        (error: unknown) => {
          this.#expireIfElapsed(phase)
          if (signal.aborted) {
            rejectAtCutoff()
            return
          }
          settle(() => rejectRun(error))
        },
      )
      this.#expireIfElapsed(phase)
      if (signal.aborted) rejectAtCutoff()
    })
  }

  #expireIfElapsed(phase: WholeSamplePhaseState): void {
    if (this.#clock.now() >= phase.cutoffAtMs) this.#expire(phase)
  }

  #expire(phase: WholeSamplePhaseState): void {
    if (phase.controller.signal.aborted) return
    phase.timer?.cancel()
    phase.timer = undefined
    phase.controller.abort(new WholeSampleDeadlineExpiredError(phase.phase, phase.cutoffAtMs))
  }

  #cancelTimers(): void {
    for (const phase of this.#phases()) {
      phase.timer?.cancel()
      phase.timer = undefined
    }
  }

  #phases(): readonly WholeSamplePhaseState[] {
    return [this.#work, this.#cleanup, this.#publication]
  }
}

export async function acquireWholeSampleResource<T>(
  deadline: WholeSampleDeadline,
  acquire: (signal: AbortSignal) => T | PromiseLike<T>,
  rollbackBoundary: string,
  rollback: (resource: T, signal: AbortSignal) => unknown | PromiseLike<unknown>,
  registerLateCleanup: (boundary: string, task: Promise<unknown>) => void,
): Promise<T> {
  let acquisition: Promise<T> | undefined
  try {
    return await deadline.runWork((signal) => {
      // Create the raw promise synchronously so a cutoff can attach compensation
      // even when acquisition itself has not produced a handle yet.
      acquisition = Promise.resolve().then(() => acquire(signal))
      return acquisition
    })
  } catch (error) {
    // A callback rejected before invocation owns nothing. This distinction keeps
    // the authoritative deadline failure intact instead of manufacturing rollback.
    if (acquisition !== undefined) {
      const rollbackTask = acquisition.then(
        (resource) => rollback(resource, deadline.cleanupSignal),
        () => undefined,
      )
      // Registration observes rejection immediately and lets independent owner
      // teardown proceed while an abort-insensitive acquisition settles.
      registerLateCleanup(rollbackBoundary, rollbackTask)
    }
    throw error
  }
}

function createWholeSamplePhase(
  phase: WholeSampleDeadlinePhase,
  cutoffAtMs: number,
): WholeSamplePhaseState {
  return { phase, cutoffAtMs, controller: new AbortController(), timer: undefined }
}

function validateWholeSampleDeadlineTiming(timing: WholeSampleDeadlineTiming): void {
  const durations = [
    ['totalTimeoutMs', timing.totalTimeoutMs],
    ['teardownReserveMs', timing.teardownReserveMs],
    ['evidencePublicationMs', timing.evidencePublicationMs],
    ['completionMarginMs', timing.completionMarginMs],
  ] as const
  for (const [name, value] of durations) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new RangeError(`Whole-sample ${name} must be a positive safe integer`)
    }
  }
  if (timing.teardownReserveMs >= timing.totalTimeoutMs) {
    throw new RangeError('Whole-sample teardown reserve must be smaller than the total timeout')
  }
  const teardownAfterCompletionMargin = timing.teardownReserveMs - timing.completionMarginMs
  if (
    teardownAfterCompletionMargin <= 0 ||
    timing.evidencePublicationMs >= teardownAfterCompletionMargin
  ) {
    // A strict gap preserves real cleanup authority instead of creating two
    // different phase names that share the same absolute cutoff.
    throw new RangeError(
      'Whole-sample teardown reserve must exceed evidence publication plus completion margin',
    )
  }
}

function validateMaximumDeadline(maximumMs: number): void {
  if (!Number.isSafeInteger(maximumMs) || maximumMs <= 0) {
    throw new RangeError('Whole-sample phase maximum must be a positive safe integer')
  }
}
