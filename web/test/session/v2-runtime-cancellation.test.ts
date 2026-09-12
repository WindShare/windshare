import { afterEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { V2BlockBroker, V2LaneSet, type V2BlockRouteEligibility } from '../../src/content/v2-broker'
import { V2SessionBlockLane, type V2RevisionService } from '../../src/content/v2-session-services'
import { encodeV2Body, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2_OPERATION_CANCEL_REASON } from '../../src/session/v2-runtime-types'
import { V2_SESSION_SEND_TIMEOUT_MILLISECONDS } from '../../src/session/v2-writer'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { projectProtocolTraceEvent } from '../../src/ui/v2-production-trace'
import { id, openSent, runtimeFixture, SHARE } from './v2-send-fixture'

afterEach(() => vi.useRealTimers())
const BODY = encodeV2Body([])

describe('request cancellation across send backpressure', () => {
  it('withdraws an unsent request and does not send a CANCEL for an unknown peer operation', async () => {
    const { runtime, channel, events } = runtimeFixture()
    try {
      const first = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY)
      await channel.sending.promise
      const controller = new AbortController()
      const second = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY, { signal: controller.signal })
      await Promise.resolve()
      controller.abort(new Error('queued request cancelled'))
      await expect(second).rejects.toThrow('queued request cancelled')
      channel.unblock()
      await first
      await runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY)
      const sent = await openSent(channel)
      expect(sent.map(value => value.message.kind)).toEqual([
        V2_MESSAGE_KIND.listChildren, V2_MESSAGE_KIND.listChildren,
      ])
      expect(events).toContainEqual(expect.objectContaining({ transition: 'send_withdrawn' }))
      for (const event of events) {
        expect(() => snapshotTraceEventObservationV2(projectProtocolTraceEvent(event))).not.toThrow()
      }
    } finally { await runtime.close() }
  })

  it('returns cancellation promptly but preserves request/CANCEL/next-request sequence order', async () => {
    const { runtime, channel } = runtimeFixture()
    try {
      const controller = new AbortController()
      const pending = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY, { signal: controller.signal })
      await channel.sending.promise
      const reason = new Error('pause')
      controller.abort(reason)
      await expect(pending).rejects.toBe(reason)
      expect(channel.state).toBe('open')
      expect(channel.sent).toHaveLength(0)
      const next = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY)
      channel.unblock()
      await next
      const sent = await openSent(channel)
      expect(sent.map(value => [value.sequence, value.message.kind])).toEqual([
        [0n, V2_MESSAGE_KIND.listChildren], [1n, V2_MESSAGE_KIND.cancel], [2n, V2_MESSAGE_KIND.listChildren],
      ])
      expect(sent[0]!.message.operationId).toEqual(sent[1]!.message.operationId)
    } finally { await runtime.close() }
  })

  it('completes explicit cancellation of a sent operation without awaiting its remote notification', async () => {
    const { runtime, channel } = runtimeFixture()
    channel.unblock()
    const operation = await runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY)
    channel.block()
    const pending = expect(runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY)).rejects.toMatchObject({
      scope: 'lane',
    })
    try {
      await channel.sending.promise
      const reason = new Error('stop sent operation')
      await runtime.cancelOperation(operation, { cause: reason, protocolReason: V2_OPERATION_CANCEL_REASON.user })
      await expect(operation.next()).rejects.toBe(reason)
      expect(channel.state).toBe('open')
      expect(channel.sent).toHaveLength(1)
    } finally {
      await runtime.close()
      await pending
    }
  })

  it('publishes recoverable lane retirement when send capacity never returns', async () => {
    vi.useFakeTimers()
    const { runtime, channel, events } = runtimeFixture()
    try {
      const pending = expect(runtime.beginOperation(V2_MESSAGE_KIND.listChildren, BODY))
        .rejects.toMatchObject({ scope: 'lane' })
      await channel.sending.promise
      await vi.advanceTimersByTimeAsync(V2_SESSION_SEND_TIMEOUT_MILLISECONDS)
      await pending
      expect(runtime.laneIds()).toEqual([])
      expect(runtime.isClosed).toBe(false)
      expect(events).toContainEqual(expect.objectContaining({
        transition: 'detached', detachmentClass: 'physical_failure',
        failure: expect.objectContaining({ cause: expect.objectContaining({
          cause: expect.objectContaining({ message: 'Session lane send timed out' }),
        }) }),
      }))
    } finally { await runtime.close() }
  })

  it('releases a cancelled upstream slot and its lease barrier before transport unblocks', async () => {
    const { runtime, channel } = runtimeFixture()
    const dispatched = vi.fn()
    const lanes = new V2LaneSet({ onBlockDispatched: dispatched })
    const revisions = { leaseError: () => undefined } as unknown as V2RevisionService
    lanes.add(new V2SessionBlockLane(1, runtime, SHARE, KEY_BYTES, revisions), 'direct')
    const broker = new V2BlockBroker(lanes, { maximumUpstreamReads: 1 })
    const routes: V2BlockRouteEligibility = {
      active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
    }
    const descriptor = {
      shareInstance: SHARE.shareInstance, shareInstanceId: SHARE.shareInstanceId,
      fileId: id(30), fileIdText: 'file', fileRevision: id(31), fileRevisionText: 'revision',
      exactSize: 131_072n, geometry: new FileGeometry(131_072n, 65_536n),
    }
    const firstController = new AbortController()
    const nextController = new AbortController()
    try {
      const first = broker.readBlock({ descriptor, leaseId: id(32), localBlockIndex: 0n },
        { routes, signal: firstController.signal })
      await channel.sending.promise
      const next = broker.readBlock({ descriptor, leaseId: id(33), localBlockIndex: 1n },
        { routes, signal: nextController.signal })
      firstController.abort(new Error('stop old download'))
      await expect(first).rejects.toThrow('stop old download')
      await broker.waitForLeaseIdle(id(32))
      await vi.waitFor(() => expect(dispatched).toHaveBeenCalledTimes(2))
      expect(channel.sent).toHaveLength(0)
      nextController.abort(new Error('stop next download'))
      await expect(next).rejects.toThrow('stop next download')
      await broker.waitForLeaseIdle(id(33))
    } finally {
      broker.close()
      lanes.close()
      await runtime.close()
    }
  })
})

const KEY_BYTES = new Uint8Array(32).fill(4)
