import {
  parseAttemptEvidence,
  type AttemptEvidence,
  type BrowserAttemptEvidence,
  type LaneIdentity,
} from '../../scripts/browser-evidence/attempt-evidence'
import {
  AttemptCollector,
  type LogicalAttempt,
} from '../../scripts/browser-evidence/attempt-collector'
import type { DeliveryEvidence } from '../../scripts/browser-evidence/result'
import {
  parseMainRouteEvidence,
  type MainRouteEvidence,
  type RouteObservation,
} from '../../scripts/browser-evidence/route-evidence'

const TERMINAL_ATTEMPT_STAGES = new Set(['admitted', 'failed'] as const)
const SENDER_EVIDENCE_POLL_INTERVAL_MS = 25

export type BrowserAttemptTerminal = Extract<
  BrowserAttemptEvidence,
  { readonly stage: 'admitted' | 'failed' }
>

export interface HotSwitchDispatch {
  readonly dispatchSequence: number
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'relay' | 'peer'
}

export interface HotSwitchLaneObservation {
  readonly laneId: number
  readonly laneEpoch: number
  readonly route: 'relay' | 'peer'
}

export interface ObservedTransferFailure {
  readonly kind: 'directory' | 'file'
  readonly id: string
  readonly reason: string
}

export interface ObservedJobOutcome {
  readonly status: 'Succeeded' | 'CompletedWithErrors' | 'Aborted'
  readonly failures: readonly ObservedTransferFailure[]
  readonly failureCount: number
  readonly omittedFailureCount: number
}

export interface HotSwitchDeliveryTerminal {
  readonly outcome: 'succeeded' | 'failed'
  readonly evidence: DeliveryEvidence
  readonly jobOutcome?: ObservedJobOutcome
  readonly failureMessage?: string
}

export interface HotSwitchRuntimeTerminal {
  readonly error?: string
}

export interface SenderAttemptEvidenceSnapshot {
  readonly records: readonly unknown[]
  readonly hasUnterminatedFinalRecord: boolean
}

export interface AttemptEvidenceReporter {
  recordAttempt(evidence: AttemptEvidence): void
}

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

export type HotSwitchPageEvent =
  | { readonly kind: 'attempt'; readonly evidence: BrowserAttemptEvidence }
  | { readonly kind: 'dispatch'; readonly observation: HotSwitchDispatch }
  | { readonly kind: 'lane-admitted'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'lane-detached'; readonly observation: HotSwitchLaneObservation }
  | { readonly kind: 'relay-ineligible' }
  | ({ readonly kind: 'delivery' } & HotSwitchDeliveryTerminal)
  | ({ readonly kind: 'runtime-settled' } & HotSwitchRuntimeTerminal)

interface EventWaiter<T> {
  readonly predicate: (value: T) => boolean
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
  readonly timeout: ReturnType<typeof setTimeout>
}

class EventStream<T> {
  readonly #values: T[] = []
  readonly #waiters = new Set<EventWaiter<T>>()
  #terminalError: unknown

