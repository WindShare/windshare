import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  assertRuntimeSuiteCoverage,
  createSampleCommandAuthority,
  executeOwnedSuitePhase,
  expectedSampleIdentities,
  fullCommand,
  localOperationPlan,
  localSuiteContextPaths,
  parseSampleRunnerRecord,
  preflightReductionTraceEvent,
  runSuiteProduction,
  sampleChildCommand,
  suiteExecutionPlan,
} from '../../orchestrator.mjs'
import {
  canonicalSampleCommandComponentSha256,
  canonicalSampleCommandSha256,
  sampleDriverOwnedRuntimeOperation,
} from '../../process/sample-command-authority.mjs'
import {
  BROWSERGATE_OPERATION_CLASS,
  BROWSERGATE_OPERATION_DEADLINE_MS,
} from '../../operation-deadlines.mjs'
import { browserRunPolicy } from '../../../../../web/scripts/browser-evidence/run-policy.ts'
import { BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION } from '../../../../../web/scripts/browser-evidence/sample-driver.ts'
import { BROWSER_SAMPLE_TRACE_SCHEMA_VERSION } from '../../../../../web/scripts/browser-evidence/sample-runner.ts'
import { verifyPlaywrightDiscoveryContract } from '../../testsupport/playwright-discovery.assertions.mjs'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../../../..')
const PROCESS_OWNER = Object.freeze({
  path: process.execPath,
})
const SAMPLE_COMMAND_CAPABILITY = Object.freeze({
  repositoryRoot: REPOSITORY_ROOT,
  node: process.execPath,
  driverSource: join(REPOSITORY_ROOT, 'web', 'scripts', 'browser-evidence', 'sample-driver.ts'),
  playwrightCli: join(REPOSITORY_ROOT, 'web', 'node_modules', '@playwright', 'test', 'cli.js'),
  playwrightRunner: join(
    REPOSITORY_ROOT,
    'web',
    'scripts',
    'browser-evidence',
    'playwright-owned-runner.mjs',
  ),
  environment: Object.freeze({}),
})
const RUNTIME = Object.freeze({
  artifactCapability(kind) {
    assert.equal(kind, 'test-process-owner')
    return PROCESS_OWNER
  },
  environmentForSuite(suite) {
    assert(['main', 'pion'].includes(suite))
    return Object.freeze({})
  },
  sampleCommandCapability() {
    return SAMPLE_COMMAND_CAPABILITY
  },
})

verifyOperationPlans()
verifyPreflightReductionTraceProjection()
verifyRuntimeSuiteCoverage()
verifyPolicySampleIdentities()
verifyFocusedAndSmokeCommands()
verifyPlaywrightDiscoveryContract(suiteExecutionPlan)
await verifyProductionUsesOneCrossPlatformPath()
await verifyFullCommandRuntimeLifecycle()
await verifyOwnedSuiteSettlementSemantics()
await verifyDynamicSampleCommandContract()
verifySampleRecordCapabilityBoundary()

process.stdout.write('browsergate orchestration contracts: PASS\n')

function verifyPreflightReductionTraceProjection() {
  const passed = preflightReductionTraceEvent(Object.freeze({
    operationId: 'browser-contract',
    outcome: 'success',
  }), 'preflight-contract')
  assert.equal(passed.milestone, 'settled')
  assert.equal(passed.outcome, 'succeeded')
  assert.deepEqual(passed.payload, {
    projectedOperationId: 'browser-contract',
    reportedOutcome: 'success',
  })

  const failed = preflightReductionTraceEvent(Object.freeze({
    operationId: 'generated-semantic-process',
    outcome: 'failure',
    failureCode: 'operation-rejected',
  }), 'preflight-contract')
  assert.equal(failed.milestone, 'settled')
  assert.equal(failed.outcome, 'failed')
  assert.deepEqual(failed.payload, {
    projectedOperationId: 'generated-semantic-process',
    failureCode: 'operation-rejected',
    reportedOutcome: 'failure',
  })
}

