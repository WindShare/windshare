import assert from 'node:assert/strict'

import { browserRunPolicy } from '../../../web/scripts/browser-evidence/run-policy.ts'
import * as deadlineModule from './operation-deadlines.mjs'

const {
  BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
  BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS,
  BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS,
  BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS,
  BROWSERGATE_OPERATION_CLASS,
  BROWSERGATE_OPERATION_DEADLINE_MS,
  BROWSERGATE_OPERATION_PHASE,
  BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
  BROWSERGATE_RUNTIME_ARTIFACT_BUILD_DEADLINE_MS,
  BROWSERGATE_RUNTIME_ARTIFACT_PREFLIGHT_DEADLINE_MS,
  BROWSERGATE_RUNTIME_RUNNER,
  BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS,
  BROWSERGATE_VERDICT_JOB_MAXIMUM_MS,
  BROWSERGATE_VERDICT_REDUCER_DEADLINE_MS,
  BROWSERGATE_WARM_RUNTIME_SLO_MS,
  BROWSER_SAMPLE_SUBPROCESS_TIMEOUT_MS,
  createBootstrapDeadlineAuthority,
  createContractRunnerDeadlinePolicy,
  createGithubSuiteJobDeadlinePolicy,
  createGithubVerdictDeadlinePolicy,
  createLocalBrowsergateDeadlinePolicy,
  createOperationDeadlineAuthority,
  createRuntimeSetupDeadlinePolicy,
  createSuiteDeadlinePolicy,
  createWindowsProcessOwnerDeadlinePolicy,
  evaluateBrowsergateWarmRuntimeSlo,
  operationClassDeadlineMs,
} = deadlineModule

const TEST_CHECKOUT_SHA = 'a'.repeat(40)
const TEST_CONTEXT_ID = 'browsergate-test-context'

assert.equal('INVOCATION_OPERATION_DEADLINES' in deadlineModule, false)
assert.equal(
  Object.keys(BROWSERGATE_OPERATION_DEADLINE_MS).length,
  Object.keys(BROWSERGATE_OPERATION_CLASS).length,
)
for (const operationClass of Object.values(BROWSERGATE_OPERATION_CLASS)) {
  assert.equal(operationClassDeadlineMs(operationClass), BROWSERGATE_OPERATION_DEADLINE_MS[operationClass])
  assert(Number.isSafeInteger(operationClassDeadlineMs(operationClass)))
  assert(operationClassDeadlineMs(operationClass) > 0)
}
assert.throws(() => operationClassDeadlineMs('suite-wide-d5'), /unknown Browsergate operation class/u)

const expectedSuitePolicies = Object.freeze({
  blocking: Object.freeze({
    focusedProcessLeaseCount: 3,
    normalWorkBudgetMs: 34.25 * 60_000,
    finalizationReserveMs: 9 * 60_000,
    hardBudgetMs: 43.25 * 60_000,
  }),
  closure: Object.freeze({
    focusedProcessLeaseCount: 9,
    normalWorkBudgetMs: 72.75 * 60_000,
    finalizationReserveMs: 9 * 60_000,
    hardBudgetMs: 81.75 * 60_000,
  }),
  stability: Object.freeze({
    focusedProcessLeaseCount: 15,
    normalWorkBudgetMs: 111.25 * 60_000,
    finalizationReserveMs: 9 * 60_000,
    hardBudgetMs: 120.25 * 60_000,
  }),
})

for (const [policyId, expected] of Object.entries(expectedSuitePolicies)) {
  for (const suite of ['main', 'pion']) {
    const policy = createSuiteDeadlinePolicy(suite, browserRunPolicy(policyId))
    assert.equal(policy.suite, suite)
    assert.equal(policy.focusedProcessLeaseCount, expected.focusedProcessLeaseCount)
    assert.equal(policy.remainderProcessLeaseCount, 1)
    assert.equal(policy.normalWorkBudgetMs, expected.normalWorkBudgetMs)
    assert.equal(policy.finalizationReserveMs, expected.finalizationReserveMs)
    assert.equal(policy.hardBudgetMs, expected.hardBudgetMs)
    assert.equal(new Set(policy.leases.map(({ leaseId }) => leaseId)).size, policy.leases.length)
    assert.equal(
      policy.leases.filter(({ processRole }) => processRole === 'focused').length,
      expected.focusedProcessLeaseCount,
    )
    assert.equal(
      policy.leases.filter(({ processRole }) => processRole === 'remainder').length,
      1,
    )
    assert.deepEqual(
      policy.leases.filter(({ phase }) => phase === BROWSERGATE_OPERATION_PHASE.FINALIZATION)
        .map(({ operationClass }) => operationClass),
      [
        BROWSERGATE_OPERATION_CLASS.ARTIFACT_GUARD,
        BROWSERGATE_OPERATION_CLASS.RUNTIME_TEARDOWN,
      ],
    )
  }
}

