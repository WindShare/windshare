import type { V2PeerAttemptFailure } from './v2-peer-failure'
import {
  V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES,
  V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION,
  v2TypedErrorForPeerOperationCode,
  type V2AttemptOrdinalCorrelation,
  type V2BrowserConnectivityAttemptDiagnostic,
  type V2StableBrowserConnectivityAttemptStage,
  type V2ConnectivityObserver,
  type V2LaneIdentity,
} from './diagnostics'

const BROWSER_LINEAR_STAGES = V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES.filter(
  (stage): stage is Exclude<
    V2StableBrowserConnectivityAttemptStage,
    'negotiation-deadline-expired' | 'admission-deadline-expired' | 'failed'
  > => stage !== 'negotiation-deadline-expired' &&
    stage !== 'admission-deadline-expired' && stage !== 'failed',
)

type AttemptEnvelopeKey =
  | 'schemaVersion' | 'stream' | 'sessionId' | 'peerPathId' | 'attemptId' | 'side'
  | 'sideSequence' | 'attemptElapsedMs' | keyof V2AttemptOrdinalCorrelation

type BrowserAttemptPayload = V2BrowserConnectivityAttemptDiagnostic extends infer Event
  ? Event extends V2BrowserConnectivityAttemptDiagnostic ? Omit<Event, AttemptEnvelopeKey> : never
  : never

interface BrowserAttemptCorrelation extends V2AttemptOrdinalCorrelation {
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
}

/** Owns the one linear, privacy-safe event stream for a browser attempt. */
export class BrowserAttemptLifecycle {
  readonly #observers: readonly V2ConnectivityObserver[]
  readonly #now: () => number
  readonly #correlation: BrowserAttemptCorrelation
  readonly #startedAt: number
  #nextStageIndex = 1
  #sideSequence = 0
  #lastElapsedMs = 0
  #terminal = false
  #offerOperationId: string | undefined
  #grantOperationId: string | undefined
  #lane: V2LaneIdentity | undefined

  constructor(
    correlation: BrowserAttemptCorrelation,
    observers: readonly (V2ConnectivityObserver | undefined)[],
    now: () => number,
  ) {
    this.#observers = observers.filter(
      (observer): observer is V2ConnectivityObserver => observer !== undefined,
    )
    this.#now = now
    this.#correlation = Object.freeze({ ...correlation })
    this.#startedAt = this.#readNow()
    this.#emit({ stage: 'started' })
  }

  setOfferOperationId(operationId: string): void {
    if (!this.#terminal && operationId !== '') this.#offerOperationId ??= operationId
  }

  advance(payload: BrowserAttemptPayload): void {
    if (this.#terminal || !this.#expect(payload.stage)) return
    this.#remember(payload)
    this.#nextStageIndex += 1
    this.#emit(this.#withRememberedCorrelation(payload))
    if (payload.stage === 'admitted') this.#terminal = true
  }

  deadlineExpired(
    phase: 'negotiation' | 'admission',
    deadlineBudgetMs: number,
  ): void {
    if (this.#terminal) return
    const stage = phase === 'negotiation'
      ? 'negotiation-deadline-expired'
      : 'admission-deadline-expired'
    if (stage === 'negotiation-deadline-expired') {
      this.#emit({ stage, phase: 'negotiation', deadlineBudgetMs })
      return
    }
    this.#emit(this.#withRememberedCorrelation({
      stage,
      phase: 'admission',
      deadlineBudgetMs,
      offerOperationId: this.#offerOperationId ?? '',
    }))
  }

  failed(failure: V2PeerAttemptFailure): void {
    if (this.#terminal) return
    const failedAtStage = BROWSER_LINEAR_STAGES[this.#nextStageIndex]
    if (failedAtStage === undefined || failedAtStage === 'started') return
    this.#terminal = true
    this.#emit({
      stage: 'failed',
      failedAtStage,
      failure: snapshotFailure(failure),
      failureScope: failure.kind === 'session-terminal' ? 'session' : 'attempt',
      typedErrorCode: diagnosticTypedErrorCode(failure),
      ...(this.#offerOperationId === undefined
        ? {}
        : { offerOperationId: this.#offerOperationId }),
      ...(this.#grantOperationId === undefined
        ? {}
        : { grantOperationId: this.#grantOperationId }),
      ...(this.#lane === undefined ? {} : { lane: this.#lane }),
    })
  }

  #expect(stage: BrowserAttemptPayload['stage']): boolean {
    const expected = BROWSER_LINEAR_STAGES[this.#nextStageIndex]
    return expected === stage
  }

  #remember(payload: BrowserAttemptPayload): void {
    if ('offerOperationId' in payload && payload.offerOperationId !== undefined) {
      this.#offerOperationId ??= payload.offerOperationId
    }
    if ('grantOperationId' in payload) this.#grantOperationId = payload.grantOperationId
    if ('lane' in payload) this.#lane = snapshotLane(payload.lane)
  }

  #withRememberedCorrelation(payload: BrowserAttemptPayload): BrowserAttemptPayload {
    const withOffer = this.#offerOperationId === undefined || 'offerOperationId' in payload
      ? payload
      : { ...payload, offerOperationId: this.#offerOperationId }
    return Object.freeze(withOffer) as BrowserAttemptPayload
  }

  #emit(payload: BrowserAttemptPayload): void {
    if (this.#observers.length === 0) return
    this.#sideSequence += 1
    const event = Object.freeze({
      schemaVersion: V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION,
      stream: 'attempt' as const,
      ...this.#correlation,
      side: 'browser' as const,
      sideSequence: this.#sideSequence,
      attemptElapsedMs: this.#elapsedMilliseconds(),
      ...payload,
    }) as V2BrowserConnectivityAttemptDiagnostic
    for (const observer of this.#observers) {
      try {
        observer(event)
      } catch {
        // Observer loss cannot alter phase, authenticated settlement, or cleanup.
      }
    }
  }

  #elapsedMilliseconds(): number {
    const elapsed = Math.max(0, Math.floor(this.#readNow() - this.#startedAt))
    this.#lastElapsedMs = Math.max(this.#lastElapsedMs, elapsed)
    return this.#lastElapsedMs
  }

  #readNow(): number {
    try {
      const value = this.#now()
      if (Number.isFinite(value)) {
        return Math.min(Number.MAX_SAFE_INTEGER, Math.max(0, value))
      }
    } catch {
      // A diagnostic clock is isolated for the same reason as its observer.
    }
    return 0
  }
}

function snapshotLane(lane: V2LaneIdentity): V2LaneIdentity {
  return Object.freeze({ laneId: lane.laneId, laneEpoch: lane.laneEpoch })
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
