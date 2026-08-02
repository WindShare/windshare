import { accessSync, constants as fsConstants, lstatSync, realpathSync } from 'node:fs'
import { isAbsolute, delimiter, resolve } from 'node:path'
import { types as nodeTypes } from 'node:util'

import {
  executeTestProcessOwner,
  testProcessOwnerFailureEvidence,
} from '../../../../web/scripts/browser-evidence/process/test-process-owner-client.mjs'
import {
  isProcessOwnerReservedEnvironmentName,
  parseTestIdentity,
} from '../../../../web/scripts/browser-evidence/process/test-identity.mjs'
import {
  createOwnedTraceJournal,
  requireCompleteOwnedTraceSnapshot,
} from '../owned-trace-journal.mjs'

const MAXIMUM_CAPTURE_BYTES = 16 * 1024 * 1024
const MAXIMUM_RUNTIME_TRACE_EVENTS = 2
const MAXIMUM_RUNTIME_TRACE_BYTES = 256 * 1024
const MAXIMUM_SETTLEMENT_SNAPSHOT_BYTES = 16 * 1024 * 1024
const UNBOUND_OPERATION_ID = 'unbound-request'
const UNBOUND_PLATFORM = 'unbound'
const TRACE_START_MILESTONE = 'runtime-command-started'
const TRACE_TERMINAL_MILESTONE = 'runtime-command-terminal'
const TRACE_STARTED_OUTCOME = 'started'
const TERMINAL_OUTCOME_CODES = Object.freeze([
  'succeeded',
  'request-rejected',
  'execution-rejected',
  'output-rejected',
  'ownership-rejected',
])
const SETTLEMENT_OUTCOMES = Object.freeze(['completed', 'failed', 'not-observed'])
const REQUEST_FIELDS = Object.freeze([
  'command',
  'deadlineMs',
  'executeOwner',
  'inheritedEnvironment',
  'operationId',
  'platform',
  'processOwner',
  'runId',
  'scenario',
  'terminationGraceMs',
  'terminationSignal',
])
const COMMAND_FIELDS = Object.freeze(['arguments', 'cwd', 'executable', 'stdin'])
const PROCESS_OWNER_FIELDS = Object.freeze(['path'])
const OUTPUT_PAIR_FIELDS = Object.freeze(['stderr', 'stdout'])
const OUTPUT_SNAPSHOT_FIELDS = Object.freeze([
  'bytes',
  'capturedBytes',
  'completed',
  'observedBytes',
  'truncated',
])
const EXECUTION_FIELDS = new Set([
  'cleanupOutcome',
  'events',
  'inputEvidence',
  'ownerFailure',
  'output',
  'ownershipEvidence',
  'processEvidence',
  'startEvidence',
  'treeEmpty',
])
const TRACE_EVENT_FIELDS = Object.freeze([
  'context',
  'milestone',
  'outcomeCode',
  'schemaVersion',
  'sequence',
])
const TRACE_START_CONTEXT_FIELDS = Object.freeze(['operationId', 'platform'])
const TRACE_TERMINAL_CONTEXT_FIELDS = Object.freeze(['operationId', 'platform', 'settlement'])
const SETTLEMENT_FIELDS = Object.freeze([
  'cleanupOutcome',
  'controlOutcome',
  'inputEvidence',
  'ownerFailure',
  'ownershipEvidence',
  'processEvidence',
  'transportEvidence',
  'transportOutcome',
  'treeEmpty',
])

export const RUNTIME_COMMAND_TRACE_SCHEMA_VERSION =
  'windshare.browsergate.runtime-command-trace/v1'
export const RUNTIME_COMMAND_TRACE_OUTCOME_CODES = TERMINAL_OUTCOME_CODES

const outputErrors = new WeakSet()
const ownershipErrors = new WeakSet()
const typedArrayPrototype = Object.getPrototypeOf(Uint8Array.prototype)
const typedArrayByteLength = Object.getOwnPropertyDescriptor(typedArrayPrototype, 'byteLength').get
const abortSignalAborted = Object.getOwnPropertyDescriptor(AbortSignal.prototype, 'aborted').get

export class RuntimeCommandOutputError extends Error {
  constructor(stream, message, evidence, cause) {
    super(message, cause === undefined ? undefined : { cause })
    this.name = 'RuntimeCommandOutputError'
    this.stream = stream
    this.evidence = evidence
    outputErrors.add(this)
  }
}

