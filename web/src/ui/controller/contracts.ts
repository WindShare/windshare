import type { DiagnosticFormatter, SecurityMilestone } from '../../application/browser-share/capability-lifecycle'
import type { FailureReporter } from '../../application/diagnostics/receive-trace'
import type { ReceiverTraceEvent } from '../../application/receive/contracts'
import type { ReceiveRuntimePorts } from '../../application/receive/coordinator'
import type { TraceSource } from '../../diagnostics/runtime'
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
  readonly trace?: TraceSource<ReceiverTraceEvent>
  readonly recordFailure?: FailureReporter
}
