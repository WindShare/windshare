import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  assertRuntimeSuiteCoverage,
  createSampleCommandAuthority,
  executeOperation,
  executeOwnedSuitePhase,
  executeOwnedWindowsD5,
  expectedSampleIdentities,
  fullCommand,
  localDependencyAcquisitionPlan,
  localOperationPlan,
  localSuiteContextPaths,
  parseSampleRunnerRecord,
  runSuiteProduction,
  sampleChildCommand,
  suiteExecutionPlan,
} from './main.mjs'
import {
  canonicalSampleCommandComponentSha256,
  canonicalSampleCommandSha256,
} from './process/sample-command-authority.mjs'
import {
  BROWSERGATE_OPERATION_CLASS,
  BROWSERGATE_OPERATION_DEADLINE_MS,
  BROWSERGATE_OPERATION_PHASE,
} from './operation-deadlines.mjs'
import { browserRunPolicy } from '../../../web/scripts/browser-evidence/run-policy.ts'
import { BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION } from '../../../web/scripts/browser-evidence/sample-driver.ts'
import { verifyPlaywrightDiscoveryContract } from './playwright-discovery.tests.mjs'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const WINDOWS_JOB_HELPER = Object.freeze({
  path: process.execPath,
  byteLength: 1,
  sha256: 'c'.repeat(64),
})
const SAMPLE_COMMAND_CAPABILITY = Object.freeze({
  repositoryRoot: REPOSITORY_ROOT,
  node: Object.freeze({
    path: process.execPath,
    byteLength: 1,
    sha256: 'a'.repeat(64),
  }),
  driverSource: Object.freeze({
    path: join(REPOSITORY_ROOT, 'web', 'scripts', 'browser-evidence', 'sample-driver.ts'),
    byteLength: 1,
    sha256: 'b'.repeat(64),
  }),
  playwrightCli: Object.freeze({
    path: join(REPOSITORY_ROOT, 'web', 'node_modules', '@playwright', 'test', 'cli.js'),
    byteLength: 1,
    sha256: 'c'.repeat(64),
  }),
  environment: Object.freeze({}),
})
const RUNTIME = Object.freeze({
  manifestSha256: 'd'.repeat(64),
  artifact(name) {
    assert.equal(name, 'windows-job')
    return WINDOWS_JOB_HELPER
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
verifyRuntimeSuiteCoverage()
verifyPolicySampleIdentities()
verifyFocusedRemainderPartition()
verifyPlaywrightDiscoveryContract(suiteExecutionPlan)
verifyNamedDeadlines()
await verifyFailureContinuation()
await verifyFullCommandRuntimeLifecycle()
await verifyOwnedSuitePhaseTerminalSemantics()
await verifyOwnedD5TerminalSemantics()
await verifyPinnedSampleCommandAuthority()
verifySampleRecordCapabilityBoundary()

process.stdout.write('browsergate orchestration contracts: PASS\n')

function verifyOperationPlans() {
  assert.deepEqual(localDependencyAcquisitionPlan(), ['dependency-install'])
  assert.deepEqual(
    localDependencyAcquisitionPlan({ skipDependencyInstall: true }),
    ['dependency-install-reuse'],
  )
  assert.throws(
    () => localDependencyAcquisitionPlan({ skipDependencyInstall: 'yes' }),
    /must be boolean/u,
  )
  assert.deepEqual(localOperationPlan('linux'), [
    'dependency-install',
    'browser-contract',
    'browser-runtime-build',
    'browser-install',
    'browser-preflight',
    'main-topology-lock',
    'main-pre-execution-discovery',
    'main-preflight-integration',
    'main-focused-samples',
    'main-exclusive-remainder',
    'pion-topology-lock',
    'pion-pre-execution-discovery',
    'pion-focused-samples',
    'pion-exclusive-remainder',
    'main-suite-guard-and-seal',
    'pion-suite-guard-and-seal',
    'browser-runtime-retirement',
    'standard-library-verdict',
  ])
  assert.deepEqual(localOperationPlan('win32'), [
    'dependency-install',
    'browser-contract',
    'browser-runtime-build',
    'browser-install',
    'browser-preflight',
    'main-topology-lock',
    'main-pre-execution-discovery',
    'main-preflight-integration',
    'main-d5-focused-and-exclusive-remainder',
    'pion-topology-lock',
    'pion-pre-execution-discovery',
    'pion-focused-samples',
    'pion-exclusive-remainder',
    'main-suite-guard-and-seal',
    'pion-suite-guard-and-seal',
    'browser-runtime-retirement',
    'standard-library-verdict',
  ])
  assert.deepEqual(
    localOperationPlan('linux', { skipDependencyInstall: true }).slice(0, 2),
    ['dependency-install-reuse', 'browser-contract'],
    'make ci may reuse dependencies but the contract remains the sole suite upstream owner',
  )

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
    () => assertRuntimeSuiteCoverage(['main', 'pion'], []),
    /at least one browsergate runtime suite is required/u,
  )
  assert.throws(
    () => assertRuntimeSuiteCoverage(['main', 'pion'], ['main', 'main']),
    /must be canonical and unique/u,
  )
  assert.throws(
    () => assertRuntimeSuiteCoverage(['main'], ['not-a-suite']),
    /must be canonical and unique/u,
  )
}

function verifyPolicySampleIdentities() {
  for (const [policyId, sampleCount] of [
    ['blocking', 1],
    ['closure', 3],
    ['stability', 5],
  ]) {
    for (const suite of ['main', 'pion']) {
      const identities = expectedSampleIdentities(suite, browserRunPolicy(policyId))
      assert.equal(identities.length, 3 * sampleCount)
      assert.deepEqual(
        identities,
        ['chromium', 'firefox', 'webkit'].flatMap((browser) =>
          Array.from({ length: sampleCount }, (_, index) => ({
            suite,
            browser,
            sampleIndex: index + 1,
          }))),
      )
    }
  }
  assert.throws(
    () => expectedSampleIdentities('main', browserRunPolicy('blocking'), [
      'firefox',
      'chromium',
      'webkit',
    ]),
    /canonical ordered engine set/u,
  )
}

function verifyFocusedRemainderPartition() {
  const partitions = {
    main: {
      focusedSpec: 'e2e/v2-real-hot-switch.spec.ts',
      focusedConfig: 'playwright.config.ts',
      remainderConfig: 'playwright.remainder.config.ts',
    },
    pion: {
      focusedSpec: 'pion-interop.spec.ts',
      focusedConfig: 'test/transport/webrtc/browser.playwright.config.ts',
      remainderConfig: 'test/transport/webrtc/browser.remainder.playwright.config.ts',
    },
  }
  for (const [suite, expected] of Object.entries(partitions)) {
    const plan = suiteExecutionPlan(suite, 'linux')
    assert.equal(plan.suite, suite)
    assert.deepEqual(plan.preExecutionDiscovery, {
      kind: 'playwright-discovery',
      operationId: `${suite}-pre-execution-discovery`,
      suite,
    })
    assert.equal(plan.focused.kind, 'focused-samples')
    assert.equal(plan.focused.specPath, expected.focusedSpec)
    assert.equal(plan.focused.configPath, expected.focusedConfig)
    assert.deepEqual(plan.remainder, {
      kind: 'playwright-remainder',
      operationId: `${suite}-exclusive-remainder`,
      configPath: expected.remainderConfig,
    })
    assert.deepEqual(plan.guardPublisher, {
      kind: 'guard-publisher',
      operationId: `${suite}-suite-guard-and-seal`,
    })
    assert(!JSON.stringify(plan).includes('test:browser:'))
    const child = sampleChildCommand({
      suite,
      browser: 'firefox',
      platform: 'linux',
      insideWindowsD5: false,
      commandCapability: SAMPLE_COMMAND_CAPABILITY,
    })
    assert.equal(child.executable, SAMPLE_COMMAND_CAPABILITY.node.path)
    assert(child.arguments.includes(expected.focusedSpec))
    assert(child.arguments.includes(expected.focusedConfig))
    assert(child.arguments.includes('--config'))
    assert(child.arguments.includes('--project=firefox'))
    assert(child.arguments.includes('--workers=1'))
    assert(child.arguments.includes('--retries=0'))
    assert(!child.arguments.some((argument) => argument.startsWith('--repeat-each')))
    assert(!child.arguments.includes(expected.remainderConfig))
  }

  assert.deepEqual(suiteExecutionPlan('main', 'linux').preflightIntegration, {
    kind: 'vitest-integration',
    operationId: 'main-preflight-integration',
    testFiles: [
      'test/browser-evidence/artifact-guard-clean-bootstrap.integration.test.ts',
      'test/browser-evidence/native-process-group-backend.test.ts',
    ],
    processBackendAuthority: 'owned-native-process-group-test',
  })
  assert.deepEqual(suiteExecutionPlan('main', 'win32').preflightIntegration, {
    kind: 'vitest-integration',
    operationId: 'main-preflight-integration',
    testFiles: ['test/browser-evidence/artifact-guard-clean-bootstrap.integration.test.ts'],
    processBackendAuthority: 'external-windows-process-gate',
  })
  assert.equal(suiteExecutionPlan('pion', 'linux').preflightIntegration, null)

  const mainRemainder = readFileSync(
    join(REPOSITORY_ROOT, 'web', 'playwright.remainder.config.ts'),
    'utf8',
  )
  const pionRemainder = readFileSync(
    join(
      REPOSITORY_ROOT,
      'web',
      'test',
      'transport',
      'webrtc',
      'browser.remainder.playwright.config.ts',
    ),
    'utf8',
  )
  assert.match(mainRemainder, /testIgnore:\s*\['e2e\/v2-real-hot-switch\.spec\.ts'\]/u)
  assert.match(pionRemainder, /testIgnore:\s*\['pion-interop\.spec\.ts'\]/u)
  assert.throws(
    () => sampleChildCommand({
      suite: 'main',
      browser: 'chromium',
      platform: 'win32',
      insideWindowsD5: false,
      commandCapability: SAMPLE_COMMAND_CAPABILITY,
    }),
    /must execute inside the leased D5/u,
  )
  assert.doesNotThrow(() => sampleChildCommand({
    suite: 'main',
    browser: 'chromium',
    platform: 'win32',
    insideWindowsD5: true,
    commandCapability: SAMPLE_COMMAND_CAPABILITY,
  }))
}

async function verifyPinnedSampleCommandAuthority() {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'windshare-sample-command-'))
  const manifestPath = join(fixtureRoot, 'runtime-manifest.json')
  const manifestBytes = Buffer.from('authenticated runtime manifest', 'utf8')
  writeFileSync(manifestPath, manifestBytes, { flag: 'wx' })
  const manifestSha256 = createHash('sha256').update(manifestBytes).digest('hex')
  const capability = Object.freeze({
    ...SAMPLE_COMMAND_CAPABILITY,
    environment: Object.freeze({ PATH: 'runtime-pinned-path' }),
  })
  const runtime = sampleAuthorityRuntime(capability, manifestPath, manifestSha256)
  const context = Object.freeze({
    runId: 'sample-command-contract',
    checkoutSha: '1'.repeat(40),
    runPolicy: browserRunPolicy('closure'),
    profilePath: join(fixtureRoot, 'profile.json'),
    topologyProfileSha256: '2'.repeat(64),
    resolutionPath: join(fixtureRoot, 'resolution.json'),
    topologyResolutionSha256: '3'.repeat(64),
    topologyLock: Object.freeze({
      profile: Object.freeze({ topologyId: 'sample-command-topology' }),
    }),
  })
  const identity = Object.freeze({ suite: 'main', browser: 'chromium', sampleIndex: 1 })
  const sampleOutputRoot = join(fixtureRoot, 'evidence')
  const sampleDirectory = join(sampleOutputRoot, 'main', 'chromium', 'sample-1')
  const request = Object.freeze({
    context,
    identity,
    suite: 'main',
    sampleOutputRoot,
    sampleDirectory,
    insideWindowsD5: true,
    platform: 'win32',
  })
  const previousPath = process.env.PATH
  try {
    process.env.PATH = 'ambient-before-signing'
    const signedAuthority = await createSampleCommandAuthority({ ...request, runtime })
    process.env.PATH = 'ambient-before-guard'
    const guardAuthority = await createSampleCommandAuthority({ ...request, runtime })
    assert.deepEqual(
      canonicalSampleCommandComponentSha256(guardAuthority),
      canonicalSampleCommandComponentSha256(signedAuthority),
      'ambient host drift cannot alter a manifest-pinned command component',
    )
    assert.equal(
      canonicalSampleCommandSha256(guardAuthority),
      canonicalSampleCommandSha256(signedAuthority),
    )

    const tampered = await createSampleCommandAuthority({
      ...request,
      runtime: sampleAuthorityRuntime(
        Object.freeze({
          ...capability,
          environment: Object.freeze({ PATH: 'tampered-runtime-path' }),
        }),
        manifestPath,
        manifestSha256,
      ),
    })
    const signedComponents = canonicalSampleCommandComponentSha256(signedAuthority)
    const tamperedComponents = canonicalSampleCommandComponentSha256(tampered)
    assert.deepEqual(
      Object.keys(signedComponents).filter((name) =>
        signedComponents[name] !== tamperedComponents[name]),
      ['driver', 'leaf', 'launch'],
      'the component comparison must isolate environment authority drift',
    )
    assert.notEqual(
      canonicalSampleCommandSha256(tampered),
      canonicalSampleCommandSha256(signedAuthority),
      'a manifest capability change must remain settlement-fatal',
    )
  } finally {
    if (previousPath === undefined) delete process.env.PATH
    else process.env.PATH = previousPath
    rmSync(fixtureRoot, { recursive: true, force: true })
  }
}

