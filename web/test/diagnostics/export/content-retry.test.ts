import { describe, expect, it } from 'vitest'
import { projectProtocolTraceEvent } from '../../../src/application/diagnostics/protocol-trace'
import { snapshotTraceEventObservationV2 } from '../../../src/diagnostics/export/trace-event-v2'
import { traceContentRetry, traceContentScheduling } from '../../../src/diagnostics/trace/content-scheduling'
import { traceEventRetention } from '../../../src/diagnostics/trace/retention'
import type { TraceEventObservationV2 } from '../../../src/diagnostics/trace/model'
import { createProtocolSessionIdentity } from '../../../src/session/identities'

const sessionId = createProtocolSessionIdentity(new Uint8Array(16).fill(1))
const context = {
  firstDispatchSequence: 7, fileId: 'AQAAAAAAAAAAAAAAAAAAAA', localBlockIndex: 27n,
  attempts: 3, maximumAttempts: 16,
}
function capture() {
  const events: TraceEventObservationV2[] = []
  return { events, source: { current: (event: Parameters<typeof projectProtocolTraceEvent>[0]) =>
    events.push(snapshotTraceEventObservationV2(projectProtocolTraceEvent(event))) } }
}

describe('content retry evidence export', () => {
  it('retains exact block, attempt and lane evidence through the production projection', () => {
    const { events, source } = capture()
    traceContentRetry({
      ...context, transition: 'retrying', dispatchSequence: 15, laneId: 1, laneEpoch: 0,
      route: 'application-relay', reason: 'recovered_lane',
      successfulBlocks: 5, successfulBlocksAtFailure: 2,
    }, sessionId, source)
    expect(events[0]).toMatchObject({
      eventName: 'content_retry', correlation: { protocol_session_id: 'AQEBAQEBAQEBAQEBAQEBAQ', lane_id: 1, lane_epoch: 0 },
      payload: { first_dispatch_sequence: '7', dispatch_sequence: '15', block_index: '27',
        attempts: 3, maximum_attempts: 16, reason: 'recovered_lane',
        successful_blocks: '5', successful_blocks_at_failure: '2' },
    })
    expect(traceEventRetention(events[0]!)).toBe('milestone')
    expect(() => snapshotTraceEventObservationV2({
      ...events[0], payload: { ...events[0]!.payload, successful_blocks: '2' },
    } as never)).toThrow('lacks new authenticated delivery')
  })

  it.each(['attempt_limit', 'no_recovered_lane'] as const)('preserves %s as a bounded outcome', reason => {
    const { events, source } = capture()
    traceContentRetry({ ...context, attempts: reason === 'attempt_limit' ? 16 : 3,
      transition: 'exhausted', reason }, sessionId, source)
    expect(events[0]).toMatchObject({ eventName: 'content_retry',
      payload: { transition: 'exhausted', reason, first_dispatch_sequence: '7' } })
    expect(traceEventRetention(events[0]!)).toBe('outcome')
  })

  it('keeps normal scheduling evidence exportable alongside retry decisions', () => {
    const { events, source } = capture()
    traceContentScheduling({
      dispatchSequence: 15, laneId: 1, laneEpoch: 0, fileId: context.fileId, localBlockIndex: 27n,
      route: 'application-relay', purpose: 'rescue', expectedMilliseconds: 100,
      deliveryEvidence: 'recent', rescueReason: 'no-progress', pendingBytes: 1024, bytesPerSecond: 10240,
    }, sessionId, source)
    expect(events[0]).toMatchObject({ payload: { delivery_evidence: 'recent', rescue_reason: 'no-progress' } })
  })
})