function verifyOperationPlans() {
  const linux = localOperationPlan('linux')
  const windows = localOperationPlan('win32')
  assert.deepEqual(windows, linux)
  assert.deepEqual(linux.slice(5, 12), [
    'main-topology-lock',
    'main-pre-execution-discovery',
    'main-preflight-integration',
    'main-focused-samples',
    'main-exclusive-remainder',
    'pion-topology-lock',
    'pion-pre-execution-discovery',
  ])
  const contextRoot = resolve(REPOSITORY_ROOT, 'test-results', 'browser-contract')
  assert.deepEqual(localSuiteContextPaths(contextRoot), {
    main: join(contextRoot, 'main', 'context.json'),
    pion: join(contextRoot, 'pion', 'context.json'),
  })
}

function verifyRuntimeSuiteCoverage() {
  assert.deepEqual(assertRuntimeSuiteCoverage(['main', 'pion'], ['main']), ['main'])
  assert.deepEqual(assertRuntimeSuiteCoverage(['main', 'pion'], ['pion']), ['pion'])
  assert.deepEqual(
    assertRuntimeSuiteCoverage(['main', 'pion'], ['main', 'pion']),
    ['main', 'pion'],
  )
  assert.throws(
    () => assertRuntimeSuiteCoverage(['main'], ['pion']),
    /does not authorize every requested command suite/u,
  )
  assert.throws(
    () => assertRuntimeSuiteCoverage(['main', 'pion'], ['main', 'main']),
    /canonical and unique/u,
  )
}

function verifyPolicySampleIdentities() {
  for (const [policyId, sampleCount] of [['blocking', 1], ['closure', 3], ['stability', 5]]) {
    const identities = expectedSampleIdentities('main', browserRunPolicy(policyId))
    assert.equal(identities.length, 3 * sampleCount)
  }
  assert.deepEqual(
    expectedSampleIdentities('main', browserRunPolicy('blocking'), ['chromium']),
    [{ suite: 'main', browser: 'chromium', sampleIndex: 1 }],
  )
  assert.throws(
    () => expectedSampleIdentities('main', browserRunPolicy('blocking'), ['firefox', 'chromium']),
    /canonical ordered engine subset/u,
  )
}

function verifyFocusedAndSmokeCommands() {
  const expected = Object.freeze({
    main: Object.freeze({
      spec: 'e2e/v2-real-hot-switch.spec.ts',
      config: 'playwright.config.ts',
      remainder: 'playwright.remainder.config.ts',
    }),
    pion: Object.freeze({
      spec: 'pion-interop.spec.ts',
      config: 'test/transport/webrtc/browser.playwright.config.ts',
      remainder: 'test/transport/webrtc/browser.remainder.playwright.config.ts',
    }),
  })
  for (const suite of ['main', 'pion']) {
    const plan = suiteExecutionPlan(suite, 'win32')
    assert.equal(plan.focused.specPath, expected[suite].spec)
    assert.equal(plan.focused.configPath, expected[suite].config)
    assert.equal(plan.remainder.configPath, expected[suite].remainder)
    const command = sampleChildCommand({
      suite,
      browser: 'firefox',
      platform: 'win32',
      commandCapability: SAMPLE_COMMAND_CAPABILITY,
    })
    assert(command.arguments.includes(expected[suite].spec))
    assert(command.arguments.includes(expected[suite].config))
    assert(command.arguments.includes('--workers=1'))
    assert(command.arguments.includes('--retries=0'))
  }
  const smoke = sampleChildCommand({
    suite: 'main',
    browser: 'chromium',
    platform: 'win32',
    commandCapability: SAMPLE_COMMAND_CAPABILITY,
    focusedConfigPath: 'playwright.smoke.config.ts',
  })
  assert(smoke.arguments.includes('playwright.smoke.config.ts'))
  assert(smoke.arguments.includes('e2e/v2-real-hot-switch.spec.ts'))
  assert(smoke.arguments.includes('--project=chromium'))
  assert.equal(smoke.arguments.filter((argument) => argument === '--retries=0').length, 1)
  assert.throws(() => sampleChildCommand({
    suite: 'main',
    browser: 'firefox',
    platform: 'win32',
    commandCapability: SAMPLE_COMMAND_CAPABILITY,
    focusedConfigPath: 'playwright.smoke.config.ts',
  }), /outside its product-path authority/u)
  assert.throws(() => sampleChildCommand({
    suite: 'pion',
    browser: 'chromium',
    platform: 'linux',
    commandCapability: SAMPLE_COMMAND_CAPABILITY,
    focusedConfigPath: 'playwright.smoke.config.ts',
  }), /outside its product-path authority/u)
}

