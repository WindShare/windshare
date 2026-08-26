import { describe, expect, it, vi } from 'vitest'

import {
  createFailureIdentity,
  createIncidentScopeIssuer,
  type FailureFact,
  type FailureFactRelation,
  type FailureFactSink,
} from '../../src/diagnostics/incident'
import {
  bindLocalOutputFailureProtocolAttempt,
  createAttemptOutputFailureCapability,
  createLateOutputCleanupCapability,
  createOutputFailureBinding,
  createOutputFailureSink,
  emitOutputTrace,
  nativeFailureClass,
  outputTraceEvent,
  recordOutputException,
  type OutputTraceEvent,
  type OutputTraceSource,
} from '../../src/output/diagnostics'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import { checkpointAuthorityObserver } from '../../src/output/file-system-access/session-diagnostics'

describe('output diagnostics', () => {
  it('records a stage-fixed privacy-safe consequence without retaining the exception', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const facts: FailureFact[] = []
    const relations: FailureFactRelation[] = []
    const forwarding: FailureFactSink = Object.freeze({
      record: (fact: FailureFact, relation: FailureFactRelation) => {
        facts.push(fact)
        relations.push(relation)
        return scope.facts.record(fact, relation)
      },
    })
    const sink = createOutputFailureSink({
      facts: forwarding,
      stage: 'settlement',
      relation: 'consequence',
      recoveryDisposition: 'needs_attention',
    })
    const error = new TargetOwnershipUnknownError('settlement', 'private-operation', {
      cause: new Error('private path and name'),
    })

    const ref = recordOutputException(sink, error)

    expect(ref?.scope).toEqual(scope.identity)
    expect(relations).toEqual(['consequence'])
    expect(facts).toEqual([
      expect.objectContaining({
        kind: 'native_output_failure',
        stage: 'settlement',
        recoveryDisposition: 'needs_attention',
        payload: {
          nativeOutputFailure: {
            nativeClass: 'invalid_state',
            code: 'ownership',
          },
        },
      }),
    ])
    const encoded = JSON.stringify(facts)
    expect(encoded).not.toContain('private-operation')
    expect(encoded).not.toContain('private path')
    expect(encoded).not.toContain('TargetOwnershipUnknownError')
  })

  it('keeps browser exception classification closed and does not trust arbitrary names', () => {
    expect(nativeFailureClass(new DOMException('ignored', 'QuotaExceededError')))
      .toBe('quota_exceeded')
    expect(nativeFailureClass(Object.freeze({ name: 'QuotaExceededError' }))).toBe('unknown')
    expect(nativeFailureClass(new Error('ignored'))).toBe('unknown')
    expect(nativeFailureClass(new Error('outer', {
      cause: new DOMException('ignored', 'QuotaExceededError'),
    }))).toBe('unknown')
  })

  it('isolates a failing fact sink from the output result', () => {
    const sink = createOutputFailureSink({
      facts: Object.freeze({
        record: () => {
          throw new Error('diagnostic sink failed')
        },
      }) as FailureFactSink,
      stage: 'cleanup',
      relation: 'consequence',
      recoveryDisposition: 'needs_attention',
    })

    expect(recordOutputException(sink, new DOMException('ignored', 'InvalidStateError')))
      .toBeUndefined()
    expect(recordOutputException(Object.freeze({
      stage: 'cleanup',
      record: () => {
        throw new Error('direct diagnostic sink failed')
      },
    }), new DOMException('ignored', 'InvalidStateError'))).toBeUndefined()
  })

  it('routes a long-lived output runtime only to the currently bound attempt', () => {
    const issuer = createIncidentScopeIssuer()
    const firstFacts: Array<Readonly<{ fact: FailureFact; relation: FailureFactRelation }>> = []
    const secondFacts: Array<Readonly<{ fact: FailureFact; relation: FailureFactRelation }>> = []
    const first = issuer.open('receive', {
      factRecorded: observation => firstFacts.push({
        fact: observation.fact,
        relation: observation.relation,
      }),
    })
    const second = issuer.open('receive', {
      factRecorded: observation => secondFacts.push({
        fact: observation.fact,
        relation: observation.relation,
      }),
    })
    const firstAttempt = createAttemptOutputFailureCapability(first.handle)
    const secondAttempt = createAttemptOutputFailureCapability(second.handle)
    const binding = createOutputFailureBinding()
    const firstLease = binding.bind(firstAttempt.sinks)

    recordOutputException(
      binding.sinks.outputReservation,
      new DOMException('private', 'NotAllowedError'),
    )
    const secondLease = binding.bind(secondAttempt.sinks)
    firstLease.revoke()
    recordOutputException(
      binding.sinks.checkpoint,
      new DOMException('private', 'QuotaExceededError'),
    )
    recordOutputException(
      binding.sinks.cleanup,
      new DOMException('private', 'InvalidStateError'),
    )

    expect(firstFacts).toMatchObject([
      {
        fact: {
          kind: 'native_output_failure',
          stage: 'output_reservation',
          payload: { nativeOutputFailure: { nativeClass: 'not_allowed' } },
        },
        relation: 'contributor',
      },
    ])
    expect(secondFacts).toMatchObject([
      {
        fact: {
          kind: 'native_output_failure',
          stage: 'checkpoint',
          payload: { nativeOutputFailure: { nativeClass: 'quota_exceeded' } },
        },
        relation: 'contributor',
      },
      {
        fact: {
          kind: 'native_output_failure',
          stage: 'cleanup',
          payload: { nativeOutputFailure: { nativeClass: 'invalid_state' } },
        },
        relation: 'consequence',
      },
    ])

    secondLease.revoke()
    recordOutputException(
      binding.sinks.outputWrite,
      new DOMException('private', 'AbortError'),
    )
    expect(secondFacts).toHaveLength(2)
  })

  it('completes one incident-owned attempt from the gateway correlation cut exactly once', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const attempt = createAttemptOutputFailureCapability(scope.handle)
    const protocolSessionIdentity = createFailureIdentity(
      'protocol_session',
      new Uint8Array(16).fill(4),
    )

    expect(bindLocalOutputFailureProtocolAttempt(scope.handle, {
      transferJobId: 'transfer-job',
      protocolSessionIdentity,
      protocolGeneration: 9,
    })).toBe(true)
    expect(bindLocalOutputFailureProtocolAttempt(scope.handle, {
      transferJobId: 'transfer-job',
      protocolSessionIdentity,
      protocolGeneration: 9,
    })).toBe(true)
    expect(() => bindLocalOutputFailureProtocolAttempt(scope.handle, {
      transferJobId: 'transfer-job',
      protocolSessionIdentity,
      protocolGeneration: 10,
    })).toThrow('different protocol attempt correlation')

    const claimed = attempt.sinks.attempt?.claim()
    expect(claimed?.scope).toEqual(scope.identity)
    expect(claimed?.protocolAttempt('transfer-job')).toMatchObject({
      protocolGeneration: 9,
    })

    attempt.revoke()
    expect(attempt.sinks.attempt?.claim()).toBeUndefined()
    expect(bindLocalOutputFailureProtocolAttempt(scope.handle, {
      transferJobId: 'successor-job',
      protocolSessionIdentity,
      protocolGeneration: 10,
    })).toBe(false)
  })

  it('allows only explicitly owned late cleanup consequences after incident sealing', () => {
    const observations: Array<Readonly<{
      fact: FailureFact
      relation: FailureFactRelation
    }>> = []
    const scope = createIncidentScopeIssuer().open('receive', {
      factRecorded: observation => observations.push({
        fact: observation.fact,
        relation: observation.relation,
      }),
    })
    const attempt = createAttemptOutputFailureCapability(scope.handle)
    const late = createLateOutputCleanupCapability(scope.handle)
    scope.close()
    attempt.revoke()
    expect(attempt.sinks.attempt?.claim()).toBeUndefined()
    expect(late.sinks.attempt?.claim()?.scope).toEqual(scope.identity)

    recordOutputException(
      late.sinks.cleanup,
      new DOMException('private detach detail', 'InvalidStateError'),
    )
    late.revoke()
    expect(late.sinks.attempt?.claim()).toBeUndefined()
    recordOutputException(
      late.sinks.cleanup,
      new DOMException('late unrelated cleanup', 'QuotaExceededError'),
    )

    expect(observations).toMatchObject([{
      fact: {
        kind: 'native_output_failure',
        stage: 'cleanup',
        payload: { nativeOutputFailure: { nativeClass: 'invalid_state' } },
      },
      relation: 'consequence',
    }])
  })

  it('does not construct trace payloads while disabled and isolates observers', () => {
    const createEvent = vi.fn(() =>
      outputTraceEvent('checkpoint', {
        backend: 'origin_private',
        transition: 'failed',
      }))
    const disabled: OutputTraceSource = Object.freeze({ current: undefined })

    emitOutputTrace(disabled, createEvent)

    expect(createEvent).not.toHaveBeenCalled()

    const events: OutputTraceEvent[] = []
    const enabled: OutputTraceSource = {
      get current() {
        return (event: OutputTraceEvent) => {
          events.push(event)
          throw new Error('diagnostic observer failed')
        }
      },
    }
    expect(() => emitOutputTrace(enabled, createEvent)).not.toThrow()
    expect(createEvent).toHaveBeenCalledOnce()
    expect(events).toEqual([
      {
        eventName: 'checkpoint',
        payload: {
          backend: 'origin_private',
          transition: 'failed',
        },
      },
    ])
  })

  it('projects checkpoint authority identities, decisions, costs, and releases into output trace', () => {
    const events: OutputTraceEvent[] = []
    const observe = checkpointAuthorityObserver({
      backend: 'file_system_access',
      trace: { current: event => events.push(event) },
    })
    expect(observe).toBeTypeOf('function')

    observe!({
      authority: 'preserving-capacity',
      receiveOperationId: 'receive-operation',
      transferJobId: 'transfer-job',
      outputSessionId: 'output-session',
      materializationRelativePath: ['folder', 'large.bin'],
      trigger: 'pending-bytes',
      checkpointOrdinal: 2,
      cost: Object.freeze({
        prefixCopyBytes: 134_217_728n,
        writeAmplificationBytes: 134_217_728n,
        temporaryBytes: 100_663_296n,
      }),
      remainingAutomaticWriteAmplificationBytes: 536_870_912n,
      decision: 'released',
      releaseReason: 'replacement-open-failed',
    })

    expect(events).toEqual([{
      eventName: 'checkpoint',
      payload: {
        backend: 'file_system_access',
        transition: 'authority_decision',
        authority: 'preserving_capacity',
        receive_operation_id: 'receive-operation',
        transfer_job_id: 'transfer-job',
        output_session_id: 'output-session',
        materialization_relative_path: ['folder', 'large.bin'],
        trigger: 'pending_bytes',
        checkpoint_ordinal: 2,
        prefix_copy_bytes: '134217728',
        write_amplification_bytes: '134217728',
        temporary_bytes: '100663296',
        remaining_automatic_write_amplification_bytes: '536870912',
        decision: 'released',
        release_reason: 'replacement_open_failed',
      },
    }])
  })
})