const expectedLocalHardBudgetMs = Object.freeze({
  blocking: 8_460_000,
  closure: 13_080_000,
  stability: 17_700_000,
})
for (const policyId of ['blocking', 'closure', 'stability']) {
  const local = createLocalBrowsergateDeadlinePolicy(browserRunPolicy(policyId), 'linux')
  assert.equal(
    local.focusedProcessLeaseCount,
    expectedSuitePolicies[policyId].focusedProcessLeaseCount * 2,
  )
  assert.equal(local.remainderProcessLeaseCount, 2)
  assert.equal(local.suites.main.suite, 'main')
  assert.equal(local.suites.pion.suite, 'pion')
  assert.notEqual(local.suites.main, local.suites.pion)
  assert.equal(local.bootstrap.budgetMs, BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS)
  assert.deepEqual(local.bootstrap.leases.map(({ leaseId, maximumDurationMs }) => ({
    leaseId,
    maximumDurationMs,
  })), [{
    leaseId: 'bootstrap/source-control-context-query',
    maximumDurationMs: 30_000,
  }])
  assert.equal(local.sharedSetup.budgetMs, 46 * 60_000)
  assert.equal(local.hardBudgetMs, expectedLocalHardBudgetMs[policyId])
  assert.deepEqual(local.sharedSetup.leases.map(({ leaseId, maximumDurationMs }) => ({
    leaseId,
    maximumDurationMs,
  })), [
    { leaseId: 'local/dependency-install', maximumDurationMs: 600_000 },
    { leaseId: 'local/browser-contract', maximumDurationMs: 300_000 },
    { leaseId: 'runtime/batch-build', maximumDurationMs: 600_000 },
    { leaseId: 'runtime/manifest-preflight', maximumDurationMs: 180_000 },
    { leaseId: 'local/browser-install', maximumDurationMs: 900_000 },
    { leaseId: 'local/browser-preflight', maximumDurationMs: 180_000 },
  ])
  assert.deepEqual(
    local.sharedSetup.runtimeSetup.leases.map(({ runtimeStage }) => runtimeStage),
    ['batch-build', 'manifest-preflight'],
  )
}
const localDependencyReuse = createLocalBrowsergateDeadlinePolicy(
  browserRunPolicy('blocking'),
  'linux',
  { dependencyInstallReused: true },
)
assert.equal(localDependencyReuse.sharedSetup.budgetMs, 36 * 60_000)
assert.equal(localDependencyReuse.hardBudgetMs, 7_860_000)
assert.equal(
  BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
  30_000,
)
assert.equal(
  BROWSERGATE_OPERATION_DEADLINE_MS[BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY],
  30_000,
)
assert.equal(
  localDependencyReuse.sharedSetup.leases.some(
    ({ operationClass }) => operationClass === BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
  ),
  false,
)

const runtimePolicies = Object.freeze({
  contract: createRuntimeSetupDeadlinePolicy({ runner: BROWSERGATE_RUNTIME_RUNNER.CONTRACT }),
  local: createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.LOCAL,
    platform: 'linux',
  }),
  main: createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.GITHUB,
    suite: 'main',
    platform: 'linux',
  }),
  pion: createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.GITHUB,
    suite: 'pion',
    platform: 'linux',
  }),
})
assert.deepEqual(runtimePolicies.contract.manifestArtifacts, [])
assert.deepEqual(runtimePolicies.contract.leases, [])
assert.deepEqual(runtimePolicies.local.manifestArtifacts, [
  'linux-process-owner',
  'topology-materializer',
  'artifact-publisher',
  'pion-server',
])
assert.equal(runtimePolicies.main.manifestArtifacts.length, 3)
assert.equal(runtimePolicies.pion.manifestArtifacts.length, 4)
for (const policy of Object.values(runtimePolicies)) {
  const expectedStages = policy.manifestArtifacts.length === 0
    ? []
    : ['batch-build', 'manifest-preflight']
  assert.deepEqual(policy.leases.map(({ runtimeStage }) => runtimeStage), expectedStages)
  assert.deepEqual(
    policy.leases.map(({ maximumDurationMs }) => maximumDurationMs),
    policy.manifestArtifacts.length === 0
      ? []
      : [
          BROWSERGATE_RUNTIME_ARTIFACT_BUILD_DEADLINE_MS,
          BROWSERGATE_RUNTIME_ARTIFACT_PREFLIGHT_DEADLINE_MS,
        ],
  )
  assert.equal(policy.leases.length, policy.manifestArtifacts.length === 0 ? 0 : 2)
  assert.equal(new Set(policy.leases.map(({ leaseId }) => leaseId)).size, policy.leases.length)
}
assert.equal(runtimePolicies.local.budgetMs, 13 * 60_000)
assert.equal(runtimePolicies.main.budgetMs, 13 * 60_000)
assert.equal(runtimePolicies.pion.budgetMs, 13 * 60_000)

const contractRunner = createContractRunnerDeadlinePolicy()
assert.deepEqual(contractRunner.runtimeSetup.leases, [])
assert.deepEqual(
  contractRunner.leases.map(({ operationClass }) => operationClass),
  [
    BROWSERGATE_OPERATION_CLASS.DEPENDENCY_INSTALL,
    BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
  ],
)
assert.equal(contractRunner.hardBudgetMs, 15 * 60_000)

