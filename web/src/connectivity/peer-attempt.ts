import {
  LaneAdmissionRejectedError,
  LaneAdmissionTransportError,
  LaneInstallationError,
  type LaneAdmissionResult,
  type ReceiverSessionRuntime,
} from '../session/runtime'
import {
  createPeerAttemptIdentity,
  createProtocolOperationIdentity,
  equalDiagnosticIdentities,
  type PeerAttemptIdentity,
  type PeerPathIdentity,
  type ProtocolSessionIdentity,
} from '../session/identities'
import { LaneCodecError, type LaneGrant } from '../session/lane-codec'
import { SessionRuntimeError } from '../session/runtime-types'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
  UnexpectedDataChannelError,
} from './errors'
import type { PeerChannel } from './peer-channel'
import type { OfferChannelFactory } from './peer-offer'
import {
  isLaneRejection,
  type PeerAttemptFailure,
  type PeerAttemptPhase,
  type PeerAttemptResult,
} from './peer-failure'
import type {
  PeerAttemptContext,
  PeerAttemptHandle,
  PeerRecoveryAttemptFactory,
  PeerRecoveryClock,
} from './peer-set/path'
import type { ConnectivityTraceSource } from './diagnostics'
import {
  AuthenticatedPeerOperationError,
  createPeerBindingForPath,
  SessionSignalingRoute,
} from './session-signaling'
import type { PeerBinding } from './signaling-codec'
import { PEER_SIGNALING_BUDGET_MILLISECONDS, PEER_ICE_CHECK_BUDGET_MILLISECONDS,
  PEER_DTLS_BUDGET_MILLISECONDS } from './peer-set/contract'
import type { PeerOfferAttemptObserver } from './peer-offer'
import { browserPeerTabAdmission, PeerTabCapacityTimeoutError, type PeerTabAdmission, type PeerTabAdmissionPermit } from './peer-set/tab-admission'

export interface PeerCandidatePublication {
  readonly protocolSessionId: ProtocolSessionIdentity
  readonly peerPathId: PeerPathIdentity
  readonly attemptId: PeerAttemptIdentity
  readonly peer: PeerChannel
  readonly route: SessionSignalingRoute
  readonly laneId: number
  readonly laneEpoch: number
}

export type PeerCandidatePublisher = (candidate: PeerCandidatePublication) => boolean

export interface BrowserPeerAttemptExecutorOptions {
  readonly transportRoute?: 'direct' | 'turn'
  readonly tabAdmission?: PeerTabAdmission
  readonly admissionReceiver?: object
  readonly session: ReceiverSessionRuntime
  readonly offers: OfferChannelFactory
  readonly peerPathIdentity: Uint8Array
  readonly clock: PeerRecoveryClock
  readonly publish: PeerCandidatePublisher
  readonly randomBytes?: (length: number) => Uint8Array
  readonly trace?: ConnectivityTraceSource
}

interface AttemptResources {
  route?: SessionSignalingRoute
  peer?: PeerChannel
  peerOwner:
    | Readonly<{ kind: 'attempt' | 'adoption' | 'published' }>
    | Readonly<{ kind: 'runtime'; laneId: number; laneEpoch: number }>
}

type PhaseSettlementAuthority =
  | 'local'
  | 'authenticated-admission'
  | 'authenticated-peer-operation'

type PhaseWorkSettlement<T> =
  | {
      readonly type: 'completed'
      readonly value: T
      readonly authority: 'local' | 'authenticated-admission'
    }
  | {
      readonly type: 'failed'
      readonly error: unknown
      readonly authority: PhaseSettlementAuthority
    }

type PhaseOutcome<T> =
  | { readonly type: 'work'; readonly settlement: PhaseWorkSettlement<T> }
  | { readonly type: 'deadline' }
  | { readonly type: 'clock-failed'; readonly error: unknown }

const NEVER: Promise<never> = new Promise(() => undefined)

class PeerPhaseTimeoutError extends DOMException {
  readonly phase: PeerAttemptPhase

