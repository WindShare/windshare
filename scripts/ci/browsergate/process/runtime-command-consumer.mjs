import { Buffer } from 'node:buffer'
import { types as nodeTypes } from 'node:util'

import { requireRuntimeCommandTraceSnapshot } from './runtime-command-owner.mjs'

const RUNTIME_COMMAND_OUTPUT_MAXIMUM_BYTES = 16 * 1024 * 1024
const EXECUTION_MAXIMUM_ENTRIES = 4_096
const EXECUTION_MAXIMUM_DEPTH = 16
// The consumer admits both independently bounded output streams plus a fixed
// evidence reserve, so malformed metadata cannot turn copying into an unbounded cost.
const EXECUTION_MAXIMUM_STRING_BYTES =
  (2 * RUNTIME_COMMAND_OUTPUT_MAXIMUM_BYTES) + (1 * 1024 * 1024)
const EXECUTION_MAXIMUM_KEY_CHARACTERS = 128
const EXECUTION_MAXIMUM_KEY_BYTES = 512
const OPERATION_ID_MAXIMUM_BYTES = 128
const PLATFORM_ID_MAXIMUM_BYTES = 32
const EXECUTION_FIELDS = Object.freeze([
  'cleanupOutcome',
  'inputEvidence',
  'ownershipEvidence',
  'processEvidence',
  'stderr',
  'stdout',
  'treeEmpty',
])
const CONSUMPTION_FAILURE_KIND = Object.freeze({
  CONTRACT_REJECTED: 'runtime-command-contract-failed',
  OWNERSHIP_REJECTED: 'process-tree-not-empty',
  RUNTIME_REJECTED: 'runtime-command-failed',
})

/**
 * The process owner and its trace view settle independently. Consuming both in
 * one module prevents callers from treating arbitrary rejection objects as
 * evidence or observing a successful payload before its lifecycle is complete.
 */
export async function consumeOwnedRuntimeCommandExecution(value, identity) {
  const canonicalIdentity = requireRuntimeCommandIdentity(identity)
  let channel
  try {
    channel = requireRuntimeCommandExecutionChannel(value)
  } catch (cause) {
    return rejectedConsumption({
      failure: opaqueFailure(cause, 'runtime command execution channel was rejected'),
      failureKind: CONSUMPTION_FAILURE_KIND.CONTRACT_REJECTED,
      traces: null,
    })
  }

  let rawExecution
  try {
    rawExecution = await channel.result
  } catch (cause) {
    const operationFailure = opaqueFailure(cause, 'runtime command execution was rejected')
    try {
      const traces = pullSettledRuntimeCommandTraces(channel, canonicalIdentity, 'rejected')
      return rejectedConsumption({
        failure: operationFailure,
        failureKind: runtimeCommandFailureKind(traces),
        traces,
      })
    } catch (traceCause) {
      return rejectedConsumption({
        failure: aggregateOpaqueFailures(
          operationFailure,
          opaqueFailure(traceCause, 'runtime command trace settlement was rejected'),
        ),
        failureKind: CONSUMPTION_FAILURE_KIND.CONTRACT_REJECTED,
        traces: null,
      })
    }
  }

  let traces = null
  try {
    traces = pullSettledRuntimeCommandTraces(channel, canonicalIdentity, 'fulfilled')
    const execution = snapshotOwnedRuntimeExecution(rawExecution)
    requireRuntimeCommandResultTraceAgreement(execution, traces)
    return Object.freeze({
      outcome: 'fulfilled',
      execution,
      traces,
      failure: null,
      failureKind: null,
    })
  } catch (cause) {
    return rejectedConsumption({
      failure: opaqueFailure(cause, 'runtime command fulfilled evidence was rejected'),
      failureKind: CONSUMPTION_FAILURE_KIND.CONTRACT_REJECTED,
      traces,
    })
  }
}

function requireRuntimeCommandExecutionChannel(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command execution channel')
  requireExactDataDescriptors(
    descriptors,
    ['result', 'traces'],
    'runtime command execution channel',
  )
  if (!Object.isFrozen(value)) {
    throw new Error('runtime command execution channel must be immutable')
  }
  const result = descriptors.result.value
  if (!nodeTypes.isPromise(result) || Object.getPrototypeOf(result) !== Promise.prototype) {
    throw new Error('runtime command execution channel result must be an intrinsic promise')
  }
  const traceView = descriptors.traces.value
  const traceDescriptors = requireInertRecordDescriptors(traceView, 'runtime command trace view')
  requireExactDataDescriptors(traceDescriptors, ['snapshot'], 'runtime command trace view')
  const snapshot = traceDescriptors.snapshot.value
  if (
    !Object.isFrozen(traceView) || typeof snapshot !== 'function' ||
    nodeTypes.isProxy(snapshot)
  ) throw new Error('runtime command trace view must be one immutable pull authority')

  // Await consumes this exact native Promise through its internal slots; a
  // subclass or Proxy could otherwise reintroduce a user-controlled then path.
  return Object.freeze({
    result,
    snapshot: () => Reflect.apply(snapshot, traceView, []),
  })
}

