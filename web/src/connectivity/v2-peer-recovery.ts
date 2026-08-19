import type { FailureCorrelation } from '../diagnostics/incident/fact'
import {
  createV2PeerPathIdentityValue,
  createV2ProtocolSessionIdentity,
  equalV2DiagnosticIdentities,
  type V2PeerAttemptIdentity,
  type V2PeerPathIdentity,
  type V2ProtocolSessionIdentity,
} from '../session/v2-identities'
import type {
  V2ConnectivityTraceSource,
  V2PeerRecoveryTraceEvent,
  V2PeerRecoveryWaveTrigger,
} from './diagnostics'
import {
  abortOwner,
  authoritativeAfterCancellation,
  isAttemptHandle,
  localContractResult,
  normalizeAttemptResult,
  waitForAbort,
} from './v2-peer-recovery-attempt-result'
import {
  classifyV2PeerAttemptFailure,
  type V2PeerAdmittedLane,
  type V2PeerAttemptCancellationOwner,
  type V2PeerAttemptPhase,
  type V2PeerAttemptResult,
  type V2PeerFailureDecision,
  type V2ProtocolSessionTerminalSnapshot,
} from './v2-peer-failure'
import {
  browserV2PeerRecoveryClock,
  calculateV2PeerRetryDelay,
  createV2PeerRecoveryPolicy,
  requireV2PeerRecoveryCorrelation,
  type V2PeerAttemptContext,
  type V2PeerAttemptHandle,
  type V2PeerLaneDetachment,
  type V2PeerRecoveryActivation,
  type V2PeerRecoveryAttemptFactory,
  type V2PeerRecoveryAttemptRace as AttemptRace,
  type V2PeerRecoveryClock,
  type V2PeerRecoveryExhaustion as RecoveryExhaustion,
  type V2PeerRecoveryPendingWave as PendingWave,
  type V2PeerRecoveryPolicy,
  type V2PeerRecoveryRearmSource,
  type V2PeerRecoveryState,
  type V2PeerRecoverySupervisorOptions,
  type V2PeerRecoveryWaveBudget as WaveBudget,
} from './v2-peer-recovery-contract'

export * from './v2-peer-recovery-contract'

const NEVER: Promise<never> = new Promise(() => undefined)

type V2PeerRecoveryTracePayload<Event = V2PeerRecoveryTraceEvent> =
  Event extends V2PeerRecoveryTraceEvent
    ? Omit<Event, 'eventName' | 'correlation'>
    : never

export class V2PeerRecoverySupervisor {
  readonly #protocolSessionId: V2ProtocolSessionIdentity
  readonly #peerPathId: V2PeerPathIdentity
  readonly #attempts: V2PeerRecoveryAttemptFactory
  readonly #policy: V2PeerRecoveryPolicy
  readonly #clock: V2PeerRecoveryClock
  readonly #random: () => number
  readonly #trace: V2ConnectivityTraceSource | undefined
  #state: V2PeerRecoveryState = Object.freeze({ kind: 'idle' })
  #task: Promise<void> | undefined
  #waveController: AbortController | undefined
  #currentAttempt: V2PeerAttemptHandle | undefined
  #currentLane: V2PeerAdmittedLane | undefined
  #unsubscribeRearm: (() => void) | undefined
  #activationCount = 0
  #sessionAttempts = 0
  #sessionActiveElapsedMilliseconds = 0
  #activeSince: number | undefined
  #lastNow: number | undefined
  #waveOrdinal = 0
  #authenticatedNotBefore = 0
  #preferredLaneId = 0
  #terminal: V2ProtocolSessionTerminalSnapshot | undefined
  #pendingWave: PendingWave | undefined

  constructor(options: V2PeerRecoverySupervisorOptions) {
    requireV2PeerRecoveryCorrelation(options.protocolSessionId, options.peerPathId)
    this.#protocolSessionId = createV2ProtocolSessionIdentity(options.protocolSessionId.copyBytes())
    this.#peerPathId = createV2PeerPathIdentityValue(options.peerPathId.copyBytes())
    this.#attempts = options.attempts
    this.#policy = createV2PeerRecoveryPolicy(options.policy)
    this.#clock = options.clock ?? browserV2PeerRecoveryClock
    this.#random = options.random ?? Math.random
    this.#trace = options.trace
    this.#subscribeRearm(options.rearmSource)
  }

  get state(): V2PeerRecoveryState {
    return this.#state
  }

  get sessionAttemptCount(): number {
    return this.#sessionAttempts
  }

