import { expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { V2LaneSet, type V2BlockRouteEligibility } from '../../src/content/v2-broker'
import { V2SessionBlockLane, type V2RevisionService } from '../../src/content/v2-session-services'
import { encodeV2Body, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { id, KEY, runtimeFixture, SHARE } from './v2-send-fixture'

it('keeps physical send failure within the lane boundary for in-flight requests', async () => {
  const { runtime, channel } = runtimeFixture()
  const physicalFailure = new Error('physical socket failed')
  vi.spyOn(channel, 'send').mockRejectedValueOnce(physicalFailure)
  try {
    await expect(runtime.beginOperation(V2_MESSAGE_KIND.listChildren, encodeV2Body([])))
      .rejects.toMatchObject({ scope: 'lane', cause: physicalFailure })
  } finally {
    await runtime.close()
  }
})

it('keeps physical receive failure within the lane boundary for waiting sends', async () => {
  const { runtime, channel } = runtimeFixture()
  const physicalFailure = new Error('physical socket read failed')
  try {
    const pending = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, encodeV2Body([]))
    const rejected = expect(pending).rejects.toMatchObject({ scope: 'lane', cause: physicalFailure })
    await channel.sending.promise
    channel.failIncoming(physicalFailure)
    await rejected
  } finally {
    await runtime.close()
  }
})

it.each(['send', 'receive'] as const)('retries a physical %s failure on the healthy content lane', async (direction) => {
  const { runtime, channel } = runtimeFixture()
  const dispatched = vi.fn()
  const lanes = new V2LaneSet({ onBlockDispatched: dispatched })
  const revisions = { leaseError: () => undefined } as unknown as V2RevisionService
  lanes.add(new V2SessionBlockLane(1, runtime, SHARE, KEY, revisions), 'application-relay')
  const descriptor = {
    shareInstance: SHARE.shareInstance, shareInstanceId: SHARE.shareInstanceId,
    fileId: id(30), fileIdText: 'file', fileRevision: id(31), fileRevisionText: 'revision',
    exactSize: 1n, geometry: new FileGeometry(1n, 65_536n),
  }
  const demand = { descriptor, leaseId: id(32), localBlockIndex: 0n }
  const expected = { descriptor, localBlockIndex: 0n, data: Uint8Array.of(0x42) }
  const routes: V2BlockRouteEligibility = {
    active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
  }
  const fetchPeer = vi.fn(async () => expected)
  try {
    const pending = lanes.fetch(demand, routes, new AbortController().signal)
    const completed = expect(pending).resolves.toBe(expected)
    await channel.sending.promise
    lanes.add({ id: 2, fetchBlock: fetchPeer }, 'direct')
    const physicalFailure = new Error(`physical ${direction} failed during relay cut`)
    if (direction === 'send') channel.failOutgoing(physicalFailure)
    else channel.failIncoming(physicalFailure)
    await completed
    expect(fetchPeer).toHaveBeenCalledOnce()
    expect(fetchPeer).toHaveBeenCalledWith(demand, expect.any(AbortSignal))
    expect(dispatched.mock.calls.map(([observation]) => observation.route))
      .toEqual(['application-relay', 'direct'])
    expect(runtime.isClosed).toBe(false)
  } finally {
    lanes.close()
    await runtime.close()
  }
})

it('keeps envelope validation failure fatal while sends are waiting', async () => {
  const { runtime, channel, events } = runtimeFixture()
  try {
    const pending = runtime.beginOperation(V2_MESSAGE_KIND.listChildren, encodeV2Body([]))
    const rejected = expect(pending).rejects.toMatchObject({ name: 'V2EnvelopeError' })
    await channel.sending.promise
    channel.receive(Uint8Array.of(0xff))
    await rejected
    await vi.waitFor(() => expect(runtime.isClosed).toBe(true))
    expect(events).toContainEqual(expect.objectContaining({
      transition: 'detached', detachmentClass: 'authenticated_failure',
    }))
  } finally {
    await runtime.close()
  }
})