function sampleAuthorityRuntime(capability, manifestPath, manifestSha256) {
  return Object.freeze({
    manifestPath,
    manifestSha256,
    sampleCommandCapability: () => capability,
    environmentForSuite: () => Object.freeze({
      WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST: manifestPath,
      WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST_SHA256: manifestSha256,
    }),
    artifact: () => WINDOWS_JOB_HELPER,
  })
}

function verifyNamedDeadlines() {
  const command = Object.freeze({
    executable: process.execPath,
    arguments: Object.freeze(['--version']),
    cwd: REPOSITORY_ROOT,
    environment: Object.freeze({}),
    cleanEnvironment: false,
  })
  for (const operationClass of Object.values(BROWSERGATE_OPERATION_CLASS)) {
    const leaseId = 'test/deadline/' + operationClass
    let observedOptions
    const execution = executeOperation(
      'deadline-' + operationClass,
      leaseId,
      operationClass,
      command,
      {
        deadlineAuthority: fullAuthority(operationClass, leaseId),
        executeCommand(_executable, _arguments, options) {
          observedOptions = options
          return { status: 0 }
        },
      },
    )
    assert.equal(execution.exitCode, 0)
    assert.equal(execution.launched, true)
    assert.equal(observedOptions.timeout, BROWSERGATE_OPERATION_DEADLINE_MS[operationClass])
    assert.equal(observedOptions.killSignal, 'SIGKILL')
  }

  let launches = 0
  const exhausted = executeOperation(
    'exhausted-sample',
    'test/exhausted-sample',
    BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
    command,
    {
      deadlineAuthority: exhaustedAuthority(
        BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
        'test/exhausted-sample',
      ),
      executeCommand() {
        launches += 1
        return { status: 0 }
      },
    },
  )
  assert.deepEqual(exhausted, {
    exitCode: 1,
    stdout: '',
    timedOut: false,
    launched: false,
  })

  const clipped = executeOperation(
    'clipped-sample',
    'test/clipped-sample',
    BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
    command,
    {
      deadlineAuthority: clippedAuthority(
        BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
        'test/clipped-sample',
      ),
      executeCommand() {
        launches += 1
        return { status: 0 }
      },
    },
  )
  assert.deepEqual(clipped, {
    exitCode: 1,
    stdout: '',
    timedOut: false,
    launched: false,
  })
  assert.equal(launches, 0)

  const timedOut = executeOperation(
    'timed-out-contract',
    'test/timed-out-contract',
    BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
    command,
    {
      deadlineAuthority: fullAuthority(
        BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
        'test/timed-out-contract',
      ),
      executeCommand: () => ({
        status: null,
        error: Object.assign(new Error('injected timeout'), { code: 'ETIMEDOUT' }),
      }),
    },
  )
  assert.equal(timedOut.exitCode, 1)
  assert.equal(timedOut.timedOut, true)
  assert.equal(timedOut.launched, true)
}

