import {
  createDiagnosticBundleV1,
  projectDiagnosticsStatusV1,
  type DiagnosticBundleIdentityV1,
  type DiagnosticsStatusV1,
} from './export/diagnostic-bundle-v1'
import { encodeDiagnosticBundleNdjson } from './export/ndjson'
import type { IncidentRecordV1 } from './export/incident-record-v1'
import { projectDiagnosticsHealthV1 } from './export/projector'
import { isDeeplyFrozen } from './export/json'
import type { IncidentHealthReadPort } from './incident/health'
import type { IncidentHistoryReadPort } from './incident/history'
import type { IncidentLink } from './incident/reporter'
import type {
  TraceCaptureSnapshot,
  TraceCoreStatus,
  TraceEventObservationV1,
} from './trace/model'

export interface DiagnosticsIncidentRuntimePort {
  readonly history: IncidentHistoryReadPort
  readonly health: IncidentHealthReadPort
  clearRetainedIncidents(): void
}

export interface DiagnosticsTraceRuntimePort {
  enable(): TraceCoreStatus
  disable(): TraceCoreStatus
  status(): TraceCoreStatus
  clear(): void
  captureSnapshot(): TraceCaptureSnapshot<TraceEventObservationV1, IncidentLink> | undefined
}

export interface DiagnosticsExportTimeSource {
  captureTime(): string
}

export interface BrowserDiagnosticsRuntimeOptions {
  readonly identity: DiagnosticBundleIdentityV1
  readonly incident: DiagnosticsIncidentRuntimePort
  readonly trace: DiagnosticsTraceRuntimePort
  readonly timeSource?: DiagnosticsExportTimeSource
}

export interface DiagnosticsRuntimePort {
  enable(): DiagnosticsStatusV1
  disable(): DiagnosticsStatusV1
  status(): DiagnosticsStatusV1
  inspectLastFailure(): IncidentRecordV1 | null
  export(): string
  clear(): void
}

export const SYSTEM_DIAGNOSTICS_EXPORT_TIME_SOURCE: DiagnosticsExportTimeSource =
  Object.freeze({ captureTime: () => new Date().toISOString() })

export class BrowserDiagnosticsRuntime implements DiagnosticsRuntimePort {
  readonly #identity: DiagnosticBundleIdentityV1
  readonly #incident: DiagnosticsIncidentRuntimePort
  readonly #trace: DiagnosticsTraceRuntimePort
  readonly #timeSource: DiagnosticsExportTimeSource

  constructor(options: BrowserDiagnosticsRuntimeOptions) {
    this.#identity = options.identity
    this.#incident = options.incident
    this.#trace = options.trace
    this.#timeSource = options.timeSource ?? SYSTEM_DIAGNOSTICS_EXPORT_TIME_SOURCE
  }

  enable(): DiagnosticsStatusV1 {
    return this.#statusFrom(this.#trace.enable())
  }

  disable(): DiagnosticsStatusV1 {
    return this.#statusFrom(this.#trace.disable())
  }

  status(): DiagnosticsStatusV1 {
    return this.#statusFrom(this.#trace.status())
  }

  inspectLastFailure(): IncidentRecordV1 | null {
    try {
      const record = this.#incident.history.last()
      return record !== null && isDeeplyFrozen(record) ? record : null
    } catch {
      // Inspection is independent from both retained history and receive control.
      return null
    }
  }

  export(): string {
    // JavaScript cannot interleave timers inside this synchronous read sequence;
    // each live port is therefore read exactly once before encoding begins.
    const incidents = this.#incident.history.snapshot()
    const traceStatus = this.#trace.status()
    const traceCapture = this.#trace.captureSnapshot()
    const healthAtExport = projectDiagnosticsHealthV1(
      this.#incident.health.incidentHealthSnapshot(),
    )
    const status = projectDiagnosticsStatusV1(traceStatus, healthAtExport)
    const bundle = createDiagnosticBundleV1({
      identity: this.#identity,
      time: this.#timeSource.captureTime(),
      incidents,
      status,
      healthAtExport,
      ...(traceCapture === undefined ? {} : { traceCapture }),
    })
    return encodeDiagnosticBundleNdjson(bundle)
  }

  clear(): void {
    try {
      this.#incident.clearRetainedIncidents()
    } catch {
      // A custom history sink cannot prevent trace revocation/clearing.
    }
    try {
      this.#trace.clear()
    } catch {
      // Explicit clear has no product authority and is best-effort per store.
    }
  }

  #statusFrom(status: TraceCoreStatus): DiagnosticsStatusV1 {
    const health = projectDiagnosticsHealthV1(
      this.#incident.health.incidentHealthSnapshot(),
    )
    return projectDiagnosticsStatusV1(status, health)
  }
}

export function createBrowserDiagnosticsRuntime(
  options: BrowserDiagnosticsRuntimeOptions,
): BrowserDiagnosticsRuntime {
  return new BrowserDiagnosticsRuntime(options)
}
