import { describe, expect, it } from 'vitest'

import {
  createFailureIdentity,
  createIncidentScopeIssuer,
} from '../../src/diagnostics/incident'
import {
  BoundedLocalOutputOperationFailureHistory,
  bindLocalOutputFailureProtocolAttempt,
  createAttemptOutputFailureCapability,
  createOutputFailureBinding,
  type OutputExceptionProjection,
} from '../../src/output/diagnostics'
import {
  persistentOutputCapturedException,
  type PersistentOutputStageFailureMilestone,
  type PersistentOutputStageMilestone,
} from '../../src/output/persistent-tree/stage-diagnostics'

describe('local output failure diagnostics', () => {
  it('retains bounded attempt-correlated projections without retaining raw identity', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const attempt = createAttemptOutputFailureCapability(scope.handle)
    const history = new BoundedLocalOutputOperationFailureHistory(2)
    const diagnostics = history.forAttempt({
      attempt: requiredAttempt(attempt.sinks.attempt),
      transferJobId: 'transfer-job',
      outputSessionId: 'output-session',
    })
    bindLocalOutputFailureProtocolAttempt(scope.handle, {
      transferJobId: 'transfer-job',
      protocolSessionIdentity: createFailureIdentity(
        'protocol_session',
        new Uint8Array(16).fill(7),
      ),
      protocolGeneration: 3,
    })
    const first = new DOMException('first failure', 'QuotaExceededError')
    const second = new Error('second failure', { cause: 'disk detached' })
    const third = Object.freeze({ reason: 'third failure' })

    diagnostics.observe(startedMilestone(1))
    diagnostics.observe(failedMilestone(2, first))
    diagnostics.observe(startedMilestone(3))
    diagnostics.observe(failedMilestone(4, second))
    diagnostics.observe(startedMilestone(5))
    diagnostics.observe(failedMilestone(6, third))

    expect(history.snapshot().map(record => record.failure.stageFailure.sequence)).toEqual([4, 6])
    expect(JSON.stringify(history.snapshot())).not.toContain('"raw"')
    expect(history.snapshot()[0]).toMatchObject({
      owningScope: {
        scopeKind: 'receive',
        scopeSequence: scope.identity.scopeSequence.toString(10),
      },
      correlation: {
        protocol_session_id: 'BwcHBwcHBwcHBwcHBwcHBw',
        protocol_generation: 3,
        receive_operation_id: 'receive-operation',
        transfer_job_id: 'transfer-job',
        output_session_id: 'output-session',
      },
      failure: {
        stageFailure: {
          exception: {
            javascriptKind: 'unknown',
            message: 'second failure',
            cause: 'disk detached',
          },
        },
      },
    })
    expect(history.snapshot()[1]?.failure.stageFailure.exception.thrownValue)
      .toBe('[object Object]')

    history.clear()
    expect(history.snapshot()).toEqual([])
  })

  it('drops revoked in-flight work, attributes successors only after their own start, and revokes on clear', () => {
    const issuer = createIncidentScopeIssuer()
    const firstScope = issuer.open('receive')
    const secondScope = issuer.open('receive')
    const first = createAttemptOutputFailureCapability(firstScope.handle)
    const second = createAttemptOutputFailureCapability(secondScope.handle)
    const binding = createOutputFailureBinding(first.sinks)
    const history = new BoundedLocalOutputOperationFailureHistory()
    const diagnostics = history.forAttempt({
      attempt: requiredAttempt(binding.sinks.attempt),
      transferJobId: 'transfer-job',
      outputSessionId: 'output-session',
    })

    diagnostics.observe(startedMilestone(1))
    first.revoke()
    binding.bind(second.sinks)
    diagnostics.observe(failedMilestone(2, new Error('late first attempt')))
    diagnostics.observe(startedMilestone(3))
    diagnostics.observe(failedMilestone(4, new Error('owned second attempt')))

    expect(history.snapshot()).toHaveLength(1)
    expect(history.snapshot()[0]?.owningScope.scopeSequence)
      .toBe(secondScope.identity.scopeSequence.toString(10))

    history.clear()
    diagnostics.observe(startedMilestone(5))
    diagnostics.observe(failedMilestone(6, new Error('late after clear')))
    expect(history.snapshot()).toEqual([])
  })

  it('normalizes hostile and native thrown values without granting product meaning', () => {
    const hostile = new Proxy({}, {
      get: () => {
        throw new Error('hostile getter')
      },
    })

    expect(exceptionProjection(hostile)).toEqual({
      javascriptKind: 'unknown',
      nativeClass: 'unknown',
      thrownType: 'object',
      constructorName: null,
      errorName: null,
      message: null,
      stack: null,
      thrownValue: '[unprintable thrown value]',
      cause: null,
    })
    expect(exceptionProjection(
      new DOMException('storage refused', 'NotAllowedError'),
    )).toMatchObject({
      javascriptKind: 'dom-exception',
      nativeClass: 'not_allowed',
      errorName: 'NotAllowedError',
      message: 'storage refused',
    })
  })
})

function requiredAttempt<T>(attempt: T | undefined): T {
  if (attempt === undefined) throw new Error('attempt correlation source is missing')
  return attempt
}

function startedMilestone(sequence: number): PersistentOutputStageMilestone {
  return Object.freeze({
    sequence,
    transition: 'started',
    stage: 'fsa.file.writer.write',
    correlation: correlation(),
  })
}

function failedMilestone(
  sequence: number,
  error: unknown,
): PersistentOutputStageFailureMilestone {
  return Object.freeze({
    sequence,
    transition: 'failed',
    stage: 'fsa.file.writer.write',
    correlation: correlation(),
    exception: persistentOutputCapturedException(error),
    facts: emptyFailureFacts(),
  })
}

function exceptionProjection(error: unknown): OutputExceptionProjection {
  return persistentOutputCapturedException(error).projection
}

function correlation() {
  return Object.freeze({
    operationId: 'receive-operation',
    outputSessionId: 'output-session',
    target: 'file' as const,
    artifactId: 'source-file',
    artifactPath: Object.freeze(['folder', 'file.bin']),
  })
}

function emptyFailureFacts() {
  return Object.freeze({
    observation: Object.freeze({
      deadlineMilliseconds: 100,
      timedOut: false,
      providerCount: 0,
      completedProviderCount: 0,
      activeFileEvidenceCount: 0,
      checkpointPagesRead: 0,
      checkpointRecordsRetained: 0,
      retainedBytes: 0,
      truncation: Object.freeze([]),
      unavailableProviders: Object.freeze([]),
    }),
  })
}
