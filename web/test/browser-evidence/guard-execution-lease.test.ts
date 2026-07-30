import { describe, expect, it } from 'vitest'

import {
  assertGuardExecutionWindowUsable,
  GuardExecutionLease,
  GuardExecutionUnsettledError,
} from '../../scripts/browser-evidence/execution/guard-execution-lease.ts'

describe('guard execution lease', () => {
  it('rejects structurally forged native operation windows', () => {
    expect(() => assertGuardExecutionWindowUsable({
      signal: AbortSignal.timeout(1_000),
      maximumDurationMs: 1_000,
    })).toThrow(/lacks an issued execution window/u)
  })

  it('withholds every operation authority after a primary operation ignores cancellation', async () => {
    const lease = GuardExecutionLease.start({
      totalBudgetMs: 2_500,
      cleanupReserveMs: 2_200,
      nativeOperationBudgetMs: 100,
    })
    const preissuedCleanupWindow = lease.cleanupWindow('preissued native cleanup')
    let laterPrimaryStarted = false
    let cleanupStarted = false

    await expect(lease.runPrimary(
      'noncooperative primary operation',
      () => new Promise<never>(() => undefined),
    )).rejects.toBeInstanceOf(GuardExecutionUnsettledError)

    expect(() => lease.assertCleanupSafe('direct cleanup assertion'))
      .toThrow(GuardExecutionUnsettledError)
    expect(() => lease.throwIfPrimaryExpired('primary deadline assertion'))
      .toThrow(GuardExecutionUnsettledError)
    expect(() => lease.primarySignal('streaming primary operation'))
      .toThrow(GuardExecutionUnsettledError)
    expect(() => lease.primaryWindow('native primary operation'))
      .toThrow(GuardExecutionUnsettledError)
    expect(() => lease.cleanupWindow('native cleanup'))
      .toThrow(GuardExecutionUnsettledError)
    expect(() => assertGuardExecutionWindowUsable(preissuedCleanupWindow))
      .toThrow(GuardExecutionUnsettledError)
    await expect(lease.runPrimary('later primary operation', async () => {
      laterPrimaryStarted = true
    })).rejects.toBeInstanceOf(GuardExecutionUnsettledError)
    await expect(lease.runCleanup('in-process cleanup', async () => {
      cleanupStarted = true
    })).rejects.toBeInstanceOf(GuardExecutionUnsettledError)
    expect(laterPrimaryStarted).toBe(false)
    expect(cleanupStarted).toBe(false)
  }, 5_000)
})
