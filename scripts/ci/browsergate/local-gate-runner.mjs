import { projectLocalHostedJobOutcomes } from './local-hosted-outcome-projection.mjs'
import {
  createOwnedTraceJournal,
  requireCompleteOwnedTraceSnapshot,
} from './owned-trace-journal.mjs'

const SUITES = Object.freeze(['main', 'pion'])
const LOCAL_GATE_MAXIMUM_TRACE_EVENTS = 128
const LOCAL_GATE_MAXIMUM_TRACE_BYTES = 256 * 1024

export function localGateOperationPlan({
  mainProductOperations,
  pionProductOperations,
}) {
  requireOperationNames(mainProductOperations, 'main product operations')
  requireOperationNames(pionProductOperations, 'Pion product operations')
  return Object.freeze([
    'browser-contract',
    'generated-semantic-process',
    'browser-runtime-build',
    'browser-install',
    'browser-preflight',
    'main-topology-lock',
    ...mainProductOperations,
    'pion-topology-lock',
    ...pionProductOperations,
    'main-suite-guard-and-seal',
    'pion-suite-guard-and-seal',
    'browser-runtime-retirement',
    'standard-library-verdict',
  ])
}

/**
 * Execute the local browser gate through the same dependency edges exposed by
 * the hosted `needs` graph. The injected operations keep orchestration tests
 * independent from browsers and native process owners while production retains
 * one invocation-scoped runtime authority.
 */
export async function runLocalBrowserGatePipeline(ports) {
  const journal = createOwnedTraceJournal({
    label: 'local browser gate lifecycle trace',
    maximumEvents: LOCAL_GATE_MAXIMUM_TRACE_EVENTS,
    maximumBytes: LOCAL_GATE_MAXIMUM_TRACE_BYTES,
  })
  let result
  let primaryFailure
  try {
    result = await runLocalBrowserGatePipelineOperation(ports, journal.append)
  } catch (cause) {
    primaryFailure = cause
  } finally {
    journal.finish()
  }
  const traces = journal.view.snapshot()
  let traceFailure
  try {
    requireCompleteOwnedTraceSnapshot(traces, 'local browser gate lifecycle trace')
  } catch (cause) {
    traceFailure = cause
  }
  if (primaryFailure !== undefined) {
    throw new LocalBrowserGatePipelineError(primaryFailure, traces, traceFailure)
  }
  if (result === undefined) throw new Error('local browser gate pipeline returned no result')
  return Object.freeze({
    ...result,
    exitCode: traceFailure === undefined ? result.exitCode : 1,
    traces,
  })
}

export class LocalBrowserGatePipelineError extends Error {
  constructor(primaryFailure, traces, traceFailure) {
    super('local browser gate pipeline failed', {
      cause: traceFailure === undefined
        ? primaryFailure
        : new AggregateError([primaryFailure, traceFailure], 'pipeline and trace settlement failed'),
    })
    this.name = 'LocalBrowserGatePipelineError'
    this.traces = traces
  }
}