  constructor(phase: PeerAttemptPhase) {
    super(`Peer ${phase} phase timed out`, 'TimeoutError')
    this.phase = phase
  }
}

class PhaseSettlementFailure {
  readonly settlement: Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }>

  constructor(settlement: Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }>) {
    this.settlement = settlement
  }
}

/** One handle owns one immutable identity tuple and one exactly-joined workflow. */
export class BrowserPeerAttemptExecutor implements PeerRecoveryAttemptFactory {
  readonly #session: ReceiverSessionRuntime
  readonly #offers: OfferChannelFactory
  readonly #peerPathIdentity: Uint8Array<ArrayBuffer>
  readonly #clock: PeerRecoveryClock
  readonly #publish: PeerCandidatePublisher
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #trace: ConnectivityTraceSource | undefined
  readonly #tabAdmission: PeerTabAdmission
  readonly #admissionReceiver: object
  readonly #transportRoute: 'direct' | 'turn'

  constructor(options: BrowserPeerAttemptExecutorOptions) {
    if (
      options.peerPathIdentity.byteLength !== 16 ||
      !options.peerPathIdentity.some((item) => item !== 0)
    ) throw new RangeError('peer path identity must be a nonzero 16-byte value')
    this.#session = options.session
    this.#offers = options.offers
    this.#peerPathIdentity = options.peerPathIdentity.slice()
    this.#clock = options.clock
    this.#publish = options.publish
    this.#randomBytes = options.randomBytes
    this.#trace = options.trace
    this.#tabAdmission = options.tabAdmission ?? browserPeerTabAdmission()
    this.#admissionReceiver = options.admissionReceiver ?? this
    this.#transportRoute = options.transportRoute ?? 'direct'
  }

