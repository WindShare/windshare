import {
  createDiagnosticBundleV2,
  projectDiagnosticsStatusV2,
  type DiagnosticBundleIdentityV2,
  type DiagnosticsStatusV2,
} from './export/diagnostic-bundle-v2'
import { encodeDiagnosticBundleNdjson } from './export/ndjson'
import type { IncidentRecordV2 } from './export/incident-record-v2'
import { projectDiagnosticsHealthV1 } from './export/projector'
import { isDeeplyFrozen } from './export/json'
import type { IncidentHealthReadPort } from './incident/health'
import type { IncidentHistoryReadPort } from './incident/history'
import type { IncidentLink } from './incident/reporter'
import type {
  LocalOutputOperationFailureReadPort,
} from '../output/diagnostics/local-output-failure'
import type {
  TraceCaptureSnapshot,
  TraceCoreStatus,
  TraceEventObservationV2,
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
  captureSnapshot(): TraceCaptureSnapshot<TraceEventObservationV2, IncidentLink> | undefined
}

export interface DiagnosticsExportTimeSource {
  captureTime(): string
}

export interface BrowserDiagnosticsRuntimeOptions {
  readonly identity: DiagnosticBundleIdentityV2
  readonly incident: DiagnosticsIncidentRuntimePort
  readonly trace: DiagnosticsTraceRuntimePort
  readonly timeSource?: DiagnosticsExportTimeSource
  readonly localOutputFailures?: LocalOutputOperationFailureReadPort & Readonly<{
    clear(): void
  }>
}

export interface DiagnosticsRuntimePort {
  enable(): DiagnosticsStatusV2
  disable(): DiagnosticsStatusV2
  status(): DiagnosticsStatusV2
  inspectLastFailure(): IncidentRecordV2 | null
  export(): string
  clear(): void
}

export const SYSTEM_DIAGNOSTICS_EXPORT_TIME_SOURCE: DiagnosticsExportTimeSource =
  Object.freeze({ captureTime: () => new Date().toISOString() })

export class BrowserDiagnosticsRuntime implements DiagnosticsRuntimePort {
  readonly #identity: DiagnosticBundleIdentityV2
  readonly #incident: DiagnosticsIncidentRuntimePort
  readonly #trace: DiagnosticsTraceRuntimePort
  readonly #timeSource: DiagnosticsExportTimeSource
  readonly #localOutputFailures: BrowserDiagnosticsRuntimeOptions['localOutputFailures']

  constructor(options: BrowserDiagnosticsRuntimeOptions) {
    this.#identity = options.identity
    this.#incident = options.incident
    this.#trace = options.trace
    this.#timeSource = options.timeSource ?? SYSTEM_DIAGNOSTICS_EXPORT_TIME_SOURCE
    this.#localOutputFailures = options.localOutputFailures
  }

  enable(): DiagnosticsStatusV2 {
    return this.#statusFrom(this.#trace.enable())
  }

  disable(): DiagnosticsStatusV2 {
    return this.#statusFrom(this.#trace.disable())
  }

  status(): DiagnosticsStatusV2 {
    return this.#statusFrom(this.#trace.status())
  }

  inspectLastFailure(): IncidentRecordV2 | null {
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
    const status = projectDiagnosticsStatusV2(traceStatus, healthAtExport)
    const bundle = createDiagnosticBundleV2({
      identity: this.#identity,
      time: this.#timeSource.captureTime(),
      incidents,
      localOutputFailures: this.#localOutputFailures?.snapshot() ?? [],
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
    try {
      this.#localOutputFailures?.clear()
    } catch {
      // Local output evidence is diagnostic-only and clears independently.
    }
  }

  #statusFrom(status: TraceCoreStatus): DiagnosticsStatusV2 {
    const health = projectDiagnosticsHealthV1(
      this.#incident.health.incidentHealthSnapshot(),
    )
    return projectDiagnosticsStatusV2(status, health)
  }
}

export function createBrowserDiagnosticsRuntime(
  options: BrowserDiagnosticsRuntimeOptions,
): BrowserDiagnosticsRuntime {
  return new BrowserDiagnosticsRuntime(options)
}
