import { racePeerAttempt, cancelPeerAttempt } from './attempt-race'
import { PeerRecoveryTrace } from './trace'
import { PeerAttemptBudget } from './budget'
import { PeerProfilePlanner } from './profile-planner'
import { PeerRecoveryWave as WaveBudget, type PeerPathNotice } from './wave'
import type { PeerProviderFact } from './provider-facts'
import {
  createV2PeerPathIdentityValue,
  createV2ProtocolSessionIdentity,
  equalV2DiagnosticIdentities,
  type V2PeerAttemptIdentity,
  type V2PeerPathIdentity,
  type V2ProtocolSessionIdentity,
} from '../../session/v2-identities'
import type {
  V2PeerRecoveryWaveTrigger,
} from '../diagnostics'
import {
  isAttemptHandle,
} from './attempt-result'
import {
  classifyV2PeerAttemptFailure,
  type V2PeerAdmittedLane,
  type V2PeerAttemptCancellationOwner,
  type V2PeerAttemptPhase,
  type V2PeerAttemptResult,
  type V2PeerFailureDecision,
  type V2ProtocolSessionTerminalSnapshot,
} from '../v2-peer-failure'
import {
  browserV2PeerRecoveryClock,
  calculateV2PeerRetryDelay,
  createV2PeerRecoveryPolicy,
  V2_PEER_BUDGET_REFILL_MILLISECONDS,
  V2_PEER_BROWSE_RETENTION_MILLISECONDS,
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
} from './contract'

export * from './contract'


export { BrowserV2PeerRecoveryRearmSource } from './rearm'

