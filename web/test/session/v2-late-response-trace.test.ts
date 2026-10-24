import { describe, expect, it, vi } from 'vitest'
import { ContentRaceWon } from '../../src/content/scheduling/race'
import { encodeBlockRequest } from '../../src/content/flow'
import { snapshotOperationRequest } from '../../src/session/operation-diagnostics'
import { OperationRouter } from '../../src/session/operation-router'
import { encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { createProtocolSessionIdentity } from '../../src/session/identities'
import { OPERATION_CANCEL_REASON } from '../../src/session/runtime-types'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { id, openSent, runtimeFixture } from './v2-send-fixture'

function fixture() {
  const events: ProtocolTraceEvent[] = []
  const terminal = vi.fn()
  const router = new OperationRouter(terminal, () => 0, {
    protocolSessionIdentity: createProtocolSessionIdentity(id(1)),
    trace: { current: event => events.push(event) },
  })
  const operation = router.create(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(3), [7n]))
  const rejected = encodeMessage(MESSAGE_KIND.operationError, operation.id, encodeBody(
    new Map<number, unknown>([[0, 1], [1, 3], [2, 0x3008], [3, false], [4, null], [5, 'Revision operation failed']]),
  ))
  return { events, terminal, router, operation, rejected }
}

describe('retired block-request diagnostics', () => {
  it('captures only bounded lease join keys and handles absent or invalid metadata without rejecting work', () => {
    expect(snapshotOperationRequest(MESSAGE_KIND.listChildren, encodeBody([]))).toBeUndefined()
    expect(snapshotOperationRequest(MESSAGE_KIND.requestBlocks, encodeBody([]))).toBeUndefined()
    expect(snapshotOperationRequest(MESSAGE_KIND.requestBlocks, encodeBody([id(3), []]))).toBeUndefined()
    expect(snapshotOperationRequest(MESSAGE_KIND.releaseLease, encodeBody([id(3)]))).toEqual({ leaseId: '03'.repeat(16) })
    const router = new OperationRouter(() => undefined)
    const operation = router.create(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(3), [7n]))
    expect(operation.requestTrace).toBeUndefined()
    router.terminate(new Error('test cleanup'))
  })

  it('records remote-final replays separately and keeps trace observer failures outside protocol authority', async () => {
    const events: ProtocolTraceEvent[] = []
    const router = new OperationRouter(() => undefined, () => 0, {
      protocolSessionIdentity: createProtocolSessionIdentity(id(1)),
      trace: { current: event => { events.push(event); throw new Error('observer failed') } },
    })
    const operation = router.create(MESSAGE_KIND.releaseLease, encodeBody([id(3)]))
    const complete = encodeMessage(MESSAGE_KIND.operationComplete, operation.id, encodeBody([0]))
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
    operation.cancel(cause, OPERATION_CANCEL_REASON.laneRace)
    const live = router.create(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(5), [0n]))
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
    const complete = encodeMessage(MESSAGE_KIND.operationComplete, live.id, encodeBody([0]))
    await router.route(complete, 2, 1)
    await expect(live.next()).resolves.toEqual(complete)
    router.terminate(new Error('test cleanup'))
  })

  it('attributes a cancellation that wins while an inbound response is queued for routing', async () => {
    const { events, router, operation, rejected } = fixture()
    const pending = router.route(rejected, 1, 0)
    operation.cancel(new ContentRaceWon(), OPERATION_CANCEL_REASON.laneRace)
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
    const malformed = encodeMessage(MESSAGE_KIND.operationError, operation.id, encodeBody([]))
    await expect(router.route(malformed, 1, 0)).rejects.toMatchObject({ scope: 'session' })
    expect(events.some(event => event.eventName === 'protocol_operation' && event.transition === 'late_response_discarded')).toBe(false)
    router.terminate(new Error('test cleanup'))
  })

  it('carries race intent through AbortSignal while cancellation still returns before remote I/O', async () => {
    const { runtime, channel, events } = runtimeFixture()
    const controller = new AbortController()
    try {
      const pending = runtime.beginOperation(MESSAGE_KIND.requestBlocks, encodeBlockRequest(id(3), [7n]),
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
      await runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
      const sent = await openSent(channel)
      expect(sent.map(item => item.message.kind)).toEqual([
        MESSAGE_KIND.requestBlocks, MESSAGE_KIND.cancel, MESSAGE_KIND.listChildren,
      ])
      expect(sent[1]!.message.body).toEqual(encodeBody([OPERATION_CANCEL_REASON.laneRace]))
      for (const event of events) expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(event))).not.toThrow()
    } finally { await runtime.close() }
  })
})
