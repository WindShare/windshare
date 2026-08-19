import { describe, expect, it, vi } from 'vitest'

import {
  createIncidentPolicy,
  createIncidentScopeIssuer,
  excludedPresentationDecision,
  incidentPresentationDecision,
  unclassifiedFailureFact,
  type IncidentPolicy,
  type IncidentScheduleCancellation,
  type IncidentScheduler,
} from '../../../src/diagnostics/incident'
import {
  BoundedIncidentHistory,
  type IncidentHistoryPort,
} from '../../../src/diagnostics/incident/history'
import {
  IncidentDiagnosticsHealth,
} from '../../../src/diagnostics/incident/health'
import {
  createIncidentReporter,
  type IncidentTimeSource,
  type IncidentTraceSignalPort,
} from '../../../src/diagnostics/incident/reporter'
import {
  createIncidentRecordProjector,
  createRuntimeRunIdentity,
  type IncidentRecordProjector,
} from '../../../src/diagnostics/export/projector'
import type { IncidentRecordV1 } from '../../../src/diagnostics/export/incident-record-v1'

describe('browser incident reporter', () => {
  it('seals one immutable record before one Console call and ordered trace signals', () => {
    const consoleError = vi.fn()
    const traceSignal = vi.fn()
    const controllerRead = vi.fn(() => ({
      generation: 7n,
      phase: 'browsing' as const,
    }))
    const health = new IncidentDiagnosticsHealth({
      traceHealthSnapshot: () => ({
        droppedCount: 1n,
        overwrittenCount: 2n,
        sampledCount: 3n,
        coalescedCount: 4n,
      }),
    })
    const fixture = reporterFixture({
      consoleError,
      traceSignals: { signal: traceSignal },
      health,
      contextSources: {
        controller: { read: controllerRead },
      },
    })
    const scope = fixture.reporter.openScope('receive')
    const trigger = scope.facts.record(fact('content_read'), 'contributor')
    fixture.reporter.submitDecision(
      scope.handle,
      incidentPresentationDecision('receive', 'failed', trigger),
    )

    expect(fixture.reporter.history.last()).toBeNull()
    scope.close()

    const record = fixture.reporter.history.last()
    expect(record).not.toBeNull()
    expect(consoleError).toHaveBeenCalledTimes(1)
    expect(consoleError).toHaveBeenCalledWith(record)
    expect(controllerRead).toHaveBeenCalledTimes(1)
    expect(traceSignal.mock.calls.map(([signal]) => signal.kind)).toEqual([
      'incident_sealed',
      'scope_terminal',
    ])
    expect(record?.payload.diagnostics_health_at_seal).toEqual({
      fact_overflow_count: '0',
      incident_history_eviction_count: '0',
      console_suppression_count: '0',
      late_link_eviction_count: '0',
      trace_dropped_count: '1',
      trace_overwritten_count: '2',
      trace_sampled_count: '3',
      trace_coalesced_count: '4',
    })
    expect(Object.isFrozen(record)).toBe(true)
    expect(Object.isFrozen(record?.payload)).toBe(true)
  })

  it('keeps an explicit exclusion out of history and Console', () => {
    const consoleError = vi.fn()
    const traceSignal = vi.fn()
    const fixture = reporterFixture({
      consoleError,
      traceSignals: { signal: traceSignal },
    })
    const scope = fixture.reporter.openScope('preview_open')
    fixture.reporter.submitDecision(
      scope.handle,
      excludedPresentationDecision('preview', 'picker_refused'),
    )
    scope.close()

    expect(fixture.reporter.history.snapshot()).toEqual([])
    expect(consoleError).not.toHaveBeenCalled()
    expect(traceSignal.mock.calls.map(([signal]) => signal.kind)).toEqual([
      'scope_terminal',
    ])
  })

  it('seals at the injected deadline and does not duplicate on later close', () => {
    const consoleError = vi.fn()
    const fixture = reporterFixture({ consoleError })
    const scope = fixture.reporter.openScope('browse')
    const trigger = scope.facts.record(fact('browse'), 'contributor')
    fixture.reporter.submitDecision(
      scope.handle,
      incidentPresentationDecision('browse', 'failed', trigger),
    )

    expect(fixture.reporter.history.snapshot()).toHaveLength(0)
    fixture.scheduler.scheduled[0]?.callback()
    expect(fixture.reporter.history.snapshot()).toHaveLength(1)
    expect(consoleError).toHaveBeenCalledTimes(1)

    scope.close()
    expect(fixture.reporter.history.snapshot()).toHaveLength(1)
    expect(consoleError).toHaveBeenCalledTimes(1)
  })

  it('creates immutable late-linked records without mutating the root', () => {
    const policy = createIncidentPolicy({
      maxConsoleReportsPerScope: 1,
      maxIncidentHistoryRecords: 1,
    })
    const consoleError = vi.fn()
    const fixture = reporterFixture({ consoleError, policy })
    const scope = fixture.reporter.openScope('receive')
    const trigger = scope.facts.record(fact('content_read'), 'contributor')
    fixture.reporter.submitDecision(
      scope.handle,
      incidentPresentationDecision('receive', 'failed', trigger),
    )
    scope.close()
    const root = consoleError.mock.calls[0]?.[0] as IncidentRecordV1

    scope.facts.record(fact('cleanup'), 'consequence')

    const linked = fixture.reporter.history.last()
    expect(linked?.sequence).toBe('2')
    expect(linked?.payload.root_incident_sequence).toBe('1')
    expect(linked?.payload.fact_count).toBe('1')
    expect(linked?.payload.contributors).toEqual([])
    expect(linked?.payload.consequences).toEqual([])
    expect(linked?.payload.diagnostics_health_at_seal).toMatchObject({
      incident_history_eviction_count: '1',
      console_suppression_count: '1',
    })
    expect(consoleError).toHaveBeenCalledTimes(1)
    expect(root.payload.consequences).toEqual([])
    expect(Object.isFrozen(linked)).toBe(true)
  })

  it('folds projector pruning into the same final health cut', () => {
    const policy = createIncidentPolicy({ maxRecordListItems: 1 })
    const controllerRead = vi.fn(() => ({
      generation: 1n,
      phase: 'browsing' as const,
    }))
    const fixture = reporterFixture({
      policy,
      contextSources: {
        controller: { read: controllerRead },
      },
    })
    const scope = fixture.reporter.openScope('receive')
    const trigger = scope.facts.record(fact('content_read'), 'contributor')
    scope.facts.record(fact('output_write'), 'contributor')
    scope.facts.record(fact('cleanup'), 'contributor')
    fixture.reporter.submitDecision(
      scope.handle,
      incidentPresentationDecision('receive', 'failed', trigger),
    )
    scope.close()

    const record = fixture.reporter.history.last()
    expect(record?.payload.contributors).toHaveLength(1)
    expect(record?.payload.overflow_fact_count).toBe('1')
    expect(record?.payload.diagnostics_health_at_seal.fact_overflow_count).toBe(
      '1',
    )
    // Reprojection for the health fixed point reuses one detached context cut.
    expect(controllerRead).toHaveBeenCalledTimes(1)
  })

  it('clears retained history and revokes late-link authority together', () => {
    const fixture = reporterFixture()
    const root = reportRoot(fixture.reporter)
    fixture.reporter.clearRetainedIncidents()
    root.scope.facts.record(fact('cleanup'), 'consequence')

    expect(fixture.reporter.history.snapshot()).toEqual([])
  })

  it('evicts late links by age before accepting a late consequence', () => {
    const policy = createIncidentPolicy({
      maxLateIncidentLinkAgeMilliseconds: 10,
    })
    const time = new MutableTimeSource()
    const fixture = reporterFixture({ policy, timeSource: time })
    const scope = fixture.reporter.openScope('receive')
    const trigger = scope.facts.record(fact('content_read'), 'contributor')
    fixture.reporter.submitDecision(
      scope.handle,
      incidentPresentationDecision('receive', 'failed', trigger),
    )
    scope.close()
    time.elapsedMs = 10n

    scope.facts.record(fact('cleanup'), 'consequence')

    expect(fixture.reporter.history.snapshot()).toHaveLength(1)
    expect(
      fixture.reporter.health.incidentHealthSnapshot().lateLinkEvictionCount,
    ).toBe(1n)
  })

  it('evicts late links by count and records that decision in the next root', () => {
    const policy = createIncidentPolicy({ maxLateIncidentLinks: 1 })
    const fixture = reporterFixture({ policy })
    const first = reportRoot(fixture.reporter)
    const second = reportRoot(fixture.reporter)

    expect(
      second.record.payload.diagnostics_health_at_seal.late_link_eviction_count,
    ).toBe('1')
    first.scope.facts.record(fact('cleanup'), 'consequence')
    expect(fixture.reporter.history.snapshot()).toHaveLength(2)
  })

  it('isolates history, Console, trace, and projector failures', () => {
    const historyFailure = failingHistory()
    const consoleAfterHistoryFailure = vi.fn()
    const traceAfterHistoryFailure = vi.fn()
    const first = reporterFixture({
      history: historyFailure,
      consoleError: consoleAfterHistoryFailure,
      traceSignals: { signal: traceAfterHistoryFailure },
    })
    expect(() => reportRootAttempt(first.reporter)).not.toThrow()
    expect(consoleAfterHistoryFailure).toHaveBeenCalledTimes(1)
    expect(traceAfterHistoryFailure).toHaveBeenCalled()

    const consoleFailure = reporterFixture({
      consoleError: vi.fn(() => {
        throw new Error('console unavailable')
      }),
    })
    expect(() => reportRootAttempt(consoleFailure.reporter)).not.toThrow()
    expect(consoleFailure.reporter.history.snapshot()).toHaveLength(1)

    const traceFailure = reporterFixture({
      traceSignals: {
        signal: () => {
          throw new Error('trace unavailable')
        },
      },
    })
    expect(() => reportRootAttempt(traceFailure.reporter)).not.toThrow()
    expect(traceFailure.reporter.history.snapshot()).toHaveLength(1)

    const throwingProjector: IncidentRecordProjector = {
      project: () => {
        throw new Error('projection unavailable')
      },
    }
    const projectionFailure = reporterFixture({
      projector: throwingProjector,
    })
    expect(() => reportRootAttempt(projectionFailure.reporter)).not.toThrow()
    expect(projectionFailure.reporter.history.snapshot()).toHaveLength(0)
  })
})

