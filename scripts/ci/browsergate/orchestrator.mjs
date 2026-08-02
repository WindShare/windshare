import { spawnSync } from 'node:child_process'
import { AsyncLocalStorage } from 'node:async_hooks'
import {
  appendFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  guardArtifactSuite,
  requireCompleteArtifactGuardTrace,
} from '../../../web/scripts/browser-evidence/artifact/guard.ts'
import { finalizeParentOwnedBrowserSampleResult } from '../../../web/scripts/browser-evidence/contract/atomic-json.ts'
import { createNativeDirectoryPublisher } from '../../../web/scripts/browser-evidence/filesystem/native-directory-publisher.ts'
import { readStableRegularFileSnapshot } from '../../../web/scripts/browser-evidence/filesystem/snapshot.ts'
import { BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION } from '../../../web/scripts/browser-evidence/sample-driver.ts'
import { BROWSER_SAMPLE_TRACE_SCHEMA_VERSION } from '../../../web/scripts/browser-evidence/sample-runner.ts'
import {
  assertBrowserRunPolicyEqual,
  browserRunPolicy,
  parseBrowserRunPolicy,
  parseBrowserRunPolicyId,
} from '../../../web/scripts/browser-evidence/run-policy.ts'
import { sampleProcessEnvironment } from '../../../web/scripts/browser-evidence/process/sample-environment.ts'
import { requireFinalizedArtifactCollectionRoot } from '../../../web/scripts/browser-evidence/process/attachment-staging.ts'
import { parseCanonicalJsonText } from '../../../web/scripts/browser-evidence/contract/strict-json.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  verifyTestIceTopologyLock,
} from '../../../web/scripts/browser-evidence/test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
} from '../../../web/scripts/browser-evidence/vocabulary.ts'
import {
  BROWSERGATE_OPERATION_CLASS,
  BROWSERGATE_OPERATION_PHASE,
  BROWSERGATE_PROCESS_OWNERSHIP_OUTER_SLACK_MS,
  BROWSER_SAMPLE_PROCESS_DEADLINE_MS,
  createBootstrapDeadlineAuthority,
  createGithubSuiteJobDeadlinePolicy,
  createLocalBrowsergateDeadlinePolicy,
  createOperationDeadlineAuthority,
  createSuiteDeadlinePolicy,
  operationClassDeadlineMs,
} from './operation-deadlines.mjs'
import {
  localGateOperationPlan,
  runLocalBrowserGatePipeline,
} from './local-gate-runner.mjs'
import {
  BROWSERGATE_RUNTIME_MANIFEST_ENV,
  buildBrowsergateRuntime,
  disposeBrowsergateRuntime,
  loadBrowsergateRuntime,
} from './runtime-build.mjs'
import {
  executeOwnedRuntimeCommand,
  resolveHostExecutable,
} from './process/runtime-command-owner.mjs'
import { consumeOwnedRuntimeCommandExecution } from './process/runtime-command-consumer.mjs'
import { buildTestProcessOwnerFixture } from './process/test-process-owner-fixture.mjs'
import { createProcessSettlementSigner } from './process/settlement-signer.mjs'
import { createBrowsergateTraceEvent } from './trace-event.mjs'
import {
  createOwnedTraceJournal,
  requireCompleteOwnedTraceSnapshot,
} from './owned-trace-journal.mjs'
import {
  canonicalSampleCommandSha256,
  sampleDriverOwnedRuntimeOperation,
} from './process/sample-command-authority.mjs'
import { readPinnedNodeVersion } from '../node-version.mjs'
import { createGeneratedSemanticEnvironment } from './generated-semantic/build/environment.mjs'
import {
  GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE,
  GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID,
  GeneratedSemanticRuntimePreflightError,
  generatedSemanticPreflightFailureContext,
  generatedSemanticResultTraceContext,
  requireGeneratedSemanticRuntimeExecution,
  validateGeneratedSemanticRuntimeEvidence,
} from './generated-semantic/runtime-preflight.mjs'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const VITEST_CLI = join(REPOSITORY_ROOT, 'web', 'node_modules', 'vitest', 'vitest.mjs')
const PLAYWRIGHT_SUITE_DISCOVERY = join(
  REPOSITORY_ROOT,
  'scripts',
  'ci',
  'browsergate',
  'tests',
  'suite-discovery',
  'playwright-discovery.tests.mjs',
)
const GENERATED_SEMANTIC_VERIFIER = join(
  REPOSITORY_ROOT,
  'scripts',
  'ci',
  'browsergate',
  'generated-semantic',
  'verify-generated.mjs',
)
const GENERATED_SEMANTIC_ARTIFACT = join(
  REPOSITORY_ROOT,
  'scripts',
  'ci',
  'browsergate',
  'generated-semantic',
  'final-semantic-reducer.js',
)
const CLEAN_BOOTSTRAP_INTEGRATION_TEST =
  'test/browser-evidence/artifact-guard-clean-bootstrap.integration.test.ts'
const VERDICT_CLI = join(REPOSITORY_ROOT, 'scripts', 'ci', 'browsergate', 'verdict.mjs')
const DEFAULT_TOPOLOGY_PROFILE = join(
  REPOSITORY_ROOT,
  'testdata',
  'test-ice-topology',
  'pr-same-host-kernel-route-ipv4.json',
)
const CONTEXT_SCHEMA_VERSION = 2
const GUARD_INPUT_SCHEMA_VERSION = 2
const ORCHESTRATION_STATUS_SCHEMA_VERSION = 1
const CONTEXT_PROFILE_RELATIVE_PATH = 'topology/profile.json'
const CONTEXT_RESOLUTION_RELATIVE_PATH = 'topology/resolution.json'
const GUARD_INPUT_RELATIVE_PATH = 'orchestration/guard-input.json'
const GUARD_UPLOAD_PARENT = '.guard-uploads'
const MAXIMUM_CONTRACT_BYTES = 16 * 1024 * 1024
const MAXIMUM_RUNTIME_MANIFEST_BYTES = 1 * 1024 * 1024
const SUITE_PHASE_PROCESS_DEADLINE_MS = 540_000
const OWNED_PROCESS_TERMINATION_GRACE_MS = 10_000
const RUNTIME_PROCESS_CLEANUP_RESERVE_MS = 30_000
const BOOTSTRAP_RUNTIME_DIRECTORY_PREFIX = 'windshare-browsergate-bootstrap-'
const BOOTSTRAP_OWNER_SPECS = Object.freeze({
  linux: Object.freeze({
    kind: 'test-process-owner',
    filename: 'testprocessowner',
  }),
  win32: Object.freeze({
    kind: 'test-process-owner',
    filename: 'testprocessowner.exe',
  }),
})
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const CHECKOUT_SHA_PATTERN = /^[0-9a-f]{40}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]+$/u
const ORCHESTRATOR_MAXIMUM_TRACE_EVENTS = 4_096
const ORCHESTRATOR_MAXIMUM_TRACE_BYTES = 8 * 1024 * 1024
const GENERATED_SEMANTIC_MAXIMUM_TRACE_EVENTS = 8
const GENERATED_SEMANTIC_MAXIMUM_TRACE_BYTES = 64 * 1024
const OWNED_RUNTIME_OPERATION_MAXIMUM_TRACE_EVENTS = 4
const OWNED_RUNTIME_OPERATION_MAXIMUM_TRACE_BYTES = 256 * 1024
const RUNTIME_COMMAND_CONTRACT_FAILURE_KIND = 'runtime-command-contract-failed'
const ORCHESTRATOR_TRACE_CONTEXT = new AsyncLocalStorage()

export function localOperationPlan(platform = process.platform) {
  const mainOperations = [
    'main-pre-execution-discovery',
    'main-preflight-integration',
    'main-focused-samples',
    'main-exclusive-remainder',
  ]
  return localGateOperationPlan({
    mainProductOperations: mainOperations,
    pionProductOperations: [
      'pion-pre-execution-discovery',
      'pion-focused-samples',
      'pion-exclusive-remainder',
    ],
  })
}

/**
 * One discriminated plan is the authority for partition identity, process
 * integration ownership, execution order, and guard publication. Keeping the
 * phases typed prevents a package-script label from becoming dead metadata.
 */
export function suiteExecutionPlan(suite, platform = process.platform) {
  requireSuite(suite)
  if (typeof platform !== 'string' || platform.length === 0) {
    throw new Error('suite execution platform must be explicit')
  }
  const focused = suite === 'main'
    ? Object.freeze({
        kind: 'focused-samples',
        operationId: 'main-focused-samples',
        specPath: 'e2e/v2-real-hot-switch.spec.ts',
        configPath: 'playwright.config.ts',
      })
    : Object.freeze({
        kind: 'focused-samples',
        operationId: 'pion-focused-samples',
        specPath: 'pion-interop.spec.ts',
        configPath: 'test/transport/webrtc/browser.playwright.config.ts',
      })
  const preflightIntegration = suite === 'main'
    ? Object.freeze({
        kind: 'vitest-integration',
        operationId: 'main-preflight-integration',
        testFiles: Object.freeze([CLEAN_BOOTSTRAP_INTEGRATION_TEST]),
        processBackendAuthority: 'external-test-process-owner',
      })
    : null
  return Object.freeze({
    suite,
    preExecutionDiscovery: Object.freeze({
      kind: 'playwright-discovery',
      operationId: suite + '-pre-execution-discovery',
      suite,
    }),
    preflightIntegration,
    focused,
    remainder: Object.freeze({
      kind: 'playwright-remainder',
      operationId: suite + '-exclusive-remainder',
      suite,
      configPath: suite === 'main'
        ? 'playwright.remainder.config.ts'
        : 'test/transport/webrtc/browser.remainder.playwright.config.ts',
    }),
    guardPublisher: Object.freeze({
      kind: 'guard-publisher',
      operationId: suite + '-suite-guard-and-seal',
    }),
  })
}

export function localSuiteContextPaths(outputRoot) {
  const root = requireCanonicalAbsolutePath(resolve(outputRoot), 'local browser evidence root')
  return Object.freeze({
    main: join(root, 'main', 'context.json'),
    pion: join(root, 'pion', 'context.json'),
  })
}

export function expectedSampleIdentities(suite, runPolicy, browsers = BROWSER_ENGINES) {
  requireSuite(suite)
  const policy = parseBrowserRunPolicy(runPolicy)
  if (
    !Array.isArray(browsers) || browsers.length < 1 ||
    new Set(browsers).size !== browsers.length ||
    browsers.some((browser) => !BROWSER_ENGINES.includes(browser)) ||
    browsers.some((browser, index) => index > 0 &&
      BROWSER_ENGINES.indexOf(browser) <= BROWSER_ENGINES.indexOf(browsers[index - 1]))
  ) throw new Error('browser samples require a canonical ordered engine subset')
  return Object.freeze(browsers.flatMap((browser) =>
    Array.from({ length: policy.sampleCount }, (_, index) => Object.freeze({
      suite,
      browser,
      sampleIndex: index + 1,
    }))))
}

export function sampleChildCommand({
  suite,
  browser,
  platform,
  commandCapability,
  focusedConfigPath,
}) {
  requireSuite(suite)
  requireBrowser(browser)
  if (typeof platform !== 'string') {
    throw new Error('sample child ownership context must be explicit')
  }
  if (commandCapability === null || typeof commandCapability !== 'object') {
    throw new Error('sample child requires a runtime command contract')
  }
  const nodeExecutable = requireCanonicalAbsolutePath(
    commandCapability.node,
    'sample runtime Node executable',
  )
  const playwrightCli = requireCanonicalAbsolutePath(
    commandCapability.playwrightCli,
    'sample runtime Playwright CLI',
  )
  const playwrightRunner = requireCanonicalAbsolutePath(
    commandCapability.playwrightRunner,
    'sample owned Playwright runner',
  )
  const plan = suiteExecutionPlan(suite, platform)
  const configPath = focusedConfigPath ?? plan.focused.configPath
  if (
    configPath !== plan.focused.configPath &&
    !(suite === 'main' && browser === 'chromium' && configPath === 'playwright.smoke.config.ts')
  ) throw new Error('sample focused config is outside its product-path authority')
  const arguments_ = [
    playwrightRunner,
    '--suite',
    suite,
    '--playwright-cli',
    playwrightCli,
    '--',
    'test',
    '--config',
    configPath,
    plan.focused.specPath,
    '--project=' + browser,
    '--workers=1',
    '--retries=0',
  ]
  return Object.freeze({
    executable: nodeExecutable,
    arguments: Object.freeze(arguments_),
  })
}

export async function runBrowserGateCommand(command, optionArguments) {
  const journal = createOwnedTraceJournal({
    label: 'browsergate orchestration trace',
    maximumEvents: ORCHESTRATOR_MAXIMUM_TRACE_EVENTS,
    maximumBytes: ORCHESTRATOR_MAXIMUM_TRACE_BYTES,
  })
  let exitCode
  let primaryFailure
  try {
    exitCode = await ORCHESTRATOR_TRACE_CONTEXT.run(
      journal,
      () => dispatchBrowserGateCommand(command, optionArguments),
    )
  } catch (cause) {
    primaryFailure = cause
  } finally {
    journal.finish()
  }
  let traces = journal.view.snapshot()
  let traceFailure
  try {
    traces = requireCompleteOwnedTraceSnapshot(traces, 'browsergate orchestration trace')
  } catch (cause) {
    traceFailure = cause
  }
  if (primaryFailure !== undefined) {
    throw new BrowserGateCommandError(primaryFailure, traces, traceFailure)
  }
  if (!Number.isSafeInteger(exitCode) || exitCode < 0 || exitCode > 255) {
    throw new Error('browsergate command returned an invalid exit code')
  }
  return Object.freeze({
    exitCode: traceFailure === undefined ? exitCode : 1,
    traces,
  })
}

export class BrowserGateCommandError extends Error {
  constructor(primaryFailure, traces, traceFailure) {
    super('browsergate command failed', {
      cause: traceFailure === undefined
        ? primaryFailure
        : new AggregateError([primaryFailure, traceFailure], 'command and trace settlement failed'),
    })
    this.name = 'BrowserGateCommandError'
    this.traces = traces
  }
}

async function dispatchBrowserGateCommand(command, optionArguments) {
  const options = parseOptions(optionArguments)
  if (command === 'local') return localCommand(options)
  if (command === 'smoke') return smokeCommand(options)
  if (command === 'build-runtime') return buildRuntimeCommand(options)
  if (command === 'dispose-runtime') return disposeRuntimeCommand(options)
  if (command === 'hosted-produce') return hostedProduceCommand(options)
  if (command === 'prepare') return prepareCommand(options)
  if (command === 'samples') return samplesCommand(options)
  if (command === 'full') return fullCommand(options)
  if (command === 'guard-suite') return guardSuiteCommand(options)
  if (command === 'context-environment') return contextEnvironmentCommand(options)
  if (command === 'plan') return planCommand(options)
  throw new Error('browser orchestration router admitted unsupported command ' + JSON.stringify(command))
}

