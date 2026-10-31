import { describe, expect, it } from 'vitest'
import { captureFailureEvidence, MAX_FAILURE_OPERATION_BYTES } from '../../../src/diagnostics/failure/evidence'
import { MAX_FAILURE_SUMMARY_TEXT_BYTES, summarizeFailureEvidence, type FailureEvidenceSummary } from '../../../src/diagnostics/failure/summary'
import {
  snapshotTraceEventObservationV2,
  traceEventObservationBytesV2,
  traceEventObservationNameV2,
} from '../../../src/diagnostics/export/trace-event-v2'
import type { TraceEventObservationV2 } from '../../../src/diagnostics/trace/model'
import { BoundedTraceRecorder } from '../../../src/diagnostics/trace/recorder'

const SUMMARY: FailureEvidenceSummary = {
  id: '1', operation: 'session.send', signature: '12345678', message: null,
}

describe('failure summary text contracts', () => {
  it.each([undefined, ''])('retains a protocol failure with operation %j', operation => {
    const evidence = captureFailureEvidence(new Error('send failed'), operation === undefined ? {} : { operation })
    const recorder = new BoundedTraceRecorder<TraceEventObservationV2, never>({
      captureGeneration: 1n,
      clock: { nowMilliseconds: () => 0 },
      eventName: traceEventObservationNameV2,
      snapshotEvent: snapshotTraceEventObservationV2,
      eventBytes: traceEventObservationBytesV2,
      snapshotIncident: incident => incident,
      incidentMarkerBytes: () => 1,
    })
    recorder.record(observation(summarizeFailureEvidence(evidence)))

    expect(recorder.snapshot()).toMatchObject({
      retainedEventCount: 1n,
      health: { droppedCount: 0n },
      events: [{ value: { event: { payload: { failure_evidence: { operation: 'unknown' } } } } }],
    })
  })

  it('rejects an empty operation supplied outside the capture boundary', () => {
    expect(() => snapshotTraceEventObservationV2(observation({ ...SUMMARY, operation: '' }))).toThrow()
  })

  describe.each([
    { name: 'multibyte UTF-8', unit: '\u754c', bytes: 3 },
    { name: 'astral Unicode', unit: '\u{1f680}', bytes: 4 },
    { name: 'JSON escapes', unit: '\n', bytes: 2 },
    { name: 'unpaired surrogates', unit: '\ud800', bytes: 6 },
  ])('$name', ({ unit, bytes }) => {
    it.each([
      { field: 'operation', limit: MAX_FAILURE_OPERATION_BYTES },
      { field: 'message', limit: MAX_FAILURE_SUMMARY_TEXT_BYTES },
    ] as const)('enforces the encoded $field budget', ({ field, limit }) => {
      const boundary = unit.repeat(Math.floor(limit / bytes)) + 'a'.repeat(limit % bytes)
      const oversized = boundary + unit
      expect(() => snapshotTraceEventObservationV2(observation({ ...SUMMARY, [field]: boundary }))).not.toThrow()
      expect(() => snapshotTraceEventObservationV2(observation({ ...SUMMARY, [field]: oversized }))).toThrow()

      const evidence = captureFailureEvidence(new Error(oversized), { operation: oversized })
      expect(() => snapshotTraceEventObservationV2(observation(summarizeFailureEvidence(evidence)))).not.toThrow()
    })
  })

  it.each([null, ''])('preserves a message of %j', message => {
    expect(snapshotTraceEventObservationV2(observation({ ...SUMMARY, message })).payload)
      .toMatchObject({ failure_evidence: { message } })
  })
})

function observation(summary: FailureEvidenceSummary): TraceEventObservationV2 {
  return {
    eventName: 'protocol_operation',
    correlation: {
      protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA',
      protocol_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA',
    },
    payload: { transition: 'send_failed', request_kind: 'list_children', failure_evidence: summary },
  }
}
