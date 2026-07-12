export interface ConnectivityClock {
  sleep(milliseconds: number, signal?: AbortSignal): Promise<void>
}

export class BrowserConnectivityClock implements ConnectivityClock {
  sleep(milliseconds: number, signal?: AbortSignal): Promise<void> {
    if (!Number.isSafeInteger(milliseconds) || milliseconds < 0) {
      return Promise.reject(new RangeError('sleep duration must be a non-negative integer'))
    }
    if (signal?.aborted === true) {
      return Promise.reject(abortReason(signal))
    }
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        signal?.removeEventListener('abort', aborted)
        resolve()
      }, milliseconds)
      const aborted = () => {
        clearTimeout(timer)
        reject(signal === undefined ? abortError() : abortReason(signal))
      }
      signal?.addEventListener('abort', aborted, { once: true })
    })
  }
}

export const browserConnectivityClock: ConnectivityClock = Object.freeze(
  new BrowserConnectivityClock(),
)

export function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? abortError()
}

function abortError(): DOMException {
  return new DOMException('Operation aborted', 'AbortError')
}