export class RuntimeCommandOwnershipError extends Error {
  constructor(operationId, settlement) {
    super(`runtime command ownership did not settle an empty tree for ${operationId}`)
    this.name = 'RuntimeCommandOwnershipError'
    this.operationId = operationId
    this.settlement = settlement
    ownershipErrors.add(this)
  }
}

/**
 * The producer starts immediately but exposes no convenience Promise masquerading
 * as a settled execution. Callers must await result, then pull the independently
 * owned trace; this ordering prevents diagnostic consumers from entering the
 * operation or changing which failure is primary.
 */
export function executeOwnedRuntimeCommand(request) {
  const preparation = prepareRuntimeRequest(request)
  const traceOwner = createRuntimeCommandTraceOwner(preparation.identity)
  const result = runOwnedRuntimeCommand(preparation, traceOwner)
  return Object.freeze({ result, traces: traceOwner.view })
}

async function runOwnedRuntimeCommand(preparation, traceOwner) {
  let settlement = emptyRuntimeSettlement()
  let hasObservedSettlement = false
  let result
  try {
    if (preparation.failure !== undefined) throw preparation.failure
    const request = preparation.request
    const rawExecution = await request.executeOwner(request.ownerRequest)
    const execution = snapshotOwnedExecution(rawExecution)
    settlement = execution.settlement
    hasObservedSettlement = true
    assertRuntimeArtifactLive(request.processOwner, 'test process owner')
    const output = ownedCommandOutput(snapshotOutputPair(execution.output))
    const ownershipSettled = settlement.treeEmpty === true &&
      settlement.cleanupOutcome === 'completed' && settlement.ownerFailure === null
    if (!ownershipSettled) {
      throw new RuntimeCommandOwnershipError(request.operationId, settlement)
    }
    result = Object.freeze({
      processEvidence: settlement.processEvidence,
      treeEmpty: settlement.treeEmpty,
      cleanupOutcome: settlement.cleanupOutcome,
      inputEvidence: settlement.inputEvidence,
      ownershipEvidence: settlement.ownershipEvidence,
      stdout: output.stdout,
      stderr: output.stderr,
    })
  } catch (operationFailure) {
    const outcomeCode = preparation.failure !== undefined
      ? 'request-rejected'
      : runtimeFailureOutcome(operationFailure)
    if (!hasObservedSettlement) {
      settlement = settlementFromTestProcessOwnerFailure(operationFailure) ?? emptyRuntimeSettlement()
    }
    try {
      traceOwner.settle(outcomeCode, settlement)
    } catch (traceFailure) {
      throw new AggregateError(
        [operationFailure, traceFailure],
        'runtime command operation and trace settlement both failed',
      )
    }
    throw operationFailure
  }
  traceOwner.settle('succeeded', settlement)
  return result
}

function prepareRuntimeRequest(value) {
  let descriptors
  try {
    descriptors = requireInertRecordDescriptors(value, 'runtime command request')
  } catch (failure) {
    return Object.freeze({
      identity: fallbackTraceIdentity(),
      failure,
      request: undefined,
    })
  }
  const identity = traceIdentityFromDescriptors(descriptors)
  try {
    const request = snapshotRuntimeRequest(descriptors)
    return Object.freeze({ identity, failure: undefined, request })
  } catch (failure) {
    return Object.freeze({ identity, failure, request: undefined })
  }
}

