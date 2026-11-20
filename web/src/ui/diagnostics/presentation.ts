import { diagnosticsCopy } from '../locales/diagnostics'
import type { DiagnosticsStatusV2 } from '../../diagnostics/export/diagnostic-bundle-v2'

export function diagnosticsCaptureLabel(status: DiagnosticsStatusV2, t = diagnosticsCopy.en): string {
  if (status.enabled) return t.recording
  if (status.state === 'idle') return t.off
  switch (status.seal_reason) {
    case 'manual_disable': return t.stopped
    case 'expired': return t.expired
    default: return t.retained
  }
}
