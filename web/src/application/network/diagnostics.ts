import type { AuthorityObservation } from '../../network/authority/feed'
import type { DirectoryObservation } from '../../network/directory'
import type { TraceEntry, TraceSource } from '../../diagnostics/runtime'
import { formatDiagnosticText } from '../../security/diagnostic-formatter'

const MAX_TEXT = 2_048

export function authorityObserver(trace: TraceSource<TraceEntry>): (event: AuthorityObservation) => void {
  return event => {
    trace.current?.({
      event: 'authority_blocks',
      payload: {
        operation_id: event.operationId,
        endpoint: event.endpoint.slice(0, MAX_TEXT),
        transition: event.transition,
        ...(event.generation === undefined ? {} : { generation: event.generation }),
        ...(event.version === undefined ? {} : { version: event.version }),
        ...(event.failure === undefined ? {} : { failure_detail: formatDiagnosticText(event.failure).slice(0, MAX_TEXT) }),
      },
    })
  }
}

export function networkObserver(trace: TraceSource<TraceEntry>): (event: DirectoryObservation) => void {
  return event => {
    const observer = trace.current
    if (!observer) return
    if (event.transition === 'storage_fallback') {
      observer({
        event: 'network_directory',
        payload: {
          operation_id: event.operationId,
          transition: event.transition,
          failure_detail: formatDiagnosticText(event.failure).slice(0, MAX_TEXT),
        },
      })
      return
    }
    observer({
      event: 'network_directory',
      payload: {
        operation_id: event.operationId,
        mirror: event.mirror.slice(0, MAX_TEXT),
        tier: event.tier,
        transition: event.transition,
        ...(event.generation === undefined ? {} : { generation: event.generation }),
        ...(event.serial === undefined ? {} : { serial: event.serial }),
        ...(event.trustedUntil === undefined ? {} : { trusted_until_ms: event.trustedUntil }),
        ...(event.failure === undefined ? {} : { failure_detail: formatDiagnosticText(event.failure).slice(0, MAX_TEXT) }),
      },
    })
  }
}