function snapshotRuntimeRequest(descriptors) {
  requireEnumerableDataDescriptors(descriptors, 'runtime command request field')
  requireAllowedFields(descriptors, REQUEST_FIELDS, 'runtime command request')
  const operationIdValue = requiredField(descriptors, 'operationId', 'runtime command request')
  const command = snapshotCommand(requiredField(descriptors, 'command', 'runtime command request'))
  const platform = optionalField(descriptors, 'platform', process.platform)
  if (platform === 'darwin') {
    throw new Error('Darwin runtime process ownership is unsupported without a descendant authority')
  }
  if (!['win32', 'linux'].includes(platform)) {
    throw new Error(`unsupported runtime command platform ${JSON.stringify(platform)}`)
  }
  const processOwnerValue = optionalField(descriptors, 'processOwner', undefined)
  if (processOwnerValue === undefined) {
    throw new Error(`${platform} runtime command requires the authenticated test process owner`)
  }
  const processOwner = snapshotProcessOwner(processOwnerValue)
  assertRuntimeArtifactLive(processOwner, 'test process owner')
  const inheritedEnvironment = canonicalEnvironment(
    requiredField(descriptors, 'inheritedEnvironment', 'runtime command request'),
  )
  const deadlineMs = requiredField(descriptors, 'deadlineMs', 'runtime command request')
  const terminationGraceMs = requiredField(
    descriptors,
    'terminationGraceMs',
    'runtime command request',
  )
  requirePositiveInteger(deadlineMs, 'runtime command deadline')
  requirePositiveInteger(terminationGraceMs, 'runtime command termination grace')
  const terminationSignal = optionalField(descriptors, 'terminationSignal', undefined)
  requireTerminationSignal(terminationSignal)
  const identity = parseTestIdentity({
    runId: optionalField(descriptors, 'runId', 'browsergate'),
    operationId: operationIdValue,
    scenario: optionalField(descriptors, 'scenario', 'browsergate-runtime-command'),
  })
  const { runId, operationId, scenario } = identity
  const executeOwner = optionalField(descriptors, 'executeOwner', executeTestProcessOwner)
  if (typeof executeOwner !== 'function' || nodeTypes.isProxy(executeOwner)) {
    throw new Error('runtime command execution owner must be one inert callable authority')
  }
  const ownerRequest = Object.freeze({
    owner: Object.freeze({ path: processOwner.path }),
    runId,
    operationId,
    scenario,
    command,
    environment: inheritedEnvironment,
    deadlineMs,
    terminationGraceMs,
    ...(terminationSignal === undefined ? {} : { terminationSignal }),
    platform,
    capture: Object.freeze({
      stdoutBytes: MAXIMUM_CAPTURE_BYTES,
      stderrBytes: MAXIMUM_CAPTURE_BYTES,
      eventCount: 0,
    }),
  })
  return Object.freeze({
    operationId,
    platform,
    processOwner,
    executeOwner,
    ownerRequest,
  })
}

function snapshotCommand(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command')
  requireEnumerableDataDescriptors(descriptors, 'runtime command field')
  requireAllowedFields(descriptors, COMMAND_FIELDS, 'runtime command')
  for (const name of ['executable', 'arguments', 'cwd']) {
    if (!Object.hasOwn(descriptors, name)) throw new Error(`runtime command ${name} is required`)
  }
  const executable = descriptors.executable.value
  const cwd = descriptors.cwd.value
  if (typeof executable !== 'string' || !isAbsolute(executable) || resolve(executable) !== executable) {
    throw new Error('runtime command executable must be absolute and canonical')
  }
  if (typeof cwd !== 'string' || !isAbsolute(cwd) || resolve(cwd) !== cwd) {
    throw new Error('runtime command working directory must be absolute and canonical')
  }
  const argumentsSnapshot = snapshotStringArray(descriptors.arguments.value, 'runtime command arguments')
  const stdin = Object.hasOwn(descriptors, 'stdin')
    ? snapshotInput(descriptors.stdin.value)
    : undefined
  return Object.freeze({
    executable,
    arguments: argumentsSnapshot,
    cwd,
    ...(stdin === undefined ? {} : { stdin }),
  })
}

function snapshotProcessOwner(value) {
  const descriptors = requireInertRecordDescriptors(value, 'test process owner authority')
  requireEnumerableDataDescriptors(descriptors, 'test process owner authority field')
  requireExactFields(descriptors, PROCESS_OWNER_FIELDS, 'test process owner authority')
  const path = descriptors.path.value
  if (typeof path !== 'string' || !isAbsolute(path) || resolve(path) !== path) {
    throw new Error('test process owner authority path is invalid')
  }
  const identity = assertRuntimeArtifactLive({ path }, 'test process owner')
  return Object.freeze({ path, identity })
}

function snapshotStringArray(value, label) {
  if (value === null || typeof value !== 'object' || nodeTypes.isProxy(value) || !Array.isArray(value)) {
    throw new Error(`${label} must be an inert string array`)
  }
  if (Object.getPrototypeOf(value) !== Array.prototype) {
    throw new Error(`${label} must be an inert string array`)
  }
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const length = descriptors.length?.value
  if (!Number.isSafeInteger(length) || length < 0 || Reflect.ownKeys(descriptors).length !== length + 1) {
    throw new Error(`${label} must be a dense inert string array`)
  }
  const result = new Array(length)
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    if (!isEnumerableDataDescriptor(descriptor) || typeof descriptor.value !== 'string' ||
        descriptor.value.includes('\0')) {
      throw new Error(`${label} must contain only inert strings`)
    }
    result[index] = descriptor.value
  }
  return Object.freeze(result)
}