  publish(value: T): void {
    if (this.#terminalError !== undefined) {
      throw new Error('Event stream received an observation after it terminated', {
        cause: this.#terminalError,
      })
    }
    this.#values.push(value)
    for (const waiter of [...this.#waiters]) {
      let matches: boolean
      try {
        matches = waiter.predicate(value)
      } catch (error) {
        this.#settle(waiter)
        waiter.reject(error)
        continue
      }
      if (!matches) continue
      this.#settle(waiter)
      waiter.resolve(value)
    }
  }

  waitFor(
    predicate: (value: T) => boolean,
    waitingFor: string,
    deadlineMs: number,
  ): Promise<T> {
    const existing = this.#values.find(predicate)
    if (existing !== undefined) return Promise.resolve(existing)
    if (this.#terminalError !== undefined) return Promise.reject(this.#terminalError)
    if (!Number.isSafeInteger(deadlineMs) || deadlineMs <= 0) {
      return Promise.reject(new RangeError(`${waitingFor} deadline must be a positive integer`))
    }
    return new Promise<T>((resolveWait, rejectWait) => {
      const waiter: EventWaiter<T> = {
        predicate,
        resolve: resolveWait,
        reject: rejectWait,
        timeout: setTimeout(() => {
          this.#waiters.delete(waiter)
          rejectWait(new Error(`Timed out waiting for ${waitingFor} after ${deadlineMs}ms`))
        }, deadlineMs),
      }
      this.#waiters.add(waiter)
    })
  }

  end(reason: unknown): void {
    if (this.#terminalError !== undefined) return
    this.#terminalError = reason
    for (const waiter of [...this.#waiters]) {
      this.#settle(waiter)
      waiter.reject(reason)
    }
  }

  values(): readonly T[] {
    return Object.freeze([...this.#values])
  }

  #settle(waiter: EventWaiter<T>): void {
    clearTimeout(waiter.timeout)
    this.#waiters.delete(waiter)
  }
}

/**
 * The collector is event-driven on purpose: relay fallback activity is merely a
 * dispatch fact and cannot settle the peer terminal wait. Named deadlines only
 * bound missing producer evidence; they never manufacture an attempt outcome.
 */
export class HotSwitchEvidenceCollector {
  readonly #reporter: AttemptEvidenceReporter | null
  readonly #attempts = new AttemptCollector()
  readonly #browserTerminals = new EventStream<BrowserAttemptTerminal>()
  readonly #dispatches = new EventStream<HotSwitchDispatch>()
  readonly #relayIneligibilities = new EventStream<true>()
  readonly #deliveries = new EventStream<HotSwitchDeliveryTerminal>()
  readonly #runtimeTerminals = new EventStream<HotSwitchRuntimeTerminal>()
  readonly #browserAttemptEvidence: BrowserAttemptEvidence[] = []
  readonly #routeObservations: RouteObservation[] = []
  #deliveryObserved = false
  #relayIneligible = false
  #runtimeSettled = false
  #finalized = false

  constructor(reporter: AttemptEvidenceReporter | null = null) {
    this.#reporter = reporter
  }

  acceptPageEvent(value: unknown): void {
    if (this.#runtimeSettled) throw new Error('Hot-switch page emitted evidence after runtime settlement')
    const event = requirePageEvent(value)
    if (event.kind === 'attempt') {
      this.#acceptBrowserAttempt(event.evidence)
      return
    }
    if (event.kind === 'dispatch') {
      this.#recordDispatch(event.observation)
      return
    }
    if (event.kind === 'lane-admitted') {
      this.#acceptLaneAdmission(event.observation)
      return
    }
    if (event.kind === 'lane-detached') {
      return
    }
    if (event.kind === 'relay-ineligible') {
      this.#acceptRelayIneligibility()
      return
    }
    if (event.kind === 'delivery') {
      this.#acceptDelivery(event)
      return
    }
    this.#acceptRuntimeTerminal(event)
  }

  #acceptBrowserAttempt(value: BrowserAttemptEvidence): void {
    const evidence = parseAttemptEvidence(value)
    if (evidence.side !== 'browser') throw new Error('Page attempt evidence must come from the browser side')
    this.#browserAttemptEvidence.push(evidence)
    this.#attempts.ingest(evidence)
    this.#reporter?.recordAttempt(evidence)
    if (!TERMINAL_ATTEMPT_STAGES.has(evidence.stage as 'admitted' | 'failed')) return
    if (evidence.stage === 'admitted') this.#recordPeerAdmission(evidence)
    this.#browserTerminals.publish(evidence as BrowserAttemptTerminal)
  }

  #acceptLaneAdmission(observation: HotSwitchLaneObservation): void {
    if (this.#relayIneligible && observation.route === 'relay') {
      throw new Error('Receiver admitted a relay lane after publishing relay ineligibility')
    }
  }

  #acceptRelayIneligibility(): void {
    if (this.#relayIneligible) throw new Error('Receiver published relay ineligibility more than once')
    this.#relayIneligible = true
    this.#relayIneligibilities.publish(true)
  }

  #acceptDelivery(event: HotSwitchDeliveryTerminal): void {
    if (this.#deliveryObserved) throw new Error('Hot-switch delivery terminal appeared more than once')
    this.#deliveryObserved = true
    this.#deliveries.publish(event)
  }

  #acceptRuntimeTerminal(event: HotSwitchRuntimeTerminal): void {
    this.#runtimeSettled = true
    this.#runtimeTerminals.publish(event)
    const settled = new Error(
      event.error === undefined
        ? 'Hot-switch runtime settled before the requested observation'
        : `Hot-switch runtime failed: ${event.error}`,
    )
    this.#browserTerminals.end(settled)
    this.#dispatches.end(settled)
    this.#relayIneligibilities.end(settled)
    this.#deliveries.end(settled)
  }

  abort(reason: unknown): void {
    this.#browserTerminals.end(reason)
    this.#dispatches.end(reason)
    this.#relayIneligibilities.end(reason)
    this.#deliveries.end(reason)
    this.#runtimeTerminals.end(reason)
  }

  waitForFirstRelayDispatch(deadlineMs: number): Promise<HotSwitchDispatch> {
    return this.#dispatches.waitFor(
      (dispatch) => dispatch.route === 'relay',
      'the first relay block dispatch',
      deadlineMs,
    )
  }

  waitForBrowserTerminal(deadlineMs: number): Promise<BrowserAttemptTerminal> {
    return this.#browserTerminals.waitFor(
      () => true,
      'the browser peer attempt terminal',
      deadlineMs,
    )
  }

  waitForRelayIneligibility(deadlineMs: number): Promise<true> {
    return this.#relayIneligibilities.waitFor(
      () => true,
      'receiver relay ineligibility after the proxy cut',
      deadlineMs,
    )
  }

