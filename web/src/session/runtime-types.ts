import type { FrameChannel } from '../contracts/channel'
import type { ShareDescriptor } from '../catalog/records'
import type { BlockWaitTrace, ProtocolTraceSource } from './diagnostics'
import type { MessageKind, SessionMessage } from './message'

export const OPERATION_CANCEL_REASON = Object.freeze({
  user: 1,
  superseded: 2,
  outputAbort: 3,
  timeout: 4,
  laneRace: 5,
} as const)

export type OperationCancelReason =
  (typeof OPERATION_CANCEL_REASON)[keyof typeof OPERATION_CANCEL_REASON]

/** Carries cancellation intent through AbortSignal without a dependency on its producer. */
export class OperationCancellationError extends Error {
  readonly protocolReason: OperationCancelReason

  constructor(protocolReason: OperationCancelReason, message: string) {
    super(message)
    this.protocolReason = protocolReason
  }
}

export interface OperationCancellation {
  readonly protocolReason: OperationCancelReason
  readonly blockWait?: BlockWaitTrace
  readonly cause: unknown
  readonly laneId?: number
}

export interface SessionOperation {
  readonly id: Uint8Array<ArrayBuffer>
  readonly requestKind: MessageKind
  next(signal?: AbortSignal): Promise<SessionMessage>
  cancel(cause: unknown, protocolReason?: OperationCancelReason): void
}

export type LaneCloseReason = 'retired' | 'replaced' | 'session_closed'

export type LaneTermination =
  | Readonly<{ kind: 'closed'; reason: LaneCloseReason }>
  | Readonly<{ kind: 'physical_failure' | 'authenticated_failure'; failure: unknown }>

export interface LaneChange {
  readonly type: 'attached' | 'detached'
  readonly laneId: number
  readonly laneEpoch: number
  readonly failure?: unknown
}

export interface ReceiverSessionOptions {
  readonly descriptor: ShareDescriptor
  readonly readSecret: Uint8Array
  readonly initialChannel: FrameChannel
  readonly signal?: AbortSignal
  readonly randomBytes?: (length: number) => Uint8Array
  readonly connectivityCleanup?: () => void | Promise<void>
  readonly protocolTrace?: ProtocolTraceSource
}

/** An absent handshake response is transport uncertainty, before session authority exists. */
export class SessionHandshakeTimeoutError extends Error {
  readonly timeoutMilliseconds: number

  constructor(timeoutMilliseconds: number) {
    super('Protocol session handshake timed out')
    this.name = 'SessionHandshakeTimeoutError'
    this.timeoutMilliseconds = timeoutMilliseconds
  }
}

export class SessionRuntimeError extends Error {
  readonly scope: 'lane' | 'operation' | 'session'

  constructor(
    scope: 'lane' | 'operation' | 'session',
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'SessionRuntimeError'
    this.scope = scope
  }
}