function snapshotInput(value) {
  if (nodeTypes.isProxy(value) || !nodeTypes.isUint8Array(value)) {
    throw new Error('runtime command stdin is invalid')
  }
  let byteLength
  try {
    byteLength = Reflect.apply(typedArrayByteLength, value, [])
  } catch {
    throw new Error('runtime command stdin is invalid')
  }
  if (byteLength < 1 || byteLength > 1_048_576) throw new Error('runtime command stdin is invalid')
  const snapshot = new Uint8Array(byteLength)
  Reflect.apply(Uint8Array.prototype.set, snapshot, [value])
  return snapshot
}

function requireTerminationSignal(value) {
  if (value === undefined) return
  if (
    value === null || typeof value !== 'object' || nodeTypes.isProxy(value) ||
    Object.getPrototypeOf(value) !== AbortSignal.prototype
  ) throw new Error('runtime command termination signal is invalid')
  try {
    Reflect.apply(abortSignalAborted, value, [])
  } catch {
    throw new Error('runtime command termination signal is invalid')
  }
}

function snapshotOwnedExecution(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command execution')
  requireEnumerableDataDescriptors(descriptors, 'runtime command execution field')
  for (const name of ['processEvidence', 'treeEmpty', 'cleanupOutcome', 'inputEvidence', 'output', 'ownershipEvidence']) {
    if (!Object.hasOwn(descriptors, name)) throw new Error(`runtime command execution omitted ${name}`)
  }
  for (const name of Reflect.ownKeys(descriptors)) {
    if (typeof name !== 'string' || !EXECUTION_FIELDS.has(name)) {
      throw new Error('runtime command execution contains unsupported evidence')
    }
  }
  const settlement = snapshotSettlementRecord({
    processEvidence: descriptors.processEvidence.value,
    inputEvidence: descriptors.inputEvidence.value,
    ownerFailure: Object.hasOwn(descriptors, 'ownerFailure')
      ? descriptors.ownerFailure.value
      : null,
    treeEmpty: descriptors.treeEmpty.value,
    cleanupOutcome: descriptors.cleanupOutcome.value,
    ownershipEvidence: descriptors.ownershipEvidence.value,
    transportOutcome: 'completed',
    controlOutcome: 'completed',
    transportEvidence: null,
  })
  return Object.freeze({
    settlement,
    output: descriptors.output.value,
  })
}

function settlementFromTestProcessOwnerFailure(cause) {
  const failure = testProcessOwnerFailureEvidence(cause)
  if (failure === undefined) return undefined
  try {
    const base = failure.settlement === undefined
      ? emptyRuntimeSettlement()
      : snapshotOwnedExecution(failure.settlement).settlement
    const transportEvidence = snapshotPortableValue(
      failure.transportEvidence,
      'test process owner failure evidence',
    )
    if (failure.kind === 'transport-failed') {
      return snapshotSettlementRecord({
        ...base,
        transportOutcome: 'failed',
        controlOutcome: 'not-observed',
        transportEvidence,
      })
    }
    if (failure.kind === 'control-failed' && failure.settlement !== undefined) {
      return snapshotSettlementRecord({
        ...base,
        transportOutcome: 'completed',
        controlOutcome: 'failed',
        transportEvidence,
      })
    }
  } catch {
    // A malformed branded error must not replace the operation failure or leave
    // its independently owned terminal trace unavailable.
  }
  return undefined
}

function snapshotOutputPair(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command output')
  requireEnumerableDataDescriptors(descriptors, 'runtime command output field')
  requireExactFields(descriptors, OUTPUT_PAIR_FIELDS, 'runtime command output')
  return Object.freeze({
    stdout: snapshotOutputChannel(descriptors.stdout.value, 'stdout'),
    stderr: snapshotOutputChannel(descriptors.stderr.value, 'stderr'),
  })
}

