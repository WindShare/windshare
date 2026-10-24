import type { V2CapturedLocation } from '../capability/location'
import type { V2DiagnosticFormatter, V2SecurityMilestone } from '../../application/browser-share/capability-lifecycle'
import type { V2ReceiveCompositionPort } from '../../application/receive/contracts'
import type { ReceiveObservationOptions } from '../../application/receive/observation/contracts'

export interface CapabilityIntakePort {
  accept(captured: V2CapturedLocation): void
}

export interface V2ReceiverControllerOptions extends ReceiveObservationOptions {
  readonly capabilityIntake?: CapabilityIntakePort
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
  readonly receive: V2ReceiveCompositionPort
}
