import assert from 'node:assert/strict'

import {
  consumeOwnedRuntimeCommandExecution,
} from '../../process/runtime-command-consumer.mjs'
import {
  RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
} from '../../process/runtime-command-owner.mjs'
import { createOwnedTraceJournal } from '../../owned-trace-journal.mjs'

const IDENTITY = Object.freeze({
  operationId: 'runtime-consumer-contract',
  platform: 'linux',
})

await acceptsSettledSuccessAndCopiesEvidence()
await consumesNativePromiseWithoutReadingOwnThen()
await rejectsHostileFulfilledEvidenceWithoutRunningTraps()
await rejectionCausesRemainOpaqueAndUnmodified()
await rejectsHostileChannelsAndTraceViews()
await rejectsResultTraceContradictions()
await classifiesOwnershipFromTheTerminalTrace()

async function acceptsSettledSuccessAndCopiesEvidence() {
  const execution = successfulExecution()
  const trace = runtimeTrace('succeeded', settlementFor(execution))
  const fixture = runtimeChannel({ execution, trace })
  const consumed = await consumeOwnedRuntimeCommandExecution(fixture.channel, IDENTITY)
  assert.equal(consumed.outcome, 'fulfilled')
  assert.equal(consumed.failure, null)
  assert.equal(consumed.failureKind, null)
  assert.equal(consumed.traces.events.at(-1).outcomeCode, 'succeeded')
  assert.equal(fixture.snapshotCalls(), 1)
  assert(Object.isFrozen(consumed.execution))
  assert.notEqual(consumed.execution, execution)
  assert.equal(Object.getPrototypeOf(consumed.execution), null)
  assert.equal(consumed.execution.processEvidence.exitCode, 0)
}

async function consumesNativePromiseWithoutReadingOwnThen() {
  const execution = successfulExecution()
  const result = Promise.resolve(execution)
  let thenGetterCalls = 0
  Object.defineProperty(result, 'then', {
    get() {
      thenGetterCalls += 1
      throw new Error('native Promise then getter executed')
    },
    enumerable: true,
    configurable: false,
  })
  const fixture = runtimeChannel({
    result,
    trace: runtimeTrace('succeeded', settlementFor(execution)),
  })
  const consumed = await consumeOwnedRuntimeCommandExecution(fixture.channel, IDENTITY)
  assert.equal(consumed.outcome, 'fulfilled')
  assert.equal(thenGetterCalls, 0)
  assert.equal(fixture.snapshotCalls(), 1)
}

async function rejectsHostileFulfilledEvidenceWithoutRunningTraps() {
  const successfulTrace = runtimeTrace('succeeded', settlementFor(successfulExecution()))
  const { proxy: revokedExecution, revoke } = Proxy.revocable(successfulExecution(), {})
  const revokedResult = Promise.resolve(revokedExecution)
  await revokedResult
  revoke()

  let getterCalls = 0
  const accessorExecution = { ...successfulExecution() }
  Object.defineProperty(accessorExecution, 'ownershipEvidence', {
    get() {
      getterCalls += 1
      throw new Error('hostile ownership getter executed')
    },
    enumerable: true,
    configurable: false,
  })
  const nestedProxyExecution = {
    ...successfulExecution(),
    processEvidence: new Proxy(successfulExecution().processEvidence, {
      getPrototypeOf() {
        throw new Error('hostile nested prototype trap executed')
      },
    }),
  }

  for (const [name, fixture] of [
    ['revoked-result', runtimeChannel({ result: revokedResult, trace: successfulTrace })],
    ['nonconfig-accessor', runtimeChannel({ execution: accessorExecution, trace: successfulTrace })],
    ['nested-proxy', runtimeChannel({ execution: nestedProxyExecution, trace: successfulTrace })],
  ]) {
    const consumed = await consumeOwnedRuntimeCommandExecution(fixture.channel, IDENTITY)
    assert.equal(consumed.outcome, 'rejected', name)
    assert.equal(consumed.failureKind, 'runtime-command-contract-failed', name)
    assert.notEqual(consumed.traces, null, name)
    assert.equal(consumed.traces.events.at(-1).outcomeCode, 'succeeded', name)
    assert.equal(fixture.snapshotCalls(), 1, name)
    assert(consumed.failure instanceof Error, name)
  }
  assert.equal(getterCalls, 0)
}