function snapshotOutputChannel(value, stream) {
  const label = `runtime command ${stream}`
  let descriptors
  try {
    descriptors = requireInertRecordDescriptors(value, `${label} snapshot`)
    requireEnumerableDataDescriptors(descriptors, `${label} snapshot field`)
    requireExactFields(descriptors, OUTPUT_SNAPSHOT_FIELDS, `${label} snapshot`)
  } catch {
    throw new RuntimeCommandOutputError(
      stream,
      `${label} snapshot is unavailable`,
      emptyOutputEvidence(stream),
    )
  }
  const observedBytes = descriptors.observedBytes.value
  const capturedBytes = descriptors.capturedBytes.value
  const truncated = descriptors.truncated.value
  const completed = descriptors.completed.value
  const bytesReader = descriptors.bytes.value
  const metadataValid = Number.isSafeInteger(observedBytes) && observedBytes >= 0 &&
    Number.isSafeInteger(capturedBytes) && capturedBytes >= 0 &&
    typeof truncated === 'boolean' && typeof completed === 'boolean' &&
    typeof bytesReader === 'function' && !nodeTypes.isProxy(bytesReader)
  if (!metadataValid) {
    throw new RuntimeCommandOutputError(
      stream,
      `${label} snapshot metadata is invalid`,
      outputEvidence(stream, observedBytes, capturedBytes, truncated, completed),
    )
  }
  let bytes
  try {
    bytes = snapshotOutputBytes(Reflect.apply(bytesReader, value, []))
  } catch (cause) {
    throw new RuntimeCommandOutputError(
      stream,
      `${label} snapshot bytes are unavailable`,
      outputEvidence(stream, observedBytes, capturedBytes, truncated, completed),
      cause,
    )
  }
  const evidence = outputEvidence(
    stream,
    observedBytes,
    capturedBytes,
    truncated,
    completed,
    bytes,
  )
  if (!completed) throw new RuntimeCommandOutputError(stream, `${label} snapshot is incomplete`, evidence)
  if (truncated || observedBytes !== capturedBytes || capturedBytes > MAXIMUM_CAPTURE_BYTES) {
    throw new RuntimeCommandOutputError(stream, `${label} snapshot is truncated`, evidence)
  }
  if (bytes.byteLength !== capturedBytes) {
    throw new RuntimeCommandOutputError(stream, `${label} snapshot byte length is invalid`, evidence)
  }
  try {
    return Object.freeze({ text: new TextDecoder('utf-8', { fatal: true }).decode(bytes), evidence })
  } catch (cause) {
    throw new RuntimeCommandOutputError(stream, `${label} is not valid UTF-8`, evidence, cause)
  }
}

function snapshotOutputBytes(value) {
  if (nodeTypes.isProxy(value) || !nodeTypes.isUint8Array(value)) {
    throw new Error('owned output bytes are invalid')
  }
  const byteLength = Reflect.apply(typedArrayByteLength, value, [])
  const result = new Uint8Array(byteLength)
  Reflect.apply(Uint8Array.prototype.set, result, [value])
  return result
}

function ownedCommandOutput(output) {
  return Object.freeze({ stdout: output.stdout.text, stderr: output.stderr.text })
}

function outputEvidence(
  stream,
  observedBytes,
  capturedBytes,
  truncated,
  completed,
  bytes,
) {
  const canonicalObserved = nonnegativeSafeInteger(observedBytes) ? observedBytes : null
  const canonicalCaptured = nonnegativeSafeInteger(capturedBytes) ? capturedBytes : null
  return Object.freeze({
    stream,
    observedBytes: canonicalObserved,
    capturedBytes: canonicalCaptured,
    truncated: typeof truncated === 'boolean' ? truncated : null,
    completed: typeof completed === 'boolean' ? completed : null,
    segments: Object.freeze(bytes === undefined
      ? []
      : [Object.freeze({
          sequence: 0,
          offset: 0,
          byteLength: bytes.byteLength,
          base64: Buffer.from(bytes).toString('base64'),
        })]),
  })
}

function emptyOutputEvidence(stream) {
  return outputEvidence(stream, undefined, undefined, undefined, undefined, undefined)
}

function snapshotSettlementRecord(value) {
  const canonical = snapshotPortableValue(value, 'runtime command settlement')
  requireRuntimeSettlement(canonical)
  return canonical
}

function snapshotPortableValue(value, label) {
  const journal = createOwnedTraceJournal({
    label,
    maximumEvents: 1,
    maximumBytes: MAXIMUM_SETTLEMENT_SNAPSHOT_BYTES,
  })
  const appended = journal.append({ value })
  journal.finish()
  const snapshot = requireCompleteOwnedTraceSnapshot(journal.view.snapshot(), label)
  if (!appended || snapshot.events.length !== 1) throw new Error(`${label} is unavailable`)
  return snapshot.events[0].value
}

function emptyRuntimeSettlement({
  transportOutcome = 'not-observed',
  controlOutcome = 'not-observed',
  transportEvidence = null,
} = {}) {
  return Object.freeze({
    processEvidence: null,
    inputEvidence: null,
    ownerFailure: null,
    treeEmpty: null,
    cleanupOutcome: 'not-observed',
    ownershipEvidence: null,
    transportOutcome,
    controlOutcome,
    transportEvidence,
  })
}

