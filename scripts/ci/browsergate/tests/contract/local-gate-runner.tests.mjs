import assert from 'node:assert/strict'

import { evaluateBrowserGate } from '../../verdict.mjs'
import { runLocalBrowserGatePipeline } from '../../local-gate-runner.mjs'

await verifyCanonicalTraceAndOpaquePayloads()
await verifyContractFailureSkipsSuiteWorkAndStillRunsVerdict()
await verifyEveryPrerequisiteFailureProjectsHostedStatus()
await verifyRuntimeRetirementPrecedesVerdictAndFailsBothSuites()
await verifyHostileFailureCannotBlockTraceSettlement()

process.stdout.write('local browser gate DI/trace contracts: PASS\n')

async function verifyCanonicalTraceAndOpaquePayloads() {
  const harness = createHarness()
  const result = await runLocalBrowserGatePipeline(harness.ports)

  assert.equal(result.exitCode, 0)
  assert.deepEqual(harness.calls, [
    'browser-contract',
    'generated-semantic-process',
    'browser-runtime-build',
    'browser-install',
    'browser-preflight',
    'main-topology-lock',
    'main-production',
    'pion-topology-lock',
    'pion-production',
    'main-guard',
    'pion-guard',
    'browser-runtime-retirement',
    'browser-verdict',
  ])
  assert.deepEqual(
    result.traces.events.map(({ operationId, outcome }) => [operationId, outcome]),
    [
      ['browser-contract', 'success'],
      ['generated-semantic-process', 'success'],
      ['browser-runtime-build', 'success'],
      ['browser-install', 'success'],
      ['browser-preflight', 'success'],
      ['main-topology-lock', 'success'],
      ['main-production', 'success'],
      ['pion-topology-lock', 'success'],
      ['pion-production', 'success'],
      ['main-guard', 'success'],
      ['main-sealed-evidence', 'success'],
      ['pion-guard', 'success'],
      ['pion-sealed-evidence', 'success'],
      ['browser-runtime-retirement', 'success'],
      ['browser-verdict', 'success'],
    ],
  )
  assert.equal(result.traces.completed, true)
  assert.equal(result.traces.failure, null)
  assert.equal(result.traces.observedEvents, result.traces.capturedEvents)
  assert.equal(result.traces.observedBytes, result.traces.capturedBytes)
  assert.equal(result.projection.contractJobOutcome, 'success')
  assert.equal(harness.retirementCount, 1)
  for (const suite of ['main', 'pion']) {
    assert.strictEqual(result.suiteOutcomes[suite], harness.productOutcomes[suite])
    assert.strictEqual(result.guardOutcomes[suite], harness.guardOutcomes[suite])
    assert.strictEqual(harness.verdictInput.suiteOutcomes[suite], harness.productOutcomes[suite])
    assert.strictEqual(harness.verdictInput.guardOutcomes[suite], harness.guardOutcomes[suite])
  }
}

async function verifyContractFailureSkipsSuiteWorkAndStillRunsVerdict() {
  const harness = createHarness({ fault: 'browser-contract', useStandardLibraryVerdict: true })
  const result = await runLocalBrowserGatePipeline(harness.ports)

  assert.deepEqual(harness.calls, [
    'browser-contract',
    'browser-verdict',
  ])
  assert.equal(result.projection.contractJobOutcome, 'failure')
  assert.deepEqual(result.projection.verdictDependencies, {
    main: 'skipped',
    pion: 'skipped',
  })
  assert.equal(result.exitCode, 1)
  assert.equal(harness.standardLibraryVerdict.verdict, 'failed')
  assert.deepEqual(
    result.traces.events.filter(({ outcome }) => outcome === 'skipped').map(({ operationId }) => operationId),
    [
      'generated-semantic-process',
      'browser-runtime-build',
      'browser-install',
      'browser-preflight',
      'main-topology-lock',
      'main-production',
      'pion-topology-lock',
      'pion-production',
      'main-guard',
      'main-sealed-evidence',
      'pion-guard',
      'pion-sealed-evidence',
      'browser-runtime-retirement',
    ],
  )
}

