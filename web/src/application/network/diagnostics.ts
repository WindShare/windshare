import type { AuthorityObservation } from '../../network/authority/feed'
import type { DirectoryObservation } from '../../network/directory'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type { TraceEventObservationV2 } from '../../diagnostics/trace/model'
import { observation } from '../../diagnostics/export/trace-observation'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'

export function authorityObserver(trace: DomainTraceSource<TraceEventObservationV2>): (event: AuthorityObservation) => void {
  return event => {
    trace.current?.(observation('authority_blocks', {
      operation_id: event.operationId, endpoint: event.endpoint.slice(0, 2048), transition: event.transition,
      ...(event.generation === undefined ? {} : { generation: event.generation }),
      ...(event.version === undefined ? {} : { version: event.version }),
      ...(event.failure === undefined ? {} : { failure_detail: formatDiagnosticText(event.failure).slice(0, 2048) }),
    }))
  }
}

export function networkObserver(trace: DomainTraceSource<TraceEventObservationV2>): (event: DirectoryObservation) => void {
  return event => {
    const observer = trace.current
    if (!observer) return
    if (event.transition === 'storage_fallback') {
      observer(observation('network_directory', {
        operation_id: event.operationId, transition: event.transition,
        failure_detail: formatDiagnosticText(event.failure).slice(0, 2048),
      }))
      return
    }
    observer(observation('network_directory', {
      operation_id: event.operationId, mirror: event.mirror.slice(0, 2048), tier: event.tier, transition: event.transition,
      ...(event.generation === undefined ? {} : { generation: event.generation }),
      ...(event.serial === undefined ? {} : { serial: event.serial }),
      ...(event.trustedUntil === undefined ? {} : { trusted_until_ms: event.trustedUntil }),
      ...(event.failure === undefined ? {} : { failure_detail: formatDiagnosticText(event.failure).slice(0, 2048) }),
    }))
  }
}
