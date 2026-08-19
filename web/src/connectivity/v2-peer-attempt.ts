import {
  V2LaneAdmissionRejectedError,
  V2LaneAdmissionTransportError,
  V2LaneInstallationError,
  type V2LaneAdmissionResult,
  type V2ReceiverSessionRuntime,
} from '../session/v2-runtime'
import {
  createV2PeerAttemptIdentity,
  createV2ProtocolOperationIdentity,
  equalV2DiagnosticIdentities,
  type V2PeerAttemptIdentity,
  type V2PeerPathIdentity,
  type V2ProtocolSessionIdentity,
} from '../session/v2-identities'
import { V2LaneCodecError, type V2LaneGrant } from '../session/v2-lane-codec'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
  UnexpectedDataChannelError,
} from './errors'
import type { PeerChannel } from './peer-channel'
import type { OfferChannelFactory } from './peer-offer'
import {
  isV2LaneRejection,
  type V2PeerAttemptFailure,
  type V2PeerAttemptPhase,
  type V2PeerAttemptResult,
} from './v2-peer-failure'
import type {
  V2PeerAttemptContext,
  V2PeerAttemptHandle,
  V2PeerRecoveryAttemptFactory,
  V2PeerRecoveryClock,
} from './v2-peer-recovery'
import type { V2ConnectivityTraceSource } from './diagnostics'
import {
  V2AuthenticatedPeerOperationError,
  createV2PeerBindingForPath,
  V2SessionSignalingRoute,
} from './v2-session-signaling'
import type { V2PeerBinding } from './v2-signaling-codec'

export interface V2PeerCandidatePublication {
  readonly protocolSessionId: V2ProtocolSessionIdentity
  readonly peerPathId: V2PeerPathIdentity
  readonly attemptId: V2PeerAttemptIdentity
  readonly peer: PeerChannel
  readonly route: V2SessionSignalingRoute
  readonly laneId: number
  readonly laneEpoch: number
}

export type V2PeerCandidatePublisher = (candidate: V2PeerCandidatePublication) => boolean

export interface V2BrowserPeerAttemptExecutorOptions {
  readonly session: V2ReceiverSessionRuntime
  readonly offers: OfferChannelFactory
  readonly peerPathIdentity: Uint8Array
  readonly clock: V2PeerRecoveryClock
  readonly publish: V2PeerCandidatePublisher
  readonly randomBytes?: (length: number) => Uint8Array
  readonly trace?: V2ConnectivityTraceSource
}

interface AttemptResources {
  route?: V2SessionSignalingRoute
  peer?: PeerChannel
  peerOwner: 'attempt' | 'adoption' | 'runtime' | 'published'
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

class V2PeerPhaseTimeoutError extends DOMException {
  readonly phase: V2PeerAttemptPhase

  constructor(phase: V2PeerAttemptPhase) {
    super(`Peer ${phase} phase timed out`, 'TimeoutError')
    this.phase = phase
  }
}

class V2PhaseSettlementFailure {
  readonly settlement: Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }>

  constructor(settlement: Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }>) {
    this.settlement = settlement
  }
}

/** One handle owns one immutable identity tuple and one exactly-joined workflow. */
export class V2BrowserPeerAttemptExecutor implements V2PeerRecoveryAttemptFactory {
  readonly #session: V2ReceiverSessionRuntime
  readonly #offers: OfferChannelFactory
  readonly #peerPathIdentity: Uint8Array<ArrayBuffer>
  readonly #clock: V2PeerRecoveryClock
  readonly #publish: V2PeerCandidatePublisher
  readonly #randomBytes: ((length: number) => Uint8Array) | undefined
  readonly #trace: V2ConnectivityTraceSource | undefined

  constructor(options: V2BrowserPeerAttemptExecutorOptions) {
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
  }

