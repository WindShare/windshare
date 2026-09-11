import { HEDGE_CHECK_MILLISECONDS } from './exploration'

export class ContentRaceWon extends Error {
  constructor() { super('Another authenticated content attempt won') }
}
export class ContentRaceFailures extends AggregateError {
  constructor(errors: readonly unknown[]) { super(errors, 'Content race failed') }
}

/** Duplicate work is reserved for blocked output, never for normal lane exploration. */
export function raceContent<T>(
  signal: AbortSignal,
  primary: (signal: AbortSignal) => Promise<T>,
  rescue: (signal: AbortSignal) => Promise<T> | undefined,
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
      finishFailure()
    }
    const abort = () => fail(signal.reason)
    const start = (promise: Promise<T>) => {
      active += 1
      promise.then(result => {
        active -= 1
        if (settled) { finishFailure(); return }
        settled = true
        cleanup()
        controller.abort(new ContentRaceWon())
        resolve(result)
      }, (error: unknown) => {
        active -= 1
        if (settled) { finishFailure(); return }
        if (!retryable(error)) { fail(error); return }
        errors.push(error)
        if (active === 0) fail(new ContentRaceFailures(errors))
      })
    }
    signal.addEventListener('abort', abort, { once: true })
    try {
      start(primary(controller.signal))
      timer = setInterval(() => {
        if (settled || supplemented) return
        try {
          const promise = rescue(controller.signal)
          if (promise === undefined) return
          supplemented = true
          clearInterval(timer)
          start(promise)
        } catch (error) { fail(error) }
      }, HEDGE_CHECK_MILLISECONDS)
    } catch (error) { fail(error) }
  })
}
