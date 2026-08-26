import type { IncidentScopeHandle } from '../../diagnostics/incident'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { RecoverySelectionFacts } from '../../output/workspace/state'
import { dependencyContractFault } from '../fault'
import type { ReceiveIntent } from '../intent'
import {
  materializeClassifiedTransferFailure,
  normalizeV2FileTransferFailure,
  normalizedV2FileTransferFault,
  type ClassifiedTransferFailure,
} from '../job/failures'
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

export interface V2OutputSettlementDeadline {
  schedule(delayMilliseconds: number, expire: () => void): Readonly<{ cancel(): void }>
}

export class V2OutputSettlementTimeoutError extends Error {
  readonly operation: string

  constructor(operation: string) {
    super(`Output settlement timed out while attempting to ${operation}`)
    this.name = 'V2OutputSettlementTimeoutError'
    this.operation = operation
  }
}

/**
 * Carries reviewed initiating and consequence semantics across the settlement boundary.
 * Native errors remain at their immediate classifier and cannot become later policy inputs.
 */
export class V2TransferFailureSettlementError extends Error {
  readonly transferFailure: ClassifiedTransferFailure | undefined
  readonly settlementFailures: readonly ClassifiedTransferFailure[]
  readonly trigger: ClassifiedTransferFailure

  constructor(
    transferFailure: ClassifiedTransferFailure | undefined,
    settlementFailures: readonly ClassifiedTransferFailure[],
  ) {
    const initiating = transferFailure === undefined
      ? undefined
      : materializeClassifiedTransferFailure(transferFailure, undefined)
    const failures = Object.freeze(settlementFailures.map(failure =>
      materializeClassifiedTransferFailure(failure, undefined)))
    const trigger = initiating ?? failures[0]
    if (trigger === undefined) {
      throw new TypeError('Failed output settlement requires reviewed failure authority')
    }
    super('Transfer failed before output settlement completed')
    this.name = 'V2TransferFailureSettlementError'
    this.transferFailure = initiating
    this.settlementFailures = failures
    this.trigger = trigger
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
  deadline?: V2OutputSettlementDeadline,
): Promise<T> {
  return settleWithin(
    operation,
    new SettlementBudget(timeoutMilliseconds, clock),
    settle,
    deadline,
  )
}

/**
 * A DirectTree terminal cut owns live mutation and lifecycle authority until its
 * original promise drains. A deadline may revoke cancellable work and govern the
 * exposed failure, but cannot detach that authority from the job result boundary.
 */
export async function withQuiescentOutputSettlementTimeout<T>(
  operation: string,
  timeoutMilliseconds: number,
  settle: (signal: AbortSignal) => Promise<T>,
  deadline: V2OutputSettlementDeadline = SYSTEM_SETTLEMENT_DEADLINE,
): Promise<T> {
  const controller = new AbortController()
  const timeoutError = new V2OutputSettlementTimeoutError(operation)
  const terminal = Promise.resolve().then(() => settle(controller.signal))
  const terminalOutcome = terminal.then(
    value => Object.freeze({ kind: 'settled' as const, value }),
    error => Object.freeze({ kind: 'failed' as const, error }),
  )
  let deadlineTask: Readonly<{ cancel(): void }> | undefined
  const deadlineOutcome = new Promise<
    Readonly<{ kind: 'expired'; error: V2OutputSettlementTimeoutError }> |
    Readonly<{ kind: 'deadline-failed'; error: unknown }>
  >(resolve => {
    try {
      deadlineTask = deadline.schedule(
        timeoutMilliseconds,
        () => resolve(Object.freeze({ kind: 'expired' as const, error: timeoutError })),
      )
    } catch (error) {
      resolve(Object.freeze({ kind: 'deadline-failed' as const, error }))
    }
  })

  try {
    const first = await Promise.race([terminalOutcome, deadlineOutcome])
    if (first.kind === 'expired' || first.kind === 'deadline-failed') {
      controller.abort(first.error)
      // The latched deadline outcome remains authoritative even if cancellation or
      // a later terminal collaborator failure is observed while draining the cut.
      await terminalOutcome
      throw first.error
    }
    if (first.kind === 'failed') throw first.error
    return first.value
  } finally {
    deadlineTask?.cancel()
    if (!controller.signal.aborted) {
      controller.abort(new DOMException('Terminal output settlement boundary closed', 'AbortError'))
    }
  }
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
  readonly selectionFacts: RecoverySelectionFacts
  readonly reason: unknown
  readonly failureTrigger?: ClassifiedTransferFailure
  readonly incidentScope?: IncidentScopeHandle
  readonly timeoutMilliseconds: number
  readonly clock?: V2OutputSettlementClock
  readonly deadline?: V2OutputSettlementDeadline
  readonly validateState?: (state: ReceiveLifecycleState) => ReceiveLifecycleState
}): Promise<ReceiveLifecycleState> {
  const budget = new SettlementBudget(options.timeoutMilliseconds, options.clock)
  const validate = options.validateState ?? ((state: ReceiveLifecycleState) => state)
  try {
    if (options.execution === undefined) {
      return validate(await settleWithSignal(
        'settle plan execution admission failure',
        budget,
        signal => options.authority.settleExecutionAdmissionFailure(
          options.intent,
          options.reason,
          signal,
        ),
        options.deadline,
      ))
    }
    return validate(await settleWithSignal(
      'pause plan execution at a stable cut',
      budget,
      signal => options.execution!.pause({
        worker: options.worker,
        materialization: options.materialization,
        selectionFacts: options.selectionFacts,
        reason: options.reason,
      }, signal),
      options.deadline,
    ))
  } catch (settlementFailure) {
    try {
      return validate(await settleWithSignal(
        'record unknown output settlement',
        new SettlementBudget(options.timeoutMilliseconds, options.clock),
        signal => options.authority.recordSettlementUnknown(
          options.intent,
          signal,
        ),
        options.deadline,
      ))
    } catch (unknownSettlementFailure) {
      const settlementFailures = [settlementFailure, unknownSettlementFailure]
        .map(classifySettlementConsequence)
      throw new V2TransferFailureSettlementError(options.failureTrigger, settlementFailures)
    }
  }

  function classifySettlementConsequence(error: unknown): ClassifiedTransferFailure {
    const normalized = normalizeV2FileTransferFailure(error, {
      stage: 'settlement',
      relation: 'consequence',
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    })
    if (normalized.kind === 'fault') return normalized.diagnostic.classification
    return normalizedV2FileTransferFault(dependencyContractFault(), {
      stage: 'settlement',
      relation: 'consequence',
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    }).diagnostic.classification
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

const SYSTEM_SETTLEMENT_DEADLINE: V2OutputSettlementDeadline = Object.freeze({
  schedule: (delayMilliseconds: number, expire: () => void) => {
    const timer = setTimeout(expire, delayMilliseconds)
    return Object.freeze({ cancel: () => clearTimeout(timer) })
  },
})

async function settleWithin<T>(
  operation: string,
  budget: SettlementBudget,
  settle: () => Promise<T>,
  deadline: V2OutputSettlementDeadline = SYSTEM_SETTLEMENT_DEADLINE,
): Promise<T> {
  const remaining = budget.remainingMilliseconds()
  if (remaining <= 0) throw new V2OutputSettlementTimeoutError(operation)
  let deadlineTask: Readonly<{ cancel(): void }> | undefined
  const timeout = new Promise<never>((_resolve, reject) => {
    deadlineTask = deadline.schedule(
      remaining,
      () => reject(new V2OutputSettlementTimeoutError(operation)),
    )
  })
  try {
    return await Promise.race([Promise.resolve().then(settle), timeout])
  } finally {
    deadlineTask?.cancel()
  }
}

async function settleWithSignal<T>(
  operation: string,
  budget: SettlementBudget,
  settle: (signal: AbortSignal) => Promise<T>,
  deadline?: V2OutputSettlementDeadline,
): Promise<T> {
  const controller = new AbortController()
  try {
    // The transfer lifetime is already aborted. Settlement needs an independent
    // bounded signal so cleanup can establish a stable cut, while timeout still
    // revokes the collaborator before authority is recorded as unknown.
    return await settleWithin(
      operation,
      budget,
      () => settle(controller.signal),
      deadline,
    )
  } finally {
    controller.abort(new DOMException('Output settlement boundary closed', 'AbortError'))
  }
}
