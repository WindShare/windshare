import { describe, expect, it, vi } from 'vitest'

import {
  createIncidentPolicy,
  createIncidentScopeIssuer,
  unclassifiedFailureFact,
  type IncidentScheduleCancellation,
  type IncidentScheduler,
} from '../../../src/diagnostics/incident'

describe('incident scope ownership', () => {
  it('allocates monotonic scope and fact identities while keeping fact state lazy', () => {
    const scheduler = new FakeScheduler()
    const issuer = createIncidentScopeIssuer({
      clock: { elapsedMilliseconds: () => 12 },
      scheduler,
      policy: createIncidentPolicy({ incidentSealDeadlineMilliseconds: 17 }),
    })
    const first = issuer.open('preview_open')
    const second = issuer.open('preview_open')

    expect(first.identity).toEqual({
      scopeKind: 'preview_open',
      scopeSequence: 1n,
    })
    expect(second.identity.scopeSequence).toBe(2n)
    expect(first.handle).toEqual({
      identity: first.identity,
      facts: first.facts,
    })
    expect('close' in first.handle).toBe(false)
    expect(Object.isFrozen(first.handle)).toBe(true)
    expect(Object.isFrozen(first.facts)).toBe(true)
    expect(scheduler.scheduled).toHaveLength(0)

    const firstRef = first.facts.record(fact('preview_open'), 'contributor')
    const secondRef = first.facts.record(fact('preview_media'), 'consequence')
    expect(firstRef.factSequence).toBe(1n)
    expect(secondRef.factSequence).toBe(2n)
    expect(Object.isFrozen(firstRef)).toBe(true)
    expect(scheduler.scheduled).toHaveLength(1)
    expect(scheduler.scheduled[0]?.delayMilliseconds).toBe(17)
  })

  it('arms one deadline on the first fact and fences cancelled callbacks', () => {
    const scheduler = new FakeScheduler()
    const deadlineReached = vi.fn()
    const closed = vi.fn()
    const scope = createIncidentScopeIssuer({ scheduler }).open('browse', {
      deadlineReached,
      scopeClosed: closed,
    })

    scope.facts.record(fact('browse'), 'contributor')
    scope.facts.record(fact('protocol_operation'), 'contributor')
    expect(scheduler.scheduled).toHaveLength(1)

    scope.close()
    scope.close()
    expect(scheduler.scheduled[0]?.cancellation.cancel).toHaveBeenCalledTimes(1)
    expect(closed).toHaveBeenCalledTimes(1)

    scheduler.scheduled[0]?.callback()
    expect(deadlineReached).not.toHaveBeenCalled()
    expect(() => scope.facts.record(fact('browse'), 'contributor')).toThrow(/Closed/)
    expect(() => scope.facts.record(fact('cleanup'), 'consequence')).not.toThrow()
    expect(scheduler.scheduled).toHaveLength(1)
  })

  it('notifies the deadline once and isolates observer failures from workflow code', () => {
    const scheduler = new FakeScheduler()
    const deadlineReached = vi.fn(() => {
      throw new Error('diagnostic observer failed')
    })
    const factRecorded = vi.fn(() => {
      throw new Error('diagnostic observer failed')
    })
    const scope = createIncidentScopeIssuer({ scheduler }).open('receive', {
      deadlineReached,
      factRecorded,
      scopeClosed: () => {
        throw new Error('diagnostic observer failed')
      },
    })

    expect(() => scope.facts.record(fact('content_read'), 'contributor')).not.toThrow()
    expect(factRecorded).toHaveBeenCalledTimes(1)
    expect(() => scheduler.scheduled[0]?.callback()).not.toThrow()
    expect(() => scheduler.scheduled[0]?.callback()).not.toThrow()
    expect(deadlineReached).toHaveBeenCalledTimes(1)
    expect(() => scope.close()).not.toThrow()
  })

  it('keeps diagnostic scheduler failure out of product control flow', () => {
    const scope = createIncidentScopeIssuer({
      scheduler: {
        schedule: () => {
          throw new Error('timer unavailable')
        },
      },
    }).open('join')

    expect(() => scope.facts.record(fact('join'), 'contributor')).not.toThrow()
    expect(() => scope.close()).not.toThrow()
  })

  it('closing an empty scope remains lazy while permitting a late consequence', () => {
    const scheduler = new FakeScheduler()
    const scope = createIncidentScopeIssuer({ scheduler }).open('retained_action')
    scope.close()

    expect(scheduler.scheduled).toHaveLength(0)
    const lateRef = scope.facts.record(fact('cleanup'), 'consequence')
    expect(lateRef.factSequence).toBe(1n)
    expect(scheduler.scheduled).toHaveLength(0)
  })
})

function fact(stage: Parameters<typeof unclassifiedFailureFact>[0]['stage']) {
  return unclassifiedFailureFact({
    stage,
    recoveryDisposition: 'terminal',
  })
}

class FakeScheduler implements IncidentScheduler {
  readonly scheduled: Array<{
    readonly delayMilliseconds: number
    readonly callback: () => void
    readonly cancellation: IncidentScheduleCancellation & {
      cancel: ReturnType<typeof vi.fn>
    }
  }> = []

  schedule(
    delayMilliseconds: number,
    callback: () => void,
  ): IncidentScheduleCancellation {
    const cancellation = Object.freeze({
      cancel: vi.fn(),
    })
    this.scheduled.push({ delayMilliseconds, callback, cancellation })
    return cancellation
  }
}