  createAttempt(context: PeerAttemptContext): PeerAttemptHandle {
    const binding = createPeerBindingForPath(this.#peerPathIdentity, context, this.#randomBytes)
    const attemptId = createPeerAttemptIdentity(binding.attemptId)
    const controller = new AbortController()
    let cancellationOwner: Parameters<PeerAttemptHandle['cancel']>[0] | undefined
    const result = this.#run(context, binding, controller.signal, () => cancellationOwner)
    return Object.freeze({
      attemptId,
      result,
      cancel: (owner: Parameters<PeerAttemptHandle['cancel']>[0]) => {
        if (controller.signal.aborted) return
        cancellationOwner = owner
        controller.abort(owner)
      },
    })
  }

  async #run(
    context: PeerAttemptContext,
    binding: PeerBinding,
    signal: AbortSignal,
    cancellationOwner: () => Parameters<PeerAttemptHandle['cancel']>[0] | undefined,
  ): Promise<PeerAttemptResult> {
    const resources: AttemptResources = { peerOwner: { kind: 'attempt' } }
    let terminalError: unknown
    let permit: PeerTabAdmissionPermit | undefined
    try {
      signal.throwIfAborted()
      const route = new SessionSignalingRoute(
        this.#session,
        binding,
        this.#trace,
        {
          waveOrdinal: context.waveOrdinal,
          waveAttemptOrdinal: context.waveAttemptOrdinal,
          sessionAttemptOrdinal: context.sessionAttemptOrdinal,
        },
      )
      resources.route = route
      permit = await this.#tabAdmission.acquire(this.#admissionReceiver,
        this.#transportRoute === 'turn' ? 0 : context.iceProfile?.endpointIDs.length ?? 2,
        context.admissionWaitBudgetMilliseconds ?? 0, signal, this.#transportRoute, context.purpose)
      signal.throwIfAborted()
      if (context.iceProfile !== undefined) route.providerFact({ kind: 'profile',
        profileID: context.iceProfile.id, networkGenerationID: context.iceProfile.networkGenerationID,
        endpointIDs: context.iceProfile.endpointIDs.join(','), side: 'receiver' })
      const negotiation = await this.#runPhase(
        'negotiation',
        context.negotiationBudgetMilliseconds,
        signal,
        route.attemptFailureSignal,
        route,
        (phaseSignal) => settlePhaseWork(
          () => this.#negotiate(context, route, phaseSignal),
          route.attemptFailureSignal,
        ),
      )
      const peer = requirePhaseCompletion(negotiation)
      resources.peer = peer
      route.throwIfAttemptFailed()
      context.phaseChanged('admission')

      const admission = await this.#runPhase(
        'admission',
        context.admissionBudgetMilliseconds,
        signal,
        route.attemptFailureSignal,
        route,
        (phaseSignal) => this.#admit(
          context,
          resources,
          route,
          peer,
          phaseSignal,
        ),
      )
      const lane = requirePhaseCompletion(admission)
      return Object.freeze({ type: 'admitted', lane })
    } catch (error) {
      const settlement = attemptSettlement(error, resources.route?.attemptFailureSignal)
      terminalError = settlement.error
      const owner = cancellationOwner()
      const result = signal.aborted && owner !== undefined && !this.#session.isClosed &&
          settlement.authority === 'local'
        ? Object.freeze({ type: 'lifecycle-cancelled' as const, owner })
        : Object.freeze({ type: 'failed' as const, failure: this.#classifyFailure(settlement.error) })
      this.#failRoute(resources.route, result)
      return result
    } finally {
      if (resources.peerOwner.kind !== 'published') {
        if (resources.peerOwner.kind === 'runtime') {
          await this.#session.retireLane(resources.peerOwner.laneId, resources.peerOwner.laneEpoch).catch(() => undefined)
        }
        if (resources.peer !== undefined && resources.peerOwner.kind !== 'adoption') {
          await resources.peer.close().catch(() => undefined)
        }
        await resources.route?.close(
          terminalError ?? new DOMException('Peer attempt ended before publication', 'AbortError'),
        ).catch(() => undefined)
      }
      permit?.release()
    }
  }

  async #admit(
    context: PeerAttemptContext,
    resources: AttemptResources,
    route: SessionSignalingRoute,
    peer: PeerChannel,
    signal: AbortSignal,
  ): Promise<PhaseWorkSettlement<{ readonly laneId: number; readonly laneEpoch: number }>> {
    route.throwIfAttemptFailed()
    const grant = await this.#session.requestLaneGrant(0, { signal })
    const grantOperationId = createProtocolOperationIdentity(grant.grantOperationId)
    const laneIdentity = Object.freeze({ laneId: grant.laneId, laneEpoch: grant.laneEpoch })
    route.grantRequested(grantOperationId, grant.laneId)
    route.grantMilestone('grant-received', grantOperationId, laneIdentity)
    route.throwIfAttemptFailed()

    resources.peerOwner = { kind: 'adoption' }
    const adoption = await this.#session.adoptGrantedLane(peer, grant, { signal })
    if (adoption.disposition !== 'unverified') {
      route.grantMilestone('lane-hello-sent', grantOperationId, laneIdentity)
      route.grantMilestone('admission-response-received', grantOperationId, laneIdentity)
      route.admissionResponseSettled(
        grantOperationId,
        laneIdentity,
        adoption.disposition === 'accepted'
          ? Object.freeze({ disposition: 'accepted' })
          : Object.freeze({
              disposition: 'rejected',
              rejectionCode: adoption.rejection.code,
              retryAfterMilliseconds: adoption.rejection.retryAfterMilliseconds,
            }),
      )
      if (adoption.disposition === 'accepted' && adoption.installation === 'installed') {
        route.grantMilestone('lane-attached', grantOperationId, laneIdentity)
      }
    }

    const settlement = receiverAdmissionSettlement(grant, adoption)
    if (settlement.type === 'failed') return settlement
    resources.peerOwner = { kind: 'runtime', ...laneIdentity }
    route.throwIfAttemptFailed()
    let published: boolean
    try {
      published = this.#publish({
        protocolSessionId: context.protocolSessionId,
        peerPathId: context.peerPathId,
        attemptId: createPeerAttemptIdentity(route.binding.attemptId),
        peer,
        route,
        laneId: grant.laneId,
        laneEpoch: grant.laneEpoch,
      })
    } catch (cause) {
      return failedPhase(new LaneInstallationError({ cause }), 'authenticated-admission')
    }
    if (!published) {
      return failedPhase(new LaneInstallationError(), 'authenticated-admission')
    }
    resources.peerOwner = { kind: 'published' }
    route.grantMilestone('admitted', grantOperationId, laneIdentity)
    return completedPhase(laneIdentity, 'authenticated-admission')
  }

  async #runPhase<T>(
    phase: PeerAttemptPhase,
    budgetMilliseconds: number,
    ownerSignal: AbortSignal,
    routeFailureSignal: AbortSignal,
    lifecycle: SessionSignalingRoute,
    work: (signal: AbortSignal) => Promise<PhaseWorkSettlement<T>>,
  ): Promise<PhaseWorkSettlement<T>> {
    lifecycle.phaseDeadlineArmed(phase, budgetMilliseconds)
    const phaseController = linkedController(ownerSignal, routeFailureSignal)
    const timerController = new AbortController()
    let observedWork: PhaseWorkSettlement<T> | undefined
    let workPromise: Promise<PhaseWorkSettlement<T>>
    try {
      workPromise = work(phaseController.signal)
    } catch (error) {
      workPromise = Promise.resolve(failedPhaseFromSource(error, routeFailureSignal))
    }
    const workResult = workPromise
      .then(
        (settlement): PhaseOutcome<T> => {
          observedWork = settlement
          return { type: 'work', settlement }
        },
        (error: unknown): PhaseOutcome<T> => {
          const settlement = failedPhaseFromSource(error, routeFailureSignal)
          observedWork = settlement
          return { type: 'work', settlement }
        },
      )
    const deadline = this.#clock.sleep(budgetMilliseconds, timerController.signal).then(
      (): PhaseOutcome<T> => ({ type: 'deadline' }),
      (error: unknown): Promise<never> | PhaseOutcome<T> => timerController.signal.aborted
        ? NEVER
        : { type: 'clock-failed', error },
    )
    try {
      let winner = await Promise.race([workResult, deadline])
      if (winner.type === 'deadline') {
        // A phase success already queued in this turn wins over its deadline callback.
        await Promise.resolve()
        if (observedWork !== undefined) winner = { type: 'work', settlement: observedWork }
      }
      if (winner.type === 'work') return winner.settlement
      if (winner.type === 'clock-failed') {
        return failedPhaseFromSource(winner.error, routeFailureSignal)
      }

      const timeout = new PeerPhaseTimeoutError(phase)
      lifecycle.phaseDeadlineExpired(phase, budgetMilliseconds)
      phaseController.abort(timeout)
      const joined = await workResult
      if (joined.type !== 'work') return failedPhase(timeout, 'local')
      if (joined.settlement.type === 'completed' || joined.settlement.authority !== 'local') {
        return joined.settlement
      }
      return failedPhase(timeout, 'local')
    } finally {
      timerController.abort()
      phaseController.close()
    }
  }

  async #negotiate(context: PeerAttemptContext, route: SessionSignalingRoute,
    signal: AbortSignal): Promise<PeerChannel> {
    const owner = linkedController(signal)
    let timer: AbortController | undefined
    let stageOrdinal = -1
    const phases = ['signaling', 'ice-check', 'dtls-datachannel'] as const
    const budgets = [PEER_SIGNALING_BUDGET_MILLISECONDS,
      PEER_ICE_CHECK_BUDGET_MILLISECONDS, PEER_DTLS_BUDGET_MILLISECONDS] as const
    const phaseChanged = (phase: typeof phases[number]) => {
      const ordinal = phases.indexOf(phase)
      if (ordinal <= stageOrdinal) return
      stageOrdinal = ordinal
      timer?.abort()
      timer = new AbortController()
      route.providerFact({ kind: 'state', phase, state: 'budget-armed', elapsedMs: budgets[ordinal]! })
      const current = timer
      this.#clock.sleep(budgets[ordinal]!, current.signal).then(() => {
        if (!current.signal.aborted) owner.abort(new PeerPhaseTimeoutError('negotiation'))
      }).catch(() => undefined)
    }
    const observer: PeerOfferAttemptObserver = {
      offerCreated: (counts) => route.offerCreated(counts),
      offerSent: (counts) => route.offerSent(counts),
      answerReceived: (counts) => { route.answerReceived(counts); phaseChanged('ice-check') },
      dataChannelOpened: (counts) => route.dataChannelOpened(counts),
      providerFact: (fact) => { route.providerFact(fact); context.observeICEFact?.(fact) }, phaseChanged,
    }
    phaseChanged('signaling')
    try { return await this.#offers.offer(route, owner.signal, observer, context.iceProfile) }
    finally { timer?.abort(); owner.close() }
  }

  #classifyFailure(error: unknown): PeerAttemptFailure {
    if (this.#session.isClosed) {
      return Object.freeze({
        kind: 'session-terminal',
        terminal: Object.freeze({ authority: 'protocol-session-terminal', code: 'runtime-closed' }),
      })
    }
    return classifyAttemptError(error)
  }

  #failRoute(
    route: SessionSignalingRoute | undefined,
    result: PeerAttemptResult,
  ): void {
    if (route !== undefined && result.type === 'failed') route.failAttempt(result.failure)
  }

}

