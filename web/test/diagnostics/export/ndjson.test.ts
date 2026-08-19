import { describe, expect, it } from 'vitest'

import {
  createDiagnosticBundleV1,
  projectDiagnosticsStatusV1,
} from '../../../src/diagnostics/export/diagnostic-bundle-v1'
import { encodeDiagnosticBundleNdjson } from '../../../src/diagnostics/export/ndjson'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'
import type { TraceEventObservationV1 } from '../../../src/diagnostics/trace/model'
import {
  TEST_BUNDLE_IDENTITY,
  diagnosticsHealthV1,
  incidentRecord,
  traceStatus,
} from './test-support'

describe('diagnostic bundle NDJSON', () => {
  it('emits header, incidents, capture, and trace in fixed order with a final LF', () => {
    const health = diagnosticsHealthV1()
    const status = projectDiagnosticsStatusV1(traceStatus({
      state: 'recording_pre_failure',
      enabled: true,
      captureGeneration: 1n,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
      retainedEventCount: 1n,
      retainedEventBytes: 7n,
    }), health)
    const incidents = [incidentRecord('1')]
    const observation: TraceEventObservationV1 = deepFreezeJson({
      eventName: 'checkpoint',
      payload: { backend: 'origin_private', transition: 'persisted' },
    })
    const bundle = createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents,
      status,
      healthAtExport: health,
      traceCapture: Object.freeze({
        state: 'recording_pre_failure',
        captureGeneration: 1n,
        startedAtMilliseconds: Date.parse('2026-08-19T01:00:00Z'),
        retainedEventCount: 1n,
        retainedEventBytes: 7n,
        incidentMarkerCount: 0n,
        events: Object.freeze([Object.freeze({
          sequence: 1n,
          observedAtMilliseconds: Date.parse('2026-08-19T01:01:00Z'),
          elapsedMs: 60_000n,
          encodedBytes: 7,
          value: Object.freeze({
            kind: 'event' as const,
            event: observation,
            eventName: 'checkpoint' as const,
          }),
        })]),
        health: Object.freeze({
          droppedCount: 0n,
          overwrittenCount: 0n,
          sampledCount: 0n,
          coalescedCount: 0n,
        }),
      }),
    })
    const first = encodeDiagnosticBundleNdjson(bundle)
    incidents.splice(0)
    const second = encodeDiagnosticBundleNdjson(bundle)

    expect(second).toBe(first)
    expect(first.endsWith('\n')).toBe(true)
    expect(first.split('\n').at(-1)).toBe('')
    const lines = first.trimEnd().split('\n').map((line) => JSON.parse(line) as {
      line_type: string
    })
    expect(lines.map((line) => line.line_type)).toEqual([
      'bundle_header',
      'incident',
      'trace_capture',
      'trace_event',
    ])
    expect(first).toMatch(/^\{"line_type":"bundle_header","schema_version":1,/)
  })

  it('still emits one complete header line for an empty idle runtime', () => {
    const health = diagnosticsHealthV1()
    const bundle = createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-19T01:30:00Z',
      incidents: [],
      status: projectDiagnosticsStatusV1(traceStatus(), health),
      healthAtExport: health,
    })

    const encoded = encodeDiagnosticBundleNdjson(bundle)
    expect(encoded.split('\n')).toHaveLength(2)
    expect(JSON.parse(encoded.trimEnd())).toMatchObject({
      line_type: 'bundle_header',
      diagnostics_health_at_export: health,
    })
  })
})