async function buildRuntimeCommand(options) {
  assertOnlyOptions(options, [
    'output-parent',
    'suite',
    'github-output',
    'run-id',
    'checkout-sha',
    'run-policy',
  ])
  const suites = canonicalRequestedSuites(optionValues(options, 'suite'))
  if (suites.length !== 1) throw new Error('hosted runtime build requires exactly one suite')
  const suite = suites[0]
  const runPolicy = selectedRunPolicy(options, 'blocking')
  const deadlineAuthority = createOperationDeadlineAuthority({
    entryId: `github/${suite}/runtime`,
    checkoutSha: requiredOption(options, 'checkout-sha'),
    contextId: requiredOption(options, 'run-id'),
    policy: createGithubSuiteJobDeadlinePolicy(suite, runPolicy, process.platform),
  })
  const runtime = await buildInvocationRuntime({
    suites,
    outputParent: resolve(requiredOption(options, 'output-parent')),
    preserveRuntimeRoot: true,
    deadlineAuthority,
    buildLeaseId: 'runtime/batch-build',
    preflightLeaseId: 'runtime/manifest-preflight',
  })
  try {
    const githubOutput = optionalOption(options, 'github-output')
    if (githubOutput !== undefined) {
      writeRuntimeGithubOutputs(resolve(githubOutput), runtime)
    }
    process.stdout.write(JSON.stringify({
      command: 'build-runtime',
      manifestPath: runtime.manifestPath,
      suites,
      artifacts: runtime.manifest.artifacts,
    }) + '\n')
    return 0
  } finally {
    runtime.dispose()
  }
}

function disposeRuntimeCommand(options) {
  assertOnlyOptions(options, [
    'runtime-manifest',
    'suite',
    'run-id',
    'checkout-sha',
    'run-policy',
  ])
  const suite = requireSuite(requiredOption(options, 'suite'))
  const deadlineAuthority = createOperationDeadlineAuthority({
    entryId: `github/${suite}/runtime-disposal`,
    checkoutSha: requiredOption(options, 'checkout-sha'),
    contextId: requiredOption(options, 'run-id'),
    policy: createGithubSuiteJobDeadlinePolicy(
      suite,
      selectedRunPolicy(options, 'blocking'),
      process.platform,
    ),
  })
  const teardownGrant = requireDeadlineGrant(
    deadlineAuthority,
    `${suite}/runtime-teardown`,
    BROWSERGATE_OPERATION_CLASS.RUNTIME_TEARDOWN,
  )
  if (teardownGrant.outcome !== 'authorized') {
    throw new Error('runtime teardown deadline lease is unavailable')
  }
  const outcome = disposeBrowsergateRuntime({
    manifestPath: requireCanonicalAbsolutePath(
      requiredOption(options, 'runtime-manifest'),
      'browsergate runtime manifest',
    ),
  })
  process.stdout.write(JSON.stringify({
    command: 'dispose-runtime',
    outcome: 'disposed',
    runtimeRoot: outcome.runtimeRoot,
  }) + '\n')
  return 0
}

async function localCommand(options) {
  assertOnlyOptions(options, [
    'output-root',
    'run-id',
    'checkout-sha',
    'profile',
    'run-policy',
    'secret-env',
    'plan',
  ])
  const policy = selectedRunPolicy(options, 'blocking')
  if (flagOption(options, 'plan')) {
    process.stdout.write(JSON.stringify({
      platform: process.platform,
      runPolicy: policy,
      operations: localOperationPlan(process.platform),
    }) + '\n')
    return 0
  }
  const bootstrapDeadlineAuthority = createBootstrapDeadlineAuthority({
    entryId: `local/${policy.policyId}`,
  })
  const bootstrapQueryGrant = bootstrapDeadlineAuthority.grantQuery()
  const checkoutSha = optionalOption(options, 'checkout-sha') ?? gitCheckoutSha(bootstrapQueryGrant)
  requireCheckoutSha(checkoutSha)
  const runId = optionalOption(options, 'run-id') ?? localRunId(checkoutSha)
  requirePortableToken(runId, 'browser run ID')
  const entryDeadlinePolicy = createLocalBrowsergateDeadlinePolicy(policy, process.platform)
  const deadlineAuthority = bootstrapDeadlineAuthority.handoff({
    grant: bootstrapQueryGrant,
    queryOutcome: 'succeeded',
    checkoutSha,
    contextId: runId,
    policy: entryDeadlinePolicy,
  })
  const outputRoot = resolve(optionalOption(options, 'output-root') ?? join(
    REPOSITORY_ROOT,
    'test-results',
    'browser-evidence',
    runId,
  ))
  const profilePath = resolve(optionalOption(options, 'profile') ?? DEFAULT_TOPOLOGY_PROFILE)
  const contexts = localSuiteContextPaths(outputRoot)
  const suiteSetupGrants = new Map()
  const secretNames = localGuardSecretNames(optionValues(options, 'secret-env'))

  const result = await runLocalBrowserGatePipeline({
    runContract: async () => Object.freeze({
       exitCode: runOperation(
         'browser-contract',
         'local/browser-contract',
         BROWSERGATE_OPERATION_CLASS.CONTRACT_TEST,
         commandSpec(pnpmExecutable(), ['-C', 'web', 'run', 'test:browser:evidence:contract']),
         deadlineAuthority,
       ),
     }),
    runGeneratedSemanticProcess: async () => Object.freeze({
      exitCode: runOperation(
        'generated-semantic-process',
        'local/generated-semantic-process',
        BROWSERGATE_OPERATION_CLASS.GENERATED_SEMANTIC_PROCESS,
        commandSpec(pnpmExecutable(), [
          '-C',
          'web',
          'run',
          'test:browser:generated-semantic:process',
        ]),
        deadlineAuthority,
      ),
    }),
    buildRuntime: () => buildInvocationRuntime({
      suites: BROWSER_SUITES,
      runId,
      deadlineAuthority,
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
    }),
    installBrowserRuntime: async () => Object.freeze({
       exitCode: runOperation(
         'browser-install',
         'local/browser-install',
         BROWSERGATE_OPERATION_CLASS.BROWSER_INSTALL,
        commandSpec(pnpmExecutable(), [
          '-C',
          'web',
          'exec',
          'playwright',
          'install',
          'chromium',
          'firefox',
          'webkit',
         ]),
         deadlineAuthority,
       ),
    }),
    runPreflight: async () => Object.freeze({
       exitCode: runOperation(
         'browser-preflight',
         'local/browser-preflight',
         BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
         commandSpec(pnpmExecutable(), ['-C', 'web', 'run', 'test:browser:preflight']),
         deadlineAuthority,
      ),
    }),
    prepareTopology: ({ runtime, suite }) => {
      const suiteSetupGrant = requireDeadlineGrant(
        deadlineAuthority,
        `${suite}/topology`,
        BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
      )
      suiteSetupGrants.set(suite, suiteSetupGrant)
      return prepareEvidenceContext({
        contextPath: contexts[suite],
        runId,
        checkoutSha,
        runPolicy: policy,
        profilePath,
        runtime,
        authorizedTopologyGrant: suiteSetupGrant,
      })
    },
    runProduct: ({ runtime, suite }) => runSuiteProduction({
      contextPath: contexts[suite],
      suite,
      runtime,
      deadlineAuthority,
      suiteSetupGrant: suiteSetupGrants.get(suite),
    }),
    runGuard: ({ runtime, suite, suiteOutcome }) => {
      if (runtime === null) {
        throw new Error('guard cannot start without the invocation runtime authority')
      }
      return runGuardSuite({
        contextPath: contexts[suite],
        suite,
        secretNames,
        settlementTrust: suiteOutcome.settlementTrust,
        publisherHelper: runtime.artifactCapability('artifact-publisher'),
        runtime,
        deadlineAuthority,
        leaseId: `${suite}/guard-seal`,
      })
    },
    retireRuntime: ({ runtime }) => {
      for (const suite of BROWSER_SUITES) {
        const grant = deadlineAuthority.grant(`${suite}/runtime-teardown`)
        if (grant.outcome !== 'authorized') {
          throw new Error(`${suite} runtime teardown deadline lease is unavailable`)
        }
      }
      return runtime.dispose()
    },
    runVerdict: ({ projection, guardOutcomes }) => {
      const verdictPath = join(outputRoot, 'verdict.json')
      return executeOperation(
         'browser-verdict',
         'local/verdict/reduce',
         BROWSERGATE_OPERATION_CLASS.VERDICT,
        commandSpec(process.execPath, verdictArguments({
          runId,
          checkoutSha,
          mainRoot: guardOutcomes.main.uploadDirectory ??
            join(outputRoot, '.missing-main-upload'),
          pionRoot: guardOutcomes.pion.uploadDirectory ??
            join(outputRoot, '.missing-pion-upload'),
          mainJobOutcome: projection.verdictDependencies.main,
          pionJobOutcome: projection.verdictDependencies.pion,
          mainGuardOutcome: guardOutcomes.main.guardOutcome,
          pionGuardOutcome: guardOutcomes.pion.guardOutcome,
          mainManifestSha256: guardOutcomes.main.manifestSha256 ?? '',
          pionManifestSha256: guardOutcomes.pion.manifestSha256 ?? '',
          mainManifestByteLength: guardOutcomes.main.manifestByteLength ?? '',
          pionManifestByteLength: guardOutcomes.pion.manifestByteLength ?? '',
          mainDownloadOutcome: guardOutcomes.main.uploadDirectory === null
            ? 'failure'
            : 'success',
          pionDownloadOutcome: guardOutcomes.pion.uploadDirectory === null
            ? 'failure'
            : 'success',
          output: verdictPath,
        })),
        {
          phase: BROWSERGATE_OPERATION_PHASE.FINALIZATION,
          deadlineAuthority,
        },
      )
    },
  })

  for (const event of result.traces.events) {
    const { operationId, outcome, ...context } = event
    emit('local-hosted-projection', outcome, {
      runId,
      scenario: 'local-hosted-projection',
      projectedOperationId: operationId,
      ...context,
    })
  }

  emit('local', result.exitCode === 0 ? 'passed' : 'failed', {
    runId,
    scenario: 'local-browsergate',
    outputRoot,
    runPolicy: policy.policyId,
    contractJobOutcome: result.projection.contractJobOutcome,
    mainJobOutcome: result.projection.verdictDependencies.main,
    pionJobOutcome: result.projection.verdictDependencies.pion,
    failedOperationCount: result.traces.events.filter(({ outcome }) => outcome === 'failure').length,
  })
  return result.exitCode
}

async function smokeCommand(options) {
  assertOnlyOptions(options, ['output-root', 'run-id', 'checkout-sha', 'profile'])
  const policy = browserRunPolicy('blocking')
  const bootstrapDeadlineAuthority = createBootstrapDeadlineAuthority({ entryId: 'smoke/chromium' })
  const bootstrapQueryGrant = bootstrapDeadlineAuthority.grantQuery()
  const checkoutSha = optionalOption(options, 'checkout-sha') ?? gitCheckoutSha(bootstrapQueryGrant)
  requireCheckoutSha(checkoutSha)
  const runId = optionalOption(options, 'run-id') ?? localRunId(checkoutSha)
  requirePortableToken(runId, 'browser smoke run ID')
  const deadlineAuthority = bootstrapDeadlineAuthority.handoff({
    grant: bootstrapQueryGrant,
    queryOutcome: 'succeeded',
    checkoutSha,
    contextId: runId,
    policy: createLocalBrowsergateDeadlinePolicy(policy, process.platform),
  })
  const outputRoot = resolve(optionalOption(options, 'output-root') ?? join(
    REPOSITORY_ROOT,
    'test-results',
    'browser-smoke',
    runId,
  ))
  const contextPath = join(outputRoot, 'main', 'context.json')
  let runtime
  try {
    runtime = await buildInvocationRuntime({
      suites: Object.freeze(['main']),
      runId,
      deadlineAuthority,
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
    })
    const topologyGrant = requireDeadlineGrant(
      deadlineAuthority,
      'main/topology',
      BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
    )
    await prepareEvidenceContext({
      contextPath,
      runId,
      checkoutSha,
      runPolicy: policy,
      profilePath: resolve(optionalOption(options, 'profile') ?? DEFAULT_TOPOLOGY_PROFILE),
      runtime,
      authorizedTopologyGrant: topologyGrant,
    })
    const outcome = await runSamples({
      contextPath,
      suite: 'main',
      runtime,
      deadlineAuthority,
      browsers: Object.freeze(['chromium']),
      focusedConfigPath: 'playwright.smoke.config.ts',
    })
    emit('smoke', outcome.exitCode === 0 ? 'passed' : 'failed', {
      runId,
      checkoutSha,
      suite: 'main',
      browser: 'chromium',
      expectedSampleCount: 1,
      retryCount: 0,
      outputRoot,
    })
    return outcome.exitCode
  } finally {
    runtime?.dispose()
  }
}
async function hostedProduceCommand(options) {
  assertOnlyOptions(options, [
    'output-root',
    'context',
    'run-id',
    'checkout-sha',
    'run-policy',
    'suite',
    'profile',
    'github-output',
    'runtime-manifest',
  ])
  const suite = requireSuite(requiredOption(options, 'suite'))
  const outputRoot = resolve(requiredOption(options, 'output-root'))
  const contextPath = resolve(requiredOption(options, 'context'))
  if (contextPath !== join(outputRoot, 'context.json')) {
    throw new Error('hosted suite context must be the context.json child of its output root')
  }
  const runPolicy = selectedRunPolicy(options, 'blocking')
  const runId = requiredOption(options, 'run-id')
  const checkoutSha = requiredOption(options, 'checkout-sha')
  const deadlinePolicy = createGithubSuiteJobDeadlinePolicy(suite, runPolicy, process.platform)
  const deadlineAuthority = createOperationDeadlineAuthority({
    entryId: `github/${suite}`,
    checkoutSha,
    contextId: runId,
    policy: deadlinePolicy,
  })
  const suiteSetupGrant = requireDeadlineGrant(
    deadlineAuthority,
    `${suite}/topology`,
    BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
  )
  const runtime = loadRuntimeFromOptions(options, [suite])
  try {
  await prepareEvidenceContext({
    contextPath,
    runId,
    checkoutSha,
    runPolicy,
    profilePath: resolve(optionalOption(options, 'profile') ?? DEFAULT_TOPOLOGY_PROFILE),
    runtime,
    authorizedTopologyGrant: suiteSetupGrant,
  })
  const outcome = await runSuiteProduction({
    contextPath,
    suite,
    runtime,
    deadlineAuthority,
    suiteSetupGrant,
  })
  const githubOutput = optionalOption(options, 'github-output')
  if (githubOutput !== undefined && outcome.settlementTrust !== null) {
    writeSettlementGithubOutputs(resolve(githubOutput), outcome.settlementTrust)
  }
  writeAtomicJson(join(outputRoot, 'orchestration', 'produce-status.json'), {
    schemaVersion: ORCHESTRATION_STATUS_SCHEMA_VERSION,
    runId,
    checkoutSha,
    runPolicy,
    suite,
    phaseOutcomes: outcome.phaseOutcomes,
    exitCode: outcome.exitCode,
  })
  return outcome.exitCode
  } finally {
    runtime.dispose()
  }
}

