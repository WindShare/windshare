import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { ReceiveIntent } from '../intent'
import type { TransferWorkerSettlement } from '../outcome'
import type {
  MaterializationSummary,
  PlanExecution,
  V2PlanExecutionAuthority,
} from '../output-session'

export const V2_DEFAULT_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 30_000
export const V2_MAXIMUM_OUTPUT_SETTLEMENT_TIMEOUT_MILLISECONDS = 5 * 60_000

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
 * Transfer failure can request only a stable lifecycle cut. If the plan adapter
 * cannot prove that cut within the bounded interval, the lifecycle owner records
 * target authority as unknown instead of the worker inventing a terminal state.
 */
export async function pauseFailedV2Execution(options: {
  readonly intent: ReceiveIntent
  readonly execution?: PlanExecution
  readonly authority: V2PlanExecutionAuthority
  readonly worker: TransferWorkerSettlement
  readonly materialization: MaterializationSummary
  readonly reason: unknown
  readonly timeoutMilliseconds: number
  readonly clock?: V2OutputSettlementClock
  readonly validateState?: (state: ReceiveLifecycleState) => ReceiveLifecycleState
}): Promise<ReceiveLifecycleState> {
  const budget = new SettlementBudget(options.timeoutMilliseconds, options.clock)
  const validate = options.validateState ?? ((state: ReceiveLifecycleState) => state)
  try {
    if (options.execution === undefined) {
      return validate(await settleWithSignal(
        'abandon unopened plan execution',
        budget,
        signal => options.authority.abortUnopened(
          options.intent,
          options.reason,
          signal,
        ),
      ))
    }
    return validate(await settleWithSignal(
      'pause plan execution at a stable cut',
      budget,
      signal => options.execution!.pause({
        worker: options.worker,
        materialization: options.materialization,
        reason: options.reason,
      }, signal),
    ))
  } catch {
    return validate(await settleWithSignal(
      'record unknown output settlement',
      new SettlementBudget(options.timeoutMilliseconds, options.clock),
      signal => options.authority.recordSettlementUnknown(
        options.intent,
        signal,
      ),
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

async function settleWithSignal<T>(
  operation: string,
  budget: SettlementBudget,
  settle: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  const controller = new AbortController()
  try {
    // The transfer lifetime is already aborted. Settlement needs an independent
    // bounded signal so cleanup can establish a stable cut, while timeout still
    // revokes the collaborator before authority is recorded as unknown.
    return await settleWithin(operation, budget, () => settle(controller.signal))
  } finally {
    controller.abort(new DOMException('Output settlement boundary closed', 'AbortError'))
  }
}
