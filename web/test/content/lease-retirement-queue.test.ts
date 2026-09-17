import { afterEach, expect, it, vi } from 'vitest'
import {
  LeaseRetirementQueue, MAXIMUM_ACTIVE_LEASE_RETIREMENTS, MAXIMUM_QUEUED_LEASE_RETIREMENTS,
  type LeaseRetirementObservation,
} from '../../src/content/scheduling/lease-retirement'
import { deferred } from '../session/v2-send-fixture'

afterEach(() => vi.useRealTimers())

it('bounds detached cleanup, admits queued releases in order, and closes all ownership', async () => {
  vi.useFakeTimers()
  const lifetime = new AbortController()
  const events: LeaseRetirementObservation[] = []
  const queue = new LeaseRetirementQueue({ signal: lifetime.signal, observe: event => events.push(event) })
  const running = new Map<number, ReturnType<typeof deferred<void>>>()
  const started: number[] = []
  const capacity = MAXIMUM_ACTIVE_LEASE_RETIREMENTS + MAXIMUM_QUEUED_LEASE_RETIREMENTS
  for (let index = 0; index <= capacity; index += 1) {
    queue.retire(String(index), async signal => {
      started.push(index)
      const response = deferred<void>()
      running.set(index, response)
      const abort = () => response.reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
      try { await response.promise } finally { signal.removeEventListener('abort', abort) }
    })
  }
  await vi.advanceTimersByTimeAsync(0)
  expect(started).toHaveLength(MAXIMUM_ACTIVE_LEASE_RETIREMENTS)
  expect(events).toEqual([expect.objectContaining({
    leaseId: String(capacity), transition: 'abandoned', reason: 'capacity', attempt: 0,
  })])
  running.get(0)!.resolve()
  await vi.advanceTimersByTimeAsync(0)
  expect(started.at(-1)).toBe(MAXIMUM_ACTIVE_LEASE_RETIREMENTS)
  expect(events).toContainEqual({ leaseId: '0', transition: 'released', attempt: 1 })
  lifetime.abort(new DOMException('Generation closed', 'AbortError'))
  await vi.advanceTimersByTimeAsync(0)
  expect(started).toHaveLength(MAXIMUM_ACTIVE_LEASE_RETIREMENTS + 1)
  expect(events.filter(event => event.transition === 'released' || event.transition === 'abandoned'))
    .toHaveLength(capacity + 1)
  expect(vi.getTimerCount()).toBe(0)
})

it('does not start remote work after the session owner closes', () => {
  const lifetime = new AbortController()
  lifetime.abort(new DOMException('Generation closed', 'AbortError'))
  const events: LeaseRetirementObservation[] = []
  const queue = new LeaseRetirementQueue({ signal: lifetime.signal, observe: event => events.push(event) })
  const release = vi.fn(async () => undefined)
  queue.retire('lease', release)
  expect(release).not.toHaveBeenCalled()
  expect(events).toEqual([expect.objectContaining({ transition: 'abandoned', reason: 'service_closed', attempt: 0 })])
})
