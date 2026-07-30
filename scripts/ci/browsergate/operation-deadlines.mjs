import { performance } from 'node:perf_hooks'
import { createHash } from 'node:crypto'
import { types as nodeTypes } from 'node:util'

import { GUARD_SUITE_TOTAL_BUDGET_MS } from '../../../web/scripts/browser-evidence/execution/guard-execution-lease.ts'
import {
  WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS,
  WINDOWS_JOB_POST_KILL_LEASE_MS,
  WINDOWS_JOB_WATCHDOG_SLACK_MS,
} from '../../../web/scripts/browser-evidence/process/windows-job-client.ts'
import { parseBrowserRunPolicy } from '../../../web/scripts/browser-evidence/run-policy.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
} from '../../../web/scripts/browser-evidence/vocabulary.ts'

const MINUTE_MS = 60_000
const DEADLINE_POLICY_SCHEMA_VERSION = 1
const DEADLINE_POLICY_VERSION = 1
const MAXIMUM_ENTRY_ID_BYTES = 256
const MAXIMUM_CONTEXT_ID_BYTES = 256
const CHECKOUT_SHA_PATTERN = /^[0-9a-f]{40}$/u
const BOOTSTRAP_QUERY_LEASE_ID = 'bootstrap/source-control-context-query'
const SYSTEM_MONOTONIC_CLOCK = Object.freeze({ now: () => performance.now() })
const NOOP_TRACE = Object.freeze(() => undefined)
const BOOTSTRAP_AUTHORITY_OPTION_FIELDS = Object.freeze({
  required: Object.freeze(['entryId']),
  optional: Object.freeze(['clock', 'trace']),
})
const BOOTSTRAP_HANDOFF_OPTION_FIELDS = Object.freeze({
  required: Object.freeze([
    'grant',
    'queryOutcome',
    'checkoutSha',
    'contextId',
    'policy',
  ]),
  optional: Object.freeze([]),
})
const OPERATION_AUTHORITY_OPTION_FIELDS = Object.freeze({
  required: Object.freeze(['entryId', 'checkoutSha', 'contextId', 'policy']),
  optional: Object.freeze(['clock', 'trace']),
})
const MONOTONIC_CLOCK_FIELDS = Object.freeze({
  required: Object.freeze(['now']),
  optional: Object.freeze([]),
})
const LEASE_AUTHORIZATION_STATE = Object.freeze({
  PENDING: 'pending',
  CONSUMING: 'consuming',
  AUTHORIZED: 'authorized',
  FAILED: 'failed',
})
const DEADLINE_AUTHORIZATION_ERROR = Object.freeze({
  ACTIVE: Object.freeze({
    code: 'deadline-authorization-active',
    message: 'Browsergate deadline authorization is already active',
  }),
  OBSERVER_ACTIVE: Object.freeze({
    code: 'deadline-observer-active',
    message: 'Browsergate deadline authorization is unavailable during observer callback',
  }),
  INVALID_LEASE_ID: Object.freeze({
    code: 'deadline-lease-id-invalid',
    message: 'Browsergate deadline lease ID must be primitive text',
  }),
  UNKNOWN_LEASE: Object.freeze({
    code: 'deadline-lease-unknown',
    message: 'unknown Browsergate deadline lease',
  }),
})

class BrowsergateDeadlineAuthorizationError extends Error {
  constructor({ code, message }) {
    super(message)
    this.name = 'BrowsergateDeadlineAuthorizationError'
    this.code = code
  }
}

export const BROWSERGATE_OPERATION_PHASE = Object.freeze({
  NORMAL_WORK: 'normal-work',
  FINALIZATION: 'finalization',
})

export const BROWSERGATE_OPERATION_CLASS = Object.freeze({
  SOURCE_CONTROL_QUERY: 'source-control-query',
  GENERATED_SEMANTIC_PROCESS: 'generated-semantic-process',
  RUNTIME_BUILD: 'runtime-build',
  DEPENDENCY_INSTALL: 'dependency-install',
  BROWSER_INSTALL: 'browser-install',
  PREFLIGHT: 'preflight',
  RACE_TEST: 'race-test',
  TOPOLOGY_MATERIALIZATION: 'topology-materialization',
  CONTRACT_TEST: 'contract-test',
  BROWSER_SAMPLE: 'browser-sample',
  FULL_SUITE: 'full-suite',
  ARTIFACT_GUARD: 'artifact-guard',
  RUNTIME_TEARDOWN: 'runtime-teardown',
  VERDICT: 'verdict',
})

export const BROWSERGATE_RUNTIME_RUNNER = Object.freeze({
  CONTRACT: 'contract',
  LOCAL: 'local',
  GITHUB: 'github',
})

export const BROWSERGATE_WARM_RUNTIME_SLO_MS = 20 * MINUTE_MS
export const BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS = 30_000
export const BROWSERGATE_GENERATED_SEMANTIC_PROCESS_DEADLINE_MS = 300_000
export const BROWSER_SAMPLE_PROCESS_DEADLINE_MS = 300_000
export const BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS = 15_000
export const BROWSERGATE_RUNTIME_ARTIFACT_BUILD_DEADLINE_MS = 600_000
export const BROWSERGATE_RUNTIME_ARTIFACT_PREFLIGHT_DEADLINE_MS = 180_000
export const BROWSERGATE_RUNTIME_TEARDOWN_DEADLINE_MS = 300_000
export const BROWSERGATE_VERDICT_REDUCER_DEADLINE_MS = 180_000
export const BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS = 300_000
export const BROWSERGATE_VERDICT_JOB_MAXIMUM_MS = 15 * MINUTE_MS
export const BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS = Object.freeze({
  CHECKOUT: 300_000,
  SETUP_GO: 600_000,
  SETUP_PNPM: 300_000,
  SETUP_NODE: 600_000,
})
export const BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS = 300_000
export const BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS = 600_000

const WINDOWS_JOB_SUBPROCESS_LIFECYCLE_OVERHEAD_MS =
  WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS
  + WINDOWS_JOB_WATCHDOG_SLACK_MS
  + WINDOWS_JOB_POST_KILL_LEASE_MS

// A leaf owner must retain time to prove handle and process-tree settlement
// after the browser child reaches its own deadline.
export const BROWSER_SAMPLE_SUBPROCESS_TIMEOUT_MS =
  BROWSER_SAMPLE_PROCESS_DEADLINE_MS
  + WINDOWS_JOB_SUBPROCESS_LIFECYCLE_OVERHEAD_MS
  + BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS

export const BROWSERGATE_OPERATION_DEADLINE_MS = Object.freeze({
  [BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY]:
    BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
  [BROWSERGATE_OPERATION_CLASS.GENERATED_SEMANTIC_PROCESS]:
    BROWSERGATE_GENERATED_SEMANTIC_PROCESS_DEADLINE_MS,
  [BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD]:
    BROWSERGATE_RUNTIME_ARTIFACT_BUILD_DEADLINE_MS,
  [BROWSERGATE_OPERATION_CLASS.DEPENDENCY_INSTALL]: 600_000,
  [BROWSERGATE_OPERATION_CLASS.BROWSER_INSTALL]: 900_000,
  [BROWSERGATE_OPERATION_CLASS.PREFLIGHT]:
    BROWSERGATE_RUNTIME_ARTIFACT_PREFLIGHT_DEADLINE_MS,
  [BROWSERGATE_OPERATION_CLASS.RACE_TEST]: 600_000,
  [BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION]: 300_000,
  [BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST]: 300_000,
  [BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE]: BROWSER_SAMPLE_SUBPROCESS_TIMEOUT_MS,
  [BROWSERGATE_OPERATION_CLASS.FULL_SUITE]: 600_000,
  [BROWSERGATE_OPERATION_CLASS.ARTIFACT_GUARD]: GUARD_SUITE_TOTAL_BUDGET_MS,
  [BROWSERGATE_OPERATION_CLASS.RUNTIME_TEARDOWN]:
    BROWSERGATE_RUNTIME_TEARDOWN_DEADLINE_MS,
  [BROWSERGATE_OPERATION_CLASS.VERDICT]: BROWSERGATE_VERDICT_REDUCER_DEADLINE_MS,
})

const GITHUB_SUITE_RUNNER_SETUP_LEASES = Object.freeze([
  fixedLease(
    'github/checkout',
    'workflow-checkout',
    BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS.CHECKOUT,
  ),
  fixedLease(
    'github/setup-go',
    'workflow-setup-go',
    BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS.SETUP_GO,
  ),
  fixedLease(
    'github/setup-pnpm',
    'workflow-setup-pnpm',
    BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS.SETUP_PNPM,
  ),
  fixedLease(
    'github/setup-node',
    'workflow-setup-node',
    BROWSERGATE_GITHUB_RUNNER_ACTION_DEADLINE_MS.SETUP_NODE,
  ),
])

const GITHUB_VERDICT_STEP_LEASES = Object.freeze([
  fixedLease('verdict/checkout', 'workflow-checkout', 2 * MINUTE_MS),
  fixedLease('verdict/download-main', 'workflow-artifact-download', 2 * MINUTE_MS),
  fixedLease('verdict/download-pion', 'workflow-artifact-download', 2 * MINUTE_MS),
  operationLease(
    'verdict/reduce',
    BROWSERGATE_OPERATION_CLASS.VERDICT,
    BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
  ),
])

export function operationClassDeadlineMs(operationClass) {
  if (
    typeof operationClass !== 'string'
    || !Object.hasOwn(BROWSERGATE_OPERATION_DEADLINE_MS, operationClass)
  ) {
    throw new Error('unknown Browsergate operation class')
  }
  return BROWSERGATE_OPERATION_DEADLINE_MS[operationClass]
}

export function createSuiteDeadlinePolicy(suite, runPolicy) {
  const canonicalSuite = requireSuite(suite)
  const canonicalRunPolicy = parseBrowserRunPolicy(runPolicy, 'Browsergate deadline run policy')
  const focusedLeases = BROWSER_ENGINES.flatMap((browser) =>
    Array.from({ length: canonicalRunPolicy.sampleCount }, (_, index) => operationLease(
      `${canonicalSuite}/focused/${browser}/sample-${index + 1}`,
      BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      Object.freeze({ browser, sampleIndex: index + 1, processRole: 'focused' }),
    )))
  const leases = Object.freeze([
    operationLease(
      `${canonicalSuite}/topology`,
      BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    ...focusedLeases,
    operationLease(
      `${canonicalSuite}/remainder`,
      BROWSERGATE_OPERATION_CLASS.FULL_SUITE,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      Object.freeze({ processRole: 'remainder' }),
    ),
    operationLease(
      `${canonicalSuite}/guard-seal`,
      BROWSERGATE_OPERATION_CLASS.ARTIFACT_GUARD,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
    operationLease(
      `${canonicalSuite}/runtime-teardown`,
      BROWSERGATE_OPERATION_CLASS.RUNTIME_TEARDOWN,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
  ])
  const normalWorkBudgetMs = sumLeaseDurations(leases, BROWSERGATE_OPERATION_PHASE.NORMAL_WORK)
  const finalizationReserveMs = sumLeaseDurations(
    leases,
    BROWSERGATE_OPERATION_PHASE.FINALIZATION,
  )
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: `browser-suite-${canonicalRunPolicy.policyId}`,
    suite: canonicalSuite,
    runPolicy: canonicalRunPolicy,
    engineCount: BROWSER_ENGINES.length,
    focusedProcessLeaseCount: focusedLeases.length,
    remainderProcessLeaseCount: 1,
    leases,
    normalWorkBudgetMs,
    finalizationReserveMs,
    hardBudgetMs: normalWorkBudgetMs + finalizationReserveMs,
  })
}

export function createWindowsProcessOwnerDeadlinePolicy(suite, runPolicy) {
  const suitePolicy = createSuiteDeadlinePolicy(suite, runPolicy)
  const processLeases = suitePolicy.leases
    .filter(({ processRole }) => processRole !== undefined)
    .map((leaf) => Object.freeze({
      leaseId: `${leaf.leaseId}/windows-job-owner`,
      sourceLeaseId: leaf.leaseId,
      processRole: leaf.processRole,
      childDeadlineMs: leaf.maximumDurationMs,
      ownerSettlementReserveMs: BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
      maximumDurationMs:
        leaf.maximumDurationMs + BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
    }))
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: `windows-process-owner-${suitePolicy.runPolicy.policyId}`,
    suite: suitePolicy.suite,
    runPolicy: suitePolicy.runPolicy,
    focusedProcessLeaseCount: suitePolicy.focusedProcessLeaseCount,
    remainderProcessLeaseCount: suitePolicy.remainderProcessLeaseCount,
    processLeases: Object.freeze(processLeases),
    aggregateBudgetMs: processLeases.reduce(
      (sum, lease) => safeAdd(sum, lease.maximumDurationMs, 'Windows process-owner budget'),
      0,
    ),
  })
}