async function rejectionCausesRemainOpaqueAndUnmodified() {
  const failureTrace = runtimeTrace('execution-rejected', emptySettlement())
  const decorated = new Error('injected runtime rejection')
  const originalOperationTraces = Object.freeze({ sentinel: true })
  Object.defineProperty(decorated, 'operationTraces', {
    value: originalOperationTraces,
    enumerable: true,
    configurable: false,
    writable: false,
  })
  Object.defineProperty(decorated, 'traces', {
    get() {
      throw new Error('arbitrary rejection traces getter executed')
    },
    enumerable: true,
    configurable: false,
  })

  const decoratedFixture = runtimeChannel({ rejection: decorated, trace: failureTrace })
  const decoratedConsumption = await consumeOwnedRuntimeCommandExecution(
    decoratedFixture.channel,
    IDENTITY,
  )
  assert.equal(decoratedConsumption.outcome, 'rejected')
  assert.equal(decoratedConsumption.failureKind, 'runtime-command-failed')
  assert.equal(decoratedConsumption.traces.events.at(-1).outcomeCode, 'execution-rejected')
  assert.equal(decoratedConsumption.failure.cause, decorated)
  assert.equal(decorated.operationTraces, originalOperationTraces)
  assert.equal(decoratedFixture.snapshotCalls(), 1)

  const { proxy: revokedFailure, revoke } = Proxy.revocable(new Error('hidden'), {})
  revoke()
  const revokedFixture = runtimeChannel({ rejection: revokedFailure, trace: failureTrace })
  const revokedConsumption = await consumeOwnedRuntimeCommandExecution(
    revokedFixture.channel,
    IDENTITY,
  )
  assert.equal(revokedConsumption.outcome, 'rejected')
  assert.equal(revokedConsumption.failureKind, 'runtime-command-failed')
  assert(revokedConsumption.failure instanceof Error)
  assert.equal(revokedFixture.snapshotCalls(), 1)
}

async function rejectsHostileChannelsAndTraceViews() {
  const { proxy: revokedChannel, revoke } = Proxy.revocable({}, {})
  revoke()
  const rejectedChannel = await consumeOwnedRuntimeCommandExecution(revokedChannel, IDENTITY)
  assert.equal(rejectedChannel.outcome, 'rejected')
  assert.equal(rejectedChannel.failureKind, 'runtime-command-contract-failed')
  assert.equal(rejectedChannel.traces, null)

  const result = Promise.resolve(successfulExecution())
  let accessorCalls = 0
  const hostileView = {}
  Object.defineProperty(hostileView, 'snapshot', {
    get() {
      accessorCalls += 1
      throw new Error('hostile snapshot accessor executed')
    },
    enumerable: true,
    configurable: false,
  })
  Object.freeze(hostileView)
  const channel = Object.freeze({ result, traces: hostileView })
  const rejectedView = await consumeOwnedRuntimeCommandExecution(channel, IDENTITY)
  assert.equal(rejectedView.outcome, 'rejected')
  assert.equal(rejectedView.failureKind, 'runtime-command-contract-failed')
  assert.equal(rejectedView.traces, null)
  assert.equal(accessorCalls, 0)
}

