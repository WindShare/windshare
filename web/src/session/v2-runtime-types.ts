import type { FrameChannel } from '../contracts/channel'
import type { V2ShareDescriptor } from '../catalog/v2-records'
import type { V2MessageKind, V2SessionMessage } from './v2-message'

export const V2_OPERATION_CANCEL_REASON = Object.freeze({
  user: 1,
  superseded: 2,
  outputAbort: 3,
  timeout: 4,
  laneRace: 5,
} as const)

export type V2OperationCancelReason =
  (typeof V2_OPERATION_CANCEL_REASON)[keyof typeof V2_OPERATION_CANCEL_REASON]

export interface V2OperationCancellation {
  readonly protocolReason: V2OperationCancelReason
  readonly cause: unknown
  readonly laneId?: number
}

export interface V2SessionOperation {
  readonly id: Uint8Array<ArrayBuffer>
  readonly requestKind: V2MessageKind
  next(signal?: AbortSignal): Promise<V2SessionMessage>
  cancel(cause: unknown): void
}

export interface V2LaneChange {
  readonly type: 'attached' | 'detached'
  readonly laneId: number
  readonly laneEpoch: number
  readonly failure?: unknown
}

export interface V2ReceiverSessionOptions {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array
  readonly initialChannel: FrameChannel
  readonly signal?: AbortSignal
  readonly randomBytes?: (length: number) => Uint8Array
  readonly connectivityCleanup?: () => void | Promise<void>
}

export class V2SessionRuntimeError extends Error {
  readonly scope: 'lane' | 'operation' | 'session'

  constructor(
    scope: 'lane' | 'operation' | 'session',
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'V2SessionRuntimeError'
    this.scope = scope
  }
}
