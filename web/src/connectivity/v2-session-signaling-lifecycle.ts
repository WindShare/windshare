import { encodeBase64Url } from '../crypto/bytes'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import {
  V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES,
  V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION,
  type V2BrowserConnectivityAttemptDiagnostic,
  type V2BrowserConnectivityAttemptStage,
  type V2BrowserSelectedPairDiagnostic,
  type V2CandidateCounts,
  type V2ConnectivityObserver,
  type V2LaneIdentity,
  type V2TypedPeerErrorCode,
} from './diagnostics'
import type { V2PeerBinding } from './v2-signaling-codec'
import { awaitPeerEvidence } from './abortable-peer-evidence'
import { classifyV2TerminalSignalingFailure } from './v2-session-signaling-errors'

const BROWSER_SUCCESS_STAGES = V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES.filter(
  (stage): stage is Exclude<V2BrowserConnectivityAttemptStage, 'failed'> => stage !== 'failed',
)

type BrowserMilestonePayload = {
  readonly candidateCounts: V2CandidateCounts
  readonly lane?: V2LaneIdentity
}

export class BrowserAttemptLifecycle {
  readonly #observer: V2ConnectivityObserver | undefined
  readonly #now: () => number
  readonly #sessionId: string
  readonly #peerPathId: string
  readonly #attemptId: string
  readonly #startedAt: number
  #nextStageIndex = 1
  #sideSequence = 0
  #lastElapsedMs = 0
  #terminal = false
  #candidateCounts: V2CandidateCounts | undefined
  #lane: V2LaneIdentity | undefined
  #selectedPairReader: (() => Promise<V2BrowserSelectedPairDiagnostic | null>) | undefined
  #selectedPairPromise: Promise<V2BrowserSelectedPairDiagnostic | null> | undefined
  #selectedPair: V2BrowserSelectedPairDiagnostic | null = null
  #selectedPairRead = false
  #failureScope: 'attempt' | 'session' = 'attempt'

  constructor(
    session: V2ReceiverSessionRuntime,
    binding: V2PeerBinding,
    observer: V2ConnectivityObserver | undefined,
    now: () => number,
  ) {
    this.#observer = observer
    this.#now = now
    this.#sessionId = observer === undefined ? '' : encodeBase64Url(session.keys.protocolSessionId)
    this.#peerPathId = observer === undefined ? '' : encodeBase64Url(binding.peerPathId)
    this.#attemptId = observer === undefined ? '' : encodeBase64Url(binding.attemptId)
    this.#startedAt = this.#readNow()
    if (observer !== undefined) this.#emit({ stage: 'started' })
  }

  get candidateCounts(): V2CandidateCounts {
    return this.#candidateCounts ?? Object.freeze({ localEmitted: 0, remoteAccepted: 0 })
  }

  get failureScope(): 'attempt' | 'session' {
    return this.#failureScope
  }