async function verifyFailureContinuation() {
  const calls = []
  const focusedFailure = await runSuiteProduction({
    contextPath: resolve(REPOSITORY_ROOT, 'injected-context.json'),
    suite: 'main',
    insideWindowsD5: false,
    runtime: RUNTIME,
    platform: 'linux',
    runPreExecutionDiscovery: async () => {
      calls.push('discovery')
      return 0
    },
    runPreflightIntegration: async () => {
      calls.push('preflight')
      return 0
    },
    runFocused: async () => {
      calls.push('focused')
      throw new Error('injected focused failure')
    },
    runRemainder: async () => {
      calls.push('remainder')
      return 0
    },
  })
  assert.deepEqual(calls, ['discovery', 'preflight', 'focused', 'remainder'])
  assert.deepEqual(focusedFailure, {
    phaseOutcomes: {
      preExecutionDiscovery: expectedPhase('main-pre-execution-discovery', 0),
      preflightIntegration: expectedPhase('main-preflight-integration', 0),
      focused: expectedPhase(
        'main-focused-samples',
        1,
        'main-focused-samples',
        'injected focused failure',
      ),
      remainder: expectedPhase('main-exclusive-remainder', 0),
    },
    exitCode: 1,
    settlementTrust: null,
  })

  calls.length = 0
  const preExecutionFailures = await runSuiteProduction({
    contextPath: resolve(REPOSITORY_ROOT, 'injected-context.json'),
    suite: 'main',
    insideWindowsD5: false,
    runtime: RUNTIME,
    platform: 'linux',
    runPreExecutionDiscovery: async () => {
      calls.push('discovery')
      throw new Error('injected discovery failure')
    },
    runPreflightIntegration: async () => {
      calls.push('preflight')
      throw new Error('injected preflight failure')
    },
    runFocused: async () => {
      calls.push('focused')
      return { exitCode: 0 }
    },
    runRemainder: async () => {
      calls.push('remainder')
      return 0
    },
  })
  assert.deepEqual(calls, ['discovery', 'preflight', 'focused', 'remainder'])
  assert.equal(preExecutionFailures.exitCode, 1)
  assert.deepEqual(preExecutionFailures.phaseOutcomes.preExecutionDiscovery, expectedPhase(
    'main-pre-execution-discovery',
    1,
    'main-pre-execution-discovery',
    'injected discovery failure',
  ))
  assert.deepEqual(preExecutionFailures.phaseOutcomes.preflightIntegration, expectedPhase(
    'main-preflight-integration',
    1,
    'main-preflight-integration',
    'injected preflight failure',
  ))
  assert.deepEqual(preExecutionFailures.phaseOutcomes.focused, expectedPhase(
    'main-focused-samples',
    0,
  ))
  assert.deepEqual(preExecutionFailures.phaseOutcomes.remainder, expectedPhase(
    'main-exclusive-remainder',
    0,
  ))

  calls.length = 0
  const remainderFailure = await runSuiteProduction({
    contextPath: resolve(REPOSITORY_ROOT, 'injected-context.json'),
    suite: 'pion',
    insideWindowsD5: false,
    runtime: RUNTIME,
    platform: 'linux',
    runPreExecutionDiscovery: async () => {
      calls.push('discovery')
      return 0
    },
    runPreflightIntegration: async () => assert.fail('Pion has no preflight integration'),
    runFocused: async () => {
      calls.push('focused')
      return { exitCode: 0 }
    },
    runRemainder: async () => {
      calls.push('remainder')
      throw new Error('injected remainder cleanup failure')
    },
  })
  assert.deepEqual(calls, ['discovery', 'focused', 'remainder'])
  assert.deepEqual(remainderFailure, {
    phaseOutcomes: {
      preExecutionDiscovery: expectedPhase('pion-pre-execution-discovery', 0),
      preflightIntegration: {
        operationId: null,
        executionAuthority: null,
        status: 'not-applicable',
        exitCode: null,
        error: 'suite-has-no-preflight-integration',
      },
      focused: expectedPhase('pion-focused-samples', 0),
      remainder: expectedPhase(
        'pion-exclusive-remainder',
        1,
        'pion-exclusive-remainder',
        'injected remainder cleanup failure',
      ),
    },
    exitCode: 1,
    settlementTrust: null,
  })

  calls.length = 0
  let d5Calls = 0
  const windowsSettlementTrust = Object.freeze({
    invocationId: 'windows-settlement',
    runtimeManifestSha256: RUNTIME.manifestSha256,
    publicKeySpkiBase64: 'public-key',
    publicKeySha256: 'e'.repeat(64),
  })
  const windowsResult = await runSuiteProduction({
    contextPath: resolve(REPOSITORY_ROOT, 'injected-context.json'),
    suite: 'main',
    insideWindowsD5: false,
    runtime: RUNTIME,
    platform: 'win32',
    runPreExecutionDiscovery: async ({ windowsJobHelper }) => {
      calls.push('discovery')
      assert.deepEqual(windowsJobHelper, WINDOWS_JOB_HELPER)
      return 0
    },
    runPreflightIntegration: async ({ windowsJobHelper }) => {
      calls.push('preflight')
      assert.deepEqual(windowsJobHelper, WINDOWS_JOB_HELPER)
      return 0
    },
    runFocused: async () => assert.fail('Windows main must not escape D5'),
    runRemainder: async () => assert.fail('Windows main must not escape D5'),
    runWindowsD5: async ({ windowsJobHelper }) => {
      d5Calls += 1
      assert.deepEqual(windowsJobHelper, WINDOWS_JOB_HELPER)
      return Object.freeze({ exitCode: 7, settlementTrust: windowsSettlementTrust })
    },
  })
  assert.equal(d5Calls, 1)
  assert.deepEqual(calls, ['discovery', 'preflight'])
  assert.deepEqual(windowsResult, {
    phaseOutcomes: {
      preExecutionDiscovery: expectedPhase('main-pre-execution-discovery', 0),
      preflightIntegration: expectedPhase('main-preflight-integration', 0),
      focused: expectedPhase(
        'main-focused-samples',
        7,
        'main-d5-focused-and-exclusive-remainder',
      ),
      remainder: expectedPhase(
        'main-exclusive-remainder',
        7,
        'main-d5-focused-and-exclusive-remainder',
      ),
    },
    exitCode: 1,
    settlementTrust: windowsSettlementTrust,
  })
}

