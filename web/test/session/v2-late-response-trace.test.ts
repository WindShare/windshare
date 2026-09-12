import { describe, expect, it, vi } from 'vitest'
import { ContentRaceWon } from '../../src/content/scheduling/race'
import { encodeV2BlockRequest } from '../../src/content/v2-flow'
import { snapshotOperationRequest } from '../../src/session/v2-operation-diagnostics'
import { V2OperationRouter } from '../../src/session/v2-operation-router'
import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import { V2_OPERATION_CANCEL_REASON } from '../../src/session/v2-runtime-types'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { projectProtocolTraceEvent } from '../../src/ui/v2-production-trace'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { id, openSent, runtimeFixture } from './v2-send-fixture'

function fixture() {
  const events: V2ProtocolTraceEvent[] = []
  const terminal = vi.fn()
  const router = new V2OperationRouter(terminal, () => 0, {
    protocolSessionIdentity: createV2ProtocolSessionIdentity(id(1)),
    trace: { current: event => events.push(event) },
  })
  const operation = router.create(id(2), V2_MESSAGE_KIND.requestBlocks, encodeV2BlockRequest(id(3), [7n]))
  const rejected = encodeV2Message(V2_MESSAGE_KIND.operationError, operation.id, encodeV2Body(
    new Map<number, unknown>([[0, 1], [1, 3], [2, 0x3008], [3, false], [4, null], [5, 'Revision operation failed']]),
  ))
  return { events, terminal, router, operation, rejected }
}

