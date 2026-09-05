import type { FailureCorrelation } from '../diagnostics/incident/fact'
import type { PeerProviderFact } from './peer-set/provider-facts'
import type { V2ProtocolOperationIdentity } from '../session/v2-identities'
import {
  V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES,
  v2TypedErrorForPeerOperationCode,
  type V2CandidateCounts,
  type V2ConnectivityTraceSource,
  type V2LaneIdentity,
  type V2PeerAttemptCorrelation,
  type V2PeerAttemptTraceEvent,
} from './diagnostics'
import {
  classifyV2PeerAttemptFailure,
  type V2PeerAttemptFailure,
} from './v2-peer-failure'

const BROWSER_LINEAR_STAGES = V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES.filter(
  (stage) => stage !== 'negotiation-deadline-expired' &&
    stage !== 'admission-deadline-expired' &&
    stage !== 'failed',
)

type LinearAttemptStage = (typeof BROWSER_LINEAR_STAGES)[number]
type V2PeerAttemptTracePayload<Event = V2PeerAttemptTraceEvent> =
  Event extends V2PeerAttemptTraceEvent
    ? Omit<
        Event,
        'eventName' | 'correlation' | 'waveOrdinal' |
        'waveAttemptOrdinal' | 'sessionAttemptOrdinal'
      >
    : never

/** Owns one linear peer-attempt trace stream without owning peer policy. */
export class BrowserAttemptLifecycle {
  readonly #trace: V2ConnectivityTraceSource | undefined
  readonly #correlation: () => V2PeerAttemptCorrelation
  #nextStageIndex = 1
  #terminal = false
  #offerOperationId: V2ProtocolOperationIdentity | undefined
  #grantOperationId: V2ProtocolOperationIdentity | undefined
  #lane: V2LaneIdentity | undefined

  constructor(
    correlation: () => V2PeerAttemptCorrelation,
    trace?: V2ConnectivityTraceSource,
  ) {
    this.#trace = trace
    this.#correlation = correlation
    this.#emit(() => this.#event({ stage: 'started' }))
  }

