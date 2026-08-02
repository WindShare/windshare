import { describe, expect, it } from 'vitest'

import {
  NETWORK_MATRIX_MAXIMUM_TRACE_STRING_BYTES,
  createNetworkMatrixTraceJournal,
  networkMatrixTrace,
  settleNetworkMatrixTraceJournal,
  type NetworkMatrixTraceIdentity,
} from '../../scripts/browser-network-matrix/trace/index.ts'

const RUN_IDENTITY: NetworkMatrixTraceIdentity = Object.freeze({
  component: 'browser-network-matrix-runner',
  scenario: 'network-matrix-run',
  operationId: 'trace-contract-run',
  runId: 'trace-contract-run',
})
const TERMINAL_CONTEXT = Object.freeze({
  cleanupOutcome: 'completed',
  lastMilestone: 'run-result-finalized',
})

describe('browser network matrix trace evidence contract', () => {
  it('retains a detached deeply immutable canonical context', () => {
    const nested = { decision: 'before', values: [2, 1] }
    const event = networkMatrixTrace(
      RUN_IDENTITY,
      'run-started',
      'started',
      { zeta: nested, alpha: 'stable' },
    )

    nested.decision = 'after'
    nested.values.push(3)

    expect(event.context).toEqual({
      alpha: 'stable',
      zeta: { decision: 'before', values: [2, 1] },
    })
    expect(Object.isFrozen(event.context)).toBe(true)
    expect(Object.isFrozen(event.context?.zeta)).toBe(true)
    expect(Object.isFrozen(
      (event.context?.zeta as Readonly<Record<string, unknown>>).values,
    )).toBe(true)
  })

  it.each([
    ['cycle', () => {
      const context: Record<string, unknown> = {}
      context.self = context
      return context
    }],
    ['bigint', () => ({ invalid: 1n })],
    ['accessor', () => Object.defineProperty({}, 'value', {
      enumerable: true,
      get: () => 'hostile',
    })],
    ['oversize', () => ({
      value: 'x'.repeat(NETWORK_MATRIX_MAXIMUM_TRACE_STRING_BYTES + 1),
    })],
    ['array extra field', () => ({
      values: Object.assign([1], { extra: true }),
    })],
    ['array symbol field', () => {
      const values = [1]
      Object.defineProperty(values, Symbol('hidden'), { value: true })
      return { values }
    }],
    ['array hidden index', () => {
      const values: number[] = []
      Object.defineProperty(values, '0', {
        value: 1,
        enumerable: false,
        configurable: true,
      })
      return { values }
    }],
  ])('rejects non-portable %s context before it reaches a journal', (_label, context) => {
    expect(() => networkMatrixTrace(
      RUN_IDENTITY,
      'run-started',
      'started',
      context(),
    )).toThrow()
  })

  it('accepts the exact run and process-owner operation ID boundaries', () => {
    const runId = 'r'.repeat(96)
    const operationId = `A.${'b'.repeat(124)}_Z`

    const event = networkMatrixTrace(
      { ...RUN_IDENTITY, runId, operationId },
      'run-started',
      'started',
    )

    expect(event).toMatchObject({ runId, operationId })
  })

  it.each([
    ['above byte ceiling', 'r'.repeat(97)],
    ['uppercase alphabet', 'Run'],
    ['underscore separator', 'run_one'],
    ['dot separator', 'run.one'],
    ['leading separator', '-run'],
    ['trailing separator', 'run-'],
    ['non-ASCII alphabet', 'r?n'],
  ])('rejects a run ID with %s', (_label, runId) => {
    expect(() => networkMatrixTrace(
      { ...RUN_IDENTITY, runId },
      'run-started',
      'started',
    )).toThrow()
  })

  it.each([
    ['above byte ceiling', 'o'.repeat(129)],
    ['leading punctuation', '.edge'],
    ['trailing punctuation', 'edge_'],
    ['slash', 'operation/one'],
    ['colon', 'operation:one'],
    ['space', 'operation one'],
    ['non-ASCII alphabet', 'op?ration'],
  ])('rejects an operation ID with %s', (_label, operationId) => {
    expect(() => networkMatrixTrace(
      { ...RUN_IDENTITY, operationId },
      'run-started',
      'started',
    )).toThrow()
  })

  it.each(['RUN STARTED', 'x'.repeat(129)])(
    'rejects the non-portable milestone %s',
    (milestone) => {
      expect(() => networkMatrixTrace(RUN_IDENTITY, milestone, 'started')).toThrow()
    },
  )

  it.each([
    ['profile', {
      component: 'browser-network-matrix-runner',
      scenario: 'network-matrix-profile',
      operationId: 'profile-operation',
      runId: RUN_IDENTITY.runId,
      profileId: 'scheduled-unknown',
    }],
    ['browser', {
      component: 'browser-network-matrix-runner',
      scenario: 'network-matrix-sample',
      operationId: 'sample-operation',
      runId: RUN_IDENTITY.runId,
      profileId: 'scheduled-public-stun',
      browser: 'unknown-browser',
      sampleOrdinal: 1,
    }],
  ])('rejects an identity outside the frozen %s vocabulary', (_label, identity) => {
    expect(() => networkMatrixTrace(
      identity as NetworkMatrixTraceIdentity,
      'profile-started',
      'started',
    )).toThrow()
  })

  it('closes an exact expected lifecycle with count, byte, and workflow evidence', () => {
    const journal = createNetworkMatrixTraceJournal(
      Object.freeze([RUN_IDENTITY]),
      4,
      32_768,
      'trace contract test',
    )
    journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-result-finalized', 'succeeded'))
    journal.append(networkMatrixTrace(
      RUN_IDENTITY,
      'run-terminal',
      'succeeded',
      TERMINAL_CONTEXT,
    ))
    journal.finish()

    const snapshot = journal.view.snapshot()
    expect(snapshot).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
      observedEvents: 3,
      capturedEvents: 3,
      observedBytes: snapshot.capturedBytes,
    })
  })

  it.each([
    ['unexpected start', Object.freeze([{
      ...RUN_IDENTITY,
      operationId: 'other-operation',
    }]), (journal: ReturnType<typeof createNetworkMatrixTraceJournal>) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    }],
    ['duplicate start', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    }],
    ['progress before start', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-profiles-settled', 'succeeded'))
    }],
    ['missing terminal', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    }],
    ['started terminal', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-terminal', 'started', {
        cleanupOutcome: 'completed',
        lastMilestone: 'run-started',
      }))
    }],
    ['terminal without settlement context', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-terminal', 'failed'))
    }],
    ['caller-asserted last milestone', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-result-finalized', 'succeeded'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-terminal', 'failed', {
        cleanupOutcome: 'completed',
        lastMilestone: 'run-profile-execution-failed',
      }))
    }],
    ['cleanup as last progress', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-terminal', 'failed', {
        cleanupOutcome: 'failed',
        lastMilestone: 'run-cleanup-failed',
      }))
    }],
    ['succeeded terminal with failed cleanup', Object.freeze([RUN_IDENTITY]), (
      journal: ReturnType<typeof createNetworkMatrixTraceJournal>,
    ) => {
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
      journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-terminal', 'succeeded', {
        cleanupOutcome: 'failed',
        lastMilestone: 'run-started',
      }))
    }],
  ] as const)('fails settlement for %s lifecycle evidence', (_label, expected, populate) => {
    const journal = createNetworkMatrixTraceJournal(
      expected,
      8,
      32_768,
      'invalid trace contract test',
    )
    populate(journal)
    expect(() => journal.finish()).toThrow()
    expect(journal.view.snapshot()).toMatchObject({
      completed: true,
      failure: { name: 'Error' },
    })
  })

})