  waitForPostFencePeerDispatch(
    lane: LaneIdentity,
    dispatchSequenceBoundary: number,
    deadlineMs: number,
  ): Promise<HotSwitchDispatch> {
    return this.#dispatches.waitFor(
      (dispatch) => dispatch.route === 'peer' &&
        dispatch.dispatchSequence > dispatchSequenceBoundary && sameLane(dispatch, lane),
      `a post-fence peer dispatch on lane ${lane.laneId}/${lane.laneEpoch}`,
      deadlineMs,
    )
  }

  waitForDelivery(deadlineMs: number): Promise<HotSwitchDeliveryTerminal> {
    return this.#deliveries.waitFor(() => true, 'the delivery terminal', deadlineMs)
  }

  waitForRuntimeSettlement(deadlineMs: number): Promise<HotSwitchRuntimeTerminal> {
    return this.#runtimeTerminals.waitFor(() => true, 'the page runtime terminal', deadlineMs)
  }

  #recordPeerAdmission(terminal: Extract<BrowserAttemptTerminal, { readonly stage: 'admitted' }>): void {
    this.#routeObservations.push(Object.freeze({
      observationSequence: this.#routeObservations.length + 1,
      kind: 'peer-admitted',
      sessionId: terminal.sessionId,
      peerPathId: terminal.peerPathId,
      attemptId: terminal.attemptId,
      lane: terminal.lane,
    }))
  }

  recordRelayCutFence(
    dispatchSequenceBoundary: number,
    fence: { readonly proxyAccepting: false; readonly receiverRelayEligible: false },
  ): void {
    this.#routeObservations.push(Object.freeze({
      observationSequence: this.#routeObservations.length + 1,
      kind: 'relay-cut-fence',
      dispatchSequenceBoundary,
      ...fence,
    }))
  }

  latestDispatchSequence(): number {
    const dispatch = this.#dispatches.values().at(-1)
    if (dispatch === undefined) throw new Error('No block dispatch exists at the relay cut boundary')
    return dispatch.dispatchSequence
  }

  routeEvidence(mode: MainRouteEvidence['mode']): MainRouteEvidence {
    const parsed = parseMainRouteEvidence({ mode, observations: this.#routeObservations })
    if (parsed === null) throw new Error('Hot-switch route evidence unexpectedly parsed as null')
    return parsed
  }

  dispatches(): readonly HotSwitchDispatch[] {
    return this.#dispatches.values()
  }

  ingestSenderEvidence(values: readonly unknown[]): void {
    for (const value of values) {
      const evidence = parseAttemptEvidence(value)
      if (evidence.side !== 'sender') throw new Error('Sender JSONL contained non-sender attempt evidence')
      this.#attempts.ingest(evidence)
      this.#reporter?.recordAttempt(evidence)
    }
  }

  async waitForSenderTerminals(
    readSnapshot: () => Promise<SenderAttemptEvidenceSnapshot>,
    deadlineMs: number,
  ): Promise<readonly unknown[]> {
    if (!Number.isSafeInteger(deadlineMs) || deadlineMs <= 0) {
      throw new RangeError('Sender evidence terminal deadline must be a positive integer')
    }
    const requiredByBrowser = requiredSenderAttemptKeys(this.#browserAttemptEvidence)
    const expiresAt = performance.now() + deadlineMs

    for (;;) {
      const snapshot = await readSnapshot()
      const missing = missingSenderTerminalKeys(snapshot.records, requiredByBrowser)
      if (missing.length === 0 && !snapshot.hasUnterminatedFinalRecord) {
        return Object.freeze([...snapshot.records])
      }

      const remainingMs = expiresAt - performance.now()
      if (remainingMs <= 0) {
        const missingDescription = missing.length === 0 ? 'none' : missing.join(', ')
        throw new Error(
          `Timed out waiting for sender attempt terminals after ${deadlineMs}ms ` +
          `(missing=${missingDescription}, ` +
          `unterminatedFinalRecord=${String(snapshot.hasUnterminatedFinalRecord)})`,
        )
      }
      await new Promise<void>((resolveWait) => {
        setTimeout(resolveWait, Math.min(SENDER_EVIDENCE_POLL_INTERVAL_MS, remainingMs))
      })
    }
  }

  finalizeAttempts(): readonly LogicalAttempt[] {
    if (this.#finalized) throw new Error('Hot-switch attempt evidence can only be finalized once')
    this.#finalized = true
    return this.#attempts.finalize()
  }

  #recordDispatch(dispatch: HotSwitchDispatch): void {
    this.#dispatches.publish(dispatch)
    this.#routeObservations.push(Object.freeze({
      observationSequence: this.#routeObservations.length + 1,
      kind: 'dispatch',
      dispatchSequence: dispatch.dispatchSequence,
      route: dispatch.route,
      lane: Object.freeze({ laneId: dispatch.laneId, laneEpoch: dispatch.laneEpoch }),
    }))
  }
}

