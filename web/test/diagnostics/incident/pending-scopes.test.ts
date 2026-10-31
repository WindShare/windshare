import { describe, expect, it, vi } from 'vitest'
import { createIncidentRecordProjector, createRuntimeRunIdentity } from '../../../src/diagnostics/export/projector'
import {
  createIncidentScopeIssuer,
  excludedPresentationDecision,
  incidentPresentationDecision,
  unclassifiedFailureFact,
  type FailureFactRef,
  type IncidentScopeOwner,
} from '../../../src/diagnostics/incident'
import { MAX_PENDING_INCIDENT_CONTEXTS } from '../../../src/diagnostics/incident/pending-scopes'
import { createIncidentReporter, type IncidentReporter } from '../../../src/diagnostics/incident/reporter'

function recordFailure(scope: IncidentScopeOwner, message: string, relation: 'contributor' | 'consequence' = 'contributor') {
  return scope.facts.record(unclassifiedFailureFact({
    stage: relation === 'contributor' ? 'content_read' : 'cleanup',
    recoveryDisposition: 'terminal',
    error: new Error(message),
  }), relation)
}

function fixture() {
  const deadlines: Array<() => void> = []
  const consoleError = vi.fn()
  const traceSignal = vi.fn()
  const reporter = createIncidentReporter({
    projector: createIncidentRecordProjector({
      runtimeRunId: createRuntimeRunIdentity(new Uint8Array(16).fill(1)),
      build: { version: '0.0.0', mode: 'test' },
      secureContext: true,
    }),
    timeSource: { capture: () => ({ time: '2026-10-31T00:00:00Z', elapsedMs: 0n }) },
    consoleSink: { error: consoleError },
    traceSignals: { signal: traceSignal },
    scopeIssuer: createIncidentScopeIssuer({
      clock: { elapsedMilliseconds: () => 0 },
      scheduler: { schedule: (_delay, callback) => {
        deadlines.push(callback)
        return { cancel: () => undefined }
      } },
    }),
  })
  return { reporter, consoleError, traceSignal, deadlines }
}

function fillContexts(reporter: IncidentReporter): void {
  for (let index = 0; index < MAX_PENDING_INCIDENT_CONTEXTS; index++) {
    recordFailure(reporter.openScope('receive'), 'another pending operation')
  }
}

