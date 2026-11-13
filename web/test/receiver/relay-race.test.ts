import { describe, expect, it, vi } from 'vitest'
import { firstUsableRelay, receiverRelayBases, RelayEndpointFailure } from '../../src/receiver/relay-race'
import { StaleShareInstanceError } from '../../src/receiver/session-factory'
import { isShareRecoveryFailure } from '../../src/receiver/recovery-failure'
import { RelayCandidates } from '../../src/receiver/relay-candidates'

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
