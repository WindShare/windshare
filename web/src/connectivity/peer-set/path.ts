import { racePeerAttempt, cancelPeerAttempt } from './attempt-race'
import type { PeerSlotLease } from '../../session/peer-slots/slots'
import { PeerRecoveryTrace } from './trace'
import { PeerActivityClock, PeerAttemptBudget } from './budget'
import { PeerRetryBackoff, type PeerRetryDelay } from './backoff'
import { PeerProfilePlanner } from './profile-planner'
import { PeerRecoveryWave as WaveBudget, type PeerPathNotice } from './wave'
import type { PeerProviderFact } from './provider-facts'
import {
  createPeerPathIdentityValue,
  createProtocolSessionIdentity,
  equalDiagnosticIdentities,
  type PeerAttemptIdentity,
  type PeerPathIdentity,
  type ProtocolSessionIdentity,
} from '../../session/identities'
import type {
  PeerRecoveryWaveTrigger,
} from '../diagnostics'
import {
  isAttemptHandle,
} from './attempt-result'
import {
  classifyPeerAttemptFailure,
  type PeerAdmittedLane,
  type PeerAttemptCancellationOwner,
  type PeerAttemptPhase,
  type PeerAttemptResult,
  type PeerAttemptFailure,
  type PeerFailureDecision,
  type ProtocolSessionTerminalSnapshot,
} from '../peer-failure'
import {
  browserPeerRecoveryClock,
  createPeerRecoveryPolicy,
  PEER_BUDGET_REFILL_MILLISECONDS,
  PEER_BROWSE_RETENTION_MILLISECONDS,
  requirePeerRecoveryCorrelation,
  type PeerAttemptContext,
  type PeerAttemptHandle,
  type PeerLaneDetachment,
  type PeerRecoveryActivation,
  type PeerRecoveryAttemptFactory,
  type PeerRecoveryAttemptRace as AttemptRace,
  type PeerRecoveryClock,
  type PeerRecoveryExhaustion as RecoveryExhaustion,
  type PeerRecoveryPendingWave as PendingWave,
  type PeerRecoveryPolicy,
  type PeerRecoveryRearmSource,
  type PeerRecoveryState,
  type PeerRetryReason,
  type PeerRecoverySupervisorOptions,
} from './contract'

export * from './contract'


export { BrowserPeerRecoveryRearmSource } from './rearm'

export class PeerRecoverySupervisor {
  readonly #protocolSessionId: ProtocolSessionIdentity
  readonly #peerPathId: PeerPathIdentity
  readonly #purpose: import('../signaling-codec').ConnectionPurpose
  readonly #attempts: PeerRecoveryAttemptFactory
  readonly #slot: PeerSlotLease
  readonly #policy: PeerRecoveryPolicy
  readonly #clock: PeerRecoveryClock
  readonly #backoff: PeerRetryBackoff
  readonly #trace: PeerRecoveryTrace
  readonly #budget: PeerAttemptBudget
  readonly #profiles: PeerProfilePlanner
  readonly #onUnavailable: () => void
  readonly #observeState: ((state: PeerRecoveryState) => void) | undefined
  #lastNetworkHintAt = Number.NEGATIVE_INFINITY
  readonly #releaseLane: ((lane: PeerAdmittedLane) => void) | undefined
  #activeWave: WaveBudget | undefined
  #browseCount = 0
  #prewarmed = false
  #idleTimer: AbortController | undefined
  #rearmTimer: AbortController | undefined
  #state: PeerRecoveryState = Object.freeze({ kind: 'idle' })
  #task: Promise<void> | undefined
  #waveController: AbortController | undefined
  #currentAttempt: PeerAttemptHandle | undefined
  #currentLane: PeerAdmittedLane | undefined
  #detachedAdmission: (PeerLaneDetachment & { readonly attemptId: PeerAttemptIdentity }) | undefined
  #unsubscribeRearm: (() => void) | undefined
  #activationCount = 0
  #sessionAttempts = 0
  readonly #activity: PeerActivityClock
  #waveOrdinal = 0
  #terminal: ProtocolSessionTerminalSnapshot | undefined
  #pendingWave: PendingWave | undefined

