import { describe, expect, it } from 'vitest'

import {
  FaultScope,
  OutputFaultCode,
  outputFault,
} from '../../../src/transfer/fault'
import {
  createFailureFactAccumulator,
  createFailureIdentity,
  createIncidentScopeIssuer,
  createProtocolFailure,
  faultFailureFact,
  lifecycleFailureFact,
  nativeOutputFailureFact,
  peerFailureFact,
  protocolFailureFact,
  unclassifiedFailureFact,
  type FailureFact,
  type SealedFailureFacts,
} from '../../../src/diagnostics/incident'
import {
  createIncidentPolicy,
  type IncidentPolicy,
} from '../../../src/diagnostics/incident/policy'
import {
  captureDiagnosticContextV1,
} from '../../../src/diagnostics/export/context'
import {
  createIncidentRecordProjector,
  createRuntimeRunIdentity,
  type IncidentRecordProjectionInput,
} from '../../../src/diagnostics/export/projector'

describe('IncidentRecordV1 projection', () => {
  it('emits the exact envelope/payload order and correlation-safe immutable DTO', () => {
    const sourceRunId = identityBytes(1)
    const projector = createIncidentRecordProjector({
      runtimeRunId: createRuntimeRunIdentity(sourceRunId),
      build: {
        version: '0.0.0',
        revision: 'abcdef0',
        mode: 'test',
      },
      secureContext: true,
    })
    sourceRunId.fill(0)
    const facts = sealFacts([
      peerFailureFact({
        stage: 'peer_attempt',
        recoveryDisposition: 'retryable',
        scope: 'attempt',
        code: 'peer-timeout',
        retryable: true,
        correlation: {
          peerPathId: createFailureIdentity('peer_path', identityBytes(2)),
          peerAttemptId: createFailureIdentity(
            'peer_attempt',
            identityBytes(3),
          ),
          lane: { id: 1, epoch: 0 },
        },
      }),
    ])

    const { record, overflowFactCount } = projector.project(input(facts))

    expect(Object.keys(record)).toEqual([
      'schema_version',
      'sequence',
      'time',
      'elapsed_ms',
      'level',
      'event',
      'runtime_run_id',
      'correlation',
      'payload',
    ])
    expect(Object.keys(record.payload)).toEqual([
      'scope',
      'presentation',
      'build',
      'runtime',
      'trigger',
      'contributors',
      'consequences',
      'fact_count',
      'overflow_fact_count',
      'context',
      'diagnostics_health_at_seal',
    ])
    expect(record).toMatchObject({
      schema_version: 1,
      sequence: '1',
      time: '2026-08-19T01:02:03.456Z',
      elapsed_ms: '9',
      level: 'error',
      event: 'failure_incident',
      runtime_run_id: 'AQAAAAAAAAAAAAAAAAAAAA',
      correlation: {
        peer_path_id: 'AgAAAAAAAAAAAAAAAAAAAA',
        peer_attempt_id: 'AwAAAAAAAAAAAAAAAAAAAA',
        lane_id: 1,
        lane_epoch: 0,
      },
      payload: {
        presentation: { boundary: 'receive', outcome: 'failed' },
        build: {
          application: 'windshare_web',
          version: '0.0.0',
          revision: 'abcdef0',
          mode: 'test',
        },
        runtime: { kind: 'browser', secure_context: true },
        trigger: {
          kind: 'peer_failure',
          stage: 'peer_attempt',
          recovery_disposition: 'retryable',
          payload: {
            peer_failure: {
              scope: 'attempt',
              code: 'peer_timeout',
              retryable: true,
            },
          },
        },
        fact_count: '1',
        overflow_fact_count: '0',
      },
    })
    expect(overflowFactCount).toBe(0n)
    expect(Object.isFrozen(record)).toBe(true)
    expect(Object.isFrozen(record.payload.trigger.payload)).toBe(true)
    expect(Object.isFrozen(record.payload.contributors)).toBe(true)
    expect(JSON.stringify(record)).not.toContain('Uint8Array')
  })

  it('projects authenticated protocol failure without remote text', () => {
    const failure = createProtocolFailure({
      requestKind: 'request_blocks',
      wireScope: 'block',
      wireCode: 9,
      retryable: true,
      retryAfterMilliseconds: 300,
      settlement: { kind: 'received_authenticated' },
      correlation: {
        protocolSessionId: createFailureIdentity(
          'protocol_session',
          identityBytes(4),
        ),
        protocolOperationId: createFailureIdentity(
          'protocol_operation',
          identityBytes(5),
        ),
      },
    })
    const facts = sealFacts([
      protocolFailureFact({
        stage: 'protocol_operation',
        recoveryDisposition: 'retryable',
        protocolFailure: failure,
      }),
    ])

    const record = projector().project(input(facts)).record
    expect(record.payload.trigger.payload).toEqual({
      protocol_failure: {
        request_kind: 'request_blocks',
        wire_scope: 'block',
        wire_code: 9,
        retryable: true,
        retry_after_ms: 300,
        settlement: { kind: 'received_authenticated' },
        correlation: {
          protocol_session_id: 'BAAAAAAAAAAAAAAAAAAAAA',
          protocol_operation_id: 'BQAAAAAAAAAAAAAAAAAAAA',
        },
      },
    })
    expect(JSON.stringify(record)).not.toMatch(/message|stack|cause|body/)
  })

  it('projects existing hyphenated fault/output/lifecycle values to snake case', () => {
    const facts = sealFacts([
      faultFailureFact({
        stage: 'output_write',
        recoveryDisposition: 'retryable',
        fault: outputFault(
          FaultScope.OutputPause,
          OutputFaultCode.StateIO,
        ),
      }),
      nativeOutputFailureFact({
        stage: 'output_write',
        recoveryDisposition: 'terminal',
        nativeClass: 'not_allowed',
        code: OutputFaultCode.UnsupportedFilesystem,
      }),
      lifecycleFailureFact({
        stage: 'lifecycle_action',
        recoveryDisposition: 'restart_required',
        kind: 'restart-required',
        reason: 'target-deleted',
      }),
    ])

    const record = projector().project(input(facts)).record
    expect(record.payload.trigger.payload).toEqual({
      fault: {
        domain: 'output',
        scope: 'output_pause',
        code: 'state_io',
      },
    })
    const representatives = record.payload.contributors.map(
      (bucket) => bucket.representative,
    )
    expect(representatives.find(
      (factValue) => factValue.kind === 'native_output_failure',
    )?.payload).toEqual({
      native_output_failure: {
        native_class: 'not_allowed',
        code: 'unsupported_filesystem',
      },
    })
    expect(representatives.find(
      (factValue) => factValue.kind === 'lifecycle_failure',
    )?.payload).toEqual({
      lifecycle_failure: {
        state: 'restart_required',
        reason: 'target_deleted',
      },
    })
  })

  it('prunes list and byte overflow from the deterministic end with exact counts', () => {
    const facts = sealFacts([
      fact('content_read'),
      fact('output_write'),
      fact('cleanup'),
    ])
    const listLimited = projector(createIncidentPolicy({
      maxRecordListItems: 1,
    })).project(input(facts))
    expect(listLimited.record.payload.contributors).toHaveLength(1)
    expect(listLimited.overflowFactCount).toBe(1n)
    expect(listLimited.record.payload.overflow_fact_count).toBe('1')

    const full = projector().project(input(facts))
    const fullBytes = new TextEncoder().encode(
      JSON.stringify(full.record),
    ).byteLength
    const byteLimited = projector(createIncidentPolicy({
      maxIncidentRecordBytes: fullBytes - 1,
    })).project(input(facts))
    expect(byteLimited.record.payload.contributors.length).toBeLessThan(
      full.record.payload.contributors.length,
    )
    expect(byteLimited.overflowFactCount).toBeGreaterThan(0n)
    expect(new TextEncoder().encode(
      JSON.stringify(byteLimited.record),
    ).byteLength).toBeLessThanOrEqual(fullBytes - 1)
  })

  it('omits invalid build revisions and rejects invalid run/time identities', () => {
    const invalidRevision = createIncidentRecordProjector({
      runtimeRunId: createRuntimeRunIdentity(identityBytes(8)),
      build: {
        version: '1.2.3',
        revision: 'PRIVATE',
        mode: 'production',
      },
      secureContext: false,
    })
    const record = invalidRevision.project(input(sealFacts([
      fact('join'),
    ]))).record
    expect(record.payload.build).toEqual({
      application: 'windshare_web',
      version: '1.2.3',
      mode: 'production',
    })
    expect(() => createRuntimeRunIdentity(new Uint8Array(16))).toThrow(
      /non-zero/,
    )
    expect(() => invalidRevision.project({
      ...input(sealFacts([fact('join')])),
      time: 'local time',
    })).toThrow(/RFC3339/)
  })
})

