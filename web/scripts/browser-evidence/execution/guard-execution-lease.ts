import { performance } from 'node:perf_hooks'

export const GUARD_WORKFLOW_STEP_BUDGET_MS = 300_000 as const
export const GUARD_SUITE_TOTAL_BUDGET_MS = 240_000 as const
export const GUARD_SUITE_CLEANUP_RESERVE_MS = 30_000 as const
export const GUARD_NATIVE_OPERATION_BUDGET_MS = 30_000 as const

const MINIMUM_EXECUTION_WINDOW_MS = 1
const PRIMARY_OPERATION_SETTLEMENT_RESERVE_MS = 2_000

export class GuardExecutionUnsettledError extends Error {
  constructor(label: string) {
    super(`${label} ignored cancellation and remained active`)
    this.name = 'GuardExecutionUnsettledError'
  }
}

export interface GuardExecutionBudget {
  readonly totalBudgetMs: number
  readonly cleanupReserveMs: number
  readonly nativeOperationBudgetMs: number
}

export interface GuardExecutionWindow {
  readonly signal: AbortSignal
  readonly maximumDurationMs: number
}

const issuedExecutionWindows = new WeakMap<object, () => void>()

/**
 * Native operations revalidate the issuing lease at point of use. This closes
 * the stale-capability gap where cleanup authority was obtained before a
 * primary operation timed out, then invoked only after the lease was poisoned.
 */
export function assertGuardExecutionWindowUsable(
  value: unknown,
): asserts value is GuardExecutionWindow {
  if ((typeof value !== 'object' && typeof value !== 'function') || value === null) {
    throw new Error('native guard operation lacks an issued execution window')
  }
  const assertUsable = issuedExecutionWindows.get(value)
  if (assertUsable === undefined) {
    throw new Error('native guard operation lacks an issued execution window')
  }
  assertUsable()
}

/**
 * One monotonic lease spans scanning, staging, publication, recovery, and
 * cleanup. The cleanup deadline is intentionally later than the primary
 * deadline so failure handling retains time that ordinary work cannot consume.
 */
export class GuardExecutionLease {
  readonly #primaryDeadline: number
  readonly #totalDeadline: number
  readonly #nativeOperationBudgetMs: number
  #unsettledPrimaryOperation = false

  private constructor(startedAt: number, budget: GuardExecutionBudget) {
    this.#primaryDeadline = startedAt + budget.totalBudgetMs - budget.cleanupReserveMs
    this.#totalDeadline = startedAt + budget.totalBudgetMs
    this.#nativeOperationBudgetMs = budget.nativeOperationBudgetMs
  }

  static start(overrides: Partial<GuardExecutionBudget> = {}): GuardExecutionLease {
    const budget = Object.freeze({
      totalBudgetMs: overrides.totalBudgetMs ?? GUARD_SUITE_TOTAL_BUDGET_MS,
      cleanupReserveMs: overrides.cleanupReserveMs ?? GUARD_SUITE_CLEANUP_RESERVE_MS,
      nativeOperationBudgetMs:
        overrides.nativeOperationBudgetMs ?? GUARD_NATIVE_OPERATION_BUDGET_MS,
    })
    requireBudget(budget)
    return new GuardExecutionLease(performance.now(), budget)
  }

  throwIfPrimaryExpired(label: string): void {
    this.assertCleanupSafe(label)
    if (this.#remaining(this.#primaryDeadline) < MINIMUM_EXECUTION_WINDOW_MS) {
      throw new Error(`${label} exceeded the guard primary execution lease`)
    }
  }

