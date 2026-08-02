import assert from 'node:assert/strict'
import { chmodSync, copyFileSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import {
  TestProcessOwnerControlError,
  TestProcessOwnerTransportError,
} from '../../../../../web/scripts/browser-evidence/process/test-process-owner-client.mjs'

import {
  executeOwnedRuntimeCommand,
  requireRuntimeCommandTraceSnapshot,
  RUNTIME_COMMAND_TRACE_OUTCOME_CODES,
  RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
  RuntimeCommandOutputError,
  RuntimeCommandOwnershipError,
} from '../../process/runtime-command-owner.mjs'

assert.deepEqual(RUNTIME_COMMAND_TRACE_OUTCOME_CODES, [
  'succeeded',
  'request-rejected',
  'execution-rejected',
  'output-rejected',
  'ownership-rejected',
])

await exposesPullTraceOnlyAfterResultSettlement()
await rejectsInvalidRequestsWithTerminalEvidence()
await reportsFullSuccessfulSettlementAndSnapshotsRequest()
await rejectsNonEmptyTreeWithFullSettlement()
await rejectsAuthenticatedOwnerFailureDespiteProvenCleanup()
await rejectsIncompleteOutputWithoutLosingCleanupSettlement()
await preservesAuthenticatedOwnerFailureSettlement()
await rejectsTruncatedAndInvalidUtf8WithRawEvidence()
await rejectsAProcessOwnerArtifactChangedDuringExecution()
await preservesHostileProxyFailuresWithoutAnyTrap()
await preservesNonconfigurableTraceFailuresAsPrimary()
await preservesPrimitiveFailuresWithoutClassification()
await rejectsActiveAndProxyInputsWithoutEnteringThem()
await canonicalizesPrototypeNamedEnvironmentEntries()
await projectsAmbientProcessOwnerCapabilities()
await enforcesPortableOperationIdentifiers()
await enforcesSharedTestIdentityFields()
await deeplySnapshotsSettlementEvidence()
await rejectsNoncanonicalTraceLifecycles()
await aggregatesOperationAndTraceCapacityFailures()

async function exposesPullTraceOnlyAfterResultSettlement() {
  const deferred = Promise.withResolvers()
  const run = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => {
      await deferred.promise
      return executionFixture(true, 'completed')
    },
  }))
  assert.deepEqual(Object.keys(run).sort(), ['result', 'traces'])
  assert.ok(run.result instanceof Promise)
  assert.deepEqual(Object.keys(run.traces), ['snapshot'])
  assert.throws(() => run.traces.snapshot(), /before result settlement/u)
  deferred.resolve()
  const result = await run.result
  assert.equal(Object.hasOwn(result, 'traces'), false)
  const traces = traceSnapshot(run)
  assert.equal(traces.capturedEvents, 2)
  assert.equal(traces.observedEvents, 2)
  assert.equal(traces.completed, true)
  assert.equal(traces.failure, null)
  assert.deepEqual(
    traces.events.map(({ sequence, milestone, outcomeCode }) => ({ sequence, milestone, outcomeCode })),
    [
      { sequence: 0, milestone: 'runtime-command-started', outcomeCode: 'started' },
      { sequence: 1, milestone: 'runtime-command-terminal', outcomeCode: 'succeeded' },
    ],
  )
  for (const event of traces.events) {
    assert.equal(event.schemaVersion, RUNTIME_COMMAND_TRACE_SCHEMA_VERSION)
  }
}

async function rejectsInvalidRequestsWithTerminalEvidence() {
  for (const [request, expected, operationId, platform] of [
    [without(validRequest(), 'processOwner'), /authenticated test process owner/u,
      validRequest().operationId, process.platform],
    [{ ...validRequest(), processOwner: { path: 'relative-owner' } }, /path is invalid/u,
      validRequest().operationId, process.platform],
    [{ ...validRequest(), processOwner: { path: resolve(import.meta.dirname) } }, /regular file/u,
      validRequest().operationId, process.platform],
    [{ ...validRequest(), platform: 'darwin' }, /descendant authority/u,
      validRequest().operationId, 'darwin'],
    [{ ...validRequest(), platform: 'freebsd' }, /unsupported runtime command platform/u,
      validRequest().operationId, 'freebsd'],
  ]) {
    const run = executeOwnedRuntimeCommand(request)
    const [cause] = await rejection(run)
    assert.match(cause.message, expected)
    const traces = traceSnapshot(run, { operationId, platform })
    assert.equal(terminal(traces).outcomeCode, 'request-rejected')
    assert.deepEqual(portableJson(terminal(traces).context.settlement), emptySettlement())
  }
}