  createAttempt(context: V2PeerAttemptContext): V2PeerAttemptHandle {
    const binding = this.#randomBytes === undefined
      ? createV2PeerBindingForPath(this.#peerPathIdentity)
      : createV2PeerBindingForPath(this.#peerPathIdentity, this.#randomBytes)
    const attemptId = createV2PeerAttemptIdentity(binding.attemptId)
    const controller = new AbortController()
    let cancellationOwner: Parameters<V2PeerAttemptHandle['cancel']>[0] | undefined
    const result = this.#run(context, binding, controller.signal, () => cancellationOwner)
    return Object.freeze({
      attemptId,
      result,
      cancel: (owner: Parameters<V2PeerAttemptHandle['cancel']>[0]) => {
        if (controller.signal.aborted) return
        cancellationOwner = owner
        controller.abort(owner)
      },
    })
  }

  async #run(
    context: V2PeerAttemptContext,
    binding: V2PeerBinding,
    signal: AbortSignal,
    cancellationOwner: () => Parameters<V2PeerAttemptHandle['cancel']>[0] | undefined,
  ): Promise<V2PeerAttemptResult> {
    const resources: AttemptResources = { peerOwner: 'attempt' }
    let terminalError: unknown
    try {
      signal.throwIfAborted()
      const route = new V2SessionSignalingRoute(
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
      const negotiation = await this.#runPhase(
        'negotiation',
        context.negotiationBudgetMilliseconds,
        signal,
        route.attemptFailureSignal,
        route,
        (phaseSignal) => settlePhaseWork(
          () => this.#offers.offer(route, phaseSignal, route),
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
      if (resources.peerOwner !== 'published') {
        if (resources.peer !== undefined && resources.peerOwner !== 'adoption') {
          await resources.peer.close().catch(() => undefined)
        }
        await resources.route?.close(
          terminalError ?? new DOMException('Peer attempt ended before publication', 'AbortError'),
        ).catch(() => undefined)
      }
    }
  }

  async #admit(
    context: V2PeerAttemptContext,
    resources: AttemptResources,
    route: V2SessionSignalingRoute,
    peer: PeerChannel,
    signal: AbortSignal,
  ): Promise<PhaseWorkSettlement<{ readonly laneId: number; readonly laneEpoch: number }>> {
    route.throwIfAttemptFailed()
    const grant = await this.#session.requestLaneGrant(context.requestedLaneId, { signal })
    const grantOperationId = createV2ProtocolOperationIdentity(grant.grantOperationId)
    const laneIdentity = Object.freeze({ laneId: grant.laneId, laneEpoch: grant.laneEpoch })
    route.grantRequested(grantOperationId, grant.laneId)
    route.grantMilestone('grant-received', grantOperationId, laneIdentity)
    route.throwIfAttemptFailed()

    resources.peerOwner = 'adoption'
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
    resources.peerOwner = 'runtime'
    route.throwIfAttemptFailed()
    let published: boolean
    try {
      published = this.#publish({
        protocolSessionId: context.protocolSessionId,
        peerPathId: context.peerPathId,
        attemptId: createV2PeerAttemptIdentity(route.binding.attemptId),
        peer,
        route,
        laneId: grant.laneId,
        laneEpoch: grant.laneEpoch,
      })
    } catch (cause) {
      return failedPhase(new V2LaneInstallationError({ cause }), 'authenticated-admission')
    }
    if (!published) {
      return failedPhase(new V2LaneInstallationError(), 'authenticated-admission')
    }
    resources.peerOwner = 'published'
    route.grantMilestone('admitted', grantOperationId, laneIdentity)
    return completedPhase(laneIdentity, 'authenticated-admission')
  }

  async #runPhase<T>(
    phase: V2PeerAttemptPhase,
    budgetMilliseconds: number,
    ownerSignal: AbortSignal,
    routeFailureSignal: AbortSignal,
    lifecycle: V2SessionSignalingRoute,
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

      const timeout = new V2PeerPhaseTimeoutError(phase)
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

  #classifyFailure(error: unknown): V2PeerAttemptFailure {
    if (this.#session.isClosed) {
      return Object.freeze({
        kind: 'session-terminal',
        terminal: Object.freeze({ authority: 'protocol-session-terminal', code: 'runtime-closed' }),
      })
    }
    return classifyAttemptError(error)
  }

  #failRoute(
    route: V2SessionSignalingRoute | undefined,
    result: V2PeerAttemptResult,
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
    routeFailureSignal.reason === error && error instanceof V2AuthenticatedPeerOperationError
  return failedPhase(
    error,
    authenticatedPeerOperation ? 'authenticated-peer-operation' : 'local',
  )
}

