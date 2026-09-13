import { afterEach, describe, expect, it, vi } from 'vitest'
import { InitialJoinControl, InitialJoinUnavailableError, runInitialJoin } from '../../src/receiver/initial-join'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../../src/transport/relay/v2-protocol'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { RecoveryWake } from '../../src/receiver/recovery-wake'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'

afterEach(() => vi.useRealTimers())

const unavailable = () => new V2RelayReceiverError('not found', { relayError: {
  code: V2_RELAY_ERROR.notFound, retryAfterMilliseconds: 0,
} })

function advancingClock() {
  let now = 0
  return { now: () => now, sleep: async (milliseconds: number, signal: AbortSignal) => {
    signal.throwIfAborted()
    now += milliseconds
  } }
}

describe('initial receiver waiting', () => {
  it('retries NotFound inside a finite window and reports temporary unavailability', async () => {
    const connect = vi.fn(async () => { throw unavailable() })
    await expect(runInitialJoin({ signal: new AbortController().signal, clock: advancingClock(),
      windowMilliseconds: 1_000, connect, close: async () => undefined,
    })).rejects.toBeInstanceOf(InitialJoinUnavailableError)
    expect(connect.mock.calls.length).toBeGreaterThan(1)
    expect(connect.mock.calls.length).toBeLessThan(8)
  })

  it('holds one join until explicit continued waiting and recovers without reacquiring input', async () => {
    const control = new InitialJoinControl()
    let continuing = false
    let attemptsAfterChoice = 0
    const states: string[] = []
    const result = await runInitialJoin({ signal: new AbortController().signal, clock: advancingClock(),
      windowMilliseconds: 1_000, control, close: async () => undefined,
      connect: async () => {
        if (!continuing || ++attemptsAfterChoice < 3) throw unavailable()
        return 'authenticated-session'
      },
      onState: state => {
        states.push(state)
        if (state === 'waiting-for-choice') {
          continuing = true
          control.request('continue')
        }
      },
    })
    expect(result).toBe('authenticated-session')
    expect(states.filter(state => state === 'waiting-for-choice')).toHaveLength(1)
    expect(states).toContain('waiting-for-sender')
  })

  it('cancels paused join ownership without another dial', async () => {
    const controller = new AbortController()
    const control = new InitialJoinControl()
    const connect = vi.fn(async () => { throw unavailable() })
    const reason = new DOMException('Cancelled by user', 'AbortError')
    const task = runInitialJoin({ signal: controller.signal, clock: advancingClock(), windowMilliseconds: 100,
      connect, close: async () => undefined, control,
      onState: state => { if (state === 'waiting-for-choice') controller.abort(reason) },
    })
    await expect(task).rejects.toBe(reason)
    const calls = connect.mock.calls.length
    control.request('retry')
    expect(connect).toHaveBeenCalledTimes(calls)
  })

  it('rejects descriptor authentication failures immediately', async () => {
    const failure = new SenderObjectError('signature', 'Invalid sender signature')
    const connect = vi.fn(async () => { throw failure })
    await expect(runInitialJoin({ signal: new AbortController().signal, clock: advancingClock(),
      connect, close: async () => undefined,
    })).rejects.toBe(failure)
    expect(connect).toHaveBeenCalledTimes(1)
  })

  it('bounds an unresponsive complete handshake and disposes late success', async () => {
    vi.useFakeTimers()
    let resolve!: (value: string) => void
    const work = new Promise<string>(accepted => { resolve = accepted })
    const close = vi.fn(async () => undefined)
    const task = runInitialJoin({ signal: new AbortController().signal,
      clock: { now: () => Date.now(), sleep: systemReconnectClock.sleep },
      windowMilliseconds: 1_000, connect: async () => work, close,
    })
    const result = expect(task).rejects.toBeInstanceOf(InitialJoinUnavailableError)
    await vi.advanceTimersByTimeAsync(1_001)
    await result
    resolve('late')
    await vi.advanceTimersByTimeAsync(0)
    expect(close).toHaveBeenCalledWith('late')
    expect(vi.getTimerCount()).toBe(0)
  })
})

it('coalesces retry requests for existing waits and releases delay timers on cancellation', async () => {
  vi.useFakeTimers()
  const wake = new RecoveryWake()
  const lifetime = new AbortController()
  const waiting = wake.sleep(systemReconnectClock, 30_000, lifetime.signal)
  wake.request()
  wake.request()
  await waiting
  expect(vi.getTimerCount()).toBe(0)
  const next = wake.sleep(systemReconnectClock, 30_000, lifetime.signal)
  const stopped = expect(next).rejects.toMatchObject({ name: 'AbortError' })
  lifetime.abort()
  await stopped
  expect(vi.getTimerCount()).toBe(0)
})