async function runLocalBrowserGatePipelineOperation({
  runContract,
  runGeneratedSemanticProcess,
  buildRuntime,
  installBrowserRuntime,
  runPreflight,
  prepareTopology,
  runProduct,
  runGuard,
  retireRuntime,
  runVerdict,
}, trace) {
  for (const [name, operation] of Object.entries({
    runContract,
    runGeneratedSemanticProcess,
    buildRuntime,
    installBrowserRuntime,
    runPreflight,
    prepareTopology,
    runProduct,
    runGuard,
    retireRuntime,
    runVerdict,
    trace,
  })) requireFunction(operation, name)

  // Make has already completed the shared web-dependencies leaf. Recording that
  // edge as successful preserves hosted/local projection parity without giving
  // Browsergate a second dependency-acquisition authority.
  const projectionInput = initialProjectionInput()
  const suiteOutcomes = Object.fromEntries(SUITES.map((suite) => [
    suite,
    failedProductOutcome(),
  ]))
  const guardOutcomes = Object.fromEntries(SUITES.map((suite) => [
    suite,
    missingGuardOutcome(),
  ]))
  let runtime = null

  projectionInput.contract.browserContract = await exitOperation({
    operationId: 'browser-contract',
    operation: runContract,
    trace,
  })

  const contractSucceeded = projectionInput.contract.browserContract === 'success'
  if (contractSucceeded) {
    projectionInput.process.generatedSemantic = await exitOperation({
      operationId: 'generated-semantic-process',
      operation: runGeneratedSemanticProcess,
      trace,
    })
  } else {
    skippedTrace(trace, 'generated-semantic-process', 'browser-contract')
  }

  const generatedSemanticSucceeded =
    projectionInput.process.generatedSemantic === 'success'
  if (contractSucceeded && generatedSemanticSucceeded) {
    const built = await valueOperation({
      operationId: 'browser-runtime-build',
      operation: buildRuntime,
      trace,
    })
    projectionInput.suiteShared.runtimeBuild = built.outcome
    runtime = built.value
  } else {
    skippedTrace(
      trace,
      'browser-runtime-build',
      contractSucceeded ? 'generated-semantic-process' : 'browser-contract',
    )
  }

  if (projectionInput.suiteShared.runtimeBuild === 'success') {
    projectionInput.suiteShared.browserInstall = await exitOperation({
      operationId: 'browser-install',
      operation: installBrowserRuntime,
      trace,
    })
  } else {
    skippedTrace(trace, 'browser-install', 'browser-runtime-build')
  }

  if (projectionInput.suiteShared.browserInstall === 'success') {
    projectionInput.suiteShared.preflight = await exitOperation({
      operationId: 'browser-preflight',
      operation: runPreflight,
      trace,
    })
  } else {
    skippedTrace(trace, 'browser-preflight', 'browser-install')
  }

  const suiteWorkAuthorized = projectionInput.suiteShared.preflight === 'success'
  for (const suite of SUITES) {
    const topologyOperationId = `${suite}-topology-lock`
    const productOperationId = `${suite}-production`
    if (!suiteWorkAuthorized) {
      skippedTrace(trace, topologyOperationId, 'browser-preflight')
      skippedTrace(trace, productOperationId, topologyOperationId)
      continue
    }
    projectionInput.suites[suite].topology = await voidOperation({
      operationId: topologyOperationId,
      operation: () => prepareTopology({ runtime, suite }),
      trace,
    })
    if (projectionInput.suites[suite].topology !== 'success') {
      skippedTrace(trace, productOperationId, topologyOperationId)
      continue
    }
    const product = await valueOperation({
      operationId: productOperationId,
      operation: () => runProduct({ runtime, suite }),
      trace,
      validate: requireProductOutcome,
    })
    projectionInput.suites[suite].product = product.outcome
    if (product.value !== null) suiteOutcomes[suite] = product.value
  }

  if (contractSucceeded) {
    for (const suite of SUITES) {
      const guarded = await valueOperation({
        operationId: `${suite}-guard`,
        operation: () => runGuard({
          runtime,
          suite,
          suiteOutcome: suiteOutcomes[suite],
        }),
        trace,
        validate: requireGuardOutcome,
      })
      projectionInput.suites[suite].guard = guarded.outcome
      if (guarded.value !== null) guardOutcomes[suite] = guarded.value
      const sealedEvidence = hasSealedEvidence(guardOutcomes[suite])
        ? 'success'
        : 'failure'
      projectionInput.suites[suite].sealedEvidence = sealedEvidence
      terminalTrace(trace, `${suite}-sealed-evidence`, sealedEvidence)
    }
  } else {
    for (const suite of SUITES) {
      skippedTrace(trace, `${suite}-guard`, 'browser-contract')
      skippedTrace(trace, `${suite}-sealed-evidence`, `${suite}-guard`)
    }
  }

  if (runtime !== null) {
    projectionInput.suiteShared.runtimeRetirement = await voidOperation({
      operationId: 'browser-runtime-retirement',
      operation: () => retireRuntime({ runtime }),
      trace,
    })
  } else {
    skippedTrace(trace, 'browser-runtime-retirement', 'browser-runtime-build')
  }

  const projection = projectLocalHostedJobOutcomes(projectionInput)
  const verdictExecution = await valueOperation({
    operationId: 'browser-verdict',
    operation: () => runVerdict({
      projection,
      suiteOutcomes,
      guardOutcomes,
    }),
    trace,
    validate: requireVerdictExecution,
  })
  const verdictExitCode = verdictExecution.value?.exitCode ?? 1
  const dependenciesPassed = Object.values(projection.verdictDependencies)
    .every((outcome) => outcome === 'success') &&
    projection.processJobOutcome === 'success'

  return Object.freeze({
    exitCode: verdictExitCode === 0 && dependenciesPassed ? 0 : 1,
    projection,
    suiteOutcomes: Object.freeze(suiteOutcomes),
    guardOutcomes: Object.freeze(guardOutcomes),
    verdictExecution: verdictExecution.value,
  })
}

