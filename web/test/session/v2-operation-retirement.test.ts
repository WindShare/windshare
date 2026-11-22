import { describe, expect, it } from 'vitest'
import { encodeBlockRequest } from '../../src/content/flow'
import { decodeMessage, encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { OperationRouter, MAXIMUM_RECENT_TOMBSTONES } from '../../src/session/operation-router'
import { EnvelopeSealer } from '../../src/session/envelope'
import { OPERATION_CANCEL_REASON } from '../../src/session/runtime-types'
import { projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { fragmentRecord } from '../content/v2-fragment-fixture'
import { BINDING, KEY, id, runtimeFixture } from './v2-send-fixture'

const BODY = encodeBlockRequest(id(7), [0n])
const PAUSE = new DOMException('User paused', 'AbortError')
const RECORD = new Uint8Array([1, 2, 3, 4])
const fragment = (operationId: bigint) => decodeMessage(fragmentRecord(operationId, RECORD)[0]!)

describe('monotonic operation retirement and bounded recent tombstones', () => {
  it('discards cancelled block responses both in the recent tombstone cache and after FIFO eviction', async () => {
    const router = new OperationRouter(() => undefined)
    const live = router.create(MESSAGE_KIND.requestBlocks, BODY)
    const cancelled = router.create(MESSAGE_KIND.requestBlocks, BODY)
    cancelled.cancel(PAUSE)
    await expect(router.route(fragment(cancelled.id))).resolves.toBeUndefined()
    await expect(router.route(encodeMessage(MESSAGE_KIND.operationComplete, cancelled.id,
      encodeBody(new Map([[0, 1], [1, 0]]))))).resolves.toBeUndefined()

    // Evict cancelled from the bounded FIFO tombstone cache.
    for (let index = 0; index < MAXIMUM_RECENT_TOMBSTONES; index += 1) {
      router.create(MESSAGE_KIND.requestBlocks, BODY).cancel(PAUSE)
    }
    await expect(router.route(fragment(cancelled.id))).resolves.toBeUndefined()
    await expect(router.route(encodeMessage(MESSAGE_KIND.operationComplete, cancelled.id,
      encodeBody(new Map([[0, 1], [1, 0]]))))).resolves.toBeUndefined()

    expect(router.active()).toEqual([live])
    const current = fragment(live.id)
    await router.route(current)
    await expect(live.next()).resolves.toEqual(current)
    await expect(cancelled.next()).rejects.toBe(PAUSE)
    router.terminate(PAUSE)
  })

  it('rejects never-issued IDs, malformed errors, and wrong-kind or wrong-scope responses in the tombstone window', async () => {
    const router = new OperationRouter(() => undefined)
    const operation = router.create(MESSAGE_KIND.requestBlocks, BODY)
    operation.cancel(PAUSE)
    const future = operation.id + 1n
    await expect(router.route(fragment(future))).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(encodeMessage(MESSAGE_KIND.openResults, operation.id, encodeBody([]))))
      .rejects.toMatchObject({ scope: 'session' })
    const wrongScope = encodeMessage(MESSAGE_KIND.operationError, operation.id,
      encodeBody(new Map<number, unknown>([[0, 1], [1, 2], [2, 0x2001], [3, false], [4, null], [5, 'directory failed']])))
    await expect(router.route(wrongScope)).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(encodeMessage(MESSAGE_KIND.operationError, operation.id, encodeBody([]))))
      .rejects.toMatchObject({ scope: 'session' })

    // Even after FIFO eviction, unissued IDs and malformed errors are still rejected.
    for (let index = 0; index < MAXIMUM_RECENT_TOMBSTONES; index += 1) {
      router.create(MESSAGE_KIND.requestBlocks, BODY).cancel(PAUSE)
    }
    await expect(router.route(fragment(BigInt(MAXIMUM_RECENT_TOMBSTONES + 10))))
      .rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(encodeMessage(MESSAGE_KIND.operationError, operation.id, encodeBody([]))))
      .rejects.toMatchObject({ scope: 'session' })
    router.terminate(PAUSE)
  })

  it('sustains repeated full tombstone budgets without blocking and never reuses IDs', async () => {
    const router = new OperationRouter(() => undefined)
    const first = router.create(MESSAGE_KIND.requestBlocks, BODY)
    first.cancel(PAUSE)
    let previous = first.id
    for (let index = 0; index < MAXIMUM_RECENT_TOMBSTONES * 2; index += 1) {
      const next = router.create(MESSAGE_KIND.requestBlocks, BODY)
      expect(next.id).toBe(previous + 1n)
      previous = next.id
      next.cancel(PAUSE)
    }
    await expect(router.route(fragment(first.id))).resolves.toBeUndefined()
    const admitted = await router.admit(MESSAGE_KIND.requestBlocks, BODY)
    expect(admitted.id).toBe(previous + 1n)
    router.terminate(PAUSE)
  })

  it('keeps authenticated lanes usable after an evicted cancelled fragment and bounds its trace', async () => {
    const harness = runtimeFixture()
    harness.channel.unblock()
    try {
      const live = await harness.runtime.beginOperation(MESSAGE_KIND.requestBlocks, BODY)
      const cancelled = await harness.runtime.beginOperation(MESSAGE_KIND.requestBlocks, BODY)
      await harness.runtime.cancelOperation(cancelled, { protocolReason: OPERATION_CANCEL_REASON.user, cause: PAUSE })
      for (let index = 0; index < MAXIMUM_RECENT_TOMBSTONES; index += 1) {
        const filler = await harness.runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
        filler.cancel(PAUSE)
      }
      const sealer = new EnvelopeSealer(KEY, { ...BINDING, direction: 1 })
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
      expect(discarded[0]!.correlation.protocolOperationId!.value).toBe(cancelled.id)
      const projected = projectProtocolTraceEvent(discarded[0]!)
      expect(projected).toMatchObject({
        event: 'protocol_operation',
        payload: { transition: 'retired_response_discarded' },
      })
      expect(() => JSON.stringify(projected)).not.toThrow()
    } finally { await harness.runtime.close() }
  })

  it('rejects non-request message kinds when creating operations', () => {
    const router = new OperationRouter(() => undefined)
    expect(() => router.create(MESSAGE_KIND.blockFragment, BODY)).toThrow('cannot begin')
    router.terminate(PAUSE)
  })
})
