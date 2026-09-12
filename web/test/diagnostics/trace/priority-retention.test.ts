import { describe, expect, it } from 'vitest'
import { BoundedTraceRecorder } from '../../../src/diagnostics/trace/recorder'
import { traceEventRetention, TraceRetentionWindow } from '../../../src/diagnostics/trace/retention'
import type { TraceEventObservationV2 } from '../../../src/diagnostics/trace/model'
import {
  snapshotTraceEventObservationV2, traceEventObservationBytesV2,
} from '../../../src/diagnostics/export/trace-event-v2'
import { FakeTraceTime } from './test-support'

describe('trace evidence under high-volume protocol traffic', () => {
  it('retains cancellation and product milestones across a trace-11 sized burst within the original budgets', () => {
    const time = new FakeTraceTime()
    const recorder = new BoundedTraceRecorder<TraceEventObservationV2, number, number>({
      captureGeneration: 1n, clock: time, scheduler: time,
      eventName: event => event.eventName,
      eventRetention: traceEventRetention,
      snapshotEvent: snapshotTraceEventObservationV2, eventBytes: traceEventObservationBytesV2,
      snapshotIncident: value => value, incidentMarkerBytes: () => 1,
      incidentScope: value => value, sameScope: (a, b) => a === b,
    })
    const correlation = { protocol_session_id: 'AQEBAQEBAQEBAQEBAQEBAQ', protocol_operation_id: 'AgICAgICAgICAgICAgICAg' }
    const cancelled: TraceEventObservationV2 = {
      eventName: 'protocol_operation', correlation, payload: {
        transition: 'cancelled', request_kind: 'request_blocks', cancellation_reason: 'lane_race',
        request: { lease_id: '03'.repeat(16), blocks: { first_index: '7', count: 1 } },
      },
    }
    const milestone: TraceEventObservationV2 = { eventName: 'join_transition', payload: { transition: 'started' } }
    const late: TraceEventObservationV2 = {
      eventName: 'protocol_operation', correlation, payload: {
        transition: 'late_response_discarded', request_kind: 'request_blocks', response_kind: 'operation_error',
        settlement: 'local_cancel', cancellation_reason: 'lane_race',
        protocol_error: { scope: 'revision', code: 0x3008, retryable: false },
      },
    }
    recorder.record(cancelled)
    recorder.record(late)
    recorder.record(milestone)
    for (let count = 0; count < 16_420; count++) {
      if (count % 2 === 0) recorder.record({ eventName: 'protocol_operation', correlation,
        payload: { transition: 'send_completed', request_kind: 'open_revisions' } })
      else recorder.record({ eventName: 'browser_delivery', payload: {
        operation_id: correlation.protocol_operation_id, file_id: correlation.protocol_session_id,
        transition: 'checkpoint', checkpoint_stage: 'advanced', received_bytes: String(count),
      } })
    }
    const snapshot = recorder.snapshot()
    const events = snapshot.events.flatMap(record => record.value.kind === 'event' ? [record.value.event] : [])
    expect(events).toContainEqual(cancelled)
    expect(events).toContainEqual(late)
    expect(events).toContainEqual(milestone)
    expect(snapshot.retainedEventCount).toBe(2048n)
    expect(snapshot.retainedEventBytes).toBeLessThanOrEqual(2_097_152n)
    expect(snapshot.health.droppedCount).toBe(0n)
    expect(snapshot.health.overwrittenCount).toBe(14_375n)
    expect(snapshot.events.map(event => event.sequence)).toEqual(
      snapshot.events.map(event => event.sequence).sort((a, b) => a < b ? -1 : 1))
  })

  it('bounds milestones independently from outcomes and releases their reservations on clear and removal', () => {
    const retained = new TraceRetentionWindow<string>(8, 32)
    retained.retain('failure', 4, 'outcome')
    for (let index = 0; index < 20; index++) retained.retain(String(index), 4, 'milestone')
    expect(retained.priority('failure')).toBe(2)
    expect(retained.priority('17')).toBe(0)
    expect(retained.priority('18')).toBe(1)
    retained.retain('large', 8, 'milestone')
    expect(retained.priority('18')).toBe(0)
    expect(retained.priority('19')).toBe(0)
    retained.remove('large')
    retained.retain('next', 4, 'milestone')
    expect(retained.priority('next')).toBe(1)
    retained.clear()
    expect(retained.priority('failure')).toBe(0)
    expect(retained.priority('next')).toBe(0)
  })
})