  setOfferOperationId(operationId: V2ProtocolOperationIdentity): void {
    if (!this.#terminal) this.#offerOperationId ??= operationId
  }

  providerFact(fact: PeerProviderFact): void {
    this.#emit(() => this.#event({ stage: 'provider-fact', fact }, this.#offerOperationId, this.#lane))
  }

  offerMilestone(
    stage: 'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open',
    candidateCounts: V2CandidateCounts | (() => V2CandidateCounts),
  ): void {
    this.#advance(stage, () => {
      const counts = typeof candidateCounts === 'function' ? candidateCounts() : candidateCounts
      return this.#event({
        stage,
        candidateCounts: Object.freeze({ ...counts }),
      }, this.#offerOperationId)
    })
  }

  phaseDeadlineArmed(
    phase: 'negotiation' | 'admission',
    deadlineBudgetMilliseconds: number,
  ): void {
    if (phase === 'negotiation') {
      this.#advance('negotiation-deadline-armed', () => this.#event({
        stage: 'negotiation-deadline-armed',
        phase: 'negotiation',
        deadlineBudgetMilliseconds,
      }, this.#offerOperationId))
      return
    }
    this.#advance('admission-deadline-armed', () => this.#event({
      stage: 'admission-deadline-armed',
      phase: 'admission',
      deadlineBudgetMilliseconds,
    }, this.#offerOperationId))
  }

  phaseDeadlineExpired(
    phase: 'negotiation' | 'admission',
    deadlineBudgetMilliseconds: number,
  ): void {
    if (this.#terminal) return
    if (phase === 'negotiation') {
      this.#emit(() => this.#event({
        stage: 'negotiation-deadline-expired',
        phase: 'negotiation',
        deadlineBudgetMilliseconds,
      }, this.#offerOperationId))
      return
    }
    this.#emit(() => this.#event({
      stage: 'admission-deadline-expired',
      phase: 'admission',
      deadlineBudgetMilliseconds,
    }, this.#offerOperationId))
  }

  grantRequested(
    grantOperationId: V2ProtocolOperationIdentity,
    requestedLaneId: number,
  ): void {
    this.#grantOperationId = grantOperationId
    this.#advance('grant-requested', () => this.#event({
      stage: 'grant-requested',
      phase: 'admission',
      requestedLaneId,
    }, grantOperationId))
  }

  grantMilestone(
    stage: 'grant-received' | 'lane-hello-sent' | 'admission-response-received' |
      'lane-attached' | 'admitted',
    grantOperationId: V2ProtocolOperationIdentity,
    lane: V2LaneIdentity,
  ): void {
    this.#grantOperationId = grantOperationId
    this.#lane = lane
    this.#advance(stage, () => this.#event({
      stage,
      phase: 'admission',
    }, grantOperationId, lane))
  }

  admissionResponseSettled(
    grantOperationId: V2ProtocolOperationIdentity,
    lane: V2LaneIdentity,
    settlement:
      | Readonly<{ disposition: 'accepted' }>
      | Readonly<{
          disposition: 'rejected'
          rejectionCode: number
          retryAfterMilliseconds: number
        }>,
  ): void {
    this.#grantOperationId = grantOperationId
    this.#lane = lane
    this.#advance('admission-response-settled', () => this.#event({
      stage: 'admission-response-settled',
      phase: 'admission',
      settlement: Object.freeze({ ...settlement }),
    }, grantOperationId, lane))
  }

  failed(failure: V2PeerAttemptFailure): void {
    if (this.#terminal) return
    const failedAtStage = BROWSER_LINEAR_STAGES[this.#nextStageIndex]
    if (failedAtStage === undefined || failedAtStage === 'started') return
    this.#terminal = true
    this.#emit(() => {
      const decision = classifyV2PeerAttemptFailure(failure)
      return this.#event({
        stage: 'failed',
        failedAtStage,
        failure: snapshotFailure(failure),
        failureScope: decisionScope(decision.type),
        typedErrorCode: diagnosticTypedErrorCode(failure),
        retryable: decision.type === 'retry-attempt',
      }, this.#grantOperationId ?? this.#offerOperationId, this.#lane)
    })
  }

  #advance(
    stage: LinearAttemptStage,
    createEvent: () => V2PeerAttemptTraceEvent,
  ): void {
    if (this.#terminal || BROWSER_LINEAR_STAGES[this.#nextStageIndex] !== stage) return
    this.#nextStageIndex += 1
    this.#emit(createEvent)
    if (stage === 'admitted') this.#terminal = true
  }

  #event(
    payload: V2PeerAttemptTracePayload,
    operationId?: V2ProtocolOperationIdentity,
    lane?: V2LaneIdentity,
  ): V2PeerAttemptTraceEvent {
    const attemptCorrelation = this.#correlation()
    const correlation: FailureCorrelation = Object.freeze({
      protocolSessionId: attemptCorrelation.protocolSessionId,
      ...(operationId === undefined ? {} : { protocolOperationId: operationId }),
      peerPathId: attemptCorrelation.peerPathId,
      peerAttemptId: attemptCorrelation.attemptId,
      ...(lane === undefined
        ? {}
        : { lane: Object.freeze({ id: lane.laneId, epoch: lane.laneEpoch }) }),
    })
    return Object.freeze({
      eventName: 'peer_attempt',
      correlation,
      waveOrdinal: attemptCorrelation.waveOrdinal,
      waveAttemptOrdinal: attemptCorrelation.waveAttemptOrdinal,
      sessionAttemptOrdinal: attemptCorrelation.sessionAttemptOrdinal,
      ...payload,
    }) as V2PeerAttemptTraceEvent
  }

  #emit(createEvent: () => V2PeerAttemptTraceEvent): void {
    try {
      const observer = this.#trace?.current
      if (observer === undefined) return
      observer(createEvent())
    } catch {
      // Trace loss cannot alter phase, authenticated settlement, or cleanup.
    }
  }
}

function snapshotFailure(failure: V2PeerAttemptFailure): V2PeerAttemptFailure {
  if (failure.kind === 'authenticated-lane-rejection') {
    return Object.freeze({
      kind: failure.kind,
      rejection: Object.freeze({ ...failure.rejection }),
    })
  }
  if (failure.kind === 'session-terminal') {
    return Object.freeze({ kind: failure.kind, terminal: Object.freeze({ ...failure.terminal }) })
  }
  return Object.freeze({ ...failure })
}

function diagnosticTypedErrorCode(failure: V2PeerAttemptFailure) {
  if (failure.kind === 'session-terminal') return 'runtime-stopped' as const
  if (failure.kind === 'authenticated-peer-operation') {
    return v2TypedErrorForPeerOperationCode(failure.code) ?? 'unexpected'
  }
  if (failure.kind === 'authenticated-lane-rejection') return 'peer-admission' as const
  if (failure.kind === 'local-contract') return 'signaling-contract' as const
  if (failure.kind === 'local-policy') {
    return failure.code === 'candidate-limit' ? 'peer-candidates' as const : 'unexpected' as const
  }
  if (failure.reason === 'negotiation-timeout' || failure.reason === 'admission-timeout') {
    return 'peer-timeout' as const
  }
  return failure.phase === 'admission' ? 'peer-admission' as const : 'peer-negotiation' as const
}

function decisionScope(decision: string): 'session-terminal' | 'path-terminal' | 'attempt-transient' {
  if (decision === 'stop-session') return 'session-terminal'
  return decision === 'stop-path' ? 'path-terminal' : 'attempt-transient'
}
