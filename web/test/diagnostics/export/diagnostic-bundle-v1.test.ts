import { describe, expect, it } from 'vitest'

import {
  createDiagnosticBundleV1,
  projectDiagnosticsStatusV1,
} from '../../../src/diagnostics/export/diagnostic-bundle-v1'
import { deepFreezeJson, isDeeplyFrozen } from '../../../src/diagnostics/export/json'
import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../../src/diagnostics/export/trace-event-v1'
import type { IncidentLink } from '../../../src/diagnostics/incident/reporter'
import type {
  TraceCaptureSnapshot,
  TraceCapturedEvent,
  TraceEventObservationV1,
} from '../../../src/diagnostics/trace/model'
import {
  TEST_BUNDLE_IDENTITY,
  diagnosticsHealthV1,
  incidentRecord,
  traceStatus,
} from './test-support'

describe('DiagnosticBundleV1', () => {
  it('takes an immutable ordered capture cut with exact export health', () => {
    const health = diagnosticsHealthV1({
      incident_history_eviction_count: '3',
      trace_dropped_count: '5',
      trace_overwritten_count: '8',
    })
    const status = projectDiagnosticsStatusV1(traceStatus({
      state: 'recording_post_failure',
      enabled: true,
      captureGeneration: 2n,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
      retainedEventCount: 2n,
      retainedEventBytes: 20n,
      incidentMarkerCount: 1n,
    }), health)
    const bundle = createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00.123Z',
      incidents: [incidentRecord('2'), incidentRecord('1')],
      status,
      healthAtExport: health,
      traceCapture: capture(),
    })

    expect(bundle.incidents.map((line) => line.record.sequence)).toEqual(['1', '2'])
    expect(bundle.traceEvents.map((line) => line.record.sequence)).toEqual(['3', '5'])
    expect(bundle.traceEvents.map((line) => line.record.event)).toEqual([
      'cleanup',
      'incident_marker',
    ])
    expect(bundle.traceEvents[1]?.record.payload).toEqual({
      incident_sequence: '9',
      scope: { scope_kind: 'join', scope_sequence: '4' },
    })
    expect(bundle.header.diagnostics_health_at_export).toEqual(health)
    expect(bundle.traceCapture?.status.health).toEqual(health)
    expect(Object.keys(bundle.header)).toEqual([
      'line_type',
      'schema_version',
      'build',
      'runtime',
      'runtime_run_id',
      'time',
      'diagnostics_health_at_export',
    ])
    expect(Object.keys(bundle.traceEvents[0]!.record)).toEqual([
      'schema_version',
      'sequence',
      'time',
      'elapsed_ms',
      'level',
      'event',
      'runtime_run_id',
      'payload',
    ])
    expect(isDeeplyFrozen(bundle)).toBe(true)
  })

  it('publishes every named trace capacity using the frozen key order', () => {
    const status = projectDiagnosticsStatusV1(traceStatus(), diagnosticsHealthV1())

    expect(Object.keys(status)).toEqual([
      'schema_version',
      'state',
      'enabled',
      'capture_generation',
      'capacity',
      'retained_event_count',
      'retained_event_bytes',
      'incident_marker_count',
      'health',
    ])
    expect(Object.keys(status.capacity)).toEqual([
      'default_trace_capture_expiry_ms',
      'max_trace_event_count',
      'max_trace_event_bytes',
      'max_trace_total_bytes',
      'max_trace_pre_failure_event_count',
      'max_trace_pre_failure_bytes',
      'max_trace_post_failure_event_count',
      'max_trace_post_failure_bytes',
      'max_trace_incident_markers',
      'max_trace_post_failure_ms',
      'trace_post_failure_silence_ms',
      'trace_progress_sample_interval_ms',
      'trace_checkpoint_coalesce_interval_ms',
    ])
    expect(status.capacity).toEqual({
      default_trace_capture_expiry_ms: 1_800_000,
      max_trace_event_count: 4_096,
      max_trace_event_bytes: 16_384,
      max_trace_total_bytes: 4_194_304,
      max_trace_pre_failure_event_count: 2_048,
      max_trace_pre_failure_bytes: 2_097_152,
      max_trace_post_failure_event_count: 2_048,
      max_trace_post_failure_bytes: 2_097_152,
      max_trace_incident_markers: 32,
      max_trace_post_failure_ms: 30_000,
      trace_post_failure_silence_ms: 5_000,
      trace_progress_sample_interval_ms: 1_000,
      trace_checkpoint_coalesce_interval_ms: 1_000,
    })
    expect(status.capture_generation).toBe('0')
    expect(Object.isFrozen(status.capacity)).toBe(true)
  })

  it('rejects a trace payload that tries to smuggle unreviewed fields', () => {
    const unsafe = deepFreezeJson({
      eventName: 'cleanup',
      payload: {
        backend: 'portable',
        transition: 'failed',
        message: 'C:/private/name.txt',
      },
    }) as unknown as TraceEventObservationV1
    const traceCapture = capture([domainEvent(3n, unsafe)])
    const health = diagnosticsHealthV1({
      trace_dropped_count: '5',
      trace_overwritten_count: '8',
    })
    const status = projectDiagnosticsStatusV1(traceStatus({
      state: 'recording_pre_failure',
      enabled: true,
      captureGeneration: 2n,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
      retainedEventCount: 1n,
      retainedEventBytes: 10n,
      incidentMarkerCount: 0n,
    }), health)

    expect(() => createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents: [],
      status,
      healthAtExport: health,
      traceCapture,
    })).toThrow(/unexpected field message/)
  })

  it('rejects health and capture cuts that were read from different states', () => {
    const statusHealth = diagnosticsHealthV1({ trace_dropped_count: '1' })
    const status = projectDiagnosticsStatusV1(traceStatus(), statusHealth)

    expect(() => createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents: [],
      status,
      healthAtExport: diagnosticsHealthV1({ trace_dropped_count: '2' }),
    })).toThrow(/one health cut/)
  })

  it('rejects an injected capture that exceeds the named per-event capacity', () => {
    const oversizedEvent = Object.freeze({
      ...domainEvent(3n, cleanupObservation()),
      encodedBytes: 16_385,
    })
    const traceCapture = Object.freeze({
      ...capture([oversizedEvent]),
      retainedEventBytes: 16_385n,
    })
    const health = diagnosticsHealthV1({
      trace_dropped_count: '5',
      trace_overwritten_count: '8',
    })
    const status = projectDiagnosticsStatusV1(traceStatus({
      state: 'recording_pre_failure',
      enabled: true,
      captureGeneration: 2n,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
      retainedEventCount: 1n,
      retainedEventBytes: 16_385n,
      incidentMarkerCount: 0n,
    }), health)

    expect(() => createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents: [],
      status,
      healthAtExport: health,
      traceCapture,
    })).toThrow(/per-event capacity/)
  })

  it('provides a detached validated adapter for the bounded recorder', () => {
    const source: {
      eventName: 'cleanup'
      payload: {
        backend: 'portable'
        transition: 'completed' | 'failed'
      }
    } = {
      eventName: 'cleanup',
      payload: { backend: 'portable', transition: 'completed' },
    }
    const snapshot = snapshotTraceEventObservationV1(source)
    source.payload.transition = 'failed'

    expect(snapshot.payload).toEqual({ backend: 'portable', transition: 'completed' })
    expect(traceEventObservationNameV1(snapshot)).toBe('cleanup')
    expect(traceEventObservationBytesV1(snapshot)).toBeGreaterThan(0)
    expect(isDeeplyFrozen(snapshot)).toBe(true)
  })
})

