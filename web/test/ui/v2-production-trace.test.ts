import { describe, expect, it, vi } from 'vitest'

import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import {
  createBrowserDiagnosticsComposition,
  type BrowserDiagnosticsClock,
} from '../../src/diagnostics/browser-composition'
import { installWindShareDiagnostics } from '../../src/diagnostics/export/developer-api'
import { unclassifiedFailureFact } from '../../src/diagnostics/incident'
import type {
  TraceScheduledTask,
  TraceScheduler,
} from '../../src/diagnostics/trace/ports'
import {
  createV2ReceiverTraceSource,
  projectV2ReceiverTraceEvent,
} from '../../src/ui/v2-production-trace'
import type { V2ReceiverTraceEvent } from '../../src/ui/v2-controller'

describe('browser diagnostics production composition', () => {
  it('uses injected package identity and the test build mode', () => {
    expect(browserBuildSnapshot()).toEqual({
      version: '0.0.0',
      mode: 'test',
    })
  })

  it('keeps the event factory untouched until tracing is explicitly enabled', () => {
    const composition = productionComposition()
    const source = createV2ReceiverTraceSource(composition.trace)
    const createEvent = vi.fn<() => V2ReceiverTraceEvent>(() =>
      Object.freeze({
        name: 'join_transition',
        transition: 'started',
      }))

    emit(source, createEvent)
    expect(createEvent).not.toHaveBeenCalled()
    expect(composition.runtime.status()).toMatchObject({
      state: 'idle',
      enabled: false,
      retained_event_count: '0',
    })

    composition.runtime.enable()
    emit(source, createEvent)
    expect(createEvent).toHaveBeenCalledOnce()
    expect(composition.runtime.status()).toMatchObject({
      state: 'recording_pre_failure',
      enabled: true,
      retained_event_count: '1',
    })
  })

  it('exports enabled trace without writing per-event console output', () => {
    const error = vi.fn()
    const composition = productionComposition(error)
    const source = createV2ReceiverTraceSource(composition.trace)

    composition.runtime.enable()
    emit(source, () => Object.freeze({
      name: 'join_transition',
      transition: 'started',
    }))
    const lines = composition.runtime.export().trimEnd().split('\n').map(
      (line) => JSON.parse(line) as Record<string, unknown>,
    )

    expect(lines.map((line) => line.line_type)).toEqual([
      'bundle_header',
      'trace_capture',
      'trace_event',
    ])
    expect(lines[2]).toMatchObject({
      line_type: 'trace_event',
      record: {
        event: 'join_transition',
        payload: { transition: 'started' },
      },
    })
    expect(error).not.toHaveBeenCalled()
  })

  it('keeps retained-inventory failure events closed at the typed adapter', () => {
    const projected = projectV2ReceiverTraceEvent({
      name: 'receive.inventory.load.failed',
    })

    expect(projected).toEqual({
      eventName: 'retained_inventory',
      payload: { transition: 'load_failed' },
    })
    expect(Object.keys(projected.payload)).toEqual(['transition'])
  })

  it('retains and exports an incident even when the console sink throws', () => {
    const error = vi.fn(() => {
      throw new Error('console unavailable')
    })
    const composition = productionComposition(error)
    const owner = composition.incidents.openScope('join')
    const trigger = owner.facts.record(unclassifiedFailureFact({
      stage: 'join',
      recoveryDisposition: 'terminal',
    }), 'contributor')

    composition.incidents.submitDecision(owner.handle, {
      kind: 'incident',
      boundary: 'join',
      outcome: 'failed',
      trigger,
    })
    owner.close()

    expect(error).toHaveBeenCalledOnce()
    expect(composition.runtime.inspectLastFailure()).toMatchObject({
      event: 'failure_incident',
      payload: {
        scope: { scope_kind: 'join', scope_sequence: '1' },
        presentation: { boundary: 'join', outcome: 'failed' },
      },
    })
    expect(composition.runtime.export()).toContain('"line_type":"incident"')
  })

  it('installs one frozen readonly developer facade without product dependencies', () => {
    const composition = productionComposition()
    const target = {}
    const api = installWindShareDiagnostics(target, composition.runtime)
    const descriptor = Object.getOwnPropertyDescriptor(
      target,
      'windshareDiagnostics',
    )

    expect(Object.isFrozen(api)).toBe(true)
    expect(descriptor).toMatchObject({
      configurable: false,
      enumerable: false,
      writable: false,
      value: api,
    })
    expect(api.status()).toMatchObject({ state: 'idle', enabled: false })
  })
})

function productionComposition(error = vi.fn()) {
  let now = 1_000
  const clock: BrowserDiagnosticsClock = Object.freeze({
    nowMilliseconds: () => now++,
    captureTime: () => new Date(now++).toISOString(),
  })
  const scheduler: TraceScheduler = Object.freeze({
    schedule: (): TraceScheduledTask => Object.freeze({ cancel: () => undefined }),
  })
  return createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    secureContext: true,
    consoleSink: Object.freeze({ error }),
    randomBytes: (byteLength) =>
      Uint8Array.from({ length: byteLength }, (_, index) => index + 1),
    clock,
    scheduler,
  })
}

function emit(
  source: ReturnType<typeof createV2ReceiverTraceSource>,
  createEvent: () => V2ReceiverTraceEvent,
): void {
  const observer = source.current
  if (observer === undefined) return
  observer(createEvent())
}
