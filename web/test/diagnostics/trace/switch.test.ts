import { describe, expect, it } from 'vitest'

import { TraceSwitch, type TraceActivationStore } from '../../../src/diagnostics/trace/switch'
import {
  FakeTraceTime,
  testEvent,
  testIncident,
  testTraceCapacity,
  type TestEvent,
  type TestIncident,
} from './test-support'

function makeSwitch(time: FakeTraceTime, activationStore?: TraceActivationStore) {
  return new TraceSwitch<TestEvent, TestIncident>({
    ...(activationStore === undefined ? {} : { activationStore }),
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

  })
}

function activationStore(expiresAt?: number): TraceActivationStore {
  return {
    readExpiry: () => expiresAt,
    writeExpiry: value => { expiresAt = value },
    clear: () => { expiresAt = undefined },
  }
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
    expect(trace.status().state).toBe('recording')

    const secondExpiryIndex = 1
    time.runEvenIfCancelled(secondExpiryIndex)
    expect(trace.current).toBeUndefined()
    expect(trace.status().sealReason).toBe('expired')
  })

  it('does not revoke a newer persisted activation when an older page expires', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    makeSwitch(time, store).enable()
    time.advance(40)
    const newerPage = makeSwitch(time, store)
    const renewed = newerPage.enable()
    time.advance(60)
    expect(newerPage.status().enabled).toBe(true)
    expect(store.readExpiry()).toBe(renewed.expiresAtMilliseconds)
    expect(makeSwitch(time, store).status().expiresAtMilliseconds).toBe(renewed.expiresAtMilliseconds)
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
    expect(cleared.state).toBe('recording')
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

  it('restores startup capture with the original deadline and fresh evidence', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    const first = makeSwitch(time, store)
    expect(first.status().state).toBe('idle')
    const enabled = first.enable()
    first.current?.(testEvent(1))
    time.advance(40)

    const reopened = makeSwitch(time, store)
    expect(reopened.status()).toMatchObject({
      enabled: true,
      expiresAtMilliseconds: enabled.expiresAtMilliseconds,
      retainedEventCount: 0n,
    })
    reopened.current?.(testEvent(2))
    expect(reopened.captureSnapshot()?.retainedEventCount).toBe(1n)
    time.advance(59)
    expect(reopened.current).toBeDefined()
    time.advance(1)
    expect(reopened.status()).toMatchObject({ enabled: false, sealReason: 'expired' })
    expect(makeSwitch(time, store).status().state).toBe('idle')
    expect(store.readExpiry()).toBeUndefined()
  })

  it('continues after failures with unchanged activation and persists the original reload deadline', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    const trace = makeSwitch(time, store)
    const enabled = trace.enable()
    trace.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 0n })
    time.advance(40)
    trace.current?.(testEvent(1))
    expect(trace.status()).toMatchObject({
      state: 'recording', captureGeneration: enabled.captureGeneration,
      expiresAtMilliseconds: enabled.expiresAtMilliseconds, retainedEventCount: 2n,
    })
    const reopened = makeSwitch(time, store)
    expect(reopened.status().expiresAtMilliseconds).toBe(enabled.expiresAtMilliseconds)
    time.advance(60)
    expect(trace.status()).toMatchObject({ enabled: false, sealReason: 'expired' })
    expect(reopened.status()).toMatchObject({ enabled: false, sealReason: 'expired' })
    expect(store.readExpiry()).toBeUndefined()
  })

  it('notifies evidence consumers of each failure without revoking the observer', () => {
    const trace = makeSwitch(new FakeTraceTime())
    trace.enable()
    const observer = trace.current
    let notifications = 0
    trace.subscribe(() => notifications++)
    trace.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 0n })
    trace.signal({ kind: 'incident_sealed', incident: testIncident(2n), elapsedMs: 1n })
    expect(notifications).toBe(2)
    expect(trace.current).toBe(observer)
    expect(trace.status().incidentMarkerCount).toBe(2n)
    trace.disable()
    const stopped = trace.captureSnapshot()
    trace.signal({ kind: 'incident_sealed', incident: testIncident(3n), elapsedMs: 2n })
    expect(trace.captureSnapshot()).toEqual(stopped)
    expect(notifications).toBe(3)
  })

  it('manual stop retains failure evidence and revokes reload activation', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    const trace = makeSwitch(time, store)
    trace.enable()
    trace.current?.(testEvent(1))
    trace.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 0n })
    const evidence = trace.captureSnapshot()?.events
    trace.disable()
    expect(trace.activation()).toEqual({ kind: 'off' })
    expect(trace.captureSnapshot()?.events).toEqual(evidence)
    expect(makeSwitch(time, store).activation()).toEqual({ kind: 'off' })
  })

  it('expires activation even after failure evidence has been cleared', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    const trace = makeSwitch(time, store)
    trace.enable()
    trace.signal({ kind: 'incident_sealed', incident: testIncident(1n), elapsedMs: 0n })
    trace.clear()
    expect(trace.status().enabled).toBe(true)
    time.advance(testTraceCapacity().captureExpiryMs)
    expect(trace.activation()).toEqual({ kind: 'off' })
    expect(store.readExpiry()).toBeUndefined()
  })

  it('clears activation even when disabling a page that has no capture', () => {
    const time = new FakeTraceTime()
    const store = activationStore()
    const idlePage = makeSwitch(time, store)
    makeSwitch(time, store).enable()
    idlePage.disable()
    expect(makeSwitch(time, store).status().state).toBe('idle')
  })

  it.each([NaN, Infinity, -1, 0, 1_000, 1_000.5, 1_101])(
    'does not activate an expired or invalid deadline: %s', expiresAt => {
      const store = activationStore(expiresAt)
      const trace = makeSwitch(new FakeTraceTime(), store)
      expect(trace.current).toBeUndefined()
      expect(trace.status().state).toBe('idle')
      expect(store.readExpiry()).toBeUndefined()
    },
  )

  it.each(['readExpiry', 'writeExpiry', 'clear'] as const)(
    'isolates storage failure in %s from capture and revocation', method => {
      const time = new FakeTraceTime()
      const store = activationStore()
      store[method] = () => { throw new Error('Storage unavailable') }
      const trace = makeSwitch(time, store)
      expect(trace.enable().enabled).toBe(true)
      trace.current?.(testEvent(1))
      expect(trace.disable().enabled).toBe(false)
      expect(trace.captureSnapshot()?.retainedEventCount).toBe(1n)
    },
  )

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