async function verifyEveryPrerequisiteFailureProjectsHostedStatus() {
  const cases = [
    ['browser-contract', 'failure', 'skipped', 'skipped', 'skipped'],
    ['generated-semantic-process', 'success', 'failure', 'failure', 'failure'],
    ['browser-runtime-build', 'success', 'success', 'failure', 'failure'],
    ['browser-install', 'success', 'success', 'failure', 'failure'],
    ['browser-preflight', 'success', 'success', 'failure', 'failure'],
    ['main-topology-lock', 'success', 'success', 'failure', 'success'],
    ['main-production', 'success', 'success', 'failure', 'success'],
    ['main-guard', 'success', 'success', 'failure', 'success'],
    ['main-sealed-evidence', 'success', 'success', 'failure', 'success'],
    ['pion-topology-lock', 'success', 'success', 'success', 'failure'],
    ['pion-production', 'success', 'success', 'success', 'failure'],
    ['pion-guard', 'success', 'success', 'success', 'failure'],
    ['pion-sealed-evidence', 'success', 'success', 'success', 'failure'],
    ['browser-runtime-retirement', 'success', 'success', 'failure', 'failure'],
  ]

  for (const [fault, contract, processJob, main, pion] of cases) {
    const harness = createHarness({ fault, useStandardLibraryVerdict: true })
    const result = await runLocalBrowserGatePipeline(harness.ports)
    assert.equal(result.projection.contractJobOutcome, contract, fault)
    assert.equal(result.projection.processJobOutcome, processJob, fault)
    assert.deepEqual(result.projection.verdictDependencies, { main, pion }, fault)
    assert.equal(result.exitCode, 1, fault)
    assert.equal(harness.standardLibraryVerdict.verdict, 'failed', fault)
    assert.equal(harness.calls.at(-1), 'browser-verdict', fault)
  }
}

async function verifyRuntimeRetirementPrecedesVerdictAndFailsBothSuites() {
  const harness = createHarness({
    fault: 'browser-runtime-retirement',
    useStandardLibraryVerdict: true,
  })
  const result = await runLocalBrowserGatePipeline(harness.ports)

  assert.equal(harness.retirementCount, 1)
  assert(
    harness.calls.indexOf('browser-runtime-retirement') <
      harness.calls.indexOf('browser-verdict'),
  )
  assert.deepEqual(result.projection.verdictDependencies, {
    main: 'failure',
    pion: 'failure',
  })
  for (const suite of ['main', 'pion']) {
    assert.strictEqual(result.suiteOutcomes[suite], harness.productOutcomes[suite])
    assert.strictEqual(result.guardOutcomes[suite], harness.guardOutcomes[suite])
  }
}

async function verifyHostileFailureCannotBlockTraceSettlement() {
  let messageReads = 0
  const hostileCause = new Error('opaque')
  Object.defineProperty(hostileCause, 'message', {
    get() {
      messageReads += 1
      throw new Error('message accessor entered')
    },
  })
  const harness = createHarness({
    fault: 'browser-contract',
    faultCause: hostileCause,
    useStandardLibraryVerdict: true,
  })
  const result = await runLocalBrowserGatePipeline(harness.ports)
  assert.equal(result.exitCode, 1)
  assert.equal(result.traces.completed, true)
  assert.equal(result.traces.truncated, false)
  assert.equal(result.traces.failure, null)
  assert.equal(messageReads, 0)
  assert.equal(harness.calls.at(-1), 'browser-verdict')
}