async function verifyFullCommandRuntimeLifecycle() {
  const contextPath = resolve(REPOSITORY_ROOT, 'injected-context.json')
  const options = new Map([
    ['context', [contextPath]],
    ['suite', ['main']],
    ['inside-windows-d5', ['true']],
    ['runtime-manifest', ['injected-runtime-manifest']],
    ['runtime-manifest-sha256', ['injected-runtime-manifest-sha256']],
  ])
  const context = Object.freeze({
    runId: 'full-command-runtime-lifecycle',
    checkoutSha: 'a'.repeat(40),
    runPolicy: browserRunPolicy('blocking'),
  })

  for (const outcome of ['resolved', 'rejected']) {
    const events = []
    let releaseRemainder
    let rejectRemainder
    let reportStarted
    const started = new Promise((resolveStarted) => {
      reportStarted = resolveStarted
    })
    const remainderSettlement = new Promise((resolveRemainder, rejectRemainderPromise) => {
      releaseRemainder = resolveRemainder
      rejectRemainder = rejectRemainderPromise
    })
    const runtime = Object.freeze({
      manifestSha256: RUNTIME.manifestSha256,
      artifact: RUNTIME.artifact,
      environmentForSuite: RUNTIME.environmentForSuite,
      dispose() {
        events.push('runtime-disposed')
      },
    })
    const completion = fullCommand(options, {
      loadRuntime(actualOptions, suites, allowEnvironment) {
        assert.equal(actualOptions, options)
        assert.deepEqual(suites, ['main'])
        assert.equal(allowEnvironment, true)
        return runtime
      },
      async readContext(actualPath) {
        assert.equal(actualPath, contextPath)
        return context
      },
      async runRemainder({ runtime: actualRuntime, insideWindowsD5 }) {
        assert.equal(actualRuntime, runtime)
        assert.equal(insideWindowsD5, true)
        events.push('remainder-started')
        reportStarted()
        try {
          const exitCode = await remainderSettlement
          events.push('remainder-resolved')
          return exitCode
        } catch (cause) {
          events.push('remainder-rejected')
          throw cause
        }
      },
    })

    await started
    assert.deepEqual(events, ['remainder-started'])
    if (outcome === 'resolved') {
      releaseRemainder(0)
      assert.equal(await completion, 0)
      assert.deepEqual(events, [
        'remainder-started',
        'remainder-resolved',
        'runtime-disposed',
      ])
      continue
    }

    const rejection = assert.rejects(completion, /injected remainder rejection/u)
    rejectRemainder(new Error('injected remainder rejection'))
    await rejection
    assert.deepEqual(events, [
      'remainder-started',
      'remainder-rejected',
      'runtime-disposed',
    ])
  }
}