async function verifyProductionUsesOneCrossPlatformPath() {
  for (const platform of ['linux', 'win32']) {
    const calls = []
    const trust = Object.freeze({ invocationId: `${platform}-settlement` })
    const result = await runSuiteProduction({
      contextPath: resolve(REPOSITORY_ROOT, `${platform}-context.json`),
      suite: 'main',
      runtime: RUNTIME,
      platform,
      runPreExecutionDiscovery: async () => { calls.push('discovery'); return 0 },
      runPreflightIntegration: async () => { calls.push('preflight'); return 0 },
      runFocused: async () => {
        calls.push('focused')
        return Object.freeze({ exitCode: 0, settlementTrust: trust })
      },
      runRemainder: async () => { calls.push('remainder'); return 0 },
    })
    assert.deepEqual(calls, ['discovery', 'preflight', 'focused', 'remainder'])
    assert.equal(result.exitCode, 0)
    assert.equal(result.settlementTrust, trust)
  }

  const calls = []
  const failure = await runSuiteProduction({
    contextPath: resolve(REPOSITORY_ROOT, 'failed-context.json'),
    suite: 'main',
    runtime: RUNTIME,
    platform: 'win32',
    runPreExecutionDiscovery: async () => 0,
    runPreflightIntegration: async () => 0,
    runFocused: async () => { calls.push('focused'); throw new Error('injected focused failure') },
    runRemainder: async () => { calls.push('remainder'); return 0 },
  })
  assert.deepEqual(calls, ['focused', 'remainder'])
  assert.equal(failure.exitCode, 1)
  assert.equal(failure.phaseOutcomes.focused.status, 'failed')
  assert.equal(failure.phaseOutcomes.remainder.status, 'completed')
}

async function verifyFullCommandRuntimeLifecycle() {
  const contextPath = resolve(REPOSITORY_ROOT, 'injected-context.json')
  const options = new Map([
    ['context', [contextPath]],
    ['suite', ['main']],
    ['runtime-manifest', ['injected-runtime-manifest']],
  ])
  const events = []
  const runtime = Object.freeze({
    ...RUNTIME,
    dispose() { events.push('runtime-disposed') },
  })
  const exitCode = await fullCommand(options, {
    loadRuntime(actualOptions, suites, allowEnvironment) {
      assert.equal(actualOptions, options)
      assert.deepEqual(suites, ['main'])
      assert.equal(allowEnvironment, true)
      return runtime
    },
    async readContext(actualPath) {
      assert.equal(actualPath, contextPath)
      return Object.freeze({
        runId: 'full-command-runtime-lifecycle',
        checkoutSha: 'a'.repeat(40),
        runPolicy: browserRunPolicy('blocking'),
      })
    },
    async runRemainder({ runtime: actualRuntime }) {
      assert.equal(actualRuntime, runtime)
      events.push('remainder')
      return 0
    },
  })
  assert.equal(exitCode, 0)
  assert.deepEqual(events, ['remainder', 'runtime-disposed'])
}

async function verifyOwnedSuiteSettlementSemantics() {
  for (const platform of ['linux', 'win32']) {
    let request
    const leaseId = `${platform}/remainder`
    const exitCode = await executeOwnedSuitePhase({
      phase: suiteExecutionPlan('main', platform).remainder,
      leaseId,
      environment: {},
      runtime: RUNTIME,
      runId: `owned-${platform}`,
      deadlineAuthority: fullAuthority(BROWSERGATE_OPERATION_CLASS.FULL_SUITE, leaseId),
      platform,
      executeOwnedCommand: async (value) => {
        request = value
        return successfulOwnedExecution(platform)
      },
    })
    assert.equal(exitCode, 0)
    assert.equal(request.processOwner, PROCESS_OWNER)
    assert.equal(request.operationId, 'main-exclusive-remainder')
    assert.equal(request.runId, `owned-${platform}`)
    assert.equal(request.inheritedEnvironment.WINDSHARE_TEST_RUN_ID, undefined)
    assert.equal(request.inheritedEnvironment.WINDSHARE_TEST_OPERATION_ID, undefined)
    assert.equal(request.inheritedEnvironment.WINDSHARE_TEST_SCENARIO, undefined)
    assert(request.command.arguments.includes('playwright.remainder.config.ts'))
  }

  const cleanupFailed = Object.freeze({
    ...successfulOwnedExecution('win32'),
    cleanupOutcome: 'failed',
  })
  await assert.rejects(executeOwnedSuitePhase({
    phase: suiteExecutionPlan('main', 'win32').remainder,
    leaseId: 'win32/cleanup-failure',
    environment: {},
    runtime: RUNTIME,
    runId: 'owned-cleanup-failure',
    deadlineAuthority: fullAuthority(
      BROWSERGATE_OPERATION_CLASS.FULL_SUITE,
      'win32/cleanup-failure',
    ),
    platform: 'win32',
    executeOwnedCommand: async () => cleanupFailed,
  }), /completed cleanup of an empty owned process tree/u)
}

