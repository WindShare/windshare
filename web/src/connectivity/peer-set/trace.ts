import type { FailureCorrelation } from '../../diagnostics/incident/fact'
import type { V2PeerAttemptIdentity, V2PeerPathIdentity, V2ProtocolSessionIdentity } from '../../session/v2-identities'
import type { V2ConnectivityTraceSource, V2PeerRecoveryTraceEvent } from '../diagnostics'
export type V2PeerRecoveryTracePayload<Event = V2PeerRecoveryTraceEvent> =
  Event extends V2PeerRecoveryTraceEvent ? Omit<Event, 'eventName' | 'correlation'> : never
export class PeerRecoveryTrace {
  readonly #protocolSessionId: V2ProtocolSessionIdentity
  readonly #peerPathId: V2PeerPathIdentity
  readonly #trace: V2ConnectivityTraceSource | undefined
  constructor(session: V2ProtocolSessionIdentity, path: V2PeerPathIdentity, trace?: V2ConnectivityTraceSource) {
    this.#protocolSessionId = session
    this.#peerPathId = path
    this.#trace = trace
  }
  emit(
    createPayload: () => V2PeerRecoveryTracePayload,
    attemptId?: V2PeerAttemptIdentity,
    lane?: { readonly laneId: number; readonly laneEpoch: number },
  ): void {
    try {
      const observer = this.#trace?.current
      if (observer === undefined) return
      const correlation: FailureCorrelation = Object.freeze({
        protocolSessionId: this.#protocolSessionId,
        peerPathId: this.#peerPathId,
        ...(attemptId === undefined ? {} : { peerAttemptId: attemptId }),
        ...(lane === undefined
          ? {}
          : { lane: Object.freeze({ id: lane.laneId, epoch: lane.laneEpoch }) }),
      })
      observer(Object.freeze({
        eventName: 'peer_recovery',
        correlation,
        ...createPayload(),
      }) as V2PeerRecoveryTraceEvent)
    } catch {
      // Trace loss cannot perturb retry, budget, or session authority.
    }
  }
}
