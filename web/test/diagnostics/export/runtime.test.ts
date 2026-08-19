import { describe, expect, it, vi } from 'vitest'

import {
  createBrowserDiagnosticsRuntime,
  type DiagnosticsIncidentRuntimePort,
  type DiagnosticsTraceRuntimePort,
} from '../../../src/diagnostics/runtime'
import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../../src/diagnostics/export/trace-event-v1'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'
import type { IncidentDiagnosticsHealthSnapshot } from '../../../src/diagnostics/incident/health'
import type { IncidentRecordV1 } from '../../../src/diagnostics/export/incident-record-v1'
import type { TraceCoreStatus } from '../../../src/diagnostics/trace/model'
import type { TraceEventObservationV1 } from '../../../src/diagnostics/trace/model'
import { TraceSwitch } from '../../../src/diagnostics/trace/switch'
import type { IncidentLink } from '../../../src/diagnostics/incident/reporter'
import type { IncidentScopeIdentity } from '../../../src/diagnostics/incident/scope'
import { FakeTraceTime } from '../trace/test-support'
import {
  TEST_BUNDLE_IDENTITY,
  incidentHealth,
  incidentRecord,
  traceStatus,
} from './test-support'

describe('browser diagnostics runtime', () => {
  it('delegates enable/disable without touching incident history', () => {
    const incident = fakeIncident([incidentRecord('1')])
    const trace = new FakeTracePort()
    const runtime = createBrowserDiagnosticsRuntime({
      identity: TEST_BUNDLE_IDENTITY,
      incident,
      trace,
    })

    const enabled = runtime.enable()
    expect(enabled).toMatchObject({
      state: 'recording_pre_failure',
      enabled: true,
      capture_generation: '1',
      expires_at: '2026-08-19T02:00:00.000Z',
    })
    expect(runtime.inspectLastFailure()).toBe(incidentRecordFrom(incident))

    const disabled = runtime.disable()
    expect(disabled).toMatchObject({
      state: 'sealed',
      enabled: false,
      capture_generation: '1',
      seal_reason: 'manual_disable',
    })
    expect(runtime.inspectLastFailure()?.sequence).toBe('1')
    expect(Object.isFrozen(enabled)).toBe(true)
    expect(Object.isFrozen(enabled.health)).toBe(true)
  })

  it('takes one read from every live export port and does not mutate capture state', () => {
    const health = incidentHealth({
      incidentHistoryEvictionCount: 4n,
      traceSampledCount: 7n,
    })
    const incident = fakeIncident([incidentRecord('1')], health)
    const trace = new FakeTracePort()
    trace.enable()
    const runtime = createBrowserDiagnosticsRuntime({
      identity: TEST_BUNDLE_IDENTITY,
      incident,
      trace,
      timeSource: Object.freeze({
        captureTime: vi.fn(() => '2026-08-19T01:30:00Z'),
      }),
    })
    const before = trace.status()
    trace.resetReadCounts()

    const exported = runtime.export()

    expect(trace.statusReads).toBe(1)
    expect(trace.captureReads).toBe(1)
    expect(incident.history.snapshot).toHaveBeenCalledTimes(1)
    expect(incident.health.incidentHealthSnapshot).toHaveBeenCalledTimes(1)
    expect(trace.peek()).toEqual(before)
    const header = JSON.parse(exported.split('\n')[0]!) as {
      diagnostics_health_at_export: Record<string, string>
    }
    expect(header.diagnostics_health_at_export).toMatchObject({
      incident_history_eviction_count: '4',
      trace_sampled_count: '7',
    })
  })

  it('clears incident and trace stores independently when either port fails', () => {
    const incidentClear = vi.fn(() => { throw new Error('history unavailable') })
    const traceClear = vi.fn(() => { throw new Error('trace unavailable') })
    const incident = fakeIncident([])
    incident.clearRetainedIncidents = incidentClear
    const trace = new FakeTracePort(traceClear)
    const runtime = createBrowserDiagnosticsRuntime({
      identity: TEST_BUNDLE_IDENTITY,
      incident,
      trace,
    })

    expect(() => runtime.clear()).not.toThrow()
    expect(incidentClear).toHaveBeenCalledOnce()
    expect(traceClear).toHaveBeenCalledOnce()
  })

  it('does not expose a mutable or fallible custom history value', () => {
    const incident = fakeIncident([])
    incident.history.last = vi.fn(() => ({ sequence: '1' }) as IncidentRecordV1)
    const runtime = createBrowserDiagnosticsRuntime({
      identity: TEST_BUNDLE_IDENTITY,
      incident,
      trace: new FakeTracePort(),
    })

    expect(runtime.inspectLastFailure()).toBeNull()
    incident.history.last = vi.fn(() => { throw new Error('history failure') })
    expect(runtime.inspectLastFailure()).toBeNull()
  })

  it('preserves active expiry/generation when clear resets a real trace switch', () => {
    const time = new FakeTraceTime()
    const trace = realTraceSwitch(time)
    const incident = fakeIncident([incidentRecord('1')])
    const runtime = createBrowserDiagnosticsRuntime({
      identity: TEST_BUNDLE_IDENTITY,
      incident,
      trace,
      timeSource: Object.freeze({ captureTime: () => '2026-08-19T01:30:00Z' }),
    })
    const enabled = runtime.enable()
    trace.current?.(deepFreezeJson({
      eventName: 'cleanup',
      payload: { backend: 'portable', transition: 'completed' },
    }))

    const exported = runtime.export()
    expect(exported).toContain('"line_type":"trace_event"')
    expect(runtime.export()).toBe(exported)
    expect(runtime.status().retained_event_count).toBe('1')

    runtime.clear()
    const cleared = runtime.status()
    expect(cleared.capture_generation).toBe(enabled.capture_generation)
    expect(cleared.expires_at).toBe(enabled.expires_at)
    expect(cleared.retained_event_count).toBe('0')
    expect(cleared.state).toBe('recording_pre_failure')
    expect(runtime.inspectLastFailure()).toBeNull()

    trace.current?.(deepFreezeJson({
      eventName: 'cleanup',
      payload: { backend: 'portable', transition: 'failed' },
    }))
    expect(runtime.disable().state).toBe('sealed')
    expect(runtime.export()).toContain('"line_type":"trace_event"')
    runtime.clear()
    expect(runtime.status().state).toBe('idle')
    expect(runtime.export()).not.toContain('"line_type":"trace_capture"')
  })
})