async function verifyDynamicSampleCommandContract() {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'windshare-sample-command-'))
  const manifestPath = join(fixtureRoot, 'runtime-manifest.json')
  const manifestBytes = Buffer.from('runtime manifest fixture', 'utf8')
  writeFileSync(manifestPath, manifestBytes, { flag: 'wx' })
  const capability = Object.freeze({
    ...SAMPLE_COMMAND_CAPABILITY,
    environment: Object.freeze({ PATH: 'runtime-selected-path' }),
  })
  const context = Object.freeze({
    runId: 'sample-command-contract',
    checkoutSha: '1'.repeat(40),
    runPolicy: browserRunPolicy('blocking'),
    profilePath: join(fixtureRoot, 'profile.json'),
    topologyProfileSha256: '2'.repeat(64),
    resolutionPath: join(fixtureRoot, 'resolution.json'),
    topologyResolutionSha256: '3'.repeat(64),
    topologyLock: Object.freeze({ profile: Object.freeze({ topologyId: 'sample-command-topology' }) }),
  })
  const identity = Object.freeze({ suite: 'main', browser: 'chromium', sampleIndex: 1 })
  const sampleOutputRoot = join(fixtureRoot, 'evidence')
  const request = Object.freeze({
    context,
    identity,
    suite: 'main',
    sampleOutputRoot,
    sampleDirectory: join(sampleOutputRoot, 'main', 'chromium', 'sample-1'),
    platform: 'win32',
  })
  try {
    const runtime = sampleAuthorityRuntime(capability, manifestPath)
    const first = await createSampleCommandAuthority({ ...request, runtime })
    const second = await createSampleCommandAuthority({ ...request, runtime })
    assert.equal(canonicalSampleCommandSha256(first), canonicalSampleCommandSha256(second))
    const ownedRuntimeOperation = sampleDriverOwnedRuntimeOperation(first)
    assert.deepEqual(Object.keys(ownedRuntimeOperation), ['command', 'inheritedEnvironment'])
    assert.deepEqual(
      Object.keys(ownedRuntimeOperation.command).sort(),
      ['arguments', 'cwd', 'executable', 'stdin'],
    )
    assert.equal(Object.hasOwn(ownedRuntimeOperation.command, 'environment'), false)
    assert.deepEqual(ownedRuntimeOperation.inheritedEnvironment, {
      PATH: 'runtime-selected-path',
    })
    assert.deepEqual(first.ownership, {
      platform: 'win32',
      backend: 'inherited',
      outerAuthority: {
        kind: 'test-process-owner',
        backend: 'windows_job',
        operationId: 'main-chromium-sample-1',
      },
      operationClass: 'browser-sample',
      classDeadlineMs: BROWSERGATE_OPERATION_DEADLINE_MS[BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE],
      childDeadlineMs: 300_000,
    })
    const smoke = await createSampleCommandAuthority({
      ...request,
      runtime,
      focusedConfigPath: 'playwright.smoke.config.ts',
    })
    assert.notEqual(canonicalSampleCommandSha256(smoke), canonicalSampleCommandSha256(first))
    assert.deepEqual(
      Object.keys(canonicalSampleCommandComponentSha256(first)).filter((name) =>
        canonicalSampleCommandComponentSha256(first)[name] !==
          canonicalSampleCommandComponentSha256(smoke)[name]),
      ['leaf', 'launch'],
    )
  } finally {
    rmSync(fixtureRoot, { recursive: true, force: true })
  }
}

