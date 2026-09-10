import { HEDGE_CHECK_MILLISECONDS, type DispatchPurpose } from './exploration'

export class ContentRaceWon extends Error {
  constructor() { super('Another authenticated content attempt won') }
}

export class ContentRaceFailures extends AggregateError {
  constructor(errors: readonly unknown[]) { super(errors, 'Content race failed') }
}

/** Only a live demand owns a timer. Losers retain their budget until they settle. */
export function raceContent<T>(
  signal: AbortSignal,
  primary: (signal: AbortSignal) => Promise<T>,
  supplement: (signal: AbortSignal, purpose: 'probe' | 'rescue') => Promise<T> | undefined,
  retryable: (error: unknown) => boolean,
): Promise<T> {
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const controller = new AbortController()
    const errors: unknown[] = []
    let active = 0
    let settled = false
    let supplemented = false
    let pendingFailure: { reason: unknown } | undefined
    let timer: ReturnType<typeof setInterval> | undefined
    const cleanup = () => {
      if (timer !== undefined) clearInterval(timer)
      signal.removeEventListener('abort', abort)
    }
    const finishFailure = () => {
      if (active === 0 && pendingFailure !== undefined) reject(pendingFailure.reason)
    }
    const fail = (reason: unknown) => {
      if (settled) return
      settled = true
      cleanup()
      pendingFailure = { reason }
      controller.abort(reason)
      // Broker admission stays occupied until canceled primary work returns.
      finishFailure()
    }
    const abort = () => fail(signal.reason)
    const start = (promise: Promise<T>, purpose: DispatchPurpose) => {
      active += 1
      promise.then((result) => {
        active -= 1
        if (settled) { finishFailure(); return }
        settled = true
        cleanup()
        controller.abort(new ContentRaceWon())
        resolve(result)
      }, (error: unknown) => {
        active -= 1
        if (settled) { finishFailure(); return }
        // Optional exploration cannot turn healthy primary content into a failure.
        if (purpose === 'content' && !retryable(error)) { fail(error); return }
        errors.push(error)
        if (active === 0) fail(new ContentRaceFailures(errors))
      })
    }
    const explore = (purpose: 'probe' | 'rescue') => {
      if (settled || supplemented) return
      try {
        const promise = supplement(controller.signal, purpose)
        if (promise === undefined) return
        supplemented = true
        if (timer !== undefined) clearInterval(timer)
        start(promise, purpose)
      } catch (error) { fail(error) }
    }
    signal.addEventListener('abort', abort, { once: true })
    try {
      start(primary(controller.signal), 'content')
      explore('probe')
      if (!settled && !supplemented) {
        timer = setInterval(() => explore('rescue'), HEDGE_CHECK_MILLISECONDS)
      }
    } catch (error) { fail(error) }
  })
}
