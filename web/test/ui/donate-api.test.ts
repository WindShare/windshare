import { afterEach, describe, expect, it, vi } from 'vitest'
import { createCryptoIntent, fetchDonationWall } from '../../src/ui/donate/api'

describe('donate API request signal composition', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('aborts createCryptoIntent when either the caller signal or timeout fires', async () => {
    const timeoutController = new AbortController()
    const timeoutSpy = vi.spyOn(AbortSignal, 'timeout').mockReturnValue(timeoutController.signal)
    let capturedSignal: AbortSignal | undefined
    vi.spyOn(globalThis, 'fetch').mockImplementation((_input, init) => {
      capturedSignal = init?.signal ?? undefined
      return new Promise<Response>((_resolve, reject) => {
        capturedSignal?.addEventListener('abort', () => {
          reject(capturedSignal?.reason ?? new DOMException('Aborted', 'AbortError'))
        }, { once: true })
      })
    })

    const caller = new AbortController()
    const pending = createCryptoIntent(
      { displayName: null, message: null, amountUsd: '10' },
      caller.signal,
    )
    expect(timeoutSpy).toHaveBeenCalledWith(10_000)
    expect(capturedSignal).toBeDefined()
    expect(capturedSignal?.aborted).toBe(false)

    // Firing the timeout aborts the combined fetch signal without marking the caller cancelled.
    timeoutController.abort(new DOMException('Timed out', 'TimeoutError'))
    await expect(pending).rejects.toThrow('Timed out')
    expect(capturedSignal?.aborted).toBe(true)
    expect(caller.signal.aborted).toBe(false)
  })

  it('aborts createCryptoIntent and fetchDonationWall immediately when caller cancels', async () => {
    const timeoutController = new AbortController()
    vi.spyOn(AbortSignal, 'timeout').mockReturnValue(timeoutController.signal)
    const capturedSignals: Array<AbortSignal | undefined> = []
    vi.spyOn(globalThis, 'fetch').mockImplementation((_input, init) => {
      const signal = init?.signal ?? undefined
      capturedSignals.push(signal)
      return new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener('abort', () => {
          reject(signal.reason ?? new DOMException('Aborted', 'AbortError'))
        }, { once: true })
      })
    })

    const intentCaller = new AbortController()
    const intentPending = createCryptoIntent(
      { displayName: 'Alice', message: 'Thanks', amountUsd: '25' },
      intentCaller.signal,
    )
    intentCaller.abort(new DOMException('Unmounted', 'AbortError'))
    await expect(intentPending).rejects.toThrow('Unmounted')
    expect(capturedSignals[0]?.aborted).toBe(true)

    const wallCaller = new AbortController()
    const wallPending = fetchDonationWall({ signal: wallCaller.signal })
    wallCaller.abort(new DOMException('Cancelled', 'AbortError'))
    await expect(wallPending).rejects.toThrow('Cancelled')
    expect(capturedSignals[1]?.aborted).toBe(true)
  })
})