async function reportsFullSuccessfulSettlementAndSnapshotsRequest() {
  const release = Promise.withResolvers()
  const argumentsValue = ['-e', 'process.exit(0)']
  const environment = { SNAPSHOT: 'before' }
  const command = {
    executable: process.execPath,
    arguments: argumentsValue,
    cwd: resolve(import.meta.dirname),
    stdin: Uint8Array.of(0x61, 0x62, 0x63),
  }
  const processOwner = { path: process.execPath }
  let ownerRequest
  const request = {
    ...validRequest(),
    command,
    inheritedEnvironment: environment,
    processOwner,
    executeOwner: async (captured) => {
      await release.promise
      ownerRequest = captured
      return executionFixture(true, 'completed', 'semantic result\n', 'diagnostic\n')
    },
  }
  const run = executeOwnedRuntimeCommand(request)
  argumentsValue[0] = '--hostile'
  command.stdin[0] = 0x7a
  environment.SNAPSHOT = 'after'
  command.cwd = resolve(import.meta.dirname, '..')
  processOwner.path = resolve(import.meta.dirname)
  request.deadlineMs = 1
  release.resolve()
  const result = await run.result
  assert.deepEqual(portableJson(result), {
    ...projectedExecution(executionFixture(true, 'completed')),
    stdout: 'semantic result\n',
    stderr: 'diagnostic\n',
  })
  assert.deepEqual(ownerRequest.command.arguments, ['-e', 'process.exit(0)'])
  assert.equal(ownerRequest.command.cwd, resolve(import.meta.dirname))
  assert.deepEqual([...ownerRequest.command.stdin], [0x61, 0x62, 0x63])
  assert.equal(ownerRequest.environment.SNAPSHOT, 'before')
  assert.equal(ownerRequest.owner.path, process.execPath)
  assert.equal(ownerRequest.deadlineMs, 5_000)
  assert.deepEqual(ownerRequest.capture, {
    stdoutBytes: 16 * 1024 * 1024,
    stderrBytes: 16 * 1024 * 1024,
    eventCount: 0,
  })
  assert.equal(Object.hasOwn(ownerRequest, 'stdout'), false)
  assert.equal(Object.hasOwn(ownerRequest, 'stderr'), false)
  const settlement = terminal(traceSnapshot(run)).context.settlement
  assert.equal(settlement.transportOutcome, 'completed')
  assert.equal(settlement.controlOutcome, 'completed')
}

async function rejectsNonEmptyTreeWithFullSettlement() {
  const expected = executionFixture(false, 'failed')
  const run = executeOwnedRuntimeCommand(validRequest({ executeOwner: async () => expected }))
  const [cause] = await rejection(run)
  assert.ok(cause instanceof RuntimeCommandOwnershipError)
  assert.equal(Object.hasOwn(cause, 'traces'), false)
  assert.deepEqual(portableJson(cause.settlement), {
    ...portableJson(projectedExecution(expected)),
    ownerFailure: null,
    transportOutcome: 'completed',
    controlOutcome: 'completed',
    transportEvidence: null,
  })
  const traceSettlement = terminal(traceSnapshot(run)).context.settlement
  assert.equal(terminal(traceSnapshot(run)).outcomeCode, 'ownership-rejected')
  assert.deepEqual(portableJson(traceSettlement), portableJson(cause.settlement))
}

async function rejectsAuthenticatedOwnerFailureDespiteProvenCleanup() {
  const ownerFailure = Object.freeze({
    code: 'linux-subreaper-wait-failed',
    message: 'descendant enumeration became unavailable',
  })
  const expected = Object.freeze({
    ...executionFixture(true, 'completed'),
    ownerFailure,
  })
  const run = executeOwnedRuntimeCommand(validRequest({ executeOwner: async () => expected }))
  const [cause] = await rejection(run)
  assert.ok(cause instanceof RuntimeCommandOwnershipError)
  const trace = terminal(traceSnapshot(run))
  assert.equal(trace.outcomeCode, 'ownership-rejected')
  assert.equal(trace.context.settlement.treeEmpty, true)
  assert.equal(trace.context.settlement.cleanupOutcome, 'completed')
  assert.deepEqual(portableJson(trace.context.settlement.ownerFailure), ownerFailure)
}

