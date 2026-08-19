import { describe, expect, it } from 'vitest'

import { TraceSwitch } from '../../../src/diagnostics/trace/switch'
import {
  FakeTraceTime,
  testEvent,
  testTraceCapacity,
  type TestEvent,
  type TestIncident,
  type TestScope,
} from './test-support'

function makeSwitch(time: FakeTraceTime) {
  return new TraceSwitch<TestEvent, TestIncident, TestScope>({
    capacity: testTraceCapacity(),
    clock: time,
    scheduler: time,
    eventName: (event) => event.name,
    snapshotEvent: (event) => Object.freeze({ ...event }),
    eventBytes: (event) => event.bytes,
    snapshotIncident: (incident) => Object.freeze({
      sequence: incident.sequence,
      scope: Object.freeze({ ...incident.scope }),
    }),
    incidentMarkerBytes: () => 1,
    incidentScope: (incident) => incident.scope,
    sameScope: (left, right) => left.kind === right.kind && left.sequence === right.sequence,
  })
}

describe('trace switch', () => {
  it('exposes absence so a disabled producer never constructs its payload', () => {
    const trace = makeSwitch(new FakeTraceTime())
    let payloadConstructions = 0
    const produce = () => {
      const observer = trace.current
      if (observer === undefined) return
      payloadConstructions += 1
      observer(testEvent(payloadConstructions))
    }

    produce()
    expect(payloadConstructions).toBe(0)
    expect(trace.status().state).toBe('idle')

    trace.enable()
    produce()
    expect(payloadConstructions).toBe(1)
    trace.disable()
    produce()
    expect(payloadConstructions).toBe(1)
    expect(trace.captureSnapshot()?.retainedEventCount).toBe(1n)
  })

  it('makes an observer captured before revocation inert', () => {
    const trace = makeSwitch(new FakeTraceTime())
    trace.enable()
    const observer = trace.current
    trace.disable()

    observer?.(testEvent(1))

    expect(trace.captureSnapshot()?.retainedEventCount).toBe(0n)
    expect(trace.traceHealthSnapshot().droppedCount).toBe(0n)
  })

  it('replaces captures and generation-fences a stale expiry callback', () => {
    const time = new FakeTraceTime()
    const trace = makeSwitch(time)
    const first = trace.enable()
    const firstExpiryIndex = 0
    const second = trace.enable()

    expect(first.captureGeneration).toBe(1n)
    expect(second.captureGeneration).toBe(2n)
    time.runEvenIfCancelled(firstExpiryIndex)
    expect(trace.status().state).toBe('recording_pre_failure')

    const secondExpiryIndex = 1
    time.runEvenIfCancelled(secondExpiryIndex)
    expect(trace.current).toBeUndefined()
    expect(trace.status().sealReason).toBe('expired')
  })

  it('clears an active recorder without changing generation or original expiry', () => {
    const time = new FakeTraceTime()
    const trace = makeSwitch(time)
    const enabled = trace.enable()
    trace.current?.(testEvent(1))
    trace.clear()
    const cleared = trace.status()

    expect(cleared.captureGeneration).toBe(enabled.captureGeneration)
    expect(cleared.expiresAtMilliseconds).toBe(enabled.expiresAtMilliseconds)
    expect(cleared.state).toBe('recording_pre_failure')
    expect(cleared.retainedEventCount).toBe(0n)
    expect(trace.current).toBeDefined()
  })

  it('clears a sealed capture to idle without reusing its generation', () => {
    const trace = makeSwitch(new FakeTraceTime())
    trace.enable()
    trace.disable()
    trace.clear()

    expect(trace.status()).toMatchObject({
      state: 'idle',
      enabled: false,
      captureGeneration: 1n,
      retainedEventCount: 0n,
    })
    expect(trace.captureSnapshot()).toBeUndefined()
  })

  it('keeps trace health cumulative when enable replaces retained trace data', () => {
    const time = new FakeTraceTime()
    const trace = makeSwitch(time)
    trace.enable()
    trace.current?.(testEvent(1, { bytes: 5 }))
    expect(trace.traceHealthSnapshot().droppedCount).toBe(1n)

    trace.clear()
    expect(trace.status().retainedEventCount).toBe(0n)
    expect(trace.traceHealthSnapshot().droppedCount).toBe(1n)

    trace.enable()
    expect(trace.status().retainedEventCount).toBe(0n)
    expect(trace.traceHealthSnapshot().droppedCount).toBe(1n)
  })
})
