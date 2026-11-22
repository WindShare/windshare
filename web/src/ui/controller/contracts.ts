import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { IncidentReporter } from '../../diagnostics/incident/reporter'
import type { DiagnosticFormatter, SecurityMilestone } from '../../application/browser-share/capability-lifecycle'
import type { ReceiverTraceEvent } from '../../application/receive/contracts'
import type { ReceiveRuntimePorts } from '../../application/receive/coordinator'
import type { CapturedLocation } from '../capability/location'
import type { PresentationPreferences } from '../output'

export interface CapabilityIntakePort {
  accept(captured: CapturedLocation): void
}

export interface ReceiverControllerOptions extends ReceiveRuntimePorts {
  readonly capabilityIntake?: CapabilityIntakePort
  readonly diagnosticFormatter?: DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: SecurityMilestone) => void
  readonly preferences?: PresentationPreferences
  readonly trace?: DomainTraceSource<ReceiverTraceEvent>
  readonly incidents?: IncidentReporter
}