for (const [policyId, expected] of Object.entries(expectedSuitePolicies)) {
  const windows = createWindowsProcessOwnerDeadlinePolicy('main', browserRunPolicy(policyId))
  assert.equal(windows.focusedProcessLeaseCount, expected.focusedProcessLeaseCount)
  assert.equal(windows.remainderProcessLeaseCount, 1)
  assert.equal(windows.processLeases.length, expected.focusedProcessLeaseCount + 1)
  assert.equal(windows.processLeases.some(({ leaseId }) => leaseId.includes('suite-wide')), false)
  for (const lease of windows.processLeases) {
    assert.equal(lease.ownerSettlementReserveMs, BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS)
    assert.equal(
      lease.maximumDurationMs,
      lease.childDeadlineMs + BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
    )
    assert.equal(
      lease.childDeadlineMs,
      lease.processRole === 'focused'
        ? BROWSER_SAMPLE_SUBPROCESS_TIMEOUT_MS
        : BROWSERGATE_OPERATION_DEADLINE_MS[BROWSERGATE_OPERATION_CLASS.FULL_SUITE],
    )
  }
  assert.equal(
    windows.aggregateBudgetMs,
    expected.focusedProcessLeaseCount * (
      BROWSER_SAMPLE_SUBPROCESS_TIMEOUT_MS + BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS
    ) + BROWSERGATE_OPERATION_DEADLINE_MS[BROWSERGATE_OPERATION_CLASS.FULL_SUITE]
      + BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
  )
}

const githubMain = createGithubSuiteJobDeadlinePolicy(
  'main',
  browserRunPolicy('blocking'),
  'linux',
)
const githubPion = createGithubSuiteJobDeadlinePolicy(
  'pion',
  browserRunPolicy('blocking'),
  'linux',
)
assert.deepEqual(BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS, {
  CHECKOUT: 300_000,
  SETUP_GO: 600_000,
  SETUP_PNPM: 300_000,
  SETUP_NODE: 600_000,
})
assert.equal(
  Object.values(BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS)
    .reduce((sum, deadlineMs) => sum + deadlineMs, 0),
  1_800_000,
)
assert.deepEqual(
  githubMain.runnerSetup.workflowLeases.map(({ leaseId, maximumDurationMs }) => ({
    leaseId,
    maximumDurationMs,
  })),
  [
    { leaseId: 'github/checkout', maximumDurationMs: 300_000 },
    { leaseId: 'github/setup-go', maximumDurationMs: 600_000 },
    { leaseId: 'github/setup-pnpm', maximumDurationMs: 300_000 },
    { leaseId: 'github/setup-node', maximumDurationMs: 600_000 },
  ],
)
assert.equal(BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS, 300_000)
assert.equal(BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS, 600_000)
assert.equal(githubMain.runnerSetup.budgetMs, 4_260_000)
assert.equal(githubPion.runnerSetup.budgetMs, 4_260_000)
assert.equal(githubMain.hardBudgetMs, 7_755_000)
assert.equal(githubPion.hardBudgetMs, 7_755_000)
assert.equal(githubMain.minimumJobTimeoutMinutes, 130)
assert.equal(githubPion.minimumJobTimeoutMinutes, 130)
assert.equal(githubPion.hardBudgetMs, githubMain.hardBudgetMs)
assert.deepEqual(
  githubMain.runnerSetup.runtimeSetup.leases.map(({ runtimeStage }) => runtimeStage),
  ['batch-build', 'manifest-preflight'],
)
assert.deepEqual(
  githubPion.runnerSetup.runtimeSetup.leases.map(({ runtimeStage }) => runtimeStage),
  ['batch-build', 'manifest-preflight'],
)
for (const policy of [githubMain, githubPion]) {
  assert.deepEqual(policy.runnerSetup.operationLeases.map(({ leaseId }) => leaseId), [
    `github/${policy.suite}/dependency-install`,
    'runtime/batch-build',
    'runtime/manifest-preflight',
    `github/${policy.suite}/browser-install`,
    `github/${policy.suite}/browser-preflight`,
  ])
  assert.equal(
    policy.runnerSetup.operationLeases.some(
      ({ operationClass }) => operationClass === BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
    ),
    false,
  )
  assert.equal(
    policy.runnerSetup.operationLeases
      .reduce((sum, lease) => sum + lease.maximumDurationMs, 0),
    2_460_000,
  )
  assert.equal(
    policy.runnerSetup.operationLeases.some(
      ({ operationClass }) => operationClass === BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
    ),
    false,
  )
}
assert.equal(githubMain.suitePolicy.focusedProcessLeaseCount, 3)
assert.equal(githubPion.suitePolicy.focusedProcessLeaseCount, 3)
assert.equal(githubMain.suitePolicy.hardBudgetMs, 2_595_000)
assert.equal(githubPion.suitePolicy.hardBudgetMs, 2_595_000)

const verdict = createGithubVerdictDeadlinePolicy()
assert.equal(verdict.steps.length, 4)
assert.equal(verdict.operationBudgetMs, 9 * 60_000)
assert.equal(verdict.finalizationReserveMs, BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS)
assert.equal(verdict.hardBudgetMs, 14 * 60_000)
assert.equal(verdict.maximumJobBudgetMs, BROWSERGATE_VERDICT_JOB_MAXIMUM_MS)
assert.equal(BROWSERGATE_VERDICT_REDUCER_DEADLINE_MS, 3 * 60_000)
assert(verdict.hardBudgetMs < verdict.maximumJobBudgetMs)