function expectedPhase(operationId, exitCode, executionAuthority = operationId, error = null) {
  return {
    operationId,
    executionAuthority,
    status: exitCode === 0 ? 'completed' : 'failed',
    exitCode,
    error,
  }
}

async function verifyOwnedSuitePhaseTerminalSemantics() {
  const operationClass = BROWSERGATE_OPERATION_CLASS.FULL_SUITE
  const setupOperationClass = BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION
  let nativeLaunches = 0
  assert.equal(await executeOwnedSuitePhase({
    phase: ownedRemainderPhase('clipped-remainder'),
    leaseId: 'test/clipped-remainder',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: clippedAuthority(operationClass, 'test/clipped-remainder'),
    platform: 'linux',
    executeNative: async () => {
      nativeLaunches += 1
      return exited(0)
    },
  }), 1)
  assert.equal(nativeLaunches, 0)

  let nativeCommand
  assert.equal(await executeOwnedSuitePhase({
    phase: ownedRemainderPhase('successful-remainder'),
    leaseId: 'test/successful-remainder',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: fullAuthority(operationClass, 'test/successful-remainder'),
    platform: 'linux',
    executeNative: async (options) => {
      nativeLaunches += 1
      nativeCommand = options
      return exited(0)
    },
  }), 0)
  assert.equal(nativeLaunches, 1)
  assert(nativeCommand.command.arguments.includes('playwright.remainder.config.ts'))
  assert(nativeCommand.deadlineMs > 0)
  assert(nativeCommand.terminationGraceMs > 0)

  let discoveryCommand
  assert.equal(await executeOwnedSuitePhase({
    phase: suiteExecutionPlan('main', 'linux').preExecutionDiscovery,
    leaseId: 'test/discovery',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: fullAuthority(setupOperationClass, 'test/discovery'),
    platform: 'linux',
    executeNative: async (options) => {
      discoveryCommand = options.command
      return exited(0)
    },
  }), 0)
  assert.equal(discoveryCommand.executable, process.execPath)
  assert.match(discoveryCommand.arguments[0], /playwright-discovery\.integration\.tests\.mjs$/u)
  assert.deepEqual(discoveryCommand.arguments.slice(1), ['main'])

  let preflightCommand
  assert.equal(await executeOwnedSuitePhase({
    phase: suiteExecutionPlan('main', 'linux').preflightIntegration,
    leaseId: 'test/preflight',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: fullAuthority(setupOperationClass, 'test/preflight'),
    platform: 'linux',
    executeNative: async (options) => {
      preflightCommand = options.command
      return exited(0)
    },
  }), 0)
  assert.equal(preflightCommand.executable, process.execPath)
  assert(preflightCommand.arguments.some((argument) => argument.endsWith('vitest.mjs')))
  assert(preflightCommand.arguments.includes(
    'test/browser-evidence/artifact-guard-clean-bootstrap.integration.test.ts',
  ))
  assert(preflightCommand.arguments.includes(
    'test/browser-evidence/native-process-group-backend.test.ts',
  ))
  assert(!preflightCommand.arguments.includes(
    'test/browser-evidence/native-directory-publisher.test.ts',
  ))

  assert.equal(await executeOwnedSuitePhase({
    phase: ownedRemainderPhase('timed-out-remainder'),
    leaseId: 'test/timed-out-remainder',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: fullAuthority(operationClass, 'test/timed-out-remainder'),
    platform: 'linux',
    executeNative: async () => exited(0, true),
  }), 1)

  assert.equal(await executeOwnedSuitePhase({
    phase: ownedRemainderPhase('signaled-remainder'),
    leaseId: 'test/signaled-remainder',
    environment: {},
    windowsJobHelper: null,
    deadlineAuthority: fullAuthority(operationClass, 'test/signaled-remainder'),
    platform: 'linux',
    executeNative: async () => ({
      processEvidence: { terminal: 'signaled', signal: 'SIGKILL' },
      timedOut: false,
    }),
  }), 1)

  await assert.rejects(
    executeOwnedSuitePhase({
      phase: ownedRemainderPhase('cleanup-failed-remainder'),
      leaseId: 'test/cleanup-failed-remainder',
      environment: {},
      windowsJobHelper: null,
      deadlineAuthority: fullAuthority(operationClass, 'test/cleanup-failed-remainder'),
      platform: 'linux',
      executeNative: async () => {
        throw new Error('injected tree cleanup failure')
      },
    }),
    /tree cleanup failure/u,
  )

  let windowsOptions
  assert.equal(await executeOwnedSuitePhase({
    phase: ownedRemainderPhase('windows-remainder'),
    leaseId: 'test/windows-remainder',
    environment: {},
    windowsJobHelper: WINDOWS_JOB_HELPER,
    deadlineAuthority: fullAuthority(operationClass, 'test/windows-remainder'),
    platform: 'win32',
    executeNative: async () => assert.fail('Windows must use the Job backend'),
    executeJob: async (options) => {
      windowsOptions = options
      return exited(0)
    },
  }), 0)
  assert.deepEqual(windowsOptions.inheritedEnvironment, {})
  assert.deepEqual(windowsOptions.injectedEnvironment, {})
  assert.equal(windowsOptions.helperPath, process.execPath)
}