function linkedController(...sources: readonly AbortSignal[]): {
  readonly signal: AbortSignal
  readonly abort: (reason: unknown) => void
  readonly close: () => void
} {
  const controller = new AbortController()
  const listeners = sources.map((source) => {
    const listener = () => {
      if (!controller.signal.aborted) {
        controller.abort(source.reason ?? new DOMException('Peer attempt aborted', 'AbortError'))
      }
    }
    source.addEventListener('abort', listener, { once: true })
    if (source.aborted) listener()
    return { source, listener }
  })
  return {
    signal: controller.signal,
    abort: (reason) => controller.abort(reason),
    close: () => {
      for (const { source, listener } of listeners) source.removeEventListener('abort', listener)
    },
  }
}

function completedPhase<T>(
  value: T,
  authority: 'local' | 'authenticated-admission',
): PhaseWorkSettlement<T> {
  return Object.freeze({ type: 'completed', value, authority })
}

function failedPhase(
  error: unknown,
  authority: PhaseSettlementAuthority,
): Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }> {
  return Object.freeze({ type: 'failed', error, authority })
}

async function settlePhaseWork<T>(
  work: () => Promise<T>,
  routeFailureSignal: AbortSignal,
): Promise<PhaseWorkSettlement<T>> {
  try {
    return completedPhase(await work(), 'local')
  } catch (error) {
    return failedPhaseFromSource(error, routeFailureSignal)
  }
}