const bootstrapClock = createFakeClock(5_000)
const bootstrapTrace = []
const bootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/main',
  clock: bootstrapClock.clock,
  trace: (event) => bootstrapTrace.push(event),
})
assert.equal(bootstrapClock.readCount, 0)
const bootstrapQueryGrant = bootstrap.grantQuery()
assert.equal(bootstrapClock.readCount, 1)
assert.equal(bootstrapQueryGrant.outcome, 'authorized')
assert.equal(bootstrapQueryGrant.timeoutMs, BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS)
assert.equal(
  bootstrapQueryGrant.expiresAtMs - bootstrapQueryGrant.authorizedAtMs,
  BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
)
assert.equal(bootstrap.grantQuery().reason, 'bootstrap-query-already-granted')
bootstrapClock.advance(125)
const handoffAtMs = bootstrapQueryGrant.authorizedAtMs + 125
const readsBeforeHandoff = bootstrapClock.readCount
const boundAuthority = handoffSuite(bootstrap, {
  grant: bootstrapQueryGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: TEST_CONTEXT_ID,
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
})
assert.equal(bootstrapClock.readCount, readsBeforeHandoff + 1)
assert.equal(boundAuthority.checkoutSha, TEST_CHECKOUT_SHA)
assert.equal(boundAuthority.contextId, TEST_CONTEXT_ID)
const boundSnapshot = boundAuthority.snapshot()
assert.equal(boundSnapshot.checkoutSha, TEST_CHECKOUT_SHA)
assert.equal(boundSnapshot.contextId, TEST_CONTEXT_ID)
assert.equal(boundSnapshot.epochMs, handoffAtMs)
assert.equal(boundSnapshot.elapsedMs, 0)
const creationTrace = bootstrapTrace.find(
  ({ milestone }) => milestone === 'deadline-authority-created',
)
assert.equal(creationTrace.checkoutSha, TEST_CHECKOUT_SHA)
assert.equal(creationTrace.contextId, TEST_CONTEXT_ID)
assert.equal(
  bootstrapTrace.some(({ milestone }) => milestone === 'bootstrap-deadline-handoff-completed'),
  true,
)
assert.equal(bootstrap.snapshot().state, 'handed-off')
assert.throws(
  () => handoffSuite(bootstrap, {
    grant: bootstrapQueryGrant,
    queryOutcome: 'succeeded',
    checkoutSha: 'b'.repeat(40),
    contextId: 'cross-context-after-completion',
    suite: 'pion',
    runPolicy: browserRunPolicy('closure'),
  }),
  /already handed off/u,
)

const exactExpiryClock = createFakeClock(10_000)
const exactExpiryBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/exact-expiry',
  clock: exactExpiryClock.clock,
})
const exactExpiryGrant = exactExpiryBootstrap.grantQuery()
exactExpiryClock.advance(BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS)
const exactExpiryAuthority = handoffSuite(exactExpiryBootstrap, {
  grant: exactExpiryGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: 'exact-expiry-context',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
})
assert.equal(exactExpiryAuthority.snapshot().epochMs, exactExpiryGrant.expiresAtMs)

