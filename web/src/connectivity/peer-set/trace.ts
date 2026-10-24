import type { FailureCorrelation } from '../../diagnostics/incident/failure-correlation'
import type { PeerAttemptIdentity, PeerPathIdentity, ProtocolSessionIdentity } from '../../session/identities'
import type { ConnectivityTraceSource, PeerRecoveryTraceEvent } from '../diagnostics'
export type PeerRecoveryTracePayload<Event = PeerRecoveryTraceEvent> =
  Event extends PeerRecoveryTraceEvent ? Omit<Event, 'eventName' | 'correlation'> : never
export class PeerRecoveryTrace {
  readonly #protocolSessionId: ProtocolSessionIdentity
  readonly #peerPathId: PeerPathIdentity
  readonly #trace: ConnectivityTraceSource | undefined
  constructor(session: ProtocolSessionIdentity, path: PeerPathIdentity, trace?: ConnectivityTraceSource) {
    this.#protocolSessionId = session
    this.#peerPathId = path
    this.#trace = trace
  }
  emit(
    createPayload: () => PeerRecoveryTracePayload,
    attemptId?: PeerAttemptIdentity,
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
      }) as PeerRecoveryTraceEvent)
    } catch {
      // Trace loss cannot perturb retry, budget, or session authority.
    }
  }
}