async function rejectsIncompleteOutputWithoutLosingCleanupSettlement() {
  const stdout = outputSnapshot('partial', { completed: false })
  const expected = executionFixture(true, 'completed', stdout, '')
  const run = executeOwnedRuntimeCommand(validRequest({ executeOwner: async () => expected }))
  const [cause] = await rejection(run)
  assert.ok(cause instanceof RuntimeCommandOutputError)
  assert.match(cause.message, /snapshot is incomplete/u)
  assert.equal(cause.stream, 'stdout')
  assert.deepEqual(cause.evidence.segments, [{
    sequence: 0,
    offset: 0,
    byteLength: 7,
    base64: 'cGFydGlhbA==',
  }])
  assert.equal(Object.hasOwn(cause, 'traces'), false)
  const trace = terminal(traceSnapshot(run))
  assert.equal(trace.outcomeCode, 'output-rejected')
  assert.equal(trace.context.settlement.treeEmpty, true)
  assert.equal(trace.context.settlement.cleanupOutcome, 'completed')
  assert.equal(trace.context.settlement.transportOutcome, 'completed')
  assert.equal(trace.context.settlement.controlOutcome, 'completed')
}

async function preservesAuthenticatedOwnerFailureSettlement() {
  const expected = executionFixture(true, 'completed')
  const terminalEvidence = Object.freeze({ code: 17, signal: null })
  const transportFailure = new TestProcessOwnerTransportError(
    'authenticated transport failure',
    expected,
    terminalEvidence,
    expected.output,
    Object.freeze([]),
    new Error('transport failed'),
  )
  const transportRun = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw transportFailure },
  }))
  assert.equal((await rejection(transportRun))[0], transportFailure)
  const transportTrace = terminal(traceSnapshot(transportRun))
  assert.equal(transportTrace.outcomeCode, 'execution-rejected')
  assert.equal(transportTrace.context.settlement.treeEmpty, true)
  assert.equal(transportTrace.context.settlement.cleanupOutcome, 'completed')
  assert.equal(transportTrace.context.settlement.transportOutcome, 'failed')
  assert.equal(transportTrace.context.settlement.controlOutcome, 'not-observed')
  assert.deepEqual(portableJson(transportTrace.context.settlement.transportEvidence), {
    kind: 'test-process-owner-transport',
    terminal: terminalEvidence,
  })

  const controlFailure = new TestProcessOwnerControlError(
    'authenticated control publication failure',
    expected,
    new Error('control publication failed'),
  )
  const controlRun = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw controlFailure },
  }))
  assert.equal((await rejection(controlRun))[0], controlFailure)
  const controlTrace = terminal(traceSnapshot(controlRun))
  assert.equal(controlTrace.outcomeCode, 'execution-rejected')
  assert.equal(controlTrace.context.settlement.treeEmpty, true)
  assert.equal(controlTrace.context.settlement.cleanupOutcome, 'completed')
  assert.equal(controlTrace.context.settlement.transportOutcome, 'completed')
  assert.equal(controlTrace.context.settlement.controlOutcome, 'failed')
  assert.deepEqual(portableJson(controlTrace.context.settlement.transportEvidence), {
    kind: 'test-process-owner-control',
    publication: 'failed',
  })

  const forgedFailure = Object.assign(new Error('forgeable public fields'), {
    kind: 'transport-failed',
    settlement: expected,
    transportEvidence: terminalEvidence,
  })
  const forgedRun = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw forgedFailure },
  }))
  assert.equal((await rejection(forgedRun))[0], forgedFailure)
  assert.deepEqual(
    portableJson(terminal(traceSnapshot(forgedRun)).context.settlement),
    emptySettlement(),
  )
}