const staleBootstrapClock = createFakeClock(20_000)
const staleBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/stale',
  clock: staleBootstrapClock.clock,
})
const staleGrant = staleBootstrap.grantQuery()
staleBootstrapClock.advance(BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS + 1)
assert.throws(
  () => handoffSuite(staleBootstrap, {
    grant: staleGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'stale-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /expired before handoff/u,
)

const rejectedBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/rejected',
  clock: createFakeClock(30_000).clock,
})
const rejectedGrant = rejectedBootstrap.grantQuery()
assert.throws(
  () => handoffSuite(rejectedBootstrap, {
    grant: rejectedGrant,
    queryOutcome: 'failed',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'rejected-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /successful query outcome/u,
)
assert.equal(rejectedBootstrap.snapshot().state, 'query-failed')
assert.throws(
  () => handoffSuite(rejectedBootstrap, {
    grant: rejectedGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'rejected-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /query failed and cannot be handed off/u,
)

const foreignBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/foreign-owner',
  clock: createFakeClock(40_000).clock,
})
const foreignOwnerGrant = foreignBootstrap.grantQuery()
const foreignGrant = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/foreign-grant',
  clock: createFakeClock(40_000).clock,
}).grantQuery()
assert.throws(
  () => handoffSuite(foreignBootstrap, {
    grant: foreignGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'foreign-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /its authorized query grant/u,
)
assert.equal(foreignBootstrap.snapshot().state, 'handoff-failed')
assert.throws(
  () => handoffSuite(foreignBootstrap, {
    grant: foreignOwnerGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'foreign-owner-retry',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /handoff failed and cannot be retried/u,
)

let traceReentryBootstrap
let traceReentryGrant
let traceReentryAuthority
let traceReentryError
let traceReentryAttempted = false
const traceReentryClock = createFakeClock(45_000)
traceReentryBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/trace-reentry',
  clock: traceReentryClock.clock,
  trace: (event) => {
    if (event.milestone !== 'deadline-authority-created' || traceReentryAttempted) return
    traceReentryAttempted = true
    try {
      traceReentryAuthority = handoffSuite(traceReentryBootstrap, {
        grant: traceReentryGrant,
        queryOutcome: 'succeeded',
        checkoutSha: 'b'.repeat(40),
        contextId: 'trace-reentry-context',
        suite: 'pion',
        runPolicy: browserRunPolicy('closure'),
      })
    } catch (cause) {
      traceReentryError = cause
    }
  },
})
traceReentryGrant = traceReentryBootstrap.grantQuery()
const traceReentryOuter = handoffSuite(traceReentryBootstrap, {
  grant: traceReentryGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: 'trace-outer-context',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
})
assert.equal(traceReentryAuthority, undefined)
assert.match(traceReentryError.message, /already consuming its query grant/u)
assert.equal(traceReentryOuter.checkoutSha, TEST_CHECKOUT_SHA)
assert.equal(traceReentryOuter.contextId, 'trace-outer-context')
assert.equal(traceReentryOuter.policy.suite, 'main')
assert.equal(traceReentryBootstrap.snapshot().state, 'handed-off')

let clockReentryBootstrap
let clockReentryGrant
let clockReentryAuthority
let clockReentryError
let clockReadCount = 0
let clockReentryAttempted = false
const reentrantClock = Object.freeze({
  now: () => {
    clockReadCount += 1
    if (clockReadCount === 2 && !clockReentryAttempted) {
      clockReentryAttempted = true
      try {
        clockReentryAuthority = handoffSuite(clockReentryBootstrap, {
          grant: clockReentryGrant,
          queryOutcome: 'succeeded',
          checkoutSha: 'c'.repeat(40),
          contextId: 'clock-reentry-context',
          suite: 'pion',
          runPolicy: browserRunPolicy('stability'),
        })
      } catch (cause) {
        clockReentryError = cause
      }
    }
    return 50_000
  },
})
clockReentryBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/clock-reentry',
  clock: reentrantClock,
})
clockReentryGrant = clockReentryBootstrap.grantQuery()
const clockReentryOuter = handoffSuite(clockReentryBootstrap, {
  grant: clockReentryGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: 'clock-outer-context',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
})
assert.equal(clockReentryAuthority, undefined)
assert.match(clockReentryError.message, /already consuming its query grant/u)
assert.equal(clockReentryOuter.checkoutSha, TEST_CHECKOUT_SHA)
assert.equal(clockReentryOuter.contextId, 'clock-outer-context')
assert.equal(clockReentryOuter.policy.suite, 'main')
assert.equal(clockReentryBootstrap.snapshot().state, 'handed-off')