function sampleAuthorityRuntime(capability, manifestPath) {
  return Object.freeze({
    manifestPath,
    sampleCommandCapability: () => capability,
    environmentForSuite: () => Object.freeze({
      WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST: manifestPath,
    }),
    artifactCapability: (kind) => {
      assert.equal(kind, 'test-process-owner')
      return PROCESS_OWNER
    },
  })
}

function verifySampleRecordCapabilityBoundary() {
  const temporaryRoot = resolve(mkdtempSync(join(tmpdir(), 'windshare-browsergate-record-')))
  try {
    const sampleDirectory = join(temporaryRoot, 'sample-1')
    const artifactRoot = join(temporaryRoot, '.sample-1-child-attachments-AbC123')
    mkdirSync(sampleDirectory)
    mkdirSync(artifactRoot)
    const identity = Object.freeze({
      runId: 'sample-record-boundary',
      operationId: 'main-chromium-sample-1',
      scenario: 'browser-sample-main-chromium-1',
      suite: 'main',
      browser: 'chromium',
      sampleIndex: 1,
    })
    const value = {
      schemaVersion: BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION,
      runId: identity.runId,
      operationId: identity.operationId,
      scenario: identity.scenario,
      outcome: 'succeeded',
      resultPath: join(sampleDirectory, 'result.json'),
      artifactRoot,
      candidate: Object.freeze({ resultStatus: 'final-valid' }),
      acceptedBeforeGuard: true,
      traces: {
        events: [
          {
            schemaVersion: BROWSER_SAMPLE_TRACE_SCHEMA_VERSION,
            component: 'browser-evidence-runner',
            runId: identity.runId,
            operationId: identity.operationId,
            scenario: identity.scenario,
            outcome: 'started',
            milestone: 'operation-started',
            suite: identity.suite,
            browser: identity.browser,
            sampleIndex: identity.sampleIndex,
          },
          {
            schemaVersion: BROWSER_SAMPLE_TRACE_SCHEMA_VERSION,
            component: 'browser-evidence-runner',
            runId: identity.runId,
            operationId: identity.operationId,
            scenario: identity.scenario,
            outcome: 'succeeded',
            milestone: 'operation-terminal',
            suite: identity.suite,
            browser: identity.browser,
            sampleIndex: identity.sampleIndex,
            context: { cleanupOutcome: 'deferred-to-outer-owner' },
          },
        ],
        observedEvents: 2,
        capturedEvents: 2,
        truncated: false,
        completed: true,
      },
    }
    const parsed = parseSampleRunnerRecord(JSON.stringify(value) + '\n', identity, sampleDirectory)
    assert.equal(parsed.artifactRoot, artifactRoot)
    assert.equal(parsed.identity, identity)
    assert.equal(parsed.traces.events.at(-1).milestone, 'operation-terminal')
    assert.throws(
      () => parseSampleRunnerRecord(JSON.stringify({ ...value, extra: true }) + '\n', identity, sampleDirectory),
      /invalid field set/u,
    )
    assert.throws(
      () => parseSampleRunnerRecord(JSON.stringify({
        ...value,
        traces: { ...value.traces, truncated: true },
      }) + '\n', identity, sampleDirectory),
      /lifecycle traces are incomplete/u,
    )
    assert.throws(
      () => parseSampleRunnerRecord(JSON.stringify(value) + '\n' + JSON.stringify(value) + '\n', identity, sampleDirectory),
      /exactly one result record/u,
    )
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true })
  }
}

function successfulOwnedExecution(platform) {
  const backend = platform === 'win32' ? 'windows_job' : 'linux_subreaper'
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    treeEmpty: true,
    cleanupOutcome: 'completed',
    inputEvidence: Object.freeze({ outcome: 'not_requested', failureCode: '', failureMessage: '' }),
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner',
      backend,
      terminationReason: 'natural',
      platform: Object.freeze({ kind: backend }),
    }),
    stdout: '',
    stderr: '',
  })
}

function fullAuthority(operationClass, expectedLeaseId) {
  const classDeadlineMs = BROWSERGATE_OPERATION_DEADLINE_MS[operationClass]
  return Object.freeze({
    grant(actualLeaseId) {
      assert.equal(actualLeaseId, expectedLeaseId)
      return Object.freeze({
        outcome: 'authorized',
        leaseId: expectedLeaseId,
        operationClass,
        classDeadlineMs,
        timeoutMs: classDeadlineMs,
        remainingBudgetMs: classDeadlineMs,
      })
    },
  })
}