function ownedRemainderPhase(operationId) {
  return Object.freeze({
    ...suiteExecutionPlan('main', 'linux').remainder,
    operationId,
  })
}

async function verifyOwnedD5TerminalSemantics() {
  const contextPath = resolve(REPOSITORY_ROOT, 'injected-context.json')
  const handoff = Object.freeze({
    outputPath: resolve(REPOSITORY_ROOT, 'd5-settlement-trust.json'),
    invocationId: 'f'.repeat(64),
  })
  const settlementTrust = Object.freeze({
    invocationId: handoff.invocationId,
    runtimeManifestSha256: RUNTIME.manifestSha256,
    publicKeySpkiBase64: 'public-key',
    publicKeySha256: 'a'.repeat(64),
  })
  const prepareSettlementHandoff = async (value) => {
    assert.equal(value.contextPath, contextPath)
    assert.equal(value.runtimeManifestSha256, RUNTIME.manifestSha256)
    return handoff
  }
  const readSettlementHandoff = async (value) => {
    assert.equal(value, handoff)
    return settlementTrust
  }
  const readContext = async (value) => {
    assert.equal(value, contextPath)
    return Object.freeze({ runPolicy: browserRunPolicy('blocking') })
  }
  let launches = 0
  let options
  const authorizationEnvironment = 'WINDSHARE_D5_AUTHORIZATION_PIPE'
  const previousAuthorization = process.env[authorizationEnvironment]
  let firstResult
  try {
    process.env[authorizationEnvironment] = 'one-use-go-test-authority'
    firstResult = await executeOwnedWindowsD5({
      contextPath,
      executeHarness: async (value) => {
        launches += 1
        options = value
        return exited(0)
      },
      powershellExecutable: process.execPath,
      runtime: RUNTIME,
      prepareSettlementHandoff,
      readSettlementHandoff,
      readContext,
    })
  } finally {
    if (previousAuthorization === undefined) delete process.env[authorizationEnvironment]
    else process.env[authorizationEnvironment] = previousAuthorization
  }
  assert.deepEqual(firstResult, { exitCode: 0, settlementTrust })
  assert.equal(launches, 1)
  assert(options.deadlineMs > 0)
  assert(options.command.arguments.includes('BrowserTests'))
  assert(options.command.arguments.includes(contextPath))
  assert(options.command.arguments.includes(handoff.outputPath))
  assert(options.command.arguments.includes(handoff.invocationId))
  assert.equal(Object.hasOwn(options.command.environment, authorizationEnvironment), false)

  assert.deepEqual(await executeOwnedWindowsD5({
    contextPath,
    executeHarness: async () => exited(0, true),
    powershellExecutable: process.execPath,
    runtime: RUNTIME,
    prepareSettlementHandoff,
    readSettlementHandoff,
    readContext,
  }), { exitCode: 1, settlementTrust })

  let handoffReads = 0
  await assert.rejects(
    executeOwnedWindowsD5({
      contextPath,
      executeHarness: async () => ({ ...exited(0), launched: false }),
      powershellExecutable: process.execPath,
      runtime: RUNTIME,
      prepareSettlementHandoff,
      readSettlementHandoff: async () => {
        handoffReads += 1
        return settlementTrust
      },
      readContext,
    }),
    /did not launch/u,
  )
  assert.equal(handoffReads, 0)

  await assert.rejects(
    executeOwnedWindowsD5({
      contextPath,
      executeHarness: async () => {
        throw new Error('injected harness failure')
      },
      powershellExecutable: process.execPath,
      runtime: RUNTIME,
      prepareSettlementHandoff,
      readSettlementHandoff,
      readContext,
    }),
    /harness failure/u,
  )
}

