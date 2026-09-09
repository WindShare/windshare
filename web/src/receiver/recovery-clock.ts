const V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS = 100
const V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS = 5_000

export interface V2ReconnectClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}

export function requireBackoff(milliseconds: number): number {
  if (!Number.isFinite(milliseconds) || milliseconds < 0 ||
      milliseconds > V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS) {
    throw new RangeError('Receiver reconnect backoff is outside its bounded range')
  }
  return milliseconds
}

export function defaultReconnectBackoff(attempt: number): number {
  return Math.min(
    V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS * 2 ** Math.min(attempt, 8),
    V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS,
  )
}

export const systemReconnectClock: V2ReconnectClock = Object.freeze({
  now: () => performance.now(),
  sleep: (milliseconds: number, signal: AbortSignal) => abortableDelay(milliseconds, signal),
})

function abortableDelay(milliseconds: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, milliseconds)
    const abort = () => {
      globalThis.clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Reconnect delay aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}