async function rejectsTruncatedAndInvalidUtf8WithRawEvidence() {
  for (const [snapshot, expectedMessage, expectedBase64] of [
    [outputSnapshot('bounded', { observedBytes: 8, capturedBytes: 7, truncated: true }),
      /snapshot is truncated/u, 'Ym91bmRlZA=='],
    [outputSnapshot(Uint8Array.of(0xc3, 0x28)), /not valid UTF-8/u, 'wyg='],
  ]) {
    const run = executeOwnedRuntimeCommand(validRequest({
      executeOwner: async () => executionFixture(true, 'completed', snapshot, ''),
    }))
    const [cause] = await rejection(run)
    assert.ok(cause instanceof RuntimeCommandOutputError)
    assert.match(cause.message, expectedMessage)
    assert.equal(cause.evidence.segments[0].base64, expectedBase64)
    assert.equal(terminal(traceSnapshot(run)).outcomeCode, 'output-rejected')
  }
}

async function rejectsAProcessOwnerArtifactChangedDuringExecution() {
  const directory = mkdtempSync(join(tmpdir(), 'windshare-runtime-owner-'))
  try {
    const ownerPath = resolve(directory, process.platform === 'win32' ? 'owner.exe' : 'owner')
    copyFileSync(process.execPath, ownerPath)
    if (process.platform !== 'win32') chmodSync(ownerPath, 0o755)
    const canonicalOwnerPath = realpathSync(ownerPath)
    const run = executeOwnedRuntimeCommand(validRequest({
      processOwner: Object.freeze({ path: canonicalOwnerPath }),
      executeOwner: async () => {
        writeFileSync(ownerPath, 'replaced while the operation was active')
        return executionFixture(true, 'completed')
      },
    }))
    assert.match((await rejection(run))[0].message, /changed while used/u)
    const trace = terminal(traceSnapshot(run))
    assert.equal(trace.outcomeCode, 'execution-rejected')
    assert.equal(trace.context.settlement.treeEmpty, true)
    assert.equal(trace.context.settlement.cleanupOutcome, 'completed')
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
}

async function preservesHostileProxyFailuresWithoutAnyTrap() {
  let traps = 0
  const trap = () => {
    traps += 1
    throw new Error('hostile Error proxy trap entered')
  }
  const proxiedError = new Proxy(new Error('primary proxy failure'), {
    defineProperty: trap,
    get: trap,
    getOwnPropertyDescriptor: trap,
    getPrototypeOf: trap,
    ownKeys: trap,
    set: trap,
  })
  const proxiedRun = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw proxiedError },
  }))
  assert.equal((await rejection(proxiedRun))[0], proxiedError)
  assert.equal(traps, 0)
  assert.equal(terminal(traceSnapshot(proxiedRun)).outcomeCode, 'execution-rejected')

  const revocable = Proxy.revocable(new Error('revoked primary failure'), {})
  const revokedError = revocable.proxy
  revocable.revoke()
  const revokedRun = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw revokedError },
  }))
  assert.equal((await rejection(revokedRun))[0], revokedError)
  const revokedTrace = traceSnapshot(revokedRun)
  assert.equal(revokedTrace.failure, null)
  assert.equal(revokedTrace.capturedEvents, 2)
  assert.equal(terminal(revokedTrace).outcomeCode, 'execution-rejected')
}

async function preservesNonconfigurableTraceFailuresAsPrimary() {
  const primary = new Error('primary execution failure')
  const sentinel = Object.freeze({ caller: 'owned' })
  Object.defineProperty(primary, 'traces', {
    value: sentinel,
    enumerable: true,
    writable: false,
    configurable: false,
  })
  const run = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw primary },
  }))
  assert.equal((await rejection(run))[0], primary)
  assert.equal(primary.traces, sentinel)
  assert.equal(terminal(traceSnapshot(run)).outcomeCode, 'execution-rejected')
}

async function preservesPrimitiveFailuresWithoutClassification() {
  const run = executeOwnedRuntimeCommand(validRequest({
    executeOwner: async () => { throw undefined },
  }))
  assert.equal((await rejection(run))[0], undefined)
  assert.equal(terminal(traceSnapshot(run)).outcomeCode, 'execution-rejected')
}

