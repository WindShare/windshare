import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  ReceiveLifecycleObservation, sameReceiveLifecycleState,
} from '../../../src/output/workspace/lifecycle/observation'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../../src/output/workspace/state'

const identity = (fill: number, width = 16) => encodeBase64Url(new Uint8Array(width).fill(fill))
const initial = initialReceiveLifecycleState({ operationId: identity(1), receiveIntentDigest: identity(2, 32) })
const receiving = nextReceiveLifecycleState(initial, { kind: 'receiving', activeLeaseId: identity(3) })

describe('operation lifecycle observation', () => {
  it('keeps a current monotonic snapshot without promoting another operation or intent', () => {
    const source = new ReceiveLifecycleObservation(initial)
    const listener = vi.fn()
    const stop = source.subscribe(listener)
    source.publish(receiving)
    source.publish(initial)
    source.publish({ ...receiving })
    source.publish({ ...receiving, operationId: identity(4), generation: 10n })
    source.publish({ ...receiving, receiveIntentDigest: identity(5, 32), generation: 11n })
    expect(source.getSnapshot()).toBe(receiving)
    expect(listener).toHaveBeenCalledExactlyOnceWith(receiving)
    stop()
    source.publish(nextReceiveLifecycleState(receiving, { kind: 'finalizing-tree', activeLeaseId: identity(3) }))
    expect(listener).toHaveBeenCalledOnce()
  })

  it('isolates a throwing observer and releases all listeners on close', () => {
    const source = new ReceiveLifecycleObservation(initial)
    source.subscribe(() => { throw new Error('Failed view') })
    const listener = vi.fn()
    source.subscribe(listener)
    expect(() => source.publish(receiving)).not.toThrow()
    expect(listener).toHaveBeenCalledExactlyOnceWith(receiving)
    source.close()
    source.publish(nextReceiveLifecycleState(receiving, { kind: 'finalizing-tree', activeLeaseId: identity(3) }))
    expect(listener).toHaveBeenCalledOnce()
  })

  it('accepts equivalent settlement snapshots but rejects same-generation changes', () => {
    if (receiving.kind !== 'receiving') throw new Error('Expected receiving state')
    expect(sameReceiveLifecycleState(receiving, { ...receiving })).toBe(true)
    expect(sameReceiveLifecycleState(receiving, { ...receiving, activeLeaseId: identity(4) })).toBe(false)
    expect(sameReceiveLifecycleState(receiving, { ...receiving, timing: { startedAtMilliseconds: 1 } })).toBe(false)
  })
})
