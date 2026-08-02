import { randomBytes } from 'node:crypto'

import { requireOperationId, requireRunId } from './contract-support.ts'
import type {
  NetworkMatrixOperationClass,
  NetworkMatrixOwnershipInput,
  NetworkMatrixOwnershipRegistrar,
  NetworkMatrixOwnershipRegistration,
} from './owned-operation.ts'

const RETRY_DELAY_MS = 1_000

interface RetainedOwnership {
  readonly token: symbol
  readonly sequence: number
  readonly operationId: string
  readonly operationClass: NetworkMatrixOperationClass
  readonly forceTerminateAndWait: (reason: NetworkMatrixOperationClass) => Promise<void>
  forceInFlight?: Promise<void>
}

export interface NetworkMatrixInvocationBinding {
  readonly invocationId: string
  readonly runId: string
}

export interface NetworkMatrixOwnershipRetryWaiter {
  wait(): Promise<void>
}

/**
 * An invocation ledger is the last in-process owner of every operation whose
 * forced settlement has not been proven. Evidence publication may proceed after
 * a failure, but the invocation cannot return while this registry is non-empty.
 */
export class NetworkMatrixInvocationOwnershipLedger implements NetworkMatrixOwnershipRegistrar {
  readonly binding: NetworkMatrixInvocationBinding
  readonly #retained = new Map<string, RetainedOwnership>()
  #sequence = 0

  constructor(runId: string, invocationId = newNetworkMatrixInvocationId()) {
    this.binding = Object.freeze({
      invocationId: requireOperationId(invocationId, 'network matrix invocation ID'),
      runId: requireRunId(runId, 'network matrix invocation run ID'),
    })
  }

  get retainedCount(): number { return this.#retained.size }

  get retainedOperationIds(): readonly string[] {
    return Object.freeze([...this.#retained.values()]
      .sort((left, right) => left.sequence - right.sequence)
      .map(({ operationId }) => operationId))
  }

  register(input: NetworkMatrixOwnershipInput): NetworkMatrixOwnershipRegistration {
    const operationId = requireOperationId(input.operationId, 'network matrix ownership operation ID')
    if (typeof input.forceTerminateAndWait !== 'function' || this.#retained.has(operationId)) {
      throw new Error('network matrix ownership operation is duplicated or invalid')
    }
    const token = Symbol(operationId)
    const retained: RetainedOwnership = {
      token,
      sequence: this.#sequence,
      operationId,
      operationClass: input.operationClass,
      forceTerminateAndWait: input.forceTerminateAndWait,
    }
    this.#sequence += 1
    this.#retained.set(operationId, retained)
    return this.#registration(retained)
  }

  #registration(retained: RetainedOwnership): NetworkMatrixOwnershipRegistration {
    const settle = (): void => {
      const { operationId, token } = retained
      if (this.#retained.get(operationId)?.token === token) this.#retained.delete(operationId)
    }
    return Object.freeze({
      normalTerminal: settle,
      forcedTerminal: settle,
      handoff: (input: NetworkMatrixOwnershipInput): NetworkMatrixOwnershipRegistration => {
        const current = this.#retained.get(retained.operationId)
        if (current?.token !== retained.token) {
          throw new Error('network matrix ownership predecessor is no longer retained')
        }
        const operationId = requireOperationId(
          input.operationId,
          'network matrix ownership successor operation ID',
        )
        if (
          typeof input.forceTerminateAndWait !== 'function' ||
          operationId !== retained.operationId && this.#retained.has(operationId)
        ) throw new Error('network matrix ownership successor is duplicated or invalid')
        const successor: RetainedOwnership = {
          token: Symbol(operationId),
          sequence: retained.sequence,
          operationId,
          operationClass: input.operationClass,
          forceTerminateAndWait: input.forceTerminateAndWait,
        }
        this.#retained.set(operationId, successor)
        if (operationId !== retained.operationId) this.#retained.delete(retained.operationId)
        return this.#registration(successor)
      },
    })
  }

  async retryRetainedOnce(): Promise<void> {
    const retained = [...this.#retained.values()].sort(
      (left, right) => right.sequence - left.sequence,
    )
    for (const ownership of retained) {
      if (this.#retained.get(ownership.operationId)?.token !== ownership.token) continue
      if (ownership.forceInFlight === undefined) {
        const force = Promise.resolve().then(() => ownership.forceTerminateAndWait(
          ownership.operationClass,
        ))
        const retryable = force.catch((cause: unknown) => {
          if (ownership.forceInFlight === retryable) delete ownership.forceInFlight
          throw cause
        })
        ownership.forceInFlight = retryable
      }
      try {
        await ownership.forceInFlight
        if (this.#retained.get(ownership.operationId)?.token === ownership.token) {
          this.#retained.delete(ownership.operationId)
        }
      } catch (cause) {
        // Registration order is the dependency order: a later child may retain
        // leases owned by an earlier parent. Crossing an unsettled child would
        // destroy the parent authority needed by the child's exact retry.
        throw new AggregateError(
          [cause],
          `network matrix invocation ownership remains unsettled at ${ownership.operationId}`,
          { cause },
        )
      }
    }
  }

  async retainUntilEmpty(
    waiter: NetworkMatrixOwnershipRetryWaiter = systemOwnershipRetryWaiter,
    onRetryFailure: (failure: unknown, retainedOperationIds: readonly string[]) => void = () => {},
  ): Promise<void> {
    while (this.#retained.size !== 0) {
      try {
        await this.retryRetainedOnce()
      } catch (cause) {
        onRetryFailure(cause, this.retainedOperationIds)
      }
      if (this.#retained.size !== 0) await waiter.wait()
    }
  }
}

export function newNetworkMatrixInvocationId(): string {
  return `invocation-${randomBytes(16).toString('hex')}`
}

const systemOwnershipRetryWaiter: NetworkMatrixOwnershipRetryWaiter = Object.freeze({
  wait: () => new Promise<void>((resolve) => setTimeout(resolve, RETRY_DELAY_MS)),
})