function capture(
  events: readonly TraceCapturedEvent<TraceEventObservationV1, IncidentLink>[] = [
    markerEvent(5n),
    domainEvent(3n, cleanupObservation()),
  ],
): TraceCaptureSnapshot<TraceEventObservationV1, IncidentLink> {
  return Object.freeze({
    state: events.length === 1 ? 'recording_pre_failure' : 'recording_post_failure',
    captureGeneration: 2n,
    startedAtMilliseconds: Date.parse('2026-08-19T01:00:00Z'),
    retainedEventCount: BigInt(events.length),
    retainedEventBytes: BigInt(events.length * 10),
    incidentMarkerCount: BigInt(events.filter(
      (event) => event.value.kind === 'incident_marker',
    ).length),
    events: Object.freeze([...events]),
    health: Object.freeze({
      droppedCount: 5n,
      overwrittenCount: 8n,
      sampledCount: 0n,
      coalescedCount: 0n,
    }),
  })
}

function cleanupObservation(): TraceEventObservationV1 {
  return deepFreezeJson({
    eventName: 'cleanup',
    payload: { backend: 'portable', transition: 'failed' },
  })
}

function domainEvent(
  sequence: bigint,
  event: TraceEventObservationV1,
): TraceCapturedEvent<TraceEventObservationV1, IncidentLink> {
  return Object.freeze({
    sequence,
    observedAtMilliseconds: Date.parse('2026-08-19T01:10:00Z') + Number(sequence),
    elapsedMs: sequence * 10n,
    encodedBytes: 10,
    value: Object.freeze({
      kind: 'event',
      event,
      eventName: event.eventName,
    }),
  })
}

function markerEvent(
  sequence: bigint,
): TraceCapturedEvent<TraceEventObservationV1, IncidentLink> {
  return Object.freeze({
    sequence,
    observedAtMilliseconds: Date.parse('2026-08-19T01:20:00Z'),
    elapsedMs: 99n,
    encodedBytes: 10,
    value: Object.freeze({
      kind: 'incident_marker',
      eventName: 'incident_marker',
      incident: Object.freeze({
        incidentSequence: 9n,
        scope: Object.freeze({ scopeKind: 'join', scopeSequence: 4n }),
      }),
    }),
  })
}