function runtimeFailureOutcome(cause) {
  if (outputErrors.has(cause)) return 'output-rejected'
  if (ownershipErrors.has(cause)) return 'ownership-rejected'
  return 'execution-rejected'
}

function createRuntimeCommandTraceOwner(identity) {
  const journal = createOwnedTraceJournal({
    label: 'runtime command ownership trace',
    maximumEvents: MAXIMUM_RUNTIME_TRACE_EVENTS,
    maximumBytes: MAXIMUM_RUNTIME_TRACE_BYTES,
  })
  let settled = false
  let snapshot
  appendTraceEvent(journal, runtimeTraceEvent(0, TRACE_START_MILESTONE, TRACE_STARTED_OUTCOME, {
    operationId: identity.operationId,
    platform: identity.platform,
  }))
  const view = Object.freeze({
    snapshot() {
      if (!settled) throw new Error('runtime command ownership trace is unavailable before result settlement')
      return snapshot
    },
  })
  const settle = (outcomeCode, settlement) => {
    if (settled) throw new Error('runtime command ownership trace already settled')
    try {
      appendTraceEvent(journal, runtimeTraceEvent(1, TRACE_TERMINAL_MILESTONE, outcomeCode, {
        operationId: identity.operationId,
        platform: identity.platform,
        settlement,
      }))
    } finally {
      journal.finish()
      snapshot = journal.view.snapshot()
      settled = true
    }
    return requireRuntimeCommandTraceSnapshot(snapshot, identity)
  }
  return Object.freeze({ view, settle })
}

function runtimeTraceEvent(sequence, milestone, outcomeCode, context) {
  return Object.freeze({
    schemaVersion: RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
    sequence,
    milestone,
    outcomeCode,
    context: Object.freeze(context),
  })
}

function appendTraceEvent(journal, event) {
  if (!journal.append(event)) throw new Error('runtime command ownership trace rejected a lifecycle event')
}

export function requireRuntimeCommandTraceSnapshot(snapshot, identity) {
  const expectedIdentity = requireTraceValidationIdentity(identity)
  const canonical = requireCompleteOwnedTraceSnapshot(snapshot, 'runtime command ownership trace')
  if (
    canonical.events.length !== MAXIMUM_RUNTIME_TRACE_EVENTS ||
    canonical.capturedEvents !== MAXIMUM_RUNTIME_TRACE_EVENTS ||
    canonical.observedEvents !== MAXIMUM_RUNTIME_TRACE_EVENTS
  ) throw new Error('runtime command ownership trace lifecycle cardinality is invalid')
  const [started, terminal] = canonical.events
  requireExactObjectFields(started, TRACE_EVENT_FIELDS, 'runtime command start trace')
  requireExactObjectFields(terminal, TRACE_EVENT_FIELDS, 'runtime command terminal trace')
  requireExactObjectFields(started.context, TRACE_START_CONTEXT_FIELDS, 'runtime command start context')
  requireExactObjectFields(
    terminal.context,
    TRACE_TERMINAL_CONTEXT_FIELDS,
    'runtime command terminal context',
  )
  if (
    started.schemaVersion !== RUNTIME_COMMAND_TRACE_SCHEMA_VERSION ||
    terminal.schemaVersion !== RUNTIME_COMMAND_TRACE_SCHEMA_VERSION ||
    started.sequence !== 0 || terminal.sequence !== 1 ||
    started.milestone !== TRACE_START_MILESTONE ||
    terminal.milestone !== TRACE_TERMINAL_MILESTONE ||
    started.outcomeCode !== TRACE_STARTED_OUTCOME ||
    !TERMINAL_OUTCOME_CODES.includes(terminal.outcomeCode) ||
    started.context.operationId !== expectedIdentity.operationId ||
    terminal.context.operationId !== expectedIdentity.operationId ||
    started.context.platform !== expectedIdentity.platform ||
    terminal.context.platform !== expectedIdentity.platform
  ) throw new Error('runtime command ownership trace lifecycle sequence is invalid')
  requireRuntimeSettlement(terminal.context.settlement)
  if (
    terminal.outcomeCode === 'succeeded' &&
    (terminal.context.settlement.treeEmpty !== true ||
      terminal.context.settlement.cleanupOutcome !== 'completed' ||
      terminal.context.settlement.ownerFailure !== null ||
      terminal.context.settlement.transportOutcome !== 'completed' ||
      terminal.context.settlement.controlOutcome !== 'completed')
  ) throw new Error('successful runtime command trace has an invalid settlement')
  return canonical
}

