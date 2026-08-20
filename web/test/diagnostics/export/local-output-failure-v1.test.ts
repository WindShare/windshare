import { describe, expect, it } from 'vitest'

import {
  createDiagnosticBundleV1,
  projectDiagnosticsStatusV1,
} from '../../../src/diagnostics/export/diagnostic-bundle-v1'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'
import { encodeDiagnosticBundleNdjson } from '../../../src/diagnostics/export/ndjson'
import type { IncidentRecordV1 } from '../../../src/diagnostics/export/incident-record-v1'
import { createFailureIdentity } from '../../../src/diagnostics/incident'
import { localOutputOperationFailureRecord } from '../../../src/output/diagnostics'
import type {
  PersistentOutputStageFailureMilestone,
} from '../../../src/output/persistent-tree/stage-diagnostics'
import { projectProtocolTraceEvent } from '../../../src/ui/v2-production-trace'
import {
  TEST_BUNDLE_IDENTITY,
  diagnosticsHealthV1,
  traceStatus,
} from './test-support'

const PRIVATE_ARTIFACT_PATH = Object.freeze(['C:', 'Users', 'private', 'secret.bin'])

describe('local output failure bundle projection', () => {
  it('joins two detailed failures to exactly one incident each in deterministic order', () => {
    const firstProtocol = createFailureIdentity(
      'protocol_session',
      new Uint8Array(16).fill(2),
    )
    const secondProtocol = createFailureIdentity(
      'protocol_session',
      new Uint8Array(16).fill(3),
    )
    const first = requiredRecord(localOutputOperationFailureRecord(
      failedMilestone({
        sequence: 8,
        operationId: 'receive-operation-b',
        outputSessionId: 'output-session-b',
        artifactPath: ['folder-b', 'file.bin'],
      }),
      {
        owningScope: { scopeKind: 'receive', scopeSequence: 12n },
        correlation: {
          receiveOperationId: 'receive-operation-b',
          transferJobId: 'transfer-job-b',
          outputSessionId: 'output-session-b',
          protocolSessionIdentity: secondProtocol,
          protocolGeneration: 7,
        },
      },
    ))
    const second = requiredRecord(localOutputOperationFailureRecord(
      failedMilestone({
        sequence: 4,
        operationId: 'receive-operation-a',
        outputSessionId: 'output-session-a',
        artifactPath: PRIVATE_ARTIFACT_PATH,
      }),
      {
        owningScope: { scopeKind: 'receive', scopeSequence: 11n },
        correlation: {
          receiveOperationId: 'receive-operation-a',
          transferJobId: 'transfer-job-a',
          outputSessionId: 'output-session-a',
          protocolSessionIdentity: firstProtocol,
          protocolGeneration: 2,
        },
      },
    ))
    const orphan = requiredRecord(localOutputOperationFailureRecord(
      failedMilestone({
        sequence: 1,
        operationId: 'orphan-operation',
        outputSessionId: 'orphan-output',
        artifactPath: ['orphan'],
      }),
      {
        owningScope: { scopeKind: 'receive', scopeSequence: 99n },
        correlation: {
          receiveOperationId: 'orphan-operation',
          transferJobId: 'orphan-job',
          outputSessionId: 'orphan-output',
        },
      },
    ))
    const health = diagnosticsHealthV1()
    const bundle = createDiagnosticBundleV1({
      identity: TEST_BUNDLE_IDENTITY,
      time: '2026-08-20T00:00:00Z',
      incidents: [nativeOutputIncident('9', '12'), nativeOutputIncident('3', '11')],
      localOutputFailures: [first.projection, orphan.projection, second.projection],
      status: projectDiagnosticsStatusV1(traceStatus(), health),
      healthAtExport: health,
    })
    const encoded = encodeDiagnosticBundleNdjson(bundle)

    expect(bundle.localOutputFailures.map((line) => ({
      incident: line.owning_incident.incident_sequence,
      operation: line.correlation.receive_operation_id,
      transfer: line.correlation.transfer_job_id,
      output: line.correlation.output_session_id,
      protocol: line.correlation.protocol_session_id,
      generation: line.correlation.protocol_generation,
    }))).toEqual([
      {
        incident: '3',
        operation: 'receive-operation-a',
        transfer: 'transfer-job-a',
        output: 'output-session-a',
        protocol: 'AgICAgICAgICAgICAgICAg',
        generation: 2,
      },
      {
        incident: '9',
        operation: 'receive-operation-b',
        transfer: 'transfer-job-b',
        output: 'output-session-b',
        protocol: 'AwMDAwMDAwMDAwMDAwMDAw',
        generation: 7,
      },
    ])
    for (const line of bundle.localOutputFailures) {
      expect(bundle.incidents.filter((incident) =>
        incident.record.sequence === line.owning_incident.incident_sequence &&
        incident.record.payload.scope.scope_kind === line.owning_incident.scope.scope_kind &&
        incident.record.payload.scope.scope_sequence === line.owning_incident.scope.scope_sequence))
        .toHaveLength(1)
    }
    expect(encoded).toContain('"message":"checkpoint transaction aborted"')
    expect(encoded).not.toMatch(/destination|decision|proof|omission|fallback|"raw"/u)
  })

  it('keeps diagnostic-only output identities and artifact paths out of protocol traces', () => {
    const protocolSessionId = createFailureIdentity(
      'protocol_session',
      new Uint8Array(16).fill(5),
    )
    const protocolOperationId = createFailureIdentity(
      'protocol_operation',
      new Uint8Array(16).fill(6),
    )
    const trace = projectProtocolTraceEvent({
      eventName: 'protocol_operation',
      transition: 'request_sent',
      requestKind: 'request_blocks',
      correlation: { protocolSessionId, protocolOperationId },
    })
    const encodedTrace = JSON.stringify(trace)

    expect(trace.correlation).toEqual({
      protocol_session_id: 'BQUFBQUFBQUFBQUFBQUFBQ',
      protocol_operation_id: 'BgYGBgYGBgYGBgYGBgYGBg',
    })
    expect(encodedTrace).not.toContain(PRIVATE_ARTIFACT_PATH.join('/'))
    expect(encodedTrace).not.toMatch(/receive_operation|transfer_job|output_session|artifact|private/u)
  })
})

