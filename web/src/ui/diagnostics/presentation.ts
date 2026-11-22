import { diagnosticsCopy } from '../locales/diagnostics'
import type { DiagnosticsStatus } from '../../diagnostics/runtime'

export function diagnosticsCaptureLabel(status: DiagnosticsStatus, t = diagnosticsCopy.en): string {
  if (status.enabled) return t.recording
  if (status.state === 'idle') return t.off
  switch (status.seal_reason) {
    case 'manual_disable': return t.stopped
    case 'expired': return t.expired
    default: return t.retained
  }
}