  assertCleanupSafe(label: string): void {
    if (this.#unsettledPrimaryOperation) throw new GuardExecutionUnsettledError(label)
  }

  primarySignal(label: string): AbortSignal {
    this.assertCleanupSafe(label)
    const remaining = this.#remaining(this.#primaryDeadline)
    if (remaining < MINIMUM_EXECUTION_WINDOW_MS) {
      throw new Error(`${label} cannot start after its guard execution lease expired`)
    }
    return AbortSignal.timeout(Math.max(MINIMUM_EXECUTION_WINDOW_MS, Math.floor(remaining)))
  }

  primaryWindow(label: string): GuardExecutionWindow {
    this.assertCleanupSafe(label)
    return this.#window(this.#primaryDeadline, this.#nativeOperationBudgetMs, label)
  }

  cleanupWindow(label: string): GuardExecutionWindow {
    this.assertCleanupSafe(label)
    return this.#window(this.#totalDeadline, this.#nativeOperationBudgetMs, label)
  }

  async runPrimary<T>(
    label: string,
    operation: (signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    this.assertCleanupSafe(label)
    return this.#run(this.#primaryDeadline, label, operation)
  }

  async runCleanup<T>(
    label: string,
    operation: (signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    this.assertCleanupSafe(label)
    return this.#run(this.#totalDeadline, label, operation)
  }

  #window(deadline: number, maximumDurationMs: number, label: string): GuardExecutionWindow {
    const remaining = this.#remaining(deadline)
    if (remaining < MINIMUM_EXECUTION_WINDOW_MS) {
      throw new Error(`${label} cannot start after its guard execution lease expired`)
    }
    const duration = Math.max(
      MINIMUM_EXECUTION_WINDOW_MS,
      Math.min(maximumDurationMs, Math.floor(remaining)),
    )
    const signal = AbortSignal.timeout(Math.max(MINIMUM_EXECUTION_WINDOW_MS, Math.floor(remaining)))
    const window = Object.freeze({
      signal,
      maximumDurationMs: duration,
    })
    issuedExecutionWindows.set(window, () => {
      this.assertCleanupSafe(label)
      if (signal.aborted || this.#remaining(deadline) < MINIMUM_EXECUTION_WINDOW_MS) {
        throw new Error(`${label} cannot run after its guard execution lease expired`)
      }
    })
    return window
  }

  async #run<T>(
    deadline: number,
    label: string,
    operation: (signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    const remaining = this.#remaining(deadline)
    if (remaining < MINIMUM_EXECUTION_WINDOW_MS) {
      throw new Error(`${label} cannot start after its guard execution lease expired`)
    }
    const controller = new AbortController()
    const timer = setTimeout(
      () => controller.abort(new Error(`${label} exceeded its guard execution lease`)),
      Math.max(MINIMUM_EXECUTION_WINDOW_MS, Math.floor(remaining)),
    )
    let operationSettled = false
    const operationPromise = Promise.resolve()
      .then(() => operation(controller.signal))
      .finally(() => { operationSettled = true })
    try {
      return await Promise.race([
        operationPromise,
        new Promise<never>((_resolve, reject) => {
          controller.signal.addEventListener('abort', () => reject(controller.signal.reason), {
            once: true,
          })
        }),
      ])
    } catch (cause) {
      if (controller.signal.aborted && !operationSettled) {
        let settlementTimer: NodeJS.Timeout | undefined
        try {
          await Promise.race([
            operationPromise.then(() => undefined, () => undefined),
            new Promise<void>((resolve) => {
              settlementTimer = setTimeout(resolve, Math.min(
                PRIMARY_OPERATION_SETTLEMENT_RESERVE_MS,
                Math.max(MINIMUM_EXECUTION_WINDOW_MS, Math.floor(this.#remaining(this.#totalDeadline))),
              ))
            }),
          ])
        } finally {
          if (settlementTimer !== undefined) clearTimeout(settlementTimer)
        }
        if (!operationSettled) {
          this.#unsettledPrimaryOperation = true
          throw new GuardExecutionUnsettledError(label)
        }
      }
      throw cause
    } finally {
      clearTimeout(timer)
    }
  }

  #remaining(deadline: number): number {
    return deadline - performance.now()
  }
}

function requireBudget(budget: GuardExecutionBudget): void {
  const values = [budget.totalBudgetMs, budget.cleanupReserveMs, budget.nativeOperationBudgetMs]
  if (values.some((value) => !Number.isSafeInteger(value) || value < MINIMUM_EXECUTION_WINDOW_MS)) {
    throw new Error('guard execution budget must contain positive integer milliseconds')
  }
  if (budget.totalBudgetMs > GUARD_SUITE_TOTAL_BUDGET_MS ||
      budget.cleanupReserveMs > GUARD_SUITE_CLEANUP_RESERVE_MS ||
      budget.nativeOperationBudgetMs > GUARD_NATIVE_OPERATION_BUDGET_MS ||
      budget.cleanupReserveMs >= budget.totalBudgetMs) {
    throw new Error('guard execution budget exceeds its frozen workflow authority')
  }
  if (budget.totalBudgetMs >= GUARD_WORKFLOW_STEP_BUDGET_MS) {
    throw new Error('guard execution budget does not leave an outer workflow settlement reserve')
  }
}
