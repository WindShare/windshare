import { describe, expect, it } from 'vitest'
import { RemoteOperationError } from '../../src/content/session-services'
import {
  createFailureIdentity,
  createIncidentScopeIssuer,
  createReceivedProtocolError,
  type IncidentScopeHandle,
  type IncidentScopeObserver,
} from '../../src/diagnostics/incident'
import {
  FaultScope,
  OutputFaultCode,
  SourceFaultCode,
  dependencyContractFault,
  outputFault,
  sourceFault,
} from '../../src/transfer/failure/fault'
import { TransferAdmissionFailureError } from '../../src/transfer/job/admission-error'
import {
  FileOutputError,
  normalizeFileTransferFailure,
} from '../../src/transfer/job/failures'

describe('transfer failure classification', () => {
  it('produces product authority, one immutable fact/ref, and materialization semantics together', () => {
    const recorded: Parameters<
      NonNullable<IncidentScopeObserver['factRecorded']>
    >[0][] = []
    const scope = incidentScope(observation => { recorded.push(observation) })

    const normalized = normalizeFileTransferFailure(
      new FileOutputError('native detail is retained', 'output-commit-failed'),
      { incidentScope: scope },
    )
    expect(normalized.kind).toBe('fault')
    if (normalized.kind !== 'fault') return

    expect(normalized.fault).toEqual(
      outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
    )
    expect(normalized.fact).toMatchObject({
      kind: 'fault',
      stage: 'output_commit',
      recoveryDisposition: 'resumable_receive',
      payload: { fault: normalized.fault },
    })
    expect(normalized.factRef).toBeDefined()
    expect(normalized.materializationFailureReason).toBe('output-commit-failed')
    expect(Object.isFrozen(normalized.fact)).toBe(true)
    expect(Object.isFrozen(normalized.diagnostic.classification)).toBe(true)
    expect(normalized.fact.evidence?.exception.message).toBe('native detail is retained')

    const replay = normalizeFileTransferFailure(normalized.diagnostic, {
      incidentScope: scope,
    })
    expect(replay.kind).toBe('fault')
    expect(recorded).toHaveLength(1)
    if (replay.kind === 'fault') expect(replay.factRef).toBe(normalized.factRef)
  })

  it('carries reviewed authority and immutable exception evidence across transfer admission', () => {
    const normalized = normalizeFileTransferFailure(new Error('private admission detail'), {
      stage: 'authority_selection', operation: 'output.select', context: { destination: 'tree' },
    })
    expect(normalized.kind).toBe('fault')
    if (normalized.kind !== 'fault') return

    const error = new TransferAdmissionFailureError(Object.freeze({
      kind: 'fault',
      classification: normalized.diagnostic.classification,
    }))
    expect(error.authority).toMatchObject({
      kind: 'fault',
      classification: normalized.diagnostic.classification,
    })
    expect(error.cause).toBeUndefined()
    expect(normalized.fault).toEqual(dependencyContractFault())
    expect(normalized.fact).toMatchObject({
      kind: 'unclassified', stage: 'authority_selection',
      evidence: { operation: 'output.select', context: { destination: 'tree' },
        exception: { errorName: 'Error', message: 'private admission detail' } },
    })
  })

  it('reuses the authenticated protocol fact while producing product fault authority', () => {
    const remote = new RemoteOperationError(createReceivedProtocolError({
      requestKind: 'request_blocks', correlation: {
        protocolSessionId: createFailureIdentity('protocol_session', identityBytes(1)),
        protocolOperationId: createFailureIdentity('protocol_operation', identityBytes(2))
      }, content: {
        scope: 'block', code: 0xffff, retryable: true, retryAfterMilliseconds: 250
      }
    }))

    const normalized = normalizeFileTransferFailure(remote)
    expect(normalized.kind).toBe('fault')
    if (normalized.kind !== 'fault') return
    expect(normalized.fault).toEqual(
      sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
    )
    expect(normalized.fact).toBe(remote.failureFact)
    expect(normalized.fact.kind).toBe('protocol_failure')
    expect(normalized.fact.stage).toBe('protocol_operation')
  })

  it('does not traverse nested causes to recover product or materialization policy', () => {
    const nested = new FileOutputError('hidden nested output detail', 'output-commit-failed')
    const outer = new Error('unclassified boundary', { cause: nested })

    const normalized = normalizeFileTransferFailure(outer)
    expect(normalized.kind).toBe('fault')
    if (normalized.kind !== 'fault') return
    expect(normalized.fault).toEqual(dependencyContractFault())
    expect(normalized.fact.stage).toBe('content_read')
    expect(normalized.materializationFailureReason).toBe('content-read-failed')
    expect(normalized.diagnostic.cause).toBeUndefined()
  })

  it('isolates a throwing fact sink from the product classification', () => {
    const scope: IncidentScopeHandle = Object.freeze({
      identity: Object.freeze({ scopeKind: 'receive', scopeSequence: 1n }),
      facts: Object.freeze({
        record: () => {
          throw new Error('diagnostics unavailable')
        },
      }),
    })
    const normalized = normalizeFileTransferFailure(
      new FileOutputError('write failed', 'output-write-failed'),
      { incidentScope: scope },
    )

    expect(normalized.kind).toBe('fault')
    if (normalized.kind !== 'fault') return
    expect(normalized.fault).toEqual(
      outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
    )
    expect(normalized.factRef).toBeUndefined()
  })

  it('records no failure fact for an operation-owned cancellation', () => {
    let factCount = 0
    const scope = incidentScope(() => { factCount += 1 })
    const controller = new AbortController()
    controller.abort(new DOMException('cancelled', 'AbortError'))

    const normalized = normalizeFileTransferFailure(controller.signal.reason, {
      signal: controller.signal,
      incidentScope: scope,
    })

    expect(normalized.kind).toBe('canceled')
    expect(factCount).toBe(0)
  })
})

function identityBytes(seed: number): Uint8Array {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  bytes[15] = seed + 1
  return bytes
}

function incidentScope(
  factRecorded: NonNullable<IncidentScopeObserver['factRecorded']>,
): IncidentScopeHandle {
  return createIncidentScopeIssuer({
    clock: { elapsedMilliseconds: () => 1 },
    scheduler: {
      schedule: () => Object.freeze({ cancel: () => undefined }),
    },
  }).open('receive', { factRecorded }).handle
}
