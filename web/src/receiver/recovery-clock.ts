const V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS = 100
const V2_RECONNECT_MAXIMUM_BACKOFF_MILLISECONDS = 5_000
export const RECOVERY_FAST_WINDOW_MILLISECONDS = 55_000
export const RECOVERY_FAST_ATTEMPTS = 8
const RECOVERY_WAIT_MILLISECONDS = 30_000
const RECOVERY_MAXIMUM_WAIT_MILLISECONDS = 60_000
const GENERATION_RECONNECT_BACKOFF_GROWTH = 3
const RECOVERY_JITTER_MINIMUM = 0.8
const RECOVERY_JITTER_SPREAD = 0.4

export interface V2ReconnectClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}

export function requireBackoff(milliseconds: number): number {
  if (!Number.isFinite(milliseconds) || milliseconds < 0 ||
      milliseconds > RECOVERY_MAXIMUM_WAIT_MILLISECONDS) {
    throw new RangeError('Receiver reconnect backoff is outside its bounded range')
  }
  return milliseconds
}

export function waitingReconnectBackoff(random: () => number = Math.random): number {
  return jitteredBackoff(RECOVERY_WAIT_MILLISECONDS, random)
}

export function generationReconnectBackoff(attempt: number, random: () => number = Math.random): number {
  // The shared ledger refills only one attempt every 75 seconds. Spread its
  // initial capacity across roughly a minute instead of exhausting it in a burst.
  // Keep the first retry prompt; threefold growth then preserves later probes.
  return jitteredBackoff(Math.min(
    V2_RECONNECT_INITIAL_BACKOFF_MILLISECONDS * GENERATION_RECONNECT_BACKOFF_GROWTH **
      Math.min(attempt, RECOVERY_FAST_ATTEMPTS),
    RECOVERY_WAIT_MILLISECONDS,
  ), random)
}

function jitteredBackoff(milliseconds: number, random: () => number): number {
  return milliseconds * (RECOVERY_JITTER_MINIMUM + Math.max(0, Math.min(1, random())) * RECOVERY_JITTER_SPREAD)
}

export function reconnectPhase(attempt: number, elapsed: number): 'fast' | 'waiting' {
  return attempt >= RECOVERY_FAST_ATTEMPTS || elapsed >= RECOVERY_FAST_WINDOW_MILLISECONDS ? 'waiting' : 'fast'
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