async function prepareCommand(options) {
  assertOnlyOptions(options, [
    'context',
    'run-id',
    'checkout-sha',
    'run-policy',
    'profile',
    'runtime-manifest',
  ])
  const runtime = loadRuntimeFromOptions(options, BROWSER_SUITES)
  try {
  await prepareEvidenceContext({
    contextPath: resolve(requiredOption(options, 'context')),
    runId: requiredOption(options, 'run-id'),
    checkoutSha: requiredOption(options, 'checkout-sha'),
    runPolicy: selectedRunPolicy(options, 'blocking'),
    profilePath: resolve(optionalOption(options, 'profile') ?? DEFAULT_TOPOLOGY_PROFILE),
    runtime,
  })
  return 0
  } finally {
    runtime.dispose()
  }
}

async function samplesCommand(options) {
  assertOnlyOptions(options, [
    'context',
    'suite',
    'runtime-manifest',
    'settlement-invocation-id',
  ])
  const suite = requireSuite(requiredOption(options, 'suite'))
  const runtime = loadRuntimeFromOptions(options, [suite], true)
  const contextPath = resolve(requiredOption(options, 'context'))
  const settlementInvocationId = optionalOption(options, 'settlement-invocation-id')
  try {
  const context = await readEvidenceContext(contextPath)
  const deadlineAuthority = createSuiteCommandDeadlineAuthority({
    context,
    suite,
    entryId: `command/${suite}/focused`,
  })
  const outcome = await runSamples({
    contextPath,
    suite,
    runtime,
    deadlineAuthority,
    ...(settlementInvocationId === undefined ? {} : { settlementInvocationId }),
  })
  return outcome.exitCode
  } finally {
    runtime.dispose()
  }
}

export async function fullCommand(options, {
  loadRuntime = loadRuntimeFromOptions,
  readContext = readEvidenceContext,
  runRemainder = runRemainderSuite,
} = {}) {
  assertOnlyOptions(options, [
    'context',
    'suite',
    'runtime-manifest',
  ])
  const suite = requireSuite(requiredOption(options, 'suite'))
  const runtime = loadRuntime(options, [suite], true)
  try {
    const contextPath = resolve(requiredOption(options, 'context'))
    const context = await readContext(contextPath)
    return await runRemainder({
      contextPath,
      suite,
      runtime,
      deadlineAuthority: createSuiteCommandDeadlineAuthority({
        context,
        suite,
        entryId: `command/${suite}/remainder`,
      }),
    })
  } finally {
    runtime.dispose()
  }
}

async function guardSuiteCommand(options) {
  assertOnlyOptions(options, [
    'context',
    'suite',
    'secret-env',
    'github-output',
    'runtime-manifest',
    'settlement-invocation-id',
    'settlement-public-key-spki-base64',
    'settlement-public-key-sha256',
  ])
  const githubOutput = optionalOption(options, 'github-output')
  const suite = requireSuite(requiredOption(options, 'suite'))
  const runtime = loadRuntimeFromOptions(options, [suite])
  let outcome
  try {
    const contextPath = resolve(requiredOption(options, 'context'))
    const context = await readEvidenceContext(contextPath)
    const deadlineAuthority = createSuiteCommandDeadlineAuthority({
      context,
      suite,
      entryId: `github/${suite}/guard`,
    })
    outcome = await runGuardSuite({
      contextPath,
      suite,
      secretNames: optionValues(options, 'secret-env'),
      settlementTrust: settlementTrustFromOptions(options),
      publisherHelper: runtime.artifactCapability('artifact-publisher'),
      runtime,
      deadlineAuthority,
      leaseId: `${suite}/guard-seal`,
    })
  } catch (cause) {
    const guardPublisher = suiteExecutionPlan(suite).guardPublisher
    outcome = Object.freeze({
      exitCode: 1,
      guardOutcome: 'failed',
      uploadDirectory: null,
      manifestSha256: null,
      manifestByteLength: null,
      sampleOutcomes: Object.freeze([]),
      phaseOutcomes: Object.freeze({
        guardPublisher: failedPhaseOutcome(guardPublisher, cause),
      }),
      error: errorMessage(cause),
    })
  }
  if (githubOutput !== undefined) writeGuardGithubOutputs(resolve(githubOutput), outcome)
  process.stdout.write(JSON.stringify({
    command: 'guard-suite',
    guardOutcome: outcome.guardOutcome,
    sealedUploadPath: outcome.uploadDirectory,
    manifestSha256: outcome.manifestSha256,
    manifestByteLength: outcome.manifestByteLength,
    sampleOutcomes: outcome.sampleOutcomes,
    ...(outcome.error === undefined ? {} : { error: outcome.error }),
  }) + '\n')
  runtime.dispose()
  return outcome.exitCode
}

async function contextEnvironmentCommand(options) {
  assertOnlyOptions(options, ['context'])
  const context = await readEvidenceContext(resolve(requiredOption(options, 'context')))
  process.stdout.write(JSON.stringify({
    schemaVersion: 1,
    runId: context.runId,
    checkoutSha: context.checkoutSha,
    environment: topologyEnvironment(context),
  }) + '\n')
  return 0
}

function planCommand(options) {
  assertOnlyOptions(options, ['platform', 'run-policy'])
  const platform = optionalOption(options, 'platform') ?? process.platform
  if (!['win32', 'linux', 'darwin'].includes(platform)) {
    throw new Error('unsupported browser orchestration platform ' + JSON.stringify(platform))
  }
  process.stdout.write(JSON.stringify({
    platform,
    runPolicy: selectedRunPolicy(options, 'blocking'),
    operations: localOperationPlan(platform),
  }) + '\n')
  return 0
}

export async function prepareEvidenceContext({
  contextPath,
  runId,
  checkoutSha,
  runPolicy,
  profilePath,
  runtime,
  deadlineAuthority,
  topologyLeaseId,
  authorizedTopologyGrant,
}) {
  const canonicalContextPath = requireCanonicalAbsolutePath(
    resolve(contextPath),
    'browser evidence context',
  )
  const policy = parseBrowserRunPolicy(runPolicy)
  requirePortableToken(runId, 'browser run ID')
  requireCheckoutSha(checkoutSha)
  const canonicalProfileSource = requireCanonicalAbsolutePath(
    resolve(profilePath),
    'topology profile source',
  )
  if (existsSync(canonicalContextPath)) {
    const existing = await readEvidenceContext(canonicalContextPath)
    if (existing.runId !== runId || existing.checkoutSha !== checkoutSha) {
      throw new Error('existing browser evidence context has a different run identity')
    }
    assertBrowserRunPolicyEqual(existing.runPolicy, policy, 'existing browser evidence context policy')
    return existing
  }
  const topologyGrant = authorizedTopologyGrant ?? requireDeadlineGrant(
    deadlineAuthority,
    topologyLeaseId,
    BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
  )

  const root = dirname(canonicalContextPath)
  const topologyRoot = join(root, 'topology')
  const copiedProfilePath = join(root, ...CONTEXT_PROFILE_RELATIVE_PATH.split('/'))
  const resolutionPath = join(root, ...CONTEXT_RESOLUTION_RELATIVE_PATH.split('/'))
  mkdirSync(topologyRoot, { recursive: true, mode: 0o700 })
  requireRegularNoFollowPath(canonicalProfileSource, 'topology profile source')
  const profileBytes = readFileSync(canonicalProfileSource)
  parseTestIceTopologyJson(decodeUtf8(profileBytes, 'topology profile'))
  writeFileSync(copiedProfilePath, profileBytes, { flag: 'wx', mode: 0o600 })

  const execution = executeOperation(
    'topology-materialization-' + basename(root),
    topologyLeaseId,
    BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION,
    commandSpec(runtime.artifactCapability('topology-materializer').path, [
      '--profile',
      copiedProfilePath,
      '--output',
      resolutionPath,
    ], {}, REPOSITORY_ROOT, true),
    {
      captureStdout: true,
      authorizedGrant: topologyGrant,
    },
  )
  if (execution.exitCode !== 0) {
    throw new Error('topology materialization failed with exit code ' + execution.exitCode)
  }
  const record = parseMaterializationRecord(execution.stdout)
  if (
    record.profilePath !== copiedProfilePath ||
    record.resolutionPath !== resolutionPath
  ) throw new Error('topology materializer returned paths outside its context')
  const context = Object.freeze({
    schemaVersion: CONTEXT_SCHEMA_VERSION,
    runId,
    checkoutSha,
    runPolicy: policy,
    profileRelativePath: CONTEXT_PROFILE_RELATIVE_PATH,
    resolutionRelativePath: CONTEXT_RESOLUTION_RELATIVE_PATH,
    topologyProfileSha256: requireSha256(record.topologyProfileSha256, 'topology profile digest'),
    topologyResolutionSha256: requireSha256(
      record.topologyResolutionSha256,
      'topology resolution digest',
    ),
  })
  writeAtomicJson(canonicalContextPath, context)
  emit('topology-context-' + basename(root), 'completed', {
    runId,
    runPolicy: policy.policyId,
    contextPath: canonicalContextPath,
  })
  return readEvidenceContext(canonicalContextPath)
}

export async function readEvidenceContext(contextPath) {
  const canonicalPath = requireCanonicalAbsolutePath(resolve(contextPath), 'browser evidence context')
  const value = readCanonicalJson(canonicalPath, 'browser evidence context')
  requireExactKeys(value, [
    'schemaVersion',
    'runId',
    'checkoutSha',
    'runPolicy',
    'profileRelativePath',
    'resolutionRelativePath',
    'topologyProfileSha256',
    'topologyResolutionSha256',
  ], 'browser evidence context')
  if (value.schemaVersion !== CONTEXT_SCHEMA_VERSION) {
    throw new Error('unsupported browser evidence context schema version')
  }
  const root = dirname(canonicalPath)
  if (
    value.profileRelativePath !== CONTEXT_PROFILE_RELATIVE_PATH ||
    value.resolutionRelativePath !== CONTEXT_RESOLUTION_RELATIVE_PATH
  ) throw new Error('browser evidence context topology paths are not canonical')
  const profilePath = join(root, ...CONTEXT_PROFILE_RELATIVE_PATH.split('/'))
  const resolutionPath = join(root, ...CONTEXT_RESOLUTION_RELATIVE_PATH.split('/'))
  requireRegularNoFollowPath(profilePath, 'topology profile')
  requireRegularNoFollowPath(resolutionPath, 'topology resolution')
  const profileEncoded = readFileSync(profilePath, 'utf8')
  const profile = parseTestIceTopologyJson(profileEncoded)
  const resolution = parseTestIceTopologyResolutionJson(
    readFileSync(resolutionPath, 'utf8'),
    profile,
    value.topologyProfileSha256,
  )
  const lock = await verifyTestIceTopologyLock(
    profile,
    resolution,
    value.topologyProfileSha256,
    value.topologyResolutionSha256,
  )
  return Object.freeze({
    schemaVersion: CONTEXT_SCHEMA_VERSION,
    runId: requirePortableToken(value.runId, 'browser run ID'),
    checkoutSha: requireCheckoutSha(value.checkoutSha),
    runPolicy: parseBrowserRunPolicy(value.runPolicy),
    profileRelativePath: CONTEXT_PROFILE_RELATIVE_PATH,
    resolutionRelativePath: CONTEXT_RESOLUTION_RELATIVE_PATH,
    topologyProfileSha256: requireSha256(value.topologyProfileSha256, 'topology profile digest'),
    topologyResolutionSha256: requireSha256(
      value.topologyResolutionSha256,
      'topology resolution digest',
    ),
    contextPath: canonicalPath,
    root,
    profilePath,
    resolutionPath,
    topologyLock: lock,
  })
}

export async function runSuiteProduction({
  contextPath,
  suite,
  runtime,
  platform = process.platform,
  deadlineAuthority,
  suiteSetupGrant,
  runPreExecutionDiscovery = runSuitePreExecutionDiscovery,
  runPreflightIntegration = runSuitePreflightIntegration,
  runFocused = runSamples,
  runRemainder = runRemainderSuite,
}) {
  requireSuite(suite)
  const plan = suiteExecutionPlan(suite, platform)
  const phaseOutcomes = {
    preExecutionDiscovery: pendingPhaseOutcome(plan.preExecutionDiscovery),
    preflightIntegration: plan.preflightIntegration === null
      ? notApplicablePhaseOutcome('suite-has-no-preflight-integration')
      : pendingPhaseOutcome(plan.preflightIntegration),
    focused: pendingPhaseOutcome(plan.focused),
    remainder: pendingPhaseOutcome(plan.remainder),
  }
  let settlementTrust = null

  phaseOutcomes.preExecutionDiscovery = await captureSuitePhase({
    phase: plan.preExecutionDiscovery,
    suite,
    execute: () => runPreExecutionDiscovery({
      contextPath,
      suite,
      runtime,
      platform,
      authorizedGrant: suiteSetupGrant,
      ...optionalDeadlineAuthority(deadlineAuthority),
    }),
  })

  if (plan.preflightIntegration !== null) {
    phaseOutcomes.preflightIntegration = await captureSuitePhase({
      phase: plan.preflightIntegration,
      suite,
      execute: () => runPreflightIntegration({
        contextPath,
        suite,
        runtime,
        platform,
        authorizedGrant: suiteSetupGrant,
        ...optionalDeadlineAuthority(deadlineAuthority),
      }),
    })
  }

  try {
    const focused = await runFocused({
      contextPath,
      suite,
      runtime,
      platform,
      ...optionalDeadlineAuthority(deadlineAuthority),
    })
    phaseOutcomes.focused = phaseOutcome(plan.focused, focused.exitCode)
    settlementTrust = focused.settlementTrust ?? null
  } catch (cause) {
    phaseOutcomes.focused = failedPhaseOutcome(plan.focused, cause)
    emit(plan.focused.operationId, 'failed', { error: errorMessage(cause) })
  }
  try {
    const remainderExitCode = await runRemainder({
      contextPath,
      suite,
      runtime,
      platform,
      ...optionalDeadlineAuthority(deadlineAuthority),
    })
    phaseOutcomes.remainder = phaseOutcome(plan.remainder, remainderExitCode)
  } catch (cause) {
    phaseOutcomes.remainder = failedPhaseOutcome(plan.remainder, cause)
    emit(plan.remainder.operationId, 'failed', { error: errorMessage(cause) })
  }
  return productionOutcome(phaseOutcomes, settlementTrust)
}

function productionOutcome(phaseOutcomes, settlementTrust) {
  const frozenPhaseOutcomes = Object.freeze({ ...phaseOutcomes })
  return Object.freeze({
    phaseOutcomes: frozenPhaseOutcomes,
    exitCode: Object.values(frozenPhaseOutcomes).every((outcome) =>
      outcome.status === 'completed' || outcome.status === 'not-applicable')
      ? 0
      : 1,
    settlementTrust,
  })
}

