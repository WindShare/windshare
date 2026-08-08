import {
  COMPLETED_JOB_SETTLEMENT,
  needsAttentionJobSettlement,
  pausedJobSettlement,
  type JobSettlement,
  type OutputSession,
  type V2OutputAuthority,
} from '../output-session'
import { FaultScope, OutputFaultCode, outputFault } from '../fault'

export const V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 30_000
export const V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 5 * 60_000

export { COMPLETED_JOB_SETTLEMENT }

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

/**
 * Transfer faults own only stable-pause authority. Current resume state is
 * discarded exclusively by the user-confirmed ResumeStateAuthority workflow.
 */
export async function pauseFailedV2Output(options: {
  readonly output?: OutputSession
  readonly authority: V2OutputAuthority
  readonly reason: unknown
  readonly timeoutMilliseconds: number
  readonly clock?: V2OutputSettlementClock
}): Promise<JobSettlement> {
  const budget = new SettlementBudget(options.timeoutMilliseconds, options.clock)
  try {
    if (options.output === undefined) {
      await settleWithin(
        'abandon unopened output capability',
        budget,
        () => options.authority.abort(options.reason),
      )
      return pausedJobSettlement('None')
    }
    return await settleWithin(
      'pause output at a stable cut',
      budget,
      () => options.output!.pauseJob(options.reason),
    )
  } catch {
    return needsAttentionJobSettlement(outputFault(
      FaultScope.OutputPause,
      OutputFaultCode.MutationAmbiguous,
    ))
  }
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