function createHarness({
  fault = null,
  faultMessage = null,
  faultCause = null,
  useStandardLibraryVerdict = false,
} = {}) {
  const calls = []
  const runtime = Object.freeze({ authorityId: 'local-runtime-authority' })
  const productOutcomes = Object.freeze(Object.fromEntries(['main', 'pion'].map((suite) => [
    suite,
    Object.freeze({
      exitCode: 0,
      settlementTrust: Object.freeze({ invocationId: `${suite}-settlement` }),
      samplePayload: Object.freeze({ suite, bytes: `${suite}-sample-bytes` }),
    }),
  ])))
  const guardOutcomes = Object.freeze(Object.fromEntries(['main', 'pion'].map((suite) => [
    suite,
    Object.freeze({
      exitCode: 0,
      guardOutcome: 'passed',
      uploadDirectory: `C:\\sealed\\${suite}`,
      manifestSha256: suite === 'main' ? 'a'.repeat(64) : 'b'.repeat(64),
      manifestByteLength: '1',
      sampleOutcomes: Object.freeze([productOutcomes[suite].samplePayload]),
    }),
  ])))
  let verdictInput = null
  let standardLibraryVerdict = null
  let retirementCount = 0

  function enter(operationId) {
    calls.push(operationId)
    if (fault === operationId && !operationId.endsWith('-sealed-evidence')) {
      throw faultCause ?? new Error(faultMessage ?? `injected ${operationId} failure`)
    }
  }

  const ports = {
    async runContract() {
      enter('browser-contract')
      return Object.freeze({ exitCode: 0 })
    },
    async runGeneratedSemanticProcess() {
      enter('generated-semantic-process')
      return Object.freeze({ exitCode: 0 })
    },
    async buildRuntime() {
      enter('browser-runtime-build')
      return runtime
    },
    async installBrowserRuntime() {
      enter('browser-install')
      return Object.freeze({ exitCode: 0 })
    },
    async runPreflight() {
      enter('browser-preflight')
      return Object.freeze({ exitCode: 0 })
    },
    async prepareTopology({ runtime: actualRuntime, suite }) {
      assert.strictEqual(actualRuntime, runtime)
      enter(`${suite}-topology-lock`)
    },
    async runProduct({ runtime: actualRuntime, suite }) {
      assert.strictEqual(actualRuntime, runtime)
      enter(`${suite}-production`)
      return productOutcomes[suite]
    },
    async runGuard({ runtime: actualRuntime, suite, suiteOutcome }) {
      enter(`${suite}-guard`)
      if (actualRuntime === null) throw new Error('runtime authority unavailable')
      assert.strictEqual(actualRuntime, runtime)
      assert.strictEqual(suiteOutcome, productOutcomes[suite])
      if (fault === `${suite}-sealed-evidence`) {
        return Object.freeze({ ...guardOutcomes[suite], uploadDirectory: null })
      }
      return guardOutcomes[suite]
    },
    async retireRuntime({ runtime: actualRuntime }) {
      assert.strictEqual(actualRuntime, runtime)
      retirementCount += 1
      enter('browser-runtime-retirement')
    },
    async runVerdict(input) {
      enter('browser-verdict')
      verdictInput = input
      if (useStandardLibraryVerdict) {
        standardLibraryVerdict = await evaluateBrowserGate({
          runId: 'local-parity-test',
          checkoutSha: 'c'.repeat(40),
          suites: Object.fromEntries(['main', 'pion'].map((suite) => [suite, {
            root: input.guardOutcomes[suite].uploadDirectory ?? `C:\\missing\\${suite}`,
            jobOutcome: input.projection.verdictDependencies[suite],
            guardOutcome: input.guardOutcomes[suite].guardOutcome,
            downloadOutcome: input.guardOutcomes[suite].uploadDirectory === null
              ? 'failure'
              : 'success',
            manifestSha256: input.guardOutcomes[suite].manifestSha256 ?? '',
            manifestByteLength: input.guardOutcomes[suite].manifestByteLength ?? '',
          }])),
        })
        return Object.freeze({ exitCode: standardLibraryVerdict.verdict === 'passed' ? 0 : 1 })
      }
      return Object.freeze({ exitCode: 0 })
    },
  }

  return {
    calls,
    ports,
    productOutcomes,
    guardOutcomes,
    get verdictInput() { return verdictInput },
    get standardLibraryVerdict() { return standardLibraryVerdict },
    get retirementCount() { return retirementCount },
  }
}
