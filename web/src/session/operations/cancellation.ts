import {
  OperationCancellationError,
  OPERATION_CANCEL_REASON,
  type OperationCancellation,
} from '../runtime-types'

export function cancellationFromSignal(signal: AbortSignal): OperationCancellation {
  const cause = signal.reason ?? new DOMException('Operation aborted', 'AbortError')
  if (cause instanceof OperationCancellationError) return { cause, protocolReason: cause.protocolReason }
  const protocolReason = cause instanceof DOMException && cause.name === 'TimeoutError'
    ? OPERATION_CANCEL_REASON.timeout : OPERATION_CANCEL_REASON.user
  return { cause, protocolReason }
}
