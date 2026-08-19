import { describe, expect, it } from 'vitest'

import { TraceHealthAccumulator } from '../../../src/diagnostics/trace/recorder'
import {
  FakeTraceTime,
  makeTestRecorder,
  retainedOrdinals,
  testEvent,
  testIncident,
  testTraceCapacity,
  type TestScope,
} from './test-support'

describe('bounded trace recorder', () => {
  it('crosses pre-failure count and byte bounds with only limit plus one', () => {
    const countTime = new FakeTraceTime()
    const countRecorder = makeTestRecorder(countTime, testTraceCapacity({
      maxEventCount: 4,
      maxTotalBytes: 8,
      maxPreFailureEventCount: 2,
      maxPreFailureBytes: 8,
      maxPostFailureEventCount: 2,
      maxPostFailureBytes: 4,
    }))
    countRecorder.record(testEvent(1))
    countRecorder.record(testEvent(2))
    countRecorder.record(testEvent(3))

    expect(retainedOrdinals(countRecorder)).toEqual([2, 3])
    expect(countRecorder.snapshot().health.overwrittenCount).toBe(1n)

    const byteTime = new FakeTraceTime()
    const byteRecorder = makeTestRecorder(byteTime, testTraceCapacity({
      maxEventCount: 4,
      maxEventBytes: 3,
      maxTotalBytes: 6,
      maxPreFailureEventCount: 2,
      maxPreFailureBytes: 3,
      maxPostFailureEventCount: 2,
      maxPostFailureBytes: 3,
    }))
    byteRecorder.record(testEvent(1, { bytes: 2 }))
    byteRecorder.record(testEvent(2, { bytes: 2 }))

    expect(retainedOrdinals(byteRecorder)).toEqual([2])
    expect(byteRecorder.snapshot().retainedEventBytes).toBe(2n)
    expect(byteRecorder.snapshot().health.overwrittenCount).toBe(1n)
  })

  it('drops an oversized event without sealing or evicting the pre-window', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time, testTraceCapacity({ maxEventBytes: 2 }))
    recorder.record(testEvent(1))
    recorder.record(testEvent(2, { bytes: 3 }))

    expect(recorder.state).toBe('recording_pre_failure')
    expect(retainedOrdinals(recorder)).toEqual([1])
    expect(recorder.snapshot().health).toEqual({
      droppedCount: 1n,
      overwrittenCount: 0n,
      sampledCount: 0n,
      coalescedCount: 0n,
    })
  })

  it('pins the pre-window and seals instead of overwriting it at tail capacity', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time, testTraceCapacity({
      maxEventCount: 4,
      maxEventBytes: 2,
      maxTotalBytes: 4,
      maxPreFailureEventCount: 2,
      maxPreFailureBytes: 2,
      maxPostFailureEventCount: 2,
      maxPostFailureBytes: 2,
    }))
    recorder.record(testEvent(1))
    recorder.record(testEvent(2))
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 10n })
    recorder.record(testEvent(3))
    recorder.record(testEvent(4))

    const snapshot = recorder.snapshot()
    expect(snapshot.state).toBe('sealed')
    expect(snapshot.sealReason).toBe('capacity_exhausted')
    expect(retainedOrdinals(recorder)).toEqual([1, 2, 3])
    expect(snapshot.incidentMarkerCount).toBe(1n)
    expect(snapshot.health.droppedCount).toBe(1n)
    expect(snapshot.health.overwrittenCount).toBe(0n)
  })

  it('crosses distinct post-failure and whole-capture bounds with limit plus one', () => {
    const postCountRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxPostFailureEventCount: 2,
    }))
    postCountRecorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n),
      elapsedMs: 1n,
    })
    postCountRecorder.record(testEvent(1))
    postCountRecorder.record(testEvent(2))
    expect(postCountRecorder.snapshot().sealReason).toBe('capacity_exhausted')

    const postBytesRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxEventBytes: 3,
      maxPostFailureBytes: 3,
    }))
    postBytesRecorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n),
      elapsedMs: 1n,
    })
    postBytesRecorder.record(testEvent(1, { bytes: 2 }))
    postBytesRecorder.record(testEvent(2))
    expect(postBytesRecorder.snapshot().sealReason).toBe('capacity_exhausted')

    const totalCountRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxEventCount: 3,
      maxPreFailureEventCount: 2,
      maxPostFailureEventCount: 3,
    }))
    totalCountRecorder.record(testEvent(1))
    totalCountRecorder.record(testEvent(2))
    totalCountRecorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n),
      elapsedMs: 1n,
    })
    totalCountRecorder.record(testEvent(3))
    expect(totalCountRecorder.snapshot().sealReason).toBe('capacity_exhausted')

    const totalBytesRecorder = makeTestRecorder(new FakeTraceTime(), testTraceCapacity({
      maxEventBytes: 3,
      maxTotalBytes: 4,
      maxPreFailureBytes: 4,
      maxPostFailureBytes: 4,
    }))
    totalBytesRecorder.record(testEvent(1))
    totalBytesRecorder.record(testEvent(2))
    totalBytesRecorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n),
      elapsedMs: 1n,
    })
    totalBytesRecorder.record(testEvent(3, { bytes: 2 }))
    expect(totalBytesRecorder.snapshot().sealReason).toBe('capacity_exhausted')
  })

  it('keeps one root tail, bounds later markers, and seals only for its root scope', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    const rootScope: TestScope = Object.freeze({ kind: 'receive', sequence: 7n })
    recorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n, rootScope),
      elapsedMs: 1n,
    })
    recorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(2n, Object.freeze({ kind: 'preview', sequence: 8n })),
      elapsedMs: 2n,
    })
    recorder.signal({
      kind: 'scope_terminal',
      scope: Object.freeze({ kind: 'preview', sequence: 8n }),
      elapsedMs: 3n,
    })

    expect(recorder.state).toBe('recording_post_failure')
    expect(recorder.snapshot().incidentMarkerCount).toBe(2n)

    recorder.signal({ kind: 'scope_terminal', scope: rootScope, elapsedMs: 4n })
    expect(recorder.snapshot().sealReason).toBe('scope_terminal')
  })

  it('seals a bounded marker overflow as capacity exhaustion', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(2n), elapsedMs: 2n })
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(3n), elapsedMs: 3n })

    expect(recorder.snapshot().incidentMarkerCount).toBe(2n)
    expect(recorder.snapshot().sealReason).toBe('capacity_exhausted')
    expect(recorder.snapshot().health.droppedCount).toBe(1n)
  })

  it('uses resettable silence and an absolute post-failure deadline', () => {
    const silenceTime = new FakeTraceTime()
    const silenceRecorder = makeTestRecorder(silenceTime)
    silenceRecorder.signal({
      kind: 'incident_sealed',
      incident: testIncident(1n),
      elapsedMs: 1n,
    })
    silenceTime.advance(4)
    silenceRecorder.record(testEvent(1))
    silenceTime.advance(4)
    expect(silenceRecorder.state).toBe('recording_post_failure')
    silenceTime.advance(1)
    expect(silenceRecorder.snapshot().sealReason).toBe('post_failure_silence')

    const hardTime = new FakeTraceTime()
    const hardRecorder = makeTestRecorder(hardTime)
    hardRecorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    for (let ordinal = 1; ordinal <= 3; ordinal += 1) {
      hardTime.advance(4)
      hardRecorder.record(testEvent(ordinal, { name: 'checkpoint' }))
    }
    hardTime.advance(8)
    expect(hardRecorder.snapshot().sealReason).toBe('post_failure_silence')
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

  it('clears back to a fresh pre-window and fences stale tail timers', () => {
    const time = new FakeTraceTime()
    const recorder = makeTestRecorder(time)
    recorder.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 1n })
    const staleDeadlineIndex = 0
    recorder.clear()
    time.runEvenIfCancelled(staleDeadlineIndex)

    expect(recorder.state).toBe('recording_pre_failure')
    expect(recorder.snapshot().retainedEventCount).toBe(0n)
    expect(recorder.snapshot().incidentMarkerCount).toBe(0n)
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