function verifySampleRecordCapabilityBoundary() {
  const temporaryRoot = resolve(mkdtempSync(join(tmpdir(), 'windshare-browsergate-record-')))
  try {
    const sampleDirectory = join(temporaryRoot, 'sample-1')
    const artifactRoot = join(temporaryRoot, '.sample-1-child-attachments-AbC123')
    mkdirSync(sampleDirectory)
    mkdirSync(artifactRoot)
    const identity = Object.freeze({ suite: 'main', browser: 'chromium', sampleIndex: 1 })
    const value = {
      schemaVersion: BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION,
      resultPath: join(sampleDirectory, 'result.json'),
      artifactRoot,
      candidate: Object.freeze({ resultStatus: 'final-valid' }),
      acceptedBeforeGuard: true,
    }
    const parsed = parseSampleRunnerRecord(JSON.stringify(value) + '\n', identity, sampleDirectory)
    assert.equal(parsed.artifactRoot, artifactRoot)
    assert.equal(parsed.identity, identity)

    assert.throws(
      () => parseSampleRunnerRecord(
        JSON.stringify({ ...value, extra: true }) + '\n',
        identity,
        sampleDirectory,
      ),
      /invalid field set/u,
    )
    assert.throws(
      () => parseSampleRunnerRecord(
        JSON.stringify({ ...value, resultPath: join(temporaryRoot, 'result.json') }) + '\n',
        identity,
        sampleDirectory,
      ),
      /does not match its sample slot/u,
    )

    const nestedArtifactRoot = join(sampleDirectory, '.sample-1-child-attachments-AbC123')
    mkdirSync(nestedArtifactRoot)
    assert.throws(
      () => parseSampleRunnerRecord(
        JSON.stringify({ ...value, artifactRoot: nestedArtifactRoot }) + '\n',
        identity,
        sampleDirectory,
      ),
      /private direct sibling/u,
    )
    assert.throws(
      () => parseSampleRunnerRecord(
        JSON.stringify(value) + '\n' + JSON.stringify(value) + '\n',
        identity,
        sampleDirectory,
      ),
      /exactly one result record/u,
    )
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true })
  }
}