function fakeIncident(
  records: readonly IncidentRecordV1[],
  health: IncidentDiagnosticsHealthSnapshot = incidentHealth(),
) {
  const retained = [...records]
  const port = {
    history: {
      last: vi.fn(() => retained.at(-1) ?? null),
      snapshot: vi.fn(() => Object.freeze([...retained])),
    },
    health: {
      incidentHealthSnapshot: vi.fn(() => health),
    },
    clearRetainedIncidents: vi.fn(() => { retained.splice(0) }),
  }
  return port satisfies DiagnosticsIncidentRuntimePort
}

function incidentRecordFrom(incident: DiagnosticsIncidentRuntimePort): IncidentRecordV1 | null {
  return incident.history.last()
}

class FakeTracePort implements DiagnosticsTraceRuntimePort {
  #status: TraceCoreStatus = traceStatus()
  readonly #clearHook: (() => void) | undefined
  statusReads = 0
  captureReads = 0

  constructor(clearHook?: () => void) {
    this.#clearHook = clearHook
  }

  enable(): TraceCoreStatus {
    const generation = this.#status.captureGeneration + 1n
    this.#status = traceStatus({
      state: 'recording_pre_failure',
      enabled: true,
      captureGeneration: generation,
      expiresAtMilliseconds: Date.parse('2026-08-19T02:00:00Z'),
    })
    return this.#status
  }

  disable(): TraceCoreStatus {
    if (this.#status.enabled) {
      this.#status = traceStatus({
        state: 'sealed',
        enabled: false,
        captureGeneration: this.#status.captureGeneration,
        sealReason: 'manual_disable',
      })
    }
    return this.#status
  }

  status(): TraceCoreStatus {
    this.statusReads += 1
    return this.#status
  }

  clear(): void {
    this.#clearHook?.()
    if (this.#status.enabled) return
    this.#status = traceStatus({ captureGeneration: this.#status.captureGeneration })
  }

  captureSnapshot() {
    this.captureReads += 1
    if (this.#status.state === 'idle') return undefined
    return Object.freeze({
      state: this.#status.state,
      captureGeneration: this.#status.captureGeneration,
      startedAtMilliseconds: Date.parse('2026-08-19T01:00:00Z'),
      ...(this.#status.sealReason === undefined ? {} : { sealReason: this.#status.sealReason }),
      retainedEventCount: 0n,
      retainedEventBytes: 0n,
      incidentMarkerCount: 0n,
      events: Object.freeze([]),
      health: Object.freeze({
        droppedCount: 0n,
        overwrittenCount: 0n,
        sampledCount: 7n,
        coalescedCount: 0n,
      }),
    })
  }

  resetReadCounts(): void {
    this.statusReads = 0
    this.captureReads = 0
  }

  peek(): TraceCoreStatus {
    return this.#status
  }
}

function realTraceSwitch(time: FakeTraceTime) {
  return new TraceSwitch<
    TraceEventObservationV1,
    IncidentLink,
    IncidentScopeIdentity
  >({
    clock: time,
    scheduler: time,
    eventName: traceEventObservationNameV1,
    snapshotEvent: snapshotTraceEventObservationV1,
    eventBytes: traceEventObservationBytesV1,
    snapshotIncident: (incident) => Object.freeze({
      incidentSequence: incident.incidentSequence,
      ...(incident.rootIncidentSequence === undefined
        ? {}
        : { rootIncidentSequence: incident.rootIncidentSequence }),
      scope: Object.freeze({ ...incident.scope }),
    }),
    incidentMarkerBytes: () => 1,
    incidentScope: (incident) => incident.scope,
    sameScope: (left, right) =>
      left.scopeKind === right.scopeKind && left.scopeSequence === right.scopeSequence,
  })
}