async function rejectsActiveAndProxyInputsWithoutEnteringThem() {
  let traceGetterCalls = 0
  let launches = 0
  const getterRequest = { ...validRequest() }
  Object.defineProperty(getterRequest, 'trace', {
    enumerable: true,
    get() {
      traceGetterCalls += 1
      return () => {}
    },
  })
  const getterRun = executeOwnedRuntimeCommand(getterRequest)
  assert.match((await rejection(getterRun))[0].message, /enumerable inert data/u)
  assert.equal(traceGetterCalls, 0)
  assert.equal(launches, 0)
  assert.equal(terminal(traceSnapshot(getterRun)).outcomeCode, 'request-rejected')

  for (const field of ['request', 'command', 'processOwner', 'arguments']) {
    let proxyTraps = 0
    const proxy = new Proxy(field === 'arguments' ? [] : {}, {
      get: () => { proxyTraps += 1; throw new Error('proxy get entered') },
      getOwnPropertyDescriptor: () => { proxyTraps += 1; throw new Error('proxy descriptor entered') },
      getPrototypeOf: () => { proxyTraps += 1; throw new Error('proxy prototype entered') },
      ownKeys: () => { proxyTraps += 1; throw new Error('proxy keys entered') },
    })
    const request = field === 'request'
      ? proxy
      : field === 'command'
        ? { ...validRequest(), command: proxy }
        : field === 'processOwner'
          ? { ...validRequest(), processOwner: proxy }
          : {
              ...validRequest(),
              command: { ...validRequest().command, arguments: proxy },
            }
    const run = executeOwnedRuntimeCommand(request)
    await rejection(run)
    assert.equal(proxyTraps, 0, field)
    const trace = run.traces.snapshot()
    assert.equal(trace.capturedEvents, 2)
    assert.equal(terminal(trace).outcomeCode, 'request-rejected')
  }

  const revoked = Proxy.revocable({}, {})
  revoked.revoke()
  const revokedRequestRun = executeOwnedRuntimeCommand(revoked.proxy)
  await rejection(revokedRequestRun)
  const revokedRequestTrace = revokedRequestRun.traces.snapshot()
  requireRuntimeCommandTraceSnapshot(revokedRequestTrace, {
    operationId: 'unbound-request',
    platform: 'unbound',
  })
  assert.equal(terminal(revokedRequestTrace).outcomeCode, 'request-rejected')

  const activeEnvironment = {}
  let environmentGetterCalls = 0
  Object.defineProperty(activeEnvironment, 'PATH', {
    enumerable: true,
    get() {
      environmentGetterCalls += 1
      return 'active'
    },
  })
  const activeRun = executeOwnedRuntimeCommand(validRequest({ inheritedEnvironment: activeEnvironment }))
  assert.match((await rejection(activeRun))[0].message, /enumerable inert data/u)
  assert.equal(environmentGetterCalls, 0)

  const unsupportedRun = executeOwnedRuntimeCommand({ ...validRequest(), trace: () => { launches += 1 } })
  assert.match((await rejection(unsupportedRun))[0].message, /unsupported options/u)
  assert.equal(launches, 0)
}

async function canonicalizesPrototypeNamedEnvironmentEntries() {
  const inheritedEnvironment = Object.create(null)
  Object.defineProperty(inheritedEnvironment, '__proto__', {
    enumerable: true,
    value: 'inert-environment-value',
  })
  Object.defineProperty(inheritedEnvironment, 'Path', {
    enumerable: true,
    value: 'C:\\authority',
  })
  let capturedEnvironment
  const run = executeOwnedRuntimeCommand(validRequest({
    inheritedEnvironment,
    executeOwner: async (request) => {
      capturedEnvironment = request.environment
      return executionFixture(true, 'completed')
    },
  }))
  await run.result
  assert.equal(Object.getPrototypeOf(capturedEnvironment), null)
  assert.equal(Object.hasOwn(capturedEnvironment, '__proto__'), true)
  assert.equal(capturedEnvironment.__proto__, 'inert-environment-value')
  assert.equal(capturedEnvironment.Path, 'C:\\authority')
  assert.equal({}.polluted, undefined)

  const duplicate = executeOwnedRuntimeCommand(validRequest({
    inheritedEnvironment: { PATH: 'first', Path: 'second' },
  }))
  assert.match((await rejection(duplicate))[0].message, /case-insensitive duplicate/u)
}

