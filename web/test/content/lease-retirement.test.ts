import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  LEASE_RETIREMENT_WAIT_MILLISECONDS, retireRemoteLease, type LeaseRetirementObservation,
} from '../../src/content/scheduling/lease-retirement'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import { deferred } from '../session/v2-send-fixture'

const LEASE_ID = '01000000000000000000000000000000'
afterEach(() => vi.useRealTimers())

function fixture() {
  const lifetime = new AbortController()
  const events: LeaseRetirementObservation[] = []
  const owner = { signal: lifetime.signal, observe: (event: LeaseRetirementObservation) => events.push(event) }
  return { lifetime, events, owner }
}

describe('bounded remote lease retirement', () => {
  it('bounds repeated physical failures and stops retrying after its deadline', async () => {
    vi.useFakeTimers()
    const f = fixture()
    const release = vi.fn(async () => { throw new V2SessionRuntimeError('lane', 'physical failure') })
    const retiring = retireRemoteLease(LEASE_ID, f.owner, release)
    await vi.advanceTimersByTimeAsync(LEASE_RETIREMENT_WAIT_MILLISECONDS)
    await expect(retiring).resolves.toBeUndefined()
    expect(release.mock.calls.length).toBeGreaterThan(1)
    expect(release.mock.calls.length).toBeLessThan(20)
    expect(f.events.at(-1)).toMatchObject({ transition: 'abandoned', reason: 'deadline', leaseId: LEASE_ID })
    const attempts = release.mock.calls.length
    await vi.advanceTimersByTimeAsync(LEASE_RETIREMENT_WAIT_MILLISECONDS)
    expect(release).toHaveBeenCalledTimes(attempts)
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['retry_backoff', 'request_pending'] as const)('stops %s when the generation closes', async phase => {
    vi.useFakeTimers()
    const f = fixture()
    const reached = deferred<void>()
    const release = vi.fn(async (signal: AbortSignal) => {
      reached.resolve()
      if (phase === 'retry_backoff') throw new V2SessionRuntimeError('lane', 'detached')
      await new Promise<void>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
      })
    })
    const retiring = retireRemoteLease(LEASE_ID, f.owner, release)
    await reached.promise
    f.lifetime.abort(new DOMException('Generation closed', 'AbortError'))
    await expect(retiring).resolves.toBeUndefined()
    expect(f.events.at(-1)).toMatchObject({ transition: 'abandoned', reason: 'service_closed' })
    expect(release).toHaveBeenCalledTimes(1)
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each([
    new V2SessionRuntimeError('session', 'terminal'),
    new Error('Authenticated release completion was invalid'),
  ])('records a non-retryable remote failure without replay or download failure', async failure => {
    const f = fixture()
    const release = vi.fn(async () => { throw failure })
    await expect(retireRemoteLease(LEASE_ID, f.owner, release)).resolves.toBeUndefined()
    expect(release).toHaveBeenCalledTimes(1)
    expect(f.events).toEqual([{ leaseId: LEASE_ID, attempt: 1, transition: 'abandoned',
      reason: 'remote_failure', failure }])
  })

  it('keeps observer exceptions outside reclamation control', async () => {
    const f = fixture()
    const release = vi.fn(async () => undefined)
    await expect(retireRemoteLease(LEASE_ID, {
      signal: f.lifetime.signal, observe: () => { throw new Error('broken observer') },
    }, release)).resolves.toBeUndefined()
    expect(release).toHaveBeenCalledTimes(1)
  })
})
