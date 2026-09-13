import { describe, expect, it, vi } from 'vitest'
import { firstUsableRelay, receiverRelayBases, RelayEndpointFailure } from '../../src/receiver/relay-race'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { isShareRecoveryFailure } from '../../src/receiver/recovery-failure'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => { resolve = accept })
  return { promise, resolve }
}

describe('first usable relay', () => {
  it('rejects authenticated identity failures beside a stalled endpoint and disposes its late result', async () => {
    const slow = deferred<string>()
    const failure = new SenderObjectError('signature', 'Invalid sender signature')
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
