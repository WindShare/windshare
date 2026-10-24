import { createProtocolErrorContent } from '../diagnostics/incident/protocol-error'
import { protocolMessageKindV1, type ProtocolOperationSettlement, type ProtocolTraceSource } from './diagnostics'
import { createProtocolOperationIdentity, type ProtocolSessionIdentity } from './identities'
import { decodeOperationErrorControl, MESSAGE_KIND, type MessageKind, type SessionMessage } from './message'
import { OperationContinuationAuthority } from './operation-continuation'
import type { snapshotOperationRequest, CancellationTraceReason } from './operation-diagnostics'

export interface ProtocolTraceContext {
  readonly protocolSessionIdentity: ProtocolSessionIdentity
  readonly trace?: ProtocolTraceSource
}

export class OperationTombstone {
  readonly expiresAt: number
  readonly #authority: OperationContinuationAuthority
  readonly settlement: ProtocolOperationSettlement
  readonly requestTrace: ReturnType<typeof snapshotOperationRequest>
  readonly cancellationReason: CancellationTraceReason | undefined
  #responseTraced = false

  constructor(
    expiresAt: number,
    authority: OperationContinuationAuthority,
    settlement: ProtocolOperationSettlement,
    requestTrace: ReturnType<typeof snapshotOperationRequest>,
    cancellationReason: CancellationTraceReason | undefined,
  ) {
    this.expiresAt = expiresAt
    this.#authority = authority
    this.settlement = settlement
    this.requestTrace = requestTrace
    this.cancellationReason = cancellationReason
  }

  get requestKind(): MessageKind { return this.#authority.requestKind }

  #takeResponseTrace(): boolean {
    if (this.#responseTraced) return false
    this.#responseTraced = true
    return true
  }

  traceResponse(message: SessionMessage, diagnostics: ProtocolTraceContext | undefined, laneId?: number, laneEpoch?: number): void {
    const observer = diagnostics?.trace?.current
    if (observer === undefined || diagnostics === undefined || message.operationId === undefined ||
      (message.kind !== MESSAGE_KIND.operationError && message.kind !== MESSAGE_KIND.operationComplete) ||
      !this.#takeResponseTrace()) return
    // This is a discarded response, not an active operation failure. Publishing an
    // incident here would turn successful race cleanup into a failed download.
    try {
      const correlation = {
        protocolSessionId: diagnostics.protocolSessionIdentity,
        protocolOperationId: createProtocolOperationIdentity(message.operationId),
        ...(laneId === undefined || laneEpoch === undefined ? {} : { lane: { id: laneId, epoch: laneEpoch } }),
      }
      const failure = message.kind === MESSAGE_KIND.operationError ? decodeOperationErrorControl(message.body) : undefined
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

  accept(message: SessionMessage): Promise<void> {
    return this.#authority.acceptLate(message)
  }

  close(): void {
    this.#authority.close()
  }
}