describe('browser network matrix trace hostile-boundary containment', () => {
  it('rejects a vacuous expected set before execution can invent its scope', () => {
    expect(() => createNetworkMatrixTraceJournal(
      Object.freeze([]),
      4,
      32_768,
      'vacuous trace contract test',
    )).toThrow(/at least one expected lifecycle/u)
  })

  it('keeps missing-lifecycle and append-after-finish failures sticky', () => {
    const journal = createNetworkMatrixTraceJournal(
      Object.freeze([RUN_IDENTITY]),
      4,
      32_768,
      'sticky trace contract test',
    )
    journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    expect(() => journal.finish()).toThrow(/did not publish exactly one start and terminal/u)
    expect(() => journal.finish()).toThrow(/did not publish exactly one start and terminal/u)
    journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-result-finalized', 'succeeded'))
    expect(() => journal.finish()).toThrow(/did not publish exactly one start and terminal/u)
    expect(journal.view.snapshot()).toMatchObject({
      completed: true,
      truncated: true,
      failure: { message: expect.stringContaining('did not publish exactly one start and terminal') },
    })
  })

  it('rejects operation ID reuse across distinct expected dimensions', () => {
    expect(() => createNetworkMatrixTraceJournal(Object.freeze([
      RUN_IDENTITY,
      Object.freeze({
        component: 'browser-network-matrix-runner',
        scenario: 'network-matrix-profile',
        operationId: RUN_IDENTITY.operationId,
        runId: RUN_IDENTITY.runId,
        profileId: 'scheduled-public-stun',
      }),
    ]), 4, 32_768, 'unique operation trace test')).toThrow(/operation ID is not unique/u)
  })

  it('rejects event-only fields in an expected identity', () => {
    expect(() => createNetworkMatrixTraceJournal(Object.freeze([{
      ...RUN_IDENTITY,
      schemaVersion: 'windshare.browser-network-matrix-trace/v1',
      milestone: 'run-started',
      outcome: 'started',
    } as NetworkMatrixTraceIdentity]), 4, 32_768, 'strict identity trace test'))
      .toThrow(/unknown keys/u)
  })

  it('rejects symbolic context fields without dropping them', () => {
    const context: Record<PropertyKey, unknown> = { safe: true }
    Object.defineProperty(context, Symbol('hidden'), { value: 'secret', enumerable: true })
    expect(() => networkMatrixTrace(
      RUN_IDENTITY,
      'run-started',
      'started',
      context as Readonly<Record<string, unknown>>,
    )).toThrow(/symbolic/u)
  })

  it('preserves __proto__ as inert null-prototype data', () => {
    const event = networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started', {
      ['__proto__']: { polluted: true },
    })
    expect(Object.getPrototypeOf(event.context)).toBeNull()
    expect(event.context?.['__proto__']).toEqual({ polluted: true })
    expect(({} as { polluted?: boolean }).polluted).toBeUndefined()
  })

  it('rejects Proxy identity and context values before invoking their traps', () => {
    let traps = 0
    const hostileContext = new Proxy({ value: 'blocked' }, {
      getPrototypeOf: () => {
        traps += 1
        throw new Error('proxy trap executed')
      },
    })
    const hostileIdentity = new Proxy(RUN_IDENTITY, {
      ownKeys: () => {
        traps += 1
        throw new Error('proxy trap executed')
      },
    })
    const hostileExpectedIdentities = new Proxy([RUN_IDENTITY], {
      get: () => {
        traps += 1
        throw new Error('proxy trap executed')
      },
    })
    expect(() => networkMatrixTrace(
      RUN_IDENTITY,
      'run-started',
      'started',
      { hostileContext },
    )).toThrow(/Proxy/u)
    expect(() => networkMatrixTrace(
      hostileIdentity,
      'run-started',
      'started',
    )).toThrow(/Proxy/u)
    expect(() => createNetworkMatrixTraceJournal(
      hostileExpectedIdentities,
      4,
      4_096,
      'hostile expected identity list',
    )).toThrow(/at least one expected lifecycle/u)
    expect(traps).toBe(0)
  })

  it('records a framework-owned failure without invoking hostile event traps', () => {
    let traps = 0
    const hostileEvent = new Proxy(
      networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'),
      {
        get: () => {
          traps += 1
          throw new Error('event get trap must remain opaque')
        },
        getPrototypeOf: () => {
          traps += 1
          throw new Error('event prototype trap must remain opaque')
        },
      },
    )
    const journal = createNetworkMatrixTraceJournal(
      Object.freeze([RUN_IDENTITY]),
      4,
      4_096,
      'hostile event trace contract test',
    )

    journal.append(hostileEvent)
    expect(() => journal.finish()).toThrow(/rejected a malformed event/u)
    expect(journal.view.snapshot()).toMatchObject({
      completed: true,
      failure: {
        name: 'Error',
        message: expect.stringContaining('rejected a malformed event'),
      },
    })
    expect(traps).toBe(0)
  })

  it('retains operation and incomplete-evidence causes when both settlement domains fail', async () => {
    const operationFailure = new Error('orchestration failed first')
    const journal = createNetworkMatrixTraceJournal(
      Object.freeze([RUN_IDENTITY]),
      4,
      4_096,
      'dual failure trace contract test',
    )

    let observed: unknown
    try {
      await settleNetworkMatrixTraceJournal(Promise.reject(operationFailure), journal)
    } catch (cause) {
      observed = cause
    }

    expect(observed).toBeInstanceOf(AggregateError)
    const aggregate = observed as AggregateError
    expect(aggregate.errors[0]).toBe(operationFailure)
    expect(aggregate.errors[1]).toEqual(expect.objectContaining({
      message: expect.stringContaining('did not publish exactly one start and terminal'),
    }))
    expect(aggregate.cause).toBe(operationFailure)
  })

  it('fails settlement when encoded evidence exceeds its byte authority', () => {
    const journal = createNetworkMatrixTraceJournal(
      Object.freeze([RUN_IDENTITY]),
      4,
      32,
      'bounded trace contract test',
    )
    journal.append(networkMatrixTrace(RUN_IDENTITY, 'run-started', 'started'))
    journal.append(networkMatrixTrace(
      RUN_IDENTITY,
      'run-terminal',
      'failed',
      { cleanupOutcome: 'failed', lastMilestone: 'run-started' },
    ))
    expect(() => journal.finish()).toThrow(/byte capture authority/u)
    expect(journal.view.snapshot()).toMatchObject({
      completed: true,
      truncated: true,
      capturedEvents: 0,
      failure: { message: expect.stringContaining('byte capture authority') },
    })
  })
})
