import { spawnSync } from 'node:child_process'
import { lstatSync, readdirSync } from 'node:fs'
import { performance } from 'node:perf_hooks'
import { dirname, isAbsolute, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

export const BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION =
  'windshare.browser-contract.runner/v1'

const COMPONENT = 'browser-contract-runner'
const OPERATION_ID = 'browser-contract'
const TEST_FILE_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*\.tests\.mjs$/u
const DIAGNOSTIC_TOKEN_PATTERN = /^[A-Za-z0-9_-]{1,64}$/u
const MAXIMUM_DIAGNOSTIC_MESSAGE_CHARACTERS = 512
const BROWSERGATE_ROOT = dirname(fileURLToPath(import.meta.url))
const REPOSITORY_ROOT = resolve(BROWSERGATE_ROOT, '..', '..', '..')
const CONTRACT_ROOT = join(BROWSERGATE_ROOT, 'tests', 'contract')
const CHILD_PROCESS_GUARD_URL = pathToFileURL(
  join(BROWSERGATE_ROOT, 'contract-child-process-guard.mjs'),
).href

/**
 * A strict direct-file surface makes the directory itself the registry. Helpers,
 * nested suites, and links must live elsewhere so they cannot be mistaken for a
 * contract that the gate silently skips.
 */
export function discoverContractTests(entries, contractRoot = CONTRACT_ROOT) {
  const root = requireCanonicalAbsolutePath(contractRoot, 'browser contract root')
  if (!Array.isArray(entries)) throw new Error('browser contract discovery must return entries')

  const names = entries.map((entry, index) => {
    if (!isRecord(entry) || typeof entry.name !== 'string' || typeof entry.kind !== 'string') {
      throw new Error(`browser contract discovery entry ${index} is invalid`)
    }
    if (entry.kind !== 'file') {
      throw new Error(`browser contract discovery forbids ${entry.kind} entry ${JSON.stringify(entry.name)}`)
    }
    if (!TEST_FILE_PATTERN.test(entry.name)) {
      throw new Error(`browser contract discovery found unsupported file ${JSON.stringify(entry.name)}`)
    }
    return entry.name
  }).sort()

  if (new Set(names).size !== names.length) {
    throw new Error('browser contract discovery returned duplicate test files')
  }
  return Object.freeze(names.map((name) => Object.freeze({
    testId: `${OPERATION_ID}/${name}`,
    path: join(root, name),
  })))
}

/** Convert Node's deliberately nullable process result into one closed terminal. */
export function classifyContractTestExecution(execution, durationMs) {
  const duration = requireDuration(durationMs, 'browser contract test duration')
  if (!isRecord(execution)) {
    return failedClassification(duration, 'invalid-execution-result', {
      terminal: 'invalid',
    })
  }

  if (execution.error !== undefined && execution.error !== null) {
    const errorCode = safeDiagnosticToken(execution.error.code)
    return failedClassification(
      duration,
      errorCode === 'ETIMEDOUT' ? 'timed-out' : 'start-failed',
      {
        terminal: 'spawn-failed',
        errorCode,
        errorMessage: boundedMessage(execution.error),
      },
    )
  }

  const status = execution.status
  const signal = execution.signal
  if (signal !== undefined && signal !== null) {
    if (
      status !== undefined && status !== null ||
      typeof signal !== 'string' || !DIAGNOSTIC_TOKEN_PATTERN.test(signal)
    ) {
      return failedClassification(duration, 'invalid-execution-result', {
        terminal: 'invalid',
      })
    }
    return failedClassification(duration, 'signaled', {
      terminal: 'signaled',
      signal,
    })
  }

  if (Number.isSafeInteger(status) && status >= 0) {
    const processTerminal = Object.freeze({ terminal: 'exited', exitCode: status })
    return status === 0
      ? Object.freeze({ durationMs: duration, result: 'passed', process: processTerminal })
      : failedClassification(duration, 'nonzero-exit', processTerminal)
  }

  return failedClassification(duration, 'invalid-execution-result', {
    terminal: 'invalid',
  })
}

/**
 * Aggregation is intentionally unaware of the filesystem and process launcher.
 * Its exact-set check prevents missing, duplicate, or foreign results from ever
 * being projected as a successful gate.
 */
export function aggregateContractResults({
  expectedTests,
  results,
  durationMs,
  discoveryFailure = null,
}) {
  const duration = requireDuration(durationMs, 'browser contract run duration')
  if (!Array.isArray(expectedTests) || !Array.isArray(results)) {
    throw new Error('browser contract aggregation requires test and result arrays')
  }

  if (discoveryFailure !== null) {
    return suiteResult({
      durationMs: duration,
      result: 'failed',
      reason: 'discovery-failed',
      counts: resultCounts(0, 0, 0, 0),
      discovery: Object.freeze({
        errorCode: safeDiagnosticToken(discoveryFailure?.code),
        errorMessage: boundedMessage(discoveryFailure),
      }),
    })
  }

  const expectedIds = expectedTests.map((test, index) => requireExpectedTestId(test, index))
  if (new Set(expectedIds).size !== expectedIds.length) {
    throw new Error('browser contract aggregation received duplicate expected test IDs')
  }
  if (expectedIds.length === 0) {
    return suiteResult({
      durationMs: duration,
      result: 'failed',
      reason: 'zero-tests',
      counts: resultCounts(0, results.length, 0, 0),
    })
  }

  const expected = new Set(expectedIds)
  const observed = new Map()
  let exactResultSet = results.length === expectedIds.length
  for (const result of results) {
    if (!consistentTestResult(result) || !expected.has(result.testId) || observed.has(result.testId)) {
      exactResultSet = false
      continue
    }
    observed.set(result.testId, result)
  }
  if (observed.size !== expectedIds.length) exactResultSet = false

  const passed = expectedIds.reduce(
    (count, testId) => count + (observed.get(testId)?.result === 'passed' ? 1 : 0),
    0,
  )
  const failed = expectedIds.length - passed
  const allPassed = exactResultSet && failed === 0
  return suiteResult({
    durationMs: duration,
    result: allPassed ? 'passed' : 'failed',
    ...(allPassed ? {} : { reason: exactResultSet ? 'test-failed' : 'result-set-invalid' }),
    counts: resultCounts(expectedIds.length, results.length, passed, failed),
  })
}

export function runBrowserContractTests({
  readEntries = readContractEntries,
  executeTest = executeContractTest,
  monotonicNow = () => performance.now(),
  write = (encoded) => process.stderr.write(encoded),
} = {}) {
  requireFunction(readEntries, 'browser contract entry reader')
  requireFunction(executeTest, 'browser contract executor')
  requireFunction(monotonicNow, 'browser contract monotonic clock')
  requireFunction(write, 'browser contract record writer')

  const runStartedAt = monotonicNow()
  let tests
  try {
    tests = discoverContractTests(readEntries())
  } catch (cause) {
    const summary = aggregateContractResults({
      expectedTests: [],
      results: [],
      durationMs: elapsedDuration(runStartedAt, monotonicNow()),
      discoveryFailure: cause,
    })
    emitRecord(write, summary)
    return runOutcome([], [], summary)
  }

  const results = []
  for (const test of tests) {
    emitRecord(write, Object.freeze({
      schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
      component: COMPONENT,
      recordType: 'test-started',
      operationId: OPERATION_ID,
      testId: test.testId,
    }))
    const startedAt = monotonicNow()
    let execution
    try {
      execution = executeTest(contractTestCommand(test))
    } catch (cause) {
      execution = Object.freeze({ status: null, signal: null, error: cause })
    }
    const classified = classifyContractTestExecution(
      execution,
      elapsedDuration(startedAt, monotonicNow()),
    )
    const result = Object.freeze({
      schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
      component: COMPONENT,
      recordType: 'test-result',
      operationId: OPERATION_ID,
      testId: test.testId,
      ...classified,
    })
    results.push(result)
    emitRecord(write, result)
  }

  const summary = aggregateContractResults({
    expectedTests: tests,
    results,
    durationMs: elapsedDuration(runStartedAt, monotonicNow()),
  })
  emitRecord(write, summary)
  return runOutcome(tests, results, summary)
}

export function runBrowserContractCli(arguments_, composition) {
  if (!Array.isArray(arguments_) || arguments_.some((argument) => typeof argument !== 'string')) {
    throw new Error('browser contract runner arguments must be strings')
  }
  if (arguments_.length !== 0) throw new Error('browser contract runner accepts no arguments')
  return runBrowserContractTests(composition).exitCode
}

function readContractEntries() {
  const metadata = lstatSync(CONTRACT_ROOT)
  if (metadata.isSymbolicLink() || !metadata.isDirectory()) {
    throw new Error('browser contract root must be one non-symlink directory')
  }
  return readdirSync(CONTRACT_ROOT, { withFileTypes: true }).map((entry) => Object.freeze({
    name: entry.name,
    kind: directoryEntryKind(entry),
  }))
}

function directoryEntryKind(entry) {
  if (entry.isFile()) return 'file'
  if (entry.isDirectory()) return 'directory'
  if (entry.isSymbolicLink()) return 'symbolic-link'
  return 'other'
}

function contractTestCommand(test) {
  return Object.freeze({
    executable: process.execPath,
    arguments: Object.freeze(['--import', CHILD_PROCESS_GUARD_URL, test.path]),
    options: Object.freeze({
      cwd: REPOSITORY_ROOT,
      shell: false,
      stdio: 'inherit',
      windowsHide: true,
    }),
  })
}

function executeContractTest(command) {
  return spawnSync(command.executable, [...command.arguments], command.options)
}

function failedClassification(durationMs, failureKind, processTerminal) {
  return Object.freeze({
    durationMs,
    result: 'failed',
    failureKind,
    process: Object.freeze(processTerminal),
  })
}

function consistentTestResult(result) {
  if (
    !isRecord(result) ||
    result.schemaVersion !== BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION ||
    result.component !== COMPONENT ||
    result.recordType !== 'test-result' ||
    result.operationId !== OPERATION_ID ||
    typeof result.testId !== 'string' ||
    !Number.isSafeInteger(result.durationMs) || result.durationMs < 0 ||
    !isRecord(result.process)
  ) return false

  const terminal = result.process.terminal
  const successfulTerminal = terminal === 'exited' && result.process.exitCode === 0
  if (result.result === 'passed') return successfulTerminal && result.failureKind === undefined
  if (result.result !== 'failed' || typeof result.failureKind !== 'string') return false
  return !successfulTerminal
}

function suiteResult({ durationMs, result, reason, counts, discovery }) {
  return Object.freeze({
    schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
    component: COMPONENT,
    recordType: 'suite-result',
    operationId: OPERATION_ID,
    durationMs,
    result,
    ...(reason === undefined ? {} : { reason }),
    counts,
    ...(discovery === undefined ? {} : { discovery }),
  })
}

function resultCounts(discovered, settled, passed, failed) {
  return Object.freeze({ discovered, settled, passed, failed })
}

function runOutcome(tests, results, summary) {
  return Object.freeze({
    exitCode: summary.result === 'passed' ? 0 : 1,
    tests: Object.freeze([...tests]),
    results: Object.freeze([...results]),
    summary,
  })
}

function emitRecord(write, record) {
  write(`${JSON.stringify(record)}\n`)
}

function elapsedDuration(startedAt, settledAt) {
  if (!Number.isFinite(startedAt) || !Number.isFinite(settledAt) || settledAt < startedAt) {
    throw new Error('browser contract monotonic clock regressed or returned a non-finite value')
  }
  return requireDuration(Math.floor(settledAt - startedAt), 'browser contract elapsed duration')
}

function requireDuration(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`${label} must be a nonnegative integer`)
  return value
}