let fatalClockBootstrap
let fatalClockGrant
let fatalClockReadCount = 0
const fatalReentrantClock = Object.freeze({
  now: () => {
    fatalClockReadCount += 1
    if (fatalClockReadCount === 2) {
      handoffSuite(fatalClockBootstrap, {
        grant: fatalClockGrant,
        queryOutcome: 'succeeded',
        checkoutSha: 'd'.repeat(40),
        contextId: 'fatal-clock-reentry-context',
        suite: 'pion',
        runPolicy: browserRunPolicy('closure'),
      })
    }
    return 52_000
  },
})
fatalClockBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/fatal-clock-reentry',
  clock: fatalReentrantClock,
})
fatalClockGrant = fatalClockBootstrap.grantQuery()
assert.throws(
  () => handoffSuite(fatalClockBootstrap, {
    grant: fatalClockGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'fatal-clock-outer-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /already consuming its query grant/u,
)
assert.equal(fatalClockBootstrap.snapshot().state, 'handoff-failed')
assert.throws(
  () => handoffSuite(fatalClockBootstrap, {
    grant: fatalClockGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'fatal-clock-retry-context',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /handoff failed and cannot be retried/u,
)

const accessorHandoffBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/accessor-handoff',
  clock: createFakeClock(55_000).clock,
})
const accessorHandoffGrant = accessorHandoffBootstrap.grantQuery()
let accessorHandoffReads = 0
const accessorHandoffOptions = {
  grant: accessorHandoffGrant,
  queryOutcome: 'succeeded',
  contextId: 'accessor-handoff-context',
  policy: createSuiteDeadlinePolicy('main', browserRunPolicy('blocking')),
}
Object.defineProperty(accessorHandoffOptions, 'checkoutSha', {
  enumerable: true,
  get() {
    accessorHandoffReads += 1
    return TEST_CHECKOUT_SHA
  },
})
assert.throws(
  () => accessorHandoffBootstrap.handoff(accessorHandoffOptions),
  /enumerable data property/u,
)
assert.equal(accessorHandoffReads, 0)
assert.equal(accessorHandoffBootstrap.snapshot().state, 'handoff-failed')
assert.equal(accessorHandoffBootstrap.grantQuery().reason, 'bootstrap-authority-not-pending')
assert.throws(
  () => handoffSuite(accessorHandoffBootstrap, {
    grant: accessorHandoffGrant,
    queryOutcome: 'succeeded',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: 'accessor-handoff-retry',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /handoff failed and cannot be retried/u,
)

const proxyHandoffBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/proxy-handoff',
  clock: createFakeClock(60_000).clock,
})
const proxyHandoffGrant = proxyHandoffBootstrap.grantQuery()
let proxyHandoffReads = 0
const proxyHandoffOptions = new Proxy({
  grant: proxyHandoffGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: 'proxy-handoff-context',
  policy: createSuiteDeadlinePolicy('main', browserRunPolicy('blocking')),
}, {
  get(target, property, receiver) {
    proxyHandoffReads += 1
    return Reflect.get(target, property, receiver)
  },
})
assert.throws(
  () => proxyHandoffBootstrap.handoff(proxyHandoffOptions),
  /plain own-data record/u,
)
assert.equal(proxyHandoffReads, 0)
assert.equal(proxyHandoffBootstrap.snapshot().state, 'handoff-failed')

let authorityOptionAccessorReads = 0
const accessorAuthorityOptions = {
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: TEST_CONTEXT_ID,
  policy: createSuiteDeadlinePolicy('main', browserRunPolicy('blocking')),
}
Object.defineProperty(accessorAuthorityOptions, 'entryId', {
  enumerable: true,
  get() {
    authorityOptionAccessorReads += 1
    return 'accessor-options/main'
  },
})
assert.throws(
  () => createOperationDeadlineAuthority(accessorAuthorityOptions),
  /enumerable data property/u,
)
assert.equal(authorityOptionAccessorReads, 0)
assert.throws(
  () => createOperationDeadlineAuthority(new Proxy({
    entryId: 'proxy-options/main',
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: TEST_CONTEXT_ID,
    policy: createSuiteDeadlinePolicy('main', browserRunPolicy('blocking')),
  }, {})),
  /plain own-data record/u,
)

let clockAccessorReads = 0
const accessorClock = {}
Object.defineProperty(accessorClock, 'now', {
  enumerable: true,
  get() {
    clockAccessorReads += 1
    return () => 0
  },
})
assert.throws(
  () => createBootstrapDeadlineAuthority({
    entryId: 'bootstrap/accessor-clock',
    clock: accessorClock,
  }),
  /enumerable data property/u,
)
assert.equal(clockAccessorReads, 0)
assert.throws(
  () => createBootstrapDeadlineAuthority({
    entryId: 'bootstrap/proxy-clock',
    clock: new Proxy({ now: () => 0 }, {}),
  }),
  /plain own-data record/u,
)
assert.throws(
  () => createBootstrapDeadlineAuthority({
    entryId: 'bootstrap/proxy-trace',
    trace: new Proxy(() => undefined, {}),
  }),
  /trace must be a function/u,
)

const snapshottedTraceEvents = []
let replacementTraceCalls = 0
const snapshottedTraceOptions = {
  entryId: 'bootstrap/snapshotted-trace',
  trace: (event) => snapshottedTraceEvents.push(event),
}
const snapshottedTraceBootstrap = createBootstrapDeadlineAuthority(snapshottedTraceOptions)
snapshottedTraceOptions.trace = () => { replacementTraceCalls += 1 }
snapshottedTraceBootstrap.grantQuery()
assert.deepEqual(
  snapshottedTraceEvents.map(({ milestone }) => milestone),
  ['bootstrap-deadline-authority-created', 'bootstrap-query-lease-authorized'],
)
assert.equal(replacementTraceCalls, 0)

const mutableClock = { now: () => 70_000 }
const snapshottedClockAuthority = createTestOperationDeadlineAuthority({
  entryId: 'snapshotted-clock/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: mutableClock,
})
mutableClock.now = () => 1
assert.equal(snapshottedClockAuthority.snapshot().epochMs, 70_000)
assert.equal(snapshottedClockAuthority.snapshot().elapsedMs, 0)

const throwingTraceClock = createFakeClock(75_000)
const throwingTraceBootstrap = createBootstrapDeadlineAuthority({
  entryId: 'bootstrap/throwing-trace',
  clock: throwingTraceClock.clock,
  trace: () => { throw new Error('hostile trace sink') },
})
const throwingTraceGrant = throwingTraceBootstrap.grantQuery()
const throwingTraceAuthority = handoffSuite(throwingTraceBootstrap, {
  grant: throwingTraceGrant,
  queryOutcome: 'succeeded',
  checkoutSha: TEST_CHECKOUT_SHA,
  contextId: 'throwing-trace-context',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
})
assert.equal(throwingTraceBootstrap.snapshot().state, 'handed-off')
assert.equal(throwingTraceBootstrap.snapshot().traceFailureCount, 3)
assert.equal(
  throwingTraceAuthority.grant(throwingTraceAuthority.policy.leases[0].leaseId).outcome,
  'authorized',
)
assert.equal(throwingTraceAuthority.snapshot().traceFailureCount, 2)

for (const policyId of ['blocking', 'closure', 'stability']) {
  const clock = createFakeClock(1_000)
  const trace = []
  const authority = createTestOperationDeadlineAuthority({
    entryId: `run-a/${policyId}/main`,
    suite: 'main',
    runPolicy: browserRunPolicy(policyId),
    clock: clock.clock,
    trace: (event) => trace.push(event),
  })
  const normalLeases = authority.policy.leases.filter(
    ({ phase }) => phase === BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
  )
  const finalLeases = authority.policy.leases.filter(
    ({ phase }) => phase === BROWSERGATE_OPERATION_PHASE.FINALIZATION,
  )
  for (const lease of normalLeases) {
    const grant = authority.grant(lease.leaseId)
    assert.equal(grant.outcome, 'authorized')
    assert.equal(grant.timeoutMs, lease.maximumDurationMs)
    clock.advance(lease.maximumDurationMs)
  }
  assert.equal(authority.snapshot().normalWorkRemainingMs, 0)
  assert.equal(
    authority.snapshot().protectedFinalizationReserveRemainingMs,
    authority.policy.finalizationReserveMs,
  )
  assert.equal(
    authority.grant(normalLeases[0].leaseId).outcome,
    'rejected',
  )
  // Finalization remains independently authorized even when normal work used
  // every millisecond of its graph; callers can therefore guard and clean up
  // after any preceding operation reports failure.
  for (const lease of finalLeases) {
    const grant = authority.grant(lease.leaseId)
    assert.equal(grant.outcome, 'authorized')
    assert.equal(grant.timeoutMs, lease.maximumDurationMs)
    clock.advance(lease.maximumDurationMs)
  }
  const terminal = authority.snapshot()
  assert.equal(terminal.hardDeadlineRemainingMs, 0)
  assert.equal(terminal.remainingLeaseCount, 0)
  assert.equal(trace[0].milestone, 'deadline-authority-created')
  assert.equal(
    trace.filter(({ milestone }) => milestone === 'deadline-lease-authorized').length,
    authority.policy.leases.length,
  )
}

const sharedClock = createFakeClock(10_000)
const first = createTestOperationDeadlineAuthority({
  entryId: 'sequential/main/first',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: sharedClock.clock,
})
const firstLeaseId = first.policy.leases[0].leaseId
assert.equal(first.grant(firstLeaseId).outcome, 'authorized')
sharedClock.advance(123)
const second = createTestOperationDeadlineAuthority({
  entryId: 'sequential/main/second',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: sharedClock.clock,
})
assert.equal(second.snapshot().elapsedMs, 0)
assert.equal(second.snapshot().grantedLeaseCount, 0)
assert.equal(second.grant(second.policy.leases[0].leaseId).grantSequence, 1)
assert.equal(first.snapshot().grantedLeaseCount, 1)

const concurrentClockA = createFakeClock(50_000)
const concurrentClockB = createFakeClock(90_000)
const concurrentA = createTestOperationDeadlineAuthority({
  entryId: 'concurrent/main/a',
  suite: 'main',
  runPolicy: browserRunPolicy('closure'),
  clock: concurrentClockA.clock,
})
const concurrentB = createTestOperationDeadlineAuthority({
  entryId: 'concurrent/main/b',
  suite: 'main',
  runPolicy: browserRunPolicy('closure'),
  clock: concurrentClockB.clock,
})
assert.equal(concurrentA.grant(concurrentA.policy.leases[0].leaseId).grantSequence, 1)
assert.equal(concurrentB.grant(concurrentB.policy.leases[0].leaseId).grantSequence, 1)
concurrentClockA.advance(concurrentA.policy.normalWorkBudgetMs)
assert.equal(concurrentA.snapshot().normalWorkRemainingMs, 0)
assert.equal(concurrentB.snapshot().normalWorkRemainingMs, concurrentB.policy.normalWorkBudgetMs)
assert.equal(concurrentB.snapshot().grantedLeaseCount, 1)

const traceFailureClock = createFakeClock(0)
const traceFailureAuthority = createTestOperationDeadlineAuthority({
  entryId: 'trace-failure/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: traceFailureClock.clock,
  trace: () => { throw new Error('hostile trace sink') },
})
assert.equal(
  traceFailureAuthority.grant(traceFailureAuthority.policy.leases[0].leaseId).outcome,
  'authorized',
)
assert.equal(traceFailureAuthority.snapshot().traceFailureCount, 2)

const monotonicClock = createFakeClock(100)
const monotonicAuthority = createTestOperationDeadlineAuthority({
  entryId: 'monotonic/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: monotonicClock.clock,
})
monotonicClock.set(99)
assert.throws(() => monotonicAuthority.snapshot(), /clock must be monotonic/u)
assert.throws(
  () => createTestOperationDeadlineAuthority({
    entryId: 'invalid-clock/main',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
    clock: { now: () => Number.NaN },
  }),
  /finite timestamp/u,
)
assert.throws(
  () => createTestOperationDeadlineAuthority({
    entryId: '',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /entry ID/u,
)
assert.throws(
  () => createTestOperationDeadlineAuthority({
    entryId: 'invalid-checkout/main',
    checkoutSha: 'A'.repeat(40),
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /checkout SHA/u,
)
assert.throws(
  () => createTestOperationDeadlineAuthority({
    entryId: 'invalid-context/main',
    contextId: 'line-one\nline-two',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }),
  /context ID/u,
)
assert.throws(
  () => createTestOperationDeadlineAuthority({
    entryId: 'unknown-lease/main',
    suite: 'main',
    runPolicy: browserRunPolicy('blocking'),
  }).grant('main/focused/chromium/sample-99'),
  /unknown Browsergate deadline lease/u,
)

let clockReentrantAuthority
let clockReentrantReadCount = 0
let sameLeaseReentryError
let otherLeaseReentryError
const clockReentrantClock = Object.freeze({
  now: () => {
    clockReentrantReadCount += 1
    if (clockReentrantReadCount === 2) {
      try {
        clockReentrantAuthority.grant(clockReentrantAuthority.policy.leases[0].leaseId)
      } catch (cause) {
        sameLeaseReentryError = cause
      }
      try {
        clockReentrantAuthority.grant(clockReentrantAuthority.policy.leases[1].leaseId)
      } catch (cause) {
        otherLeaseReentryError = cause
      }
    }
    return 0
  },
})
clockReentrantAuthority = createTestOperationDeadlineAuthority({
  entryId: 'clock-reentrant-lease/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: clockReentrantClock,
})
const clockOuterLease = clockReentrantAuthority.policy.leases[0].leaseId
const clockOtherLease = clockReentrantAuthority.policy.leases[1].leaseId
assert.equal(clockReentrantAuthority.grant(clockOuterLease).outcome, 'authorized')
assert.equal(sameLeaseReentryError.code, 'deadline-authorization-active')
assert.equal(otherLeaseReentryError.code, 'deadline-authorization-active')
assert.equal(clockReentrantAuthority.grant(clockOtherLease).outcome, 'authorized')

let observerAuthority
let observerReentryError
let observerAttempted = false
observerAuthority = createTestOperationDeadlineAuthority({
  entryId: 'observer-reentrant-lease/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  trace: (event) => {
    if (event.milestone !== 'deadline-lease-authorized' || observerAttempted) return
    observerAttempted = true
    try {
      observerAuthority.grant(observerAuthority.policy.leases[1].leaseId)
    } catch (cause) {
      observerReentryError = cause
    }
  },
})
assert.equal(observerAuthority.grant(observerAuthority.policy.leases[0].leaseId).outcome, 'authorized')
assert.equal(observerReentryError.code, 'deadline-observer-active')
assert.equal(observerAuthority.grant(observerAuthority.policy.leases[1].leaseId).outcome, 'authorized')

let invalidLeaseCoercionCount = 0
assert.throws(
  () => observerAuthority.grant({
    toJSON: () => {
      invalidLeaseCoercionCount += 1
      return 'unexpected-coercion'
    },
  }),
  (cause) => cause.code === 'deadline-lease-id-invalid',
)
assert.equal(invalidLeaseCoercionCount, 0)

let failingClockAuthority
let failingClockReadCount = 0
let failingClockReentryError
const failingClock = Object.freeze({
  now: () => {
    failingClockReadCount += 1
    if (failingClockReadCount === 2) {
      try {
        failingClockAuthority.grant(failingClockAuthority.policy.leases[1].leaseId)
      } catch (cause) {
        failingClockReentryError = cause
      }
      return Number.NaN
    }
    return 0
  },
})
failingClockAuthority = createTestOperationDeadlineAuthority({
  entryId: 'failing-clock-lease/main',
  suite: 'main',
  runPolicy: browserRunPolicy('blocking'),
  clock: failingClock,
})
const failedClockLease = failingClockAuthority.policy.leases[0].leaseId
assert.throws(() => failingClockAuthority.grant(failedClockLease), /finite timestamp/u)
assert.equal(failingClockReentryError.code, 'deadline-authorization-active')
assert.deepEqual(
  failingClockAuthority.grant(failedClockLease),
  {
    outcome: 'rejected',
    reason: 'lease-authorization-failed',
    leaseId: failedClockLease,
    operationClass: failingClockAuthority.policy.leases[0].operationClass,
    phase: failingClockAuthority.policy.leases[0].phase,
  },
)
assert.equal(
  failingClockAuthority.grant(failingClockAuthority.policy.leases[1].leaseId).outcome,
  'authorized',
)

const withinSlo = evaluateBrowsergateWarmRuntimeSlo({
  setup: 4 * 60_000,
  main: 5 * 60_000,
  pion: 5 * 60_000,
  finalization: 2 * 60_000,
})
assert.equal(withinSlo.targetMs, BROWSERGATE_WARM_RUNTIME_SLO_MS)
assert.equal(withinSlo.observedRuntimeMs, 16 * 60_000)
assert.equal(withinSlo.outcome, 'within-slo')
assert.equal(evaluateBrowsergateWarmRuntimeSlo({ all: 20 * 60_000 + 1 }).outcome, 'exceeded-slo')
assert.throws(
  () => evaluateBrowsergateWarmRuntimeSlo({ setup: -1 }),
  /non-negative integer/u,
)

console.log('browsergate versioned operation graph and independent deadline authorities: PASS')

function createTestOperationDeadlineAuthority(options) {
  const { suite, runPolicy, ...authorityOptions } = options
  return createOperationDeadlineAuthority({
    checkoutSha: TEST_CHECKOUT_SHA,
    contextId: TEST_CONTEXT_ID,
    ...authorityOptions,
    policy: createSuiteDeadlinePolicy(suite, runPolicy),
  })
}

function handoffSuite(authority, options) {
  const { suite, runPolicy, ...handoffOptions } = options
  return authority.handoff({
    ...handoffOptions,
    policy: createSuiteDeadlinePolicy(suite, runPolicy),
  })
}

function createFakeClock(initialMs) {
  let nowMs = initialMs
  let readCount = 0
  const clock = Object.freeze({
    now: () => {
      readCount += 1
      return nowMs
    },
  })
  return Object.freeze({
    clock,
    advance: (durationMs) => { nowMs += durationMs },
    set: (value) => { nowMs = value },
    get readCount() { return readCount },
  })
}
