import { describe, expect, it } from 'vitest'
import { snapshotTraceEventObservationV2 } from '../../../src/diagnostics/export/trace-event-v2'
import { projectProtocolTraceEvent } from '../../../src/ui/v2-protocol-trace'
import { createV2ProtocolOperationIdentity, createV2ProtocolSessionIdentity } from '../../../src/session/v2-identities'

describe('block wait diagnostics', () => {
  it.each(['awaiting_first_fragment', 'receiving_fragments'] as const)('exports correlated %s timeout decisions', phase => {
    const event = projectProtocolTraceEvent({
      eventName: 'protocol_operation', transition: 'cancelled', requestKind: 'request_blocks',
      cancellationReason: 'timeout',
      blockWait: { phase, waitedMilliseconds: 35_000.9, queueProgress: 12 },
      correlation: {
        protocolSessionId: createV2ProtocolSessionIdentity(new Uint8Array(16).fill(1)),
        protocolOperationId: createV2ProtocolOperationIdentity(new Uint8Array(16).fill(2)),
        lane: { id: 1, epoch: 0 },
      },
    })
    const snapshot = snapshotTraceEventObservationV2(event)
    expect(snapshot).toMatchObject({
      payload: { block_wait: { phase, waited_ms: '35000', queue_progress: '12' } },
    })
    expect(() => snapshotTraceEventObservationV2({
      ...event, payload: { ...event.payload, block_wait: { phase: 'unknown', waited_ms: '35000', queue_progress: '12' } },
    } as never)).toThrow()
  })
})
