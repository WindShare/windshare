import { afterEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { BlockBroker, LaneSet, type BlockRouteEligibility } from '../../src/content/broker'
import { SessionBlockLane, type RevisionService } from '../../src/content/session-services'
import { encodeBody, MESSAGE_KIND } from '../../src/session/message'
import { OPERATION_CANCEL_REASON } from '../../src/session/runtime-types'
import { SESSION_SEND_TIMEOUT_MILLISECONDS } from '../../src/session/writer'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { id, openSent, runtimeFixture, SHARE } from './v2-send-fixture'

afterEach(() => vi.useRealTimers())
const BODY = encodeBody([])

describe('request cancellation across send backpressure', () => {
  it('snapshots the request before asynchronous admission can yield to its caller', async () => {
    const { runtime, channel } = runtimeFixture()
    try {
      const body = encodeBody([1])
      const original = body.slice()
      const pending = runtime.beginOperation(MESSAGE_KIND.listChildren, body)
      body.fill(0)
      channel.unblock()
      const operation = await pending
      const sent = await openSent(channel)
      expect(sent).toHaveLength(1)
      expect(sent[0]!.message.operationId).toEqual(operation.id)
      expect(sent[0]!.message.body).toEqual(original)
    } finally { await runtime.close() }
  })

  it('withdraws an unsent request and does not send a CANCEL for an unknown peer operation', async () => {
    const { runtime, channel, events } = runtimeFixture()
    try {
      const first = runtime.beginOperation(MESSAGE_KIND.listChildren, BODY)
      await channel.sending.promise
      const controller = new AbortController()
      const second = runtime.beginOperation(MESSAGE_KIND.listChildren, BODY, { signal: controller.signal })
      await Promise.resolve()
      controller.abort(new Error('queued request cancelled'))
      await expect(second).rejects.toThrow('queued request cancelled')
      channel.unblock()
      await first
      await runtime.beginOperation(MESSAGE_KIND.listChildren, BODY)
      const sent = await openSent(channel)
      expect(sent.map(value => value.message.kind)).toEqual([
        MESSAGE_KIND.listChildren, MESSAGE_KIND.listChildren,
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
      const pending = runtime.beginOperation(MESSAGE_KIND.listChildren, BODY, { signal: controller.signal })
      await channel.sending.promise
      const reason = new Error('pause')
      controller.abort(reason)
      await expect(pending).rejects.toBe(reason)
      expect(channel.state).toBe('open')
      expect(channel.sent).toHaveLength(0)
      const next = runtime.beginOperation(MESSAGE_KIND.listChildren, BODY)
      channel.unblock()
      await next
      const sent = await openSent(channel)
      expect(sent.map(value => [value.sequence, value.message.kind])).toEqual([
        [0n, MESSAGE_KIND.listChildren], [1n, MESSAGE_KIND.cancel], [2n, MESSAGE_KIND.listChildren],
      ])
      expect(sent[0]!.message.operationId).toEqual(sent[1]!.message.operationId)
    } finally { await runtime.close() }
  })

  it('completes explicit cancellation of a sent operation without awaiting its remote notification', async () => {
    const { runtime, channel } = runtimeFixture()
    channel.unblock()
    const operation = await runtime.beginOperation(MESSAGE_KIND.listChildren, BODY)
    channel.block()
    const pending = expect(runtime.beginOperation(MESSAGE_KIND.listChildren, BODY)).rejects.toMatchObject({
      scope: 'lane',
    })
    try {
      await channel.sending.promise
      const reason = new Error('stop sent operation')
      await runtime.cancelOperation(operation, { cause: reason, protocolReason: OPERATION_CANCEL_REASON.user })
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
      const pending = expect(runtime.beginOperation(MESSAGE_KIND.listChildren, BODY))
        .rejects.toMatchObject({ scope: 'lane' })
      await channel.sending.promise
      await vi.advanceTimersByTimeAsync(SESSION_SEND_TIMEOUT_MILLISECONDS)
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
    const lanes = new LaneSet({ onBlockDispatched: dispatched })
    const revisions = { leaseError: () => undefined } as unknown as RevisionService
    lanes.add(new SessionBlockLane(1, runtime, SHARE, KEY_BYTES, revisions), 'direct')
    const broker = new BlockBroker(lanes, { maximumUpstreamReads: 1 })
    const routes: BlockRouteEligibility = {
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