function failedPhaseFromSource(
  error: unknown,
  routeFailureSignal: AbortSignal | undefined,
): Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }> {
  const authenticatedPeerOperation = routeFailureSignal?.aborted === true &&
    routeFailureSignal.reason === error && error instanceof AuthenticatedPeerOperationError
  return failedPhase(
    error,
    authenticatedPeerOperation ? 'authenticated-peer-operation' : 'local',
  )
}

function requirePhaseCompletion<T>(settlement: PhaseWorkSettlement<T>): T {
  if (settlement.type === 'completed') return settlement.value
  throw new PhaseSettlementFailure(settlement)
}

function attemptSettlement(
  error: unknown,
  routeFailureSignal: AbortSignal | undefined,
): Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }> {
  return error instanceof PhaseSettlementFailure
    ? error.settlement
    : failedPhaseFromSource(error, routeFailureSignal)
}

function receiverAdmissionSettlement(
  grant: LaneGrant,
  result: LaneAdmissionResult,
): PhaseWorkSettlement<void> {
  if (result.disposition === 'unverified') return failedPhase(result.error, 'local')
  if (
    !equalDiagnosticIdentities(
      result.grantOperationId,
      createProtocolOperationIdentity(grant.grantOperationId),
    ) ||
    result.laneId !== grant.laneId ||
    result.laneEpoch !== grant.laneEpoch
  ) return inconsistentAdmissionSettlement()

  if (result.disposition === 'rejected') {
    const errorMatches = result.error instanceof LaneAdmissionRejectedError &&
      isLaneRejection(result.rejection) &&
      result.error.rejection.code === result.rejection.code &&
      result.error.rejection.retryAfterMilliseconds === result.rejection.retryAfterMilliseconds
    return errorMatches
      ? failedPhase(result.error, 'authenticated-admission')
      : inconsistentAdmissionSettlement()
  }
  if (Object.hasOwn(result, 'rejection')) return inconsistentAdmissionSettlement()
  if (result.installation === 'failed') {
    return result.disposition === 'accepted' && result.error instanceof LaneInstallationError
      ? failedPhase(result.error, 'authenticated-admission')
      : inconsistentAdmissionSettlement()
  }
  if (result.disposition === 'accepted' && result.installation === 'installed') {
    if (Object.hasOwn(result, 'error')) return inconsistentAdmissionSettlement()
    return completedPhase(undefined, 'authenticated-admission')
  }
  return inconsistentAdmissionSettlement()
}