function requirePhaseCompletion<T>(settlement: PhaseWorkSettlement<T>): T {
  if (settlement.type === 'completed') return settlement.value
  throw new V2PhaseSettlementFailure(settlement)
}

function attemptSettlement(
  error: unknown,
  routeFailureSignal: AbortSignal | undefined,
): Extract<PhaseWorkSettlement<never>, { readonly type: 'failed' }> {
  return error instanceof V2PhaseSettlementFailure
    ? error.settlement
    : failedPhaseFromSource(error, routeFailureSignal)
}

function receiverAdmissionSettlement(
  grant: V2LaneGrant,
  result: V2LaneAdmissionResult,
): PhaseWorkSettlement<void> {
  if (result.disposition === 'unverified') return failedPhase(result.error, 'local')
  if (
    !equalV2DiagnosticIdentities(
      result.grantOperationId,
      createV2ProtocolOperationIdentity(grant.grantOperationId),
    ) ||
    result.laneId !== grant.laneId ||
    result.laneEpoch !== grant.laneEpoch
  ) return inconsistentAdmissionSettlement()

  if (result.disposition === 'rejected') {
    const errorMatches = result.error instanceof V2LaneAdmissionRejectedError &&
      isV2LaneRejection(result.rejection) &&
      result.error.rejection.code === result.rejection.code &&
      result.error.rejection.retryAfterMilliseconds === result.rejection.retryAfterMilliseconds
    return errorMatches
      ? failedPhase(result.error, 'authenticated-admission')
      : inconsistentAdmissionSettlement()
  }
  if (Object.hasOwn(result, 'rejection')) return inconsistentAdmissionSettlement()
  if (result.installation === 'failed') {
    return result.disposition === 'accepted' && result.error instanceof V2LaneInstallationError
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
    new V2SessionRuntimeError('lane', 'Lane admission runtime returned inconsistent settlement'),
    'local',
  )
}

function classifyAttemptError(error: unknown): V2PeerAttemptFailure {
  if (error instanceof V2AuthenticatedPeerOperationError) {
    return Object.freeze({
      kind: 'authenticated-peer-operation',
      code: error.protocolFailure.wireCode,
    })
  }
  if (error instanceof V2LaneAdmissionRejectedError) {
    return Object.freeze({ kind: 'authenticated-lane-rejection', rejection: error.rejection })
  }
  if (error instanceof CandidateLimitExceededError) {
    return Object.freeze({ kind: 'local-policy', code: 'candidate-limit' })
  }
  if (error instanceof UnexpectedDataChannelError) {
    return Object.freeze({ kind: 'local-policy', code: 'unexpected-data-channel' })
  }
  if (error instanceof V2PeerPhaseTimeoutError) {
    return Object.freeze({
      kind: 'local-transient',
      phase: error.phase,
      reason: error.phase === 'admission' ? 'admission-timeout' : 'negotiation-timeout',
    })
  }
  if (error instanceof V2LaneInstallationError) {
    return Object.freeze({
      kind: 'local-transient',
      phase: 'admission',
      reason: 'lane-installation-failed',
    })
  }
  if (error instanceof V2LaneAdmissionTransportError) {
    return Object.freeze({ kind: 'local-transient', phase: 'admission', reason: 'transport-loss' })
  }
  if (error instanceof PeerNegotiationError) {
    return Object.freeze({ kind: 'local-transient', phase: 'negotiation', reason: 'transport-loss' })
  }
  if (error instanceof V2LaneCodecError) {
    return Object.freeze({ kind: 'local-contract', code: laneCodecFailureCode(error) })
  }
  if (error instanceof V2SessionRuntimeError && error.scope === 'session') {
    return Object.freeze({ kind: 'local-contract', code: 'identity-mismatch' })
  }
  return Object.freeze({ kind: 'local-contract', code: 'unknown-local-failure' })
}

function laneCodecFailureCode(
  error: V2LaneCodecError,
): 'invalid-proof' | 'invalid-signature' | 'invalid-lane-response' {
  if (error.kind === 'proof') return 'invalid-proof'
  if (error.kind === 'signature') return 'invalid-signature'
  return 'invalid-lane-response'
}
