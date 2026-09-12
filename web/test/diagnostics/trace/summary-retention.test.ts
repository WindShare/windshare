import { describe, expect, it } from 'vitest'
import { BoundedTraceRecorder } from '../../../src/diagnostics/trace/recorder'
import { FakeTraceTime, testTraceCapacity } from './test-support'

interface Event { name: 'peer_attempt' | 'cleanup' | 'checkpoint'; id: number; bytes: number }
function recorder() {
  const time = new FakeTraceTime()
  return new BoundedTraceRecorder<Event, number, number>({
    captureGeneration: 1n, clock: time, scheduler: time, capacity: testTraceCapacity(),
    eventName: event => event.name, snapshotEvent: event => Object.freeze({ ...event }), eventBytes: event => event.bytes,
    eventRetention: event => event.name === 'peer_attempt' ? 'outcome' : 'recent',
    snapshotIncident: value => value, incidentMarkerBytes: () => 1, incidentScope: value => value, sameScope: (a, b) => a === b,
  })
}
function ids(value: ReturnType<typeof recorder>) {
  return value.snapshot().events.flatMap(event => event.value.kind === 'event' ? [event.value.event.id] : [])
}

describe('bounded summary retention', () => {
  it('keeps only a bounded reservation and evicts older attempts as new ones finish', () => {
    const trace = recorder()
    for (let id = 1; id <= 10; id++) trace.record({ name: 'peer_attempt', id, bytes: 1 })
    for (let id = 11; id <= 20; id++) trace.record({ name: 'cleanup', id, bytes: 1 })
    expect(ids(trace)).toEqual([10, 18, 19, 20])
    expect(trace.snapshot().retainedEventCount).toBe(4n)
    expect(trace.snapshot().retainedEventBytes).toBe(4n)
  })

  it('respects byte budgets and clear, including a coalesced event that grows', () => {
    const trace = recorder()
    trace.record({ name: 'peer_attempt', id: 1, bytes: 2 })
    trace.record({ name: 'cleanup', id: 2, bytes: 2 })
    trace.record({ name: 'checkpoint', id: 3, bytes: 1 })
    trace.record({ name: 'cleanup', id: 4, bytes: 2 })
    trace.record({ name: 'checkpoint', id: 5, bytes: 4 })
    expect(ids(trace)).toEqual([1, 4, 5])
    expect(trace.snapshot().retainedEventBytes).toBe(8n)
    trace.clear()
    for (let id = 6; id <= 10; id++) trace.record({ name: 'cleanup', id, bytes: 2 })
    expect(ids(trace)).toEqual([7, 8, 9, 10])
  })
})