function reportRoot(
  reporter: ReturnType<typeof createIncidentReporter>,
) {
  const scope = reportRootAttempt(reporter)
  const record = reporter.history.last()
  if (record === null) throw new Error('test reporter did not retain a record')
  return Object.freeze({ record, scope })
}

function reportRootAttempt(
  reporter: ReturnType<typeof createIncidentReporter>,
) {
  const scope = reporter.openScope('receive')
  const trigger = scope.facts.record(fact('content_read'), 'contributor')
  reporter.submitDecision(
    scope.handle,
    incidentPresentationDecision('receive', 'failed', trigger),
  )
  scope.close()
  return scope
}

function reporterFixture(options: {
  readonly policy?: IncidentPolicy
  readonly consoleError?: (record: IncidentRecordV1) => void
  readonly traceSignals?: IncidentTraceSignalPort
  readonly health?: IncidentDiagnosticsHealth
  readonly history?: IncidentHistoryPort
  readonly projector?: IncidentRecordProjector
  readonly timeSource?: MutableTimeSource
  readonly contextSources?: Parameters<
    typeof createIncidentReporter
  >[0]['contextSources']
} = {}) {
  const policy = options.policy ?? createIncidentPolicy()
  const scheduler = new ManualScheduler()
  const projector = options.projector ?? createIncidentRecordProjector({
    runtimeRunId: createRuntimeRunIdentity(identityBytes(1)),
    build: { version: '0.0.0', mode: 'test' },
    secureContext: true,
    policy,
  })
  const timeSource = options.timeSource ?? new MutableTimeSource()
  const reporter = createIncidentReporter({
    projector,
    timeSource,
    consoleSink: { error: options.consoleError ?? vi.fn() },
    ...(options.traceSignals === undefined
      ? {}
      : { traceSignals: options.traceSignals }),
    scopeIssuer: createIncidentScopeIssuer({
      scheduler,
      clock: { elapsedMilliseconds: () => Number(timeSource.elapsedMs) },
      policy,
    }),
    history: options.history ?? new BoundedIncidentHistory(policy),
    ...(options.health === undefined ? {} : { health: options.health }),
    ...(options.contextSources === undefined
      ? {}
      : { contextSources: options.contextSources }),
    policy,
  })
  return { reporter, scheduler, timeSource }
}

