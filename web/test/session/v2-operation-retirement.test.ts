import { afterEach, describe, expect, it, vi } from 'vitest'
import { encodeV2BlockRequest } from '../../src/content/v2-flow'
import { decodeV2Message, encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2OperationRouter, V2_MAXIMUM_TRACKED_OPERATIONS, V2_OPERATION_TOMBSTONE_MILLISECONDS } from '../../src/session/v2-operation-router'
import { V2IssuedOperations } from '../../src/session/operations/issued-operations'
import { V2EnvelopeSealer } from '../../src/session/v2-envelope'
import { V2_OPERATION_CANCEL_REASON } from '../../src/session/v2-runtime-types'
import { projectProtocolTraceEvent } from '../../src/ui/v2-production-trace'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'
import { fragmentRecord } from '../content/v2-fragment-fixture'
import { BINDING, KEY, id, runtimeFixture } from './v2-send-fixture'

const BODY = encodeV2BlockRequest(id(7), [0n])
const PAUSE = new DOMException('User paused', 'AbortError')
const STALLED_MILLISECONDS = 31_500
const RECORD = new Uint8Array([1, 2, 3, 4])
const fragment = (operationId: Uint8Array) => decodeV2Message(fragmentRecord(operationId, RECORD)[0]!)

afterEach(() => vi.useRealTimers())

describe('issued operation history outlives replay detail', () => {
  it.each([29_999, 30_000, STALLED_MILLISECONDS, 86_400_000])('discards cancelled block responses after %i ms', async elapsed => {
    let now = 0
    const router = new V2OperationRouter(() => undefined, () => now)
    const live = router.create(V2_MESSAGE_KIND.requestBlocks, BODY)
    const cancelled = router.create(V2_MESSAGE_KIND.requestBlocks, BODY)
    cancelled.cancel(PAUSE)
    now = elapsed
    await expect(router.route(fragment(cancelled.id))).resolves.toBeUndefined()
    await expect(router.route(encodeV2Message(V2_MESSAGE_KIND.operationComplete, cancelled.id,
      encodeV2Body(new Map([[0, 1], [1, 0]]))))).resolves.toBeUndefined()
    expect(router.active()).toEqual([live])
    const current = fragment(live.id)
    await router.route(current)
    await expect(live.next()).resolves.toEqual(current)
    await expect(cancelled.next()).rejects.toBe(PAUSE)
    router.terminate(PAUSE)
  })

  it('rejects never-issued, other-session, wrong-kind and wrong-scope responses after collection', async () => {
    let now = 0
    const router = new V2OperationRouter(() => undefined, () => now)
    const operation = router.create(V2_MESSAGE_KIND.requestBlocks, BODY)
    operation.cancel(PAUSE)
    now = STALLED_MILLISECONDS
    const future = operation.id.slice()
    future[future.length - 1] = future[future.length - 1]! + 1
    const otherSession = operation.id.slice()
    otherSession[0] = otherSession[0]! ^ 1
    const unissuedKind = operation.id.slice()
    unissuedKind[8] = V2_MESSAGE_KIND.listChildren
    for (const unknown of [future, otherSession, unissuedKind]) {
      await expect(router.route(fragment(unknown))).rejects.toMatchObject({ scope: 'session' })
    }
    await expect(router.route(encodeV2Message(V2_MESSAGE_KIND.openResults, operation.id, encodeV2Body([]))))
      .rejects.toMatchObject({ scope: 'session' })
    const wrongScope = encodeV2Message(V2_MESSAGE_KIND.operationError, operation.id,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, 2], [2, 0x2001], [3, false], [4, null], [5, 'directory failed']])))
    await expect(router.route(wrongScope)).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(encodeV2Message(V2_MESSAGE_KIND.operationError, operation.id, encodeV2Body([]))))
      .rejects.toMatchObject({ scope: 'session' })
    router.terminate(PAUSE)
  })

  it('reclaims detailed capacity while preserving old identities through repeated full budgets', async () => {
    let now = 0
    const router = new V2OperationRouter(() => undefined, () => now)
    const first = router.create(V2_MESSAGE_KIND.requestBlocks, BODY)
    first.cancel(PAUSE)
    for (let generation = 0; generation < 2; generation += 1) {
      now += V2_OPERATION_TOMBSTONE_MILLISECONDS
      for (let index = 0; index < V2_MAXIMUM_TRACKED_OPERATIONS; index += 1) {
        router.create(V2_MESSAGE_KIND.requestBlocks, BODY).cancel(PAUSE)
      }
      await expect(router.route(fragment(first.id))).resolves.toBeUndefined()
    }
    now += V2_OPERATION_TOMBSTONE_MILLISECONDS
    const admitted = await router.admit(V2_MESSAGE_KIND.requestBlocks, BODY)
    expect(admitted.id).not.toEqual(first.id)
    router.terminate(PAUSE)
  })

  it('keeps authenticated lanes usable after a delayed cancelled fragment and bounds its trace', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    const started = Date.now()
    const harness = runtimeFixture()
    harness.channel.unblock()
    try {
      const live = await harness.runtime.beginOperation(V2_MESSAGE_KIND.requestBlocks, BODY)
      const cancelled = await harness.runtime.beginOperation(V2_MESSAGE_KIND.requestBlocks, BODY)
      await harness.runtime.cancelOperation(cancelled, { protocolReason: V2_OPERATION_CANCEL_REASON.user, cause: PAUSE })
      vi.setSystemTime(started + STALLED_MILLISECONDS)
      const sealer = new V2EnvelopeSealer(KEY, { ...BINDING, direction: 1 })
      for (let copy = 0; copy < 3; copy += 1) {
        harness.channel.receive(await sealer.seal(fragment(cancelled.id).plaintext))
      }
      const current = fragment(live.id)
      harness.channel.receive(await sealer.seal(current.plaintext))
      await expect(live.next()).resolves.toEqual(current)
      expect(harness.runtime.isClosed).toBe(false)
      const discarded = harness.events.filter(event =>
        event.eventName === 'protocol_operation' && event.transition === 'retired_response_discarded')
      expect(discarded).toHaveLength(1)
      expect(discarded[0]!.correlation.protocolOperationId!.copyBytes()).toEqual(cancelled.id)
      const projected = projectProtocolTraceEvent(discarded[0]!)
      expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    } finally { await harness.runtime.close() }
  })

  it('keeps kind-specific issuance immutable and rejects invalid identity sources', () => {
    const prefix = new Uint8Array(8).fill(3)
    const random = vi.fn(() => prefix)
    const issued = new V2IssuedOperations(random)
    const first = issued.issue(V2_MESSAGE_KIND.requestBlocks)
    const saved = first.slice()
    first.fill(0)
    prefix.fill(0)
    const second = issued.issue(V2_MESSAGE_KIND.releaseLease)
    expect(issued.requestKind(saved)).toBe(V2_MESSAGE_KIND.requestBlocks)
    expect(issued.requestKind(second)).toBe(V2_MESSAGE_KIND.releaseLease)
    expect(issued.requestKind(first)).toBeUndefined()
    expect(issued.requestKind(saved.subarray(1))).toBeUndefined()
    expect(random).toHaveBeenCalledTimes(1)
    expect(() => issued.issue(V2_MESSAGE_KIND.blockFragment)).toThrow('cannot begin')
    for (const bad of [new Uint8Array(8), new Uint8Array(7).fill(1)]) {
      expect(() => new V2IssuedOperations(() => bad).issue(V2_MESSAGE_KIND.requestBlocks))
        .toThrow('invalid bytes')
    }
  })
})