function pullSettledRuntimeCommandTraces(channel, identity, resultOutcome) {
  const traces = requireRuntimeCommandTraceSnapshot(channel.snapshot(), identity)
  const terminalOutcome = terminalTraceOutcome(traces)
  if (
    (resultOutcome === 'fulfilled' && terminalOutcome !== 'succeeded') ||
    (resultOutcome === 'rejected' && terminalOutcome === 'succeeded')
  ) throw new Error('runtime command result and terminal trace outcomes disagree')
  return traces
}

function snapshotOwnedRuntimeExecution(value) {
  const budget = {
    entries: EXECUTION_MAXIMUM_ENTRIES,
    stringBytes: EXECUTION_MAXIMUM_STRING_BYTES,
  }
  const execution = snapshotRuntimeValue(value, budget, new WeakSet(), 0)
  requireExactDataFields(execution, EXECUTION_FIELDS, 'runtime command execution')
  if (
    execution.treeEmpty !== true || execution.cleanupOutcome !== 'completed' ||
    typeof execution.stdout !== 'string' || typeof execution.stderr !== 'string' ||
    !isDataRecord(execution.processEvidence) ||
    !isDataRecord(execution.inputEvidence) ||
    !isDataRecord(execution.ownershipEvidence) ||
    typeof execution.processEvidence.terminal !== 'string' ||
    typeof execution.ownershipEvidence.terminationReason !== 'string'
  ) throw new Error('runtime command execution lacks canonical settled evidence')
  if (
    execution.processEvidence.terminal === 'exited' &&
    (!Number.isSafeInteger(execution.processEvidence.exitCode) ||
      execution.processEvidence.exitCode < 0)
  ) throw new Error('runtime command execution exit evidence is invalid')
  return execution
}

function snapshotRuntimeValue(value, budget, visiting, depth) {
  if (value === null || typeof value === 'boolean') return value
  if (typeof value === 'string') {
    consumeStringBytes(budget, value, 'runtime command execution string evidence')
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) {
      throw new Error('runtime command execution number evidence is invalid')
    }
    return value
  }
  if (typeof value !== 'object' || nodeTypes.isProxy(value)) {
    throw new Error('runtime command execution contains non-inert evidence')
  }
  if (depth >= EXECUTION_MAXIMUM_DEPTH) {
    throw new Error('runtime command execution evidence exceeded its maximum depth')
  }
  if (visiting.has(value)) throw new Error('runtime command execution evidence contains a cycle')
  visiting.add(value)
  try {
    const prototype = Object.getPrototypeOf(value)
    if (Array.isArray(value)) {
      if (prototype !== Array.prototype) {
        throw new Error('runtime command execution array evidence is not inert')
      }
      return snapshotRuntimeArray(value, budget, visiting, depth)
    }
    if (prototype !== Object.prototype && prototype !== null) {
      throw new Error('runtime command execution record evidence is not inert')
    }
    return snapshotRuntimeRecord(value, budget, visiting, depth)
  } finally {
    visiting.delete(value)
  }
}

function snapshotRuntimeArray(value, budget, visiting, depth) {
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  const length = descriptors.length?.value
  if (
    !Number.isSafeInteger(length) || length < 0 ||
    names.some((name) => typeof name !== 'string') ||
    names.length !== length + 1
  ) throw new Error('runtime command execution array evidence is sparse or decorated')
  consumeEntries(budget, length)
  const result = new Array(length)
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    requireEnumerableDataDescriptor(descriptor, 'runtime command execution array entry')
    result[index] = snapshotRuntimeValue(descriptor.value, budget, visiting, depth + 1)
  }
  return Object.freeze(result)
}

function snapshotRuntimeRecord(value, budget, visiting, depth) {
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('runtime command execution record contains symbol fields')
  }
  consumeEntries(budget, names.length)
  const result = Object.create(null)
  for (const name of names.sort()) {
    if (
      name.length > EXECUTION_MAXIMUM_KEY_CHARACTERS ||
      Buffer.byteLength(name, 'utf8') > EXECUTION_MAXIMUM_KEY_BYTES
    ) throw new Error('runtime command execution record key exceeded its bounded capacity')
    consumeStringBytes(budget, name, 'runtime command execution record key')
    const descriptor = descriptors[name]
    requireEnumerableDataDescriptor(descriptor, 'runtime command execution record field')
    Object.defineProperty(result, name, {
      value: snapshotRuntimeValue(descriptor.value, budget, visiting, depth + 1),
      enumerable: true,
      writable: false,
      configurable: false,
    })
  }
  return Object.freeze(result)
}