async function projectsAmbientProcessOwnerCapabilities() {
  let ownerRequest
  const run = executeOwnedRuntimeCommand(validRequest({
    inheritedEnvironment: {
      PATH: 'retained-path',
      WINDSHARE_TEST_EVENT_FD: 'ambient-fd',
      windshare_test_event_handle: 'ambient-handle',
      WINDSHARE_TEST_RUN_ID: 'ambient-run',
      windshare_test_run_id: 'duplicate-ambient-run',
      WINDSHARE_TEST_OPERATION_ID: 'ambient-operation',
      WINDSHARE_TEST_SCENARIO: 'ambient-scenario',
    },
    executeOwner: async (request) => {
      ownerRequest = request
      return executionFixture(true, 'completed')
    },
  }))
  await run.result

  assert.deepEqual(Object.keys(ownerRequest.environment), ['PATH'])
  assert.equal(ownerRequest.environment.PATH, 'retained-path')
  assert.equal(ownerRequest.runId, 'browsergate')
  assert.equal(ownerRequest.operationId, validRequest().operationId)
  assert.equal(ownerRequest.scenario, 'browsergate-runtime-command')
}

async function enforcesPortableOperationIdentifiers() {
  for (const operationId of ['._-', '.edge', 'edge-', '_edge']) {
    const run = executeOwnedRuntimeCommand(validRequest({ operationId }))
    assert.match((await rejection(run))[0].message, /operation ID is invalid/u)
    assert.equal(terminal(run.traces.snapshot()).outcomeCode, 'request-rejected')
  }
  let launched = false
  const run = executeOwnedRuntimeCommand(validRequest({
    operationId: 'edge._-interior',
    executeOwner: async () => {
      launched = true
      return executionFixture(true, 'completed')
    },
  }))
  await run.result
  assert.equal(launched, true)
  traceSnapshot(run, { operationId: 'edge._-interior', platform: process.platform })
}

async function enforcesSharedTestIdentityFields() {
  for (const [field, value, expected] of [
    ['runId', 'invalid/run', /run ID is invalid/u],
    ['runId', '-leading', /run ID is invalid/u],
    ['scenario', '/leading', /scenario is invalid/u],
    ['scenario', 'trailing/', /scenario is invalid/u],
  ]) {
    let launched = false
    const run = executeOwnedRuntimeCommand(validRequest({
      [field]: value,
      executeOwner: async () => {
        launched = true
        return executionFixture(true, 'completed')
      },
    }))
    assert.match((await rejection(run))[0].message, expected)
    assert.equal(launched, false)
    assert.equal(terminal(run.traces.snapshot()).outcomeCode, 'request-rejected')
  }

  let identity
  const accepted = executeOwnedRuntimeCommand(validRequest({
    runId: 'run_1',
    scenario: 'network/relay',
    executeOwner: async (request) => {
      identity = { runId: request.runId, scenario: request.scenario }
      return executionFixture(true, 'completed')
    },
  }))
  await accepted.result
  assert.deepEqual(identity, { runId: 'run_1', scenario: 'network/relay' })
}

async function deeplySnapshotsSettlementEvidence() {
  const mutableDetail = { state: 'before-settlement' }
  const execution = {
    ...executionFixture(true, 'completed'),
    processEvidence: { terminal: 'exited', exitCode: 0, detail: mutableDetail },
  }
  const run = executeOwnedRuntimeCommand(validRequest({ executeOwner: async () => execution }))
  await run.result
  mutableDetail.state = 'after-settlement'
  const captured = terminal(traceSnapshot(run)).context.settlement.processEvidence.detail
  assert.equal(captured.state, 'before-settlement')
  assert.equal(Object.isFrozen(captured), true)
  assert.throws(() => { captured.state = 'mutation' }, TypeError)
}

async function rejectsNoncanonicalTraceLifecycles() {
  const run = executeOwnedRuntimeCommand(validRequest())
  await run.result
  const identity = { operationId: 'runtime-owner-contract', platform: process.platform }
  const unknownOutcome = portableJson(run.traces.snapshot())
  unknownOutcome.events[1].outcomeCode = 'unknown-terminal'
  recountTrace(unknownOutcome)
  assert.throws(
    () => requireRuntimeCommandTraceSnapshot(unknownOutcome, identity),
    /lifecycle sequence/u,
  )

  const missingTerminal = portableJson(run.traces.snapshot())
  missingTerminal.events.pop()
  recountTrace(missingTerminal)
  assert.throws(
    () => requireRuntimeCommandTraceSnapshot(missingTerminal, identity),
    /lifecycle cardinality/u,
  )
}

