import { describe, expect, it } from 'vitest'
import { snapshotTraceEventObservationV1 } from '../../src/diagnostics/export/trace-event-v1'
import type { TraceEventObservationV1 } from '../../src/diagnostics/trace/model'
import type { OutputTraceEvent, OutputTraceSource } from '../../src/output/diagnostics'
import {
  traceDirectZipMemberRollback,
  type DirectZipMemberRollbackTraceInput,
} from '../../src/ui/browser-receive/direct-zip/member-rollback-trace'
import { createOutputTraceSource } from '../../src/ui/v2-production-trace'

const INPUT: DirectZipMemberRollbackTraceInput = {
  operationId: 'AQAAAAAAAAAAAAAAAAAAAA',
  sessionId: 'AgAAAAAAAAAAAAAAAAAAAA',
  candidateId: 'AwAAAAAAAAAAAAAAAAAAAA',
  phase: 'requested',
  oldCommittedLength: 9_007_199_254_741_000n,
  newCommittedLength: 9_007_199_254_740_000n,
  retainedSelectedPayloadBytes: 9_007_199_254_739_000n,
  memberOrdinal: 3n,
  sourceChangeReason: 'revision-changed',
}

describe('production direct ZIP member rollback tracing', () => {
  it('preserves rollback correlation and exact prefix lengths through the production trace adapter', () => {
    const events: TraceEventObservationV1[] = []
    const source = createOutputTraceSource({ current: event => events.push(event) })
    for (const phase of ['requested', 'persisted', 'recovering', 'completed', 'failed'] as const) {
      traceDirectZipMemberRollback(source, {
        ...INPUT, phase,
        ...(phase === 'failed' ? { error: new DOMException('target close failed', 'DataError') } : {}),
      })
    }
    expect(events.map(event => event.payload)).toEqual(
      ['requested', 'persisted', 'recovering', 'completed', 'failed'].map(phase => ({
        operation_id: INPUT.operationId,
        session_id: INPUT.sessionId,
        candidate_id: INPUT.candidateId,
        phase,
        old_committed_length: '9007199254741000',
        new_committed_length: '9007199254740000',
        retained_selected_payload_bytes: '9007199254739000',
        member_ordinal: '3',
        source_change_reason: 'revision_changed',
        ...(phase === 'failed' ? { native_error_name: 'DataError' } : {}),
      })),
    )
    for (const event of events) {
      expect(event.eventName).toBe('direct_zip_member_rollback')
      expect(() => snapshotTraceEventObservationV1(event)).not.toThrow()
    }
  })

  it('records candidate recovery without inventing a source-change reason', () => {
    const events: TraceEventObservationV1[] = []
    const source = createOutputTraceSource({ current: event => events.push(event) })
    const recovery = { ...INPUT }
    delete recovery.sourceChangeReason
    traceDirectZipMemberRollback(source, { ...recovery, phase: 'recovering' })
    expect(events).toHaveLength(1)
    expect(events[0]!.payload).not.toHaveProperty('source_change_reason')
    expect(() => snapshotTraceEventObservationV1(events[0]!)).not.toThrow()
  })

  it('keeps disabled or failing trace observers outside rollback authority', () => {
    const inaccessible = {
      ...INPUT,
      get oldCommittedLength(): bigint { throw new Error('disabled trace read rollback evidence') },
    }
    const source: { current: OutputTraceSource['current'] } = { current: undefined }
    expect(() => traceDirectZipMemberRollback(undefined, inaccessible)).not.toThrow()
    expect(() => traceDirectZipMemberRollback(source, inaccessible)).not.toThrow()
    source.current = () => { throw new Error('observer failed') }
    expect(() => traceDirectZipMemberRollback(source, INPUT)).not.toThrow()
    const events: OutputTraceEvent[] = []
    source.current = event => events.push(event)
    traceDirectZipMemberRollback(source, { ...INPUT, phase: 'completed' })
    expect(events).toHaveLength(1)
  })
})