function consumeStringBytes(budget, value, label) {
  const bytes = Buffer.byteLength(value, 'utf8')
  if (bytes > budget.stringBytes) throw new Error(`${label} exceeded its bounded capacity`)
  budget.stringBytes -= bytes
}

function consumeEntries(budget, count) {
  if (count > budget.entries) {
    throw new Error('runtime command execution evidence exceeded its entry capacity')
  }
  budget.entries -= count
}

function requireRuntimeCommandIdentity(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command trace identity')
  requireExactDataDescriptors(descriptors, ['operationId', 'platform'], 'runtime command trace identity')
  const operationId = descriptors.operationId.value
  const platform = descriptors.platform.value
  if (
    typeof operationId !== 'string' || operationId.length === 0 ||
    Buffer.byteLength(operationId, 'utf8') > OPERATION_ID_MAXIMUM_BYTES ||
    typeof platform !== 'string' || platform.length === 0 ||
    Buffer.byteLength(platform, 'utf8') > PLATFORM_ID_MAXIMUM_BYTES
  ) throw new Error('runtime command trace identity is invalid')
  return Object.freeze({ operationId, platform })
}

function requireInertRecordDescriptors(value, label) {
  if (
    value === null || typeof value !== 'object' || nodeTypes.isProxy(value) ||
    Array.isArray(value)
  ) throw new Error(`${label} must be an inert data record`)
  try {
    const prototype = Object.getPrototypeOf(value)
    if (prototype !== Object.prototype && prototype !== null) {
      throw new Error(`${label} must be an inert data record`)
    }
    return Object.getOwnPropertyDescriptors(value)
  } catch {
    throw new Error(`${label} must be an inert data record`)
  }
}

function requireExactDataDescriptors(descriptors, fields, label) {
  const names = Reflect.ownKeys(descriptors)
  const expected = [...fields].sort()
  if (
    names.some((name) => typeof name !== 'string') ||
    names.length !== expected.length ||
    [...names].sort().some((name, index) => name !== expected[index])
  ) throw new Error(`${label} has an invalid evidence shape`)
  for (const name of names) {
    requireEnumerableDataDescriptor(descriptors[name], `${label} field`)
  }
}

function requireExactDataFields(value, fields, label) {
  if (!isDataRecord(value)) throw new Error(`${label} must be one data record`)
  const names = Object.keys(value).sort()
  const expected = [...fields].sort()
  if (
    names.length !== expected.length ||
    names.some((name, index) => name !== expected[index])
  ) throw new Error(`${label} has an invalid evidence shape`)
}

function requireEnumerableDataDescriptor(descriptor, label) {
  if (
    descriptor === undefined || !Object.hasOwn(descriptor, 'value') ||
    descriptor.enumerable !== true
  ) throw new Error(`${label} must be enumerable inert data`)
}

function isDataRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function requireRuntimeCommandResultTraceAgreement(execution, traces) {
  const settlement = traces.events.at(-1).context.settlement
  for (const field of [
    'processEvidence',
    'inputEvidence',
    'treeEmpty',
    'cleanupOutcome',
    'ownershipEvidence',
  ]) {
    if (JSON.stringify(execution[field]) !== JSON.stringify(settlement[field])) {
      throw new Error('runtime command result differs from its terminal trace settlement')
    }
  }
}

function runtimeCommandFailureKind(traces) {
  return terminalTraceOutcome(traces) === 'ownership-rejected'
    ? CONSUMPTION_FAILURE_KIND.OWNERSHIP_REJECTED
    : CONSUMPTION_FAILURE_KIND.RUNTIME_REJECTED
}

function terminalTraceOutcome(traces) {
  return traces.events.at(-1).outcomeCode
}

function opaqueFailure(cause, message) {
  return Object.freeze(new Error(message, { cause }))
}

function aggregateOpaqueFailures(operationFailure, traceFailure) {
  return Object.freeze(new AggregateError(
    [operationFailure, traceFailure],
    'runtime command operation and trace settlement both failed',
  ))
}

function rejectedConsumption({ failure, failureKind, traces }) {
  return Object.freeze({
    outcome: 'rejected',
    execution: null,
    traces,
    failure,
    failureKind,
  })
}