describe('incident decision ownership under context pressure', () => {
  it('retains supporting evidence while many operations have no failure facts', () => {
    const { reporter } = fixture()
    const scope = reporter.openScope('receive')
    const trigger = recordFailure(scope, 'receive failed')
    recordFailure(scope, 'supporting failure')
    for (let index = 0; index < MAX_PENDING_INCIDENT_CONTEXTS * 2; index++) reporter.openScope('browse')

    reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
    scope.close()

    expect(reporter.history.last()?.payload).toMatchObject({
      trigger: { evidence: { exception: { message: 'receive failed' } } },
      fact_count: '2',
      overflow_fact_count: '0',
      contributors: [{ representative: { evidence: { exception: { message: 'supporting failure' } } } }],
    })
  })

  it('accepts the first failure of a long-running operation after other contexts fill up', () => {
    const { reporter, consoleError, traceSignal } = fixture()
    const scope = reporter.openScope('receive')
    fillContexts(reporter)
    const trigger = recordFailure(scope, 'late receive failure')
    reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
    scope.close()

    expect(reporter.history.last()?.payload).toMatchObject({ fact_count: '1', overflow_fact_count: '0' })
    expect(consoleError).toHaveBeenCalledTimes(1)
    expect(traceSignal).toHaveBeenCalledTimes(1)
  })

  it.each([
    { triggerTiming: 'before eviction', seal: 'close' },
    { triggerTiming: 'after eviction', seal: 'close' },
    { triggerTiming: 'before eviction', seal: 'deadline' },
    { triggerTiming: 'after eviction', seal: 'deadline' },
  ])('reports an evicted scope with trigger $triggerTiming and decision after $seal', ({ triggerTiming, seal }) => {
    const { reporter, consoleError, traceSignal, deadlines } = fixture()
    const scope = reporter.openScope('receive')
    recordFailure(scope, 'early supporting failure')
    let trigger = triggerTiming === 'before eviction' ? recordFailure(scope, 'selected failure') : undefined
    recordFailure(scope, 'early cleanup', 'consequence')
    fillContexts(reporter)
    trigger ??= recordFailure(scope, 'selected failure')
    recordFailure(scope, 'later supporting failure')
    if (seal === 'close') scope.close()
    else deadlines[0]!()

    const decision = incidentPresentationDecision('receive', 'failed', trigger)
    reporter.submitDecision(scope.handle, decision)
    const root = reporter.history.last()
    expect(root?.payload).toMatchObject({
      scope: { scope_kind: 'receive', scope_sequence: scope.identity.scopeSequence.toString() },
      trigger: { evidence: { exception: { message: 'selected failure' } } },
      fact_count: '4',
      overflow_fact_count: '3',
      contributors: [],
      consequences: [],
      diagnostics_health_at_seal: { fact_overflow_count: '3' },
    })
    expect(consoleError).toHaveBeenCalledExactlyOnceWith(root)
    expect(traceSignal).toHaveBeenCalledExactlyOnceWith({
      kind: 'incident_sealed',
      incident: { incidentSequence: 1n, scope: scope.identity },
      elapsedMs: 0n,
    })

    reporter.submitDecision(scope.handle, decision)
    scope.close()
    deadlines[0]!()
    expect(reporter.history.snapshot()).toEqual([root])
    expect(consoleError).toHaveBeenCalledTimes(1)
    expect(reporter.health.incidentHealthSnapshot().factOverflowCount).toBe(3n)

    recordFailure(scope, 'late cleanup', 'consequence')
    expect(reporter.history.snapshot()).toEqual([root])
    expect(reporter.history.consequencesSnapshot()[0]).toMatchObject({ incident_sequence: '1', fact_count: '1' })
    expect(traceSignal).toHaveBeenCalledTimes(1)
  })

  it('still validates the issuing handle, boundary, scope and contributor before accepting a decision', () => {
    const { reporter, consoleError } = fixture()
    const scope = reporter.openScope('receive')
    const trigger = recordFailure(scope, 'selected failure')
    const consequence = recordFailure(scope, 'cleanup', 'consequence')
    fillContexts(reporter)
    scope.close()
    const decision = incidentPresentationDecision('receive', 'failed', trigger)
    const foreignScope = fixture().reporter.openScope('receive')
    const foreignTrigger = recordFailure(foreignScope, 'foreign failure')

    reporter.submitDecision({ ...scope.handle }, decision)
    reporter.submitDecision(foreignScope.handle, incidentPresentationDecision('receive', 'failed', foreignTrigger))
    reporter.submitDecision(scope.handle, incidentPresentationDecision('browse', 'failed', trigger))
    reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', foreignTrigger))
    reporter.submitDecision(scope.handle, { ...decision, trigger: consequence })
    reporter.submitDecision(scope.handle, { ...decision, trigger: { ...trigger } as FailureFactRef })
    expect(reporter.history.snapshot()).toEqual([])

    reporter.submitDecision(scope.handle, decision)
    expect(reporter.history.last()?.payload.trigger.evidence?.exception.message).toBe('selected failure')
    expect(consoleError).toHaveBeenCalledTimes(1)
  })

  it.each(['success', 'user_stopped'] as const)('keeps an evicted scope silent when the owner decides %s', reason => {
    const { reporter, consoleError, traceSignal, deadlines } = fixture()
    const scope = reporter.openScope('receive')
    const trigger = recordFailure(scope, 'temporary failure')
    fillContexts(reporter)
    scope.close()
    reporter.submitDecision(scope.handle, excludedPresentationDecision('receive', reason))
    reporter.submitDecision(scope.handle, incidentPresentationDecision('receive', 'failed', trigger))
    deadlines[0]!()
    recordFailure(scope, 'late cleanup', 'consequence')

    expect(reporter.history.snapshot()).toEqual([])
    expect(reporter.history.consequencesSnapshot()).toEqual([])
    expect(consoleError).not.toHaveBeenCalled()
    expect(traceSignal).not.toHaveBeenCalled()
  })
})
