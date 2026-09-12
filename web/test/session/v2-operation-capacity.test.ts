import { afterEach, describe, expect, it, vi } from 'vitest'

import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import { projectProtocolTraceEvent } from '../../src/ui/v2-production-trace'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'
import {
  V2_MAXIMUM_ACTIVE_OPERATIONS,
  V2_MAXIMUM_TRACKED_OPERATIONS,
  V2_OPERATION_TOMBSTONE_MILLISECONDS,
  V2OperationRouter,
} from '../../src/session/v2-operation-router'

const BODY = encodeV2Body([])

function identity(index: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  new DataView(value.buffer).setUint32(0, index + 1)
  return value
}

afterEach(() => vi.useRealTimers())

describe('operation lifecycle capacity', () => {
  it.each(['cancel', 'complete', 'terminate'] as const)('does not lose a synchronous %s during admission tracing', async (action) => {
    const controller = new AbortController()
    let settleActive: () => void = () => undefined
    const router = new V2OperationRouter(() => undefined, () => Date.now(), {
      protocolSessionIdentity: createV2ProtocolSessionIdentity(identity(0)),
      trace: { current: (event) => {
        if (event.eventName !== 'protocol_operation' || event.transition !== 'admission_waiting') return
        if (action === 'cancel') controller.abort(new Error('cancelled during trace'))
        if (action === 'complete') settleActive()
        if (action === 'terminate') router.terminate(new Error('closed during trace'))
      } },
    })
    for (let index = 0; index < V2_MAXIMUM_ACTIVE_OPERATIONS; index += 1) {
      const operation = router.create(identity(index), V2_MESSAGE_KIND.listChildren, BODY)
      if (index === 0) settleActive = () => operation.cancel(new Error('settled during trace'))
    }
    const pending = router.admit(identity(V2_MAXIMUM_ACTIVE_OPERATIONS), V2_MESSAGE_KIND.listChildren, BODY, controller.signal)
    if (action === 'complete') {
      await expect(pending).resolves.toMatchObject({ requestKind: V2_MESSAGE_KIND.listChildren })
    } else {
      await expect(pending).rejects.toBeDefined()
    }
    router.terminate(new Error('done'))
  })

  it('waits for a retained slot and resumes on expiry without incoming traffic', async () => {
    vi.useFakeTimers()
    const events: V2ProtocolTraceEvent[] = []
    const router = new V2OperationRouter(() => undefined, () => Date.now(), {
      protocolSessionIdentity: createV2ProtocolSessionIdentity(identity(0)),
      trace: { current: (event) => events.push(event) },
    })
    for (let index = 0; index < V2_MAXIMUM_TRACKED_OPERATIONS; index += 1) {
      router.create(identity(index), V2_MESSAGE_KIND.listChildren, BODY).cancel(new Error('completed work'))
    }
    events.length = 0
    let admitted = false
    const pending = router.admit(identity(V2_MAXIMUM_TRACKED_OPERATIONS), V2_MESSAGE_KIND.listChildren, BODY)
      .then((operation) => { admitted = true; return operation })
    await Promise.resolve()
    expect(admitted).toBe(false)
    expect(vi.getTimerCount()).toBe(1)
    await vi.advanceTimersByTimeAsync(V2_OPERATION_TOMBSTONE_MILLISECONDS - 1)
    expect(admitted).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    const operation = await pending
    expect(events).toMatchObject([
      { transition: 'admission_waiting', capacity: 'retained', activeOperations: 0, trackedOperations: V2_MAXIMUM_TRACKED_OPERATIONS },
      { transition: 'admission_ready' },
    ])
    for (const event of events) {
      const projected = projectProtocolTraceEvent(event)
      expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
      expect(event.correlation.protocolOperationId).toBeDefined()
    }
    await router.route(encodeV2Message(V2_MESSAGE_KIND.catalogResult, operation.id, BODY))
    expect((await operation.next()).kind).toBe(V2_MESSAGE_KIND.catalogResult)
    router.terminate(new Error('done'))
    expect(vi.getTimerCount()).toBe(0)
  })

  it('reserves concurrent admissions atomically and cancels waiters without creating operations', async () => {
    const router = new V2OperationRouter(() => undefined)
    const active = Array.from({ length: V2_MAXIMUM_ACTIVE_OPERATIONS }, (_, index) =>
      router.create(identity(index), V2_MESSAGE_KIND.listChildren, BODY))
    await expect(router.admit(identity(0), V2_MESSAGE_KIND.listChildren, BODY)).rejects.toThrow('Operation ID was reused')
    const controller = new AbortController()
    const cancelledId = identity(V2_MAXIMUM_ACTIVE_OPERATIONS)
    const cancelled = router.admit(cancelledId, V2_MESSAGE_KIND.listChildren, BODY, controller.signal)
    const first = router.admit(identity(V2_MAXIMUM_ACTIVE_OPERATIONS + 1), V2_MESSAGE_KIND.listChildren, BODY)
    let secondAdmitted = false
    const second = router.admit(identity(V2_MAXIMUM_ACTIVE_OPERATIONS + 2), V2_MESSAGE_KIND.listChildren, BODY)
      .then((operation) => { secondAdmitted = true; return operation })
    const cancellation = new Error('user stopped waiting')
    const rejection = expect(cancelled).rejects.toBe(cancellation)
    controller.abort(cancellation)
    await rejection
    active[0]!.cancel(new Error('finished'))
    const admitted = await first
    expect(secondAdmitted).toBe(false)
    admitted.cancel(new Error('finished'))
    await second
    active[1]!.cancel(new Error('finished'))
    // Cancellation while waiting must not consume an ID or a retention slot.
    router.create(cancelledId, V2_MESSAGE_KIND.listChildren, BODY)
    router.terminate(new Error('done'))
  })

  it('settles pending admissions on session termination and rejects pre-aborted requests', async () => {
    const router = new V2OperationRouter(() => undefined)
    for (let index = 0; index < V2_MAXIMUM_ACTIVE_OPERATIONS; index += 1) {
      router.create(identity(index), V2_MESSAGE_KIND.listChildren, BODY)
    }
    const pending = router.admit(identity(V2_MAXIMUM_ACTIVE_OPERATIONS), V2_MESSAGE_KIND.listChildren, BODY)
    router.terminate(new Error('session closed'))
    await expect(pending).rejects.toMatchObject({ scope: 'session' })
    const controller = new AbortController()
    controller.abort(new Error('already cancelled'))
    await expect(router.admit(identity(0), V2_MESSAGE_KIND.listChildren, BODY, controller.signal))
      .rejects.toThrow('already cancelled')
  })
})
