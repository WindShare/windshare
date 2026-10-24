import type { CapturedLocation } from '../capability/location'
import type { DiagnosticFormatter, SecurityMilestone } from '../../application/browser-share/capability-lifecycle'
import type { ReceiveCompositionPort } from '../../application/receive/contracts'
import type { ReceiveObservationOptions } from '../../application/receive/observation/contracts'

export interface CapabilityIntakePort {
  accept(captured: CapturedLocation): void
}

export interface ReceiverControllerOptions extends ReceiveObservationOptions {
  readonly capabilityIntake?: CapabilityIntakePort
  readonly diagnosticFormatter?: DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: SecurityMilestone) => void
  readonly receive: ReceiveCompositionPort
}
