const MAX_TIMER_DELAY_MS = 2_147_483_647

export class RelayJoinWindowError extends Error {
  constructor(cause: unknown) {
    super('relay join retry window expired', { cause })
    this.name = 'RelayJoinWindowError'
  }
}

export function relayTimerDuration(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0 || value > MAX_TIMER_DELAY_MS) {
    throw new RangeError(`${label} must be an integer in [1, ${MAX_TIMER_DELAY_MS}]`)
  }
  return value
}

export function sleepForRelayRetry(
  milliseconds: number,
  signal: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason)
      return
    }
    const timer = setTimeout(done, milliseconds)
    function done(): void {
      signal.removeEventListener('abort', aborted)
      resolve()
    }
    function aborted(): void {
      clearTimeout(timer)
      signal.removeEventListener('abort', aborted)
      reject(signal.reason)
    }
    signal.addEventListener('abort', aborted, { once: true })
  })
}

export function relayDeadlineSignal(
  milliseconds: number,
  outer?: AbortSignal,
): { readonly signal: AbortSignal; cleanup(): void } {
  const controller = new AbortController()
  const timer = setTimeout(
    () => controller.abort(new RelayJoinWindowError(
      new DOMException('Deadline exceeded', 'TimeoutError'),
    )),
    milliseconds,
  )
  const abort = () => controller.abort(outer?.reason)
  if (outer?.aborted === true) {
    abort()
  } else {
    outer?.addEventListener('abort', abort, { once: true })
  }
  return {
    signal: controller.signal,
    cleanup: () => {
      clearTimeout(timer)
      outer?.removeEventListener('abort', abort)
    },
  }
}
