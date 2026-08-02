import assert from 'node:assert/strict'
import { join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  aggregateContractResults,
  BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
  classifyContractTestExecution,
  discoverContractTests,
  runBrowserContractCli,
  runBrowserContractTests,
} from '../../test-runner.mjs'

const guardState = globalThis[Symbol.for('windshare.browser-contract.child-process-guard')]
assert.notEqual(guardState, undefined, 'browser contract runner tests require the child-process guard')

const BROWSERGATE_ROOT = resolve(import.meta.dirname, '..', '..')
const REPOSITORY_ROOT = resolve(BROWSERGATE_ROOT, '..', '..', '..')
const CONTRACT_ROOT = join(BROWSERGATE_ROOT, 'tests', 'contract')
const GUARD_URL = pathToFileURL(join(BROWSERGATE_ROOT, 'contract-child-process-guard.mjs')).href

verifyStrictStableDiscovery()
verifyZeroTestsFailClosed()
verifyDiscoveryFailureFailsClosed()
verifyStableCommandsDurationsAndRecords()
verifyEveryProcessFailureFailsClosedAndExecutionContinues()
verifyMalformedProcessResultsFailClosed()
verifyAggregationRequiresAnExactResultSet()
verifyCliRejectsArgumentsBeforeDiscovery()
verifyHostileCausesCannotSuppressSettlement()

assert.deepEqual(guardState.invocations, [])
process.stdout.write('browser contract test runner contracts: PASS\n')

function verifyStrictStableDiscovery() {
  const discovered = discoverContractTests([
    fileEntry('zeta.tests.mjs'),
    fileEntry('alpha.tests.mjs'),
  ], CONTRACT_ROOT)
  assert.deepEqual(discovered, [
    {
      testId: 'browser-contract/alpha.tests.mjs',
      path: join(CONTRACT_ROOT, 'alpha.tests.mjs'),
    },
    {
      testId: 'browser-contract/zeta.tests.mjs',
      path: join(CONTRACT_ROOT, 'zeta.tests.mjs'),
    },
  ])
  assert(Object.isFrozen(discovered))
  assert(discovered.every(Object.isFrozen))
  assert(discovered.every(({ testId }) => !testId.includes('\\')))

  assert.throws(
    () => discoverContractTests([fileEntry('assertions.mjs')], CONTRACT_ROOT),
    /unsupported file/u,
  )
  assert.throws(
    () => discoverContractTests([
      { name: 'linked.tests.mjs', kind: 'symbolic-link' },
    ], CONTRACT_ROOT),
    /forbids symbolic-link/u,
  )
  assert.throws(
    () => discoverContractTests([
      fileEntry('duplicate.tests.mjs'),
      fileEntry('duplicate.tests.mjs'),
    ], CONTRACT_ROOT),
    /duplicate test files/u,
  )
}

function verifyZeroTestsFailClosed() {
  const writes = []
  const execution = runBrowserContractTests({
    readEntries: () => [],
    executeTest: forbiddenExecutor,
    monotonicNow: sequenceClock([10, 14]),
    write: (encoded) => writes.push(encoded),
  })

  assert.equal(execution.exitCode, 1)
  assert.deepEqual(execution.results, [])
  assert.deepEqual(execution.summary, {
    schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
    component: 'browser-contract-runner',
    recordType: 'suite-result',
    operationId: 'browser-contract',
    durationMs: 4,
    result: 'failed',
    reason: 'zero-tests',
    counts: { discovered: 0, settled: 0, passed: 0, failed: 0 },
  })
  assert.deepEqual(decodeRecords(writes), [execution.summary])
}

function verifyDiscoveryFailureFailsClosed() {
  const writes = []
  let executeCalls = 0
  const discoveryError = Object.assign(new Error('injected discovery denial'), { code: 'EACCES' })
  const execution = runBrowserContractTests({
    readEntries() { throw discoveryError },
    executeTest() {
      executeCalls += 1
      return successfulExecution()
    },
    monotonicNow: sequenceClock([20, 27]),
    write: (encoded) => writes.push(encoded),
  })

  assert.equal(executeCalls, 0)
  assert.equal(execution.exitCode, 1)
  assert.equal(execution.summary.reason, 'discovery-failed')
  assert.equal(execution.summary.durationMs, 7)
  assert.deepEqual(execution.summary.discovery, {
    errorCode: 'EACCES',
    errorMessage: 'browser contract test discovery failed',
  })
  assert.deepEqual(decodeRecords(writes), [execution.summary])
}

