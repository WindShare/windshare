import { networkMatrixError } from './contract-support.ts'

const MAXIMUM_OPERATION_DEADLINE_MS = 3_600_000

export type NetworkMatrixOperationClass =
  | 'runtime-bootstrap'
  | 'runtime-close'
  | 'authority-prepare'
  | 'sample-execute'
  | 'authority-close'

export interface NetworkMatrixOwnedOperation<T> {
  /** Resolves only after normal process/fixture ownership has settled. */
  readonly result: Promise<T>

  /** Must kill the full owned subtree/fixture and wait until it is reaped. */
  forceTerminateAndWait(reason: NetworkMatrixOperationClass): Promise<void>
}

export interface NetworkMatrixOwnershipRegistration {
  normalTerminal(): void
  forcedTerminal(): void
  handoff(input: NetworkMatrixOwnershipInput): NetworkMatrixOwnershipRegistration
}

export interface NetworkMatrixOwnershipInput {
  readonly operationId: string
  readonly operationClass: NetworkMatrixOperationClass
  readonly forceTerminateAndWait: (reason: NetworkMatrixOperationClass) => Promise<void>
}

export interface NetworkMatrixOwnershipRegistrar {
  register(input: NetworkMatrixOwnershipInput): NetworkMatrixOwnershipRegistration
}

export interface NetworkMatrixOwnedSettlementAuthority<T = never> {
  readonly registrar: NetworkMatrixOwnershipRegistrar
  readonly operationId: string
  readonly successor?: (value: T) => NetworkMatrixOwnershipInput
  readonly onSuccessorRegistered?: (registration: NetworkMatrixOwnershipRegistration) => void
}

export interface NetworkMatrixDeadlineTimer {
  readonly elapsed: Promise<void>
  cancel(): void
}

export interface NetworkMatrixDeadlineScheduler {
  schedule(delayMs: number): NetworkMatrixDeadlineTimer
}

export class NetworkMatrixDeadlineExceeded extends Error {
  readonly operationClass: NetworkMatrixOperationClass

  constructor(operationClass: NetworkMatrixOperationClass) {
    super(`browser network matrix ${operationClass} deadline exceeded`)
    this.name = 'NetworkMatrixDeadlineExceeded'
    this.operationClass = operationClass
  }
}

export class NetworkMatrixOwnershipCleanupError extends AggregateError {
  readonly operationClass: NetworkMatrixOperationClass
  readonly primaryFailure: unknown
  readonly cleanupFailure: unknown

  constructor(
    operationClass: NetworkMatrixOperationClass,
    primaryFailure: unknown,
    cleanupFailure: unknown,
  ) {
    super(
      [primaryFailure, cleanupFailure],
      `browser network matrix ${operationClass} failed and ownership cleanup also failed`,
    )
    this.name = 'NetworkMatrixOwnershipCleanupError'
    this.operationClass = operationClass
    this.primaryFailure = primaryFailure
    this.cleanupFailure = cleanupFailure
  }
}

/**
 * Deadline expiry is not merely cancellation signalling. The owner must prove
 * that its process tree or fixture has been forcibly terminated and reaped
 * before control returns to the orchestrator.
 */
export async function settleOwnedOperation<T>(
  operation: NetworkMatrixOwnedOperation<T>,
  operationClass: NetworkMatrixOperationClass,
  deadlineMs: number,
  scheduler: NetworkMatrixDeadlineScheduler = systemDeadlineScheduler,
  ownership?: NetworkMatrixOwnedSettlementAuthority<T>,
): Promise<T> {
  const registration = ownership?.registrar.register({
    operationId: ownership.operationId,
    operationClass,
    forceTerminateAndWait: operation.forceTerminateAndWait,
  })
  const timer = await scheduleOwnedOperationDeadline(
    operation,
    operationClass,
    deadlineMs,
    scheduler,
    registration,
  )
  const terminal = operation.result.then(
    (value) => ({ kind: 'completed' as const, value }),
    (cause: unknown) => ({ kind: 'failed' as const, cause }),
  )
  const winner = await Promise.race([
    terminal,
    timer.elapsed.then(() => ({ kind: 'deadline' as const })),
  ])
  timer.cancel()
  if (winner.kind === 'completed') {
    return settleCompletedOwnedOperation(
      winner.value,
      operation,
      operationClass,
      registration,
      ownership,
    )
  }
  const primaryFailure = winner.kind === 'failed'
    ? winner.cause
    : new NetworkMatrixDeadlineExceeded(operationClass)
  return forceOwnedOperationAfterFailure(
    operation,
    operationClass,
    registration,
    operationClass,
    primaryFailure,
  )
}

async function scheduleOwnedOperationDeadline<T>(
  operation: NetworkMatrixOwnedOperation<T>,
  operationClass: NetworkMatrixOperationClass,
  deadlineMs: number,
  scheduler: NetworkMatrixDeadlineScheduler,
  registration: NetworkMatrixOwnershipRegistration | undefined,
): Promise<NetworkMatrixDeadlineTimer> {
  try {
    requireDeadline(deadlineMs)
    return scheduler.schedule(deadlineMs)
  } catch (primaryFailure) {
    return forceOwnedOperationAfterFailure(
      operation,
      operationClass,
      registration,
      operationClass,
      primaryFailure,
    )
  }
}

