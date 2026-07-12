import { WebRTCChannelClosedError } from './errors'

interface LockWaiter {
  readonly resolve: (release: () => void) => void
  readonly reject: (reason?: unknown) => void
  readonly signal: AbortSignal | undefined
  readonly aborted: () => void
}

export class SendLock {
  readonly #waiters: LockWaiter[] = []
  #held = false
  #closedReason: unknown

  acquire(signal?: AbortSignal): Promise<() => void> {
    throwIfAborted(signal)
    if (this.#closedReason !== undefined) {
      return Promise.reject(this.#closedReason)
    }
    if (!this.#held) {
      this.#held = true
      return Promise.resolve(this.#releaseOnce())
    }
    return new Promise<() => void>((resolve, reject) => {
      const waiter: LockWaiter = {
        resolve,
        reject,
        signal,
        aborted: () => {
          const index = this.#waiters.indexOf(waiter)
          if (index >= 0) {
            this.#waiters.splice(index, 1)
          }
          reject(abortReason(signal))
        },
      }
      this.#waiters.push(waiter)
      signal?.addEventListener('abort', waiter.aborted, { once: true })
    })
  }

  shutdown(reason: unknown = new WebRTCChannelClosedError()): void {
    if (this.#closedReason !== undefined) {
      return
    }
    this.#closedReason = reason
    const waiters = this.#waiters.splice(0)
    for (const waiter of waiters) {
      waiter.signal?.removeEventListener('abort', waiter.aborted)
      waiter.reject(reason)
    }
  }

  #releaseOnce(): () => void {
    let released = false
    return () => {
      if (released) {
        return
      }
      released = true
      this.#release()
    }
  }

  #release(): void {
    while (this.#waiters.length > 0) {
      const waiter = this.#waiters.shift()
      if (waiter === undefined) {
        break
      }
      waiter.signal?.removeEventListener('abort', waiter.aborted)
      if (waiter.signal?.aborted === true) {
        waiter.reject(abortReason(waiter.signal))
        continue
      }
      if (this.#closedReason !== undefined) {
        waiter.reject(this.#closedReason)
        continue
      }
      waiter.resolve(this.#releaseOnce())
      return
    }
    this.#held = false
  }
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted === true) {
    throw abortReason(signal)
  }
}

function abortReason(signal?: AbortSignal): unknown {
  return signal?.reason ?? new DOMException('Operation aborted', 'AbortError')
}
