import { describe, expect, it } from 'vitest'

import { TraceHealthAccumulator } from '../../../src/diagnostics/trace/recorder'
import {
  FakeTraceTime,
  makeTestRecorder,
  retainedOrdinals,
  testEvent,
  testIncident,
  testTraceCapacity,
} from './test-support'

describe('bounded trace recorder', () => {
  it.each([false, true])('rolls count and byte limits without stopping after failure=%s', failed => {
    const countRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({ maxEventCount: 2 }))
    if (failed) countRecorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    for (let ordinal = 1; ordinal <= 3; ordinal++) countRecorder.record(testEvent(ordinal))
    expect(retainedOrdinals(countRecorder)).toEqual([2, 3])
    expect(countRecorder.snapshot()).toMatchObject({
      state: 'recording', retainedEventCount: 2n, incidentMarkerCount: 0n,
      health: { overwrittenCount: failed ? 2n : 1n, droppedCount: 0n },
    })

    const byteRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxEventBytes: 3, maxTotalBytes: 3,
    }))
    if (failed) byteRecorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    byteRecorder.record(testEvent(1, { bytes: 2 }))
    byteRecorder.record(testEvent(2, { bytes: 2 }))
    expect(retainedOrdinals(byteRecorder)).toEqual([2])
    expect(byteRecorder.snapshot()).toMatchObject({
      state: 'recording', retainedEventBytes: 2n, incidentMarkerCount: 0n,
      health: { overwrittenCount: failed ? 2n : 1n, droppedCount: 0n },
    })
  })

  it('protects a bounded failure marker while later download traffic rolls through', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    time.advance(60_000)
    for (let ordinal = 1; ordinal <= 20; ordinal++) recorder.record(testEvent(ordinal))
    expect(recorder.snapshot()).toMatchObject({
      state: 'recording', retainedEventCount: 8n, incidentMarkerCount: 1n,
      health: { overwrittenCount: 13n, droppedCount: 0n },
    })
    expect(retainedOrdinals(recorder)).toEqual([14, 15, 16, 17, 18, 19, 20])
  })

  it('rolls the marker limit independently and accepts later events', () => {
    const recorder = makeTestRecorder(new FakeTraceTime())
    recorder.record(testEvent(1))
    for (let sequence = 1n; sequence <= 3n; sequence++) {
      recorder.signal({ kind: 'incident_sealed', incident: testIncident(sequence), elapsedMs: sequence })
    }
    recorder.record(testEvent(2))
    expect(recorder.snapshot().events.flatMap(record =>
      record.value.kind === 'incident_marker' ? [record.value.incident.sequence] : [])).toEqual([2n, 3n])
    expect(retainedOrdinals(recorder)).toEqual([1, 2])
    expect(recorder.snapshot()).toMatchObject({
      state: 'recording', incidentMarkerCount: 2n,
      health: { overwrittenCount: 1n, droppedCount: 0n },
    })
  })

  it('drops oversized events and invalid incident timestamps without evicting existing evidence', () => {
    const recorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({ maxEventBytes: 2 }))
    recorder.record(testEvent(1))
    recorder.record(testEvent(2, { bytes: 3 }))
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: -1n })
    expect(recorder.state).toBe('recording')
    expect(retainedOrdinals(recorder)).toEqual([1])
    expect(recorder.snapshot().health).toMatchObject({ droppedCount: 2n, overwrittenCount: 0n })
  })

  it('keeps a growing coalesced checkpoint within the byte budget after a failure', () => {
    const recorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxTotalBytes: 8,
    }))
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    recorder.record(testEvent(1, { bytes: 3 }))
    recorder.record(testEvent(2, { name: 'checkpoint', bytes: 1 }))
    recorder.record(testEvent(3, { bytes: 2 }))
    recorder.record(testEvent(4, { name: 'checkpoint', bytes: 4 }))
    expect(retainedOrdinals(recorder)).toEqual([3, 4])
    expect(recorder.snapshot()).toMatchObject({
      state: 'recording', retainedEventBytes: 7n, incidentMarkerCount: 1n,
      health: { coalescedCount: 1n, overwrittenCount: 1n },
    })
    const sequences = recorder.snapshot().events.map(record => record.sequence)
    expect(sequences).toEqual([1n, 4n, 5n])
  })

  it('makes old observers and incident signals inert after explicit sealing', () => {
    const recorder = makeTestRecorder(new FakeTraceTime())
    recorder.record(testEvent(1))
    recorder.seal('manual_disable')
    const sealed = recorder.snapshot()
    recorder.record(testEvent(2))
    expect(recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })).toBe(false)
    recorder.clear()
    expect(recorder.snapshot()).toEqual(sealed)
  })

  it('samples progress and coalesces checkpoint observations with distinct counters', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.record(testEvent(1, { name: 'transfer_progress' }))
    recorder.record(testEvent(2, { name: 'transfer_progress' }))
    time.advance(10)
    recorder.record(testEvent(3, { name: 'transfer_progress' }))

    recorder.record(testEvent(4, { name: 'checkpoint', bytes: 1 }))
    time.advance(5)
    recorder.record(testEvent(5, { name: 'checkpoint', bytes: 2 }))
    time.advance(10)
    recorder.record(testEvent(6, { name: 'checkpoint', bytes: 1 }))

    expect(retainedOrdinals(recorder)).toEqual([1, 3, 5, 6])
    expect(recorder.snapshot().retainedEventBytes).toBe(5n)
    expect(recorder.snapshot().health).toEqual({
      droppedCount: 0n,
      overwrittenCount: 0n,
      sampledCount: 1n,
      coalescedCount: 1n,
    })
  })

  it('moves a coalesced observation to its chronological position', () => {
    const recorder = makeTestRecorder(new FakeTraceTime())
    recorder.record(testEvent(1, { name: 'checkpoint' }))
    recorder.record(testEvent(2))
    recorder.record(testEvent(3, { name: 'checkpoint' }))

    const snapshot = recorder.snapshot()
    expect(retainedOrdinals(recorder)).toEqual([2, 3])
    expect(snapshot.events.map((event) => event.sequence)).toEqual([2n, 3n])
  })

  it('clears evidence without changing the capture lifetime or sequence', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    const prior = recorder.snapshot()
    recorder.clear()
    recorder.record(testEvent(2))
    expect(recorder.state).toBe('recording')
    expect(recorder.snapshot().incidentMarkerCount).toBe(0n)
    expect(recorder.snapshot().events[0]?.sequence).toBe(2n)
    expect(prior.incidentMarkerCount).toBe(1n)
  })

  it('contains fallible projection and byte sizing as named drops', () => {
    const time = new FakeTraceTime()
    const health = new TraceHealthAccumulator()
    const recorder = makeTestRecorder(time, testTraceCapacity(), health)
    const invalid = testEvent(1, { bytes: 0 })

    expect(() => recorder.record(invalid)).not.toThrow()
    expect(health.traceHealthSnapshot().droppedCount).toBe(1n)
  })

  it('returns frozen detached collection snapshots', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.record(testEvent(1))
    const first = recorder.snapshot()
    recorder.record(testEvent(2))

    expect(first.retainedEventCount).toBe(1n)
    expect(first.events).toHaveLength(1)
    expect(Object.isFrozen(first)).toBe(true)
    expect(Object.isFrozen(first.events)).toBe(true)
    expect(Object.isFrozen(first.events[0])).toBe(true)
  })
})