function requiredRecord<T>(record: T | undefined): T {
  if (record === undefined) throw new Error('failed milestone did not project')
  return record
}

function nativeOutputIncident(sequence: string, scopeSequence: string): IncidentRecordV1 {
  return deepFreezeJson({
    schema_version: 1,
    sequence,
    time: '2026-08-20T00:00:00.000Z',
    elapsed_ms: sequence,
    level: 'error',
    event: 'failure_incident',
    runtime_run_id: TEST_BUNDLE_IDENTITY.runtimeRunId,
    payload: {
      scope: { scope_kind: 'receive', scope_sequence: scopeSequence },
      presentation: { boundary: 'receive', outcome: 'failed' },
      build: TEST_BUNDLE_IDENTITY.build,
      runtime: TEST_BUNDLE_IDENTITY.runtime,
      trigger: {
        kind: 'native_output_failure',
        stage: 'output_write',
        recovery_disposition: 'none',
        payload: {
          native_output_failure: {
            native_class: 'unknown',
            code: 'state_io',
          },
        },
      },
      contributors: [],
      consequences: [],
      fact_count: '1',
      overflow_fact_count: '0',
      context: {},
      diagnostics_health_at_seal: diagnosticsHealthV1(),
    },
  })
}

function failedMilestone(input: Readonly<{
  sequence: number
  operationId: string
  outputSessionId: string
  artifactPath: readonly string[]
}>): PersistentOutputStageFailureMilestone {
  const error = new DOMException('checkpoint transaction aborted', 'UnknownError')
  return Object.freeze({
    sequence: input.sequence,
    transition: 'failed',
    stage: 'indexeddb.checkpoint.commit',
    correlation: Object.freeze({
      operationId: input.operationId,
      outputSessionId: input.outputSessionId,
      target: 'file',
      artifactId: 'source-file',
      artifactPath: Object.freeze([...input.artifactPath]),
      checkpointRecordId: 'checkpoint-record',
      checkpointGeneration: 3n,
    }),
    exception: Object.freeze({
      raw: error,
      valueType: 'object',
      constructorName: 'DOMException',
      name: 'UnknownError',
      message: error.message,
      ...(error.stack === undefined ? {} : { stack: error.stack }),
    }),
    facts: Object.freeze({
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
    }),
  })
}