async function rejectsResultTraceContradictions() {
  const execution = successfulExecution()
  const contradicted = {
    ...settlementFor(execution),
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 7 }),
  }
  const fixture = runtimeChannel({
    execution,
    trace: runtimeTrace('succeeded', Object.freeze(contradicted)),
  })
  const consumed = await consumeOwnedRuntimeCommandExecution(fixture.channel, IDENTITY)
  assert.equal(consumed.outcome, 'rejected')
  assert.equal(consumed.failureKind, 'runtime-command-contract-failed')
  assert.equal(consumed.traces.events.at(-1).outcomeCode, 'succeeded')
  assert.equal(fixture.snapshotCalls(), 1)
}

async function classifiesOwnershipFromTheTerminalTrace() {
  const hostile = new Error('name is not classification authority')
  hostile.name = 'SomethingElse'
  const fixture = runtimeChannel({
    rejection: hostile,
    trace: runtimeTrace('ownership-rejected', Object.freeze({
      ...settlementFor(successfulExecution()),
      treeEmpty: false,
      cleanupOutcome: 'failed',
    })),
  })
  const consumed = await consumeOwnedRuntimeCommandExecution(fixture.channel, IDENTITY)
  assert.equal(consumed.outcome, 'rejected')
  assert.equal(consumed.failureKind, 'process-tree-not-empty')
  assert.equal(fixture.snapshotCalls(), 1)
}

function runtimeChannel({ execution, rejection, result: configuredResult, trace }) {
  let settled = configuredResult !== undefined
  let snapshotCallCount = 0
  const result = configuredResult ?? new Promise((resolve, reject) => {
    queueMicrotask(() => {
      settled = true
      if (rejection === undefined) resolve(execution)
      else reject(rejection)
    })
  })
  const traces = Object.freeze({
    snapshot() {
      assert.equal(settled, true, 'trace snapshot was pulled before result settlement')
      snapshotCallCount += 1
      return trace
    },
  })
  return Object.freeze({
    channel: Object.freeze({ result, traces }),
    snapshotCalls: () => snapshotCallCount,
  })
}

function successfulExecution() {
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    treeEmpty: true,
    cleanupOutcome: 'completed',
    inputEvidence: Object.freeze({
      outcome: 'not_requested',
      failureCode: '',
      failureMessage: '',
    }),
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner',
      backend: 'linux_subreaper',
      terminationReason: 'natural',
      platform: Object.freeze({ kind: 'linux_subreaper' }),
    }),
    stdout: 'verified\n',
    stderr: '',
  })
}

function settlementFor(execution) {
  return Object.freeze({
    processEvidence: execution.processEvidence,
    inputEvidence: execution.inputEvidence,
    ownerFailure: null,
    treeEmpty: execution.treeEmpty,
    cleanupOutcome: execution.cleanupOutcome,
    ownershipEvidence: execution.ownershipEvidence,
    transportOutcome: 'completed',
    controlOutcome: 'completed',
    transportEvidence: null,
  })
}

function emptySettlement() {
  return Object.freeze({
    processEvidence: null,
    inputEvidence: null,
    ownerFailure: null,
    treeEmpty: null,
    cleanupOutcome: 'not-observed',
    ownershipEvidence: null,
    transportOutcome: 'not-observed',
    controlOutcome: 'not-observed',
    transportEvidence: null,
  })
}

function runtimeTrace(outcomeCode, settlement) {
  const journal = createOwnedTraceJournal({
    label: 'runtime command consumer fixture trace',
    maximumEvents: 2,
    maximumBytes: 256 * 1024,
  })
  assert.equal(journal.append(Object.freeze({
    schemaVersion: RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
    sequence: 0,
    milestone: 'runtime-command-started',
    outcomeCode: 'started',
    context: Object.freeze({
      operationId: IDENTITY.operationId,
      platform: IDENTITY.platform,
    }),
  })), true)
  assert.equal(journal.append(Object.freeze({
    schemaVersion: RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
    sequence: 1,
    milestone: 'runtime-command-terminal',
    outcomeCode,
    context: Object.freeze({
      operationId: IDENTITY.operationId,
      platform: IDENTITY.platform,
      settlement,
    }),
  })), true)
  journal.finish()
  return journal.view.snapshot()
}
