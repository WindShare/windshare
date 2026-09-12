import { describe, expect, it } from 'vitest'

import {
  CheckpointFaultCode,
  FaultScope,
  OutputFaultCode,
  outputFault,
} from '../../../src/transfer/fault'
import {
  FAILURE_FACT_KINDS,
  FAILURE_IDENTITY_KINDS,
  FAILURE_STAGES,
  NATIVE_FAILURE_CLASSES,
  PRESENTATION_BOUNDARIES,
  PRESENTATION_EXCLUSION_REASONS,
  PRESENTATION_OUTCOMES,
  PROTOCOL_ERROR_SCOPES,
  PROTOCOL_MESSAGE_KINDS_V1,
  PROTOCOL_REQUEST_KINDS_V1,
  RECOVERY_DISPOSITIONS,
  createFailureCorrelation,
  createFailureIdentity,
  createIncidentScopeIssuer,
  createReceivedProtocolError,
  excludedPresentationDecision,
  faultFailureFact,
  incidentPresentationDecision,
  isFailureCorrelation,
  isFailureFact,
  isPresentationDecision,
  isReceivedProtocolError,
  lifecycleFailureFact,
  nativeOutputFailureFact,
  presentationBoundaryForScope,
  unclassifiedFailureFact,
  type ReceivedProtocolError,
  type ReceivedProtocolErrorInput,
} from '../../../src/diagnostics/incident'

