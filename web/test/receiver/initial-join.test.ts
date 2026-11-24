import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InitialJoinControl, InitialJoinUnavailableError, runInitialJoin, type InitialJoinState } from '../../src/receiver/initial-join'
import { resolveLinkRelays } from '../../src/application/network/resolve'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import { MemoryDirectoryCache } from '../../src/network/directory-cache'
import { NetworkDirectory } from '../../src/network/directory'
import type { NetworkRoot } from '../../src/network/node-directory'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { RecoveryWake } from '../../src/receiver/recovery-wake'
import { RelayRecovery } from '../../src/receiver/relay-selection/recovery'
import { isRelayAccessDenied } from '../../src/receiver/recovery-failure'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'

const directoryVectors = JSON.parse(readFileSync(new URL('../../../testdata/network/directory.json', import.meta.url), 'utf8')) as {
  network: string; now: string; roots: NetworkRoot[]; entries: { name: string; envelope: unknown; valid: boolean }[]
}
function directoryEnvelope(name: string): string {
  return JSON.stringify(directoryVectors.entries.find(entry => entry.name === name)!.envelope)
}

afterEach(() => vi.useRealTimers())

const unavailable = () => new RelayReceiverError('not found', { relayError: {
  code: RELAY_ERROR.notFound, retryAfterMilliseconds: 0,
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
    const states: InitialJoinState[] = []
    const result = await runInitialJoin({ signal: new AbortController().signal, clock: advancingClock(),
      windowMilliseconds: 1_000, control, close: async () => undefined,
      connect: async () => {
        if (!continuing || ++attemptsAfterChoice < 3) throw unavailable()
        return 'authenticated-session'
      },
      onState: state => {
        states.push(state)
        if (state.kind === 'paused') {
          continuing = true
          control.request('continue')
        }
      },
    })
    expect(result).toBe('authenticated-session')
    expect(states.filter(state => state.kind === 'paused')).toHaveLength(1)
    expect(states).toContainEqual({ kind: 'waiting', reason: 'sender' })
  })

  it.each([{ bases: ['one'] }, { bases: ['one', 'two'] }])('ends the initial join immediately when $bases deny access', async ({ bases }) => {
    const clock = advancingClock()
    const relayRecovery = new RelayRecovery(bases, clock)
    const dial = vi.fn(async () => { throw new RelayReceiverError('Access denied', { relayError: {
        code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0,
      } }) })
    const close = async () => undefined
    const error: unknown = await runInitialJoin({
      clock, signal: new AbortController().signal, relayRecovery, close,
      connect: attempt => relayRecovery.race(attempt.signal, dial, close),
    }).catch((cause: unknown) => cause)
    expect(isRelayAccessDenied(error)).toBe(true)
    expect(clock.now()).toBe(0)
    expect(dial).toHaveBeenCalledTimes(bases.length)
  })

  it('cancels paused join ownership without another dial', async () => {
    const controller = new AbortController()
    const control = new InitialJoinControl()
    const connect = vi.fn(async () => { throw unavailable() })
    const reason = new DOMException('Cancelled by user', 'AbortError')
    const task = runInitialJoin({ signal: controller.signal, clock: advancingClock(), windowMilliseconds: 100,
      connect, close: async () => undefined, control,
      onState: state => { if (state.kind === 'paused') controller.abort(reason) },
    })
    await expect(task).rejects.toBe(reason)
    const calls = connect.mock.calls.length
    control.request('retry')
    expect(connect).toHaveBeenCalledTimes(calls)
  })

  it('preserves the node retry deadline when users retry during bandwidth waits', async () => {
    vi.useFakeTimers()
    const control = new InitialJoinControl()
    const connect = vi.fn()
      .mockRejectedValueOnce(new RelayReceiverError('capacity exhausted', { relayError: {
        code: RELAY_ERROR.admission, retryAfterMilliseconds: 30_000,
      } }))
      .mockResolvedValue('authenticated-session')
    const task = runInitialJoin({
      signal: new AbortController().signal,
      clock: { now: () => Date.now(), sleep: systemReconnectClock.sleep },
      windowMilliseconds: 60_000, control, connect, close: async () => undefined,
    })
    await vi.advanceTimersByTimeAsync(0)
    control.request('retry')
    await vi.advanceTimersByTimeAsync(29_999)
    expect(connect).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    await expect(task).resolves.toBe('authenticated-session')
    expect(connect).toHaveBeenCalledTimes(2)
    expect(vi.getTimerCount()).toBe(0)
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

  it('recovers an unknown alias link on explicit retry after offline directory backoff', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const clock = { now: () => Date.now(), sleep: systemReconnectClock.sleep }
    const control = new InitialJoinControl()
    const lifetime = new AbortController()
    const vectorNow = Date.parse(directoryVectors.now)
    let offline = true
    let directoryCalls = 0
    const directory = new NetworkDirectory({
      network: directoryVectors.network,
      roots: directoryVectors.roots,
      mirrors: ['https://mirror.example/directory'],
      now: () => vectorNow,
      monotonicNow: () => Date.now(),
      cache: new MemoryDirectoryCache(),
      fetch: async () => {
        directoryCalls++
        if (offline) throw new Error('offline')
        return new Response(directoryEnvelope('valid'))
      },
    })
    try {
      // Seed failure backoff (60s after two failures) before opening the new-alias link.
      await expect(directory.refresh()).rejects.toThrow('offline')
      expect(directoryCalls).toBe(1)

      const key = await encodeSuite02CapabilityKey(new Uint8Array(16), new Uint8Array(16))
      const candidates = resolveLinkRelays({ ...key, relays: [], nodeAliases: ['jp1'] }, directory)
      const relayRecovery = new RelayRecovery(candidates, clock)
      const states: InitialJoinState[] = []
      const dial = vi.fn(async (relayBase: string) => `connected:${relayBase}`)
      const close = async () => undefined
      let task: Promise<string> | undefined
      try {
        task = runInitialJoin({
          clock, control, signal: lifetime.signal, relayRecovery, close,
          connect: attempt => relayRecovery.race(attempt.signal, dial, close, { explicit: attempt.explicitRetry }),
          onState: state => states.push(state),
        })

        // Opening the link performs one explicit probe; automatic retries inside the 30s window stay in backoff.
        await vi.advanceTimersByTimeAsync(30_000)
        expect(states.at(-1)).toEqual({ kind: 'paused', reason: 'unreachable' })
        expect(directoryCalls).toBe(2)
        expect(dial).not.toHaveBeenCalled()

        // Network recovers while still well inside the 60s directory backoff; clicking Retry must recover immediately.
        offline = false
        control.request('retry')
        await vi.advanceTimersByTimeAsync(0)
        await expect(task).resolves.toBe('connected:https://jp1.example')
        expect(directoryCalls).toBe(3)
        expect(dial).toHaveBeenCalledTimes(1)
      } finally {
        lifetime.abort()
        await task?.catch(() => undefined)
      }
    } finally {
      directory.close()
    }
  })

  it('wakes an active offline initial join on browser online without bypassing server cooldowns', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const online = new EventTarget()
    vi.stubGlobal('addEventListener', online.addEventListener.bind(online))
    vi.stubGlobal('removeEventListener', online.removeEventListener.bind(online))
    const clock = { now: () => Date.now(), sleep: systemReconnectClock.sleep }
    const control = new InitialJoinControl()
    const lifetime = new AbortController()
    const vectorNow = Date.parse(directoryVectors.now)
    const key = await encodeSuite02CapabilityKey(new Uint8Array(16), new Uint8Array(16))
    let offline = true
    let directoryCalls = 0
    const directory = new NetworkDirectory({
      network: directoryVectors.network,
      roots: directoryVectors.roots,
      mirrors: ['https://mirror.example/directory'],
      now: () => vectorNow,
      monotonicNow: () => Date.now(),
      cache: new MemoryDirectoryCache(),
      fetch: async () => {
        directoryCalls++
        if (offline) throw new Error('offline')
        return new Response(directoryEnvelope('valid'))
      },
    })
    let task: Promise<string> | undefined
    let cooledTask: Promise<string> | undefined
    try {
      const candidates = resolveLinkRelays({ ...key, relays: [], nodeAliases: ['jp1'] }, directory)
      const relayRecovery = new RelayRecovery(candidates, clock)
      const dial = vi.fn(async (relayBase: string) => `connected:${relayBase}`)
      const close = async () => undefined
      task = runInitialJoin({
        clock, control, signal: lifetime.signal, relayRecovery, close,
        connect: attempt => relayRecovery.race(attempt.signal, dial, close, { explicit: attempt.explicitRetry }),
      })

      await vi.advanceTimersByTimeAsync(5_000)
      expect(dial).not.toHaveBeenCalled()
      expect(directoryCalls).toBe(1)

      offline = false
      online.dispatchEvent(new Event('online'))
      await vi.advanceTimersByTimeAsync(0)
      await expect(task).resolves.toBe('connected:https://jp1.example')
      expect(directoryCalls).toBe(2)

      // Online events must still honor mandatory relay RetryAfter cooldowns.
      const cooledConnect = vi.fn()
        .mockRejectedValueOnce(new RelayReceiverError('capacity exhausted', { relayError: {
          code: RELAY_ERROR.admission, retryAfterMilliseconds: 30_000,
        } }))
        .mockResolvedValue('after-cooldown')
      cooledTask = runInitialJoin({
        clock, signal: lifetime.signal, windowMilliseconds: 60_000,
        connect: cooledConnect, close: async () => undefined,
      })
      await vi.advanceTimersByTimeAsync(0)
      online.dispatchEvent(new Event('online'))
      await vi.advanceTimersByTimeAsync(29_999)
      expect(cooledConnect).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(1)
      await expect(cooledTask).resolves.toBe('after-cooldown')
    } finally {
      lifetime.abort()
      await Promise.allSettled([task, cooledTask])
      directory.close()
      vi.unstubAllGlobals()
    }
  })
})

