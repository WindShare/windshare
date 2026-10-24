import { afterEach, describe, expect, it, vi } from 'vitest'

import { encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { createProtocolSessionIdentity } from '../../src/session/identities'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { projectProtocolTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'
import {
  MAXIMUM_ACTIVE_OPERATIONS,
  MAXIMUM_TRACKED_OPERATIONS,
  OPERATION_TOMBSTONE_MILLISECONDS,
  OperationRouter,
} from '../../src/session/operation-router'

const BODY = encodeBody([])

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
    const router = new OperationRouter(() => undefined, () => Date.now(), {
      protocolSessionIdentity: createProtocolSessionIdentity(identity(0)),
      trace: { current: (event) => {
        if (event.eventName !== 'protocol_operation' || event.transition !== 'admission_waiting') return
        if (action === 'cancel') controller.abort(new Error('cancelled during trace'))
        if (action === 'complete') settleActive()
        if (action === 'terminate') router.terminate(new Error('closed during trace'))
      } },
    })
    for (let index = 0; index < MAXIMUM_ACTIVE_OPERATIONS; index += 1) {
      const operation = router.create(MESSAGE_KIND.listChildren, BODY)
      if (index === 0) settleActive = () => operation.cancel(new Error('settled during trace'))
    }
    const pending = router.admit(MESSAGE_KIND.listChildren, BODY, controller.signal)
    if (action === 'complete') {
      await expect(pending).resolves.toMatchObject({ requestKind: MESSAGE_KIND.listChildren })
    } else {
      await expect(pending).rejects.toBeDefined()
    }
    router.terminate(new Error('done'))
  })

  it('waits for a retained slot and resumes on expiry without incoming traffic', async () => {
    vi.useFakeTimers()
    const events: ProtocolTraceEvent[] = []
    const router = new OperationRouter(() => undefined, () => Date.now(), {
      protocolSessionIdentity: createProtocolSessionIdentity(identity(0)),
      trace: { current: (event) => events.push(event) },
    })
    for (let index = 0; index < MAXIMUM_TRACKED_OPERATIONS; index += 1) {
      router.create(MESSAGE_KIND.listChildren, BODY).cancel(new Error('completed work'))
    }
    events.length = 0
    let admitted = false
    const pending = router.admit(MESSAGE_KIND.listChildren, BODY)
      .then((operation) => { admitted = true; return operation })
    await Promise.resolve()
    expect(admitted).toBe(false)
    expect(vi.getTimerCount()).toBe(1)
    await vi.advanceTimersByTimeAsync(OPERATION_TOMBSTONE_MILLISECONDS - 1)
    expect(admitted).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    const operation = await pending
    expect(events).toMatchObject([
      { transition: 'admission_waiting', capacity: 'retained', activeOperations: 0, trackedOperations: MAXIMUM_TRACKED_OPERATIONS },
      { transition: 'admission_ready' },
    ])
    for (const event of events) {
      const projected = projectProtocolTraceEvent(event)
      expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
      expect(event.correlation.protocolOperationId).toBeDefined()
    }
    await router.route(encodeMessage(MESSAGE_KIND.catalogResult, operation.id, BODY))
    expect((await operation.next()).kind).toBe(MESSAGE_KIND.catalogResult)
    router.terminate(new Error('done'))
    expect(vi.getTimerCount()).toBe(0)
  })

  it('reserves concurrent admissions atomically and cancels waiters without creating operations', async () => {
    const router = new OperationRouter(() => undefined)
    const active = Array.from({ length: MAXIMUM_ACTIVE_OPERATIONS }, () =>
      router.create(MESSAGE_KIND.listChildren, BODY))
    const controller = new AbortController()

    const cancelled = router.admit(MESSAGE_KIND.listChildren, BODY, controller.signal)
    const first = router.admit(MESSAGE_KIND.listChildren, BODY)
    let secondAdmitted = false
    const second = router.admit(MESSAGE_KIND.listChildren, BODY)
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
    // Abandoned admission cannot consume active or retained capacity.
    expect(router.create(MESSAGE_KIND.listChildren, BODY)).toBeDefined()
    router.terminate(new Error('done'))
  })

  it('settles pending admissions on session termination and rejects pre-aborted requests', async () => {
    const router = new OperationRouter(() => undefined)
    for (let index = 0; index < MAXIMUM_ACTIVE_OPERATIONS; index += 1) {
      router.create(MESSAGE_KIND.listChildren, BODY)
    }
    const pending = router.admit(MESSAGE_KIND.listChildren, BODY)
    router.terminate(new Error('session closed'))
    await expect(pending).rejects.toMatchObject({ scope: 'session' })
    const controller = new AbortController()
    controller.abort(new Error('already cancelled'))
    await expect(router.admit(MESSAGE_KIND.listChildren, BODY, controller.signal))
      .rejects.toThrow('already cancelled')
  })
})
