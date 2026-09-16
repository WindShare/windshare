import type { DiagnosticsStatusV2 } from '../../diagnostics/export/diagnostic-bundle-v2'

export function diagnosticsCaptureLabel(status: DiagnosticsStatusV2): string {
  if (status.enabled) return 'Recording diagnostics'
  if (status.state === 'idle') return 'Diagnostics are off'
  switch (status.seal_reason) {
    case 'manual_disable': return 'Recording stopped'
    case 'expired': return 'Recording time limit reached'
    case 'capacity_exhausted': return 'Diagnostic buffer is full'
    default: return 'Failure evidence retained'
  }
}
