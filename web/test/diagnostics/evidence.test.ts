import { describe, expect, it, vi } from 'vitest'
import { createBrowserDiagnosticsComposition } from '../../src/diagnostics/browser-composition'
import { deepFreezeJson } from '../../src/diagnostics/export/json'
import { FakeTraceTime } from './trace/test-support'

describe('diagnostic evidence snapshots', () => {
  it('keeps idle and unchanged evidence stable without serializing log bodies', () => {
    const { runtime, trace, time } = setup()
    const idle = runtime.readEvidence()
    expect(idle.hasEvidence).toBe(false)
    runtime.enable()
    record(trace)
    const stringify = vi.spyOn(JSON, 'stringify')
    try {
      const evidence = runtime.readEvidence()
      time.advance(10)
      expect(runtime.readEvidence()).toBe(evidence)
      expect(evidence.hasEvidence).toBe(true)
      expect(traceEncodes(stringify.mock.calls)).toBe(0)
      evidence.export()
      expect(traceEncodes(stringify.mock.calls)).toBe(1)
      evidence.export()
      expect(traceEncodes(stringify.mock.calls)).toBe(1)
    } finally {
      stringify.mockRestore()
    }
  })

  it('reuses encoded retained events while exports include fresh headers and new evidence', () => {
    const { runtime, trace, time } = setup()
    runtime.enable()
    record(trace)
    const first = runtime.export()
    const stringify = vi.spyOn(JSON, 'stringify')
    try {
      time.advance(10)
      const second = runtime.export()
      expect(second).not.toBe(first)
      expect(traceEncodes(stringify.mock.calls)).toBe(0)
      record(trace)
      const third = runtime.export()
      expect(eventLines(third)).toHaveLength(2)
      expect(traceEncodes(stringify.mock.calls)).toBe(1)
    } finally {
      stringify.mockRestore()
    }
  })

  it('detects replaced evidence with unchanged counts and preserves the earlier snapshot', () => {
    const { runtime, trace } = setup()
    runtime.enable()
    record(trace)
    const before = runtime.readEvidence()
    const original = before.export()
    trace.clear()
    record(trace)
    const after = runtime.readEvidence()
    expect(after.status.retained_event_count).toBe(before.status.retained_event_count)
    expect(after.status.retained_event_bytes).toBe(before.status.retained_event_bytes)
    expect(after).not.toBe(before)
    expect(eventLines(after.export())).not.toEqual(eventLines(original))
    expect(before.export()).toBe(original)
    runtime.disable()
    const sealed = runtime.readEvidence()
    expect(sealed).not.toBe(after)
    expect(sealed.export()).toContain('"seal_reason":"manual_disable"')
    runtime.clear()
    expect(runtime.readEvidence().hasEvidence).toBe(false)
  })
})

function setup() {
  const time = new FakeTraceTime()
  const composition = createBrowserDiagnosticsComposition({
    build: { version: '0.0.0', mode: 'test' },
    secureContext: true,
    consoleSink: { error: () => undefined },
    randomBytes: length => new Uint8Array(length).fill(1),
    scheduler: time,
    clock: { nowMilliseconds: () => time.nowMilliseconds(), captureTime: () => new Date(time.nowMilliseconds()).toISOString() },
  })
  return { ...composition, time }
}

function record(trace: ReturnType<typeof setup>['trace']) {
  trace.current?.(deepFreezeJson({ eventName: 'cleanup', payload: { backend: 'portable', transition: 'completed' } }))
}

function traceEncodes(calls: readonly (readonly unknown[])[]): number {
  return calls.filter(([value]) => typeof value === 'object' && value !== null &&
    'line_type' in value && value.line_type === 'trace_event').length
}

function eventLines(text: string): string[] {
  return text.split('\n').filter(line => line.includes('"line_type":"trace_event"'))
}
