import type { ProjectionTraceEvent } from '../../transfer/projection'
import type { TransferTraceEvent } from '../../transfer/v2-job'
import type { V2DiagnosticFormatter, V2SecurityMilestone } from '../v2-capability-lifecycle'
import type { V2OutputTraceEvent } from '../v2-output'
import type {
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'

export type V2RetainedInventoryTraceEvent =
  | Readonly<{ name: 'receive.inventory.load.started' }>
  | Readonly<{ name: 'receive.inventory.load.completed'; operation_count: number }>
  | Readonly<{ name: 'receive.inventory.load.failed'; error_name: string }>
  | Readonly<{
      name: 'receive.inventory.action.started' | 'receive.inventory.action.completed'
      retained_action: V2RetainedReceiveAction
      continuation: V2RetainedReceiveOperation['continuation']
    }>
  | Readonly<{
      name: 'receive.inventory.action.failed'
      retained_action: V2RetainedReceiveAction
      continuation: V2RetainedReceiveOperation['continuation']
      error_name: string
    }>

export type V2ReceiverTraceEvent =
  | ProjectionTraceEvent
  | TransferTraceEvent
  | V2OutputTraceEvent
  | V2RetainedInventoryTraceEvent

export interface V2ReceiverControllerOptions {
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
  readonly onOutputTrace?: (event: V2ReceiverTraceEvent) => void
  readonly receive: V2ReceiveCompositionPort
}

export class ArtifactChoiceInvalidatedError extends Error {
  constructor() {
    super('The selected artifact action is no longer offered by current authority facts')
    this.name = 'ArtifactChoiceInvalidatedError'
  }
}

export class StaleReceiveBoundaryError extends DOMException {
  constructor() {
    super('A newer receive boundary replaced this action', 'AbortError')
  }
}
