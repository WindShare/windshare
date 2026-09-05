const RECOVERY_ATTEMPT_CAPACITY = 8
const RECOVERY_TIME_CAPACITY_MILLISECONDS = 180_000
const RECOVERY_REFILL_MILLISECONDS = 600_000
const RECOVERY_WAVE_ATTEMPTS = 4
const RECOVERY_WAVE_MILLISECONDS = 120_000
const RECOVERY_ATTEMPT_MILLISECONDS = 45_000

export class GenerationRecoveryExhaustedError extends Error {
  constructor() {
    super('The connection could not recover within its budget. Resume the download when the sender is available.')
    this.name = 'GenerationRecoveryExhaustedError'
  }
}

interface RecoveryReservation {
  readonly milliseconds: number
  finish(completedAt: number): void
}
export interface GenerationRecoveryWave {
  reserve(now: number): RecoveryReservation
}

/** One joined share borrows this ledger across session generations; relay handshakes spend no ICE tokens. */
export class GenerationRecoveryBudget {
  #attempts = RECOVERY_ATTEMPT_CAPACITY
  #milliseconds = RECOVERY_TIME_CAPACITY_MILLISECONDS
  #updatedAt: number | undefined

  openWave(startedAt: number): GenerationRecoveryWave {
    let attempts = 0
    return {
      reserve: (now) => {
        this.#refill(now)
        const milliseconds = Math.min(RECOVERY_ATTEMPT_MILLISECONDS, this.#milliseconds,
          RECOVERY_WAVE_MILLISECONDS - (now - startedAt))
        if (attempts >= RECOVERY_WAVE_ATTEMPTS || this.#attempts < 1 || milliseconds < 1) {
          throw new GenerationRecoveryExhaustedError()
        }
        attempts += 1
        this.#attempts -= 1
        this.#milliseconds -= milliseconds
        let finished = false
        return { milliseconds, finish: (completedAt) => {
          if (finished) return
          finished = true
          this.#refill(completedAt)
          this.#milliseconds = Math.min(RECOVERY_TIME_CAPACITY_MILLISECONDS,
            this.#milliseconds + Math.max(0, milliseconds - (completedAt - now)))
        } }
      },
    }
  }

  #refill(now: number): void {
    if (!Number.isFinite(now) || (this.#updatedAt !== undefined && now < this.#updatedAt)) {
      throw new RangeError('Generation recovery clock must be monotonic')
    }
    const elapsed = this.#updatedAt === undefined ? 0 : now - this.#updatedAt
    this.#updatedAt = now
    this.#attempts = Math.min(RECOVERY_ATTEMPT_CAPACITY,
      this.#attempts + elapsed * RECOVERY_ATTEMPT_CAPACITY / RECOVERY_REFILL_MILLISECONDS)
    this.#milliseconds = Math.min(RECOVERY_TIME_CAPACITY_MILLISECONDS,
      this.#milliseconds + elapsed * RECOVERY_TIME_CAPACITY_MILLISECONDS / RECOVERY_REFILL_MILLISECONDS)
  }
}

export async function runGenerationRecovery<T>(options: {
  readonly reservation: RecoveryReservation
  readonly parent: AbortSignal
  readonly now: () => number
  readonly connect: (signal: AbortSignal) => Promise<T>
  readonly close: (value: T) => Promise<void>
}): Promise<T> {
  const controller = new AbortController()
  const abort = () => controller.abort(options.parent.reason)
  options.parent.addEventListener('abort', abort, { once: true })
  const timer = setTimeout(() => controller.abort(new Error('Connection recovery attempt timed out')),
    options.reservation.milliseconds)
  if (options.parent.aborted) abort()
  try {
    controller.signal.throwIfAborted()
    const interrupted = new Promise<never>((_resolve, reject) => {
      controller.signal.addEventListener('abort', () => reject(controller.signal.reason), { once: true })
    })
    const work = options.connect(controller.signal).then(async (value) => {
      if (controller.signal.aborted) {
        await options.close(value).catch(() => undefined)
        throw controller.signal.reason
      }
      return value
    })
    return await Promise.race([work, interrupted])
  } finally {
    clearTimeout(timer)
    options.parent.removeEventListener('abort', abort)
    options.reservation.finish(options.now())
  }
}