  setFailureScope(scope: 'attempt' | 'session'): void {
    if (!this.#terminal) this.#failureScope = scope
  }

  advance(
    stage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'admitted' | 'failed'>,
    payload: BrowserMilestonePayload,
  ): void {
    if (this.#observer === undefined || this.#terminal) return
    if (!this.#expect(stage)) return
    const candidateCounts = snapshotCandidateCounts(payload.candidateCounts)
    const lane = payload.lane === undefined ? undefined : snapshotLane(payload.lane)
    this.#candidateCounts = candidateCounts
    if (lane !== undefined) this.#lane = lane
    this.#nextStageIndex += 1
    this.#emit({
      stage,
      candidateCounts,
      ...(lane === undefined ? {} : { lane }),
    })
  }

  registerSelectedPairReader(
    reader: () => Promise<V2BrowserSelectedPairDiagnostic | null>,
  ): void {
    this.#selectedPairReader ??= reader
  }

  async readSelectedPair(signal?: AbortSignal): Promise<V2BrowserSelectedPairDiagnostic | null> {
    if (this.#selectedPairPromise === undefined) {
      this.#selectedPairPromise = this.#readSelectedPair()
    }
    const selectedPair = signal === undefined
      ? await this.#selectedPairPromise
      : await awaitPeerEvidence(this.#selectedPairPromise, signal)
    this.#selectedPair = selectedPair
    this.#selectedPairRead = true
    return selectedPair
  }

  admitted(lane: V2LaneIdentity, selectedPair: V2BrowserSelectedPairDiagnostic | null): void {
    if (this.#observer === undefined || this.#terminal) return
    if (!this.#expect('admitted')) return
    const authoritativeLane = snapshotLane(lane)
    if (this.#lane !== undefined && !sameLane(this.#lane, authoritativeLane)) {
      this.failed(
        new Error('Connectivity admission changed the authenticated lane identity'),
        'attempt',
        'unexpected',
      )
      return
    }
    this.#lane = authoritativeLane
    this.#selectedPair = selectedPair
    this.#selectedPairRead = true
    this.#terminal = true
    this.#nextStageIndex += 1
    this.#emit({
      stage: 'admitted',
      candidateCounts: this.candidateCounts,
      lane: authoritativeLane,
      selectedPair,
    })
  }

  failed(
    reason: unknown,
    failureScope: 'attempt' | 'session',
    typedErrorCode?: V2TypedPeerErrorCode,
  ): void {
    if (this.#observer === undefined || this.#terminal) return
    const failedAtStage = BROWSER_SUCCESS_STAGES[this.#nextStageIndex]
    if (failedAtStage === undefined || failedAtStage === 'started') return
    const failure = classifyV2TerminalSignalingFailure(
      reason,
      failedAtStage,
      failureScope,
      typedErrorCode,
    )
    this.#terminal = true
    this.#emit({
      stage: 'failed',
      failedAtStage,
      failureScope: failure.failureScope,
      typedErrorCode: failure.typedErrorCode,
      failureMessage: failure.failureMessage,
      ...(this.#candidateCounts === undefined || failedAtStage === 'offer-created'
        ? {}
        : { candidateCounts: this.#candidateCounts }),
      ...(this.#lane === undefined || (failedAtStage !== 'lane-attached' && failedAtStage !== 'admitted')
        ? {}
        : { lane: this.#lane }),
      ...(failedAtStage === 'admitted' && this.#selectedPairRead
        ? { selectedPair: this.#selectedPair }
        : {}),
      ...(failure.authenticatedSenderOperationFailure === undefined
        ? {}
        : { authenticatedSenderOperationFailure: failure.authenticatedSenderOperationFailure }),
    })
  }

  #expect(stage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'failed'>): boolean {
    const expected = BROWSER_SUCCESS_STAGES[this.#nextStageIndex]
    if (expected === stage) return true
    this.failed(
      new Error(`Connectivity milestone ${stage} arrived while ${expected ?? 'terminal'} was expected`),
      'attempt',
      'unexpected',
    )
    return false
  }

  async #readSelectedPair(): Promise<V2BrowserSelectedPairDiagnostic | null> {
    if (this.#selectedPairReader === undefined) return null
    try {
      return await this.#selectedPairReader()
    } catch {
      // getStats evidence may be unavailable even after authenticated admission.
      return null
    }
  }

  #emit(payload: Record<string, unknown>): void {
    const observer = this.#observer
    if (observer === undefined) return
    const attemptElapsedMs = this.#elapsedMilliseconds()
    this.#sideSequence += 1
    const diagnostic = Object.freeze({
      schemaVersion: V2_CONNECTIVITY_DIAGNOSTIC_SCHEMA_VERSION,
      sessionId: this.#sessionId,
      peerPathId: this.#peerPathId,
      attemptId: this.#attemptId,
      side: 'browser' as const,
      sideSequence: this.#sideSequence,
      attemptElapsedMs,
      ...payload,
    }) as V2BrowserConnectivityAttemptDiagnostic
    try {
      observer(diagnostic)
    } catch {
      // Diagnostic consumers cannot alter authenticated connectivity or cleanup.
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

function snapshotCandidateCounts(candidateCounts: V2CandidateCounts): V2CandidateCounts {
  return Object.freeze({
    localEmitted: candidateCounts.localEmitted,
    remoteAccepted: candidateCounts.remoteAccepted,
  })
}

function snapshotLane(lane: V2LaneIdentity): V2LaneIdentity {
  return Object.freeze({ laneId: lane.laneId, laneEpoch: lane.laneEpoch })
}

function sameLane(left: V2LaneIdentity, right: V2LaneIdentity): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch
}