export class V2PeerRecoverySupervisor {
  readonly #protocolSessionId: V2ProtocolSessionIdentity
  readonly #peerPathId: V2PeerPathIdentity
  readonly #attempts: V2PeerRecoveryAttemptFactory
  readonly #policy: V2PeerRecoveryPolicy
  readonly #clock: V2PeerRecoveryClock
  readonly #random: () => number
  readonly #trace: PeerRecoveryTrace
  readonly #budget: PeerAttemptBudget
  readonly #profiles: PeerProfilePlanner
  readonly #onUnavailable: () => void
  #lastNetworkHintAt = Number.NEGATIVE_INFINITY
  readonly #releaseLane: ((lane: V2PeerAdmittedLane) => void) | undefined
  #activeWave: WaveBudget | undefined
  #browseCount = 0
  #prewarmed = false
  #idleTimer: AbortController | undefined
  #rearmTimer: AbortController | undefined
  #state: V2PeerRecoveryState = Object.freeze({ kind: 'idle' })
  #task: Promise<void> | undefined
  #waveController: AbortController | undefined
  #currentAttempt: V2PeerAttemptHandle | undefined
  #currentLane: V2PeerAdmittedLane | undefined
  #detachedAdmission: (V2PeerLaneDetachment & { readonly attemptId: V2PeerAttemptIdentity }) | undefined
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
    this.#trace = new PeerRecoveryTrace(this.#protocolSessionId, this.#peerPathId, options.trace)
    this.#profiles = new PeerProfilePlanner(options.endpoints, options.network)
    this.#onUnavailable = options.onUnavailable ?? (() => undefined)
    this.#budget = options.budget ?? new PeerAttemptBudget(this.#policy.sessionMaxAttempts,
      this.#policy.sessionActiveElapsedBudgetMilliseconds, V2_PEER_BUDGET_REFILL_MILLISECONDS)
    this.#releaseLane = options.releaseLane
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
    this.#idleTimer?.abort()
    const wasInactive = this.#activationCount === 0
    this.#activationCount += 1
    if (this.#terminal !== undefined || this.#state.kind === 'path-stopped' ||
      this.#state.kind === 'session-stopped') this.#onUnavailable()
    const shouldRearm = this.#state.kind === 'quiescent' || this.#state.kind === 'session-exhausted'
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
    this.#profiles.networkChanged(this.#now())
    if (this.#task !== undefined) this.pathChanged('network-changed')
    else if (this.#activationCount > 0 && this.#terminal === undefined &&
      (this.#state.kind === 'quiescent' || this.#state.kind === 'session-exhausted')) {
      this.#scheduleWave('network-change', true)
    }
  }

  pathChanged(notice: PeerPathNotice = 'mapping-ready'): void {
    const now = this.#now()
    if (now === this.#lastNetworkHintAt || this.#activationCount === 0 ||
      this.#terminal !== undefined || this.#state.kind === 'admitted' ||
      this.#state.kind === 'path-stopped') return
    this.#lastNetworkHintAt = now
    this.#activeWave?.notice(notice)
  }

  /** Browsing owns one bounded opportunity, and content takes over that same attempt. */
  browse(): V2PeerRecoveryActivation {
    this.#browseCount += 1
    if (!this.#prewarmed && this.#state.kind === 'idle') {
      this.#prewarmed = true
      this.#scheduleWave('activation', false)
    }
    let closed = false
    return Object.freeze({ close: () => {
      if (closed) return
      closed = true
      this.#browseCount -= 1
      if (this.#browseCount === 0 && this.#activationCount === 0) {
        this.#cancelWave('last-activation')
        this.#releaseIdleLane()
      }
    } })
  }

  peerDetached(detachment: V2PeerLaneDetachment): boolean {
    if (this.#state.kind === 'attempting' && detachment.attemptId !== undefined &&
      this.#currentAttempt !== undefined &&
      equalV2DiagnosticIdentities(detachment.protocolSessionId, this.#protocolSessionId) &&
      equalV2DiagnosticIdentities(detachment.peerPathId, this.#peerPathId) &&
      equalV2DiagnosticIdentities(detachment.attemptId, this.#currentAttempt.attemptId)) {
      // Publication can synchronously retire a lane before its attempt promise settles.
      // Keep that exact admission's terminal fact until the owner consumes the result.
      this.#detachedAdmission ??= { ...detachment, attemptId: detachment.attemptId }
      return true
    }
    if (this.#state.kind !== 'admitted' || !this.#matchesCurrentLane(detachment)) return false
    this.#preferredLaneId = detachment.laneId
    this.#currentLane = undefined
    this.#trace.emit(
      () => Object.freeze({ stage: 'peer-detached' }),
      undefined,
      detachment,
    )
    if (this.#activationCount === 0) {
      this.#state = Object.freeze({ kind: 'idle' })
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
    this.#idleTimer?.abort()
    this.#rearmTimer?.abort()
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
    if (this.#activationCount > 0) return
    if (this.#state.kind === 'admitted') {
      this.#retainIdleLane()
      return
    }
    if (this.#browseCount > 0) return
    if (this.#state.kind === 'attempting' || this.#state.kind === 'waiting-retry') {
      this.#cancelWave('last-activation')
      return
    }
    if (this.#state.kind === 'quiescent') this.#state = Object.freeze({ kind: 'idle' })
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
      this.#state.kind === 'path-stopped' ||
      this.#state.kind === 'session-stopped'
    ) return
    this.#scheduleWave(pending.trigger, pending.rearmed)
  }

  async #runWave(trigger: V2PeerRecoveryWaveTrigger, rearmed: boolean): Promise<void> {
    const controller = new AbortController()
    this.#waveController = controller
    const wave = new WaveBudget(++this.#waveOrdinal, this.#now(), this.#policy, this.#budget)
    this.#activeWave = wave
    this.#activeSince = wave.startedAt
    this.#trace.emit(() => Object.freeze({ stage: 'wave-started', waveOrdinal: wave.ordinal, trigger }))
    if (rearmed && trigger !== 'detachment') {
      this.#trace.emit(() => Object.freeze({ stage: 'wave-rearmed', waveOrdinal: wave.ordinal, trigger }))
    }

    try {
      await this.#attemptWave(wave, controller.signal)
    } catch {
      this.#stopPath('local-contract')
    } finally {
      wave.release(this.#now())
      this.#activeWave = undefined
      this.#pauseSessionElapsed()
      if (this.#waveController === controller) this.#waveController = undefined
      this.#currentAttempt = undefined
    }
  }

  async #attemptWave(wave: WaveBudget, signal: AbortSignal): Promise<void> {
    let retryOrdinal = 0
    let previousAttemptId: V2PeerAttemptIdentity | undefined
    if (!await this.#waitForRetainedNotBefore(wave, signal)) return

    while (!signal.aborted && this.#terminal === undefined && this.#hasAttemptDemand(wave)) {
      const exhaustion = wave.exhaustion(this.#now(), true)
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
        this.#trace.emit(
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

  #hasAttemptDemand(wave: WaveBudget): boolean {
    return this.#activationCount > 0 || (this.#browseCount > 0 && wave.attempts === 0)
  }

  #reserveAttempt(wave: WaveBudget): V2PeerAttemptHandle | undefined {
    if (!this.#budget.takeAttempt(this.#now())) return undefined
    this.#detachedAdmission = undefined
    wave.attempts += 1
    this.#sessionAttempts += 1
    const waveAttemptOrdinal = wave.attempts
    const sessionAttemptOrdinal = this.#sessionAttempts
    const startedAt = this.#now()
    const iceProfile = this.#profiles.select(wave.ordinal, sessionAttemptOrdinal, startedAt)
    const context: V2PeerAttemptContext = Object.freeze({
      admissionWaitBudgetMilliseconds: Math.max(0, Math.min(
        wave.remaining(this.#now()).wave, wave.remaining(this.#now()).session) - Math.min(
        this.#policy.waveElapsedBudgetMilliseconds,
        this.#policy.negotiationBudgetMilliseconds + this.#policy.admissionBudgetMilliseconds)),
      iceProfile,
      observeICEFact: (fact: PeerProviderFact) => this.#profiles.observe(iceProfile, fact, startedAt, this.#now()),
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
    this.#state = Object.freeze({
      kind: 'attempting',
      phase: 'negotiation',
      waveOrdinal: wave.ordinal,
      waveAttemptOrdinal,
      sessionAttemptOrdinal,
    })

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
    this.#state = Object.freeze({ ...this.#state, phase })
  }

  #raceAttempt(attempt: V2PeerAttemptHandle, wave: WaveBudget, signal: AbortSignal): Promise<AttemptRace> {
    return racePeerAttempt(attempt, this.#clock, wave.remaining(this.#now()), signal,
      (candidate, owner) => cancelPeerAttempt(candidate, owner))
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
      if (this.#terminal !== undefined || this.#state.kind === 'path-stopped' || signal.aborted) {
        this.#releaseLane?.(result.lane)
        this.#settleOwnerCancellation()
        return false
      }
      const detached = this.#detachedAdmission
      if (detached !== undefined && equalV2DiagnosticIdentities(detached.attemptId, attemptId) &&
        detached.laneId === result.lane.laneId && detached.laneEpoch === result.lane.laneEpoch) {
        this.#preferredLaneId = detached.laneId
        this.#state = Object.freeze({ kind: 'idle' })
        this.#trace.emit(() => Object.freeze({ stage: 'peer-detached' }), attemptId, detached)
        if (this.#activationCount > 0) this.#scheduleWave('detachment', false)
        return false
      }
      this.#currentLane = Object.freeze({ ...result.lane })
      this.#preferredLaneId = result.lane.laneId
      this.#state = Object.freeze({ kind: 'admitted', lane: this.#currentLane })
      if (this.#activationCount === 0) this.#retainIdleLane()
      return false
    }
    if (result.type === 'lifecycle-cancelled') {
      this.#settleLifecycleCancellation(result.owner)
      return false
    }

    const decision = classifyV2PeerAttemptFailure(result.failure)
    this.#trace.emit(
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
    if (this.#activationCount === 0) {
      this.#state = Object.freeze({ kind: 'idle' })
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
    const exhaustion = wave.exhaustion(this.#now(), true)
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
    const delay = Math.max(localDelay, authenticatedDelay, wave.delayedOpportunity(now))
    this.#trace.emit(
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
    const remaining = wave.remaining(this.#now())
    if (delay > remaining.wave || delay > remaining.session) {
      this.#exhaust(
        remaining.session <= remaining.wave
          ? 'session-elapsed-budget'
          : 'wave-elapsed-budget',
        wave.ordinal,
      )
      return false
    }
    this.#state = Object.freeze({
      kind: 'waiting-retry',
      waveOrdinal: wave.ordinal,
      retryOrdinal,
      delayMilliseconds: delay,
    })
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
    const exhaustion = wave.exhaustion(this.#now())
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
      this.#state = Object.freeze({ kind: 'admitted', lane: this.#currentLane })
    } else if (this.#state.kind !== 'path-stopped' && this.#state.kind !== 'session-exhausted') {
      this.#state = Object.freeze({ kind: 'idle' })
    }
  }

  #sessionExhaustion(): RecoveryExhaustion | undefined {
    if (this.#budget.available(this.#now()).attempts === 0) {
      return 'session-attempt-budget'
    }
    return this.#budget.available(this.#now()).elapsedMilliseconds < Math.min(
      this.#policy.waveElapsedBudgetMilliseconds,
      this.#policy.negotiationBudgetMilliseconds + this.#policy.admissionBudgetMilliseconds)
      ? 'session-elapsed-budget'
      : undefined
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
      this.#state = Object.freeze({ kind: 'session-exhausted', reason })
      this.#trace.emit(() => Object.freeze({ stage: 'session-budget-exhausted', reason }))
      this.#onUnavailable()
      this.#rearmWhenAvailable()
      return
    }
    const reason = exhaustion
    this.#state = Object.freeze({ kind: 'quiescent', reason })
    this.#onUnavailable()
    this.#trace.emit(() => Object.freeze({
      stage: 'wave-quiesced',
      waveOrdinal: waveOrdinal ?? this.#waveOrdinal,
      reason,
    }))
    this.#rearmWhenAvailable()
  }

  #rearmWhenAvailable(): void {
    if (this.#activationCount === 0 || this.#terminal !== undefined) return
    this.#rearmTimer?.abort()
    const timer = new AbortController()
    this.#rearmTimer = timer
    // A new wave is intentionally infrequent; a network change remains an immediate hint.
    const delay = Math.max(30_000, this.#budget.nextAttemptDelay(this.#now(),
      this.#policy.negotiationBudgetMilliseconds + this.#policy.admissionBudgetMilliseconds))
    this.#clock.sleep(delay, timer.signal).then(() => {
      if (!timer.signal.aborted && this.#activationCount > 0 &&
        (this.#state.kind === 'quiescent' || this.#state.kind === 'session-exhausted')) {
        this.#scheduleWave('activation', true)
      }
    }).catch(() => undefined)
  }

  #retainIdleLane(): void {
    this.#idleTimer?.abort()
    const timer = new AbortController()
    this.#idleTimer = timer
    this.#clock.sleep(V2_PEER_BROWSE_RETENTION_MILLISECONDS, timer.signal).then(() => {
      if (!timer.signal.aborted && this.#activationCount === 0) this.#releaseIdleLane()
    }).catch(() => undefined)
  }

  #releaseIdleLane(): void {
    this.#idleTimer?.abort()
    const lane = this.#currentLane
    this.#currentLane = undefined
    if (lane !== undefined) this.#releaseLane?.(lane)
    if (this.#terminal === undefined && this.#state.kind === 'admitted') {
      this.#state = Object.freeze({ kind: 'idle' })
    }
  }

  #stopPath(reason: Extract<
    V2PeerFailureDecision,
    { readonly type: 'stop-path' }
  >['reason']): void {
    if (this.#state.kind === 'path-stopped' && this.#state.reason === reason) return
    this.#pendingWave = undefined
    this.#state = Object.freeze({ kind: 'path-stopped', reason })
    this.#onUnavailable()
    this.#trace.emit(() => Object.freeze({ stage: 'path-stopped', reason }))
    cancelPeerAttempt(this.#currentAttempt, 'runtime-stop')
    this.#waveController?.abort('runtime-stop')
  }

  #stopSession(terminal: V2ProtocolSessionTerminalSnapshot): void {
    if (this.#state.kind === 'session-stopped' && this.#state.reason === terminal.code) return
    this.#unsubscribeRearm?.()
    this.#unsubscribeRearm = undefined
    this.#pendingWave = undefined
    this.#state = Object.freeze({ kind: 'session-stopped', reason: terminal.code })
    this.#trace.emit(() => Object.freeze({ stage: 'session-stopped', reason: terminal.code }))
  }

  #cancelWave(owner: V2PeerAttemptCancellationOwner): void {
    cancelPeerAttempt(this.#currentAttempt, owner)
    this.#waveController?.abort(owner)
  }

  #matchesCurrentLane(detachment: V2PeerLaneDetachment): boolean {
    return equalV2DiagnosticIdentities(detachment.protocolSessionId, this.#protocolSessionId) &&
      equalV2DiagnosticIdentities(detachment.peerPathId, this.#peerPathId) &&
      detachment.laneId === this.#currentLane?.laneId &&
      detachment.laneEpoch === this.#currentLane.laneEpoch
  }

}
