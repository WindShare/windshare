import { encodeBase64Url } from '../crypto/bytes'
import type {
  AuthenticatedSenderOperationFailureEvidence,
  BrowserAttemptEvidence,
  BrowserSelectedPairEvidence,
  CandidateCounts,
  LaneIdentity,
} from '../../scripts/browser-evidence/attempt-evidence'
import {
  BROWSER_ATTEMPT_STAGES,
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  typedErrorForPeerOperationCode,
  type BrowserAttemptStage,
  type TypedPeerErrorCode,
} from '../../scripts/browser-evidence/vocabulary'
import {
  decodeV2OperationErrorControl,
  V2MessageError,
  type V2SessionMessage,
  V2_MESSAGE_KIND,
} from '../session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../session/v2-runtime'
import {
  V2_OPERATION_CANCEL_REASON,
  V2SessionRuntimeError,
} from '../session/v2-runtime-types'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
} from './errors'
import type { V2PeerOfferAttemptObserver } from './peer-offer'
import {
  decodeV2PeerAnswer,
  decodeV2PeerCandidate,
  encodeV2PeerCandidate,
  encodeV2PeerOffer,
  sameV2PeerBinding,
  type V2PeerBinding,
  V2SignalingCodecError,
} from './v2-signaling-codec'
import {
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
  type SignalingRoute,
} from './signaling'
import {
  V2AuthenticatedPeerOperationError,
  V2PeerProtocolError,
  V2SessionSignalingError,
} from './v2-session-signaling-errors'
import { awaitPeerEvidence } from './abortable-peer-evidence'

export {
  V2AuthenticatedPeerOperationError,
  V2SessionSignalingError,
} from './v2-session-signaling-errors'

type V2SessionSignalingDecision = {
  readonly type: 'route-failed'
  readonly failureScope: 'attempt' | 'session'
  readonly reason: unknown
}

export type V2SessionSignalingTrace = V2SessionSignalingDecision & {
  readonly peerPathId: string
  readonly attemptId: string
}

export type V2SessionSignalingObserver = (event: V2SessionSignalingTrace) => void
export type V2ConnectivityObserver = (event: BrowserAttemptEvidence) => void

/**
 * Projects one PeerConnection negotiation over a single authenticated operation.
 * Path and attempt identities live here so content/session scheduling never learns
 * provider or ICE policy, while every trickled candidate remains bound to the offer.
 */
export class V2SessionSignalingRoute implements SignalingRoute, V2PeerOfferAttemptObserver {
  readonly messages: ReadableStream<ConnectivitySignal>
  readonly binding: V2PeerBinding
  readonly #session: V2ReceiverSessionRuntime
  readonly #controller: ReadableStreamDefaultController<ConnectivitySignal>
  readonly #observe: V2SessionSignalingObserver
  readonly #attempt: BrowserAttemptLifecycle
  readonly #failureController = new AbortController()
  #operation: V2SessionOperation | undefined
  #failure: { readonly reason: unknown } | undefined
  #offerStarted = false
  #closed = false
  #answerSeen = false

