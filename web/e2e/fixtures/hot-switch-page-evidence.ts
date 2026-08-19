import type {
  HotSwitchLaneObservation,
  HotSwitchPageEvent,
  HotSwitchPeerAttemptEvidence,
  HotSwitchPeerRecoveryEvidence,
} from './hot-switch-contract'
import { OutputFence } from './hot-switch-page-transfer'
import type {
  V2PeerAttemptTraceEvent,
  V2PeerRecoveryTraceEvent,
} from '../../src/connectivity/diagnostics'
import type {
  BeginOutputFileResult,
  OutputFileRequest,
  OutputFileTransaction,
  OutputSession,
} from '../../src/transfer/output-session'

type StreamModule = typeof import('../../src/output/streams/single-file')

export class EvidenceBridge {
  readonly #bridge: (event: HotSwitchPageEvent) => Promise<void>
  readonly #maximumFailureDepth: number
  #failure: string | undefined
  #queue = Promise.resolve()

  constructor(
    bridge: (event: HotSwitchPageEvent) => Promise<void>,
    maximumFailureDepth: number,
  ) {
    this.#bridge = bridge
    this.#maximumFailureDepth = maximumFailureDepth
  }

  publish(event: HotSwitchPageEvent): Promise<void> {
    this.#queue = this.#queue
      .then(() => this.#bridge(event))
      .catch((error: unknown) => {
        this.#failure ??= describeFailure(error, this.#maximumFailureDepth)
      })
    return this.#queue
  }

  async terminalFailure(): Promise<string | undefined> {
    // Observer callbacks deliberately do not block product control flow. The
    // terminal drains their serialized bridge so a rejected event cannot vanish.
    await this.#queue
    return this.#failure
  }

  describe(reason: unknown): string {
    return describeFailure(reason, this.#maximumFailureDepth)
  }
}

export class RelayCutEvidence {
  readonly #bridge: EvidenceBridge
  readonly #activeRelayLanes = new Set<string>()
  #ineligibilityPublished = false
  #sealed = false

  constructor(bridge: EvidenceBridge) {
    this.#bridge = bridge
  }

  admit(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'relay') this.#activeRelayLanes.add(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-admitted', observation }).catch(() => undefined)
  }

  detach(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'relay') this.#activeRelayLanes.delete(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-detached', observation })
      .then(() => this.#publishIneligibility())
      .catch(() => undefined)
  }

  async seal(): Promise<void> {
    this.#sealed = true
    await this.#publishIneligibility()
  }

  async #publishIneligibility(): Promise<void> {
    if (
      !this.#sealed || this.#ineligibilityPublished || this.#activeRelayLanes.size !== 0
    ) return
    this.#ineligibilityPublished = true
    await this.#bridge.publish({ kind: 'relay-ineligible' })
  }
}

export class DeliveryBuffer {
  readonly #chunks: Uint8Array[] = []

  outputSession(
    stream: StreamModule,
    outputSessionId: string,
    outputFence: OutputFence,
  ): DirectAtomicStreamOutput {
    const session = new stream.SingleFileStreamOutputSession(
      outputSessionId,
      new WritableStream<Uint8Array>({
        write: async (chunk) => {
          await outputFence.waitForWrite()
          this.#chunks.push(chunk.slice())
        },
      }),
    )
    return new DirectAtomicStreamOutput(session)
  }

  async snapshot(): Promise<{ readonly bytes: number; readonly sha256: string }> {
    const length = this.#chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
    const bytes = new Uint8Array(length)
    let offset = 0
    for (const chunk of this.#chunks) {
      bytes.set(chunk, offset)
      offset += chunk.byteLength
    }
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
    return Object.freeze({
      bytes: length,
      sha256: Array.from(digest, (byte) => byte.toString(16).padStart(2, '0')).join(''),
    })
  }
}

export class DirectAtomicStreamOutput implements OutputSession {
  readonly identity
  readonly capabilities
  readonly #session: OutputSession
  #transaction: OutputFileTransaction | undefined

  constructor(session: OutputSession) {
    this.#session = session
    this.identity = session.identity
    this.capabilities = session.capabilities
  }