function failingHistory(): IncidentHistoryPort {
  return {
    nextAppendEvictionCount: () => {
      throw new Error('history unavailable')
    },
    append: () => {
      throw new Error('history unavailable')
    },
    last: () => null,
    snapshot: () => Object.freeze([]),
    clear: () => undefined,
  }
}

function fact(
  stage: 'browse' | 'content_read' | 'output_write' | 'cleanup',
) {
  return unclassifiedFailureFact({
    stage,
    recoveryDisposition: 'terminal',
  })
}

class MutableTimeSource implements IncidentTimeSource {
  elapsedMs = 0n

  capture() {
    return Object.freeze({
      time: '2026-08-19T01:02:03.456Z',
      elapsedMs: this.elapsedMs,
    })
  }
}

class ManualScheduler implements IncidentScheduler {
  readonly scheduled: Array<{
    readonly delayMilliseconds: number
    readonly callback: () => void
    readonly cancellation: IncidentScheduleCancellation
  }> = []

  schedule(
    delayMilliseconds: number,
    callback: () => void,
  ): IncidentScheduleCancellation {
    const cancellation = Object.freeze({ cancel: vi.fn() })
    this.scheduled.push({ delayMilliseconds, callback, cancellation })
    return cancellation
  }
}

function identityBytes(seed: number): Uint8Array {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}