async function captureSuitePhase({ phase, suite, execute }) {
  try {
    return phaseOutcome(phase, await execute())
  } catch (cause) {
    emit(phase.operationId, 'failed', { suite, error: errorMessage(cause) })
    return failedPhaseOutcome(phase, cause)
  }
}

function pendingPhaseOutcome(phase) {
  return Object.freeze({
    operationId: phase.operationId,
    executionAuthority: phase.operationId,
    status: 'pending',
    exitCode: null,
    error: null,
  })
}

function notApplicablePhaseOutcome(reason) {
  return Object.freeze({
    operationId: null,
    executionAuthority: null,
    status: 'not-applicable',
    exitCode: null,
    error: reason,
  })
}

function failedPhaseOutcome(phase, cause) {
  return phaseOutcome(phase, 1, { error: errorMessage(cause) })
}

function phaseOutcome(phase, exitCode, {
  executionAuthority = phase.operationId,
  error = null,
} = {}) {
  if (!Number.isInteger(exitCode) || exitCode < 0) {
    throw new Error(`${phase.operationId} returned an invalid exit code`)
  }
  return Object.freeze({
    operationId: phase.operationId,
    executionAuthority,
    status: exitCode === 0 ? 'completed' : 'failed',
    exitCode,
    error,
  })
}

function optionalDeadlineAuthority(deadlineAuthority) {
  return deadlineAuthority === undefined ? Object.freeze({}) : { deadlineAuthority }
}

function createSuiteCommandDeadlineAuthority({ context, suite, entryId }) {
  if (context.checkoutSha === undefined || context.runId === undefined) {
    throw new Error('suite command deadline authority requires an evidence context identity')
  }
  return createOperationDeadlineAuthority({
    entryId,
    checkoutSha: context.checkoutSha,
    contextId: context.runId,
    policy: createSuiteDeadlinePolicy(suite, context.runPolicy),
  })
}

export async function runSamples({
  contextPath,
  suite,
  runtime,
  settlementInvocationId,
  createSettlementSigner = createProcessSettlementSigner,
  executeOwnedCommand = executeOwnedRuntimeCommand,
  deadlineAuthority,
  platform = process.platform,
  browsers = BROWSER_ENGINES,
  focusedConfigPath,
}) {
  requireSuite(suite)
  const context = await readEvidenceContext(contextPath)
  const suiteRoot = context.root
  if (basename(suiteRoot) !== suite) {
    throw new Error('suite context root name must equal its suite identity')
  }
  const sampleOutputRoot = dirname(suiteRoot)
  const identities = expectedSampleIdentities(suite, context.runPolicy, browsers)
  const ledgerSamples = []
  const failures = []
  const signer = createSettlementSigner({
    invocationId: settlementInvocationId,
  })
  try {
    for (const identity of identities) {
      const sampleDirectory = join(
        suiteRoot,
        identity.browser,
        'sample-' + identity.sampleIndex,
      )
      let exitCode = 1
      try {
        const authority = await createSampleCommandAuthority({
          context,
          identity,
          suite,
          sampleOutputRoot,
          sampleDirectory,
          platform,
          runtime,
          focusedConfigPath,
        })
        const operationId = authority.identity.operationId
        const scenario = authority.identity.scenario
        const commandSha256 = canonicalSampleCommandSha256(authority)
        const runtimeOperation = sampleDriverOwnedRuntimeOperation(authority)
        const processOwner = Object.freeze({
          path: authority.runtime.processOwner.path,
        })
        const execution = await executeOwnedRuntimeOperation({
          operationId,
          leaseId: `${suite}/focused/${identity.browser}/sample-${identity.sampleIndex}`,
          operationClass: BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
          command: runtimeOperation.command,
          platform,
          runId: context.runId,
          scenario,
          inheritedEnvironment: runtimeOperation.inheritedEnvironment,
          processOwner,
          deadlineAuthority,
          executeOwnedCommand,
        })
        exitCode = runnerProcessExitCode(execution)
        requireSettledSampleExecution(execution)
        const postExecutionAuthority = await createSampleCommandAuthority({
          context,
          identity,
          suite,
          sampleOutputRoot,
          sampleDirectory,
          platform,
          runtime,
          focusedConfigPath,
        })
        if (JSON.stringify(postExecutionAuthority) !== JSON.stringify(authority)) {
          throw new Error('sample command authority changed across its owned execution')
        }
        const record = parseSampleRunnerRecord(execution.stdout, authority.identity, sampleDirectory)
        if (exitCode !== (record.acceptedBeforeGuard ? 0 : 1)) {
          throw new Error('sample driver exit code differs from its terminal candidate')
        }
        const finalized = await finalizeParentOwnedBrowserSampleResult(
          record.resultPath,
          record.candidate,
          context.topologyLock,
        )
        if (record.acceptedBeforeGuard !== (finalized.result.resultStatus === 'final-valid')) {
          throw new Error('sample driver acceptance differs from its finalized result')
        }
        const settlementAttestation = signer.signSample({
          sample: Object.freeze({
            runId: context.runId,
            runPolicy: context.runPolicy,
            ...identity,
            checkoutSha: context.checkoutSha,
          }),
          resultBytes: finalized.bytes,
          commandSha256,
          execution,
          ownershipBackend: authority.ownership.outerAuthority.backend,
        })
        ledgerSamples.push(Object.freeze({
          browser: identity.browser,
          sampleIndex: identity.sampleIndex,
          resultRelativePath: identity.browser + '/sample-' + identity.sampleIndex + '/result.json',
          artifactRoot: record.artifactRoot,
          settlementAttestation,
        }))
      } catch (cause) {
        failures.push(Object.freeze({
          ...identity,
          exitCode,
          reason: errorMessage(cause),
        }))
        continue
      }
      if (exitCode !== 0) {
        failures.push(Object.freeze({
          ...identity,
          exitCode,
          reason: 'sample was rejected before guard',
        }))
      }
    }
  writeAtomicJson(join(suiteRoot, ...GUARD_INPUT_RELATIVE_PATH.split('/')), {
    schemaVersion: GUARD_INPUT_SCHEMA_VERSION,
    runId: context.runId,
    checkoutSha: context.checkoutSha,
    runPolicy: context.runPolicy,
    suite,
    samples: ledgerSamples,
  })
  const exitCode = failures.length === 0 && ledgerSamples.length === identities.length ? 0 : 1
  emit(suite + '-focused-samples', exitCode === 0 ? 'completed' : 'failed', {
    runId: context.runId,
    runPolicy: context.runPolicy.policyId,
    expectedSampleCount: identities.length,
    recordedSampleCount: ledgerSamples.length,
    failureCount: failures.length,
  })
  return Object.freeze({
    exitCode,
    expectedSampleCount: identities.length,
    failures: Object.freeze(failures),
    settlementTrust: signer.trust,
  })
  } finally {
    signer.retire()
  }
}

export async function runRemainderSuite({
  contextPath,
  suite,
  runtime,
  platform = process.platform,
  deadlineAuthority,
}) {
  requireSuite(suite)
  const context = await readEvidenceContext(contextPath)
  const plan = suiteExecutionPlan(suite, platform)
  return executeOwnedSuitePhase({
    phase: plan.remainder,
    leaseId: `${suite}/remainder`,
    environment: {
      ...topologyEnvironment(context),
      ...runtime.environmentForSuite(suite),
    },
    runtime,
    runId: context.runId,
    platform,
    ...optionalDeadlineAuthority(deadlineAuthority),
  })
}

export async function runSuitePreExecutionDiscovery({
  contextPath,
  suite,
  runtime,
  platform = process.platform,
  deadlineAuthority,
  authorizedGrant,
}) {
  requireSuite(suite)
  const context = await readEvidenceContext(contextPath)
  return executeOwnedSuitePhase({
    phase: suiteExecutionPlan(suite, platform).preExecutionDiscovery,
    leaseId: `${suite}/topology`,
    authorizedGrant,
    environment: {
      ...topologyEnvironment(context),
      ...runtime.environmentForSuite(suite),
    },
    runtime,
    runId: context.runId,
    platform,
    ...optionalDeadlineAuthority(deadlineAuthority),
  })
}

export async function runSuitePreflightIntegration({
  contextPath,
  suite,
  runtime,
  platform = process.platform,
  deadlineAuthority,
  authorizedGrant,
}) {
  requireSuite(suite)
  const phase = suiteExecutionPlan(suite, platform).preflightIntegration
  if (phase === null) throw new Error(`${suite} does not own a preflight integration phase`)
  const context = await readEvidenceContext(contextPath)
  return executeOwnedSuitePhase({
    phase,
    leaseId: `${suite}/topology`,
    authorizedGrant,
    environment: {
      ...topologyEnvironment(context),
      ...runtime.environmentForSuite(suite),
    },
    runtime,
    runId: context.runId,
    platform,
    ...optionalDeadlineAuthority(deadlineAuthority),
  })
}

export async function executeOwnedSuitePhase({
  phase,
  leaseId,
  environment,
  runtime,
  runId,
  deadlineAuthority,
  authorizedGrant,
  platform = process.platform,
  executeOwnedCommand = executeOwnedRuntimeCommand,
}) {
  const canonicalPhase = requireSuitePhase(phase)
  const operationId = canonicalPhase.operationId
  const operationClass = canonicalPhase.kind === 'playwright-remainder'
    ? BROWSERGATE_OPERATION_CLASS.FULL_SUITE
    : BROWSERGATE_OPERATION_CLASS.TOPOLOGY_MATERIALIZATION
  const grant = authorizedGrant ?? requireDeadlineGrant(deadlineAuthority, leaseId, operationClass)
  const requiredLeaseMs = operationClassDeadlineMs(operationClass)
  if (grant.outcome !== 'authorized' || grant.timeoutMs < requiredLeaseMs) {
    emit(operationId, 'not-run', {
      operationClass,
      reason: grant.outcome === 'authorized'
        ? 'insufficient-process-ownership-lease'
        : 'operation-budget-exhausted',
    })
    return 1
  }
  const commandCapability = runtime.sampleCommandCapability()
  // Identity is an immediate-child authority injected by testprocessowner from
  // the request fields below. Repeating it in the command environment would
  // create a second, unauthenticated identity channel.
  const commandEnvironment = sampleProcessEnvironment(environment, {}, process.env)
  const command = suitePhaseCommand(canonicalPhase, commandCapability)
  emit(operationId, 'started', {
    operationClass,
    containmentBackend: 'test-process-owner',
    phaseKind: canonicalPhase.kind,
    deadlineMs: Math.min(SUITE_PHASE_PROCESS_DEADLINE_MS, grant.timeoutMs),
  })
  const processDeadlineMs = Math.min(SUITE_PHASE_PROCESS_DEADLINE_MS, grant.timeoutMs)
  const execution = await executeOwnedCommand({
    operationId,
    runId,
    scenario: `${canonicalPhase.suite ?? 'main'}-${canonicalPhase.kind}`,
    command,
    platform,
    inheritedEnvironment: commandEnvironment,
    deadlineMs: processDeadlineMs,
    terminationGraceMs: OWNED_PROCESS_TERMINATION_GRACE_MS,
    processOwner: runtime.artifactCapability('test-process-owner'),
  })
  process.stdout.write(execution.stdout)
  process.stderr.write(execution.stderr)
  requireSettledOwnedTree(execution, operationId)
  const exitCode = runnerProcessExitCode(execution)
  emit(operationId, exitCode === 0 ? 'completed' : 'failed', {
    operationClass,
    terminationReason: execution.ownershipEvidence.terminationReason,
    processTerminal: execution.processEvidence.terminal,
    exitCode,
  })
  return exitCode
}

function suitePhaseCommand(phase, commandCapability) {
  const node = commandCapability.node
  if (phase.kind === 'playwright-discovery') {
    return Object.freeze({
      executable: node,
      arguments: Object.freeze([PLAYWRIGHT_SUITE_DISCOVERY, phase.suite]),
      cwd: REPOSITORY_ROOT,
    })
  }
  if (phase.kind === 'vitest-integration') {
    return Object.freeze({
      executable: node,
      arguments: Object.freeze([
        VITEST_CLI,
        'run',
        ...phase.testFiles,
        '--reporter=verbose',
      ]),
      cwd: join(REPOSITORY_ROOT, 'web'),
    })
  }
  if (phase.kind === 'playwright-remainder') {
    return Object.freeze({
      executable: node,
      arguments: Object.freeze([
        commandCapability.playwrightRunner,
        '--suite',
        phase.suite,
        '--playwright-cli',
        commandCapability.playwrightCli,
        '--',
        'test',
        '--config',
        phase.configPath,
        '--workers=1',
        '--retries=0',
      ]),
      cwd: join(REPOSITORY_ROOT, 'web'),
    })
  }
  throw new Error(`unsupported owned suite phase ${JSON.stringify(phase.kind)}`)
}

function requireSuitePhase(phase) {
  if (phase === null || typeof phase !== 'object' || typeof phase.operationId !== 'string') {
    throw new Error('owned suite phase must carry an operation identity')
  }
  if (!['playwright-discovery', 'vitest-integration', 'playwright-remainder'].includes(phase.kind)) {
    throw new Error(`unsupported owned suite phase ${JSON.stringify(phase.kind)}`)
  }
  return phase
}

async function loadGuardSemanticEvaluator() {
  const reducer = await import('./generated-semantic/final-semantic-reducer.js')
  if (typeof reducer.evaluateFinalBrowserSample !== 'function') {
    throw new Error('generated semantic reducer does not export its guard evaluator')
  }
  return reducer.evaluateFinalBrowserSample
}