function requireTraceValidationIdentity(value) {
  const descriptors = requireInertRecordDescriptors(value, 'runtime command trace identity')
  requireEnumerableDataDescriptors(descriptors, 'runtime command trace identity field')
  requireExactFields(descriptors, ['operationId', 'platform'], 'runtime command trace identity')
  if (
    typeof descriptors.operationId.value !== 'string' ||
    typeof descriptors.platform.value !== 'string'
  ) throw new Error('runtime command trace identity is invalid')
  return Object.freeze({
    operationId: descriptors.operationId.value,
    platform: descriptors.platform.value,
  })
}

function requireRuntimeSettlement(value) {
  requireExactObjectFields(value, SETTLEMENT_FIELDS, 'runtime command settlement')
  if (
    ![true, false, null].includes(value.treeEmpty) ||
    !SETTLEMENT_OUTCOMES.includes(value.cleanupOutcome) ||
    !SETTLEMENT_OUTCOMES.includes(value.transportOutcome) ||
    !SETTLEMENT_OUTCOMES.includes(value.controlOutcome) ||
    !portableRecordOrNull(value.processEvidence) ||
    !portableRecordOrNull(value.inputEvidence) ||
    !portableRecordOrNull(value.ownerFailure) ||
    !portableRecordOrNull(value.ownershipEvidence) ||
    !portableRecordOrNull(value.transportEvidence)
  ) throw new Error('runtime command settlement outcomes are invalid')
}

function portableRecordOrNull(value) {
  return value === null || typeof value === 'object' && !Array.isArray(value)
}

function fallbackTraceIdentity() {
  return Object.freeze({ operationId: UNBOUND_OPERATION_ID, platform: UNBOUND_PLATFORM })
}

function traceIdentityFromDescriptors(descriptors) {
  const operationId = isDataDescriptor(descriptors.operationId)
    ? portableOperationIdOrFallback(descriptors.operationId.value)
    : UNBOUND_OPERATION_ID
  const platform = isDataDescriptor(descriptors.platform) &&
    typeof descriptors.platform.value === 'string' &&
    descriptors.platform.value.length > 0 && descriptors.platform.value.length <= 32
    ? descriptors.platform.value
    : process.platform
  return Object.freeze({ operationId, platform })
}

function portableOperationIdOrFallback(value) {
  try {
    return parseTestIdentity({
      runId: 'browsergate',
      operationId: value,
      scenario: 'browsergate-runtime-command',
    }).operationId
  } catch {
    return UNBOUND_OPERATION_ID
  }
}

export function resolveHostExecutable(name, options = {}) {
  const optionDescriptors = requireInertRecordDescriptors(options, 'host executable resolution options')
  requireEnumerableDataDescriptors(optionDescriptors, 'host executable resolution option')
  requireAllowedFields(optionDescriptors, ['environment', 'platform'], 'host executable resolution options')
  const platform = optionalField(optionDescriptors, 'platform', process.platform)
  const environment = optionalField(optionDescriptors, 'environment', process.env)
  if (typeof name !== 'string' || name === '' || /[\\/]/u.test(name)) {
    throw new Error('host executable name must be one path segment')
  }
  const pathValue = environmentEntry(canonicalEnvironment(environment), 'PATH')
  if (pathValue === undefined || pathValue === '') {
    throw new Error(`cannot resolve ${name}: PATH is unavailable`)
  }
  const names = platform === 'win32' && !name.toLowerCase().endsWith('.exe')
    ? [`${name}.exe`]
    : [name]
  for (const rawDirectory of pathValue.split(delimiter)) {
    const directory = rawDirectory.trim().replace(/^"|"$/gu, '')
    if (directory === '') continue
    for (const candidateName of names) {
      const candidate = resolve(directory, candidateName)
      try {
        const canonical = realpathSync(candidate)
        const metadata = lstatSync(canonical)
        if (metadata.isFile() && !metadata.isSymbolicLink()) return canonical
      } catch {
        // PATH entries are claims, not authority. Only a resolved regular file wins.
      }
    }
  }
  throw new Error(`cannot resolve host executable ${name}`)
}