function requirePageEvent(value: unknown): HotSwitchPageEvent {
  const event = requireRecord(value, 'hot-switch page event')
  const kind = event['kind']
  if (kind === 'attempt') {
    requireExactKeys(event, ['kind', 'evidence'], 'hot-switch attempt event')
    return { kind, evidence: event['evidence'] as BrowserAttemptEvidence }
  }
  if (kind === 'dispatch' || kind === 'lane-admitted' || kind === 'lane-detached') {
    requireExactKeys(event, ['kind', 'observation'], `hot-switch ${kind} event`)
    return kind === 'dispatch'
      ? { kind, observation: parseDispatch(event['observation']) }
      : { kind, observation: parseLaneObservation(event['observation']) }
  }
  if (kind === 'relay-ineligible') {
    requireExactKeys(event, ['kind'], 'hot-switch relay ineligibility event')
    return { kind }
  }
  if (kind === 'delivery') return parseDeliveryEvent(event)
  if (kind === 'runtime-settled') {
    requireExactKeys(event, ['kind'], 'hot-switch runtime terminal', ['error'])
    const error = event['error']
    if (error !== undefined && typeof error !== 'string') {
      throw new Error('Hot-switch runtime terminal error must be text')
    }
    return { kind, ...(error === undefined ? {} : { error }) }
  }
  throw new Error(`Unsupported hot-switch page event kind ${String(kind)}`)
}

function parseDispatch(value: unknown): HotSwitchDispatch {
  const observation = requireRecord(value, 'hot-switch dispatch')
  requireExactKeys(
    observation,
    ['dispatchSequence', 'laneId', 'laneEpoch', 'route'],
    'hot-switch dispatch',
  )
  return Object.freeze({
    dispatchSequence: requireInteger(observation['dispatchSequence'], 1, 'dispatch sequence'),
    laneId: requireInteger(observation['laneId'], 1, 'dispatch lane ID'),
    laneEpoch: requireInteger(observation['laneEpoch'], 0, 'dispatch lane epoch'),
    route: requireRoute(observation['route']),
  })
}

