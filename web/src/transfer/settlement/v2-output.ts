import type { OutputSession, V2OutputAuthority } from '../output-session'

export const V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 30_000
export const V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 5 * 60_000

export interface V2OutputSettlementFailure {
  readonly requested: 'retain' | 'discard'
  readonly retentionFailure?: unknown
  readonly cleanupFailure: unknown
}

export type V2OutputSettlement =
  | { readonly kind: 'Closed' }
  | { readonly kind: 'Retained' }
  | { readonly kind: 'Discarded'; readonly retentionFailure?: unknown }
  | { readonly kind: 'NeedsAttention'; readonly failure: V2OutputSettlementFailure }

export const V2_CLOSED_OUTPUT_SETTLEMENT: V2OutputSettlement = Object.freeze({ kind: 'Closed' })

export interface V2OutputSettlementClock {
  now(): number
}

export class V2OutputSettlementTimeoutError extends Error {
  readonly operation: string

  constructor(operation: string) {
    super(`Output settlement timed out while attempting to ${operation}`)
    this.name = 'V2OutputSettlementTimeoutError'
    this.operation = operation
  }
}

export function outputSettlementTimeoutMilliseconds(value: number | undefined): number {
  const timeout = value ?? V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS
  if (!Number.isSafeInteger(timeout) || timeout <= 0 ||
      timeout > V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS) {
    throw new RangeError('Output settlement timeout exceeds its finite policy bound')
  }
  return timeout
}

export async function withOutputSettlementTimeout<T>(
  operation: string,
  timeoutMilliseconds: number,
  settle: () => Promise<T>,
  clock?: V2OutputSettlementClock,
): Promise<T> {
  return settleWithin(operation, new SettlementBudget(timeoutMilliseconds, clock), settle)
}

export async function settleFailedV2Output(options: {
  readonly output?: OutputSession
  readonly authority: V2OutputAuthority
  readonly reason: unknown
  readonly preferRetention: boolean
  readonly timeoutMilliseconds: number
  readonly clock?: V2OutputSettlementClock
}): Promise<V2OutputSettlement> {
  const budget = new SettlementBudget(options.timeoutMilliseconds, options.clock)
  let retentionFailure: unknown
  if (options.preferRetention && options.output?.suspendJob !== undefined) {
    try {
      await settleWithin('retain output checkpoints', budget, () => options.output!.suspendJob!(options.reason))
      return Object.freeze({ kind: 'Retained' })
    } catch (error) {
      retentionFailure = error
      // A timed-out collaborator still owns an unresolved mutation. Starting a
      // concurrent cleanup would make namespace state ambiguous rather than safe.
      if (error instanceof V2OutputSettlementTimeoutError) {
        return needsAttention('retain', retentionFailure, error)
      }
    }
  }

  try {
    if (options.output === undefined) {
      await settleWithin('discard unopened output', budget, () => options.authority.abort(options.reason))
    } else {
      await settleWithin('discard opened output', budget, () => options.output!.abortJob(options.reason))
    }
    return Object.freeze({
      kind: 'Discarded',
      ...(retentionFailure === undefined ? {} : { retentionFailure }),
    })
  } catch (cleanupFailure) {
    return needsAttention(options.preferRetention ? 'retain' : 'discard', retentionFailure, cleanupFailure)
  }
}

function needsAttention(
  requested: V2OutputSettlementFailure['requested'],
  retentionFailure: unknown,
  cleanupFailure: unknown,
): V2OutputSettlement {
  return Object.freeze({
    kind: 'NeedsAttention',
    failure: Object.freeze({
      requested,
      ...(retentionFailure === undefined ? {} : { retentionFailure }),
      cleanupFailure,
    }),
  })
}

class SettlementBudget {
  readonly #clock: V2OutputSettlementClock
  #remainingMilliseconds: number
  #latestReading: number

  constructor(timeoutMilliseconds: number, clock: V2OutputSettlementClock = MONOTONIC_CLOCK) {
    this.#clock = clock
    this.#remainingMilliseconds = timeoutMilliseconds
    this.#latestReading = clock.now()
  }

  remainingMilliseconds(): number {
    const reading = this.#clock.now()
    const elapsed = Math.max(0, reading - this.#latestReading)
    // A clock rollback must never replenish a terminal mutation budget.
    this.#remainingMilliseconds = Math.max(0, this.#remainingMilliseconds - elapsed)
    this.#latestReading = Math.max(this.#latestReading, reading)
    return this.#remainingMilliseconds
  }
}

const MONOTONIC_CLOCK: V2OutputSettlementClock = Object.freeze({
  now: () => performance.now(),
})

async function settleWithin<T>(
  operation: string,
  budget: SettlementBudget,
  settle: () => Promise<T>,
): Promise<T> {
  const remaining = budget.remainingMilliseconds()
  if (remaining <= 0) throw new V2OutputSettlementTimeoutError(operation)
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new V2OutputSettlementTimeoutError(operation)), remaining)
  })
  try {
    return await Promise.race([Promise.resolve().then(settle), timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
