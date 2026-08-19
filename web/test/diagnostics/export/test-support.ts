import { DEFAULT_TRACE_CAPACITY_POLICY } from '../../../src/diagnostics/trace/capacity'
import type {
  DiagnosticsHealthV1,
  IncidentRecordV1,
} from '../../../src/diagnostics/export/incident-record-v1'
import type { DiagnosticBundleIdentityV1 } from '../../../src/diagnostics/export/diagnostic-bundle-v1'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'
import type { IncidentDiagnosticsHealthSnapshot } from '../../../src/diagnostics/incident/health'
import type { TraceCoreStatus } from '../../../src/diagnostics/trace/model'

export const TEST_RUNTIME_RUN_ID = 'AQAAAAAAAAAAAAAAAAAAAA'

export const TEST_BUNDLE_IDENTITY: DiagnosticBundleIdentityV1 = deepFreezeJson({
  build: {
    application: 'windshare_web',
    version: '0.0.0',
    revision: 'abcdef0',
    mode: 'test',
  },
  runtime: { kind: 'browser', secure_context: true },
  runtimeRunId: TEST_RUNTIME_RUN_ID,
})

export function diagnosticsHealthV1(
  overrides: Partial<DiagnosticsHealthV1> = {},
): DiagnosticsHealthV1 {
  return Object.freeze({
    fact_overflow_count: '0',
    incident_history_eviction_count: '0',
    console_suppression_count: '0',
    late_link_eviction_count: '0',
    trace_dropped_count: '0',
    trace_overwritten_count: '0',
    trace_sampled_count: '0',
    trace_coalesced_count: '0',
    ...overrides,
  })
}

export function incidentHealth(
  overrides: Partial<IncidentDiagnosticsHealthSnapshot> = {},
): IncidentDiagnosticsHealthSnapshot {
  return Object.freeze({
    factOverflowCount: 0n,
    incidentHistoryEvictionCount: 0n,
    consoleSuppressionCount: 0n,
    lateLinkEvictionCount: 0n,
    traceDroppedCount: 0n,
    traceOverwrittenCount: 0n,
    traceSampledCount: 0n,
    traceCoalescedCount: 0n,
    ...overrides,
  })
}

export function traceStatus(
  overrides: Partial<TraceCoreStatus> = {},
): TraceCoreStatus {
  return Object.freeze({
    state: 'idle',
    enabled: false,
    captureGeneration: 0n,
    capacity: DEFAULT_TRACE_CAPACITY_POLICY,
    retainedEventCount: 0n,
    retainedEventBytes: 0n,
    incidentMarkerCount: 0n,
    health: Object.freeze({
      droppedCount: 0n,
      overwrittenCount: 0n,
      sampledCount: 0n,
      coalescedCount: 0n,
    }),
    ...overrides,
  })
}

export function incidentRecord(sequence: string): IncidentRecordV1 {
  return deepFreezeJson({
    schema_version: 1,
    sequence,
    time: '2026-08-19T01:02:03.000Z',
    elapsed_ms: sequence,
    level: 'error',
    event: 'failure_incident',
    runtime_run_id: TEST_RUNTIME_RUN_ID,
    payload: {
      scope: { scope_kind: 'join', scope_sequence: sequence },
      presentation: { boundary: 'join', outcome: 'failed' },
      build: TEST_BUNDLE_IDENTITY.build,
      runtime: TEST_BUNDLE_IDENTITY.runtime,
      trigger: {
        kind: 'unclassified',
        stage: 'join',
        recovery_disposition: 'terminal',
        payload: { unclassified: {} },
      },
      contributors: [],
      consequences: [],
      fact_count: '1',
      overflow_fact_count: '0',
      context: {},
      diagnostics_health_at_seal: diagnosticsHealthV1(),
    },
  })
}
