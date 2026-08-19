import { encodeBase64Url } from '../crypto/bytes'
import type { V2ReceiverDiagnosticSnapshot } from '../ui/v2-model'
import { browserBuildIdentity } from './build-identity'
import type { DiagnosticContextSources } from './export/context'
import type { DiagnosticBundleIdentityV1 } from './export/diagnostic-bundle-v1'
import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from './export/trace-event-v1'
import {
  createIncidentRecordProjector,
  createRuntimeRunIdentity,
  type BuildSnapshot,
} from './export/projector'
import type { IncidentScopeIdentity } from './incident'
import { IncidentDiagnosticsHealth } from './incident/health'
import { BoundedIncidentHistory } from './incident/history'
import {
  createIncidentReporter,
  type IncidentConsoleSink,
  type IncidentLink,
  type IncidentReporter,
} from './incident/reporter'
import {
  createBrowserDiagnosticsRuntime,
  type BrowserDiagnosticsRuntime,
} from './runtime'
import type { TraceEventObservationV1 } from './trace/model'
import {
  SYSTEM_TRACE_SCHEDULER,
  type TraceClock,
  type TraceScheduler,
} from './trace/ports'
import { TraceSwitch } from './trace/switch'

const RUNTIME_RUN_ID_BYTES = 16

export interface BrowserDiagnosticsClock extends TraceClock {
  captureTime(): string
}

export interface BrowserDiagnosticsCompositionOptions {
  readonly build: BuildSnapshot
  readonly secureContext: boolean
  readonly consoleSink: IncidentConsoleSink
  readonly controllerSnapshot?: () => V2ReceiverDiagnosticSnapshot | undefined
  readonly randomBytes?: (byteLength: number) => Uint8Array
  readonly clock?: BrowserDiagnosticsClock
  readonly scheduler?: TraceScheduler
}

export interface BrowserDiagnosticsComposition {
  readonly trace: TraceSwitch<
    TraceEventObservationV1,
    IncidentLink,
    IncidentScopeIdentity
  >
  readonly incidents: IncidentReporter
  readonly runtime: BrowserDiagnosticsRuntime
}

export const SYSTEM_BROWSER_DIAGNOSTICS_CLOCK: BrowserDiagnosticsClock =
  Object.freeze({
    nowMilliseconds: () => Date.now(),
    captureTime: () => new Date().toISOString(),
  })

export function createBrowserDiagnosticsComposition(
  options: BrowserDiagnosticsCompositionOptions,
): BrowserDiagnosticsComposition {
  const clock = options.clock ?? SYSTEM_BROWSER_DIAGNOSTICS_CLOCK
  const runtimeRunId = createRuntimeRunIdentity(
    nonZeroRuntimeIdentity(
      (options.randomBytes ?? secureRandomBytes)(RUNTIME_RUN_ID_BYTES),
    ),
  )
  const trace = new TraceSwitch<
    TraceEventObservationV1,
    IncidentLink,
    IncidentScopeIdentity
  >({
    clock,
    scheduler: options.scheduler ?? SYSTEM_TRACE_SCHEDULER,
    eventName: traceEventObservationNameV1,
    snapshotEvent: snapshotTraceEventObservationV1,
    eventBytes: traceEventObservationBytesV1,
    snapshotIncident: snapshotIncidentLink,
    incidentMarkerBytes: incidentMarkerBytes,
    incidentScope: (incident) => incident.scope,
    sameScope,
  })
  const health = new IncidentDiagnosticsHealth(trace)
  const history = new BoundedIncidentHistory()
  const startedAtMilliseconds = safeNow(clock)
  const projector = createIncidentRecordProjector({
    runtimeRunId,
    build: options.build,
    secureContext: options.secureContext,
  })
  const incidents = createIncidentReporter({
    projector,
    history,
    health,
    consoleSink: options.consoleSink,
    contextSources: diagnosticContextSources(options.controllerSnapshot),
    traceSignals: Object.freeze({
      signal: (
        signal: Parameters<TraceSwitch<
          TraceEventObservationV1,
          IncidentLink,
          IncidentScopeIdentity
        >['signal']>[0],
      ) => trace.signal(signal),
    }),
    timeSource: Object.freeze({
      capture: () => {
        const now = safeNow(clock)
        return Object.freeze({
          time: clock.captureTime(),
          elapsedMs: BigInt(Math.max(0, Math.floor(now - startedAtMilliseconds))),
        })
      },
    }),
  })
  const identity: DiagnosticBundleIdentityV1 = Object.freeze({
    build: browserBuildIdentity(options.build),
    runtime: Object.freeze({
      kind: 'browser',
      secure_context: options.secureContext,
    }),
    runtimeRunId: encodeBase64Url(runtimeRunId.copyBytes()),
  })
  const runtime = createBrowserDiagnosticsRuntime({
    identity,
    incident: incidents,
    trace,
    timeSource: Object.freeze({ captureTime: () => clock.captureTime() }),
  })

  return Object.freeze({ trace, incidents, runtime })
}

function diagnosticContextSources(
  readSnapshot: BrowserDiagnosticsCompositionOptions['controllerSnapshot'],
): DiagnosticContextSources {
  if (readSnapshot === undefined) return Object.freeze({})
  return Object.freeze({
    controller: Object.freeze({
      read: () => readSnapshot()?.controller,
    }),
    lifecycle: Object.freeze({
      read: () => readSnapshot()?.lifecycle,
    }),
    progress: Object.freeze({
      read: () => readSnapshot()?.progress,
    }),
    output: Object.freeze({
      read: () => readSnapshot()?.output,
    }),
  })
}

function snapshotIncidentLink(incident: IncidentLink): IncidentLink {
  return Object.freeze({
    incidentSequence: incident.incidentSequence,
    scope: Object.freeze({ ...incident.scope }),
    ...(incident.rootIncidentSequence === undefined
      ? {}
      : { rootIncidentSequence: incident.rootIncidentSequence }),
  })
}

function incidentMarkerBytes(incident: IncidentLink): number {
  const encoded = JSON.stringify({
    incident_sequence: incident.incidentSequence.toString(10),
    scope: {
      scope_kind: incident.scope.scopeKind,
      scope_sequence: incident.scope.scopeSequence.toString(10),
    },
    ...(incident.rootIncidentSequence === undefined
      ? {}
      : { root_incident_sequence: incident.rootIncidentSequence.toString(10) }),
  })
  return new TextEncoder().encode(encoded).byteLength
}

function sameScope(
  left: IncidentScopeIdentity,
  right: IncidentScopeIdentity,
): boolean {
  return left.scopeKind === right.scopeKind &&
    left.scopeSequence === right.scopeSequence
}

function secureRandomBytes(byteLength: number): Uint8Array {
  const bytes = new Uint8Array(byteLength)
  globalThis.crypto.getRandomValues(bytes)
  return bytes
}

function nonZeroRuntimeIdentity(bytes: Uint8Array): Uint8Array {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength !== RUNTIME_RUN_ID_BYTES) {
    throw new RangeError('Runtime identity entropy must contain exactly 16 bytes')
  }
  return bytes.slice()
}

function safeNow(clock: TraceClock): number {
  const now = clock.nowMilliseconds()
  if (!Number.isSafeInteger(now) || now < 0) {
    throw new RangeError('Browser diagnostics clock must return non-negative integer milliseconds')
  }
  return now
}
