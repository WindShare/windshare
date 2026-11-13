import type { DirectoryObservation } from '../../network/directory'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { TraceEventObservationV2 } from '../../diagnostics/trace/model'
import { observation } from '../../diagnostics/export/trace-observation'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'

export function networkObserver(trace: DomainTraceSource<TraceEventObservationV2>): (event: DirectoryObservation) => void {
  return event => {
    const observer = trace.current
    if (!observer) return
    observer(observation('network_directory', {
      operation_id: event.operationId, mirror: event.mirror.slice(0, 2048), transition: event.transition,
      ...(event.generation === undefined ? {} : { generation: event.generation }),
      ...(event.serial === undefined ? {} : { serial: event.serial }),
      ...(event.trustedUntil === undefined ? {} : { trusted_until_ms: event.trustedUntil }),
      ...(event.failure === undefined ? {} : { failure_detail: formatDiagnosticText(event.failure).slice(0, 2048) }),
    }))
  }
}