function projector(policy?: IncidentPolicy) {
  return createIncidentRecordProjector({
    runtimeRunId: createRuntimeRunIdentity(identityBytes(9)),
    build: { version: '0.0.0', mode: 'test' },
    secureContext: true,
    ...(policy === undefined ? {} : { policy }),
  })
}

function input(facts: SealedFailureFacts): IncidentRecordProjectionInput {
  return {
    sequence: 1n,
    time: '2026-08-19T01:02:03.456Z',
    elapsedMs: 9n,
    presentation: { boundary: 'receive', outcome: 'failed' },
    facts,
    context: captureDiagnosticContextV1(),
    health: {
      factOverflowCount: 0n,
      incidentHistoryEvictionCount: 0n,
      consoleSuppressionCount: 0n,
      lateLinkEvictionCount: 0n,
      traceDroppedCount: 0n,
      traceOverwrittenCount: 0n,
      traceSampledCount: 0n,
      traceCoalescedCount: 0n,
    },
  }
}

function sealFacts(facts: readonly FailureFact[]): SealedFailureFacts {
  const scope = createIncidentScopeIssuer().open('receive')
  const accumulator = createFailureFactAccumulator(scope.identity)
  const refs = facts.map((value) => {
    const ref = scope.facts.record(value, 'contributor')
    accumulator.record(ref)
    return ref
  })
  const sealed = accumulator.seal(refs[0]!)
  scope.close()
  return sealed
}

function fact(stage: 'join' | 'content_read' | 'output_write' | 'cleanup') {
  return unclassifiedFailureFact({
    stage,
    recoveryDisposition: 'terminal',
  })
}

function identityBytes(seed: number): Uint8Array {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}
