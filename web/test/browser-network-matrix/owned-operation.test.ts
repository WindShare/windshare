import { describe, expect, it, vi } from 'vitest'

import {
  NetworkMatrixDeadlineExceeded,
  NetworkMatrixOwnershipCleanupError,
  settleOwnedOperation,
  type NetworkMatrixDeadlineScheduler,
} from '../../scripts/browser-network-matrix/owned-operation.ts'

describe('browser network matrix owned-operation settlement', () => {
  it('does not call forced termination after a normally settled operation', async () => {
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)

    await expect(settleOwnedOperation({
      result: Promise.resolve('settled'),
      forceTerminateAndWait,
    }, 'sample-execute', 10, neverElapsedScheduler())).resolves.toBe('settled')
    expect(forceTerminateAndWait).not.toHaveBeenCalled()
  })

  it('forcibly reaps an owned subtree after result rejection before rethrowing', async () => {
    const failure = new Error('child rejected after launch')
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)

    await expect(settleOwnedOperation({
      result: Promise.reject(failure),
      forceTerminateAndWait,
    }, 'sample-execute', 10, neverElapsedScheduler())).rejects.toBe(failure)
    expect(forceTerminateAndWait).toHaveBeenCalledWith('sample-execute')
  })

  it('forcibly reaps an owned subtree on deadline expiry', async () => {
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)

    await expect(settleOwnedOperation({
      result: new Promise<never>(() => undefined),
      forceTerminateAndWait,
    }, 'authority-prepare', 10, elapsedScheduler())).rejects.toBeInstanceOf(
      NetworkMatrixDeadlineExceeded,
    )
    expect(forceTerminateAndWait).toHaveBeenCalledWith('authority-prepare')
  })

  it('preserves both the primary failure and failed containment proof', async () => {
    const primaryFailure = new Error('child rejected')
    const cleanupFailure = new Error('subtree could not be reaped')

    await expect(settleOwnedOperation({
      result: Promise.reject(primaryFailure),
      forceTerminateAndWait: vi.fn().mockRejectedValue(cleanupFailure),
    }, 'authority-close', 10, neverElapsedScheduler())).rejects.toMatchObject({
      name: 'NetworkMatrixOwnershipCleanupError',
      operationClass: 'authority-close',
      primaryFailure,
      cleanupFailure,
      errors: [primaryFailure, cleanupFailure],
    } satisfies Partial<NetworkMatrixOwnershipCleanupError>)
  })
})

function elapsedScheduler(): NetworkMatrixDeadlineScheduler {
  return {
    schedule: () => ({ elapsed: Promise.resolve(), cancel: () => undefined }),
  }
}

function neverElapsedScheduler(): NetworkMatrixDeadlineScheduler {
  return {
    schedule: () => ({ elapsed: new Promise<void>(() => undefined), cancel: () => undefined }),
  }
}