function parseLaneObservation(value: unknown): HotSwitchLaneObservation {
  const observation = requireRecord(value, 'hot-switch lane observation')
  requireExactKeys(observation, ['laneId', 'laneEpoch', 'route'], 'hot-switch lane observation')
  return Object.freeze({
    laneId: requireInteger(observation['laneId'], 1, 'detached lane ID'),
    laneEpoch: requireInteger(observation['laneEpoch'], 0, 'detached lane epoch'),
    route: requireRoute(observation['route']),
  })
}

function parseDeliveryEvent(event: Record<string, unknown>): HotSwitchPageEvent {
  requireExactKeys(
    event,
    ['kind', 'outcome', 'evidence'],
    'hot-switch delivery event',
    ['jobOutcome', 'failureMessage'],
  )
  const outcome = event['outcome']
  if (outcome !== 'succeeded' && outcome !== 'failed') {
    throw new Error('Hot-switch delivery outcome is invalid')
  }
  const evidence = requireRecord(event['evidence'], 'hot-switch delivery evidence') as unknown as DeliveryEvidence
  const failureMessage = event['failureMessage']
  if (failureMessage !== undefined && typeof failureMessage !== 'string') {
    throw new Error('Hot-switch delivery failure message must be text')
  }
  return {
    kind: 'delivery',
    outcome,
    evidence,
    ...(event['jobOutcome'] === undefined
      ? {}
      : { jobOutcome: event['jobOutcome'] as ObservedJobOutcome }),
    ...(failureMessage === undefined ? {} : { failureMessage }),
  }
}

function requireRecord(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requireExactKeys(
  value: Record<string, unknown>,
  required: readonly string[],
  label: string,
  optional: readonly string[] = [],
): void {
  const expected = new Set([...required, ...optional])
  if (
    required.some((key) => !Object.hasOwn(value, key)) ||
    Object.keys(value).some((key) => !expected.has(key))
  ) {
    throw new Error(`${label} has an invalid shape`)
  }
}

function requireInteger(value: unknown, minimum: number, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < minimum) {
    throw new Error(`${label} must be a safe integer of at least ${minimum}`)
  }
  return value as number
}

function requireRoute(value: unknown): 'relay' | 'peer' {
  if (value !== 'relay' && value !== 'peer') throw new Error('Hot-switch route is invalid')
  return value
}

function sameLane(
  left: Pick<HotSwitchDispatch, 'laneId' | 'laneEpoch'>,
  right: LaneIdentity,
): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch
}

export function attemptEvents(attempts: readonly LogicalAttempt[]): readonly AttemptEvidence[] {
  return Object.freeze(attempts.flatMap((attempt) => attempt.events.map(({ evidence }) => evidence)))
}

function requiredSenderAttemptKeys(
  browserEvidence: readonly BrowserAttemptEvidence[],
): ReadonlySet<string> {
  const required = new Set<string>()
  for (const evidence of browserEvidence) {
    if (
      evidence.stage === 'admitted' ||
      evidence.stage === 'answer-received' ||
      (evidence.stage === 'failed' && evidence.authenticatedSenderOperationFailure !== undefined)
    ) {
      required.add(attemptIdentityKey(evidence))
    }
  }
  return required
}

function missingSenderTerminalKeys(
  records: readonly unknown[],
  requiredByBrowser: ReadonlySet<string>,
): readonly string[] {
  const required = new Set(requiredByBrowser)
  const terminals = new Set<string>()
  for (const value of records) {
    const evidence = parseAttemptEvidence(value)
    if (evidence.side !== 'sender') {
      throw new Error('Sender JSONL contained non-sender attempt evidence')
    }
    const key = attemptIdentityKey(evidence)
    required.add(key)
    if (TERMINAL_ATTEMPT_STAGES.has(evidence.stage as 'admitted' | 'failed')) {
      terminals.add(key)
    }
  }
  return [...required].filter((key) => !terminals.has(key)).sort()
}

function attemptIdentityKey(identity: {
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
}): string {
  return `${identity.sessionId}/${identity.peerPathId}/${identity.attemptId}`
}
