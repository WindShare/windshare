import assert from 'node:assert/strict'

import {
  browserGateCliFailureEvent,
  runBrowserGateCli,
  settledBrowserGateFailureTraces,
} from '../../main.mjs'

let runtimeAssertionAccesses = 0
let implementationAccesses = 0
const forbiddenImplementation = {}
Object.defineProperty(forbiddenImplementation, 'assertRuntimeNodeVersion', {
  enumerable: true,
  get() {
    runtimeAssertionAccesses += 1
    throw new Error('runtime assertion was observed before invocation validation')
  },
})
Object.defineProperty(forbiddenImplementation, 'loadCommand', {
  enumerable: true,
  get() {
    implementationAccesses += 1
    throw new Error('command implementation was observed before command validation')
  },
})

for (const [arguments_, expected] of [
  [[], /browser orchestration commands/u],
  [['unknown'], /unknown browser orchestration command/u],
  [['--plan'], /unknown browser orchestration command/u],
  [[null], /non-text token/u],
  [['plan', 7], /options must be text/u],
]) {
  await assert.rejects(runBrowserGateCli(arguments_, forbiddenImplementation), expected)
}
await assert.rejects(
  runBrowserGateCli('plan', forbiddenImplementation),
  /arguments must be an array/u,
)
assert.equal(
  runtimeAssertionAccesses,
  0,
  'usage and invalid invocations must not observe the runtime assertion port',
)
assert.equal(
  implementationAccesses,
  0,
  'usage and invalid invocations must not observe the command implementation port',
)

const dispatchOrder = []
let runtimeAssertions = 0
let implementationLoads = 0
let handlerCalls = 0
const result = await runBrowserGateCli(
  ['plan', '--platform', 'linux'],
  {
    async assertRuntimeNodeVersion() {
      runtimeAssertions += 1
      dispatchOrder.push('assert-runtime')
    },
    async loadCommand(command) {
      implementationLoads += 1
      dispatchOrder.push('load-command')
      assert.equal(command, 'plan')
      return async (optionArguments) => {
        handlerCalls += 1
        dispatchOrder.push('invoke-handler')
        assert.equal(Object.isFrozen(optionArguments), true)
        assert.deepEqual(optionArguments, ['--platform', 'linux'])
        return { exitCode: 23, traces: completeTraceSnapshot() }
      }
    },
  },
)
assert.equal(result.exitCode, 23)
assert.equal(result.traces.completed, true)
assert.equal(runtimeAssertions, 1, 'one valid dispatch must assert the runtime exactly once')
assert.equal(implementationLoads, 1, 'one valid dispatch must load one command implementation')
assert.equal(handlerCalls, 1, 'one valid dispatch must invoke one command handler')
assert.deepEqual(
  dispatchOrder,
  ['assert-runtime', 'load-command', 'invoke-handler'],
  'runtime authority must settle before implementation loading and dispatch',
)

let defaultAssertionImplementationLoads = 0
const defaultAssertionResult = await runBrowserGateCli(['plan'], {
  async loadCommand() {
    defaultAssertionImplementationLoads += 1
    return async () => ({ exitCode: 29, traces: completeTraceSnapshot() })
  },
})
assert.equal(defaultAssertionResult.exitCode, 29)
assert.equal(
  defaultAssertionImplementationLoads,
  1,
  'the repository runtime assertion must permit dispatch under the pinned Node version',
)

let mismatchAssertions = 0
let mismatchLoaderAccesses = 0
const mismatchPorts = {
  assertRuntimeNodeVersion() {
    mismatchAssertions += 1
    throw new Error('active Node version does not match repository pin')
  },
}
Object.defineProperty(mismatchPorts, 'loadCommand', {
  enumerable: true,
  get() {
    mismatchLoaderAccesses += 1
    throw new Error('command loader was observed after a runtime mismatch')
  },
})
await assert.rejects(
  runBrowserGateCli(['plan'], mismatchPorts),
  /active Node version does not match repository pin/u,
)
assert.equal(mismatchAssertions, 1, 'a valid invocation must perform one runtime assertion')
assert.equal(mismatchLoaderAccesses, 0, 'a runtime mismatch must prevent command loader observation')

let invalidAssertionLoaderAccesses = 0
const invalidAssertionPorts = { assertRuntimeNodeVersion: null }
Object.defineProperty(invalidAssertionPorts, 'loadCommand', {
  enumerable: true,
  get() {
    invalidAssertionLoaderAccesses += 1
    throw new Error('command loader was observed with an invalid runtime assertion port')
  },
})
await assert.rejects(
  runBrowserGateCli(['plan'], invalidAssertionPorts),
  /runtime Node version assertion is invalid/u,
)
assert.equal(
  invalidAssertionLoaderAccesses,
  0,
  'an invalid runtime assertion port must fail closed before loader observation',
)

const trustedFailure = new Error('opaque')
Object.defineProperty(trustedFailure, 'traces', {
  enumerable: true,
  value: completeTraceSnapshot(),
})
assert.equal(settledBrowserGateFailureTraces(trustedFailure)?.completed, true)

let hostileReads = 0
const messageAccessor = new Error('opaque')
Object.defineProperty(messageAccessor, 'message', {
  get() {
    hostileReads += 1
    throw new Error('message getter entered')
  },
})
assert.equal(settledBrowserGateFailureTraces(messageAccessor), null)
assert.equal(settledBrowserGateFailureTraces({
  toString() {
    hostileReads += 1
    throw new Error('toString entered')
  },
}), null)
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
assert.equal(settledBrowserGateFailureTraces(hostileProxy), null)
assert.equal(hostileReads, 0)
assert.deepEqual(browserGateCliFailureEvent().payload, {
  failureCode: 'browsergate-command-failed',
  reportedOutcome: 'failed',
})

function completeTraceSnapshot() {
  return Object.freeze({
    events: Object.freeze([]),
    observedEvents: 0,
    capturedEvents: 0,
    observedBytes: 0,
    capturedBytes: 0,
    truncated: false,
    completed: true,
    failure: null,
  })
}

process.stdout.write('browsergate lazy command router contracts: PASS\n')
