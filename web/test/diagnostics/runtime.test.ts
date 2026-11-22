import { describe, expect, it, vi } from 'vitest'
import {
  createBrowserTraceActivationStore,
  createDiagnosticFile,
  createDiagnosticsDelivery,
  createDiagnosticsRuntime,
  installWindShareDiagnostics,
  type DiagnosticsScheduledTask,
} from '../../src/diagnostics'

function createFakeClock(startMs = Date.parse('2026-11-23T00:00:00.000Z')) {
  let now = startMs
  return {
    advance(ms: number) { now += ms },
    nowMilliseconds: () => now,
    captureTime: () => new Date(now).toISOString(),
  }
}

function createFakeScheduler() {
  const tasks: { delay: number; callback: () => void; cancelled: boolean }[] = []
  return {
    tasks,
    schedule(delayMilliseconds: number, callback: () => void): DiagnosticsScheduledTask {
      const task = { delay: delayMilliseconds, callback, cancelled: false }
      tasks.push(task)
      return { cancel: () => { task.cancelled = true } }
    },
    runPending() {
      for (const task of [...tasks]) {
        if (!task.cancelled) {
          task.cancelled = true
          task.callback()
        }
      }
    },
  }
}

describe('DiagnosticsRuntime', () => {
  it('captures always-on redacted failures while trace is off and exports valid NDJSON', () => {
    const clock = createFakeClock()
    const consoleErrors: unknown[] = []
    const runtime = createDiagnosticsRuntime({
      build: { version: '1.0.0', revision: 'abcdef1', mode: 'test' },
      secureContext: true,
      clock,
      consoleSink: { error: record => consoleErrors.push(record) },
      randomBytes: length => new Uint8Array(length).fill(7),
    })

    expect(runtime.trace.current).toBeUndefined()
    expect(runtime.status()).toMatchObject({
      state: 'idle',
      enabled: false,
      failure_count: '0',
      retained_event_count: '0',
    })

    const cause = new Error('socket reset')
    const root = new Error('session failed', { cause })
    const recorded = runtime.recordFailure({
      stage: 'join',
      operation: 'join.connect',
      error: root,
      consoleReport: true,
    })

    expect(recorded).toBeDefined()
    expect(consoleErrors).toHaveLength(1)
    expect(runtime.lastFailure()).toEqual(recorded)
    expect(runtime.getSnapshot().hasEvidence).toBe(true)

    const bundle = runtime.export()
    const lines = bundle.trim().split('\n').map(line => JSON.parse(line) as Record<string, unknown>)
    expect(lines.map(line => line.line_type)).toEqual(['bundle_header', 'failure'])
    expect(lines[1]).toMatchObject({
      stage: 'join',
      operation: 'join.connect',
      error: { name: 'Error', message: 'session failed', cause: { message: 'socket reset' } },
    })

    const file = createDiagnosticFile(bundle)
    expect(file.runtimeRunId).toBe(runtime.runtimeRunId)
    expect(file.name).toMatch(/^windshare-diagnostics-.*\.ndjson$/u)
  })

  it('manages trace enable/record/disable/expiry/clear and sessionStorage activation restore', () => {
    const clock = createFakeClock()
    const scheduler = createFakeScheduler()
    const storageMap = new Map<string, string>()
    const activationStore = createBrowserTraceActivationStore(
      () => ({
        getItem: key => storageMap.get(key) ?? null,
        setItem: (key, value) => { storageMap.set(key, value) },
        removeItem: key => { storageMap.delete(key) },
      }),
      'https://windshare.top/share-a',
    )

    const runtime = createDiagnosticsRuntime({
      build: { version: '1.0.0', mode: 'test' },
      clock,
      scheduler,
      activationStore,
      traceExpiryMs: 60_000,
    })

    const enabled = runtime.enable()
    expect(enabled).toMatchObject({ state: 'recording', enabled: true, capture_generation: '1' })
    expect(runtime.trace.current).toBeDefined()
    runtime.trace.current?.({ event: 'join_transition', payload: { transition: 'started' } })
    expect(runtime.status().retained_event_count).toBe('1')

    // Restoring in a new runtime within the deadline resumes recording without extending expiry.
    clock.advance(10_000)
    const restored = createDiagnosticsRuntime({
      build: { version: '1.0.0', mode: 'test' },
      clock,
      scheduler,
      activationStore,
      traceExpiryMs: 60_000,
    })
    expect(restored.status()).toMatchObject({ state: 'recording', enabled: true, expires_at: enabled.expires_at })
    restored.dispose()

    // Expiry seals the capture and clears the persisted activation.
    clock.advance(50_000)
    scheduler.runPending()
    expect(runtime.status()).toMatchObject({ state: 'sealed', enabled: false, seal_reason: 'expired' })
    expect(runtime.trace.current).toBeUndefined()
    expect(storageMap.size).toBe(0)

    // Re-enabling starts generation 2, clear while recording keeps recording, disable seals.
    runtime.enable()
    runtime.trace.current?.({ event: 'join_transition', payload: { transition: 'joined' } })
    runtime.clear()
    expect(runtime.status()).toMatchObject({ state: 'recording', enabled: true, retained_event_count: '0' })
    expect(runtime.disable()).toMatchObject({ state: 'sealed', enabled: false, seal_reason: 'manual_disable' })
    runtime.clear()
    expect(runtime.status().state).toBe('idle')
    runtime.dispose()
  })

  it('installs frozen window.windshareDiagnostics and supports delivery actions', async () => {
    const runtime = createDiagnosticsRuntime({ build: { version: '1.0.0', mode: 'test' } })
    const host: Record<string, unknown> = {}
    const api = installWindShareDiagnostics(host, runtime)
    expect(Object.isFrozen(api)).toBe(true)
    expect(() => installWindShareDiagnostics(host, runtime)).toThrow(/already installed/u)

    const copied: string[] = []
    const revoked: string[] = []
    const delivery = createDiagnosticsDelivery({
      navigator: {
        canShare: () => false,
        share: vi.fn(),
        clipboard: { writeText: async (text: string) => { copied.push(text) } } as unknown as Navigator['clipboard'],
      },
      document: {
        createElement: () => ({ click: vi.fn(), remove: vi.fn(), href: '', download: '', hidden: false }) as unknown as HTMLElement,
        body: { append: vi.fn() } as unknown as HTMLElement,
      } as unknown as Pick<Document, 'createElement' | 'body'>,
      urls: {
        createObjectURL: () => 'blob:diag',
        revokeObjectURL: url => { revoked.push(url) },
      },
      defer: callback => callback(),
    })

    const file = runtime.exportFile()
    expect(delivery.supportsFileSharing()).toBe(false)
    delivery.save(file)
    expect(revoked).toEqual(['blob:diag'])
    await delivery.copy(file)
    expect(copied).toEqual([file.text])
    runtime.dispose()
  })
})