it('does not terminate initial join when a known relay stops while another alias is still unresolved', async () => {
  vi.useFakeTimers()
  vi.setSystemTime(0)
  const clock = { now: () => Date.now(), sleep: systemReconnectClock.sleep }
  const control = new InitialJoinControl()
  const lifetime = new AbortController()
  const vectorNow = Date.parse(directoryVectors.now)
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16), new Uint8Array(16))
  let offline = true
  const directory = new NetworkDirectory({
    network: directoryVectors.network,
    roots: directoryVectors.roots,
    mirrors: ['https://mirror.example/directory'],
    now: () => vectorNow,
    monotonicNow: () => Date.now(),
    cache: new MemoryDirectoryCache(),
    fetch: async () => {
      if (offline) throw new Error('offline')
      return new Response(directoryEnvelope('valid'))
    },
  })
  let task: Promise<string> | undefined
  try {
    const candidates = resolveLinkRelays({ ...key, relays: ['https://stopped.example'], nodeAliases: ['jp1'] }, directory)
    const relayRecovery = new RelayRecovery(candidates, clock)
    const states: InitialJoinState[] = []
    const dial = vi.fn(async (relayBase: string) => {
      if (relayBase === 'https://stopped.example') {
        throw new RelayReceiverError('share stopped', {
          relayError: { code: RELAY_ERROR.stopped, retryAfterMilliseconds: 0 },
        })
      }
      return `connected:${relayBase}`
    })
    const close = async () => undefined
    task = runInitialJoin({
      clock, control, signal: lifetime.signal, relayRecovery, close,
      connect: attempt => relayRecovery.race(attempt.signal, dial, close, { explicit: attempt.explicitRetry }),
      onState: state => states.push(state),
    })

    await vi.advanceTimersByTimeAsync(30_000)
    expect(states.at(-1)).toEqual({ kind: 'paused', reason: 'unreachable' })
    expect(dial.mock.calls.map(([base]) => base)).toEqual(['https://stopped.example'])

    offline = false
    control.request('retry')
    await vi.advanceTimersByTimeAsync(0)
    await expect(task).resolves.toBe('connected:https://jp1.example')
    expect(dial.mock.calls.map(([base]) => base)).toEqual(['https://stopped.example', 'https://jp1.example'])
  } finally {
    lifetime.abort()
    await task?.catch(() => undefined)
    directory.close()
  }
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