export async function runGuardSuite({
  contextPath,
  suite,
  secretNames = [],
  settlementTrust,
  publisherHelper,
  runtime,
  platform = process.platform,
  deadlineAuthority,
  leaseId,
}) {
  requireSuite(suite)
  const grant = requireDeadlineGrant(
    deadlineAuthority,
    leaseId,
    BROWSERGATE_OPERATION_CLASS.ARTIFACT_GUARD,
  )
  if (grant.outcome !== 'authorized') {
    throw new Error('guard suite deadline lease is unavailable')
  }
  const guardPublisherPhase = suiteExecutionPlan(suite, platform).guardPublisher
  if (settlementTrust === null || typeof settlementTrust !== 'object') {
    throw new Error('guard settlement trust is required')
  }
  const context = await readEvidenceContext(contextPath)
  if (basename(context.root) !== suite) {
    throw new Error('guard suite context root name must equal its suite identity')
  }
  const ledger = readGuardInputLedger(context, suite)
  const expected = expectedSampleIdentities(suite, context.runPolicy)
  if (ledger.samples.length !== expected.length) {
    throw new Error('guard input ledger does not contain every run-policy sample')
  }
  const topologyProfileSnapshot = await readStableRegularFileSnapshot(
    context.profilePath,
    MAXIMUM_CONTRACT_BYTES,
    'guard topology profile',
  )
  const topologyResolutionSnapshot = await readStableRegularFileSnapshot(
    context.resolutionPath,
    MAXIMUM_CONTRACT_BYTES,
    'guard topology resolution',
  )
  if (
    topologyProfileSnapshot.sha256 !== context.topologyProfileSha256 ||
    topologyResolutionSnapshot.sha256 !== context.topologyResolutionSha256
  ) throw new Error('guard topology snapshots differ from their context authority')
  const topologyProfileJson = decodeUtf8(topologyProfileSnapshot.bytes, 'guard topology profile')
  const topologyResolutionJson = decodeUtf8(
    topologyResolutionSnapshot.bytes,
    'guard topology resolution',
  )
  // Generated semantics become observable only after runtime identity, settlement
  // trust, context, ledger, and topology authority have all settled successfully.
  const evaluateFinalBrowserSample = await loadGuardSemanticEvaluator()
  const inputs = []
  for (const [index, identity] of expected.entries()) {
    const claim = ledger.samples[index]
    if (
      claim === undefined ||
      claim.browser !== identity.browser ||
      claim.sampleIndex !== identity.sampleIndex
    ) throw new Error('guard input ledger samples are not in canonical identity order')
    const resultPath = resolvePortableChild(
      context.root,
      claim.resultRelativePath,
      'guard input result',
    )
    const sampleDirectory = dirname(resultPath)
    const expectedSampleDirectory = join(
      context.root,
      identity.browser,
      'sample-' + identity.sampleIndex,
    )
    if (sampleDirectory !== expectedSampleDirectory) {
      throw new Error('guard input result does not occupy its canonical sample directory')
    }
    const artifactRoot = requireCanonicalAbsolutePath(claim.artifactRoot, 'guard artifact root')
    requirePrivateSampleArtifactSibling(
      sampleDirectory,
      artifactRoot,
      'guard artifact root',
    )
    const snapshot = await readStableRegularFileSnapshot(
      resultPath,
      MAXIMUM_CONTRACT_BYTES,
      'browser sample result',
    )
    const sample = (await evaluateFinalBrowserSample({
      result: parseCanonicalJsonText(
        decodeUtf8(snapshot.bytes, 'browser sample result'),
        'browser sample result',
      ),
      topologyProfileJson,
      topologyResolutionJson,
      topologyProfileSha256: context.topologyProfileSha256,
      topologyResolutionSha256: context.topologyResolutionSha256,
    })).result
    if (
      sample.runId !== context.runId ||
      sample.checkoutSha !== context.checkoutSha ||
      sample.suite !== suite ||
      sample.browser !== identity.browser ||
      sample.sampleIndex !== identity.sampleIndex
    ) throw new Error('guard sample result identity does not match its context slot')
    assertBrowserRunPolicyEqual(sample.runPolicy, context.runPolicy, 'guard sample run policy')
    const commandAuthority = await createSampleCommandAuthority({
      context,
      identity,
      suite,
      sampleOutputRoot: dirname(context.root),
      sampleDirectory,
      platform,
      runtime,
    })
    inputs.push(Object.freeze({
      sample,
      sampleResultBytes: snapshot.bytes,
      artifactRoot,
      commandSha256: canonicalSampleCommandSha256(commandAuthority),
      settlementAttestation: claim.settlementAttestation,
    }))
  }
  const uploadParent = join(context.root, GUARD_UPLOAD_PARENT)
  const explicitSecrets = secretNames.map((name) => {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(name)) {
      throw new Error('invalid guard secret environment name ' + JSON.stringify(name))
    }
    const value = process.env[name]
    if (value === undefined || value.length === 0) {
      throw new Error('guard secret environment ' + name + ' must contain a non-empty value')
    }
    return Object.freeze({ value })
  })
  const guarded = await guardArtifactSuite({
    runId: context.runId,
    runPolicy: context.runPolicy,
    suite,
    checkoutSha: context.checkoutSha,
    samples: Object.freeze(inputs),
    uploadParent,
    topology: Object.freeze({
      profileBytes: topologyProfileSnapshot.bytes,
      resolutionBytes: topologyResolutionSnapshot.bytes,
    }),
    settlementTrust,
    directoryPublisher: createNativeDirectoryPublisher(publisherHelper),
    explicitSecrets: Object.freeze(explicitSecrets),
  })
  requireCompleteArtifactGuardTrace(
    guarded.traces,
    `browsergate ${suite} artifact guard trace`,
  )
  const artifactGuardTraces = guarded.traces
  const guardOutcome = aggregateGuardOutcome(guarded.guards, guarded.upload)
  const sampleOutcomes = Object.freeze(guarded.guards.map((guard) => Object.freeze({
    browser: guard.browser,
    sampleIndex: guard.sampleIndex,
    guardOutcome: guard.guardOutcome,
  })))
  const outcome = Object.freeze({
    exitCode: guardOutcome === 'passed' ? 0 : 1,
    guardOutcome,
    uploadDirectory: guarded.upload?.uploadDirectory ?? null,
    manifestSha256: guarded.upload?.manifestSha256 ?? null,
    manifestByteLength: guarded.upload?.manifestByteLength ?? null,
    sampleOutcomes,
    artifactGuardTraces,
    phaseOutcomes: Object.freeze({
      guardPublisher: phaseOutcome(
        guardPublisherPhase,
        guardOutcome === 'passed' ? 0 : 1,
      ),
    }),
  })
  writeAtomicJson(join(context.root, 'orchestration', 'guard-status.json'), {
    schemaVersion: ORCHESTRATION_STATUS_SCHEMA_VERSION,
    runId: context.runId,
    checkoutSha: context.checkoutSha,
    runPolicy: context.runPolicy,
    suite,
    guardOutcome,
    sampleOutcomes,
    artifactGuardTraces,
    phaseOutcomes: outcome.phaseOutcomes,
  })
  emit(guardPublisherPhase.operationId, guardOutcome, {
    runId: context.runId,
    runPolicy: context.runPolicy.policyId,
    sampleCount: sampleOutcomes.length,
    uploadAuthorized: outcome.uploadDirectory !== null,
  })
  return outcome
}

function readGuardInputLedger(context, suite) {
  const path = join(context.root, ...GUARD_INPUT_RELATIVE_PATH.split('/'))
  const value = readCanonicalJson(path, 'guard input ledger')
  requireExactKeys(value, [
    'schemaVersion',
    'runId',
    'checkoutSha',
    'runPolicy',
    'suite',
    'samples',
  ], 'guard input ledger')
  if (
    value.schemaVersion !== GUARD_INPUT_SCHEMA_VERSION ||
    value.runId !== context.runId ||
    value.checkoutSha !== context.checkoutSha ||
    value.suite !== suite
  ) throw new Error('guard input ledger identity does not match its suite context')
  const runPolicy = parseBrowserRunPolicy(value.runPolicy)
  assertBrowserRunPolicyEqual(runPolicy, context.runPolicy, 'guard input ledger run policy')
  if (!Array.isArray(value.samples)) throw new Error('guard input ledger samples must be an array')
  const samples = value.samples.map((claim, index) => {
    requireExactKeys(
      claim,
      [
        'browser',
        'sampleIndex',
        'resultRelativePath',
        'artifactRoot',
        'settlementAttestation',
      ],
      'guard input sample ' + index,
    )
    return Object.freeze({
      browser: requireBrowser(claim.browser),
      sampleIndex: requirePolicySampleIndex(claim.sampleIndex, runPolicy),
      resultRelativePath: requirePortableRelativePath(
        claim.resultRelativePath,
        'guard input result path',
      ),
      artifactRoot: requireCanonicalAbsolutePath(claim.artifactRoot, 'guard input artifact root'),
      settlementAttestation: claim.settlementAttestation,
    })
  })
  return Object.freeze({ ...value, runPolicy, samples: Object.freeze(samples) })
}

function aggregateGuardOutcome(guards, upload) {
  if (upload !== null) {
    if (guards.some((guard) => guard.guardOutcome !== 'passed')) {
      throw new Error('guard upload authority contradicts a non-passed sample guard')
    }
    return 'passed'
  }
  if (guards.some((guard) => guard.guardOutcome === 'failed')) return 'failed'
  if (guards.some((guard) => guard.guardOutcome === 'quarantined')) return 'quarantined'
  return 'failed'
}

export async function buildInvocationRuntime({
  suites,
  runId = 'browsergate',
  outputParent = tmpdir(),
  preserveRuntimeRoot = false,
  executeBuild,
  executePreflight,
  platform = process.platform,
  inheritedEnvironment = process.env,
  deadlineAuthority,
  buildLeaseId,
  preflightLeaseId,
  executeOwnedCommand = executeOwnedRuntimeCommand,
  resolveExecutable = resolveHostExecutable,
  createBootstrapOwner = createBootstrapProcessOwnerAuthority,
  authenticateRuntimeFile = authenticatedFileAuthority,
  readRuntimeNodeVersion = readPinnedNodeVersion,
} = {}) {
  const outputParentPath = resolve(outputParent)
  const defaults = executeBuild === undefined || executePreflight === undefined
    ? await createRuntimeExecutionExecutors({
        platform,
        runId,
        outputParent: outputParentPath,
        inheritedEnvironment,
        deadlineAuthority,
        buildLeaseId,
        preflightLeaseId,
        executeOwnedCommand,
        resolveExecutable,
        createBootstrapOwner,
        authenticateRuntimeFile,
        readRuntimeNodeVersion,
      })
    : undefined
  if (defaults === undefined) {
    const runtime = await buildBrowsergateRuntime({
      repositoryRoot: REPOSITORY_ROOT,
      suites: canonicalRequestedSuites(suites),
      platform,
      outputParent: outputParentPath,
      preserveRuntimeRoot,
      inheritedEnvironment,
      nodeExecutable: process.execPath,
      executeBuild,
      executePreflight,
    })
    replayRuntimeBuildTraces(runtime.traces)
    return runtime
  }

  let runtime
  const failures = []
  try {
    await defaults.verifyGeneratedSemantic()
    runtime = await buildBrowsergateRuntime({
      repositoryRoot: REPOSITORY_ROOT,
      suites: canonicalRequestedSuites(suites),
      platform,
      outputParent: outputParentPath,
      preserveRuntimeRoot,
      inheritedEnvironment,
      nodeExecutable: process.execPath,
      executeBuild: defaults.executeBuild,
      executePreflight: defaults.executePreflight,
    })
    await defaults.verifyHandoff(runtime)
  } catch (cause) {
    failures.push(cause)
  }
  try {
    await defaults.close()
  } catch (cause) {
    failures.push(cause)
  }
  const generatedSemanticTraces = defaults.generatedSemanticTraces.snapshot()
  replayGeneratedSemanticTraces(generatedSemanticTraces)
  if (failures.length > 0) {
    if (runtime !== undefined) {
      try {
        runtime.dispose()
      } catch (cause) {
        failures.push(cause)
      }
    }
    throw new BrowserRuntimeBootstrapError(failures, generatedSemanticTraces)
  }
  replayRuntimeBuildTraces(runtime.traces)
  return Object.freeze({ ...runtime, generatedSemanticTraces })
}

export class BrowserRuntimeBootstrapError extends Error {
  constructor(failures, generatedSemanticTraces) {
    const cause = failures.length === 1
      ? failures[0]
      : new AggregateError(failures, 'browser runtime bootstrap handoff did not settle cleanly')
    super(
      failures.length === 1 && failures[0] instanceof Error
        ? failures[0].message
        : 'browser runtime bootstrap handoff did not settle cleanly',
      { cause },
    )
    this.name = 'BrowserRuntimeBootstrapError'
    this.generatedSemanticTraces = generatedSemanticTraces
  }
}

function replayRuntimeBuildTraces(snapshot) {
  for (const { milestone, context = {} } of snapshot.events) {
    emit('browser-runtime-build', milestone.replace(/^runtime-build-/u, ''), context)
  }
}

