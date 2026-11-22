import {
  createProtocolErrorContent,
  protocolMessageKindV1,
  type ProtocolOperationSettlement,
  type ProtocolTraceSource,
} from './diagnostics'
import { createProtocolOperationIdentity, type ProtocolSessionIdentity } from './identities'
import { decodeOperationErrorControl, MESSAGE_KIND, type MessageKind, type SessionMessage } from './message'
import { OperationContinuationAuthority } from './operation-continuation'
import type { snapshotOperationRequest, CancellationTraceReason } from './operation-diagnostics'
import { SessionRuntimeError } from './runtime-types'

const REQUEST_KINDS: ReadonlySet<MessageKind> = new Set([
  MESSAGE_KIND.listChildren,
  MESSAGE_KIND.openRevisions,
  MESSAGE_KIND.renewLease,
  MESSAGE_KIND.releaseLease,
  MESSAGE_KIND.requestBlocks,
  MESSAGE_KIND.laneAttach,
  MESSAGE_KIND.peerOffer,
])

export function requireRequestKind(kind: MessageKind): void {
  if (!REQUEST_KINDS.has(kind)) {
    throw new SessionRuntimeError('operation', `Message kind ${kind} cannot begin an operation`)
  }
}

export function inferredRetiredRequestKind(message: SessionMessage): MessageKind | undefined {
  switch (message.kind) {
    case MESSAGE_KIND.catalogResult:
    case MESSAGE_KIND.scanProgress:
      return MESSAGE_KIND.listChildren
    case MESSAGE_KIND.openResults:
      return MESSAGE_KIND.openRevisions
    case MESSAGE_KIND.leaseResult:
      return MESSAGE_KIND.renewLease
    case MESSAGE_KIND.blockFragment:
    case MESSAGE_KIND.operationComplete:
      return MESSAGE_KIND.requestBlocks
    case MESSAGE_KIND.laneAttach:
      return MESSAGE_KIND.laneAttach
    case MESSAGE_KIND.operationError: {
      try {
        switch (decodeOperationErrorControl(message.body).scope) {
          case 'directory':
            return MESSAGE_KIND.listChildren
          case 'revision':
            return MESSAGE_KIND.openRevisions
          case 'block':
            return MESSAGE_KIND.requestBlocks
          default:
            return undefined
        }
      } catch {
        return undefined
      }
    }
    default:
      return undefined
  }
}

export interface ProtocolTraceContext {
  readonly protocolSessionIdentity: ProtocolSessionIdentity
  readonly trace?: ProtocolTraceSource
}

export class OperationTombstone {
  readonly #authority: OperationContinuationAuthority
  readonly settlement: ProtocolOperationSettlement
  readonly requestTrace: ReturnType<typeof snapshotOperationRequest>
  readonly cancellationReason: CancellationTraceReason | undefined
  #responseTraced = false

  constructor(
    authority: OperationContinuationAuthority,
    settlement: ProtocolOperationSettlement,
    requestTrace: ReturnType<typeof snapshotOperationRequest>,
    cancellationReason: CancellationTraceReason | undefined,
  ) {
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