  constructor(
    session: V2ReceiverSessionRuntime,
    binding: V2PeerBinding,
    observe: V2SessionSignalingObserver = () => undefined,
    observeConnectivity?: V2ConnectivityObserver,
    now: () => number = () => performance.now(),
  ) {
    this.#session = session
    this.binding = snapshotBinding(binding)
    this.#observe = observe
    let controller!: ReadableStreamDefaultController<ConnectivitySignal>
    this.messages = new ReadableStream<ConnectivitySignal>({
      start: (candidate) => {
        controller = candidate
      },
      cancel: (reason) => this.close(reason),
    })
    this.#controller = controller
    // Started is published only after construction can no longer strand a
    // partially initialized route without a terminal owner.
    this.#attempt = new BrowserAttemptLifecycle(
      session,
      this.binding,
      observeConnectivity,
      now,
    )
  }

  async send(message: ConnectivitySignal, signal?: AbortSignal): Promise<void> {
    this.#requireOpen()
    signal?.throwIfAborted()
    if (message.kind === SIGNAL_KIND_OFFER) {
      await this.#startOffer(message.payload, signal)
      return
    }
    if (message.kind !== SIGNAL_KIND_CANDIDATE) {
      throw new V2SessionSignalingError(`Receiver cannot send signal kind ${message.kind}`)
    }
    const operation = this.#operation
    if (operation === undefined) {
      throw new V2SessionSignalingError('ICE candidate arrived before the peer offer operation')
    }
    const candidate = candidatePayload(message.payload)
    await this.#session.sendOperationMessage(
      operation,
      V2_MESSAGE_KIND.peerCandidate,
      encodeV2PeerCandidate({ ...this.binding, ...candidate }),
      signal === undefined ? {} : { signal },
    )
  }

  offerCreated(candidateCounts: CandidateCounts): void {
    this.#attempt.advance('offer-created', { candidateCounts })
  }

  offerSent(candidateCounts: CandidateCounts): void {
    this.#attempt.advance('offer-sent', { candidateCounts })
  }

  answerReceived(candidateCounts: CandidateCounts): void {
    this.#attempt.advance('answer-received', { candidateCounts })
  }

  dataChannelOpened(
    candidateCounts: CandidateCounts,
    readSelectedPair: () => Promise<BrowserSelectedPairEvidence | null>,
  ): void {
    this.#attempt.registerSelectedPairReader(readSelectedPair)
    this.#attempt.advance('datachannel-open', { candidateCounts })
  }

  laneGranted(lane: LaneIdentity): void {
    this.#attempt.advance('lane-granted', {
      candidateCounts: this.#attempt.candidateCounts,
      lane,
    })
  }

  laneAttached(lane: LaneIdentity): void {
    this.#attempt.advance('lane-attached', {
      candidateCounts: this.#attempt.candidateCounts,
      lane,
    })
  }

  async readSelectedPair(signal?: AbortSignal): Promise<BrowserSelectedPairEvidence | null> {
    return this.#attempt.readSelectedPair(signal)
  }

  /** Admission observes this independently of optional evidence consumers. */
  get attemptFailureSignal(): AbortSignal {
    return this.#failureController.signal
  }

  throwIfAttemptFailed(): void {
    if (this.#failure !== undefined) throw this.#failure.reason
  }

  admitted(lane: LaneIdentity, selectedPair: BrowserSelectedPairEvidence | null): void {
    this.#attempt.admitted(lane, selectedPair)
  }

  failAttempt(
    reason: unknown,
    options: {
      readonly failureScope?: 'attempt' | 'session'
      readonly typedErrorCode?: TypedPeerErrorCode
    } = {},
  ): void {
    this.#attempt.failed(
      reason,
      options.failureScope ?? this.#attempt.failureScope,
      options.typedErrorCode,
    )
  }

  async close(
    cause: unknown = new DOMException('Peer signaling route closed', 'AbortError'),
  ): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    const operation = this.#operation
    this.#operation = undefined
    if (operation !== undefined) {
      await this.#session.cancelOperation(operation, {
        protocolReason: V2_OPERATION_CANCEL_REASON.superseded,
        cause,
      }).catch(() => undefined)
    }
    try {
      this.#controller.close()
    } catch {
      // A consumer-originated cancellation already owns stream settlement.
    }
  }

  async #startOffer(payload: unknown, signal?: AbortSignal): Promise<void> {
    if (this.#offerStarted) throw new V2SessionSignalingError('Peer offer was sent more than once')
    this.#offerStarted = true
    const sdp = descriptionPayload(payload, SIGNAL_KIND_OFFER)
    try {
      const operation = await this.#session.beginOperation(
        V2_MESSAGE_KIND.peerOffer,
        encodeV2PeerOffer({ ...this.binding, sdp }),
        signal === undefined ? {} : { signal },
      )
      if (this.#closed) {
        await this.#session.cancelOperation(operation, {
          protocolReason: V2_OPERATION_CANCEL_REASON.superseded,
          cause: new DOMException('Peer signaling route closed before offer startup', 'AbortError'),
        }).catch(() => undefined)
        return
      }
      this.#operation = operation
      this.#pump(operation).catch(() => undefined)
    } catch (error) {
      this.#fail(error)
      throw error
    }
  }

  async #pump(operation: V2SessionOperation): Promise<void> {
    try {
      while (!this.#closed && this.#operation === operation) {
        await this.#acceptSenderMessage(await operation.next())
      }
    } catch (error) {
      this.#handlePumpFailure(error)
    }
  }

  async #acceptSenderMessage(message: V2SessionMessage): Promise<void> {
    if (message.kind === V2_MESSAGE_KIND.operationError) {
      const failure = decodeV2OperationErrorControl(message.body)
      if (failure.scope !== 'peer') {
        throw new V2PeerProtocolError('Peer operation received an error from another scope')
      }
      throw new V2AuthenticatedPeerOperationError(Object.freeze({ ...failure, scope: 'peer' }))
    }
    if (message.kind === V2_MESSAGE_KIND.peerAnswer) {
      if (this.#answerSeen) throw new V2PeerProtocolError('Sender sent more than one peer answer')
      const answer = decodeV2PeerAnswer(message.body)
      this.#requireBinding(answer)
      this.#answerSeen = true
      this.#controller.enqueue({
        kind: SIGNAL_KIND_ANSWER,
        payload: { type: SIGNAL_KIND_ANSWER, sdp: answer.sdp },
      })
      return
    }
    if (message.kind !== V2_MESSAGE_KIND.peerCandidate) {
      throw new V2PeerProtocolError('Peer operation delivered an unexpected response')
    }
    const candidate = decodeV2PeerCandidate(message.body)
    this.#requireBinding(candidate)
    if (this.#closed) return
    this.#controller.enqueue({
      kind: SIGNAL_KIND_CANDIDATE,
      payload: {
        candidate: candidate.candidate,
        sdpMid: candidate.sdpMid,
        sdpMLineIndex: candidate.sdpMLineIndex,
        usernameFragment: candidate.usernameFragment,
      },
    })
  }

  #handlePumpFailure(error: unknown): void {
    if (this.#closed) return
    if (
      error instanceof V2PeerProtocolError ||
      error instanceof V2SignalingCodecError ||
      error instanceof V2MessageError ||
      (error instanceof V2SessionRuntimeError && error.scope === 'session')
    ) {
      this.#session.close().catch(() => undefined)
      this.#fail(
        new V2SessionRuntimeError(
          'session',
          'Authenticated peer signaling violated its operation binding',
          { cause: error },
        ),
        'session',
      )
      return
    }
    this.#fail(error)
  }

  #requireBinding(candidate: V2PeerBinding): void {
    if (!sameV2PeerBinding(candidate, this.binding)) {
      throw new V2PeerProtocolError('Sender signal changed peer path or attempt identity')
    }
  }

  #fail(reason: unknown, failureScope: 'attempt' | 'session' = 'attempt'): void {
    if (this.#closed) return
    this.#closed = true
    this.#failure = Object.freeze({ reason })
    this.#failureController.abort(reason)
    this.#attempt.setFailureScope(failureScope)
    this.#attempt.failed(reason, failureScope)
    this.#trace({ type: 'route-failed', failureScope, reason })
    const operation = this.#operation
    this.#operation = undefined
    operation?.cancel(reason)
    try {
      this.#controller.error(reason)
    } catch {
      // The stream consumer may already have released the signaling route.
    }
  }

  #requireOpen(): void {
    if (this.#closed) throw new V2SessionSignalingError('Peer signaling route is closed')
  }

  #trace(event: V2SessionSignalingDecision): void {
    try {
      this.#observe(Object.freeze({
        ...event,
        peerPathId: encodeBase64Url(this.binding.peerPathId),
        attemptId: encodeBase64Url(this.binding.attemptId),
      }) as V2SessionSignalingTrace)
    } catch {
      // Diagnostics cannot own or destabilize authenticated attempt lifecycle.
    }
  }
}