  async beginFile(
    request: OutputFileRequest,
    signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
    const opened = await this.#session.beginFile(request, signal)
    this.#transaction = opened.transaction
    return opened
  }

  async pauseActiveTransaction(reason: unknown): Promise<void> {
    // OutputSession intentionally owns no job-wide pause. The plan route retains
    // the one transaction it opened so rollback remains at the authority boundary.
    await this.#transaction?.pause(reason)
  }
}

export function projectAttemptEvidence(
  event: V2PeerAttemptTraceEvent,
): HotSwitchPeerAttemptEvidence {
  const { correlation, ...payload } = event
  // The browser-test bridge keeps copied bytes instead of inventing a second
  // diagnostic encoding path; production JSON encoding remains projector-owned.
  return Object.freeze({
    ...payload,
    protocolSessionIdBytes: Object.freeze([
      ...correlation.protocolSessionId.copyBytes(),
    ]),
    peerPathIdBytes: Object.freeze([...correlation.peerPathId.copyBytes()]),
    attemptIdBytes: Object.freeze([...correlation.peerAttemptId.copyBytes()]),
    ...(correlation.protocolOperationId === undefined
      ? {}
      : {
          operationIdBytes: Object.freeze([
            ...correlation.protocolOperationId.copyBytes(),
          ]),
        }),
    ...(correlation.lane === undefined
      ? {}
      : {
          lane: Object.freeze({
            laneId: correlation.lane.id,
            laneEpoch: correlation.lane.epoch,
          }),
        }),
  }) as HotSwitchPeerAttemptEvidence
}

export function projectRecoveryEvidence(
  event: V2PeerRecoveryTraceEvent,
): HotSwitchPeerRecoveryEvidence {
  if (event.stage === 'attempt-replaced') {
    const { correlation, previousAttemptId, ...payload } = event
    return Object.freeze({
      ...payload,
      ...projectRecoveryCorrelation(correlation),
      previousAttemptIdBytes: Object.freeze([...previousAttemptId.copyBytes()]),
    }) as HotSwitchPeerRecoveryEvidence
  }
  const { correlation, ...payload } = event
  return Object.freeze({
    ...payload,
    ...projectRecoveryCorrelation(correlation),
  }) as HotSwitchPeerRecoveryEvidence
}

function projectRecoveryCorrelation(
  correlation: V2PeerRecoveryTraceEvent['correlation'],
) {
  return {
    protocolSessionIdBytes: Object.freeze([
      ...correlation.protocolSessionId.copyBytes(),
    ]),
    peerPathIdBytes: Object.freeze([...correlation.peerPathId.copyBytes()]),
    ...(correlation.peerAttemptId === undefined
      ? {}
      : {
          attemptIdBytes: Object.freeze([
            ...correlation.peerAttemptId.copyBytes(),
          ]),
        }),
    ...(correlation.lane === undefined
      ? {}
      : {
          lane: Object.freeze({
            laneId: correlation.lane.id,
            laneEpoch: correlation.lane.epoch,
          }),
        }),
  }
}

function describeFailure(reason: unknown, maximumDepth: number, depth = 0): string {
  if (depth >= maximumDepth) return '[nested failure truncated]'
  if (reason instanceof AggregateError) {
    const nested = reason.errors.map((error) => describeFailure(error, maximumDepth, depth + 1))
    const summary = `${reason.name}: ${reason.message}`
    const failures = nested.length === 0 ? summary : `${summary}; errors=[${nested.join(' | ')}]`
    return reason.cause === undefined
      ? failures
      : `${failures}; cause=${describeFailure(reason.cause, maximumDepth, depth + 1)}`
  }
  if (reason instanceof Error) {
    const summary = `${reason.name}: ${reason.message}`
    return reason.cause === undefined
      ? summary
      : `${summary}; cause=${describeFailure(reason.cause, maximumDepth, depth + 1)}`
  }
  try {
    return String(reason)
  } catch {
    return '[unprintable non-Error failure]'
  }
}

function laneKey(lane: { readonly laneId: number; readonly laneEpoch: number }): string {
  return `${lane.laneId}/${lane.laneEpoch}`
}
