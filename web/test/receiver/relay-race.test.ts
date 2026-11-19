import { describe, expect, it, vi } from 'vitest'
import { firstUsableRelay, RelayEndpointFailure } from '../../src/receiver/relay-selection/race'
import { StaleShareInstanceError } from '../../src/receiver/session-factory'
import { isShareRecoveryFailure, isTerminalRecoveryFailure } from '../../src/receiver/recovery-failure'
import { receiverRelayBases, RelayCandidates } from '../../src/receiver/relay-selection/candidates'
import { RelayRecovery } from '../../src/receiver/relay-selection/recovery'
import { RelayPolicyDeniedError } from '../../src/receiver/relay-admission'
import { RelayProtocolError } from '../../src/transport/relay/protocol'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => { resolve = accept })
  return { promise, resolve }
}

describe('first usable relay', () => {
  it('does not delay known paths for an unknown directory alias', async () => {
    const expansion = deferred<readonly string[]>()
    const source = new RelayCandidates(['known'], expansion.promise)
    const connect = vi.fn(async (endpoint: string) => endpoint)
    expect(await firstUsableRelay(source, new AbortController().signal, connect, async () => undefined)).toBe('known')
    expansion.resolve(['later'])
    await source.settled
    expect(connect).toHaveBeenCalledTimes(1)
  })
  it('admits a newly resolved alias after the explicit endpoint fails', async () => {
    const expansion = deferred<readonly string[]>()
    const source = new RelayCandidates(['unavailable'], expansion.promise)
    const result = firstUsableRelay(source, new AbortController().signal, async endpoint => {
      if (endpoint === 'unavailable') throw new Error('node disabled')
      return endpoint
    }, async () => undefined)
    expansion.resolve(['resolved'])
    await expect(result).resolves.toBe('resolved')
  })
  it('rejects authenticated identity failures beside a stalled endpoint and disposes its late result', async () => {
    const slow = deferred<string>()
    const failure = new StaleShareInstanceError('Authenticated descriptor changed')
    const close = vi.fn(async () => undefined)
    let stalledSignal: AbortSignal | undefined
    const task = firstUsableRelay(['stalled', 'invalid'], new AbortController().signal,
      async (endpoint, signal) => {
        if (endpoint === 'invalid') throw failure
        stalledSignal = signal
        return slow.promise
      }, close, isShareRecoveryFailure)
    await expect(task).rejects.toMatchObject({
      name: RelayEndpointFailure.name, relayBase: 'invalid', cause: failure,
    })
    expect(stalledSignal?.aborted).toBe(true)
    slow.resolve('late-session')
    await vi.waitFor(() => expect(close).toHaveBeenCalledWith('late-session'))
  })

  it('returns the first authenticated result without waiting for earlier slow endpoints and closes late losers', async () => {
    const slow = deferred<string>()
    const aborted: AbortSignal[] = []
    const close = vi.fn(async () => undefined)
    const value = await firstUsableRelay(['slow', 'fast'], new AbortController().signal,
      async (endpoint, signal) => {
        if (endpoint === 'slow') { aborted.push(signal); return slow.promise }
        return 'fast-connected'
      }, close)
    expect(value).toBe('fast-connected')
    expect(aborted[0]?.aborted).toBe(true)
    expect(close).not.toHaveBeenCalled()
    slow.resolve('slow-connected')
    await slow.promise
    await Promise.resolve()
    await Promise.resolve()
    expect(close).toHaveBeenCalledWith('slow-connected')
  })

  it('keeps a cooling endpoint eligible for future recovery after the remaining endpoint is rejected', async () => {
    let now = 0
    const recovery = new RelayRecovery(['official', 'independent'], { now: () => now })
    const signal = new AbortController().signal
    const close = async () => undefined
    const connect = vi.fn(async (base: string) => {
      if (base === 'independent') {
        if (now === 0) throw new Error('Independent relay temporarily unavailable')
        throw new RelayProtocolError('malformed', 'Invalid relay response')
      }
      if (now < 60_000) throw new RelayPolicyDeniedError()
      return base
    })
    const outcome = await recovery.race(signal, connect, close).catch((failure: unknown) => failure)
    expect(isTerminalRecoveryFailure(outcome)).toBe(false)
    expect(recovery.nextRetry()).toBeUndefined()

    now = 500
    const nextFailure = await recovery.race(signal, connect, close).catch((failure: unknown) => failure)
    expect(isTerminalRecoveryFailure(nextFailure)).toBe(false)
    expect(recovery.nextRetry()).toEqual({ retryAt: 60_000, reason: 'authorization' })
    expect(connect.mock.calls.map(([base]) => base)).toEqual(['official', 'independent', 'independent'])

    now = 60_000
    await expect(recovery.race(signal, connect, close)).resolves.toBe('official')
    expect(connect.mock.calls.map(([base]) => base)).toEqual(['official', 'independent', 'independent', 'official'])
  })

  it('allows a failed endpoint without delaying a healthy one and bounds configuration fanout', async () => {
    expect(receiverRelayBases([' first ', 'first', 'second'])).toEqual(['first', 'second'])
    expect(() => receiverRelayBases([])).toThrow(RangeError)
    expect(() => receiverRelayBases(Array.from({ length: 9 }, (_, index) => String(index)))).toThrow(RangeError)
    await expect(firstUsableRelay(['bad', 'good'], new AbortController().signal,
      async (endpoint) => {
        if (endpoint === 'bad') throw new Error('unavailable')
        return endpoint
      },
      async () => undefined)).resolves.toBe('good')
  })
})