export function createRuntimeSetupDeadlinePolicy({
  runner,
  suite,
  platform = process.platform,
}) {
  const canonicalRunner = requireEnum(
    runner,
    Object.values(BROWSERGATE_RUNTIME_RUNNER),
    'Browsergate runtime runner',
  )
  if (canonicalRunner === BROWSERGATE_RUNTIME_RUNNER.CONTRACT) {
    if (suite !== undefined) throw new Error('contract runtime policy cannot bind a browser suite')
    return runtimeSetupPolicy(canonicalRunner, null, Object.freeze([]))
  }
  const canonicalPlatform = requireOwnedRuntimePlatform(platform)
  const canonicalSuite = canonicalRunner === BROWSERGATE_RUNTIME_RUNNER.GITHUB
    ? requireSuite(suite)
    : null
  if (canonicalRunner === BROWSERGATE_RUNTIME_RUNNER.LOCAL && suite !== undefined) {
    throw new Error('local runtime setup is shared and cannot bind one suite')
  }
  const manifestArtifacts = Object.freeze([
    canonicalPlatform === 'win32' ? 'windows-job' : 'linux-process-owner',
    'topology-materializer',
    'artifact-publisher',
    ...(canonicalRunner === BROWSERGATE_RUNTIME_RUNNER.LOCAL || canonicalSuite === 'pion'
      ? ['pion-server']
      : []),
  ])
  return runtimeSetupPolicy(canonicalRunner, canonicalSuite, manifestArtifacts)
}

export function createContractRunnerDeadlinePolicy() {
  const runtimeSetup = createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.CONTRACT,
  })
  const leases = Object.freeze([
    operationLease(
      'contract/dependency-install',
      BROWSERGATE_OPERATION_CLASS.DEPENDENCY_INSTALL,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    operationLease(
      'contract/pure-browser-contract',
      BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
  ])
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: 'contract-runner',
    runtimeSetup,
    leases,
    normalWorkBudgetMs: sumLeaseDurations(leases, BROWSERGATE_OPERATION_PHASE.NORMAL_WORK),
    finalizationReserveMs: 0,
    authorityHardBudgetMs: sumLeaseDurations(leases),
    hardBudgetMs: sumLeaseDurations(leases),
  })
}