function verifyStableCommandsDurationsAndRecords() {
  const commands = []
  const writes = []
  const execution = runBrowserContractTests({
    readEntries: () => [fileEntry('zeta.tests.mjs'), fileEntry('alpha.tests.mjs')],
    executeTest(command) {
      commands.push(command)
      return successfulExecution()
    },
    monotonicNow: sequenceClock([100, 110, 113, 120, 127, 130]),
    write: (encoded) => writes.push(encoded),
  })

  assert.equal(execution.exitCode, 0)
  assert.deepEqual(execution.results.map(({ testId, durationMs, result }) => ({
    testId,
    durationMs,
    result,
  })), [
    { testId: 'browser-contract/alpha.tests.mjs', durationMs: 3, result: 'passed' },
    { testId: 'browser-contract/zeta.tests.mjs', durationMs: 7, result: 'passed' },
  ])
  assert.deepEqual(commands.map((command) => command.arguments.at(-1)), [
    join(CONTRACT_ROOT, 'alpha.tests.mjs'),
    join(CONTRACT_ROOT, 'zeta.tests.mjs'),
  ])
  for (const command of commands) {
    assert.equal(command.executable, process.execPath)
    assert.deepEqual(command.arguments.slice(0, 2), ['--import', GUARD_URL])
    assert.deepEqual(command.options, {
      cwd: REPOSITORY_ROOT,
      shell: false,
      stdio: 'inherit',
      windowsHide: true,
    })
    assert(Object.isFrozen(command))
    assert(Object.isFrozen(command.arguments))
    assert(Object.isFrozen(command.options))
  }
  assert.deepEqual(execution.summary, {
    schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
    component: 'browser-contract-runner',
    recordType: 'suite-result',
    operationId: 'browser-contract',
    durationMs: 30,
    result: 'passed',
    counts: { discovered: 2, settled: 2, passed: 2, failed: 0 },
  })

  const records = decodeRecords(writes)
  assert.equal(records.length, 5)
  assert.deepEqual(records.map(({ recordType, testId, result }) => ({
    recordType,
    testId: testId ?? null,
    result: result ?? null,
  })), [
    { recordType: 'test-started', testId: 'browser-contract/alpha.tests.mjs', result: null },
    { recordType: 'test-result', testId: 'browser-contract/alpha.tests.mjs', result: 'passed' },
    { recordType: 'test-started', testId: 'browser-contract/zeta.tests.mjs', result: null },
    { recordType: 'test-result', testId: 'browser-contract/zeta.tests.mjs', result: 'passed' },
    { recordType: 'suite-result', testId: null, result: 'passed' },
  ])
}

function verifyEveryProcessFailureFailsClosedAndExecutionContinues() {
  const entries = [
    'a-start-throw.tests.mjs',
    'b-start-result.tests.mjs',
    'c-signal.tests.mjs',
    'd-nonzero.tests.mjs',
    'e-success.tests.mjs',
  ].map(fileEntry)
  const calls = []
  const execution = runBrowserContractTests({
    readEntries: () => entries,
    executeTest(command) {
      const name = command.arguments.at(-1).slice(CONTRACT_ROOT.length + 1)
      calls.push(name)
      if (name.startsWith('a-')) {
        throw Object.assign(new Error('injected launch throw'), { code: 'ENOENT' })
      }
      if (name.startsWith('b-')) {
        return {
          status: null,
          signal: null,
          error: Object.assign(new Error('injected launch result'), { code: 'EACCES' }),
        }
      }
      if (name.startsWith('c-')) return { status: null, signal: 'SIGTERM' }
      if (name.startsWith('d-')) return { status: 9, signal: null }
      return successfulExecution()
    },
    monotonicNow: incrementingClock(),
    write: () => undefined,
  })

  assert.deepEqual(calls, entries.map(({ name }) => name))
  assert.equal(execution.exitCode, 1)
  assert.deepEqual(execution.results.map(({ result, failureKind, process }) => ({
    result,
    failureKind: failureKind ?? null,
    process,
  })), [
    {
      result: 'failed',
      failureKind: 'start-failed',
      process: {
        terminal: 'spawn-failed',
        errorCode: 'ENOENT',
        errorMessage: 'browser contract test process launch failed',
      },
    },
    {
      result: 'failed',
      failureKind: 'start-failed',
      process: {
        terminal: 'spawn-failed',
        errorCode: 'EACCES',
        errorMessage: 'browser contract test process launch failed',
      },
    },
    {
      result: 'failed',
      failureKind: 'signaled',
      process: { terminal: 'signaled', signal: 'SIGTERM' },
    },
    {
      result: 'failed',
      failureKind: 'nonzero-exit',
      process: { terminal: 'exited', exitCode: 9 },
    },
    {
      result: 'passed',
      failureKind: null,
      process: { terminal: 'exited', exitCode: 0 },
    },
  ])
  assert.equal(execution.summary.reason, 'test-failed')
  assert.deepEqual(execution.summary.counts, {
    discovered: 5,
    settled: 5,
    passed: 1,
    failed: 4,
  })
}

function verifyMalformedProcessResultsFailClosed() {
  for (const execution of [
    null,
    {},
    { status: null, signal: null },
    { status: 0, signal: 'SIGTERM' },
    { status: -1, signal: null },
    { status: 0.5, signal: null },
    { status: null, signal: 'not a signal' },
  ]) {
    assert.deepEqual(classifyContractTestExecution(execution, 2), {
      durationMs: 2,
      result: 'failed',
      failureKind: 'invalid-execution-result',
      process: { terminal: 'invalid' },
    })
  }

  const contradictory = classifyContractTestExecution({
    status: 0,
    signal: null,
    error: Object.assign(new Error('contradictory'), { code: 'ENOENT' }),
  }, 1)
  assert.equal(contradictory.result, 'failed')
  assert.equal(contradictory.failureKind, 'start-failed')
}