function assertRuntimeArtifactLive(artifact, label) {
  if (
    !isAbsolute(artifact.path) || resolve(artifact.path) !== artifact.path ||
    realpathSync(artifact.path) !== artifact.path
  ) throw new Error(`${label} is not a canonical real file`)
  const metadata = lstatSync(artifact.path, { bigint: true })
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1n) {
    throw new Error(`${label} is not a regular file`)
  }
  accessSync(artifact.path, fsConstants.X_OK)
  const identity = Object.freeze({
    dev: metadata.dev,
    ino: metadata.ino,
    size: metadata.size,
    mtimeNs: metadata.mtimeNs,
    ctimeNs: metadata.ctimeNs,
  })
  if (artifact.identity !== undefined && (
    identity.dev !== artifact.identity.dev || identity.ino !== artifact.identity.ino ||
    identity.size !== artifact.identity.size || identity.mtimeNs !== artifact.identity.mtimeNs ||
    identity.ctimeNs !== artifact.identity.ctimeNs
  )) throw new Error(`${label} changed while used`)
  return identity
}

function canonicalEnvironment(value) {
  // Node's host environment has a platform-owned exotic prototype. It is the
  // one trusted exception, and descriptor-copying immediately retires that live
  // view before PATH resolution or an asynchronous command can observe it.
  const descriptors = value === process.env
    ? Object.getOwnPropertyDescriptors(value)
    : requireInertRecordDescriptors(value, 'runtime command inherited environment')
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('runtime command environment contains symbol entries')
  }
  const foldedNames = new Set()
  const environment = Object.create(null)
  for (const name of names.sort((left, right) => left < right ? -1 : left > right ? 1 : 0)) {
    const descriptor = descriptors[name]
    if (!isEnumerableDataDescriptor(descriptor)) {
      throw new Error('runtime command environment entries must be enumerable inert data')
    }
    const entry = descriptor.value
    if (name === '' || name.includes('=') || name.includes('\0')) {
      throw new Error('runtime command environment contains an invalid entry')
    }
    // The request fields are the sole child identity authority. Discard ambient
    // copies before launch so wrapper-level run IDs cannot spoof or block them.
    if (isProcessOwnerReservedEnvironmentName(name)) continue
    if (typeof entry !== 'string' || entry.includes('\0')) {
      throw new Error('runtime command environment contains an invalid entry')
    }
    const foldedName = name.toUpperCase()
    if (foldedNames.has(foldedName)) {
      throw new Error('runtime command environment contains case-insensitive duplicate names')
    }
    foldedNames.add(foldedName)
    Object.defineProperty(environment, name, {
      value: entry,
      enumerable: true,
      writable: false,
      configurable: false,
    })
  }
  return Object.freeze(environment)
}

function requireInertRecordDescriptors(value, label) {
  if (
    value === null || typeof value !== 'object' || nodeTypes.isProxy(value) || Array.isArray(value)
  ) throw new Error(`${label} must be an inert data record`)
  const prototype = Object.getPrototypeOf(value)
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error(`${label} must be an inert data record`)
  }
  return Object.getOwnPropertyDescriptors(value)
}

function requireEnumerableDataDescriptors(descriptors, label) {
  for (const descriptor of Object.values(descriptors)) {
    if (!isEnumerableDataDescriptor(descriptor)) throw new Error(`${label} must be enumerable inert data`)
  }
}

function requireAllowedFields(descriptors, allowed, label) {
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string') || names.some((name) => !allowed.includes(name))) {
    throw new Error(`${label} contains unsupported options`)
  }
}

function requireExactFields(descriptors, expected, label) {
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error(`${label} has an invalid evidence shape`)
  }
  names.sort()
  if (!sameStrings(names, [...expected].sort())) throw new Error(`${label} has an invalid evidence shape`)
}

function requireExactObjectFields(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} has an invalid evidence shape`)
  }
  if (!sameStrings(Object.keys(value).sort(), [...expected].sort())) {
    throw new Error(`${label} has an invalid evidence shape`)
  }
}

function requiredField(descriptors, name, label) {
  if (!Object.hasOwn(descriptors, name)) throw new Error(`${label} omitted ${name}`)
  return descriptors[name].value
}

function optionalField(descriptors, name, fallback) {
  return Object.hasOwn(descriptors, name) ? descriptors[name].value : fallback
}

function isEnumerableDataDescriptor(descriptor) {
  return isDataDescriptor(descriptor) && descriptor.enumerable === true
}

function isDataDescriptor(descriptor) {
  return descriptor !== undefined && Object.hasOwn(descriptor, 'value')
}

function requirePositiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be a positive integer`)
}

function environmentEntry(environment, expectedName) {
  return Object.entries(environment).find(([name]) => name.toUpperCase() === expectedName)?.[1]
}

function nonnegativeSafeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0
}

function sameStrings(actual, expected) {
  return actual.length === expected.length && actual.every((value, index) => value === expected[index])
}