function createRuntimeExecutionExecutors({
  platform,
  runId,
  outputParent,
  inheritedEnvironment,
  deadlineAuthority,
  buildLeaseId,
  preflightLeaseId,
  executeOwnedCommand,
  resolveExecutable,
  createBootstrapOwner,
  authenticateRuntimeFile,
  readRuntimeNodeVersion,
}) {
  // Platform entrypoints retain one verified Go application. Nested runtime
  // builds consume that exact handle instead of resolving mutable PATH state.
  const goExecutable = inheritedEnvironment.WINDSHARE_GO_EXECUTABLE
    ?? resolveExecutable('go', { platform, environment: inheritedEnvironment })
  const buildGrant = requireDeadlineGrant(
    deadlineAuthority,
    buildLeaseId,
    BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD,
  )
  const preflightGrant = requireDeadlineGrant(
    deadlineAuthority,
    preflightLeaseId,
    BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
  )
  const ownerSpec = BOOTSTRAP_OWNER_SPECS[platform]
  if (ownerSpec === undefined) {
    throw new Error(`browser runtime bootstrap is unsupported on ${JSON.stringify(platform)}`)
  }
  const blockedGrant = ownedOperationGrantFailure(
    preflightGrant,
    BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
  ) ?? ownedOperationGrantFailure(buildGrant, BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD)
  let bootstrap = null
  let bootstrapPromise
  let adoptedFinalOwner = null
  const generatedSemanticJournal = createOwnedTraceJournal({
    label: 'generated semantic runtime preflight trace',
    maximumEvents: GENERATED_SEMANTIC_MAXIMUM_TRACE_EVENTS,
    maximumBytes: GENERATED_SEMANTIC_MAXIMUM_TRACE_BYTES,
  })

  async function acquireBootstrapOwner() {
    if (blockedGrant !== null) return null
    bootstrapPromise ??= createBootstrapOwner({
      repositoryRoot: REPOSITORY_ROOT,
      outputParent,
      platform,
      goExecutable,
    })
    bootstrap = await bootstrapPromise
    return bootstrap
  }

  function unavailableBootstrapExecution(operationId, operationClass) {
    emit(operationId, 'not-run', {
      runId,
      scenario: 'browsergate-runtime-command',
      operationClass,
      phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      reason: blockedGrant.reason,
      requiredLeaseMs: blockedGrant.requiredLeaseMs,
      remainingBudgetMs: blockedGrant.remainingBudgetMs,
      bootstrapOperationClass: blockedGrant.operationClass,
    })
    return failedOwnedExecution(platform)
  }

  function selectedOwner(availableArtifacts) {
    const finalOwner = runtimeArtifact(availableArtifacts, ownerSpec.kind)
    return finalOwner ?? bootstrap?.artifact
  }

  return Object.freeze({
    generatedSemanticTraces: generatedSemanticJournal.view,
    async verifyGeneratedSemantic() {
      const startedAtMs = Date.now()
      let result
      let execution
      appendGeneratedSemanticTrace(generatedSemanticJournal, 'started', {
        runId,
        scenario: 'generated-semantic-runtime-preflight',
        mode: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE,
        operationClass: BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
        phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
        owner: 'test-process-owner',
      })
      try {
        if (await acquireBootstrapOwner() === null) {
          throw new GeneratedSemanticRuntimePreflightError(
            'bootstrap-owner-unavailable',
            'generated semantic verifier bootstrap owner lease is unavailable',
          )
        }
        const verifierEnvironment = createGeneratedSemanticEnvironment({
          platform,
          temporaryRoot: dirname(bootstrap.artifact.path),
          inheritedEnvironment,
        })
        execution = await executeOwnedRuntimeOperation({
          operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID,
          runId,
          scenario: 'generated-semantic-runtime-preflight',
          authorizedGrant: preflightGrant,
          operationClass: BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
          command: Object.freeze({
            executable: process.execPath,
            arguments: Object.freeze([GENERATED_SEMANTIC_VERIFIER]),
            cwd: REPOSITORY_ROOT,
          }),
          platform,
          inheritedEnvironment: verifierEnvironment,
          ...runtimeOwnerRequest(platform, bootstrap.artifact),
          deadlineAuthority,
          executeOwnedCommand,
        })
        result = requireGeneratedSemanticRuntimeExecution({ execution, platform })
        const expectedArtifact = await authenticateRuntimeFile(
          GENERATED_SEMANTIC_ARTIFACT,
          MAXIMUM_CONTRACT_BYTES,
          'committed generated semantic artifact',
        )
        validateGeneratedSemanticRuntimeEvidence({
          result,
          expectedNodeVersion: readRuntimeNodeVersion(REPOSITORY_ROOT),
          expectedArtifact,
        })
        const evidence = {
          runId,
          scenario: 'generated-semantic-runtime-preflight',
          mode: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE,
          outcome: result.outcome,
          cleanupOutcome: execution.cleanupOutcome,
          treeEmpty: execution.treeEmpty,
          ...generatedSemanticResultTraceContext(result),
        }
        appendGeneratedSemanticTrace(generatedSemanticJournal, 'artifact-validated', evidence)
        appendGeneratedSemanticTrace(generatedSemanticJournal, 'settled', {
          runId,
          scenario: 'generated-semantic-runtime-preflight',
          ...evidence,
          elapsedMs: Date.now() - startedAtMs,
        })
        generatedSemanticJournal.finish()
        requireCompleteOwnedTraceSnapshot(
          generatedSemanticJournal.view.snapshot(),
          'generated semantic runtime preflight trace',
        )
        return execution
      } catch (cause) {
        if (!generatedSemanticJournal.view.snapshot().completed) {
          appendGeneratedSemanticTrace(generatedSemanticJournal, 'settled', {
          runId,
          scenario: 'generated-semantic-runtime-preflight',
          mode: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE,
          outcome: 'failed',
          cleanupOutcome: execution?.cleanupOutcome ?? 'not-observed',
          treeEmpty: execution?.treeEmpty ?? false,
          elapsedMs: Date.now() - startedAtMs,
          ...generatedSemanticPreflightFailureContext(cause),
          })
          generatedSemanticJournal.finish()
        }
        try {
          requireCompleteOwnedTraceSnapshot(
            generatedSemanticJournal.view.snapshot(),
            'generated semantic runtime preflight trace',
          )
        } catch (traceFailure) {
          throw new AggregateError(
            [cause, traceFailure],
            'generated semantic verifier and trace settlement failed',
          )
        }
        throw new Error('generated semantic verifier failed before runtime batch build', { cause })
      }
    },
    executeBuild(build) {
      if (bootstrap === null) {
        return Promise.resolve(unavailableBootstrapExecution(
          'browser-runtime-build-' + build.kind,
          BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD,
        ))
      }
      const owner = selectedOwner(build.availableArtifacts)
      return executeOwnedRuntimeOperation({
        operationId: 'browser-runtime-build-' + build.kind,
        runId,
        scenario: 'browser-runtime-build',
        authorizedGrant: buildGrant,
        operationClass: BROWSERGATE_OPERATION_CLASS.RUNTIME_BUILD,
        command: Object.freeze({
          executable: goExecutable,
          arguments: Object.freeze([
            'build',
            '-trimpath',
            '-buildvcs=false',
            '-ldflags=-buildid=',
            '-o',
            build.outputPath,
            build.packagePath,
          ]),
          cwd: build.cwd,
        }),
        platform,
        inheritedEnvironment,
        ...runtimeOwnerRequest(platform, owner),
        deadlineAuthority,
        executeOwnedCommand,
      })
    },
    async executePreflight(preflight) {
      if (bootstrap === null) {
        return unavailableBootstrapExecution(
          'browser-runtime-preflight-' + preflight.kind,
          BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
        )
      }
      const owner = selectedOwner(preflight.availableArtifacts)
      const execution = await executeOwnedRuntimeOperation({
        operationId: 'browser-runtime-preflight-' + preflight.kind,
        runId,
        scenario: 'browser-runtime-preflight',
        authorizedGrant: preflightGrant,
        operationClass: BROWSERGATE_OPERATION_CLASS.PREFLIGHT,
        command: Object.freeze({
          executable: preflight.executablePath,
          arguments: preflight.arguments,
          cwd: preflight.cwd,
        }),
        platform,
        inheritedEnvironment: Object.freeze({}),
        ...runtimeOwnerRequest(platform, owner),
        deadlineAuthority,
        executeOwnedCommand,
      })
      if (preflight.kind === ownerSpec.kind && successfulOwnedExecution(execution)) {
        adoptedFinalOwner = owner
      }
      return execution
    },
    async verifyHandoff(runtime) {
      if (bootstrap === null || adoptedFinalOwner === null) {
        throw new Error('final runtime process owner did not complete its owned handoff preflight')
      }
      await bootstrap.assertLive()
      const finalOwner = runtime.artifactCapability(ownerSpec.kind)
      if (
        finalOwner.path === bootstrap.artifact.path ||
        finalOwner.path !== adoptedFinalOwner.path
      ) throw new Error('final runtime process owner differs from its handoff evidence')
      await bootstrap.assertLive()
    },
    async close() {
      if (bootstrap !== null) await bootstrap.close()
    },
  })
}

export async function createBootstrapProcessOwnerAuthority({
  repositoryRoot,
  outputParent,
  platform,
  goExecutable,
  buildOwner = buildTestProcessOwnerFixture,
}) {
  const ownerSpec = BOOTSTRAP_OWNER_SPECS[platform]
  if (ownerSpec === undefined) {
    throw new Error(`bootstrap process owner is unsupported on ${JSON.stringify(platform)}`)
  }
  const parent = requireCanonicalAbsolutePath(resolve(outputParent), 'bootstrap output parent')
  mkdirSync(parent, { recursive: true, mode: 0o700 })
  const root = resolve(mkdtempSync(join(parent, BOOTSTRAP_RUNTIME_DIRECTORY_PREFIX)))
  const outputPath = join(root, ownerSpec.filename)
  try {
    const built = await buildOwner({
      repositoryRoot: requireCanonicalAbsolutePath(
        resolve(repositoryRoot),
        'bootstrap repository root',
      ),
      goExecutable: requireCanonicalAbsolutePath(
        resolve(goExecutable),
        'bootstrap Go executable',
      ),
      outputPath,
    })
    if (built?.path !== outputPath) {
      throw new Error('process owner fixture builder returned a different output path')
    }
    assertNonemptyRegularFile(outputPath, 'bootstrap process owner fixture')
    const artifact = Object.freeze({
      kind: ownerSpec.kind,
      path: outputPath,
    })
    let closed = false
    return Object.freeze({
      artifact,
      async assertLive() {
        if (closed) throw new Error('bootstrap process owner authority is closed')
        assertNonemptyRegularFile(outputPath, 'bootstrap process owner fixture')
      },
      async close() {
        if (closed) return
        closed = true
        const failures = []
        try {
          rmSync(root, { recursive: true, force: true })
        } catch (cause) {
          failures.push(cause)
        }
        if (failures.length === 1) throw failures[0]
        if (failures.length > 1) {
          throw new AggregateError(failures, 'bootstrap process owner cleanup did not settle')
        }
      },
    })
  } catch (cause) {
    const failures = [cause]
    try {
      rmSync(root, { recursive: true, force: true })
    } catch (cleanupCause) {
      failures.push(cleanupCause)
    }
    if (failures.length === 1) throw failures[0]
    throw new AggregateError(failures, 'bootstrap process owner creation did not settle')
  }
}

function ownedOperationGrantFailure(grant, operationClass) {
  const requiredLeaseMs = operationClassDeadlineMs(operationClass)
  if (grant.outcome !== 'authorized') {
    return Object.freeze({
      operationClass,
      reason: 'operation-budget-exhausted',
      requiredLeaseMs,
      remainingBudgetMs: grant.remainingBudgetMs,
    })
  }
  if (grant.timeoutMs < requiredLeaseMs) {
    return Object.freeze({
      operationClass,
      reason: 'insufficient-process-ownership-lease',
      requiredLeaseMs,
      remainingBudgetMs: grant.remainingBudgetMs,
    })
  }
  return null
}

function runtimeOwnerRequest(platform, artifact) {
  if (artifact === undefined || artifact === null) {
    throw new Error('runtime process owner authority is unavailable')
  }
  if (platform === 'win32' || platform === 'linux') {
    if (artifact.kind !== 'test-process-owner' || typeof artifact.path !== 'string') {
      throw new Error('runtime process owner artifact is invalid')
    }
    // The runtime manifest's kind authenticates artifact selection, but it is
    // not launch authority. Project only the held path into the process-owner
    // client so metadata cannot silently widen that client's capability shape.
    return Object.freeze({
      processOwner: Object.freeze({ path: artifact.path }),
    })
  }
  throw new Error(`runtime process owner is unsupported on ${JSON.stringify(platform)}`)
}

function successfulOwnedExecution(execution) {
  return execution.treeEmpty === true && execution.cleanupOutcome === 'completed' &&
    execution.ownershipEvidence.terminationReason === 'natural' &&
    execution.processEvidence.terminal === 'exited' &&
    execution.processEvidence.exitCode === 0
}

async function executeOwnedRuntimeOperation({
  operationId,
  leaseId,
  authorizedGrant,
  operationClass,
  command,
  platform,
  runId = 'browsergate',
  scenario = 'browsergate-runtime-command',
  inheritedEnvironment,
  processOwner,
  deadlineAuthority,
  executeOwnedCommand,
}) {
  const operationJournal = createOwnedTraceJournal({
    label: 'owned runtime operation trace',
    maximumEvents: OWNED_RUNTIME_OPERATION_MAXIMUM_TRACE_EVENTS,
    maximumBytes: OWNED_RUNTIME_OPERATION_MAXIMUM_TRACE_BYTES,
  })
  const grant = authorizedGrant ?? requireDeadlineGrant(
    deadlineAuthority,
    leaseId,
    operationClass,
  )
  const requiredLeaseMs = operationClassDeadlineMs(operationClass)
  if (grant.outcome !== 'authorized' || grant.timeoutMs < requiredLeaseMs) {
    const traces = settleOwnedRuntimeOperationTrace(operationJournal, operationId, 'not-run', {
      runId,
      scenario,
      operationClass,
      phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      reason: grant.outcome === 'authorized'
        ? 'insufficient-process-ownership-lease'
        : 'operation-budget-exhausted',
      requiredLeaseMs,
      remainingBudgetMs: grant.remainingBudgetMs,
    })
    replayOwnedRuntimeOperationTraces(traces)
    return Object.freeze({
      ...failedOwnedExecution(platform),
      traces,
      runtimeCommandTraces: null,
    })
  }
  const processDeadlineMs = grant.timeoutMs - RUNTIME_PROCESS_CLEANUP_RESERVE_MS
  if (processDeadlineMs < 1) {
    const traces = settleOwnedRuntimeOperationTrace(operationJournal, operationId, 'not-run', {
      runId,
      scenario,
      operationClass,
      phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      reason: 'insufficient-process-cleanup-reserve',
      cleanupReserveMs: RUNTIME_PROCESS_CLEANUP_RESERVE_MS,
    })
    replayOwnedRuntimeOperationTraces(traces)
    return Object.freeze({
      ...failedOwnedExecution(platform),
      traces,
      runtimeCommandTraces: null,
    })
  }
  const startedAtMs = Date.now()
  appendOwnedRuntimeOperationTrace(operationJournal, operationId, 'started', {
    runId,
    scenario,
    operationClass,
    phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    timeoutMs: processDeadlineMs,
    cleanupReserveMs: RUNTIME_PROCESS_CLEANUP_RESERVE_MS,
    owner: 'test-process-owner',
  })
  let consumption
  try {
    const channel = executeOwnedCommand({
      operationId,
      runId,
      scenario,
      command,
      platform,
      inheritedEnvironment,
      deadlineMs: processDeadlineMs,
      terminationGraceMs: OWNED_PROCESS_TERMINATION_GRACE_MS,
      processOwner,
    })
    consumption = await consumeOwnedRuntimeCommandExecution(
      channel,
      Object.freeze({ operationId, platform }),
    )
  } catch (cause) {
    consumption = Object.freeze({
      outcome: 'rejected',
      execution: null,
      traces: null,
      failure: opaqueRuntimeOperationFailure(
        cause,
        'runtime command consumer failed before settlement',
      ),
      failureKind: RUNTIME_COMMAND_CONTRACT_FAILURE_KIND,
    })
  }

  const runtimeCommandTraces = consumption.traces
  if (consumption.outcome === 'rejected') {
    const traces = settleOwnedRuntimeOperationTrace(operationJournal, operationId, 'failed', {
      runId,
      scenario,
      operationClass,
      phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      elapsedMs: Date.now() - startedAtMs,
      failureKind: consumption.failureKind,
    })
    if (runtimeCommandTraces !== null) {
      replayRuntimeCommandOwnerTraces(runtimeCommandTraces, runId, scenario)
    }
    replayOwnedRuntimeOperationTraces(traces)
    throw ownedRuntimeOperationFailure({
      operationFailure: consumption.failure,
      operationTraces: traces,
      runtimeCommandTraces,
    })
  }

  const execution = consumption.execution
  const passed = successfulOwnedExecution(execution)
  const traces = settleOwnedRuntimeOperationTrace(
    operationJournal,
    operationId,
    passed ? 'completed' : 'failed',
    {
      runId,
      scenario,
      operationClass,
      phase: BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
      elapsedMs: Date.now() - startedAtMs,
      terminationReason: execution.ownershipEvidence.terminationReason,
      treeEmpty: execution.treeEmpty,
      cleanupOutcome: execution.cleanupOutcome,
      terminal: execution.processEvidence.terminal,
      ...(execution.processEvidence.terminal === 'exited'
        ? { exitCode: execution.processEvidence.exitCode }
        : {}),
    },
  )
  replayRuntimeCommandOwnerTraces(runtimeCommandTraces, runId, scenario)
  replayOwnedRuntimeOperationTraces(traces)
  return Object.freeze({ ...execution, traces, runtimeCommandTraces })
}