function verifyAggregationRequiresAnExactResultSet() {
  const expectedTests = discoverContractTests([
    fileEntry('alpha.tests.mjs'),
    fileEntry('beta.tests.mjs'),
  ], CONTRACT_ROOT)
  const alpha = passedResult(expectedTests[0].testId)
  const beta = passedResult(expectedTests[1].testId)

  const passing = aggregateContractResults({
    expectedTests,
    results: [alpha, beta],
    durationMs: 9,
  })
  assert.equal(passing.result, 'passed')

  for (const results of [
    [alpha],
    [alpha, alpha],
    [alpha, passedResult('browser-contract/foreign.tests.mjs')],
    [alpha, { ...beta, result: 'passed', process: { terminal: 'exited', exitCode: 7 } }],
  ]) {
    const summary = aggregateContractResults({ expectedTests, results, durationMs: 9 })
    assert.equal(summary.result, 'failed')
    assert.equal(summary.reason, 'result-set-invalid')
  }
}

function verifyCliRejectsArgumentsBeforeDiscovery() {
  let reads = 0
  assert.throws(
    () => runBrowserContractCli(['unexpected'], {
      readEntries() {
        reads += 1
        return []
      },
      executeTest: forbiddenExecutor,
      monotonicNow: incrementingClock(),
      write: () => undefined,
    }),
    /accepts no arguments/u,
  )
  assert.equal(reads, 0)
}

function verifyHostileCausesCannotSuppressSettlement() {
  let hostileReads = 0
  const activeError = new Error('opaque')
  Object.defineProperty(activeError, 'code', {
    get() {
      hostileReads += 1
      throw new Error('code accessor entered')
    },
  })
  Object.defineProperty(activeError, 'message', {
    get() {
      hostileReads += 1
      throw new Error('message accessor entered')
    },
  })
  const discoveryWrites = []
  const discovery = runBrowserContractTests({
    readEntries() { throw activeError },
    executeTest: forbiddenExecutor,
    monotonicNow: incrementingClock(),
    write: (encoded) => discoveryWrites.push(encoded),
  })
  assert.equal(discovery.exitCode, 1)
  assert.equal(discovery.summary.discovery.errorCode, 'UNKNOWN')
  assert.equal(discovery.summary.discovery.errorMessage, 'browser contract test discovery failed')
  assert.equal(discoveryWrites.length, 1)

  const activeString = {
    toString() {
      hostileReads += 1
      throw new Error('toString entered')
    },
  }
  const executionWrites = []
  const execution = runBrowserContractTests({
    readEntries: () => [fileEntry('hostile.tests.mjs')],
    executeTest() { throw activeString },
    monotonicNow: incrementingClock(),
    write: (encoded) => executionWrites.push(encoded),
  })
  assert.equal(execution.exitCode, 1)
  assert.equal(execution.results[0].process.errorCode, 'UNKNOWN')
  assert.equal(
    execution.results[0].process.errorMessage,
    'browser contract test process launch failed',
  )
  assert.equal(executionWrites.length, 3)

  const hostileProxy = new Proxy({}, {
    getPrototypeOf() {
      hostileReads += 1
      throw new Error('proxy trap entered')
    },
    getOwnPropertyDescriptor() {
      hostileReads += 1
      throw new Error('proxy trap entered')
    },
  })
  const proxy = runBrowserContractTests({
    readEntries() { throw hostileProxy },
    executeTest: forbiddenExecutor,
    monotonicNow: incrementingClock(),
    write: () => undefined,
  })
  assert.equal(proxy.exitCode, 1)
  assert.equal(proxy.summary.discovery.errorCode, 'UNKNOWN')
  assert.equal(hostileReads, 0)
}

function fileEntry(name) {
  return Object.freeze({ name, kind: 'file' })
}

function successfulExecution() {
  return Object.freeze({ status: 0, signal: null })
}

function passedResult(testId) {
  return Object.freeze({
    schemaVersion: BROWSER_CONTRACT_RUNNER_SCHEMA_VERSION,
    component: 'browser-contract-runner',
    recordType: 'test-result',
    operationId: 'browser-contract',
    testId,
    durationMs: 1,
    result: 'passed',
    process: Object.freeze({ terminal: 'exited', exitCode: 0 }),
  })
}

function sequenceClock(values) {
  let index = 0
  return () => {
    assert(index < values.length, 'monotonic clock was read more often than expected')
    const value = values[index]
    index += 1
    return value
  }
}

function incrementingClock() {
  let value = 0
  return () => {
    const observed = value
    value += 1
    return observed
  }
}

function decodeRecords(writes) {
  return writes.map((encoded) => {
    assert.equal(encoded.endsWith('\n'), true, 'runner records must be newline-delimited')
    assert.equal(encoded.slice(0, -1).includes('\n'), false, 'runner emitted a multi-line record')
    return JSON.parse(encoded)
  })
}

function forbiddenExecutor() {
  throw new Error('executor must not be called')
}
