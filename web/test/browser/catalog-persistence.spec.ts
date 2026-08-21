import { expect, test } from '@playwright/test'

const PROBE_MODULE = '/test/browser/catalog-persistence-probe.ts'

test.beforeEach(async ({ page }) => {
  await page.goto('/')
})

test('rejects same-page node and portable-name collisions before persistence', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeSamePageCollisions()
  }, PROBE_MODULE)

  expect(result).toEqual({
    nodeCollisionRejected: true,
    nameCollisionRejected: true,
    failedPageKeyRemainedReusable: true,
  })
})

test('enforces atomic ownership across pages, directories, and shares', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeCrossPageOwnership()
  }, PROBE_MODULE)

  expect(result).toEqual({
    crossPageNameRejected: true,
    crossPageNodeRejected: true,
    crossDirectoryNodeRejected: true,
    failedNodeOwnershipRolledBack: true,
    failedNameOwnershipRolledBack: true,
    namesRemainDirectoryScoped: true,
    ownershipRemainsShareScoped: true,
  })
})

test('preserves commits and cleans abort and crash residue after reopen', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeCommitAbortAndReopen()
  }, PROBE_MODULE)

  expect(result).toEqual({
    commitSurvivedReopen: true,
    abortSurvivedReopen: true,
    abortReleasedPageAndOwnershipKeys: true,
    crashResidueRejected: true,
    beginReleasedCrashResidue: true,
  })
})

test('queries exact names only through the currently committed generation', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeCommittedNameMembership()
  }, PROBE_MODULE)

  expect(result).toEqual({
    stagedGenerationUnavailable: true,
    exactNameFound: true,
    portableCollisionFound: true,
    absentNameRejected: true,
    staleGenerationRejected: true,
    reopenFound: true,
  })
})

test('rejects a signed synthetic-root child before IndexedDB commit', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeSignedRootCollision()
  }, PROBE_MODULE)

  expect(result).toEqual({
    signedTrafficRejected: true,
    protocolFailureReported: true,
    noCommitBeforeClose: true,
    noCommitAfterReopen: true,
  })
})

test('reports signed IndexedDB ownership collisions as authenticated protocol failures', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeSignedOwnershipCollisions()
  }, PROBE_MODULE)

  expect(result).toEqual({
    crossPageNameRejected: true,
    crossPageNodeRejected: true,
    crossDirectoryNodeRejected: true,
    everyCollisionReportedAsProtocol: true,
    firstDirectoryCommitSurvived: true,
  })
})

test('keeps composite scope keys isolated from delimiter aliases', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeCompositeKeyIsolation()
  }, PROBE_MODULE)

  expect(result).toEqual({
    collidingLegacyKeysCoexisted: true,
    crossShareAbortStayedScoped: true,
    delimiterNameRejected: true,
  })
})

test('fails a blocked catalog upgrade closed and closes its eventual late success', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeBlockedUpgrade()
  }, PROBE_MODULE)

  expect(result).toEqual({
    blockedRejected: true,
    actionableMessage: true,
    freshOpenSucceeded: true,
  })
})

test('persists permanent and retryable failure authority across reopen', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeFailurePersistence()
  }, PROBE_MODULE)

  expect(result).toEqual({
    permanentReused: true,
    cooldownReused: true,
    transportSkippedBeforeCooldown: true,
    postCooldownFetch: true,
    permanentSurvivedReopen: true,
    retryableClearedByAttempt: true,
  })
})

test('charges aggregate catalog budgets atomically and recovers release authority', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeAggregateBudgetAuthority()
  }, PROBE_MODULE)

  expect(result).toEqual({
    shareRaceWasAtomic: true,
    profileLimitRejected: true,
    recoveredChargeRejected: true,
    abortReleasedBudget: true,
  })
})

test('evicts only inactive share caches and keeps release idempotent', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeInactiveShareEviction()
  }, PROBE_MODULE)

  expect(result).toEqual({
    activeShareProtected: true,
    activeExplicitEvictionRejected: true,
    inactiveShareEvicted: true,
    repeatedEvictionWasIdempotent: true,
    committedRemoved: true,
  })
})

test('fails malformed durable budget charges closed without hanging cleanup', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeMalformedBudgetChargeFailsClosed()
  }, PROBE_MODULE)

  expect(result).toEqual({ rejectedClosed: true, didNotHang: true })
})

test('recovers activity authority after eviction failure and close races', async ({ page }) => {
  const result = await page.evaluate(async (modulePath) => {
    const probe = await import(modulePath) as typeof import('./catalog-persistence-probe')
    return probe.probeEvictionLifecycleRecovery()
  }, PROBE_MODULE)

  expect(result).toEqual({
    injectedFailureRejected: true,
    failedEvictionReacquiredProtection: true,
    closeDuringEvictionLeftStoreClosed: true,
    activityLockWasReleased: true,
  })
})
