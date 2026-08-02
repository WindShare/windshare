import { describe, expect, it, vi } from 'vitest'

import { NetworkMatrixInvocationOwnershipLedger } from '../../scripts/browser-network-matrix/invocation-ownership.ts'
import {
  NetworkMatrixOwnershipCleanupError,
  deferredOwnedOperation,
  settleOwnedOperation,
  type NetworkMatrixDeadlineScheduler,
} from '../../scripts/browser-network-matrix/owned-operation.ts'

describe('network matrix invocation ownership ledger', () => {
  it('accepts the exact run and operation identity ceilings', () => {
    const operationId = `A.${'b'.repeat(124)}_Z`
    const ledger = new NetworkMatrixInvocationOwnershipLedger('r'.repeat(96), operationId)

    const registration = ledger.register({
      operationId,
      operationClass: 'sample-execute',
      forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
    })

    expect(ledger.retainedOperationIds).toEqual([operationId])
    registration.normalTerminal()
    expect(ledger.retainedCount).toBe(0)
  })

  it.each([
    ['above byte ceiling', 'o'.repeat(129)],
    ['leading punctuation', '.operation'],
    ['trailing punctuation', 'operation_'],
    ['colon', 'operation:one'],
    ['slash', 'operation/one'],
    ['space', 'operation one'],
    ['non-ASCII alphabet', 'op?ration'],
  ])('rejects an operation ID with %s', (_label, operationId) => {
    const ledger = new NetworkMatrixInvocationOwnershipLedger('run-alpha', 'invocation-alpha')
    expect(() => ledger.register({
      operationId,
      operationClass: 'sample-execute',
      forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
    })).toThrow()
  })

  it('registers the wrapper before a resource-producing factory is allowed to run', async () => {
    const ledger = new NetworkMatrixInvocationOwnershipLedger('run-alpha', 'invocation-alpha')
    let observedDuringFactory: readonly string[] = []
    const operation = deferredOwnedOperation(() => {
      observedDuringFactory = ledger.retainedOperationIds
      return {
        result: Promise.resolve('settled'),
        forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
      }
    })

    await expect(settleOwnedOperation(
      operation,
      'runtime-bootstrap',
      100,
      neverElapsedScheduler(),
      { registrar: ledger, operationId: 'factory-owner' },
    )).resolves.toBe('settled')

    expect(observedDuringFactory).toEqual(['factory-owner'])
    expect(ledger.retainedCount).toBe(0)
  })

  it('atomically replaces a completed producer with the live authority it returned', async () => {
    const ledger = new NetworkMatrixInvocationOwnershipLedger('run-alpha', 'invocation-alpha')
    const forceLive = vi.fn().mockResolvedValue(undefined)
    let observedAtHandoff: readonly string[] = []

    await expect(settleOwnedOperation(
      {
        result: Promise.resolve('live-runtime'),
        forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
      },
      'runtime-bootstrap',
      100,
      neverElapsedScheduler(),
      {
        registrar: ledger,
        operationId: 'runtime-bootstrap',
        successor: () => ({
          operationId: 'runtime-live',
          operationClass: 'runtime-close',
          forceTerminateAndWait: forceLive,
        }),
        onSuccessorRegistered: () => { observedAtHandoff = ledger.retainedOperationIds },
      },
    )).resolves.toBe('live-runtime')

    expect(observedAtHandoff).toEqual(['runtime-live'])
    expect(ledger.retainedOperationIds).toEqual(['runtime-live'])
    await ledger.retryRetainedOnce()
    expect(forceLive).toHaveBeenCalledOnce()
  })

  it('does not cross an unsettled child while unwinding strict reverse ownership order', async () => {
    const ledger = new NetworkMatrixInvocationOwnershipLedger('run-alpha', 'invocation-alpha')
    const parentForce = vi.fn().mockResolvedValue(undefined)
    const childForce = vi.fn()
      .mockRejectedValueOnce(new Error('child still owns a lease'))
      .mockResolvedValueOnce(undefined)
    ledger.register({
      operationId: 'parent-runtime',
      operationClass: 'runtime-close',
      forceTerminateAndWait: parentForce,
    })
    ledger.register({
      operationId: 'child-sample',
      operationClass: 'sample-execute',
      forceTerminateAndWait: childForce,
    })

    await expect(ledger.retryRetainedOnce()).rejects.toThrow('child-sample')
    expect(parentForce).not.toHaveBeenCalled()
    expect(ledger.retainedOperationIds).toEqual(['parent-runtime', 'child-sample'])

    await expect(ledger.retryRetainedOnce()).resolves.toBeUndefined()
    expect(childForce).toHaveBeenCalledTimes(2)
    expect(parentForce).toHaveBeenCalledOnce()
    expect(ledger.retainedCount).toBe(0)
  })

  it('retains a timed-out operation when its result resolves after failed forced cleanup', async () => {
    const ledger = new NetworkMatrixInvocationOwnershipLedger('run-alpha', 'invocation-alpha')
    let resolveResult: ((value: string) => void) | undefined
    const result = new Promise<string>((resolve) => { resolveResult = resolve })
    const force = vi.fn()
      .mockRejectedValueOnce(new Error('cleanup proof unavailable'))
      .mockResolvedValueOnce(undefined)

    await expect(settleOwnedOperation(
      { result, forceTerminateAndWait: force },
      'runtime-bootstrap',
      1,
      immediateDeadlineScheduler(),
      { registrar: ledger, operationId: 'late-bootstrap' },
    )).rejects.toBeInstanceOf(NetworkMatrixOwnershipCleanupError)
    resolveResult?.('late-live-runtime')
    await result

    expect(ledger.retainedOperationIds).toEqual(['late-bootstrap'])
    await ledger.retryRetainedOnce()
    expect(force).toHaveBeenCalledTimes(2)
    expect(ledger.retainedCount).toBe(0)
  })
})

function immediateDeadlineScheduler(): NetworkMatrixDeadlineScheduler {
  return {
    schedule: () => ({ elapsed: Promise.resolve(), cancel: () => undefined }),
  }
}

function neverElapsedScheduler(): NetworkMatrixDeadlineScheduler {
  return {
    schedule: () => ({ elapsed: new Promise(() => undefined), cancel: () => undefined }),
  }
}