function requireExpectedTestId(test, index) {
  if (!isRecord(test) || typeof test.testId !== 'string' || !test.testId.startsWith(`${OPERATION_ID}/`)) {
    throw new Error(`browser contract expected test ${index} has an invalid ID`)
  }
  return test.testId
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function safeDiagnosticToken(value) {
  return typeof value === 'string' && DIAGNOSTIC_TOKEN_PATTERN.test(value) ? value : 'UNKNOWN'
}

function boundedMessage(cause) {
  const message = cause instanceof Error ? cause.message : String(cause)
  return message.slice(0, MAXIMUM_DIAGNOSTIC_MESSAGE_CHARACTERS)
}

function requireFunction(value, label) {
  if (typeof value !== 'function') throw new Error(`${label} must be a function`)
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function unexpectedRunnerFailure(cause) {
  return suiteResult({
    durationMs: 0,
    result: 'failed',
    reason: 'runner-failed',
    counts: resultCounts(0, 0, 0, 0),
    discovery: Object.freeze({
      errorCode: safeDiagnosticToken(cause?.code),
      errorMessage: boundedMessage(cause),
    }),
  })
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    process.exitCode = runBrowserContractCli(process.argv.slice(2))
  } catch (cause) {
    emitRecord((encoded) => process.stderr.write(encoded), unexpectedRunnerFailure(cause))
    process.exitCode = 1
  }
}