const BROWSER_SUCCESS_STAGES = BROWSER_ATTEMPT_STAGES.filter(
  (stage): stage is Exclude<BrowserAttemptStage, 'failed'> => stage !== 'failed',
)
const MAXIMUM_DIAGNOSTIC_TEXT_BYTES = 512

type BrowserMilestonePayload = {
  readonly candidateCounts: CandidateCounts
  readonly lane?: LaneIdentity
}

class BrowserAttemptLifecycle {
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
  #candidateCounts: CandidateCounts | undefined
  #lane: LaneIdentity | undefined
  #selectedPairReader: (() => Promise<BrowserSelectedPairEvidence | null>) | undefined
  #selectedPairPromise: Promise<BrowserSelectedPairEvidence | null> | undefined
  #selectedPair: BrowserSelectedPairEvidence | null = null
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

  get candidateCounts(): CandidateCounts {
    return this.#candidateCounts ?? Object.freeze({ localEmitted: 0, remoteAccepted: 0 })
  }

  get failureScope(): 'attempt' | 'session' {
    return this.#failureScope
  }

  setFailureScope(scope: 'attempt' | 'session'): void {
    if (!this.#terminal) this.#failureScope = scope
  }

  advance(
    stage: Exclude<BrowserAttemptStage, 'started' | 'admitted' | 'failed'>,
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
    reader: () => Promise<BrowserSelectedPairEvidence | null>,
  ): void {
    this.#selectedPairReader ??= reader
  }

  async readSelectedPair(signal?: AbortSignal): Promise<BrowserSelectedPairEvidence | null> {
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

  admitted(lane: LaneIdentity, selectedPair: BrowserSelectedPairEvidence | null): void {
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
    typedErrorCode?: TypedPeerErrorCode,
  ): void {
    if (this.#observer === undefined || this.#terminal) return
    const failedAtStage = BROWSER_SUCCESS_STAGES[this.#nextStageIndex]
    if (failedAtStage === undefined || failedAtStage === 'started') return
    const authenticated = authenticatedSenderFailure(reason)
    const scope = authenticated === undefined ? failureScope : 'attempt'
    const code = typedErrorCode ?? classifyTypedFailure(reason, failedAtStage, scope)
    const failureMessage = authenticated?.message ?? boundedDiagnosticMessage(reason)
    this.#terminal = true
    this.#emit({
      stage: 'failed',
      failedAtStage,
      failureScope: scope,
      typedErrorCode: code,
      failureMessage,
      ...(this.#candidateCounts === undefined || failedAtStage === 'offer-created'
        ? {}
        : { candidateCounts: this.#candidateCounts }),
      ...(this.#lane === undefined || (failedAtStage !== 'lane-attached' && failedAtStage !== 'admitted')
        ? {}
        : { lane: this.#lane }),
      ...(failedAtStage === 'admitted' && this.#selectedPairRead
        ? { selectedPair: this.#selectedPair }
        : {}),
      ...(authenticated === undefined
        ? {}
        : { authenticatedSenderOperationFailure: authenticated }),
    })
  }

  #expect(stage: Exclude<BrowserAttemptStage, 'started' | 'failed'>): boolean {
    const expected = BROWSER_SUCCESS_STAGES[this.#nextStageIndex]
    if (expected === stage) return true
    this.failed(
      new Error(`Connectivity milestone ${stage} arrived while ${expected ?? 'terminal'} was expected`),
      'attempt',
      'unexpected',
    )
    return false
  }

  async #readSelectedPair(): Promise<BrowserSelectedPairEvidence | null> {
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
    const evidence = Object.freeze({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      sessionId: this.#sessionId,
      peerPathId: this.#peerPathId,
      attemptId: this.#attemptId,
      side: 'browser' as const,
      sideSequence: this.#sideSequence,
      attemptElapsedMs,
      ...payload,
    }) as BrowserAttemptEvidence
    try {
      observer(evidence)
    } catch {
      // Evidence consumers cannot alter authenticated connectivity or cleanup.
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

function snapshotCandidateCounts(candidateCounts: CandidateCounts): CandidateCounts {
  return Object.freeze({
    localEmitted: candidateCounts.localEmitted,
    remoteAccepted: candidateCounts.remoteAccepted,
  })
}

function snapshotLane(lane: LaneIdentity): LaneIdentity {
  return Object.freeze({ laneId: lane.laneId, laneEpoch: lane.laneEpoch })
}

function sameLane(left: LaneIdentity, right: LaneIdentity): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch
}

function authenticatedSenderFailure(
  reason: unknown,
): AuthenticatedSenderOperationFailureEvidence | undefined {
  const error = findNestedError<V2AuthenticatedPeerOperationError>(
    reason,
    (candidate): candidate is V2AuthenticatedPeerOperationError =>
      candidate instanceof V2AuthenticatedPeerOperationError,
  )
  if (error === undefined) return undefined
  const typed = typedErrorForPeerOperationCode(error.operationFailure.code)
  if (typed === undefined) return undefined
  return Object.freeze({
    scope: 'peer',
    code: error.operationFailure.code,
    message: error.operationFailure.message,
  })
}

function classifyTypedFailure(
  reason: unknown,
  failedAtStage: Exclude<BrowserAttemptStage, 'started' | 'failed'>,
  failureScope: 'attempt' | 'session',
): TypedPeerErrorCode {
  const authenticated = findNestedError<V2AuthenticatedPeerOperationError>(
    reason,
    (candidate): candidate is V2AuthenticatedPeerOperationError =>
      candidate instanceof V2AuthenticatedPeerOperationError,
  )
  if (authenticated !== undefined) {
    return typedErrorForPeerOperationCode(authenticated.operationFailure.code) ?? 'unexpected'
  }
  if (failureScope === 'session') return 'signaling-contract'
  const timeout = findNestedError(reason, (candidate) =>
    candidate instanceof DOMException && candidate.name === 'TimeoutError')
  if (timeout !== undefined) return 'peer-timeout'
  const cancelled = findNestedError(reason, (candidate) =>
    candidate instanceof DOMException && candidate.name === 'AbortError')
  if (cancelled !== undefined) return 'attempt-cancelled'
  const candidateFailure = findNestedError<CandidateLimitExceededError>(
    reason,
    (candidate): candidate is CandidateLimitExceededError =>
      candidate instanceof CandidateLimitExceededError,
  )
  if (candidateFailure !== undefined) return 'peer-candidates'
  const runtime = findNestedError<V2SessionRuntimeError>(
    reason,
    (candidate): candidate is V2SessionRuntimeError => candidate instanceof V2SessionRuntimeError,
  )
  if (runtime?.scope === 'session') return 'signaling-contract'
  if (runtime?.scope === 'lane') return 'peer-admission'
  if (failedAtStage === 'lane-granted' || failedAtStage === 'lane-attached' ||
      failedAtStage === 'admitted') return 'peer-admission'
  const signaling = findNestedError(reason, (candidate) =>
    candidate instanceof V2SessionSignalingError || candidate instanceof V2MessageError)
  if (signaling !== undefined) return 'signaling-contract'
  const negotiation = findNestedError<PeerNegotiationError>(
    reason,
    (candidate): candidate is PeerNegotiationError => candidate instanceof PeerNegotiationError,
  )
  return negotiation === undefined ? 'unexpected' : 'peer-negotiation'
}

function findNestedError<T extends Error = Error>(
  reason: unknown,
  accept: (candidate: Error) => boolean,
  seen: Set<unknown> = new Set(),
): T | undefined {
  if (!(reason instanceof Error) || seen.has(reason)) return undefined
  seen.add(reason)
  if (accept(reason)) return reason as T
  if (reason instanceof AggregateError) {
    for (const nested of reason.errors) {
      const match = findNestedError<T>(nested, accept, seen)
      if (match !== undefined) return match
    }
  }
  return findNestedError<T>(reason.cause, accept, seen)
}

function boundedDiagnosticMessage(reason: unknown): string {
  const message = diagnosticMessage(reason)
  let result = ''
  for (const character of message || 'Peer attempt failed') {
    if (new TextEncoder().encode(result + character).byteLength > MAXIMUM_DIAGNOSTIC_TEXT_BYTES) break
    result += character
  }
  return result || 'Peer attempt failed'
}

function diagnosticMessage(reason: unknown): string {
  if (reason instanceof Error) return reason.message
  if (typeof reason === 'string') return reason
  return 'Peer attempt failed without a diagnostic message'
}

export function createV2PeerBinding(
  randomBytes: (length: number) => Uint8Array = secureRandomBytes,
): V2PeerBinding {
  return Object.freeze({
    peerPathId: nonzeroIdentity(randomBytes),
    attemptId: nonzeroIdentity(randomBytes),
  })
}

function descriptionPayload(payload: unknown, expectedType: 'offer'): string {
  if (
    typeof payload !== 'object' || payload === null ||
    !('type' in payload) || payload.type !== expectedType ||
    !('sdp' in payload) || typeof payload.sdp !== 'string'
  ) {
    throw new V2SessionSignalingError('Peer offer payload is malformed')
  }
  return payload.sdp
}

function candidatePayload(payload: unknown): {
  readonly candidate: string
  readonly sdpMid: string | null
  readonly sdpMLineIndex: number | null
  readonly usernameFragment: string | null
} {
  if (
    typeof payload !== 'object' || payload === null ||
    !('candidate' in payload) || typeof payload.candidate !== 'string'
  ) {
    throw new V2SessionSignalingError('ICE candidate payload is malformed')
  }
  return Object.freeze({
    candidate: payload.candidate,
    sdpMid: optionalStringProperty(payload, 'sdpMid'),
    sdpMLineIndex: optionalNumberProperty(payload, 'sdpMLineIndex'),
    usernameFragment: optionalStringProperty(payload, 'usernameFragment'),
  })
}

function optionalStringProperty(value: object, key: string): string | null {
  if (!(key in value) || value[key as keyof typeof value] === undefined) return null
  const candidate = value[key as keyof typeof value]
  if (candidate !== null && typeof candidate !== 'string') {
    throw new V2SessionSignalingError(`${key} must be text or null`)
  }
  return candidate as string | null
}

function optionalNumberProperty(value: object, key: string): number | null {
  if (!(key in value) || value[key as keyof typeof value] === undefined) return null
  const candidate = value[key as keyof typeof value]
  if (candidate !== null && typeof candidate !== 'number') {
    throw new V2SessionSignalingError(`${key} must be a number or null`)
  }
  return candidate as number | null
}

function snapshotBinding(binding: V2PeerBinding): V2PeerBinding {
  if (
    binding.peerPathId.byteLength !== 16 || binding.attemptId.byteLength !== 16 ||
    !binding.peerPathId.some((item) => item !== 0) ||
    !binding.attemptId.some((item) => item !== 0)
  ) {
    throw new V2SessionSignalingError('Peer path and attempt IDs must be nonzero 16-byte identities')
  }
  return Object.freeze({
    peerPathId: binding.peerPathId.slice(),
    attemptId: binding.attemptId.slice(),
  })
}

function nonzeroIdentity(randomBytes: (length: number) => Uint8Array): Uint8Array<ArrayBuffer> {
  for (let attempt = 0; attempt < 4; attempt += 1) {
    const value = randomBytes(16)
    if (value.byteLength === 16 && value.some((item) => item !== 0)) return value.slice()
  }
  throw new V2SessionSignalingError('Random source did not produce a peer identity')
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(length)
  globalThis.crypto.getRandomValues(result)
  return result
}