describe('incident frozen contracts', () => {
  it('publishes exact closed vocabularies', () => {
    expect(FAILURE_FACT_KINDS).toEqual([
      'fault',
      'protocol_failure',
      'peer_failure',
      'native_output_failure',
      'lifecycle_failure',
      'unclassified',
    ])
    expect(RECOVERY_DISPOSITIONS).toEqual([
      'none',
      'retryable',
      'resumable_receive',
      'resumable_package',
      'restart_required',
      'needs_attention',
      'terminal',
    ])
    expect(FAILURE_STAGES).toHaveLength(25)
    expect(NATIVE_FAILURE_CLASSES).toHaveLength(11)
    expect(PROTOCOL_MESSAGE_KINDS_V1).toEqual([
      'list_children',
      'catalog_result',
      'open_revisions',
      'open_results',
      'renew_lease',
      'release_lease',
      'request_blocks',
      'block_fragment',
      'cancel',
      'operation_error',
      'session_terminal',
      'lane_attach',
      'scan_progress',
      'operation_complete',
      'lease_result',
      'peer_offer',
      'peer_answer',
      'peer_candidate',
      'peer_path_control',
    ])
    expect(PROTOCOL_REQUEST_KINDS_V1).toEqual([
      'list_children',
      'open_revisions',
      'renew_lease',
      'release_lease',
      'request_blocks',
      'lane_attach',
      'peer_offer',
    ])
    expect(PROTOCOL_ERROR_SCOPES).toEqual([
      'directory',
      'revision',
      'block',
      'peer',
    ])
    expect(FAILURE_IDENTITY_KINDS).toEqual([
      'protocol_session',
      'protocol_operation',
      'peer_path',
      'peer_attempt',
    ])
    expect(PRESENTATION_BOUNDARIES).toHaveLength(8)
    expect(PRESENTATION_OUTCOMES).toHaveLength(6)
    expect(PRESENTATION_EXCLUSION_REASONS).toHaveLength(9)
    expect(presentationBoundaryForScope('preview_seek')).toBe('preview')
    expect(presentationBoundaryForScope('projection')).toBe(
      'projection_authority',
    )
    expect(presentationBoundaryForScope('authority_activation')).toBe(
      'projection_authority',
    )
    for (const values of [
      FAILURE_FACT_KINDS,
      RECOVERY_DISPOSITIONS,
      FAILURE_STAGES,
      NATIVE_FAILURE_CLASSES,
      PROTOCOL_MESSAGE_KINDS_V1,
      PROTOCOL_REQUEST_KINDS_V1,
      PROTOCOL_ERROR_SCOPES,
      FAILURE_IDENTITY_KINDS,
      PRESENTATION_BOUNDARIES,
      PRESENTATION_OUTCOMES,
      PRESENTATION_EXCLUSION_REASONS,
    ]) {
      expect(Object.isFrozen(values)).toBe(true)
    }
  })

  it('allows only the frozen boundary and outcome matrix with a real trigger ref', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const trigger = scope.facts.record(unclassifiedFailureFact({
      stage: 'content_read',
      recoveryDisposition: 'terminal',
    }), 'contributor')

    expect(incidentPresentationDecision(
      'receive',
      'partial_directory_failures',
      trigger,
    )).toEqual({
      kind: 'incident',
      boundary: 'receive',
      outcome: 'partial_directory_failures',
      trigger,
    })
    expect(incidentPresentationDecision(
      'lifecycle_action',
      'needs_attention',
      trigger,
    ).outcome).toBe('needs_attention')
    expect(() => incidentPresentationDecision(
      'browse',
      'restart_required',
      trigger,
    )).toThrow(RangeError)
    const consequence = scope.facts.record(unclassifiedFailureFact({
      stage: 'cleanup',
      recoveryDisposition: 'terminal',
    }), 'consequence')
    expect(() => incidentPresentationDecision(
      'receive',
      'failed',
      consequence,
    )).toThrow(/contributor/)
    expect(() => incidentPresentationDecision(
      'receive',
      'failed',
      {
        scope: trigger.scope,
        factSequence: trigger.factSequence,
      } as typeof trigger,
    )).toThrow(/reference/)

    const exclusion = excludedPresentationDecision('preview', 'picker_refused')
    expect(isPresentationDecision(exclusion)).toBe(true)
    expect(isPresentationDecision({ ...exclusion, message: 'private' })).toBe(false)
    scope.close()
  })

  it('snapshots fixed-width identities and enforces correlation pairing', () => {
    const source = identityBytes(1)
    const sessionId = createFailureIdentity('protocol_session', source)
    source.fill(0)
    const firstCopy = sessionId.copyBytes()
    firstCopy.fill(0)

    expect([...sessionId.copyBytes()]).toEqual([...identityBytes(1)])
    expect(sessionId.copyBytes()).not.toBe(sessionId.copyBytes())
    expect(() => createFailureIdentity(
      'peer_path',
      new Uint8Array(15),
    )).toThrow(/16 bytes/)
    expect(() => createFailureIdentity(
      'peer_path',
      new Uint8Array(16),
    )).toThrow(/non-zero/)

    const operationId = createFailureIdentity('protocol_operation', identityBytes(2))
    expect(() => createFailureCorrelation({
      protocolOperationId: operationId,
    })).toThrow(/invalid/)
    expect(() => createFailureCorrelation({
      peerAttemptId: createFailureIdentity('peer_attempt', identityBytes(3)),
    })).toThrow(/invalid/)
    expect(isFailureCorrelation({
      protocolSessionId: sessionId,
      protocolOperationId: operationId,
      lane: { id: 0, epoch: 0 },
    })).toBe(true)
    expect(isFailureCorrelation({})).toBe(false)
    expect(isFailureCorrelation({
      protocolSessionId: sessionId,
      lane: { id: -1, epoch: 0 },
    })).toBe(false)
  })

  it('accepts exactly the frozen initiating request kinds', () => {
    const valid = protocolFailure()
    const requestKinds = new Set<string>(PROTOCOL_REQUEST_KINDS_V1)

    for (const requestKind of PROTOCOL_MESSAGE_KINDS_V1) {
      const candidate = { ...valid, requestKind }
      const expected = requestKinds.has(requestKind)
      expect(isReceivedProtocolError(candidate), requestKind).toBe(expected)
      if (expected) {
        expect(constructReceivedProtocolError(candidate), requestKind).toMatchObject({
          requestKind,
        })
      } else {
        expect(
          constructReceivedProtocolError.bind(undefined, candidate),
          requestKind,
        ).toThrow(/invalid/)
      }
    }
  })

  it('requires a bounded peer retry hint exactly when retryable', () => {
    const valid = protocolFailure()
    const retryAfterCases = [
      { name: 'absent', present: false },
      { name: 'explicit-undefined', present: true, value: undefined },
      { name: 'non-number', present: true, value: '1' },
      { name: 'zero', present: true, value: 0 },
      { name: 'minimum', present: true, value: 1 },
      { name: 'fractional', present: true, value: 1.5 },
      { name: 'maximum', present: true, value: 30_000 },
      { name: 'above-maximum', present: true, value: 30_001 },
    ] as const
    for (const retryable of [false, true]) {
      for (const retryAfter of retryAfterCases) {
        const content = {
          scope: valid.content.scope,
          code: valid.content.code,
          retryable,
          ...(retryAfter.present ? { retryAfterMilliseconds: retryAfter.value } : {}),
        }
        const candidate = { ...valid, content }
        const expected = retryable
          ? retryAfter.present && typeof retryAfter.value === 'number' &&
            Number.isInteger(retryAfter.value) && retryAfter.value >= 1 && retryAfter.value <= 30_000
          : !retryAfter.present
        expect(isReceivedProtocolError(candidate), retryAfter.name).toBe(expected)
        if (expected) expect(Object.isFrozen(constructReceivedProtocolError(candidate).content)).toBe(true)
        else expect(() => constructReceivedProtocolError(candidate)).toThrow(/invalid/)
      }
    }
  })

  it('snapshots received content and rejects sender settlement or remote text', () => {
    const valid = protocolFailure()
    const content = { ...valid.content }
    const lane = { id: 4, epoch: 0 }
    const received = createReceivedProtocolError({
      ...valid,
      content,
      correlation: { ...valid.correlation, lane },
    })
    lane.id = 9
    expect(received.correlation.lane).toEqual({ id: 4, epoch: 0 })
    content.code = 1
    expect(received.content.code).toBe(0xffff)
    expect(received.content).not.toBe(content)
    expect(Object.isFrozen(received.content)).toBe(true)
    expect(Object.isFrozen(received.correlation)).toBe(true)
    expect(isReceivedProtocolError(received)).toBe(true)
    expect(() => createReceivedProtocolError({
      ...valid, content: {
        ...valid.content, code: 65536
      }
    })).toThrow(/invalid/)
    expect(isReceivedProtocolError({ ...valid, settlement: { kind: 'received_authenticated' } })).toBe(false)
    expect(isReceivedProtocolError({ ...valid, settlement: { kind: 'response_send', admitted: true, settled: false, outcome: 'unknown' } })).toBe(false)
    expect(isReceivedProtocolError({ ...valid, remoteMessage: 'secret' })).toBe(false)
    expect(isReceivedProtocolError({ ...valid, content: { ...valid.content, requestKind: valid.requestKind } })).toBe(false)
  })

  it('keeps native and lifecycle payloads closed to applicable codes and reasons', () => {
    expect(isFailureFact(faultFailureFact({
      stage: 'output_write',
      recoveryDisposition: 'retryable',
      fault: outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
    }))).toBe(true)
    expect(() => faultFailureFact({
      stage: 'output_write',
      recoveryDisposition: 'terminal',
      fault: Object.freeze({
        ...outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
        message: 'private path',
      }),
    })).toThrow(/normalized fault/)

    expect(isFailureFact(nativeOutputFailureFact({
      stage: 'checkpoint',
      recoveryDisposition: 'retryable',
      nativeClass: 'data',
      code: CheckpointFaultCode.CorruptRecord,
    }))).toBe(true)
    expect(() => nativeOutputFailureFact({
      stage: 'output_write',
      recoveryDisposition: 'retryable',
      nativeClass: 'data',
      code: CheckpointFaultCode.CorruptRecord,
    })).toThrow(/stage/)
    expect(isFailureFact(nativeOutputFailureFact({
      stage: 'output_write',
      recoveryDisposition: 'retryable',
      nativeClass: 'quota_exceeded',
      code: OutputFaultCode.ResourceBudget,
    }))).toBe(true)

    expect(isFailureFact(lifecycleFailureFact({
      stage: 'lifecycle_action',
      recoveryDisposition: 'restart_required',
      kind: 'restart-required',
      reason: 'content-session-ended',
    }))).toBe(true)
    expect(isFailureFact(lifecycleFailureFact({
      stage: 'reopen',
      recoveryDisposition: 'retryable',
      kind: 'authorization-required',
    }))).toBe(true)
    expect(isFailureFact(lifecycleFailureFact({
      stage: 'reopen',
      recoveryDisposition: 'restart_required',
      kind: 'restart-required',
      reason: 'target-deleted',
    }))).toBe(true)
    expect(() => lifecycleFailureFact({
      stage: 'lifecycle_action',
      recoveryDisposition: 'terminal',
      kind: 'published',
      reason: 'content-session-ended',
    })).toThrow(/invalid/)
  })
})

function constructReceivedProtocolError(value: unknown): ReceivedProtocolError {
  return createReceivedProtocolError(value as ReceivedProtocolErrorInput)
}

function protocolFailure(): ReceivedProtocolError {
  return { requestKind: 'request_blocks', correlation: {
      protocolSessionId: createFailureIdentity(
        'protocol_session',
        identityBytes(4),
      ),
      protocolOperationId: createFailureIdentity(
        'protocol_operation',
        identityBytes(5),
      ),
      lane: { id: 0xffff_ffff, epoch: 0 },
    }, content: { scope: 'block', code: 0xffff, retryable: true, retryAfterMilliseconds: 30_000 } }
}

function identityBytes(seed: number): Uint8Array {
  const value = new Uint8Array(16)
  value[0] = seed
  value[15] = seed + 1
  return value
}
