import { createProtocolErrorContent } from '../diagnostics/incident/fact'
import { protocolMessageKindV1, type V2ProtocolOperationSettlement, type V2ProtocolTraceSource } from './v2-diagnostics'
import { createV2ProtocolOperationIdentity, type V2ProtocolSessionIdentity } from './v2-identities'
import { decodeV2OperationErrorControl, V2_MESSAGE_KIND, type V2MessageKind, type V2SessionMessage } from './v2-message'
import { V2OperationContinuationAuthority } from './v2-operation-continuation'
import type { snapshotOperationRequest, V2CancellationTraceReason } from './v2-operation-diagnostics'

export interface V2ProtocolTraceContext {
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
  readonly trace?: V2ProtocolTraceSource
}

export class V2OperationTombstone {
  readonly expiresAt: number
  readonly #authority: V2OperationContinuationAuthority
  readonly settlement: V2ProtocolOperationSettlement
  readonly requestTrace: ReturnType<typeof snapshotOperationRequest>
  readonly cancellationReason: V2CancellationTraceReason | undefined
  #responseTraced = false

  constructor(
    expiresAt: number,
    authority: V2OperationContinuationAuthority,
    settlement: V2ProtocolOperationSettlement,
    requestTrace: ReturnType<typeof snapshotOperationRequest>,
    cancellationReason: V2CancellationTraceReason | undefined,
  ) {
    this.expiresAt = expiresAt
    this.#authority = authority
    this.settlement = settlement
    this.requestTrace = requestTrace
    this.cancellationReason = cancellationReason
  }

  get requestKind(): V2MessageKind { return this.#authority.requestKind }

  #takeResponseTrace(): boolean {
    if (this.#responseTraced) return false
    this.#responseTraced = true
    return true
  }

  traceResponse(message: V2SessionMessage, diagnostics: V2ProtocolTraceContext | undefined, laneId?: number, laneEpoch?: number): void {
    const observer = diagnostics?.trace?.current
    if (observer === undefined || diagnostics === undefined || message.operationId === undefined ||
      (message.kind !== V2_MESSAGE_KIND.operationError && message.kind !== V2_MESSAGE_KIND.operationComplete) ||
      !this.#takeResponseTrace()) return
    // This is a discarded response, not an active operation failure. Publishing an
    // incident here would turn successful race cleanup into a failed download.
    try {
      const correlation = {
        protocolSessionId: diagnostics.protocolSessionIdentity,
        protocolOperationId: createV2ProtocolOperationIdentity(message.operationId),
        ...(laneId === undefined || laneEpoch === undefined ? {} : { lane: { id: laneId, epoch: laneEpoch } }),
      }
      const failure = message.kind === V2_MESSAGE_KIND.operationError ? decodeV2OperationErrorControl(message.body) : undefined
      observer(Object.freeze({
        eventName: 'protocol_operation',
        transition: 'late_response_discarded',
        requestKind: protocolMessageKindV1(this.requestKind),
        responseKind: protocolMessageKindV1(message.kind),
        settlement: this.settlement,
        ...(this.cancellationReason === undefined ? {} : { cancellationReason: this.cancellationReason }),
        ...(this.requestTrace === undefined ? {} : { request: this.requestTrace }),
        ...(failure === undefined ? {} : { protocolError: createProtocolErrorContent({
          scope: failure.scope,
          code: failure.code,
          retryable: failure.retryable,
          ...(failure.retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds: failure.retryAfterMilliseconds }),
        }) }),
        correlation,
      }))
    } catch {
      // Observing discarded traffic cannot acquire protocol or failure authority.
    }
  }

  accept(message: V2SessionMessage): Promise<void> {
    return this.#authority.acceptLate(message)
  }

  close(): void {
    this.#authority.close()
  }
}
