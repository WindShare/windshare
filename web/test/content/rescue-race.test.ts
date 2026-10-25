import { afterEach, describe, expect, it, vi } from 'vitest'
import { raceContent, ContentRaceFailures } from '../../src/content/scheduling/race'

async function flush() { for (let step = 0; step < 8; step++) await Promise.resolve() }
afterEach(() => vi.useRealTimers())

describe('queued rescue lifetime', () => {
  it('withdraws a queued rescue when the only physical attempt fails', async () => {
    vi.useFakeTimers()
    let failPrimary!: (error: unknown) => void
    let rescueAborted = false
    const result = raceContent(new AbortController().signal,
      () => new Promise((_, reject) => { failPrimary = reject }),
      signal => new Promise((_, reject) => signal.addEventListener('abort', () => {
        rescueAborted = true
        reject(signal.reason)
      }, { once: true })), () => true)
    const rejected = expect(result).rejects.toBeInstanceOf(ContentRaceFailures)
    await vi.advanceTimersByTimeAsync(10)
    failPrimary(new Error('lane failed'))
    await rejected
    expect(rescueAborted).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('settles a just-admitted reservation when the primary wins before dispatch', async () => {
    vi.useFakeTimers()
    let finishPrimary!: (value: string) => void
    let admit!: (attempt: () => Promise<string>) => void
    let rescueSignal!: AbortSignal
    const result = raceContent(new AbortController().signal,
      () => new Promise<string>(resolve => { finishPrimary = resolve }),
      signal => { rescueSignal = signal; return new Promise(resolve => { admit = resolve }) }, () => true)
    await vi.advanceTimersByTimeAsync(10)
    let released = false, fetched = false
    finishPrimary('original')
    admit(async () => {
      try {
        rescueSignal.throwIfAborted()
        fetched = true
        return 'duplicate'
      } finally { released = true }
    })
    await expect(result).resolves.toBe('original')
    await flush()
    expect(released).toBe(true)
    expect(fetched).toBe(false)
    expect(vi.getTimerCount()).toBe(0)
  })
})