  get sessionActiveElapsedMilliseconds(): number {
    return this.#sessionElapsedAt(this.#now())
  }

  activate(): V2PeerRecoveryActivation {
    const wasInactive = this.#activationCount === 0
    this.#activationCount += 1
    const shouldRearm = this.#state.kind === 'quiescent'
    if (this.#state.kind === 'idle' || shouldRearm) {
      this.#scheduleWave('activation', shouldRearm)
    } else if (
      wasInactive && this.#task !== undefined &&
      (this.#state.kind === 'attempting' || this.#state.kind === 'waiting-retry') &&
      this.#waveController?.signal.aborted
    ) {
      // A new click arriving while the last-click cancellation joins owns the next wave.
      this.#pendingWave = Object.freeze({ trigger: 'activation', rearmed: false })
    }
    let closed = false
    return Object.freeze({
      close: () => {
        if (closed) return
        closed = true
        this.#activationClosed()
      },
    })
  }

  networkChanged(): void {
    if (this.#activationCount === 0 || this.#state.kind !== 'quiescent') return
    this.#scheduleWave('network-change', true)
  }

  peerDetached(detachment: V2PeerLaneDetachment): boolean {
    if (this.#state.kind !== 'admitted' || !this.#matchesCurrentLane(detachment)) return false
    this.#preferredLaneId = detachment.laneId
    this.#currentLane = undefined
    this.#emit(
      () => Object.freeze({ stage: 'peer-detached' }),
      undefined,
      detachment,
    )
    if (this.#activationCount === 0) {
      this.#setState(Object.freeze({ kind: 'idle' }))
    } else {
      this.#scheduleWave('detachment', false)
    }
    return true
  }

  async sessionTerminated(terminal: V2ProtocolSessionTerminalSnapshot): Promise<void> {
    if (this.#terminal !== undefined) {
      await this.join().catch(() => undefined)
      this.#stopSession(this.#terminal)
      return
    }
    const decision = classifyV2PeerAttemptFailure({ kind: 'session-terminal', terminal })
    if (decision.type !== 'stop-session') {
      this.#stopPath('untyped-failure')
      return
    }
    this.#terminal = decision.terminal
    this.#pendingWave = undefined
    this.#unsubscribeRearm?.()
    this.#unsubscribeRearm = undefined
    this.#cancelWave('runtime-stop')
    await this.join().catch(() => undefined)
    this.#stopSession(decision.terminal)
  }

  close(): Promise<void> {
    return this.sessionTerminated(Object.freeze({
      authority: 'protocol-session-terminal',
      code: 'generation-retired',
    }))
  }

  join(): Promise<void> {
    return this.#task ?? Promise.resolve()
  }

  #subscribeRearm(source: V2PeerRecoveryRearmSource | undefined): void {
    if (source === undefined) return
    try {
      this.#unsubscribeRearm = source.subscribe(() => this.networkChanged())
    } catch {
      // A platform event adapter is observational input and cannot alter recovery authority.
    }
  }

  #activationClosed(): void {
    if (this.#activationCount === 0) return
    this.#activationCount -= 1
    if (this.#activationCount > 0 || this.#state.kind === 'admitted') return
    if (this.#state.kind === 'attempting' || this.#state.kind === 'waiting-retry') {
      this.#cancelWave('last-activation')
      return
    }
    if (this.#state.kind === 'quiescent') this.#setState(Object.freeze({ kind: 'idle' }))
  }

  #scheduleWave(trigger: V2PeerRecoveryWaveTrigger, rearmed: boolean): void {
    if (this.#terminal !== undefined) return
    if (this.#task !== undefined) {
      this.#pendingWave ??= Object.freeze({ trigger, rearmed })
      return
    }
    const sessionExhaustion = this.#sessionExhaustion()
    if (sessionExhaustion !== undefined) {
      this.#exhaust(sessionExhaustion)
      return
    }
    const task = this.#runWave(trigger, rearmed)
    this.#task = task
    task.then(() => this.#waveFinished(task), () => this.#waveFinished(task))
  }

  #waveFinished(task: Promise<void>): void {
    if (this.#task !== task) return
    this.#task = undefined
    const pending = this.#pendingWave
    this.#pendingWave = undefined
    if (
      pending === undefined || this.#terminal !== undefined ||
      this.#activationCount === 0 ||
      this.#state.kind === 'path-stopped' || this.#state.kind === 'session-exhausted' ||
      this.#state.kind === 'session-stopped'
    ) return
    this.#scheduleWave(pending.trigger, pending.rearmed)
  }

  async #runWave(trigger: V2PeerRecoveryWaveTrigger, rearmed: boolean): Promise<void> {
    const controller = new AbortController()
    this.#waveController = controller
    const wave: WaveBudget = {
      ordinal: ++this.#waveOrdinal,
      startedAt: this.#now(),
      attempts: 0,
    }
    this.#activeSince = wave.startedAt
    this.#emit(() => Object.freeze({ stage: 'wave-started', waveOrdinal: wave.ordinal, trigger }))
    if (rearmed && trigger !== 'detachment') {
      this.#emit(() => Object.freeze({ stage: 'wave-rearmed', waveOrdinal: wave.ordinal, trigger }))
    }

    try {
      await this.#attemptWave(wave, controller.signal)
    } catch {
      this.#stopPath('local-contract')
    } finally {
      this.#pauseSessionElapsed()
      if (this.#waveController === controller) this.#waveController = undefined
      this.#currentAttempt = undefined
    }
  }

  async #attemptWave(wave: WaveBudget, signal: AbortSignal): Promise<void> {
    let retryOrdinal = 0
    let previousAttemptId: V2PeerAttemptIdentity | undefined
    if (!await this.#waitForRetainedNotBefore(wave, signal)) return

    while (!signal.aborted && this.#terminal === undefined && this.#activationCount > 0) {
      const exhaustion = this.#reservationExhaustion(wave)
      if (exhaustion !== undefined) {
        this.#exhaust(exhaustion, wave.ordinal)
        return
      }

      const attempt = this.#reserveAttempt(wave)
      if (attempt === undefined) {
        this.#stopPath('local-contract')
        return
      }
      if (previousAttemptId !== undefined) {
        const replacedAttemptId = previousAttemptId
        this.#emit(
          () => Object.freeze({
            stage: 'attempt-replaced',
            waveOrdinal: wave.ordinal,
            previousAttemptId: replacedAttemptId,
          }),
          attempt.attemptId,
        )
      }
      previousAttemptId = attempt.attemptId

      const raced = await this.#raceAttempt(attempt, wave, signal)
      if (raced.type === 'owner-cancelled') {
        this.#settleOwnerCancellation()
        return
      }
      if (raced.type === 'budget-expired') {
        this.#exhaust(raced.exhaustion, wave.ordinal)
        return
      }
      const continueWave = await this.#settleAttemptResult(
        raced.result,
        attempt.attemptId,
        wave,
        retryOrdinal,
        signal,
      )
      if (!continueWave) return
      retryOrdinal += 1
    }

    this.#settleOwnerCancellation()
  }

  #reserveAttempt(wave: WaveBudget): V2PeerAttemptHandle | undefined {
    wave.attempts += 1
    this.#sessionAttempts += 1
    const waveAttemptOrdinal = wave.attempts
    const sessionAttemptOrdinal = this.#sessionAttempts
    const context: V2PeerAttemptContext = Object.freeze({
      protocolSessionId: this.#protocolSessionId,
      peerPathId: this.#peerPathId,
      waveOrdinal: wave.ordinal,
      waveAttemptOrdinal,
      sessionAttemptOrdinal,
      requestedLaneId: this.#preferredLaneId,
      negotiationBudgetMilliseconds: this.#policy.negotiationBudgetMilliseconds,
      admissionBudgetMilliseconds: this.#policy.admissionBudgetMilliseconds,
      phaseChanged: (phase: V2PeerAttemptPhase) => this.#attemptPhaseChanged(
        wave.ordinal,
        waveAttemptOrdinal,
        sessionAttemptOrdinal,
        phase,
      ),
    })
    this.#setState(Object.freeze({
      kind: 'attempting',
      phase: 'negotiation',
      waveOrdinal: wave.ordinal,
      waveAttemptOrdinal,
      sessionAttemptOrdinal,
    }))

    try {
      const attempt = this.#attempts.createAttempt(context)
      if (!isAttemptHandle(attempt)) return undefined
      this.#currentAttempt = attempt
      return attempt
    } catch {
      return undefined
    }
  }

  #attemptPhaseChanged(
    waveOrdinal: number,
    waveAttemptOrdinal: number,
    sessionAttemptOrdinal: number,
    phase: V2PeerAttemptPhase,
  ): void {
    if (phase !== 'negotiation' && phase !== 'admission') return
    if (
      this.#state.kind !== 'attempting' ||
      this.#state.waveOrdinal !== waveOrdinal ||
      this.#state.waveAttemptOrdinal !== waveAttemptOrdinal ||
      this.#state.sessionAttemptOrdinal !== sessionAttemptOrdinal
    ) return
    this.#setState(Object.freeze({ ...this.#state, phase }))
  }

  async #raceAttempt(
    attempt: V2PeerAttemptHandle,
    wave: WaveBudget,
    signal: AbortSignal,
  ): Promise<AttemptRace> {
    const remaining = this.#timeRemaining(wave)
    const exhaustion = remaining.session <= remaining.wave
      ? 'session-elapsed-budget'
      : 'wave-elapsed-budget'
    const timer = new AbortController()
    const budget = this.#clock.sleep(Math.min(remaining.wave, remaining.session), timer.signal)
      .then((): AttemptRace => ({ type: 'budget-expired', exhaustion }))
      .catch(() => timer.signal.aborted ? NEVER : Promise.resolve({
        type: 'result',
        result: localContractResult(),
      } as AttemptRace))
    const ownerCancellation = waitForAbort(signal)
      .then((): AttemptRace => ({ type: 'owner-cancelled' }))
    const result = Promise.resolve(attempt.result)
      .then((value): AttemptRace => ({ type: 'result', result: normalizeAttemptResult(value) }))
      .catch((): AttemptRace => ({ type: 'result', result: localContractResult() }))

    const winner = await Promise.race([result, budget, ownerCancellation])
    timer.abort()
    if (winner.type === 'result') return winner

    this.#safeCancel(attempt, winner.type === 'budget-expired' ? 'recovery-budget' : abortOwner(signal))
    const joined = await result
    if (joined.type === 'result' && authoritativeAfterCancellation(joined.result)) return joined
    return winner
  }

  async #settleAttemptResult(
    result: V2PeerAttemptResult,
    attemptId: V2PeerAttemptIdentity,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    this.#currentAttempt = undefined
    if (result.type === 'admitted') {
      this.#currentLane = Object.freeze({ ...result.lane })
      this.#preferredLaneId = result.lane.laneId
      this.#setState(Object.freeze({ kind: 'admitted', lane: this.#currentLane }))
      return false
    }
    if (result.type === 'lifecycle-cancelled') {
      this.#settleLifecycleCancellation(result.owner)
      return false
    }

    const decision = classifyV2PeerAttemptFailure(result.failure)
    this.#emit(
      () => Object.freeze({
        stage: 'retry-decided',
        waveOrdinal: wave.ordinal,
        decision: decision.type,
        reason: decision.reason,
        authenticatedRetryAfterMilliseconds: decision.type === 'retry-attempt' &&
            decision.reason === 'admission-limited'
          ? decision.authenticatedRetryAfterMilliseconds
          : 0,
      }),
      attemptId,
    )
    if (decision.type === 'stop-path') {
      this.#stopPath(decision.reason)
      return false
    }
    if (decision.type === 'stop-session') {
      this.#terminal = decision.terminal
      this.#stopSession(decision.terminal)
      return false
    }
    return this.#scheduleRetry(decision, attemptId, wave, retryOrdinal, signal)
  }

  async #scheduleRetry(
    decision: Extract<V2PeerFailureDecision, { readonly type: 'retry-attempt' }>,
    attemptId: V2PeerAttemptIdentity,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    const now = this.#now()
    if (decision.reason === 'admission-limited') {
      this.#authenticatedNotBefore = Math.max(
        this.#authenticatedNotBefore,
        now + decision.authenticatedRetryAfterMilliseconds,
      )
    }
    const exhaustion = this.#reservationExhaustion(wave)
    if (exhaustion !== undefined) {
      this.#exhaust(exhaustion, wave.ordinal)
      return false
    }

    let localDelay: number
    try {
      localDelay = calculateV2PeerRetryDelay(
        this.#policy,
        retryOrdinal,
        this.#random(),
      )
    } catch {
      this.#stopPath('local-contract')
      return false
    }
    const authenticatedDelay = Math.max(0, this.#authenticatedNotBefore - now)
    const delay = Math.max(localDelay, authenticatedDelay)
    this.#emit(
      () => Object.freeze({
        stage: 'backoff-scheduled',
        waveOrdinal: wave.ordinal,
        retryOrdinal,
        localDelayMilliseconds: localDelay,
        authenticatedRetryAfterMilliseconds: authenticatedDelay,
        effectiveDelayMilliseconds: delay,
      }),
      attemptId,
    )
    return this.#waitForRetry(delay, wave, retryOrdinal, signal)
  }

  async #waitForRetainedNotBefore(wave: WaveBudget, signal: AbortSignal): Promise<boolean> {
    const delay = Math.max(0, this.#authenticatedNotBefore - this.#now())
    if (delay === 0) return true
    return this.#waitForRetry(delay, wave, 0, signal)
  }

  async #waitForRetry(
    delay: number,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    const remaining = this.#timeRemaining(wave)
    if (delay > remaining.wave || delay > remaining.session) {
      this.#exhaust(
        remaining.session <= remaining.wave
          ? 'session-elapsed-budget'
          : 'wave-elapsed-budget',
        wave.ordinal,
      )
      return false
    }
    this.#setState(Object.freeze({
      kind: 'waiting-retry',
      waveOrdinal: wave.ordinal,
      retryOrdinal,
      delayMilliseconds: delay,
    }))
    try {
      await this.#clock.sleep(delay, signal)
    } catch {
      if (signal.aborted) {
        this.#settleOwnerCancellation()
      } else {
        this.#stopPath('local-contract')
      }
      return false
    }
    const exhaustion = this.#timeExhaustion(wave)
    if (exhaustion !== undefined) {
      this.#exhaust(exhaustion, wave.ordinal)
      return false
    }
    return true
  }

  #settleLifecycleCancellation(owner: V2PeerAttemptCancellationOwner): void {
    if (owner === 'runtime-stop' || owner === 'generation-replaced') {
      if (this.#terminal === undefined) {
        // Cancellation carries control ownership, not ProtocolSession authority.
        // A runtime terminal must arrive through sessionTerminated's sealed snapshot.
        this.#stopPath('local-contract')
      } else {
        this.#stopSession(this.#terminal)
      }
      return
    }
    if (owner === 'last-activation') {
      this.#settleOwnerCancellation()
      return
    }
    this.#stopPath('local-contract')
  }

  #settleOwnerCancellation(): void {
    if (this.#terminal !== undefined) {
      this.#stopSession(this.#terminal)
    } else if (this.#currentLane !== undefined) {
      this.#setState(Object.freeze({ kind: 'admitted', lane: this.#currentLane }))
    } else if (this.#state.kind !== 'path-stopped' && this.#state.kind !== 'session-exhausted') {
      this.#setState(Object.freeze({ kind: 'idle' }))
    }
  }

  #reservationExhaustion(wave: WaveBudget): RecoveryExhaustion | undefined {
    if (this.#sessionAttempts >= this.#policy.sessionMaxAttempts) {
      return 'session-attempt-budget'
    }
    if (wave.attempts >= this.#policy.waveMaxAttempts) return 'wave-attempt-budget'
    return this.#timeExhaustion(wave)
  }

  #sessionExhaustion(): RecoveryExhaustion | undefined {
    if (this.#sessionAttempts >= this.#policy.sessionMaxAttempts) {
      return 'session-attempt-budget'
    }
    return this.#sessionElapsedAt(this.#now()) >=
      this.#policy.sessionActiveElapsedBudgetMilliseconds
      ? 'session-elapsed-budget'
      : undefined
  }

  #timeExhaustion(wave: WaveBudget): RecoveryExhaustion | undefined {
    const remaining = this.#timeRemaining(wave)
    if (remaining.session <= 0) return 'session-elapsed-budget'
    return remaining.wave <= 0 ? 'wave-elapsed-budget' : undefined
  }

  #timeRemaining(wave: WaveBudget): { readonly wave: number; readonly session: number } {
    const now = this.#now()
    return {
      wave: Math.max(0, this.#policy.waveElapsedBudgetMilliseconds - (now - wave.startedAt)),
      session: Math.max(
        0,
        this.#policy.sessionActiveElapsedBudgetMilliseconds - this.#sessionElapsedAt(now),
      ),
    }
  }

  #sessionElapsedAt(now: number): number {
    return this.#sessionActiveElapsedMilliseconds +
      (this.#activeSince === undefined ? 0 : now - this.#activeSince)
  }

  #pauseSessionElapsed(): void {
    if (this.#activeSince === undefined) return
    this.#sessionActiveElapsedMilliseconds += this.#now() - this.#activeSince
    this.#activeSince = undefined
  }

  #now(): number {
    const now = this.#clock.now()
    if (!Number.isFinite(now) || (this.#lastNow !== undefined && now < this.#lastNow)) {
      throw new RangeError('recovery clock must be finite and monotonic')
    }
    this.#lastNow = now
    return now
  }

  #exhaust(exhaustion: RecoveryExhaustion, waveOrdinal?: number): void {
    if (exhaustion === 'session-attempt-budget' || exhaustion === 'session-elapsed-budget') {
      const reason = exhaustion
      this.#setState(Object.freeze({ kind: 'session-exhausted', reason }))
      this.#emit(() => Object.freeze({ stage: 'session-budget-exhausted', reason }))
      return
    }
    const reason = exhaustion
    this.#setState(Object.freeze({ kind: 'quiescent', reason }))
    this.#emit(() => Object.freeze({
      stage: 'wave-quiesced',
      waveOrdinal: waveOrdinal ?? this.#waveOrdinal,
      reason,
    }))
  }

  #stopPath(reason: Extract<
    V2PeerFailureDecision,
    { readonly type: 'stop-path' }
  >['reason']): void {
    if (this.#state.kind === 'path-stopped' && this.#state.reason === reason) return
    this.#pendingWave = undefined
    this.#setState(Object.freeze({ kind: 'path-stopped', reason }))
    this.#emit(() => Object.freeze({ stage: 'path-stopped', reason }))
    this.#safeCancel(this.#currentAttempt, 'runtime-stop')
    this.#waveController?.abort('runtime-stop')
  }

  #stopSession(terminal: V2ProtocolSessionTerminalSnapshot): void {
    if (this.#state.kind === 'session-stopped' && this.#state.reason === terminal.code) return
    this.#unsubscribeRearm?.()
    this.#unsubscribeRearm = undefined
    this.#pendingWave = undefined
    this.#setState(Object.freeze({ kind: 'session-stopped', reason: terminal.code }))
    this.#emit(() => Object.freeze({ stage: 'session-stopped', reason: terminal.code }))
  }

  #cancelWave(owner: V2PeerAttemptCancellationOwner): void {
    this.#safeCancel(this.#currentAttempt, owner)
    this.#waveController?.abort(owner)
  }

  #safeCancel(attempt: V2PeerAttemptHandle | undefined, owner: V2PeerAttemptCancellationOwner): void {
    try {
      attempt?.cancel(owner)
    } catch {
      // Attempt cleanup is joined below; a throwing adapter cannot change policy authority.
    }
  }

  #matchesCurrentLane(detachment: V2PeerLaneDetachment): boolean {
    return equalV2DiagnosticIdentities(detachment.protocolSessionId, this.#protocolSessionId) &&
      equalV2DiagnosticIdentities(detachment.peerPathId, this.#peerPathId) &&
      detachment.laneId === this.#currentLane?.laneId &&
      detachment.laneEpoch === this.#currentLane.laneEpoch
  }

  #setState(state: V2PeerRecoveryState): void {
    this.#state = state
  }

  #emit(
    createPayload: () => V2PeerRecoveryTracePayload,
    attemptId?: V2PeerAttemptIdentity,
    lane?: { readonly laneId: number; readonly laneEpoch: number },
  ): void {
    try {
      const observer = this.#trace?.current
      if (observer === undefined) return
      const correlation: FailureCorrelation = Object.freeze({
        protocolSessionId: this.#protocolSessionId,
        peerPathId: this.#peerPathId,
        ...(attemptId === undefined ? {} : { peerAttemptId: attemptId }),
        ...(lane === undefined
          ? {}
          : { lane: Object.freeze({ id: lane.laneId, epoch: lane.laneEpoch }) }),
      })
      observer(Object.freeze({
        eventName: 'peer_recovery',
        correlation,
        ...createPayload(),
      }) as V2PeerRecoveryTraceEvent)
    } catch {
      // Trace loss cannot perturb retry, budget, or session authority.
    }
  }
}

export class BrowserV2PeerRecoveryRearmSource implements V2PeerRecoveryRearmSource {
  subscribe(listener: () => void): () => void {
    const notify = () => {
      try {
        listener()
      } catch {
        // Browser event delivery must remain outside recovery policy authority.
      }
    }
    window.addEventListener('online', notify)
    const connection = browserNetworkConnection()
    connection?.addEventListener('change', notify)
    return () => {
      window.removeEventListener('online', notify)
      connection?.removeEventListener('change', notify)
    }
  }
}

function browserNetworkConnection(): EventTarget | undefined {
  return (navigator as Navigator & { readonly connection?: EventTarget }).connection
}