function fullAuthority(operationClass, expectedLeaseId = operationClass) {
  const classDeadlineMs = BROWSERGATE_OPERATION_DEADLINE_MS[operationClass]
  return Object.freeze({
    grant(actualLeaseId, phase) {
      assert.equal(actualLeaseId, expectedLeaseId)
      if (phase !== undefined) assert.equal(phase, BROWSERGATE_OPERATION_PHASE.NORMAL_WORK)
      return Object.freeze({
        outcome: 'authorized',
        leaseId: expectedLeaseId,
        operationClass,
        phase,
        classDeadlineMs,
        timeoutMs: classDeadlineMs,
        remainingBudgetMs: classDeadlineMs,
      })
    },
  })
}

function clippedAuthority(operationClass, expectedLeaseId = operationClass) {
  const classDeadlineMs = BROWSERGATE_OPERATION_DEADLINE_MS[operationClass]
  return Object.freeze({
    grant(actualLeaseId, phase) {
      assert.equal(actualLeaseId, expectedLeaseId)
      if (phase !== undefined) assert.equal(phase, BROWSERGATE_OPERATION_PHASE.NORMAL_WORK)
      return Object.freeze({
        outcome: 'authorized',
        leaseId: expectedLeaseId,
        operationClass,
        phase,
        classDeadlineMs,
        timeoutMs: classDeadlineMs - 1,
        remainingBudgetMs: classDeadlineMs - 1,
      })
    },
  })
}

function exhaustedAuthority(operationClass, expectedLeaseId = operationClass) {
  const classDeadlineMs = BROWSERGATE_OPERATION_DEADLINE_MS[operationClass]
  return Object.freeze({
    grant(actualLeaseId, phase) {
      assert.equal(actualLeaseId, expectedLeaseId)
      if (phase !== undefined) assert.equal(phase, BROWSERGATE_OPERATION_PHASE.NORMAL_WORK)
      return Object.freeze({
        outcome: 'exhausted',
        leaseId: expectedLeaseId,
        operationClass,
        phase,
        classDeadlineMs,
        remainingBudgetMs: 0,
      })
    },
  })
}

function exited(exitCode, timedOut = false) {
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode }),
    timedOut,
    launched: true,
    treeEmpty: true,
  })
}