function initialProjectionInput() {
  return {
    contract: {
      dependencyInstall: 'success',
      browserContract: 'skipped',
    },
    process: {
      generatedSemantic: 'skipped',
    },
    suiteShared: {
      runtimeBuild: 'skipped',
      browserInstall: 'skipped',
      preflight: 'skipped',
      runtimeRetirement: 'skipped',
    },
    suites: Object.fromEntries(SUITES.map((suite) => [suite, {
      topology: 'skipped',
      product: 'skipped',
      guard: 'skipped',
      sealedEvidence: 'skipped',
    }])),
  }
}

async function exitOperation({ operationId, operation, trace }) {
  const result = await valueOperation({
    operationId,
    operation,
    trace,
    validate: requireExitCode,
  })
  return result.outcome
}

async function voidOperation({ operationId, operation, trace }) {
  try {
    await operation()
    terminalTrace(trace, operationId, 'success')
    return 'success'
  } catch {
    terminalTrace(trace, operationId, 'failure', { failureCode: 'operation-rejected' })
    return 'failure'
  }
}

async function valueOperation({
  operationId,
  operation,
  trace,
  validate = requireOwnedValue,
}) {
  try {
    const value = await operation()
    validate(value, operationId)
    const outcome = 'exitCode' in Object(value) && value.exitCode !== 0 ? 'failure' : 'success'
    terminalTrace(trace, operationId, outcome, outcome === 'failure'
      ? { exitCode: value.exitCode }
      : {})
    return Object.freeze({ outcome, value })
  } catch {
    terminalTrace(trace, operationId, 'failure', { failureCode: 'operation-rejected' })
    return Object.freeze({ outcome: 'failure', value: null })
  }
}

function requireExitCode(value, label) {
  if (!Number.isSafeInteger(value?.exitCode) || value.exitCode < 0) {
    throw new Error(`${label} did not return a nonnegative exit code`)
  }
}

function requireProductOutcome(value, label) {
  requireExitCode(value, label)
}

function requireGuardOutcome(value, label) {
  requireExitCode(value, label)
  if (typeof value.guardOutcome !== 'string') {
    throw new Error(`${label} did not return a guard outcome`)
  }
  if (value.uploadDirectory !== null && typeof value.uploadDirectory !== 'string') {
    throw new Error(`${label} returned an invalid sealed evidence path`)
  }
}

function requireVerdictExecution(value, label) {
  requireExitCode(value, label)
}

function requireOwnedValue(value, label) {
  if (typeof value !== 'object' || value === null) {
    throw new Error(`${label} did not return its owned value`)
  }
}

function hasSealedEvidence(outcome) {
  return outcome.exitCode === 0 && outcome.guardOutcome === 'passed' &&
    typeof outcome.uploadDirectory === 'string' && outcome.uploadDirectory !== ''
}

function failedProductOutcome() {
  return Object.freeze({ exitCode: 1, settlementTrust: null })
}

function missingGuardOutcome() {
  return Object.freeze({
    exitCode: 1,
    guardOutcome: '',
    uploadDirectory: null,
    manifestSha256: null,
    manifestByteLength: null,
    sampleOutcomes: Object.freeze([]),
  })
}

function skippedTrace(trace, operationId, dependency) {
  terminalTrace(trace, operationId, 'skipped', { dependency })
}

function terminalTrace(trace, operationId, outcome, context = {}) {
  trace(Object.freeze({ operationId, outcome, ...context }))
}

function requireFunction(value, label) {
  if (typeof value !== 'function') throw new Error(`${label} must be a function`)
}

function requireOperationNames(value, label) {
  if (!Array.isArray(value) || value.length === 0 || value.some((name) =>
    typeof name !== 'string' || name === '')) throw new Error(`${label} must be nonempty names`)
}
