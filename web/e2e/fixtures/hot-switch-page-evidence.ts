import type {
  HotSwitchDispatch,
  HotSwitchLaneObservation,
  HotSwitchPageEvent,
  HotSwitchPeerAttemptEvidence,
  HotSwitchPeerRecoveryEvidence,
} from './hot-switch-contract'
import type { OutputFence } from './hot-switch-page-transfer'
import type {
  PeerAttemptTraceEvent,
  PeerRecoveryTraceEvent,
} from '../../src/connectivity/diagnostics'
import { bigintToSafeNumber } from '../../src/content/geometry'
import type { TransferFileRequest, TransferOutputFile, TransferOutputSession } from '../../src/transfer/output-port'

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
  #latestDispatchSequence = 0

  constructor(bridge: EvidenceBridge) {
    this.#bridge = bridge
  }

  dispatch(observation: HotSwitchDispatch): void {
    this.#latestDispatchSequence = Math.max(
      this.#latestDispatchSequence,
      observation.dispatchSequence,
    )
    this.#bridge.publish({ kind: 'dispatch', observation }).catch(() => undefined)
  }

  admit(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'application-relay') this.#activeRelayLanes.add(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-admitted', observation }).catch(() => undefined)
  }

  detach(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'application-relay') this.#activeRelayLanes.delete(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-detached', observation }).catch(() => undefined)
    this.#publishIneligibility().catch(() => undefined)
  }

  async seal(): Promise<void> {
    this.#sealed = true
    await this.#publishIneligibility()
  }

  async #publishIneligibility(): Promise<void> {
    if (!this.#sealed || this.#ineligibilityPublished || this.#activeRelayLanes.size !== 0) return
    this.#ineligibilityPublished = true
    await this.#bridge.publish({
      kind: 'relay-ineligible',
      dispatchSequenceBoundary: this.#latestDispatchSequence,
    })
  }
}

export class DeliveryBuffer {
  #buffer: Uint8Array<ArrayBuffer> = new Uint8Array(0)
  #writtenBytes = 0

  outputSession(
    sessionId: string,
    outputFence: OutputFence,
  ): TransferOutputSession {
    return Object.freeze({
      sessionId,
      capabilities: Object.freeze({
        maxConcurrentFiles: 1,
      }),
      openFile: async (request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> => {
        signal.throwIfAborted()
        const exactSize = bigintToSafeNumber(request.exactSize, 'hot-switch delivery size')
        this.#buffer = new Uint8Array(exactSize)
        this.#writtenBytes = 0
        return Object.freeze({
          completedRanges: Object.freeze([]),
          writeAt: async (offset: bigint, data: Uint8Array) => {
            await outputFence.waitForWrite()
            const start = bigintToSafeNumber(offset, 'hot-switch write offset')
            this.#buffer.set(data, start)
            this.#writtenBytes = Math.max(this.#writtenBytes, start + data.byteLength)
          },
          checkpoint: () => Promise.resolve(),
          commit: () => Promise.resolve(),
          pause: () => Promise.resolve(),
          abort: () => Promise.resolve(),
        })
      },
    })
  }

  async snapshot(): Promise<{ readonly bytes: number; readonly sha256: string }> {
    const view = this.#buffer.subarray(0, this.#writtenBytes)
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', view))
    return Object.freeze({
      bytes: view.byteLength,
      sha256: Array.from(digest, byte => byte.toString(16).padStart(2, '0')).join(''),
    })
  }
}

export function projectAttemptEvidence(
  event: PeerAttemptTraceEvent,
): HotSwitchPeerAttemptEvidence {
  const { correlation, ...payload } = event
  return Object.freeze({
    ...payload,
    protocolSessionIdBytes: Object.freeze([...correlation.protocolSessionId.copyBytes()]),
    peerPathIdBytes: Object.freeze([...correlation.peerPathId.copyBytes()]),
    attemptIdBytes: Object.freeze([...correlation.peerAttemptId.copyBytes()]),
    ...(correlation.protocolOperationId === undefined
      ? {}
      : {
          operationId: correlation.protocolOperationId.toDecimalString(),
        }),
    ...(correlation.lane === undefined
      ? {}
      : { lane: Object.freeze({ laneId: correlation.lane.id, laneEpoch: correlation.lane.epoch }) }),
  }) as HotSwitchPeerAttemptEvidence
}

export function projectRecoveryEvidence(
  event: PeerRecoveryTraceEvent,
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
  correlation: PeerRecoveryTraceEvent['correlation'],
) {
  return {
    protocolSessionIdBytes: Object.freeze([...correlation.protocolSessionId.copyBytes()]),
    peerPathIdBytes: Object.freeze([...correlation.peerPathId.copyBytes()]),
    ...(correlation.peerAttemptId === undefined
      ? {}
      : { attemptIdBytes: Object.freeze([...correlation.peerAttemptId.copyBytes()]) }),
    ...(correlation.lane === undefined
      ? {}
      : { lane: Object.freeze({ laneId: correlation.lane.id, laneEpoch: correlation.lane.epoch }) }),
  }
}

function describeFailure(reason: unknown, maximumDepth: number, depth = 0): string {
  if (depth >= maximumDepth) return '[nested failure truncated]'
  if (reason instanceof AggregateError) {
    const nested = reason.errors.map(error => describeFailure(error, maximumDepth, depth + 1))
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