describe('retired block-request diagnostics', () => {
  it('captures only bounded lease join keys and handles absent or invalid metadata without rejecting work', () => {
    expect(snapshotOperationRequest(V2_MESSAGE_KIND.listChildren, encodeV2Body([]))).toBeUndefined()
    expect(snapshotOperationRequest(V2_MESSAGE_KIND.requestBlocks, encodeV2Body([]))).toBeUndefined()
    expect(snapshotOperationRequest(V2_MESSAGE_KIND.requestBlocks, encodeV2Body([id(3), []]))).toBeUndefined()
    expect(snapshotOperationRequest(V2_MESSAGE_KIND.releaseLease, encodeV2Body([id(3)]))).toEqual({ leaseId: '03'.repeat(16) })
    const router = new V2OperationRouter(() => undefined)
    const operation = router.create(id(2), V2_MESSAGE_KIND.requestBlocks, encodeV2BlockRequest(id(3), [7n]))
    expect(operation.requestTrace).toBeUndefined()
    router.terminate(new Error('test cleanup'))
  })

  it('records remote-final replays separately and keeps trace observer failures outside protocol authority', async () => {
    const events: V2ProtocolTraceEvent[] = []
    const router = new V2OperationRouter(() => undefined, () => 0, {
      protocolSessionIdentity: createV2ProtocolSessionIdentity(id(1)),
      trace: { current: event => { events.push(event); throw new Error('observer failed') } },
    })
    const operation = router.create(id(2), V2_MESSAGE_KIND.releaseLease, encodeV2Body([id(3)]))
    const complete = encodeV2Message(V2_MESSAGE_KIND.operationComplete, operation.id, encodeV2Body([0]))
    await router.route(complete, 1, 0)
    await expect(operation.next()).resolves.toEqual(complete)
    await expect(router.route(complete, 1, 0)).resolves.toBeUndefined()
    const late = events.find(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')!
    expect(late).toMatchObject({ settlement: 'remote_final', request: { leaseId: '03'.repeat(16) } })
    expect(late).not.toHaveProperty('cancellationReason')
    expect(late).not.toHaveProperty('protocolFailure')
    expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(late))).not.toThrow()
    router.terminate(new Error('test cleanup'))
  })

  it('explains a cancelled lease rejection once without creating an active failure or ending another request', async () => {
    const { events, terminal, router, operation, rejected } = fixture()
    const cause = new ContentRaceWon()
    operation.cancel(cause, V2_OPERATION_CANCEL_REASON.laneRace)
    const live = router.create(id(4), V2_MESSAGE_KIND.requestBlocks, encodeV2BlockRequest(id(5), [0n]))
    await router.route(rejected, 1, 0)
    await router.route(rejected, 1, 0)
    expect(events.filter(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')).toHaveLength(1)
    const late = events.find(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')!
    expect(late).toMatchObject({
      requestKind: 'request_blocks', responseKind: 'operation_error', settlement: 'local_cancel',
      cancellationReason: 'lane_race',
      request: { leaseId: '03'.repeat(16), blocks: { firstIndex: 7n, count: 1 } },
      protocolError: { code: 0x3008, scope: 'revision' },
      correlation: { lane: { id: 1, epoch: 0 } },
    })
    expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(late))).not.toThrow()
    expect(router.protocolFailureFor(rejected)).toBeUndefined()
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'authenticated_failure')).toBe(false)
    await expect(operation.next()).rejects.toBe(cause)
    expect(terminal).not.toHaveBeenCalled()
    const complete = encodeV2Message(V2_MESSAGE_KIND.operationComplete, live.id, encodeV2Body([0]))
    await router.route(complete, 2, 1)
    await expect(live.next()).resolves.toEqual(complete)
    router.terminate(new Error('test cleanup'))
  })

  it('attributes a cancellation that wins while an inbound response is queued for routing', async () => {
    const { events, router, operation, rejected } = fixture()
    const pending = router.route(rejected, 1, 0)
    operation.cancel(new ContentRaceWon(), V2_OPERATION_CANCEL_REASON.laneRace)
    await pending
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')).toBe(true)
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'response_received')).toBe(false)
    router.terminate(new Error('test cleanup'))
  })

  it('continues reporting INVALID_LEASE for a live request and validates malformed retired traffic', async () => {
    const { events, router, operation, rejected } = fixture()
    await router.route(rejected, 1, 0)
    await expect(operation.next()).resolves.toEqual(rejected)
    expect(router.protocolFailureFor(rejected)?.content.code).toBe(0x3008)
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'authenticated_failure')).toBe(true)
    const malformed = encodeV2Message(V2_MESSAGE_KIND.operationError, operation.id, encodeV2Body([]))
    await expect(router.route(malformed, 1, 0)).rejects.toMatchObject({ scope: 'session' })
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')).toBe(false)
    router.terminate(new Error('test cleanup'))
  })

  it('carries race intent through AbortSignal while cancellation still returns before remote I/O', async () => {
    const { runtime, channel, events } = runtimeFixture()
    const controller = new AbortController()
    try {
      const pending = runtime.beginOperation(V2_MESSAGE_KIND.requestBlocks, encodeV2BlockRequest(id(3), [7n]),
        { signal: controller.signal })
      await channel.sending.promise
      controller.abort(new ContentRaceWon())
      await expect(pending).rejects.toBeInstanceOf(ContentRaceWon)
      expect(channel.sent).toHaveLength(0)
      expect(events).toContainEqual(expect.objectContaining({
        transition: 'cancelled', cancellationReason: 'lane_race',
        request: { leaseId: '03'.repeat(16), blocks: { firstIndex: 7n, count: 1 } },
      }))
      channel.unblock()
      await runtime.beginOperation(V2_MESSAGE_KIND.listChildren, encodeV2Body([]))
      const sent = await openSent(channel)
      expect(sent.map(item => item.message.kind)).toEqual([
        V2_MESSAGE_KIND.requestBlocks, V2_MESSAGE_KIND.cancel, V2_MESSAGE_KIND.listChildren,
      ])
      expect(sent[1]!.message.body).toEqual(encodeV2Body([V2_OPERATION_CANCEL_REASON.laneRace]))
      for (const event of events) expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(event))).not.toThrow()
    } finally { await runtime.close() }
  })
})