async function settleCompletedOwnedOperation<T>(
  value: T,
  operation: NetworkMatrixOwnedOperation<T>,
  operationClass: NetworkMatrixOperationClass,
  registration: NetworkMatrixOwnershipRegistration | undefined,
  ownership: NetworkMatrixOwnedSettlementAuthority<T> | undefined,
): Promise<T> {
  if (ownership?.successor === undefined) {
    registration?.normalTerminal()
    return value
  }
  let successorInput: NetworkMatrixOwnershipInput | undefined
  let successorRegistration: NetworkMatrixOwnershipRegistration | undefined
  try {
    if (registration === undefined) {
      throw new Error('network matrix ownership handoff lacks its predecessor registration')
    }
    successorInput = ownership.successor(value)
    successorRegistration = registration.handoff(successorInput)
    ownership.onSuccessorRegistered?.(successorRegistration)
  } catch (primaryFailure) {
    const owner = successorInput ?? operation
    return forceOwnedOperationAfterFailure(
      owner,
      successorInput?.operationClass ?? operationClass,
      successorRegistration ?? registration,
      operationClass,
      primaryFailure,
    )
  }
  return value
}

async function forceOwnedOperationAfterFailure(
  owner: Pick<NetworkMatrixOwnershipInput, 'forceTerminateAndWait'>,
  forceReason: NetworkMatrixOperationClass,
  registration: NetworkMatrixOwnershipRegistration | undefined,
  operationClass: NetworkMatrixOperationClass,
  primaryFailure: unknown,
): Promise<never> {
  try {
    await owner.forceTerminateAndWait(forceReason)
    registration?.forcedTerminal()
  } catch (cleanupFailure) {
    throw new NetworkMatrixOwnershipCleanupError(
      operationClass,
      primaryFailure,
      cleanupFailure,
    )
  }
  throw primaryFailure
}

export function completedOwnedOperation<T>(value: T): NetworkMatrixOwnedOperation<T> {
  return Object.freeze({
    result: Promise.resolve(value),
    forceTerminateAndWait: () => Promise.resolve(),
  })
}

/**
 * Defers a resource-producing factory to the next microtask. The orchestrator can
 * therefore register this wrapper before the factory is allowed to construct or
 * start its nested owner. A force request made during that handoff is remembered
 * and applied to the nested operation before its value can escape.
 */
export function deferredOwnedOperation<T>(
  factory: () => NetworkMatrixOwnedOperation<T>,
): NetworkMatrixOwnedOperation<T> {
  let forceReason: NetworkMatrixOperationClass | undefined
  let forceOperation: Promise<void> | undefined
  const nestedFactory = Promise.resolve().then(() => {
    const operation = factory()
    if (
      typeof operation !== 'object' || operation === null ||
      !(operation.result instanceof Promise) || typeof operation.forceTerminateAndWait !== 'function'
    ) throw new Error('network matrix owned-operation factory returned an invalid owner')
    return operation
  })
  const force = (reason: NetworkMatrixOperationClass): Promise<void> => {
    forceReason ??= reason
    if (forceOperation !== undefined) return forceOperation
    const operation = nestedFactory.then(
      (owned) => owned.forceTerminateAndWait(forceReason as NetworkMatrixOperationClass),
      () => undefined,
    )
    const retryable = operation.catch((cause: unknown) => {
      if (forceOperation === retryable) forceOperation = undefined
      throw cause
    })
    forceOperation = retryable
    return retryable
  }
  const result = nestedFactory.then(async (operation) => {
    if (forceReason !== undefined) {
      await force(forceReason)
      throw new Error('network matrix owned-operation factory was terminated before transfer')
    }
    const value = await operation.result
    if (forceReason !== undefined) {
      await force(forceReason)
      throw new Error('network matrix owned-operation factory was terminated during transfer')
    }
    return value
  })
  return Object.freeze({
    result,
    forceTerminateAndWait: force,
  })
}

export function mapOwnedOperation<Source, Target>(
  operation: NetworkMatrixOwnedOperation<Source>,
  map: (source: Source) => Target | Promise<Target>,
): NetworkMatrixOwnedOperation<Target> {
  return Object.freeze({
    result: operation.result.then(map),
    forceTerminateAndWait: (reason: NetworkMatrixOperationClass) =>
      operation.forceTerminateAndWait(reason),
  })
}

export const systemDeadlineScheduler: NetworkMatrixDeadlineScheduler = Object.freeze({
  schedule(delayMs: number): NetworkMatrixDeadlineTimer {
    let timeout: ReturnType<typeof setTimeout> | undefined
    const elapsed = new Promise<void>((resolve) => {
      timeout = setTimeout(resolve, delayMs)
    })
    return Object.freeze({
      elapsed,
      cancel(): void {
        if (timeout !== undefined) clearTimeout(timeout)
      },
    })
  },
})

function requireDeadline(value: number): void {
  if (!Number.isSafeInteger(value) || value < 1 || value > MAXIMUM_OPERATION_DEADLINE_MS) {
    networkMatrixError('network matrix operation deadline is outside the safe range')
  }
}