export function createLocalBrowsergateDeadlinePolicy(
  runPolicy,
  platform = process.platform,
  { dependencyInstallReused = false } = {},
) {
  if (typeof dependencyInstallReused !== 'boolean') {
    throw new Error('local dependency-install reuse selection must be boolean')
  }
  const canonicalRunPolicy = parseBrowserRunPolicy(runPolicy, 'local Browsergate run policy')
  const runtimeSetup = createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.LOCAL,
    platform,
  })
  const bootstrapLeases = Object.freeze([
    operationLease(
      BOOTSTRAP_QUERY_LEASE_ID,
      BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
  ])
  const sharedSetupLeases = Object.freeze([
    ...(dependencyInstallReused
      ? []
      : [operationLease(
          'local/dependency-install',
          BROWSERGATE_OPERATION_CLASS.DEPENDENCY_INSTALL,
          BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
        )]),
    operationLease(
      'local/browser-contract',
      BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    operationLease(
      'local/generated-semantic-process',
      BROWSERGATE_OPERATION_CLASS.GENERATED_SEMANTIC_PROCESS,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    ...runtimeSetup.leases,
    operationLease(
      'local/browser-install',
      BROWSERGATE_OPERATION_CLASS.BROWSER_INSTALL,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    operationLease(
      'local/browser-preflight',
      BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
  ])
  const suites = Object.freeze(Object.fromEntries(BROWSER_SUITES.map((suite) => [
    suite,
    createSuiteDeadlinePolicy(suite, canonicalRunPolicy),
  ])))
  const verdict = Object.freeze({
    maximumDurationMs: BROWSERGATE_VERDICT_REDUCER_DEADLINE_MS,
    finalizationReserveMs: BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS,
  })
  const verdictLeases = Object.freeze([
    operationLease(
      'local/verdict/reduce',
      BROWSERGATE_OPERATION_CLASS.VERDICT,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
    fixedLease(
      'local/verdict/finalize',
      'local-verdict-finalization',
      BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
  ])
  const authorityLeases = Object.freeze([
    ...sharedSetupLeases,
    ...BROWSER_SUITES.flatMap((suite) => suites[suite].leases),
    ...verdictLeases,
  ])
  const normalWorkBudgetMs = sumLeaseDurations(
    authorityLeases,
    BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
  )
  const finalizationReserveMs = sumLeaseDurations(
    authorityLeases,
    BROWSERGATE_OPERATION_PHASE.FINALIZATION,
  )
  const authorityHardBudgetMs = safeAdd(
    normalWorkBudgetMs,
    finalizationReserveMs,
    'local Browsergate authority budget',
  )
  const bootstrapBudgetMs = sumLeaseDurations(bootstrapLeases)
  const sharedSetupBudgetMs = sumLeaseDurations(sharedSetupLeases)
  const suiteBudgetMs = Object.values(suites).reduce(
    (sum, policy) => safeAdd(sum, policy.hardBudgetMs, 'local suite deadline budget'),
    0,
  )
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: `local-browsergate-${canonicalRunPolicy.policyId}-${dependencyInstallReused
      ? 'dependency-reuse'
      : 'dependency-acquire'}`,
    runPolicy: canonicalRunPolicy,
    dependencyInstallReused,
    bootstrap: Object.freeze({
      leases: bootstrapLeases,
      budgetMs: bootstrapBudgetMs,
    }),
    sharedSetup: Object.freeze({
      leases: sharedSetupLeases,
      runtimeSetup,
      budgetMs: sharedSetupBudgetMs,
    }),
    suites,
    leases: authorityLeases,
    normalWorkBudgetMs,
    finalizationReserveMs,
    authorityHardBudgetMs,
    focusedProcessLeaseCount: Object.values(suites).reduce(
      (sum, policy) => sum + policy.focusedProcessLeaseCount,
      0,
    ),
    remainderProcessLeaseCount: BROWSER_SUITES.length,
    verdict,
    hardBudgetMs: safeAdd(
      safeAdd(
        bootstrapBudgetMs,
        safeAdd(sharedSetupBudgetMs, suiteBudgetMs, 'local Browsergate deadline budget'),
        'local Browsergate deadline budget',
      ),
      verdict.maximumDurationMs + verdict.finalizationReserveMs,
      'local Browsergate deadline budget',
    ),
  })
}

export function createGithubSuiteJobDeadlinePolicy(suite, runPolicy, platform = 'linux') {
  const canonicalSuite = requireSuite(suite)
  const suitePolicy = createSuiteDeadlinePolicy(canonicalSuite, runPolicy)
  const runtimeSetup = createRuntimeSetupDeadlinePolicy({
    runner: BROWSERGATE_RUNTIME_RUNNER.GITHUB,
    suite: canonicalSuite,
    platform,
  })
  const operationLeases = Object.freeze([
    operationLease(
      `github/${canonicalSuite}/dependency-install`,
      BROWSERGATE_OPERATION_CLASS.DEPENDENCY_INSTALL,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    ...runtimeSetup.leases,
    operationLease(
      `github/${canonicalSuite}/browser-install`,
      BROWSERGATE_OPERATION_CLASS.BROWSER_INSTALL,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
    operationLease(
      `github/${canonicalSuite}/browser-preflight`,
      BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
      BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    ),
  ])
  const runnerSetupBudgetMs = safeAdd(
    sumLeaseDurations(GITHUB_SUITE_RUNNER_SETUP_LEASES),
    sumLeaseDurations(operationLeases),
    'GitHub suite runner setup budget',
  )
  const hardBudgetMs = [
    runnerSetupBudgetMs,
    suitePolicy.hardBudgetMs,
    BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS,
    BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS,
  ].reduce((sum, value) => safeAdd(sum, value, 'GitHub suite job deadline budget'), 0)
  const finalLeases = Object.freeze([
    fixedLease(
      `github/${canonicalSuite}/sealed-upload`,
      'workflow-artifact-upload',
      BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
    fixedLease(
      `github/${canonicalSuite}/settlement`,
      'workflow-job-settlement',
      BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS,
      BROWSERGATE_OPERATION_PHASE.FINALIZATION,
    ),
  ])
  const authorityLeases = Object.freeze([
    ...operationLeases,
    ...suitePolicy.leases,
    ...finalLeases,
  ])
  const normalWorkBudgetMs = sumLeaseDurations(
    authorityLeases,
    BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
  )
  const finalizationReserveMs = sumLeaseDurations(
    authorityLeases,
    BROWSERGATE_OPERATION_PHASE.FINALIZATION,
  )
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: `github-${canonicalSuite}-${suitePolicy.runPolicy.policyId}`,
    suite: canonicalSuite,
    runPolicy: suitePolicy.runPolicy,
    runnerSetup: Object.freeze({
      workflowLeases: GITHUB_SUITE_RUNNER_SETUP_LEASES,
      operationLeases,
      runtimeSetup,
      budgetMs: runnerSetupBudgetMs,
    }),
    suitePolicy,
    leases: authorityLeases,
    normalWorkBudgetMs,
    finalizationReserveMs,
    authorityHardBudgetMs: safeAdd(
      normalWorkBudgetMs,
      finalizationReserveMs,
      'GitHub suite authority budget',
    ),
    artifactUploadDeadlineMs: BROWSERGATE_GITHUB_ARTIFACT_UPLOAD_DEADLINE_MS,
    jobSettlementReserveMs: BROWSERGATE_GITHUB_SUITE_JOB_SETTLEMENT_RESERVE_MS,
    hardBudgetMs,
    minimumJobTimeoutMinutes: Math.ceil(hardBudgetMs / MINUTE_MS),
  })
}

export function createGithubVerdictDeadlinePolicy() {
  const operationBudgetMs = sumLeaseDurations(GITHUB_VERDICT_STEP_LEASES)
  const hardBudgetMs = operationBudgetMs + BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS
  if (hardBudgetMs > BROWSERGATE_VERDICT_JOB_MAXIMUM_MS) {
    throw new Error('GitHub verdict graph exceeds its 15-minute job authority')
  }
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: 'github-browser-verdict',
    steps: GITHUB_VERDICT_STEP_LEASES,
    operationBudgetMs,
    finalizationReserveMs: BROWSERGATE_VERDICT_FINALIZATION_RESERVE_MS,
    hardBudgetMs,
    maximumJobBudgetMs: BROWSERGATE_VERDICT_JOB_MAXIMUM_MS,
  })
}

export function createBootstrapDeadlineAuthority(options) {
  const authorityOptions = snapshotOwnDataOptions(
    options,
    BOOTSTRAP_AUTHORITY_OPTION_FIELDS,
    'Browsergate bootstrap deadline authority options',
  )
  const {
    entryId,
    clock = SYSTEM_MONOTONIC_CLOCK,
    trace = NOOP_TRACE,
  } = authorityOptions
  const canonicalEntryId = requireEntryId(entryId)
  const now = requireClock(clock)
  const emitTrace = requireTrace(trace)
  let lastObservedAtMs = null
  let queryGrant = null
  let handoffState = 'pending'
  let handoffFailureReason = null
  let traceFailureCount = 0

  const emit = (milestone, context) => {
    try {
      emitTrace(Object.freeze({
        schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
        milestone,
        entryId: canonicalEntryId,
        context: Object.freeze({ ...context }),
      }))
    } catch {
      traceFailureCount += 1
    }
  }

  const observeNow = () => {
    const observedAtMs = requireFiniteTimestamp(now(), 'Browsergate bootstrap deadline clock')
    if (lastObservedAtMs !== null && observedAtMs < lastObservedAtMs) {
      throw new Error('Browsergate bootstrap deadline clock must be monotonic')
    }
    lastObservedAtMs = observedAtMs
    return observedAtMs
  }

  const beginHandoff = () => {
    if (handoffState === 'pending') {
      // Claiming the one-shot grant before inspecting caller-owned input closes
      // every accessor, Proxy, clock, policy, and trace reentrancy path.
      handoffState = 'consuming'
      return
    }
    if (handoffState === 'consuming') {
      throw new Error('Browsergate bootstrap deadline handoff is already consuming its query grant')
    }
    if (handoffState === 'completed') {
      throw new Error('Browsergate bootstrap deadline authority already handed off')
    }
    if (handoffFailureReason === 'query-did-not-succeed') {
      throw new Error('Browsergate bootstrap query failed and cannot be handed off')
    }
    throw new Error('Browsergate bootstrap deadline handoff failed and cannot be retried')
  }

  emit('bootstrap-deadline-authority-created', {
    operationClass: BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
    maximumDurationMs: BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
  })

  return Object.freeze({
    entryId: canonicalEntryId,

    grantQuery() {
      if (handoffState !== 'pending') {
        // Terminal rejection is returned without tracing because a hostile trace
        // could otherwise recurse indefinitely through this same branch.
        return Object.freeze({
          outcome: 'rejected',
          reason: 'bootstrap-authority-not-pending',
          leaseId: BOOTSTRAP_QUERY_LEASE_ID,
          operationClass: BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
          remainingBudgetMs: queryGrant === null || lastObservedAtMs === null
            ? 0
            : Math.max(0, Math.floor(queryGrant.expiresAtMs - lastObservedAtMs)),
        })
      }
      const observedAtMs = observeNow()
      if (queryGrant !== null) {
        const rejected = Object.freeze({
          outcome: 'rejected',
          reason: 'bootstrap-query-already-granted',
          leaseId: queryGrant.leaseId,
          operationClass: queryGrant.operationClass,
          remainingBudgetMs: Math.max(0, Math.floor(queryGrant.expiresAtMs - observedAtMs)),
        })
        emit('bootstrap-query-lease-rejected', rejected)
        return rejected
      }
      queryGrant = Object.freeze({
        outcome: 'authorized',
        leaseId: BOOTSTRAP_QUERY_LEASE_ID,
        operationClass: BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
        timeoutMs: BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
        authorizedAtMs: observedAtMs,
        expiresAtMs: observedAtMs + BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS,
      })
      emit('bootstrap-query-lease-authorized', queryGrant)
      return queryGrant
    },

    handoff(options) {
      beginHandoff()
      let rejectionReason = 'handoff-input-invalid'
      let rejectionContext = Object.freeze({})
      const reject = (reason, message, context = Object.freeze({})) => {
        rejectionReason = reason
        rejectionContext = context
        throw new Error(message)
      }

      try {
        const handoffOptions = snapshotOwnDataOptions(
          options,
          BOOTSTRAP_HANDOFF_OPTION_FIELDS,
          'Browsergate bootstrap deadline handoff options',
        )
        const {
          grant,
          queryOutcome,
          checkoutSha,
          contextId,
          policy,
        } = handoffOptions
        if (queryGrant === null || grant !== queryGrant) {
          reject(
            'foreign-or-missing-query-grant',
            'Browsergate bootstrap handoff requires its authorized query grant',
          )
        }
        if (queryOutcome !== 'succeeded') {
          reject(
            'query-did-not-succeed',
            'Browsergate bootstrap handoff requires a successful query outcome',
            Object.freeze({ queryOutcome }),
          )
        }
        const canonicalCheckoutSha = requireCheckoutSha(checkoutSha)
        const canonicalContextId = requireContextId(contextId)
        const canonicalPolicy = requireEntryDeadlinePolicy(policy)
        rejectionReason = 'handoff-authority-creation-failed'
        const handoffAtMs = observeNow()
        if (handoffAtMs > queryGrant.expiresAtMs) {
          reject(
            'bootstrap-query-deadline-expired',
            'Browsergate bootstrap query deadline expired before handoff',
            Object.freeze({ handoffAtMs, expiresAtMs: queryGrant.expiresAtMs }),
          )
        }

        // The first read is the already-observed handoff instant. The run-policy
        // authority therefore starts at the same monotonic instant without an
        // unowned interval introduced by a second clock read.
        let handoffEpochPending = true
        const handoffClock = Object.freeze({
          now: () => {
            if (handoffEpochPending) {
              handoffEpochPending = false
              return handoffAtMs
            }
            return now()
          },
        })
        const authority = createOperationDeadlineAuthority({
          entryId: canonicalEntryId,
          checkoutSha: canonicalCheckoutSha,
          contextId: canonicalContextId,
          policy: canonicalPolicy,
          clock: handoffClock,
          trace: emitTrace,
        })
        handoffState = 'completed'
        emit('bootstrap-deadline-handoff-completed', {
          handoffAtMs,
          checkoutSha: canonicalCheckoutSha,
          contextId: canonicalContextId,
          policyId: authority.policy.policyId,
          policyDigest: authority.policyDigest,
        })
        return authority
      } catch (cause) {
        // A claimed query grant is never released back to pending. Retrying a
        // partially observed handoff could bind one query to two authorities.
        handoffState = 'failed'
        handoffFailureReason = rejectionReason
        emit('bootstrap-deadline-handoff-rejected', {
          reason: rejectionReason,
          ...rejectionContext,
        })
        throw cause
      }
    },

    snapshot() {
      const observedAtMs = observeNow()
      return Object.freeze({
        entryId: canonicalEntryId,
        state: handoffState === 'completed'
          ? 'handed-off'
          : handoffState === 'failed'
            ? handoffFailureReason === 'query-did-not-succeed' ? 'query-failed' : 'handoff-failed'
            : handoffState === 'consuming'
              ? 'handoff-consuming'
              : queryGrant === null ? 'awaiting-query' : 'query-authorized',
        queryDeadlineRemainingMs: queryGrant === null
          ? BROWSERGATE_BOOTSTRAP_QUERY_DEADLINE_MS
          : Math.max(0, Math.floor(queryGrant.expiresAtMs - observedAtMs)),
        traceFailureCount,
      })
    },
  })
}

export function createOperationDeadlineAuthority(options) {
  const authorityOptions = snapshotOwnDataOptions(
    options,
    OPERATION_AUTHORITY_OPTION_FIELDS,
    'Browsergate operation deadline authority options',
  )
  const {
    entryId,
    checkoutSha,
    contextId,
    policy: requestedPolicy,
    clock = SYSTEM_MONOTONIC_CLOCK,
    trace = NOOP_TRACE,
  } = authorityOptions
  const canonicalEntryId = requireEntryId(entryId)
  const canonicalCheckoutSha = requireCheckoutSha(checkoutSha)
  const canonicalContextId = requireContextId(contextId)
  const policy = requireEntryDeadlinePolicy(requestedPolicy)
  const policyDigest = entryDeadlinePolicyDigest(policy)
  const now = requireClock(clock)
  const emitTrace = requireTrace(trace)
  const startedAtMs = requireFiniteTimestamp(now(), 'Browsergate deadline authority start')
  let lastObservedAtMs = startedAtMs
  let grantSequence = 0
  let traceFailureCount = 0
  let activeAuthorization = false
  let inObserverCallback = false
  const grantedLeaseIds = new Set()
  const leasesById = new Map(policy.leases.map((lease) => [lease.leaseId, lease]))
  const leaseStateById = new Map(policy.leases.map((lease) => [
    lease.leaseId,
    LEASE_AUTHORIZATION_STATE.PENDING,
  ]))

  const emit = (milestone, context) => {
    inObserverCallback = true
    try {
      emitTrace(Object.freeze({
        schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
        milestone,
        entryId: canonicalEntryId,
        checkoutSha: canonicalCheckoutSha,
        contextId: canonicalContextId,
        policyId: policy.policyId,
        policyDigest,
        context: Object.freeze({ ...context }),
      }))
    } catch {
      traceFailureCount += 1
    } finally {
      inObserverCallback = false
    }
  }
  emit('deadline-authority-created', {
    epochMs: startedAtMs,
    policyId: policy.policyId,
    policyVersion: policy.policyVersion,
    focusedProcessLeaseCount: policy.focusedProcessLeaseCount,
    remainderProcessLeaseCount: policy.remainderProcessLeaseCount,
    normalWorkBudgetMs: policy.normalWorkBudgetMs,
    finalizationReserveMs: policy.finalizationReserveMs,
    hardBudgetMs: policy.hardBudgetMs,
  })

  function observeNow() {
    const observedAtMs = requireFiniteTimestamp(now(), 'Browsergate deadline authority clock')
    if (observedAtMs < lastObservedAtMs) {
      throw new Error('Browsergate deadline authority clock must be monotonic')
    }
    lastObservedAtMs = observedAtMs
    return observedAtMs
  }

  function remainingFor(phase, observedAtMs) {
    const budgetMs = phase === BROWSERGATE_OPERATION_PHASE.NORMAL_WORK
      ? policy.normalWorkBudgetMs
      : policy.hardBudgetMs
    return Math.max(0, Math.floor(budgetMs - (observedAtMs - startedAtMs)))
  }

  return Object.freeze({
    entryId: canonicalEntryId,
    checkoutSha: canonicalCheckoutSha,
    contextId: canonicalContextId,
    policy,
    policyDigest,

    grant(leaseId) {
      if (inObserverCallback) {
        throw new BrowsergateDeadlineAuthorizationError(
          DEADLINE_AUTHORIZATION_ERROR.OBSERVER_ACTIVE,
        )
      }
      if (activeAuthorization) {
        throw new BrowsergateDeadlineAuthorizationError(
          DEADLINE_AUTHORIZATION_ERROR.ACTIVE,
        )
      }
      activeAuthorization = true
      let claimedLeaseId
      let milestone
      let result
      try {
        if (typeof leaseId !== 'string') {
          throw new BrowsergateDeadlineAuthorizationError(
            DEADLINE_AUTHORIZATION_ERROR.INVALID_LEASE_ID,
          )
        }
        const lease = leasesById.get(leaseId)
        if (lease === undefined) {
          throw new BrowsergateDeadlineAuthorizationError(
            DEADLINE_AUTHORIZATION_ERROR.UNKNOWN_LEASE,
          )
        }
        const leaseState = leaseStateById.get(leaseId)
        if (leaseState === LEASE_AUTHORIZATION_STATE.AUTHORIZED) {
          result = Object.freeze({
            outcome: 'rejected',
            reason: 'lease-already-granted',
            leaseId,
            operationClass: lease.operationClass,
            phase: lease.phase,
          })
          milestone = 'deadline-lease-rejected'
        } else if (leaseState === LEASE_AUTHORIZATION_STATE.FAILED) {
          result = Object.freeze({
            outcome: 'rejected',
            reason: 'lease-authorization-failed',
            leaseId,
            operationClass: lease.operationClass,
            phase: lease.phase,
          })
          milestone = 'deadline-lease-rejected'
        } else {
          leaseStateById.set(leaseId, LEASE_AUTHORIZATION_STATE.CONSUMING)
          claimedLeaseId = leaseId
          const observedAtMs = observeNow()
          const remainingBudgetMs = remainingFor(lease.phase, observedAtMs)
          if (remainingBudgetMs < lease.maximumDurationMs) {
            leaseStateById.set(leaseId, LEASE_AUTHORIZATION_STATE.FAILED)
            result = Object.freeze({
              outcome: 'exhausted',
              reason: 'insufficient-phase-budget',
              leaseId,
              operationClass: lease.operationClass,
              phase: lease.phase,
              requiredDurationMs: lease.maximumDurationMs,
              remainingBudgetMs,
            })
            milestone = 'deadline-lease-exhausted'
          } else {
            grantSequence += 1
            leaseStateById.set(leaseId, LEASE_AUTHORIZATION_STATE.AUTHORIZED)
            grantedLeaseIds.add(leaseId)
            result = Object.freeze({
              outcome: 'authorized',
              leaseId,
              operationClass: lease.operationClass,
              phase: lease.phase,
              grantSequence,
              timeoutMs: lease.maximumDurationMs,
              remainingBudgetMs,
              authorizedAtMs: observedAtMs,
              expiresAtMs: observedAtMs + lease.maximumDurationMs,
            })
            milestone = 'deadline-lease-authorized'
          }
        }
      } catch (cause) {
        if (
          claimedLeaseId !== undefined
          && leaseStateById.get(claimedLeaseId) === LEASE_AUTHORIZATION_STATE.CONSUMING
        ) {
          leaseStateById.set(claimedLeaseId, LEASE_AUTHORIZATION_STATE.FAILED)
        }
        throw cause
      } finally {
        activeAuthorization = false
      }
      emit(milestone, result)
      return result
    },

    snapshot() {
      const observedAtMs = observeNow()
      const elapsedMs = Math.max(0, Math.floor(observedAtMs - startedAtMs))
      const hardDeadlineRemainingMs = remainingFor(
        BROWSERGATE_OPERATION_PHASE.FINALIZATION,
        observedAtMs,
      )
      return Object.freeze({
        entryId: canonicalEntryId,
        checkoutSha: canonicalCheckoutSha,
        contextId: canonicalContextId,
        policyId: policy.policyId,
        policyDigest,
        epochMs: startedAtMs,
        elapsedMs,
        normalWorkRemainingMs: remainingFor(
          BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
          observedAtMs,
        ),
        protectedFinalizationReserveRemainingMs: Math.min(
          policy.finalizationReserveMs,
          hardDeadlineRemainingMs,
        ),
        hardDeadlineRemainingMs,
        grantedLeaseCount: grantedLeaseIds.size,
        remainingLeaseCount: policy.leases.length - grantedLeaseIds.size,
        grantedLeaseIds: Object.freeze([...grantedLeaseIds]),
        traceFailureCount,
      })
    },
  })
}

export function evaluateBrowsergateWarmRuntimeSlo(observedPhaseDurationsMs) {
  if (
    typeof observedPhaseDurationsMs !== 'object'
    || observedPhaseDurationsMs === null
    || Array.isArray(observedPhaseDurationsMs)
  ) throw new Error('Browsergate observed phase durations must be a record')
  const phases = Object.entries(observedPhaseDurationsMs).map(([phase, durationMs]) => {
    if (phase === '' || /[\r\n]/u.test(phase)) {
      throw new Error('Browsergate observed phase name must be non-empty single-line text')
    }
    requireNonNegativeInteger(durationMs, `Browsergate observed ${phase} duration`)
    return Object.freeze({ phase, durationMs })
  })
  const observedRuntimeMs = phases.reduce(
    (sum, { durationMs }) => safeAdd(sum, durationMs, 'Browsergate observed runtime'),
    0,
  )
  return Object.freeze({
    targetMs: BROWSERGATE_WARM_RUNTIME_SLO_MS,
    observedRuntimeMs,
    outcome: observedRuntimeMs <= BROWSERGATE_WARM_RUNTIME_SLO_MS
      ? 'within-slo'
      : 'exceeded-slo',
    deltaMs: observedRuntimeMs - BROWSERGATE_WARM_RUNTIME_SLO_MS,
    phases: Object.freeze(phases),
  })
}

function requireEntryDeadlinePolicy(value) {
  const canonical = canonicalFrozenPolicyData(value, 'Browsergate entry deadline policy')
  if (!Number.isSafeInteger(canonical.schemaVersion) || canonical.schemaVersion < 1) {
    throw new Error('Browsergate entry deadline policy schema version is invalid')
  }
  if (!Number.isSafeInteger(canonical.policyVersion) || canonical.policyVersion < 1) {
    throw new Error('Browsergate entry deadline policy version is invalid')
  }
  if (typeof canonical.policyId !== 'string' || canonical.policyId.length === 0) {
    throw new Error('Browsergate entry deadline policy ID is invalid')
  }
  if (!Array.isArray(canonical.leases) || canonical.leases.length === 0) {
    throw new Error('Browsergate entry deadline policy must own at least one lease')
  }
  const leaseIds = new Set()
  for (const lease of canonical.leases) {
    if (typeof lease !== 'object' || lease === null || Array.isArray(lease)) {
      throw new Error('Browsergate entry deadline lease must be a record')
    }
    if (typeof lease.leaseId !== 'string' || lease.leaseId.length === 0) {
      throw new Error('Browsergate entry deadline lease ID is invalid')
    }
    if (leaseIds.has(lease.leaseId)) {
      throw new Error('Browsergate entry deadline policy contains duplicate lease IDs')
    }
    leaseIds.add(lease.leaseId)
    requireOperationPhase(lease.phase)
    requirePositiveInteger(lease.maximumDurationMs, `${lease.leaseId} deadline`)
  }
  const normalWorkBudgetMs = sumLeaseDurations(
    canonical.leases,
    BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
  )
  const finalizationReserveMs = sumLeaseDurations(
    canonical.leases,
    BROWSERGATE_OPERATION_PHASE.FINALIZATION,
  )
  const hardBudgetMs = safeAdd(
    normalWorkBudgetMs,
    finalizationReserveMs,
    'Browsergate entry deadline authority budget',
  )
  if (
    canonical.normalWorkBudgetMs !== normalWorkBudgetMs
    || canonical.finalizationReserveMs !== finalizationReserveMs
    || (canonical.authorityHardBudgetMs ?? canonical.hardBudgetMs) !== hardBudgetMs
  ) throw new Error('Browsergate entry deadline policy budget differs from its lease graph')
  return Object.freeze({
    schemaVersion: canonical.schemaVersion,
    policyVersion: canonical.policyVersion,
    policyId: canonical.policyId,
    ...(canonical.suite === undefined ? {} : { suite: canonical.suite }),
    ...(canonical.runPolicy === undefined ? {} : { runPolicy: canonical.runPolicy }),
    leases: canonical.leases,
    normalWorkBudgetMs,
    finalizationReserveMs,
    hardBudgetMs,
  })
}

function entryDeadlinePolicyDigest(policy) {
  return createHash('sha256').update(JSON.stringify({
    schemaVersion: policy.schemaVersion,
    policyVersion: policy.policyVersion,
    policyId: policy.policyId,
    leases: policy.leases,
    normalWorkBudgetMs: policy.normalWorkBudgetMs,
    finalizationReserveMs: policy.finalizationReserveMs,
    hardBudgetMs: policy.hardBudgetMs,
  })).digest('hex')
}

function canonicalFrozenPolicyData(value, label) {
  if (value === null || ['string', 'boolean'].includes(typeof value)) return value
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) throw new Error(`${label} contains a non-finite number`)
    return value
  }
  if (typeof value !== 'object' || nodeTypes.isProxy(value) || !Object.isFrozen(value)) {
    throw new Error(`${label} must be frozen plain data`)
  }
  if (Array.isArray(value)) {
    const descriptors = Object.getOwnPropertyDescriptors(value)
    const canonical = Array.from({ length: value.length }, (_, index) => {
      const descriptor = descriptors[index]
      if (descriptor === undefined || !Object.hasOwn(descriptor, 'value')) {
        throw new Error(`${label} arrays must be dense own-data values`)
      }
      return canonicalFrozenPolicyData(descriptor.value, `${label}[${index}]`)
    })
    return Object.freeze(canonical)
  }
  if (Object.getPrototypeOf(value) !== Object.prototype) {
    throw new Error(`${label} must contain plain records`)
  }
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Object.keys(descriptors).sort()
  if (Reflect.ownKeys(descriptors).some((key) => typeof key !== 'string')) {
    throw new Error(`${label} cannot contain symbol properties`)
  }
  return Object.freeze(Object.fromEntries(names.map((name) => {
    const descriptor = descriptors[name]
    if (!Object.hasOwn(descriptor, 'value')) {
      throw new Error(`${label}.${name} must be an own data property`)
    }
    return [name, canonicalFrozenPolicyData(descriptor.value, `${label}.${name}`)]
  })))
}

function runtimeSetupPolicy(runner, suite, manifestArtifacts) {
  // The complete helper set is one authenticated build product. Per-helper
  // leases would silently turn one runner build into N ambient toolchain calls.
  const leases = manifestArtifacts.length === 0
    ? Object.freeze([])
    : Object.freeze([
        operationLease(
          'runtime/batch-build',
          BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD,
          BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
          Object.freeze({ runtimeStage: 'batch-build' }),
        ),
        operationLease(
          'runtime/manifest-preflight',
          BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
          BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
          Object.freeze({ runtimeStage: 'manifest-preflight' }),
        ),
      ])
  return Object.freeze({
    schemaVersion: DEADLINE_POLICY_SCHEMA_VERSION,
    policyVersion: DEADLINE_POLICY_VERSION,
    policyId: `runtime-setup-${runner}${suite === null ? '' : `-${suite}`}`,
    runner,
    suite,
    manifestArtifacts,
    leases,
    budgetMs: sumLeaseDurations(leases),
  })
}

function operationLease(leaseId, operationClass, phase, context = Object.freeze({})) {
  requireOperationPhase(phase)
  return Object.freeze({
    leaseId,
    operationClass,
    phase,
    maximumDurationMs: operationClassDeadlineMs(operationClass),
    ...context,
  })
}

function fixedLease(
  leaseId,
  role,
  maximumDurationMs,
  phase = BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
) {
  requirePositiveInteger(maximumDurationMs, `${leaseId} deadline`)
  requireOperationPhase(phase)
  return Object.freeze({
    leaseId,
    role,
    phase,
    maximumDurationMs,
  })
}

function sumLeaseDurations(leases, phase) {
  return leases.reduce((sum, lease) => (
    phase === undefined || lease.phase === phase
      ? safeAdd(sum, lease.maximumDurationMs, 'Browsergate lease graph budget')
      : sum
  ), 0)
}

function requireOperationPhase(phase) {
  return requireEnum(
    phase,
    Object.values(BROWSERGATE_OPERATION_PHASE),
    'Browsergate operation phase',
  )
}

function requireSuite(suite) {
  return requireEnum(suite, BROWSER_SUITES, 'Browsergate suite')
}

function requireOwnedRuntimePlatform(platform) {
  return requireEnum(platform, ['linux', 'win32'], 'Browsergate owned runtime platform')
}

function requireEnum(value, allowed, label) {
  if (typeof value !== 'string' || !allowed.includes(value)) {
    throw new Error(`${label} must be one of ${allowed.join(', ')}`)
  }
  return value
}

function requireEntryId(value) {
  if (
    typeof value !== 'string'
    || value.length < 1
    || Buffer.byteLength(value, 'utf8') > MAXIMUM_ENTRY_ID_BYTES
    || /[\r\n]/u.test(value)
  ) throw new Error('Browsergate deadline entry ID must be bounded non-empty single-line text')
  return value
}

function requireCheckoutSha(value) {
  if (typeof value !== 'string' || !CHECKOUT_SHA_PATTERN.test(value)) {
    throw new Error('Browsergate checkout SHA must be a lowercase 40-character commit ID')
  }
  return value
}

function requireContextId(value) {
  if (
    typeof value !== 'string'
    || value.trim().length < 1
    || Buffer.byteLength(value, 'utf8') > MAXIMUM_CONTEXT_ID_BYTES
    || /[\r\n]/u.test(value)
  ) throw new Error('Browsergate context ID must be bounded non-empty single-line text')
  return value
}

function requireClock(clock) {
  const snapshot = snapshotOwnDataOptions(
    clock,
    MONOTONIC_CLOCK_FIELDS,
    'Browsergate deadline clock',
  )
  if (typeof snapshot.now !== 'function' || nodeTypes.isProxy(snapshot.now)) {
    throw new Error('Browsergate deadline clock must expose now()')
  }
  const now = snapshot.now
  // Calling the captured data function without the caller object prevents a
  // later property replacement or mutable receiver from changing authority.
  return Object.freeze(() => Reflect.apply(now, undefined, []))
}

function requireTrace(trace) {
  if (typeof trace !== 'function' || nodeTypes.isProxy(trace)) {
    throw new Error('Browsergate deadline trace must be a function')
  }
  const captured = trace
  return Object.freeze((event) => Reflect.apply(captured, undefined, [event]))
}

function snapshotOwnDataOptions(value, fields, label) {
  if (
    typeof value !== 'object'
    || value === null
    || Array.isArray(value)
    || nodeTypes.isProxy(value)
  ) throw new Error(`${label} must be a plain own-data record`)
  const prototype = Object.getPrototypeOf(value)
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error(`${label} must be a plain own-data record`)
  }
  const allowed = new Set([...fields.required, ...fields.optional])
  const keys = Reflect.ownKeys(value)
  for (const key of keys) {
    if (typeof key !== 'string' || !allowed.has(key)) {
      throw new Error(`${label} contains an unknown field`)
    }
  }
  for (const key of fields.required) {
    if (!keys.includes(key)) throw new Error(`${label} is missing ${key}`)
  }
  const snapshot = {}
  for (const key of keys) {
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (
      descriptor === undefined
      || !descriptor.enumerable
      || !Object.hasOwn(descriptor, 'value')
    ) throw new Error(`${label} field ${key} must be an enumerable data property`)
    snapshot[key] = descriptor.value
  }
  return Object.freeze(snapshot)
}

function requirePositiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) {
    throw new Error(`${label} must be a positive integer`)
  }
  return value
}

function requireNonNegativeInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} must be a non-negative integer`)
  }
  return value
}

function requireFiniteTimestamp(value, label) {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`${label} must be a finite timestamp`)
  }
  return value
}

function safeAdd(left, right, label) {
  const value = left + right
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} exceeds safe integer milliseconds`)
  }
  return value
}
