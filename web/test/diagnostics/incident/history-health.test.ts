import { describe, expect, it } from 'vitest'

import {
  BoundedIncidentHistory,
} from '../../../src/diagnostics/incident/history'
import {
  IncidentDiagnosticsHealth,
} from '../../../src/diagnostics/incident/health'
import {
  createIncidentPolicy,
} from '../../../src/diagnostics/incident/policy'
import type { IncidentRecordV1 } from '../../../src/diagnostics/export/incident-record-v1'
import { deepFreezeJson } from '../../../src/diagnostics/export/json'

describe('bounded incident history', () => {
  it('evicts at limit plus one and returns detached immutable cuts', () => {
    const history = new BoundedIncidentHistory(createIncidentPolicy({
      maxIncidentHistoryRecords: 1,
    }))
    const first = record('1')
    const second = record('2')
    history.append(first)
    const oldCut = history.snapshot()

    expect(history.nextAppendEvictionCount()).toBe(1n)
    history.append(second)

    expect(history.last()).toBe(second)
    expect(history.snapshot()).toEqual([second])
    expect(oldCut).toEqual([first])
    expect(Object.isFrozen(oldCut)).toBe(true)
    history.clear()
    expect(history.last()).toBeNull()
  })

  it('rejects a mutable record instead of retaining caller-owned state', () => {
    const history = new BoundedIncidentHistory()
    expect(() => history.append({
      ...record('1'),
      payload: { ...record('1').payload },
    })).toThrow(/immutable/)
  })
})

describe('incident diagnostics health', () => {
  it('keeps exact cumulative counters and the last valid trace cut', () => {
    let fail = false
    const health = new IncidentDiagnosticsHealth({
      traceHealthSnapshot: () => {
        if (fail) throw new Error('trace unavailable')
        return {
          droppedCount: 5n,
          overwrittenCount: 6n,
          sampledCount: 7n,
          coalescedCount: 8n,
        }
      },
    })
    health.recordFactOverflow(2n)
    health.recordHistoryEviction()
    health.recordConsoleSuppression(3n)
    health.recordLateLinkEviction(4n)

    const first = health.incidentHealthSnapshot()
    fail = true
    const fallback = health.incidentHealthSnapshot()

    expect(first).toEqual({
      factOverflowCount: 2n,
      incidentHistoryEvictionCount: 1n,
      consoleSuppressionCount: 3n,
      lateLinkEvictionCount: 4n,
      traceDroppedCount: 5n,
      traceOverwrittenCount: 6n,
      traceSampledCount: 7n,
      traceCoalescedCount: 8n,
    })
    expect(fallback).toEqual(first)
    expect(Object.isFrozen(first)).toBe(true)
  })
})

function record(sequence: string): IncidentRecordV1 {
  return deepFreezeJson({
    schema_version: 1,
    sequence,
    time: '2026-08-19T01:02:03Z',
    elapsed_ms: sequence,
    level: 'error',
    event: 'failure_incident',
    runtime_run_id: 'AQAAAAAAAAAAAAAAAAAAAA',
    payload: {
      scope: { scope_kind: 'join', scope_sequence: sequence },
      presentation: { boundary: 'join', outcome: 'failed' },
      build: {
        application: 'windshare_web',
        version: '0.0.0',
        mode: 'test',
      },
      runtime: { kind: 'browser', secure_context: true },
      trigger: {
        kind: 'unclassified',
        stage: 'join',
        recovery_disposition: 'terminal',
        payload: { unclassified: {} },
      },
      contributors: [],
      consequences: [],
      fact_count: '1',
      overflow_fact_count: '0',
      context: {},
      diagnostics_health_at_seal: {
        fact_overflow_count: '0',
        incident_history_eviction_count: '0',
        console_suppression_count: '0',
        late_link_eviction_count: '0',
        trace_dropped_count: '0',
        trace_overwritten_count: '0',
        trace_sampled_count: '0',
        trace_coalesced_count: '0',
      },
    },
  })
}