function appendOwnedRuntimeOperationTrace(journal, operationId, milestone, context) {
  if (!journal.append({ operationId, milestone, context })) {
    journal.finish()
    requireCompleteOwnedTraceSnapshot(journal.view.snapshot(), 'owned runtime operation trace')
  }
}

function settleOwnedRuntimeOperationTrace(journal, operationId, milestone, context) {
  appendOwnedRuntimeOperationTrace(journal, operationId, milestone, context)
  journal.finish()
  return requireCompleteOwnedTraceSnapshot(
    journal.view.snapshot(),
    'owned runtime operation trace',
  )
}

function replayOwnedRuntimeOperationTraces(snapshot) {
  for (const { operationId, milestone, context } of snapshot.events) {
    emit(operationId, milestone, context)
  }
}

function replayRuntimeCommandOwnerTraces(snapshot, runId, scenario) {
  for (const { milestone, context } of snapshot.events) {
    emit(context.operationId, milestone, { runId, scenario, ...context })
  }
}

function opaqueRuntimeOperationFailure(cause, message) {
  return Object.freeze(new Error(message, { cause }))
}

function ownedRuntimeOperationFailure({
  operationFailure,
  operationTraces,
  runtimeCommandTraces,
}) {
  const failure = new Error('owned runtime operation failed', { cause: operationFailure })
  Object.defineProperties(failure, {
    operationTraces: {
      value: operationTraces,
      enumerable: true,
      writable: false,
      configurable: false,
    },
    runtimeCommandTraces: {
      value: runtimeCommandTraces,
      enumerable: true,
      writable: false,
      configurable: false,
    },
  })
  return Object.freeze(failure)
}

function runtimeArtifact(artifacts, kind) {
  return artifacts.find((artifact) => artifact.kind === kind)
}

function failedOwnedExecution(platform = process.platform) {
  const backend = platform === 'win32' ? 'windows_job' : 'linux_subreaper'
  return Object.freeze({
    processEvidence: Object.freeze({
      terminal: 'spawn-failed',
      errorCode: 'NOT_AUTHORIZED',
      errorMessage: 'runtime operation did not receive a full process-ownership lease',
    }),
    treeEmpty: false,
    cleanupOutcome: 'failed',
    inputEvidence: Object.freeze({
      outcome: 'not_started',
      failureCode: 'NOT_AUTHORIZED',
      failureMessage: 'runtime operation did not receive a full process-ownership lease',
    }),
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner',
      backend,
      terminationReason: 'owner_failure',
      platform: Object.freeze({ kind: backend }),
    }),
    stdout: '',
    stderr: '',
  })
}

function appendGeneratedSemanticTrace(journal, milestone, context) {
  journal.append(Object.freeze({
    operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID,
    milestone,
    context: Object.freeze({ ...context }),
  }))
}

function replayGeneratedSemanticTraces(snapshot) {
  for (const { operationId, milestone, context } of snapshot.events) {
    emit(operationId, milestone, context)
  }
}

function loadRuntimeFromOptions(options, suites, allowEnvironment = false) {
  const manifestPath = optionalOption(options, 'runtime-manifest') ??
    (allowEnvironment ? process.env[BROWSERGATE_RUNTIME_MANIFEST_ENV] : undefined)
  if (manifestPath === undefined || manifestPath === '') {
    throw new Error('browsergate runtime manifest path is required')
  }
  const runtime = loadBrowsergateRuntime({
    manifestPath: requireCanonicalAbsolutePath(manifestPath, 'browsergate runtime manifest'),
  })
  try {
    assertRuntimeSuiteCoverage(runtime.manifest.suites, suites)
  } catch (cause) {
    runtime.dispose()
    throw cause
  }
  return runtime
}

export function assertRuntimeSuiteCoverage(authorizedSuites, requestedSuites) {
  const authorized = canonicalRequestedSuites(authorizedSuites)
  const requested = canonicalRequestedSuites(requestedSuites)
  if (requested.some((suite) => !authorized.includes(suite))) {
    throw new Error('browsergate runtime does not authorize every requested command suite')
  }
  return requested
}

function canonicalRequestedSuites(suites) {
  if (!Array.isArray(suites) || suites.length < 1) {
    throw new Error('at least one browsergate runtime suite is required')
  }
  const canonical = BROWSER_SUITES.filter((suite) => suites.includes(suite))
  if (
    canonical.length !== suites.length ||
    canonical.some((suite, index) => suite !== suites[index])
  ) throw new Error('browsergate runtime suites must be canonical and unique')
  return Object.freeze(canonical)
}

function writeRuntimeGithubOutputs(path, runtime) {
  const values = {
    runtime_manifest_path: runtime.manifestPath,
  }
  for (const [name, value] of Object.entries(values)) {
    if (typeof value !== 'string' || value === '' || /[\r\n]/u.test(value)) {
      throw new Error('GitHub runtime output ' + name + ' is not non-empty single-line text')
    }
  }
  appendFileSync(path, Object.entries(values).map(([name, value]) =>
    name + '=' + value + '\n').join(''), 'utf8')
}

function runnerProcessExitCode(execution) {
  if (
    execution.ownershipEvidence?.terminationReason !== 'natural' ||
    execution.processEvidence?.terminal !== 'exited'
  ) return 1
  return execution.processEvidence.exitCode
}

export async function createSampleCommandAuthority({
  context,
  identity,
  suite,
  sampleOutputRoot,
  sampleDirectory,
  platform,
  runtime,
  focusedConfigPath,
}) {
  requireSuite(suite)
  requireBrowser(identity.browser)
  if (identity.suite !== suite) throw new Error('sample command identity differs from its suite')
  if (!['linux', 'win32'].includes(platform)) {
    throw new Error(`sample command authority does not support platform ${JSON.stringify(platform)}`)
  }
  const ownerKind = 'test-process-owner'
  const commandCapability = runtime.sampleCommandCapability()
  if (commandCapability.repositoryRoot !== REPOSITORY_ROOT) {
    throw new Error('sample runtime repository capability differs from the orchestrator root')
  }
  const ownerArtifact = runtime.artifactCapability(ownerKind)
  const child = sampleChildCommand({
    suite,
    browser: identity.browser,
    platform,
    commandCapability,
    focusedConfigPath,
  })
  const operationId = `${suite}-${identity.browser}-sample-${identity.sampleIndex}`
  const scenario = `${suite}-focused-product`
  const semanticEnvironment = Object.freeze({
    ...topologyEnvironment(context),
    ...runtime.environmentForSuite(suite),
  })
  const driverEnvironment = commandCapability.environment
  const leafEnvironment = sampleProcessEnvironment(
    semanticEnvironment,
    {},
    commandCapability.environment,
  )
  const runtimeManifest = runtime.manifestPath
  return Object.freeze({
    repository: Object.freeze({
      root: commandCapability.repositoryRoot,
      checkoutSha: context.checkoutSha,
    }),
    driver: Object.freeze({
      node: commandCapability.node,
      source: commandCapability.driverSource,
      cwd: commandCapability.repositoryRoot,
      environment: driverEnvironment,
    }),
    identity: Object.freeze({
      runId: context.runId,
      operationId,
      scenario,
      runPolicy: context.runPolicy,
      suite,
      browser: identity.browser,
      sampleIndex: identity.sampleIndex,
      checkoutSha: context.checkoutSha,
    }),
    topology: Object.freeze({
      topologyId: context.topologyLock.profile.topologyId,
      profilePath: context.profilePath,
      profileSha256: context.topologyProfileSha256,
      resolutionPath: context.resolutionPath,
      resolutionSha256: context.topologyResolutionSha256,
    }),
    runtime: Object.freeze({
      manifest: runtimeManifest,
      processOwner: Object.freeze({
        kind: ownerKind,
        path: ownerArtifact.path,
      }),
    }),
    output: Object.freeze({
      root: sampleOutputRoot,
      sampleDirectory,
      resultPath: join(sampleDirectory, 'result.json'),
    }),
    ownership: Object.freeze({
      platform,
      backend: 'inherited',
      outerAuthority: Object.freeze({
        kind: 'test-process-owner',
        backend: platform === 'linux' ? 'linux_subreaper' : 'windows_job',
        operationId,
      }),
      operationClass: BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE,
      classDeadlineMs: operationClassDeadlineMs(BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE),
      childDeadlineMs: BROWSER_SAMPLE_PROCESS_DEADLINE_MS,
    }),
    leaf: Object.freeze({
      executable: commandCapability.node,
      entrypoint: commandCapability.playwrightRunner,
      arguments: child.arguments,
      cwd: join(commandCapability.repositoryRoot, 'web'),
      environment: leafEnvironment,
    }),
  })
}

function requireSettledSampleExecution(execution) {
  requireSettledOwnedTree(execution, 'sample driver')
  if (
    execution.processEvidence?.terminal !== 'exited' ||
    ![0, 1].includes(execution.processEvidence.exitCode)
  ) throw new Error('sample driver did not prove an exited process and empty descendant tree')
  if (execution.inputEvidence?.outcome !== 'delivered') {
    throw new Error('sample driver request was not delivered through its exact input channel')
  }
}

function requireSettledOwnedTree(execution, label) {
  if (
    execution?.treeEmpty !== true || execution.cleanupOutcome !== 'completed' ||
    execution.ownershipEvidence?.kind !== 'test-process-owner' ||
    execution.ownershipEvidence.terminationReason !== 'natural'
  ) throw new Error(`${label} did not prove completed cleanup of an empty owned process tree`)
}

async function authenticatedFileAuthority(path, maximumBytes, label) {
  const canonicalPath = requireCanonicalAbsolutePath(path, label)
  const snapshot = await readStableRegularFileSnapshot(canonicalPath, maximumBytes, label)
  if (snapshot.bytes.byteLength < 1) throw new Error(`${label} is empty`)
  return Object.freeze({
    path: canonicalPath,
    byteLength: snapshot.bytes.byteLength,
    sha256: snapshot.sha256,
  })
}

function assertNonemptyRegularFile(path, label) {
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error(`${label} is not a non-empty regular file`)
  }
}

export function parseSampleRunnerRecord(stdout, identity, sampleDirectory) {
  const lines = stdout.split(/\r?\n/u).filter((line) => line !== '')
  if (lines.length !== 1) throw new Error('sample driver must emit exactly one result record')
  const value = parseCanonicalJsonText(lines[0], 'sample driver result')
  requireExactKeys(value, [
    'schemaVersion',
    'runId',
    'operationId',
    'scenario',
    'outcome',
    'resultPath',
    'artifactRoot',
    'candidate',
    'acceptedBeforeGuard',
    'traces',
  ], 'sample driver result')
  if (
    value.schemaVersion !== BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION ||
    value.runId !== identity.runId || value.operationId !== identity.operationId ||
    value.scenario !== identity.scenario ||
    value.outcome !== (value.acceptedBeforeGuard === true ? 'succeeded' : 'failed') ||
    typeof value.acceptedBeforeGuard !== 'boolean'
  ) throw new Error('sample driver emitted an invalid terminal record')
  const traces = requireCompleteSampleDriverTraces(value.traces, identity)
  const expectedResultPath = join(sampleDirectory, 'result.json')
  const resultPath = requireCanonicalAbsolutePath(value.resultPath, 'sample driver result path')
  if (resultPath !== expectedResultPath) {
    throw new Error('sample driver result path does not match its sample slot')
  }
  const artifactRoot = requireCanonicalAbsolutePath(value.artifactRoot, 'sample artifact root')
  requirePrivateSampleArtifactSibling(sampleDirectory, artifactRoot, 'sample artifact root')
  return Object.freeze({ ...value, traces, resultPath, artifactRoot, identity })
}

function requireCompleteSampleDriverTraces(value, identity) {
  requireExactKeys(value, [
    'events',
    'observedEvents',
    'capturedEvents',
    'truncated',
    'completed',
  ], 'sample driver lifecycle traces')
  if (
    !Array.isArray(value.events) ||
    !Number.isSafeInteger(value.observedEvents) ||
    !Number.isSafeInteger(value.capturedEvents) ||
    value.observedEvents < 2 ||
    value.observedEvents !== value.capturedEvents ||
    value.capturedEvents !== value.events.length ||
    value.truncated !== false ||
    value.completed !== true
  ) throw new Error('sample driver lifecycle traces are incomplete')
  const events = value.events.map((event, index) => requireSampleDriverTrace(event, identity, index))
  if (
    events[0].milestone !== 'operation-started' || events[0].outcome !== 'started' ||
    events.at(-1).milestone !== 'operation-terminal' || events.at(-1).outcome !== 'succeeded'
  ) throw new Error('sample driver lifecycle traces lack exact start and terminal events')
  return Object.freeze({ ...value, events: Object.freeze(events) })
}

function requireSampleDriverTrace(value, identity, index) {
  const fields = [
    'schemaVersion',
    'component',
    'runId',
    'operationId',
    'scenario',
    'outcome',
    'milestone',
    'suite',
    'browser',
    'sampleIndex',
  ]
  if (value !== null && typeof value === 'object' && Object.hasOwn(value, 'context')) {
    fields.push('context')
  }
  requireExactKeys(value, fields, `sample driver lifecycle trace ${index + 1}`)
  if (
    value.schemaVersion !== BROWSER_SAMPLE_TRACE_SCHEMA_VERSION ||
    value.component !== 'browser-evidence-runner' ||
    value.runId !== identity.runId || value.operationId !== identity.operationId ||
    value.scenario !== identity.scenario || value.suite !== identity.suite ||
    value.browser !== identity.browser || value.sampleIndex !== identity.sampleIndex ||
    !['started', 'succeeded', 'failed'].includes(value.outcome) ||
    typeof value.milestone !== 'string' || value.milestone.length < 1
  ) throw new Error(`sample driver lifecycle trace ${index + 1} has invalid identity or semantics`)
  if (
    Object.hasOwn(value, 'context') &&
    (value.context === null || typeof value.context !== 'object' || Array.isArray(value.context))
  ) throw new Error(`sample driver lifecycle trace ${index + 1} has invalid context`)
  return Object.freeze(value)
}

function parseMaterializationRecord(stdout) {
  const lines = stdout.split(/\r?\n/u).filter((line) => line !== '')
  if (lines.length !== 1) throw new Error('topology materializer must emit exactly one record')
  const value = JSON.parse(lines[0])
  requireExactKeys(value, [
    'component',
    'outcome',
    'profilePath',
    'resolutionPath',
    'topologyProfileSha256',
    'topologyResolutionSha256',
  ], 'topology materialization record')
  if (
    value.component !== 'browser-evidence-topology-resolution' ||
    value.outcome !== 'materialized'
  ) throw new Error('topology materializer did not report successful materialization')
  return Object.freeze(value)
}

function topologyCliArguments(context) {
  return [
    '--profile',
    context.profilePath,
    '--profile-sha256',
    context.topologyProfileSha256,
    '--resolution',
    context.resolutionPath,
    '--resolution-sha256',
    context.topologyResolutionSha256,
  ]
}

