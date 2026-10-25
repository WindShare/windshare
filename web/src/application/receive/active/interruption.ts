import { receiveInterruptionControls, receiveInterruptionReason, type ReceiveInterruption } from '../../../transfer/lifecycle/interruption'
import type { BoundReceiveOperation } from '../contracts'

/** Closing admission is shared; workers drain accepted writes before the storage adapter settles. */
export function interruptReceive(
  operation: Pick<BoundReceiveOperation, 'intent' | 'activeControls'>,
  control: ReceiveInterruption,
  transfer: AbortController,
): void {
  if (!receiveInterruptionControls(operation.intent.plan.kind).includes(control) ||
      !operation.activeControls.includes(control)) {
    throw new TypeError(`Receive route does not support ${control}`)
  }
  transfer.abort(receiveInterruptionReason(control))
}
