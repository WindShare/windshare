import { expect, it } from 'vitest'
import { projectProtocolTraceEvent } from '../../../src/application/diagnostics/protocol-trace'
import { validateTraceEventPayloadV2 } from '../../../src/diagnostics/export/trace-event-payload-v2'
import { createProtocolSessionIdentity } from '../../../src/session/identities'

it.each(['retired', 'replaced', 'session_closed'] as const)(
  'exports %s closure with its lane identity and no failure detail', reason => {
    const projected = projectProtocolTraceEvent({
      eventName: 'lane_transition', transition: 'detached', termination: { kind: 'closed', reason },
      correlation: {
        protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
        lane: { id: 3, epoch: 7 },
      },
    })
    expect(projected.payload).toEqual({ transition: 'detached', detachment_class: 'closed', close_reason: reason })
    expect(projected.correlation).toMatchObject({ lane_id: 3, lane_epoch: 7 })
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
  },
)

it.each([
  { detachment_class: 'closed' },
  { detachment_class: 'closed', close_reason: 'unknown' },
  { detachment_class: 'closed', close_reason: 'retired', failure_detail: 'fabricated failure' },
  { detachment_class: 'physical_failure', close_reason: 'retired' },
])('rejects contradictory or missing closure evidence: %j', payload => {
  expect(() => validateTraceEventPayloadV2('lane_transition', { transition: 'detached', ...payload })).toThrow()
})