function topologyEnvironment(context) {
  return Object.freeze({
    WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE: context.profilePath,
    WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION: context.resolutionPath,
    WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256: context.topologyProfileSha256,
    WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256: context.topologyResolutionSha256,
  })
}

function verdictArguments({
  runId,
  checkoutSha,
  mainRoot,
  pionRoot,
  mainJobOutcome,
  pionJobOutcome,
  mainGuardOutcome,
  pionGuardOutcome,
  mainManifestSha256,
  pionManifestSha256,
  mainManifestByteLength,
  pionManifestByteLength,
  mainDownloadOutcome,
  pionDownloadOutcome,
  output,
}) {
  return [
    VERDICT_CLI,
    '--run-id',
    runId,
    '--checkout-sha',
    checkoutSha,
    '--main-root',
    mainRoot,
    '--pion-root',
    pionRoot,
    '--suite-job-outcome',
    'main=' + mainJobOutcome,
    '--suite-job-outcome',
    'pion=' + pionJobOutcome,
    '--guard-outcome',
    'main=' + mainGuardOutcome,
    '--guard-outcome',
    'pion=' + pionGuardOutcome,
    '--manifest-sha256',
    'main=' + mainManifestSha256,
    '--manifest-sha256',
    'pion=' + pionManifestSha256,
    '--manifest-byte-length',
    'main=' + mainManifestByteLength,
    '--manifest-byte-length',
    'pion=' + pionManifestByteLength,
    '--download-outcome',
    'main=' + mainDownloadOutcome,
    '--download-outcome',
    'pion=' + pionDownloadOutcome,
    '--output',
    output,
  ]
}

function writeGuardGithubOutputs(path, outcome) {
  const values = {
    guard_outcome: outcome.guardOutcome,
    sealed_upload_path: outcome.uploadDirectory ?? '',
    manifest_sha256: outcome.manifestSha256 ?? '',
    manifest_byte_length: outcome.manifestByteLength ?? '',
  }
  for (const [name, value] of Object.entries(values)) {
    if (typeof value !== 'string' || /[\r\n]/u.test(value)) {
      throw new Error('GitHub output ' + name + ' is not single-line text')
    }
  }
  appendFileSync(path, Object.entries(values).map(([name, value]) =>
    name + '=' + value + '\n').join(''), 'utf8')
}

function writeSettlementGithubOutputs(path, trust) {
  requireExactKeys(trust, [
    'invocationId',
    'publicKeySpkiBase64',
    'publicKeySha256',
  ], 'process settlement trust anchor')
  const values = {
    settlement_invocation_id: trust.invocationId,
    settlement_public_key_spki_base64: trust.publicKeySpkiBase64,
    settlement_public_key_sha256: trust.publicKeySha256,
  }
  for (const [name, value] of Object.entries(values)) {
    if (typeof value !== 'string' || value === '' || /[\r\n]/u.test(value)) {
      throw new Error('GitHub settlement output ' + name + ' is not non-empty single-line text')
    }
  }
  appendFileSync(path, Object.entries(values).map(([name, value]) =>
    name + '=' + value + '\n').join(''), 'utf8')
}

function settlementTrustFromOptions(options) {
  return Object.freeze({
    invocationId: requiredOption(options, 'settlement-invocation-id'),
    publicKeySpkiBase64: requiredOption(options, 'settlement-public-key-spki-base64'),
    publicKeySha256: requiredOption(options, 'settlement-public-key-sha256'),
  })
}

export function executeOperation(
  operationId,
  leaseId,
  operationClass,
  command,
  {
    captureStdout = false,
    phase = BROWSERGATE_OPERATION_PHASE.NORMAL_WORK,
    executeCommand = spawnSync,
    deadlineAuthority,
    authorizedGrant,
  } = {},
) {
  const grant = authorizedGrant ?? requireDeadlineGrant(
    deadlineAuthority,
    leaseId,
    operationClass,
  )
  const requiredLeaseMs = operationClassDeadlineMs(operationClass)
  if (grant.outcome !== 'authorized') {
    emit(operationId, 'not-run', {
      operationClass,
      leaseId,
      phase,
      reason: 'operation-budget-exhausted',
    })
    return Object.freeze({ exitCode: 1, stdout: '', timedOut: false, launched: false })
  }
  if (
    operationOwnsProcessTree(operationClass) &&
    grant.timeoutMs < requiredLeaseMs
  ) {
    emit(operationId, 'not-run', {
      operationClass,
      phase,
      reason: 'insufficient-process-ownership-lease',
      requiredLeaseMs,
      remainingBudgetMs: grant.remainingBudgetMs,
    })
    return Object.freeze({ exitCode: 1, stdout: '', timedOut: false, launched: false })
  }
  const environment = command.cleanEnvironment
    ? sampleProcessEnvironment(command.environment, {}, process.env)
    : { ...process.env, ...(command.environment ?? {}) }
  emit(operationId, 'started', {
    operationClass,
    phase,
    timeoutMs: grant.timeoutMs,
    cleanEnvironment: command.cleanEnvironment,
  })
  const result = executeCommand(command.executable, command.arguments, {
    cwd: command.cwd,
    env: environment,
    shell: false,
    stdio: captureStdout ? ['ignore', 'pipe', 'inherit'] : 'inherit',
    ...(captureStdout ? { encoding: 'utf8' } : {}),
    timeout: grant.timeoutMs,
    killSignal: 'SIGKILL',
  })
  const timedOut = result.error?.code === 'ETIMEDOUT'
  if (result.error !== undefined && !timedOut) {
    process.stderr.write((result.error.stack ?? result.error.message) + '\n')
  }
  const exitCode = Number.isInteger(result.status) ? result.status : 1
  emit(operationId, exitCode === 0 ? 'completed' : 'failed', {
    operationClass,
    phase,
    exitCode,
    timedOut,
  })
  return Object.freeze({
    exitCode,
    stdout: typeof result.stdout === 'string' ? result.stdout : '',
    timedOut,
    launched: result.error?.code !== 'ENOENT',
  })
}

function requireDeadlineGrant(deadlineAuthority, leaseId, operationClass) {
  if (deadlineAuthority === null || typeof deadlineAuthority !== 'object') {
    throw new Error('Browsergate operation requires an entry deadline authority')
  }
  if (typeof leaseId !== 'string' || leaseId.length === 0) {
    throw new Error('Browsergate operation requires an exact deadline lease ID')
  }
  const grant = deadlineAuthority.grant(leaseId)
  if (
    grant.outcome === 'authorized'
    && grant.operationClass !== undefined
    && grant.operationClass !== operationClass
  ) throw new Error('Browsergate deadline lease operation class is inconsistent')
  return grant
}

function operationOwnsProcessTree(operationClass) {
  return operationClass === BROWSERGATE_OPERATION_CLASS.BROWSER_SAMPLE
}

function runOperation(operationId, leaseId, operationClass, command, deadlineAuthority) {
  return executeOperation(operationId, leaseId, operationClass, command, {
    deadlineAuthority,
  }).exitCode
}

function commandSpec(
  executable,
  arguments_,
  environment = {},
  cwd = REPOSITORY_ROOT,
  cleanEnvironment = false,
) {
  return Object.freeze({
    executable,
    arguments: Object.freeze([...arguments_]),
    environment: Object.freeze({ ...environment }),
    cwd,
    cleanEnvironment,
  })
}

function selectedRunPolicy(options, fallback) {
  const value = optionalOption(options, 'run-policy') ?? fallback
  return browserRunPolicy(parseBrowserRunPolicyId(value))
}

function localGuardSecretNames(requested) {
  const names = [
    ...requested,
    ...(process.env.GITHUB_TOKEN === undefined || process.env.GITHUB_TOKEN === ''
      ? []
      : ['GITHUB_TOKEN']),
  ]
  return Object.freeze([...new Set(names)].sort())
}

function gitCheckoutSha(bootstrapQueryGrant) {
  const execution = executeOperation(
    'checkout-identity',
    bootstrapQueryGrant.leaseId,
    BROWSERGATE_OPERATION_CLASS.SOURCE_CONTROL_QUERY,
    commandSpec('git', ['rev-parse', '--verify', 'HEAD']),
    { captureStdout: true, authorizedGrant: bootstrapQueryGrant },
  )
  if (execution.exitCode !== 0) throw new Error('cannot resolve checkout SHA')
  return requireCheckoutSha(execution.stdout.trim())
}

function localRunId(checkoutSha) {
  return 'local-' + checkoutSha.slice(0, 12) + '-' + Date.now() + '-' + process.pid
}

function pnpmExecutable() {
  return resolveHostExecutable('pnpm')
}

function readCanonicalJson(path, label) {
  let encoded
  try {
    encoded = readFileSync(path, 'utf8')
  } catch (cause) {
    throw new Error('cannot read ' + label + ': ' + errorMessage(cause), { cause })
  }
  return parseCanonicalJsonText(encoded, label)
}

function writeAtomicJson(path, value) {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 })
  const temporary = path + '.' + process.pid + '.' + Date.now() + '.tmp'
  const encoded = JSON.stringify(value)
  try {
    writeFileSync(temporary, encoded, { encoding: 'utf8', flag: 'wx', mode: 0o600 })
    renameSync(temporary, path)
  } finally {
    rmSync(temporary, { force: true })
  }
}

function resolvePortableChild(root, relativePath, label) {
  const portable = requirePortableRelativePath(relativePath, label)
  const path = resolve(root, ...portable.split('/'))
  assertRootConfined(root, path, label)
  return path
}

function requirePortableRelativePath(value, label) {
  if (
    typeof value !== 'string' ||
    value === '' ||
    value.includes('\\') ||
    value.startsWith('/') ||
    /^[A-Za-z]:/u.test(value) ||
    value.split('/').some((segment) => segment === '' || segment === '.' || segment === '..')
  ) throw new Error(label + ' must be a portable root-confined relative path')
  return value
}

function assertRootConfined(root, path, label) {
  const child = relative(root, path)
  if (child === '' || child === '..' || child.startsWith('..' + sep) || isAbsolute(child)) {
    throw new Error(label + ' must be a strict descendant of its owned root')
  }
}

function requirePrivateSampleArtifactSibling(sampleDirectory, artifactRoot, label) {
  const canonicalArtifactRoot = requireFinalizedArtifactCollectionRoot(
    requireCanonicalAbsolutePath(sampleDirectory, 'sample directory'),
    requireCanonicalAbsolutePath(artifactRoot, label),
  )
  const metadata = lstatSync(canonicalArtifactRoot)
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new Error(label + ' must be a non-symbolic directory')
  }
  return canonicalArtifactRoot
}

function requireRegularNoFollowPath(path, label) {
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error(label + ' must be a regular non-symbolic file')
  }
}

function isRegularNoFollowFile(path) {
  try {
    const metadata = lstatSync(path)
    return metadata.isFile() && !metadata.isSymbolicLink()
  } catch {
    return false
  }
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(label + ' must be an absolute canonical path')
  }
  return value
}

function requireExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(label + ' must be an object')
  }
  const actual = Object.keys(value).sort()
  const canonical = [...expected].sort()
  if (
    actual.length !== canonical.length ||
    actual.some((name, index) => name !== canonical[index])
  ) throw new Error(label + ' has an invalid field set')
  return value
}

function requirePortableToken(value, label) {
  if (
    typeof value !== 'string' ||
    value.length < 1 ||
    value.length > 128 ||
    !PORTABLE_TOKEN_PATTERN.test(value)
  ) throw new Error(label + ' must be a portable token')
  return value
}

function requireCheckoutSha(value) {
  if (typeof value !== 'string' || !CHECKOUT_SHA_PATTERN.test(value)) {
    throw new Error('checkout SHA must be a lowercase 40-character object ID')
  }
  return value
}

function requireSha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(label + ' must be a lowercase SHA-256 digest')
  }
  return value
}

function requirePolicySampleIndex(value, runPolicy) {
  if (!Number.isSafeInteger(value) || value < 1 || value > runPolicy.sampleCount) {
    throw new Error('sample index is outside its run-policy range')
  }
  return value
}

function requireSuite(value) {
  if (!BROWSER_SUITES.includes(value)) throw new Error('unknown browser suite ' + value)
  return value
}

function requireBrowser(value) {
  if (!BROWSER_ENGINES.includes(value)) throw new Error('unknown browser engine ' + value)
  return value
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error(label + ' is not valid UTF-8', { cause })
  }
}

function parseOptions(arguments_) {
  const options = new Map()
  for (let index = 0; index < arguments_.length; index += 1) {
    const token = arguments_[index]
    if (token === undefined || !token.startsWith('--') || token.length === 2) {
      throw new Error('expected a browser orchestration option')
    }
    const equals = token.indexOf('=')
    const name = token.slice(2, equals < 0 ? undefined : equals)
    const inlineValue = equals < 0 ? undefined : token.slice(equals + 1)
    const next = arguments_[index + 1]
    const isFlag = inlineValue === undefined && (next === undefined || next.startsWith('--'))
    const value = inlineValue ?? (isFlag ? 'true' : next)
    if (value === undefined || value === '') throw new Error('--' + name + ' requires a value')
    if (inlineValue === undefined && !isFlag) index += 1
    const values = options.get(name) ?? []
    values.push(value)
    options.set(name, values)
  }
  return options
}

function assertOnlyOptions(options, allowed) {
  const allowedSet = new Set(allowed)
  for (const name of options.keys()) {
    if (!allowedSet.has(name)) throw new Error('unknown option --' + name)
  }
}

function requiredOption(options, name) {
  const values = optionValues(options, name)
  if (values.length !== 1 || values[0] === '') {
    throw new Error('--' + name + ' must appear exactly once')
  }
  return values[0]
}

function optionalOption(options, name) {
  const values = optionValues(options, name)
  if (values.length > 1) throw new Error('--' + name + ' may appear at most once')
  return values[0]
}

function flagOption(options, name) {
  const value = optionalOption(options, name)
  if (value === undefined) return false
  if (value !== 'true') throw new Error('--' + name + ' is a flag')
  return true
}

function optionValues(options, name) {
  return options.get(name) ?? []
}

function emit(operationId, milestone, context = {}) {
  const {
    runId,
    operationId: contextualOperationId,
    scenario,
    outcome: reportedOutcome,
    ...payload
  } = context
  if (contextualOperationId !== undefined && contextualOperationId !== operationId) {
    throw new Error('browsergate trace context operation identity differs from its event')
  }
  const event = createBrowsergateTraceEvent({
    ...(runId === undefined ? {} : { runId }),
    operationId,
    ...(scenario === undefined ? {} : { scenario }),
    milestone,
    ...(reportedOutcome === undefined ? {} : { reportedOutcome }),
    payload,
  })
  ORCHESTRATOR_TRACE_CONTEXT.getStore()?.append(event)
}

function errorMessage() {
  // Dependency causes are opaque at lifecycle publication boundaries. Reading
  // message/toString can execute hostile code and suppress the terminal event.
  return 'dependency-operation-failed'
}