function inconsistentAdmissionSettlement(): PhaseWorkSettlement<never> {
  return failedPhase(
    new SessionRuntimeError('lane', 'Lane admission runtime returned inconsistent settlement'),
    'local',
  )
}

function classifyAttemptError(error: unknown): PeerAttemptFailure {
  if (error instanceof PeerTabCapacityTimeoutError) {
    return Object.freeze({ kind: 'local-transient', phase: 'negotiation', reason: 'tab-capacity-timeout' })
  }
  if (error instanceof AuthenticatedPeerOperationError) {
    return Object.freeze({
      kind: 'authenticated-peer-operation',
      code: error.protocolFailure.content.code,
    })
  }
  if (error instanceof LaneAdmissionRejectedError) {
    return Object.freeze({ kind: 'authenticated-lane-rejection', rejection: error.rejection })
  }
  if (error instanceof CandidateLimitExceededError) {
    return Object.freeze({ kind: 'local-policy', code: 'candidate-limit' })
  }
  if (error instanceof UnexpectedDataChannelError) {
    return Object.freeze({ kind: 'local-policy', code: 'unexpected-data-channel' })
  }
  if (error instanceof PeerPhaseTimeoutError) {
    return Object.freeze({
      kind: 'local-transient',
      phase: error.phase,
      reason: error.phase === 'admission' ? 'admission-timeout' : 'negotiation-timeout',
    })
  }
  if (error instanceof LaneInstallationError) {
    return Object.freeze({
      kind: 'local-transient',
      phase: 'admission',
      reason: 'lane-installation-failed',
    })
  }
  if (error instanceof LaneAdmissionTransportError) {
    return Object.freeze({ kind: 'local-transient', phase: 'admission', reason: 'transport-loss' })
  }
  if (error instanceof PeerNegotiationError) {
    return Object.freeze({ kind: 'local-transient', phase: 'negotiation', reason: 'transport-loss' })
  }
  if (error instanceof LaneCodecError) {
    return Object.freeze({ kind: 'local-contract', code: laneCodecFailureCode(error) })
  }
  if (error instanceof SessionRuntimeError && error.scope === 'session') {
    return Object.freeze({ kind: 'local-contract', code: 'identity-mismatch' })
  }
  return Object.freeze({ kind: 'local-contract', code: 'unknown-local-failure' })
}

function laneCodecFailureCode(
  error: LaneCodecError,
): 'invalid-proof' | 'invalid-signature' | 'invalid-lane-response' {
  if (error.kind === 'proof') return 'invalid-proof'
  if (error.kind === 'signature') return 'invalid-signature'
  return 'invalid-lane-response'
}
