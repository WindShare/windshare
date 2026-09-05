import { browserConnectivityClock } from '../clock'

export interface PeerTabAdmissionClock {
  now(): number
  sleep(milliseconds: number, signal: AbortSignal): Promise<void>
}
export interface PeerTabAdmissionPolicy {
  readonly concurrentAttempts: number
  readonly startsPerInterval: number
  readonly stunEndpointsPerInterval: number
  readonly intervalMilliseconds: number
  readonly queuedAttempts: number
}
export const DEFAULT_PEER_TAB_ADMISSION_POLICY: PeerTabAdmissionPolicy = Object.freeze({
  concurrentAttempts: 4, startsPerInterval: 16, stunEndpointsPerInterval: 32,
  intervalMilliseconds: 60_000, queuedAttempts: 64,
})
export interface PeerTabAdmissionPermit { release(): void }
export class PeerTabCapacityTimeoutError extends DOMException {
  constructor() { super('Tab peer capacity was unavailable within the attempt start window', 'TimeoutError') }
}
interface WaitingAttempt {
  readonly receiver: object
  readonly endpoints: number
  readonly signal: AbortSignal
  readonly expiresAt: number
  readonly abort: () => void
  readonly resolve: (permit: PeerTabAdmissionPermit) => void
  readonly reject: (reason: unknown) => void
}

/** Tab-wide resource admission has no peer/session failure or cancellation authority. */
export class PeerTabAdmission {
  readonly #policy: PeerTabAdmissionPolicy
  readonly #clock: PeerTabAdmissionClock
  readonly #receivers = new Map<object, WaitingAttempt[]>()
  #lastReceiver: WeakRef<object> | undefined
  #queued = 0
  #active = 0
  #starts: number
  #endpoints: number
  #updatedAt: number
  #timer: AbortController | undefined

  constructor(policy: Partial<PeerTabAdmissionPolicy> = {}, clock: PeerTabAdmissionClock = {
    now: () => performance.now(), sleep: (ms, signal) => browserConnectivityClock.sleep(Math.ceil(ms), signal),
  }) {
    this.#policy = Object.freeze({ ...DEFAULT_PEER_TAB_ADMISSION_POLICY, ...policy })
    if (!Object.values(this.#policy).every((value) => Number.isSafeInteger(value) && value > 0)) {
      throw new RangeError('Tab peer admission limits must be positive integers')
    }
    this.#clock = clock
    this.#updatedAt = clock.now()
    this.#starts = this.#policy.startsPerInterval
    this.#endpoints = this.#policy.stunEndpointsPerInterval
  }

  acquire(receiver: object, endpoints: number, waitMilliseconds: number, signal: AbortSignal): Promise<PeerTabAdmissionPermit> {
    signal.throwIfAborted()
    if (!Number.isInteger(endpoints) || endpoints < 0 || endpoints > 2 ||
      !Number.isFinite(waitMilliseconds) || waitMilliseconds < 0) {
      return Promise.reject(new RangeError('Invalid tab peer admission request'))
    }
    if (this.#queued >= this.#policy.queuedAttempts) return Promise.reject(capacityTimeout())
    return new Promise((resolve, reject) => {
      const pending: WaitingAttempt = {
        receiver, endpoints, signal, expiresAt: this.#clock.now() + waitMilliseconds,
        resolve, reject, abort: () => {
          this.#remove(pending)
          reject(signal.reason)
          this.#drain()
        },
      }
      const queue = this.#receivers.get(receiver) ?? []
      queue.push(pending)
      this.#receivers.set(receiver, queue)
      this.#queued += 1
      signal.addEventListener('abort', pending.abort, { once: true })
      // A zero-wait request may take an immediately available permit. Sampling
      // the clock twice would turn ordinary synchronous call overhead into expiry.
      this.#drain(pending.expiresAt - waitMilliseconds)
    })
  }

  #remove(pending: WaitingAttempt): void {
    const queue = this.#receivers.get(pending.receiver)
    const index = queue?.indexOf(pending) ?? -1
    if (queue === undefined || index < 0) return
    queue.splice(index, 1)
    this.#queued -= 1
    pending.signal.removeEventListener('abort', pending.abort)
    if (queue.length === 0) this.#receivers.delete(pending.receiver)
  }

  #drain(now = this.#clock.now()): void {
    this.#timer?.abort()
    this.#timer = undefined
    const elapsed = Math.max(0, now - this.#updatedAt)
    this.#updatedAt = now
    this.#starts = Math.min(this.#policy.startsPerInterval,
      this.#starts + elapsed * this.#policy.startsPerInterval / this.#policy.intervalMilliseconds)
    this.#endpoints = Math.min(this.#policy.stunEndpointsPerInterval,
      this.#endpoints + elapsed * this.#policy.stunEndpointsPerInterval / this.#policy.intervalMilliseconds)
    for (const queue of this.#receivers.values()) for (const pending of [...queue]) {
      if (pending.expiresAt >= now) continue
      this.#remove(pending)
      pending.reject(capacityTimeout())
    }
    // Rotate receivers, not individual attempts: one receiver cannot fill the
    // waiting list with paths and indefinitely pass a later independent receiver.
    while (this.#queued > 0) {
      const receiver = this.#nextReceiver()
      const pending = this.#receivers.get(receiver)![0]!
      if (pending.expiresAt < now) {
        this.#remove(pending)
        pending.reject(capacityTimeout())
        continue
      }
      if (this.#active >= this.#policy.concurrentAttempts || this.#starts < 1 || this.#endpoints < pending.endpoints) break
      this.#remove(pending)
      this.#lastReceiver = new WeakRef(receiver)
      this.#active += 1
      this.#starts -= 1
      this.#endpoints -= pending.endpoints
      let released = false
      pending.resolve(Object.freeze({ release: () => {
        if (released) return
        released = true
        this.#active -= 1
        this.#drain()
      } }))
    }
    this.#schedule(now)
  }

  #nextReceiver(): object {
    const keys = [...this.#receivers.keys()]
    const previous = keys.indexOf(this.#lastReceiver?.deref() ?? {})
    return keys[(previous + 1) % keys.length]!
  }

  #schedule(now: number): void {
    if (this.#queued === 0) return
    const pending = this.#receivers.get(this.#nextReceiver())![0]!
    const deadline = Math.min(...[...this.#receivers.values()].flatMap((queue) => queue.map((entry) => entry.expiresAt)))
    const refill = this.#active >= this.#policy.concurrentAttempts ? Number.POSITIVE_INFINITY : Math.max(
      (1 - this.#starts) * this.#policy.intervalMilliseconds / this.#policy.startsPerInterval,
      (pending.endpoints - this.#endpoints) * this.#policy.intervalMilliseconds / this.#policy.stunEndpointsPerInterval, 0,
    )
    const timer = new AbortController()
    this.#timer = timer
    this.#clock.sleep(Math.max(1, Math.min(deadline - now, refill)), timer.signal).then(() => {
      if (!timer.signal.aborted) this.#drain()
    }).catch(() => undefined)
  }
}

let tabAdmission: PeerTabAdmission | undefined
export function browserPeerTabAdmission(): PeerTabAdmission {
  tabAdmission ??= new PeerTabAdmission()
  return tabAdmission
}
function capacityTimeout(): PeerTabCapacityTimeoutError {
  return new PeerTabCapacityTimeoutError()
}
