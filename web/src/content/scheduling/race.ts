import { OperationCancellationError, OPERATION_CANCEL_REASON } from '../../session/runtime-types'
import { RESCUE_POLL_MILLISECONDS } from './exploration'

export class ContentRaceWon extends OperationCancellationError {
  constructor() { super(OPERATION_CANCEL_REASON.laneRace, 'Another authenticated content attempt won') }
}
export class ContentRaceFailures extends AggregateError {
  constructor(errors: readonly unknown[]) { super(errors, 'Content race failed') }
}

/** Duplicate work is reserved for blocked output, never for normal lane exploration. */
export function raceContent<T>(
  signal: AbortSignal,
  primary: (signal: AbortSignal) => Promise<T>,
  rescue: (signal: AbortSignal) => Promise<() => Promise<T>> | undefined,
  retryable: (error: unknown) => boolean,
): Promise<T> {
  signal.throwIfAborted()
  return new Promise<T>((resolve, reject) => {
    const controller = new AbortController()
    const errors: unknown[] = []
    let active = 0
    let awaitingAdmission = false
    let settled = false
    let supplemented = false
    let pendingFailure: { reason: unknown } | undefined
    let timer: ReturnType<typeof setInterval> | undefined
    const cleanup = () => {
      if (timer !== undefined) clearInterval(timer)
      signal.removeEventListener('abort', abort)
    }
    const finishFailure = () => {
      if (active === 0 && !awaitingAdmission && pendingFailure !== undefined) reject(pendingFailure.reason)
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
          // A queued rescue owns no live attempt. A failed primary must cancel
          // that intent and retry, even if its old progress no longer justifies a hedge.
          awaitingAdmission = true
          promise.then(attempt => {
            awaitingAdmission = false
            // An admitted reservation still needs settlement if cancellation won
            // the microtask race; the attempt observes the already-aborted signal.
            try { start(attempt()) } catch (error) { fail(error); finishFailure() }
          }, (error: unknown) => {
            awaitingAdmission = false
            if (settled) { finishFailure(); return }
            fail(error)
          })
        } catch (error) { fail(error) }
      }, RESCUE_POLL_MILLISECONDS)
    } catch (error) { fail(error) }
  })
}