  constructor(options: PeerRecoverySupervisorOptions) {
    requirePeerRecoveryCorrelation(options.protocolSessionId, options.peerPathId)
    this.#protocolSessionId = createProtocolSessionIdentity(options.protocolSessionId.copyBytes())
    this.#peerPathId = createPeerPathIdentityValue(options.peerPathId.copyBytes())
    this.#purpose = options.purpose ?? 'initial'
    this.#attempts = options.attempts
    this.#slot = options.slot
    this.#policy = createPeerRecoveryPolicy(this.#purpose === 'throughput'
      ? { ...options.policy, waveMaxAttempts: 1 } : options.policy)
    this.#clock = options.clock ?? browserPeerRecoveryClock
    this.#activity = new PeerActivityClock(() => this.#clock.now())
    this.#backoff = new PeerRetryBackoff(this.#policy, options.random ?? Math.random)
    this.#trace = new PeerRecoveryTrace(this.#protocolSessionId, this.#peerPathId, options.trace)
    this.#profiles = new PeerProfilePlanner(options.endpoints, options.network)
    this.#onUnavailable = options.onUnavailable ?? (() => undefined)
    this.#observeState = options.observeState
    this.#budget = (this.#purpose === 'throughput' ? undefined : options.budget) ?? new PeerAttemptBudget(this.#policy.sessionMaxAttempts,
      this.#policy.sessionActiveElapsedBudgetMilliseconds, PEER_BUDGET_REFILL_MILLISECONDS)
    this.#releaseLane = options.releaseLane
    this.#subscribeRearm(options.rearmSource)
  }

  get state(): PeerRecoveryState {
    return this.#state
  }

  #setState(state: PeerRecoveryState): void {
    this.#state = Object.freeze(state)
    try { this.#observeState?.(this.#state) } catch { /* Presentation cannot own recovery. */ }
  }

  get sessionAttemptCount(): number {
    return this.#sessionAttempts
  }

  get sessionActiveElapsedMilliseconds(): number {
    return this.#activity.elapsed()
  }

  activate(): PeerRecoveryActivation {
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
    this.#profiles.networkChanged(this.#activity.now())
    if (this.#task !== undefined) this.pathChanged('network-changed')
    else if (this.#activationCount > 0 && this.#terminal === undefined &&
      (this.#state.kind === 'quiescent' || this.#state.kind === 'session-exhausted')) {
      this.#scheduleWave('network-change', true)
    }
  }

  pathChanged(notice: PeerPathNotice = 'mapping-ready'): void {
    const now = this.#activity.now()
    if (now === this.#lastNetworkHintAt || this.#activationCount === 0 ||
      this.#terminal !== undefined || this.#state.kind === 'admitted' ||
      this.#state.kind === 'path-stopped') return
    this.#lastNetworkHintAt = now
    this.#activeWave?.notice(notice)
  }

  /** Browsing owns one bounded opportunity, and content takes over that same attempt. */
  browse(): PeerRecoveryActivation {
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

  peerDetached(detachment: PeerLaneDetachment): boolean {
    if (this.#state.kind === 'attempting' && detachment.attemptId !== undefined &&
      this.#currentAttempt !== undefined &&
      equalDiagnosticIdentities(detachment.protocolSessionId, this.#protocolSessionId) &&
      equalDiagnosticIdentities(detachment.peerPathId, this.#peerPathId) &&
      equalDiagnosticIdentities(detachment.attemptId, this.#currentAttempt.attemptId)) {
      // Publication can synchronously retire a lane before its attempt promise settles.
      // Keep that exact admission's terminal fact until the owner consumes the result.
      this.#detachedAdmission ??= { ...detachment, attemptId: detachment.attemptId }
      return true
    }
    if (this.#state.kind !== 'admitted' || !this.#matchesCurrentLane(detachment)) return false
    this.#currentLane = undefined
    this.#trace.emit(
      () => Object.freeze({ stage: 'peer-detached' }),
      undefined,
      detachment,
    )
    if (this.#activationCount === 0) {
      this.#setState({ kind: 'idle' })
    } else {
      this.#scheduleWave('detachment', false)
    }
    return true
  }

  async sessionTerminated(terminal: ProtocolSessionTerminalSnapshot): Promise<void> {
    if (this.#terminal !== undefined) {
      await this.join().catch(() => undefined)
      this.#stopSession(this.#terminal)
      return
    }
    const decision = classifyPeerAttemptFailure({ kind: 'session-terminal', terminal })
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
    })).finally(() => this.#slot.close())
  }

  join(): Promise<void> {
    return this.#task ?? Promise.resolve()
  }

  #subscribeRearm(source: PeerRecoveryRearmSource | undefined): void {
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
    if (this.#state.kind === 'quiescent') this.#setState({ kind: 'idle' })
  }

  #scheduleWave(trigger: PeerRecoveryWaveTrigger, rearmed: boolean): void {
    if (this.#terminal !== undefined) return
    if (this.#purpose === 'throughput' && this.#sessionAttempts > 0) {
      this.#exhaust('wave-attempt-budget')
      return
    }
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

  async #runWave(trigger: PeerRecoveryWaveTrigger, rearmed: boolean): Promise<void> {
    const controller = new AbortController()
    this.#waveController = controller
    const wave = new WaveBudget(++this.#waveOrdinal, this.#activity.now(), this.#policy, this.#budget)
    this.#activeWave = wave
    this.#activity.begin(wave.startedAt)
    this.#trace.emit(() => Object.freeze({ stage: 'wave-started', waveOrdinal: wave.ordinal, trigger }))
    if (rearmed && trigger !== 'detachment') {
      this.#trace.emit(() => Object.freeze({ stage: 'wave-rearmed', waveOrdinal: wave.ordinal, trigger }))
    }

    try {
      await this.#attemptWave(wave, controller.signal)
    } catch {
      this.#stopPath('local-contract')
    } finally {
      wave.release(this.#activity.now())
      this.#activeWave = undefined
      this.#activity.pause()
      if (this.#waveController === controller) this.#waveController = undefined
      this.#currentAttempt = undefined
    }
  }

  async #attemptWave(wave: WaveBudget, signal: AbortSignal): Promise<void> {
    let retryOrdinal = 0
    let previousAttemptId: PeerAttemptIdentity | undefined
    if (!await this.#waitForRetainedNotBefore(wave, signal)) return

    while (!signal.aborted && this.#terminal === undefined && this.#hasAttemptDemand(wave)) {
      const exhaustion = wave.exhaustion(this.#activity.now(), true)
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

  #reserveAttempt(wave: WaveBudget): PeerAttemptHandle | undefined {
    if (!this.#budget.takeAttempt(this.#activity.now())) return undefined
    this.#detachedAdmission = undefined
    wave.attempts += 1
    this.#sessionAttempts += 1
    const waveAttemptOrdinal = wave.attempts
    const sessionAttemptOrdinal = this.#sessionAttempts
    const startedAt = this.#activity.now()
    const iceProfile = this.#profiles.select(wave.ordinal, sessionAttemptOrdinal, startedAt)
    const context: PeerAttemptContext = Object.freeze({
      ...this.#slot.next(),
      purpose: this.#purpose === 'initial' && sessionAttemptOrdinal > 1 ? 'recovery' : this.#purpose,
      admissionWaitBudgetMilliseconds: Math.max(0, Math.min(
        wave.remaining(this.#activity.now()).wave, wave.remaining(this.#activity.now()).session) - Math.min(
        this.#policy.waveElapsedBudgetMilliseconds,
        this.#policy.negotiationBudgetMilliseconds + this.#policy.admissionBudgetMilliseconds)),
      iceProfile,
      observeICEFact: (fact: PeerProviderFact) => this.#profiles.observe(iceProfile, fact, startedAt, this.#activity.now()),
      protocolSessionId: this.#protocolSessionId,
      peerPathId: this.#peerPathId,
      waveOrdinal: wave.ordinal,
      waveAttemptOrdinal,
      sessionAttemptOrdinal,
      negotiationBudgetMilliseconds: this.#policy.negotiationBudgetMilliseconds,
      admissionBudgetMilliseconds: this.#policy.admissionBudgetMilliseconds,
      phaseChanged: (phase: PeerAttemptPhase) => this.#attemptPhaseChanged(
        wave.ordinal,
        waveAttemptOrdinal,
        sessionAttemptOrdinal,
        phase,
      ),
    })
    this.#setState({
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
    phase: PeerAttemptPhase,
  ): void {
    if (phase !== 'negotiation' && phase !== 'admission') return
    if (
      this.#state.kind !== 'attempting' ||
      this.#state.waveOrdinal !== waveOrdinal ||
      this.#state.waveAttemptOrdinal !== waveAttemptOrdinal ||
      this.#state.sessionAttemptOrdinal !== sessionAttemptOrdinal
    ) return
    this.#setState({ ...this.#state, phase })
  }

  #raceAttempt(attempt: PeerAttemptHandle, wave: WaveBudget, signal: AbortSignal): Promise<AttemptRace> {
    return racePeerAttempt(attempt, this.#clock, wave.remaining(this.#activity.now()), signal,
      (candidate, owner) => cancelPeerAttempt(candidate, owner))
  }

  async #settleAttemptResult(
    result: PeerAttemptResult,
    attemptId: PeerAttemptIdentity,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    this.#currentAttempt = undefined
    if (result.type === 'admitted') return this.#settleAdmission(result.lane, attemptId, signal)
    if (result.type === 'lifecycle-cancelled') {
      this.#settleLifecycleCancellation(result.owner)
      return false
    }

    const decision = classifyPeerAttemptFailure(result.failure)
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
    switch (decision.type) {
      case 'stop-path':
        this.#stopPath(decision.reason)
        return false
      case 'stop-session':
        this.#terminal = decision.terminal
        this.#stopSession(decision.terminal)
        return false
    }
    if (this.#purpose === 'throughput') {
      this.#exhaust('wave-attempt-budget', wave.ordinal)
      return false
    }
    if (decision.reason === 'resource-deferred') {
      // The sender's bounded resource wait is not a failed negotiation. Elapsed
      // wave/session budgets and the tab's actual-start limits still apply.
      wave.attempts -= 1
      this.#budget.refundUnstartedAttempt()
    }
    if (this.#activationCount === 0) {
      this.#setState({ kind: 'idle' })
      return false
    }
    return this.#scheduleRetry(decision, result.failure, attemptId, wave, retryOrdinal, signal)
  }

  #settleAdmission(lane: PeerAdmittedLane, attemptId: PeerAttemptIdentity, signal: AbortSignal): false {
      if (this.#admissionObsolete(signal)) {
        this.#releaseLane?.(lane)
        this.#settleOwnerCancellation()
        return false
      }
      const detached = this.#detachedAdmission
      if (detached !== undefined && equalDiagnosticIdentities(detached.attemptId, attemptId) &&
        detached.laneId === lane.laneId && detached.laneEpoch === lane.laneEpoch) {
        this.#setState({ kind: 'idle' })
        this.#trace.emit(() => Object.freeze({ stage: 'peer-detached' }), attemptId, detached)
        if (this.#activationCount > 0) this.#scheduleWave('detachment', false)
        return false
      }
      this.#currentLane = Object.freeze({ ...lane })
      this.#setState({ kind: 'admitted', lane: this.#currentLane })
      if (this.#activationCount === 0) this.#retainIdleLane()
      return false
  }

  #admissionObsolete(signal: AbortSignal): boolean {
    return this.#terminal !== undefined || this.#state.kind === 'path-stopped' || signal.aborted
  }

  async #scheduleRetry(
    decision: Extract<PeerFailureDecision, { readonly type: 'retry-attempt' }>,
    failure: PeerAttemptFailure,
    attemptId: PeerAttemptIdentity,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    const now = this.#activity.now()
    this.#backoff.retain(decision, now)
    const exhaustion = wave.exhaustion(this.#activity.now(), true)
    if (exhaustion !== undefined) {
      this.#exhaust(exhaustion, wave.ordinal)
      return false
    }

    let backoff: PeerRetryDelay
    try {
      backoff = this.#backoff.schedule(now, retryOrdinal, wave.delayedOpportunity(now))
    } catch {
      this.#stopPath('local-contract')
      return false
    }
    this.#trace.emit(
      () => Object.freeze({
        stage: 'backoff-scheduled',
        waveOrdinal: wave.ordinal,
        retryOrdinal,
        ...backoff,
      }),
      attemptId,
    )
    const reason = failure.kind === 'local-transient' ? failure.reason : decision.reason
    return this.#waitForRetry(backoff.effectiveDelayMilliseconds, reason, wave, retryOrdinal, signal)
  }

  async #waitForRetainedNotBefore(wave: WaveBudget, signal: AbortSignal): Promise<boolean> {
    const delay = this.#backoff.retainedDelay(this.#activity.now())
    if (delay === 0) return true
    return this.#waitForRetry(delay, 'retained-backoff', wave, 0, signal)
  }

  async #waitForRetry(
    delay: number,
    reason: PeerRetryReason,
    wave: WaveBudget,
    retryOrdinal: number,
    signal: AbortSignal,
  ): Promise<boolean> {
    const remaining = wave.remaining(this.#activity.now())
    if (delay > remaining.wave || delay > remaining.session) {
      this.#exhaust(
        remaining.session <= remaining.wave
          ? 'session-elapsed-budget'
          : 'wave-elapsed-budget',
        wave.ordinal,
      )
      return false
    }
    this.#setState({
      kind: 'waiting-retry',
      waveOrdinal: wave.ordinal,
      retryOrdinal,
      delayMilliseconds: delay,
      reason,
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
    const exhaustion = wave.exhaustion(this.#activity.now())
    if (exhaustion !== undefined) {
      this.#exhaust(exhaustion, wave.ordinal)
      return false
    }
    return true
  }

  #settleLifecycleCancellation(owner: PeerAttemptCancellationOwner): void {
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
      this.#setState({ kind: 'admitted', lane: this.#currentLane })
    } else if (this.#state.kind !== 'path-stopped' && this.#state.kind !== 'session-exhausted') {
      this.#setState({ kind: 'idle' })
    }
  }

  #sessionExhaustion(): RecoveryExhaustion | undefined {
    if (this.#budget.available(this.#activity.now()).attempts === 0) {
      return 'session-attempt-budget'
    }
    return this.#budget.available(this.#activity.now()).elapsedMilliseconds < Math.min(
      this.#policy.waveElapsedBudgetMilliseconds,
      this.#policy.negotiationBudgetMilliseconds + this.#policy.admissionBudgetMilliseconds)
      ? 'session-elapsed-budget'
      : undefined
  }

  #exhaust(exhaustion: RecoveryExhaustion, waveOrdinal?: number): void {
    if (exhaustion === 'session-attempt-budget' || exhaustion === 'session-elapsed-budget') {
      const reason = exhaustion
      this.#setState({ kind: 'session-exhausted', reason })
      this.#trace.emit(() => Object.freeze({ stage: 'session-budget-exhausted', reason }))
      this.#onUnavailable()
      this.#rearmWhenAvailable()
      return
    }
    const reason = exhaustion
    this.#setState({ kind: 'quiescent', reason })
    this.#onUnavailable()
    this.#trace.emit(() => Object.freeze({
      stage: 'wave-quiesced',
      waveOrdinal: waveOrdinal ?? this.#waveOrdinal,
      reason,
    }))
    this.#rearmWhenAvailable()
  }

  #rearmWhenAvailable(): void {
    if (this.#purpose === 'throughput' || this.#activationCount === 0 || this.#terminal !== undefined) return
    this.#rearmTimer?.abort()
    const timer = new AbortController()
    this.#rearmTimer = timer
    // A new wave is intentionally infrequent; a network change remains an immediate hint.
    const delay = Math.max(30_000, this.#budget.nextAttemptDelay(this.#activity.now(),
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
    this.#clock.sleep(PEER_BROWSE_RETENTION_MILLISECONDS, timer.signal).then(() => {
      if (!timer.signal.aborted && this.#activationCount === 0) this.#releaseIdleLane()
    }).catch(() => undefined)
  }

  #releaseIdleLane(): void {
    this.#idleTimer?.abort()
    const lane = this.#currentLane
    this.#currentLane = undefined
    if (lane !== undefined) this.#releaseLane?.(lane)
    if (this.#terminal === undefined && this.#state.kind === 'admitted') {
      this.#setState({ kind: 'idle' })
    }
  }

  #stopPath(reason: Extract<
    PeerFailureDecision,
    { readonly type: 'stop-path' }
  >['reason']): void {
    if (this.#state.kind === 'path-stopped' && this.#state.reason === reason) return
    this.#pendingWave = undefined
    this.#setState({ kind: 'path-stopped', reason })
    this.#onUnavailable()
    this.#trace.emit(() => Object.freeze({ stage: 'path-stopped', reason }))
    cancelPeerAttempt(this.#currentAttempt, 'runtime-stop')
    this.#waveController?.abort('runtime-stop')
  }

  #stopSession(terminal: ProtocolSessionTerminalSnapshot): void {
    if (this.#state.kind === 'session-stopped' && this.#state.reason === terminal.code) return
    this.#unsubscribeRearm?.()
    this.#unsubscribeRearm = undefined
    this.#pendingWave = undefined
    this.#setState({ kind: 'session-stopped', reason: terminal.code })
    this.#trace.emit(() => Object.freeze({ stage: 'session-stopped', reason: terminal.code }))
  }

  #cancelWave(owner: PeerAttemptCancellationOwner): void {
    cancelPeerAttempt(this.#currentAttempt, owner)
    this.#waveController?.abort(owner)
  }

  #matchesCurrentLane(detachment: PeerLaneDetachment): boolean {
    return equalDiagnosticIdentities(detachment.protocolSessionId, this.#protocolSessionId) &&
      equalDiagnosticIdentities(detachment.peerPathId, this.#peerPathId) &&
      detachment.laneId === this.#currentLane?.laneId &&
      detachment.laneEpoch === this.#currentLane.laneEpoch
  }

}