async function aggregatesOperationAndTraceCapacityFailures() {
  const oversizedEvidence = { detail: 'x'.repeat(300 * 1024) }
  const execution = {
    ...executionFixture(true, 'completed', outputSnapshot('partial', { completed: false }), ''),
    processEvidence: oversizedEvidence,
  }
  const run = executeOwnedRuntimeCommand(validRequest({ executeOwner: async () => execution }))
  const [cause] = await rejection(run)
  assert.ok(cause instanceof AggregateError)
  assert.equal(cause.errors.length, 2)
  assert.ok(cause.errors[0] instanceof RuntimeCommandOutputError)
  assert.match(cause.errors[1].message, /rejected a lifecycle event/u)
  const trace = run.traces.snapshot()
  assert.equal(trace.completed, true)
  assert.equal(trace.truncated, true)
  assert.equal(trace.failure.code, 'capacity-exceeded')
  assert.equal(trace.observedEvents, 2)
  assert.equal(trace.capturedEvents, 1)
  assert.equal(trace.events[0].milestone, 'runtime-command-started')
}

function validRequest(overrides = {}) {
  return {
    operationId: 'runtime-owner-contract',
    command: {
      executable: process.execPath,
      arguments: ['-e', 'process.exit(0)'],
      cwd: resolve(import.meta.dirname),
    },
    platform: process.platform,
    inheritedEnvironment: {},
    deadlineMs: 5_000,
    terminationGraceMs: 1_000,
    processOwner: processOwnerArtifact(),
    executeOwner: async () => executionFixture(true, 'completed'),
    ...overrides,
  }
}

function executionFixture(treeEmpty, cleanupOutcome, stdout = '', stderr = '') {
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    treeEmpty,
    cleanupOutcome,
    inputEvidence: Object.freeze({ outcome: 'not_requested', failureCode: '', failureMessage: '' }),
    output: Object.freeze({
      stdout: typeof stdout === 'string' ? outputSnapshot(stdout) : stdout,
      stderr: typeof stderr === 'string' ? outputSnapshot(stderr) : stderr,
    }),
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner',
      backend: process.platform === 'win32' ? 'windows_job' : 'linux_subreaper',
      terminationReason: 'natural',
      platform: Object.freeze({ kind: process.platform === 'win32' ? 'windows_job' : 'linux_subreaper' }),
    }),
  })
}

function projectedExecution(execution) {
  return Object.freeze({
    processEvidence: execution.processEvidence,
    treeEmpty: execution.treeEmpty,
    cleanupOutcome: execution.cleanupOutcome,
    inputEvidence: execution.inputEvidence,
    ownershipEvidence: execution.ownershipEvidence,
  })
}

function outputSnapshot(value, overrides = {}) {
  const bytes = typeof value === 'string' ? new TextEncoder().encode(value) : Uint8Array.from(value)
  return Object.freeze({
    observedBytes: bytes.byteLength,
    capturedBytes: bytes.byteLength,
    truncated: false,
    completed: true,
    ...overrides,
    bytes: () => Uint8Array.from(bytes),
  })
}

function processOwnerArtifact() {
  return Object.freeze({ path: process.execPath })
}

async function rejection(run) {
  try {
    await run.result
  } catch (cause) {
    return Object.freeze([cause])
  }
  assert.fail('runtime command result unexpectedly resolved')
}

function traceSnapshot(run, identity = {
  operationId: 'runtime-owner-contract',
  platform: process.platform,
}) {
  const snapshot = run.traces.snapshot()
  return requireRuntimeCommandTraceSnapshot(snapshot, identity)
}

function terminal(snapshot) {
  return snapshot.events[1]
}

function recountTrace(snapshot) {
  const bytes = snapshot.events.reduce(
    (total, event) => total + Buffer.byteLength(JSON.stringify(event), 'utf8'),
    0,
  )
  snapshot.observedEvents = snapshot.events.length
  snapshot.capturedEvents = snapshot.events.length
  snapshot.observedBytes = bytes
  snapshot.capturedBytes = bytes
}

function emptySettlement() {
  return {
    processEvidence: null,
    inputEvidence: null,
    ownerFailure: null,
    treeEmpty: null,
    cleanupOutcome: 'not-observed',
    ownershipEvidence: null,
    transportOutcome: 'not-observed',
    controlOutcome: 'not-observed',
    transportEvidence: null,
  }
}

function without(value, name) {
  const result = { ...value }
  delete result[name]
  return result
}

function portableJson(value) {
  return JSON.parse(JSON.stringify(value))
}

process.stdout.write('runtime command owner pull-channel contracts: PASS\n')
