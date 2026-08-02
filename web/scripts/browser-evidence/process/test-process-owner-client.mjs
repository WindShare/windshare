import { spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { lstatSync } from 'node:fs'
import { createServer } from 'node:net'
import { isAbsolute, resolve } from 'node:path'

import {
  createOwnedByteChannel,
  createOwnedEventChannel,
  normalizeOwnedProcessCapture,
  waitForExactWritableCompletion,
} from './owned-process-channel.mjs'
import {
  isProcessOwnerReservedEnvironmentName,
  parseTestIdentity,
} from './test-identity.mjs'

const REQUEST_SCHEMA = 'windshare.process-owner-request/v2'
const CONTROL_SCHEMA = 'windshare.process-owner-control/v1'
const SETTLEMENT_SCHEMA = 'windshare.process-owner-settlement/v3'
const START_EVIDENCE_SCHEMA = 'windshare.process-owner-start-evidence/v1'
const START_DECISION_SCHEMA = 'windshare.process-owner-start-decision/v1'
const EVENT_SCHEMA = 'windshare.test-event/v1'
const MAXIMUM_DOCUMENT_BYTES = 1_048_576
const MAXIMUM_EVENT_FIELD_BYTES = 256
const MAXIMUM_DIAGNOSTIC_BYTES = 512
const MAXIMUM_DEADLINE_MILLISECONDS = 3_600_000
const MAXIMUM_TERMINATION_MILLISECONDS = 60_000
const OWNER_STARTUP_LEASE_MILLISECONDS = 10_000
const OWNER_TRANSPORT_MARGIN_MILLISECONDS = 5_000
const OWNER_FORCED_JOIN_MILLISECONDS = 1_000
const LINUX_GUARDIAN_MARGIN_MILLISECONDS = 4_000
const OWNER_READY_BYTE = 0xa5
const MAXIMUM_UINT64 = 18_446_744_073_709_551_615n
const EXECUTABLE_VOLUME_ID_PATTERN = /^[0-9a-f]{16}$/u
const EXECUTABLE_OBJECT_ID_PATTERN = /^[0-9a-f]{32}$/u
const CONTROL_FAILURE_BRAND = new WeakSet()
const TRANSPORT_FAILURE_BRAND = new WeakSet()
const PROCESS_OWNER_FAILURE_EVIDENCE = new WeakMap()

export class TestProcessOwnerDeadlineError extends Error {
  constructor(message) {
    super(message)
    this.name = 'TestProcessOwnerDeadlineError'
  }
}

export class TestProcessOwnerControlError extends Error {
  constructor(message, settlement, cause) {
    super(message, { cause })
    this.name = 'TestProcessOwnerControlError'
    this.settlement = settlement
    CONTROL_FAILURE_BRAND.add(this)
    PROCESS_OWNER_FAILURE_EVIDENCE.set(this, Object.freeze({
      kind: 'control-failed',
      settlement,
      transportEvidence: Object.freeze({
        kind: 'test-process-owner-control',
        publication: 'failed',
      }),
    }))
  }
}

export class TestProcessOwnerTransportError extends Error {
  constructor(message, settlement, terminal, output, events, cause) {
    super(message, { cause })
    this.name = 'TestProcessOwnerTransportError'
    this.settlement = settlement
    this.terminal = terminal
    this.output = output
    this.events = events
    const inertTerminal = terminal === undefined
      ? null
      : Object.freeze({ code: terminal.code, signal: terminal.signal })
    TRANSPORT_FAILURE_BRAND.add(this)
    PROCESS_OWNER_FAILURE_EVIDENCE.set(this, Object.freeze({
      kind: 'transport-failed',
      settlement,
      transportEvidence: Object.freeze({
        kind: 'test-process-owner-transport',
        terminal: inertTerminal,
      }),
    }))
  }
}

// Runtime orchestration must classify only failures minted by this module. A
// WeakMap lookup neither invokes Proxy traps nor trusts forgeable public fields.
export function testProcessOwnerFailureEvidence(value) {
  if (typeof value !== 'object' || value === null) return undefined
  if (!CONTROL_FAILURE_BRAND.has(value) && !TRANSPORT_FAILURE_BRAND.has(value)) return undefined
  return PROCESS_OWNER_FAILURE_EVIDENCE.get(value)
}

export async function startTestProcessOwner(options) {
  const capture = normalizeOwnedProcessCapture(options.capture)
  const stdout = createOwnedByteChannel(capture.stdoutBytes, 'owned stdout')
  const stderr = createOwnedByteChannel(capture.stderrBytes, 'owned stderr')
  const events = createOwnedEventChannel(capture.eventCount, 'owned test events')
  if (capture.eventCount === 0) events.finish()
  const captures = Object.freeze({
    stdout,
    stderr,
    events,
    eventEnabled: capture.eventCount > 0,
  })
  const completion = executePreparedTestProcessOwner(options, captures)
    .finally(() => finishOwnedCaptures(captures))
  observePromise(completion)
  return Object.freeze({
    stdout: stdout.view,
    stderr: stderr.view,
    events: events.view,
    completion,
  })
}

export async function executeTestProcessOwner(options) {
  const run = await startTestProcessOwner(options)
  return run.completion
}

async function executePreparedTestProcessOwner({
  owner,
  runId,
  operationId,
  scenario,
  command,
  environment,
  deadlineMs,
  terminationGraceMs,
  terminationSignal,
  platform = process.platform,
}, captures) {
  const identity = parseTestIdentity({ runId, operationId, scenario })
  const ownerArtifact = requireOwner(owner)
  const ownerIdentity = assertOwnerArtifactLive(ownerArtifact)
  requireCommand(command)
  // The request's byte authority must describe the invocation, not whichever
  // bytes happen to remain in a caller-owned view after asynchronous attach.
  const frozenInput = command.stdin === undefined ? undefined : Buffer.from(command.stdin)
  requireBoundedPositiveInteger(
    deadlineMs,
    MAXIMUM_DEADLINE_MILLISECONDS,
    'deadline',
  )
  requireBoundedPositiveInteger(
    terminationGraceMs,
    MAXIMUM_TERMINATION_MILLISECONDS,
    'termination grace',
  )
  const request = Object.freeze({
    schema_version: REQUEST_SCHEMA,
    run_id: identity.runId,
    operation_id: identity.operationId,
    scenario: identity.scenario,
    command: Object.freeze({
      executable: command.executable,
      arguments: Object.freeze([...command.arguments]),
      working_directory: command.cwd,
      environment: canonicalEnvironmentEntries(environment),
      stdin: frozenInput === undefined
        ? null
        : Object.freeze({ byte_length: frozenInput.byteLength }),
    }),
    deadline_milliseconds: deadlineMs,
    termination_grace_milliseconds: terminationGraceMs,
  })
  let transport
  try {
    transport = platform === 'win32'
      ? await executeWindows(
          ownerArtifact.path, request, frozenInput, identity, terminationSignal, captures,
        )
      : platform === 'linux'
        ? await executeLinux(
            ownerArtifact.path, request, frozenInput, identity, terminationSignal, captures,
          )
        : (() => { throw new Error(`test process owner is unsupported on ${platform}`) })()
  } catch (cause) {
    finishOwnedCaptures(captures)
    const evidence = ownedCaptureEvidence(captures)
    throw new TestProcessOwnerTransportError(
      'test process owner transport failed before it could return bounded evidence',
      undefined,
      undefined,
      evidence.output,
      evidence.events,
      cause,
    )
  }
  finishOwnedCaptures(captures)
  const captureEvidence = ownedCaptureEvidence(captures)
  let ownerArtifactFailure
  try {
    assertOwnerArtifactLive(ownerArtifact, ownerIdentity)
  } catch (cause) {
    ownerArtifactFailure = cause
  }
  let settlement
  let settlementFailure = transport.settlementFailure
  try {
    if (transport.settlement !== undefined) {
      const parsedSettlement = parseTestProcessOwnerSettlementForRequest(
        transport.settlement,
        request,
        platform,
      )
      let startEvidence
      if (transport.startGate?.status === 'fulfilled') {
        try {
          startEvidence = reconcileStartGate(parsedSettlement, transport.startGate.evidence)
        } catch (cause) {
          // Preserve the independently authenticated terminal settlement while
          // making missing pre-release authority a transport-blocking verdict.
          settlementFailure = cause
        }
      } else if (transport.transportFailure === undefined) {
        settlementFailure = new Error('test process owner start gate returned no terminal evidence')
      }
      settlement = projectSettlement(parsedSettlement, captureEvidence, startEvidence)
    } else if (settlementFailure === undefined) {
      settlementFailure = new Error('test process owner returned no settlement document')
    }
  } catch (cause) {
    settlementFailure = cause
  }
  const terminalFailure = ownerTerminalSucceeded(transport.terminal)
    ? undefined
    : new Error(ownerTerminalFailureMessage(transport.terminal))
  const transportFailure = aggregateErrors(
    'test process owner transport failed',
    [transport.transportFailure, terminalFailure, settlementFailure, ownerArtifactFailure],
  )
  if (transportFailure !== undefined) {
    throw new TestProcessOwnerTransportError(
      terminalFailure?.message ?? 'test process owner transport returned incomplete evidence',
      settlement,
      transport.terminal,
      captureEvidence.output,
      captureEvidence.events,
      transportFailure,
    )
  }
  if (transport.controlFailure !== undefined) {
    throw new TestProcessOwnerControlError(
      `test process owner control publication failed: ${transport.controlFailure.message}`,
      settlement,
      transport.controlFailure,
    )
  }
  return settlement
}

async function executeLinux(ownerPath, request, input, identity, signal, captures) {
  // Stable descriptor positions are part of the owner ABI. The event pipe is
  // still drained when capture is disabled so fd7/fd8 cannot shift and a child
  // that emits an event cannot deadlock its start-gated process tree.
  const stdio = ['pipe', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe']
  const child = spawn(ownerPath, ['guard'], {
    detached: true,
    shell: false,
    stdio,
    windowsHide: true,
  })
  const transportLease = ownerTransportLease(
    request.deadline_milliseconds + (2 * request.termination_grace_milliseconds) +
      LINUX_GUARDIAN_MARGIN_MILLISECONDS,
    'Linux test process owner exceeded its bounded transport lease',
  )
  const terminal = trackTransportTask('owner terminal', childTerminal(child))
  const status = trackTransportTask(
    'settlement stream',
    boundedPipe(child.stdio[3], 'test process owner status'),
  )
  const stdoutTask = drainPipe(child.stdout, captures.stdout, 'owned stdout')
  const stderrTask = drainPipe(child.stderr, captures.stderr, 'owned stderr')
  const eventTask = !captures.eventEnabled
    ? drainDiscardPipe(child.stdio[6], 'private test-event')
    : drainEventPipe(child.stdio[6], identity, captures.events)
  const stdoutDrain = trackTransportTask('owned stdout drain', stdoutTask)
  const stderrDrain = trackTransportTask('owned stderr drain', stderrTask)
  const eventDrain = trackTransportTask('test-event drain', eventTask)
  const control = child.stdio[4]
  const rawInput = child.stdio[5]
  const startEvidence = child.stdio[7]
  const startDecision = child.stdio[8]
  if (
    child.stdin === null || control === null || rawInput === null ||
    startEvidence === null || startDecision === null
  ) {
    child.kill('SIGKILL')
    throw new Error('Linux test process owner did not create its private protocol pipes')
  }
  const startGate = trackTransportTask(
    'start gate',
    completeStartGate(
      boundedPipe(
        startEvidence,
        'Linux test process owner start-evidence',
        MAXIMUM_DOCUMENT_BYTES + 5,
      ),
      (bytes) => publishPipeBytesAndClose(
        startDecision,
        bytes,
        'Linux test process owner start-decision pipe',
      ),
      request,
      'linux',
    ),
  )
  const requestCompletion = trackTransportTask(
    'request publication',
    waitForExactWritableCompletion(child.stdin, 'Linux test process owner request pipe'),
  )
  const rawInputCompletion = trackTransportTask(
    'raw-input publication',
    waitForExactWritableCompletion(rawInput, 'Linux test process owner raw-input pipe'),
  )
  child.stdin.end(Buffer.from(canonicalJSONString(request), 'utf8'))
  rawInput.end(input === undefined ? undefined : Buffer.from(input))
  let controlFailure
  let controlCompletion
  const recordControlFailure = (cause) => {
    controlFailure ??= new Error('Linux test process owner control pipe failed', { cause })
  }
  control.once('error', recordControlFailure)
  const tracked = [
    terminal, status, stdoutDrain, stderrDrain, eventDrain, startGate, requestCompletion,
    rawInputCompletion,
  ]
  const endControl = (payload) => {
    if (controlCompletion !== undefined) return controlCompletion.task
    controlCompletion = trackTransportTask(
      'control publication',
      waitForExactWritableCompletion(control, 'Linux test process owner control pipe'),
    )
    tracked.push(controlCompletion)
    try {
      if (!control.destroyed) control.end(payload)
    } catch (cause) {
      recordControlFailure(cause)
      control.destroy(cause instanceof Error ? cause : new Error(String(cause)))
    }
    return controlCompletion.task
  }
  const requestStop = () => { endControl(frame(controlRecord(identity, signal))) }
  const removeAbort = listenForAbort(signal, requestStop)
  let phaseFailure
  let retirementFailure
  try {
    phaseFailure = await waitForTrackedTransport(tracked, transportLease)
  } finally {
    removeAbort()
    transportLease.cancel()
    retirementFailure = await retireLinuxOwner(
      child,
      terminal.task,
      control,
      controlCompletion?.task,
      endControl,
      request.termination_grace_milliseconds,
    )
  }
  await Promise.resolve()
  const transportState = snapshotTrackedTransport(tracked)
  let settlement
  let settlementFailure
  if (status.state.status === 'fulfilled') {
    try {
      settlement = decodeStatusLine(status.state.value)
    } catch (cause) {
      settlementFailure = cause
    }
  } else if (status.state.status === 'rejected') {
    settlementFailure = status.state.reason
  }
  return Object.freeze({
    settlement,
    settlementFailure,
    controlFailure,
    terminal: terminal.state.status === 'fulfilled' ? terminal.state.value : undefined,
    startGate: snapshotStartGate(startGate),
    transportFailure: aggregateErrors(
      'Linux test process owner transport did not settle cleanly',
      [phaseFailure, retirementFailure, ...transportState.failures],
    ),
  })
}

async function executeWindows(ownerPath, request, input, identity, signal, captures) {
  const endpointSet = await openWindowsEndpointSet(input !== undefined, identity, captures)
  const {
    status: statusEndpoint,
    control: controlEndpoint,
    parent: parentEndpoint,
    input: inputEndpoint,
    event: eventEndpoint,
    startEvidence: startEvidenceEndpoint,
    startDecision: startDecisionEndpoint,
    endpoints,
  } = endpointSet
  const startupLease = ownerTransportLease(
    OWNER_STARTUP_LEASE_MILLISECONDS,
    'Windows test process owner did not establish readiness within its startup lease',
  )
  let child
  let terminal
  let startGate
  let tracked = []
  let ready = false
  let controlFailure
  const phaseFailures = []
  let removeAbort = () => undefined
  let lifecycleLease
  try {
    child = spawn(ownerPath, [
      'supervise',
      '--status-pipe', statusEndpoint.path,
      '--control-pipe', controlEndpoint.path,
      '--parent-pipe', parentEndpoint.path,
      '--start-evidence-pipe', startEvidenceEndpoint.path,
      '--start-decision-pipe', startDecisionEndpoint.path,
      '--ready-stdout',
      ...(inputEndpoint === undefined ? [] : ['--input-pipe', inputEndpoint.path]),
      ...(eventEndpoint === undefined ? [] : ['--event-pipe', eventEndpoint.path]),
    ], {
      shell: false,
      stdio: ['pipe', 'pipe', 'pipe'],
      windowsHide: true,
    })
    terminal = trackTransportTask('owner terminal', childTerminal(child))
    const ownerOutput = drainWindowsOwnerOutput(child.stdout, captures.stdout)
    const stderrTask = drainPipe(child.stderr, captures.stderr, 'owned stderr')
    startGate = trackTransportTask(
      'start gate',
      completeStartGate(
        startEvidenceEndpoint.completion.then(() => startEvidenceEndpoint.bytes()),
        (bytes) => {
          startDecisionEndpoint.end(bytes)
          return startDecisionEndpoint.completion
        },
        request,
        'win32',
      ),
    )
    tracked = [
      terminal,
      startGate,
      trackTransportTask('owned stdout drain', ownerOutput.completion),
      trackTransportTask('owned stderr drain', stderrTask),
      ...endpoints.map((endpoint) => trackTransportTask(
        `private ${endpoint.label} endpoint`,
        endpoint.completion,
      )),
    ]
    if (child.stdin === null) {
      throw new Error('Windows test process owner did not create its framed request pipe')
    }
    tracked.push(trackTransportTask(
      'request publication',
      waitForExactWritableCompletion(child.stdin, 'Windows test process owner request pipe'),
    ))
    child.stdin.end(frame(request))
    inputEndpoint?.end(Buffer.from(input))

    const requestStop = () => {
      try {
        controlEndpoint.end(frame(controlRecord(identity, signal)))
      } catch (cause) {
        controlFailure ??= new Error('Windows test process owner control pipe failed', { cause })
      }
    }
    removeAbort = listenForAbort(signal, requestStop)
    const readiness = Promise.all([
      ownerOutput.ready,
      ...endpoints.map((endpoint) => endpoint.connected),
    ])
    observePromise(readiness)
    try {
      await Promise.race([
        readiness,
        rejectOwnerExitBeforeReadiness(terminal.task),
        startupLease.expired,
      ])
      ready = true
    } catch (cause) {
      phaseFailures.push(cause)
    } finally {
      startupLease.cancel()
    }
    if (ready) {
      lifecycleLease = ownerTransportLease(
        request.deadline_milliseconds + request.termination_grace_milliseconds +
          OWNER_TRANSPORT_MARGIN_MILLISECONDS,
        'Windows test process owner exceeded its bounded lifecycle lease',
      )
      const lifecycleFailure = await waitForTrackedTransport(tracked, lifecycleLease)
      if (lifecycleFailure !== undefined) phaseFailures.push(lifecycleFailure)
    }
  } catch (cause) {
    phaseFailures.push(cause)
  } finally {
    removeAbort()
    startupLease.cancel()
    lifecycleLease?.cancel()
    if (child !== undefined && child.exitCode === null && child.signalCode === null) {
      if (ready) {
        // Once readiness transfers containment authority, let the owner retire
        // its Job before the kill-on-close fallback consumes remaining evidence.
        const authorityClose = await Promise.allSettled([
          controlEndpoint.close(),
          parentEndpoint.close(),
        ])
        collectRejectedPromises(
          phaseFailures,
          authorityClose,
          'close Windows retirement authority',
        )
        await Promise.race([
          terminal?.task.catch(() => undefined),
          delay(request.termination_grace_milliseconds + OWNER_TRANSPORT_MARGIN_MILLISECONDS),
        ])
      }
    }
    if (child !== undefined && child.exitCode === null && child.signalCode === null) {
      try {
        child.kill('SIGKILL')
      } catch (cause) {
        phaseFailures.push(new Error('force-retire Windows test process owner', { cause }))
      }
      if (terminal !== undefined) {
        await Promise.race([
          terminal.task.catch(() => undefined),
          delay(OWNER_FORCED_JOIN_MILLISECONDS),
        ])
      }
    }
    const endpointClose = await Promise.allSettled(endpoints.map((endpoint) => endpoint.close()))
    collectRejectedPromises(phaseFailures, endpointClose, 'close private Windows endpoint')
  }
  await Promise.resolve()
  const transportState = snapshotTrackedTransport(tracked)
  let settlement
  let settlementFailure
  try {
    settlement = decodeStatusLine(statusEndpoint.bytes())
  } catch (cause) {
    settlementFailure = cause
  }
  return Object.freeze({
    settlement,
    settlementFailure,
    controlFailure,
    terminal: terminal?.state.status === 'fulfilled' ? terminal.state.value : undefined,
    startGate: snapshotStartGate(startGate),
    transportFailure: aggregateErrors(
      'Windows test process owner transport did not settle cleanly',
      [...phaseFailures, ...transportState.failures],
    ),
  })
}

// This framed oracle is shared by live transports and protocol vectors so EOF,
// canonical JSON, request identity, and start authority cannot drift apart.
export function parseTestProcessOwnerStartEvidenceFrameForRequest(bytes, request, platform) {
  if (!(bytes instanceof Uint8Array)) {
    throw new Error('test process owner start-evidence frame must be bytes')
  }
  if (bytes.byteLength === 0) return undefined
  const frameBytes = Buffer.from(bytes)
  const evidence = parseTestProcessOwnerStartEvidenceForRequest(
    decodeExactFrame(frameBytes, 'test process owner start evidence'),
    request,
    platform,
  )
  const canonical = Buffer.from(canonicalJSONString(startEvidenceRecord(evidence)), 'utf8')
  if (!frameBytes.subarray(4).equals(canonical)) {
    throw new Error('test process owner start evidence is not canonical JSON')
  }
  return evidence
}

function parseTestProcessOwnerStartEvidenceForRequest(value, request, platform) {
  exactKeys(value, [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'platform', 'process_id',
    'process_instance', 'executable',
  ], 'test process owner start evidence')
  if (
    value.schema_version !== START_EVIDENCE_SCHEMA || value.run_id !== request.run_id ||
    value.operation_id !== request.operation_id || value.scenario !== request.scenario
  ) throw new Error('test process owner start evidence differs from its request identity')
  const expectedPlatform = processOwnerPlatform(platform)
  if (value.platform !== expectedPlatform) {
    throw new Error('test process owner start evidence platform is inconsistent')
  }
  if (!Number.isSafeInteger(value.process_id) || value.process_id < 1) {
    throw new Error('test process owner start evidence PID is invalid')
  }
  const processInstance = requireCanonicalPositiveUint64(
    value.process_instance,
    'test process owner process instance',
  )
  exactKeys(value.executable, ['volume', 'object'], 'test process owner executable identity')
  if (
    !EXECUTABLE_VOLUME_ID_PATTERN.test(value.executable.volume) ||
    !EXECUTABLE_OBJECT_ID_PATTERN.test(value.executable.object)
  ) {
    throw new Error('test process owner executable identity is not canonical lowercase hexadecimal')
  }
  if (/^0+$/u.test(value.executable.object)) {
    throw new Error('test process owner executable identity is unavailable')
  }
  return Object.freeze({
    schemaVersion: START_EVIDENCE_SCHEMA,
    runId: value.run_id,
    operationId: value.operation_id,
    scenario: value.scenario,
    platform: value.platform,
    processId: value.process_id,
    processInstance,
    executable: Object.freeze({
      volume: value.executable.volume,
      object: value.executable.object,
    }),
  })
}

async function completeStartGate(evidenceBytes, publishDecision, request, platform) {
  let bytes
  try {
    bytes = await evidenceBytes
  } catch (cause) {
    return failStartGate(publishDecision, cause)
  }
  let evidence
  try {
    evidence = parseTestProcessOwnerStartEvidenceFrameForRequest(bytes, request, platform)
  } catch (cause) {
    return failStartGate(publishDecision, cause)
  }
  if (evidence === undefined) {
    await publishDecision(Buffer.alloc(0))
    return undefined
  }
  await publishDecision(encodeTestProcessOwnerStartDecisionFrame(
    evidence,
    Object.freeze({ outcome: 'accepted' }),
  ))
  return evidence
}

async function failStartGate(publishDecision, cause) {
  try {
    // EOF denies release without inventing a rejected decision that could not be
    // bound to authenticated evidence.
    await publishDecision(Buffer.alloc(0))
  } catch (closeCause) {
    throw new AggregateError(
      [cause, closeCause],
      'reject invalid process-owner start evidence and close its decision authority',
    )
  }
  throw cause
}

function startEvidenceRecord(evidence) {
  return Object.freeze({
    schema_version: START_EVIDENCE_SCHEMA,
    run_id: evidence.runId,
    operation_id: evidence.operationId,
    scenario: evidence.scenario,
    platform: evidence.platform,
    process_id: evidence.processId,
    process_instance: evidence.processInstance,
    executable: Object.freeze({
      volume: evidence.executable.volume,
      object: evidence.executable.object,
    }),
  })
}

export function encodeTestProcessOwnerStartDecisionFrame(evidence, decision) {
  requireRecord(decision, 'test process owner start decision')
  let failure = {}
  if (decision.outcome === 'accepted') {
    exactKeys(decision, ['outcome'], 'accepted test process owner start decision')
  } else if (decision.outcome === 'rejected') {
    exactKeys(
      decision,
      ['outcome', 'failureCode', 'failureMessage'],
      'rejected test process owner start decision',
    )
    failure = {
      failure_code: requireNonemptyDiagnostic(
        decision.failureCode,
        'start rejection code',
      ),
      failure_message: requireNonemptyDiagnostic(
        decision.failureMessage,
        'start rejection message',
      ),
    }
  } else {
    throw new Error('test process owner start decision outcome is unsupported')
  }
  return frame(Object.freeze({
    schema_version: START_DECISION_SCHEMA,
    run_id: evidence.runId,
    operation_id: evidence.operationId,
    scenario: evidence.scenario,
    platform: evidence.platform,
    process_id: evidence.processId,
    process_instance: evidence.processInstance,
    executable: Object.freeze({
      volume: evidence.executable.volume,
      object: evidence.executable.object,
    }),
    outcome: decision.outcome,
    ...failure,
  }))
}

function reconcileStartGate(settlement, evidence) {
  if (evidence !== undefined) return evidence
  if (settlement.target.outcome === 'spawn_failed' || settlement.target.outcome === 'not_started') {
    return undefined
  }
  throw new Error('created target lacks authenticated pre-release start evidence')
}

function processOwnerPlatform(platform) {
  if (platform === 'win32') return 'windows_job'
  if (platform === 'linux') return 'linux_subreaper'
  throw new Error(`test process owner is unsupported on ${platform}`)
}

// Protocol-vector tests import this pure oracle so malformed evidence is tested
// at the same request-bound boundary used by live transports.
export function parseTestProcessOwnerSettlementForRequest(value, request, platform) {
  exactOptionalKeys(value, [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'termination_reason',
    'target', 'input', 'tree_state', 'cleanup', 'platform',
  ], ['owner_failure'], 'test process owner settlement')
  if (
    value.schema_version !== SETTLEMENT_SCHEMA || value.run_id !== request.run_id ||
    value.operation_id !== request.operation_id || value.scenario !== request.scenario
  ) throw new Error('test process owner settlement differs from its request identity')
  if (![
    'natural', 'deadline', 'stop', 'parent_lost', 'initialization_failed', 'start_rejected',
    'owner_failure',
  ].includes(value.termination_reason)) {
    throw new Error('test process owner settlement termination reason is unsupported')
  }
  if (!['proven_empty', 'nonempty', 'unknown'].includes(value.tree_state)) {
    throw new Error('test process owner tree state is unsupported')
  }
  const target = parseTargetEvidence(value.target)
  const input = parseInputEvidence(value.input)
  const cleanup = parseCleanupEvidence(value.cleanup)
  const ownerFailure = Object.hasOwn(value, 'owner_failure')
    ? parseFailureEvidence(value.owner_failure, 'process owner failure')
    : undefined
  validateSettlementState(value.termination_reason, target, input, ownerFailure)
  validateRequestInputEvidence(request.command.stdin, target, input)
  validateCleanupEvidence(value.tree_state, cleanup, ownerFailure)
  const platformEvidence = parsePlatformEvidence(
    value.platform,
    platform,
    value.termination_reason,
    value.tree_state,
    target,
    ownerFailure,
  )
  return Object.freeze({
    terminationReason: value.termination_reason,
    target,
    input,
    treeState: value.tree_state,
    cleanup,
    ownerFailure,
    platform: platformEvidence,
  })
}

function projectSettlement(settlement, captureEvidence, startEvidence) {
  return Object.freeze({
    processEvidence: projectProcessEvidence(settlement.target),
    ...(startEvidence === undefined ? {} : { startEvidence }),
    treeEmpty: settlement.treeState === 'proven_empty',
    cleanupOutcome: settlement.cleanup.outcome,
    inputEvidence: settlement.input,
    ...(settlement.ownerFailure === undefined ? {} : { ownerFailure: settlement.ownerFailure }),
    output: captureEvidence.output,
    events: captureEvidence.events,
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner',
      backend: settlement.platform.kind,
      terminationReason: settlement.terminationReason,
      platform: settlement.platform,
    }),
  })
}

function finishOwnedCaptures(captures) {
  captures.stdout.finish()
  captures.stderr.finish()
  captures.events.finish()
}

function ownedCaptureEvidence(captures) {
  return Object.freeze({
    output: Object.freeze({
      stdout: captures.stdout.view.snapshot(),
      stderr: captures.stderr.view.snapshot(),
    }),
    events: captures.events.view.snapshot(),
  })
}

function parseTargetEvidence(value) {
  requireRecord(value, 'test process owner target evidence')
  if (value.outcome === 'exited') {
    exactKeys(value, ['outcome', 'exit_code'], 'exited target evidence')
    if (!Number.isSafeInteger(value.exit_code)) throw new Error('exited target lacks an exit code')
    return Object.freeze({ outcome: value.outcome, exitCode: value.exit_code })
  }
  if (value.outcome === 'signaled') {
    exactKeys(value, ['outcome', 'signal'], 'signaled target evidence')
    return Object.freeze({
      outcome: value.outcome,
      signal: requireNonemptyDiagnostic(value.signal, 'target signal'),
    })
  }
  if ([
    'spawn_failed', 'not_started', 'terminal_evidence_lost', 'start_evidence_lost',
  ].includes(value.outcome)) {
    exactKeys(value, ['outcome', 'failure_code', 'failure_message'], 'failed target evidence')
    return Object.freeze({
      outcome: value.outcome,
      failureCode: requireNonemptyDiagnostic(value.failure_code, 'target failure code'),
      failureMessage: requireNonemptyDiagnostic(value.failure_message, 'target failure message'),
    })
  }
  throw new Error('test process owner target outcome is unsupported')
}

function projectProcessEvidence(target) {
  if (target.outcome === 'exited') {
    return Object.freeze({ terminal: 'exited', exitCode: target.exitCode })
  }
  if (target.outcome === 'signaled') {
    return Object.freeze({ terminal: 'signaled', signal: target.signal })
  }
  if (target.outcome === 'not_started') return Object.freeze({ terminal: 'not-started' })
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: target.failureCode,
    errorMessage: target.failureMessage,
  })
}

function parseInputEvidence(value) {
  requireRecord(value, 'test process owner input evidence')
  if (!['not_requested', 'delivered', 'failed', 'not_started', 'evidence_lost'].includes(value.outcome)) {
    throw new Error('test process owner input outcome is unsupported')
  }
  if (value.outcome === 'failed' || value.outcome === 'evidence_lost') {
    exactKeys(value, ['outcome', 'failure_code', 'failure_message'], 'failed input evidence')
    return Object.freeze({
      outcome: value.outcome,
      failureCode: requireNonemptyDiagnostic(value.failure_code, 'input failure code'),
      failureMessage: requireNonemptyDiagnostic(value.failure_message, 'input failure message'),
    })
  }
  exactKeys(value, ['outcome'], 'successful input evidence')
  return Object.freeze({
    outcome: value.outcome,
    failureCode: '',
    failureMessage: '',
  })
}

function parseCleanupEvidence(value) {
  requireRecord(value, 'test process owner cleanup evidence')
  if (!['completed', 'failed'].includes(value.outcome)) {
    throw new Error('test process owner cleanup outcome is unsupported')
  }
  if (value.outcome === 'failed') {
    exactKeys(value, ['outcome', 'failure_code', 'failure_message'], 'failed cleanup evidence')
    return Object.freeze({
      outcome: value.outcome,
      failureCode: requireNonemptyDiagnostic(value.failure_code, 'cleanup failure code'),
      failureMessage: requireNonemptyDiagnostic(value.failure_message, 'cleanup failure message'),
    })
  }
  exactKeys(value, ['outcome'], 'completed cleanup evidence')
  return Object.freeze({ outcome: value.outcome, failureCode: '', failureMessage: '' })
}

function parseFailureEvidence(value, label) {
  exactKeys(value, ['code', 'message'], label)
  return Object.freeze({
    code: requireNonemptyDiagnostic(value.code, `${label} code`),
    message: requireNonemptyDiagnostic(value.message, `${label} message`),
  })
}

function parsePlatformEvidence(
  value,
  platform,
  terminationReason,
  treeState,
  target,
  ownerFailure,
) {
  requireRecord(value, 'test process owner platform evidence')
  rejectUnknownKeys(value, [
    'kind', 'owner_pid', 'root', 'root_start_time_ticks', 'active_process_count',
    'inventory_scans', 'maximum_observed_descendants', 'quiet_inventory_count',
  ], 'test process owner platform evidence')
  const expectedKind = platform === 'win32' ? 'windows_job' : 'linux_subreaper'
  if (value.kind !== expectedKind) throw new Error('test process owner platform evidence is inconsistent')
  if (!Number.isSafeInteger(value.owner_pid) || value.owner_pid < 1) {
    throw new Error('test process owner PID evidence is invalid')
  }
  const root = Object.hasOwn(value, 'root') ? parseRootEvidence(value.root) : undefined
  const activeProcessCount = Object.hasOwn(value, 'active_process_count')
    ? requireNonnegativeInteger(value.active_process_count, 'active process count', 0xffff_ffff)
    : undefined
  validateTreeEvidence(treeState, activeProcessCount, root)
  validateTargetRootConsistency(target, root)
  for (const name of [
    'inventory_scans', 'maximum_observed_descendants', 'quiet_inventory_count',
  ]) {
    if (Object.hasOwn(value, name) && (!Number.isSafeInteger(value[name]) || value[name] < 0)) {
      throw new Error(`test process owner ${name} evidence is invalid`)
    }
  }
  if (platform === 'win32') {
    for (const name of [
      'root_start_time_ticks', 'inventory_scans', 'maximum_observed_descendants',
      'quiet_inventory_count',
    ]) {
      if (Object.hasOwn(value, name)) {
        throw new Error('Windows Job evidence contains Linux-only fields')
      }
    }
    if (target.outcome === 'signaled' || root?.state === 'signaled') {
      throw new Error('Windows Job evidence cannot report POSIX signal termination')
    }
    // A Windows owner creates the Job-contained root suspended, then authenticates the
    // consumer decision. Every controlled pre-release trigger can therefore terminate
    // a real root without ever releasing the requested program to execute.
    const terminatedBeforeRelease = target.outcome === 'not_started' &&
      ['stop', 'parent_lost', 'deadline', 'start_rejected'].includes(terminationReason)
    if (root !== undefined && (
      target.outcome === 'spawn_failed' || target.outcome === 'start_evidence_lost' ||
      (target.outcome === 'not_started' && !terminatedBeforeRelease)
    )) {
      throw new Error('Windows root creation contradicts target-not-created evidence')
    }
    if (target.exitCode !== undefined && (target.exitCode < 0 || target.exitCode > 0xffff_ffff)) {
      throw new Error('Windows target exit code is outside the DWORD range')
    }
    if (root?.exitCode !== undefined && (root.exitCode < 0 || root.exitCode > 0xffff_ffff)) {
      throw new Error('Windows root exit code is outside the DWORD range')
    }
  } else {
    validateLinuxPlatformEvidence(value, treeState, root)
  }
  if ((target.outcome === 'terminal_evidence_lost' || target.outcome === 'start_evidence_lost' ||
      root?.state === 'terminal_evidence_lost') && ownerFailure === undefined) {
    throw new Error('lost target or root evidence requires process owner failure evidence')
  }
  return Object.freeze({
    kind: value.kind,
    ownerPid: value.owner_pid,
    ...(root === undefined ? {} : { root }),
    ...(Object.hasOwn(value, 'root_start_time_ticks')
      ? { rootStartTimeTicks: value.root_start_time_ticks }
      : {}),
    ...(activeProcessCount === undefined ? {} : { activeProcessCount }),
    inventoryScans: value.inventory_scans ?? 0,
    maximumObservedDescendants: value.maximum_observed_descendants ?? 0,
    quietInventoryCount: value.quiet_inventory_count ?? 0,
  })
}

function parseRootEvidence(value) {
  requireRecord(value, 'test process owner root evidence')
  rejectUnknownKeys(value, ['pid', 'state', 'exit_code', 'signal'], 'test process owner root evidence')
  if (!Number.isSafeInteger(value.pid) || value.pid < 1) {
    throw new Error('test process owner root PID is invalid')
  }
  if (value.state === 'exited') {
    exactKeys(value, ['pid', 'state', 'exit_code'], 'exited root evidence')
    if (!Number.isSafeInteger(value.exit_code)) throw new Error('root exit code is invalid')
    return Object.freeze({ pid: value.pid, state: value.state, exitCode: value.exit_code })
  }
  if (value.state === 'signaled') {
    exactKeys(value, ['pid', 'state', 'signal'], 'signaled root evidence')
    return Object.freeze({
      pid: value.pid,
      state: value.state,
      signal: requireNonemptyDiagnostic(value.signal, 'root signal'),
    })
  }
  if (value.state !== 'active' && value.state !== 'terminal_evidence_lost') {
    throw new Error('test process owner root state is unsupported')
  }
  exactKeys(value, ['pid', 'state'], 'active or evidence-lost root evidence')
  return Object.freeze({ pid: value.pid, state: value.state })
}

function validateSettlementState(terminationReason, target, input, ownerFailure) {
  if (terminationReason === 'natural' && target.outcome !== 'exited' && target.outcome !== 'signaled') {
    throw new Error('natural termination requires exact target terminal evidence')
  }
  if (terminationReason === 'initialization_failed' &&
      !['spawn_failed', 'not_started', 'start_evidence_lost'].includes(target.outcome)) {
    throw new Error('initialization failure target evidence is inconsistent')
  }
  if (terminationReason === 'start_rejected') {
    if (target.outcome !== 'not_started') {
      throw new Error('start rejection requires exact target-not-started evidence')
    }
    if (ownerFailure !== undefined) {
      throw new Error('start rejection excludes process owner failure evidence')
    }
  }
  if (terminationReason === 'owner_failure' && ownerFailure === undefined) {
    throw new Error('owner-triggered termination requires process owner failure evidence')
  }
  if ((target.outcome === 'terminal_evidence_lost' || target.outcome === 'start_evidence_lost' ||
      input.outcome === 'evidence_lost') && ownerFailure === undefined) {
    throw new Error('lost target or input evidence requires process owner failure evidence')
  }
}

function validateRequestInputEvidence(stdin, target, input) {
  if (stdin === null) {
    if (input.outcome !== 'not_requested') {
      throw new Error('settlement input evidence contradicts an input-free request')
    }
    return
  }
  const [started, known] = targetStarted(target.outcome)
  if (known && !started) {
    if (input.outcome !== 'not_started') {
      throw new Error('known-unstarted target input requires not-started evidence')
    }
    return
  }
  if (known) {
    if (!['delivered', 'failed', 'evidence_lost'].includes(input.outcome)) {
      throw new Error('known-started target input lacks delivery evidence')
    }
    return
  }
  if (input.outcome !== 'not_started' && input.outcome !== 'evidence_lost') {
    throw new Error('unknown target start has inconsistent input evidence')
  }
}

function validateCleanupEvidence(treeState, cleanup, ownerFailure) {
  if (cleanup.outcome === 'completed') {
    if (treeState !== 'proven_empty') {
      throw new Error('completed cleanup requires a proven-empty tree')
    }
    return
  }
  if (ownerFailure === undefined) {
    throw new Error('failed cleanup requires process owner failure evidence')
  }
}

function validateTreeEvidence(treeState, activeProcessCount, root) {
  if (treeState === 'proven_empty') {
    if (activeProcessCount !== 0 || root?.state === 'active') {
      throw new Error('proven-empty tree evidence is inconsistent')
    }
    return
  }
  if (treeState === 'nonempty') {
    if (activeProcessCount === undefined || activeProcessCount === 0) {
      throw new Error('nonempty tree evidence requires a positive process count')
    }
    return
  }
  if (activeProcessCount !== undefined) {
    throw new Error('unknown tree evidence excludes an active process count')
  }
}

function validateTargetRootConsistency(target, root) {
  if (target.outcome === 'exited' &&
      (root?.state !== 'exited' || root.exitCode !== target.exitCode)) {
    throw new Error('exited target requires matching root exit evidence')
  }
  if (target.outcome === 'signaled' &&
      (root?.state !== 'signaled' || root.signal !== target.signal)) {
    throw new Error('signaled target requires matching root signal evidence')
  }
  if (target.outcome === 'terminal_evidence_lost' &&
      root !== undefined && root.state !== 'terminal_evidence_lost') {
    throw new Error('lost target terminal evidence contradicts exact root evidence')
  }
}

function validateLinuxPlatformEvidence(value, treeState, root) {
  if (root === undefined) {
    if (Object.hasOwn(value, 'root_start_time_ticks')) {
      throw new Error('Linux evidence without a root excludes root start-time ticks')
    }
    return
  }
  if (Object.hasOwn(value, 'root_start_time_ticks') || treeState !== 'unknown') {
    if (typeof value.root_start_time_ticks !== 'string' ||
        !/^[1-9][0-9]*$/u.test(value.root_start_time_ticks)) {
      throw new Error('created Linux root requires canonical positive start-time ticks')
    }
  }
  if (treeState === 'proven_empty' &&
      ((value.quiet_inventory_count ?? 0) < 2 ||
       (value.inventory_scans ?? 0) < (value.quiet_inventory_count ?? 0))) {
    throw new Error('proven-empty Linux tree requires repeated quiet inventory evidence')
  }
}

function targetStarted(outcome) {
  if (['exited', 'signaled', 'terminal_evidence_lost'].includes(outcome)) return [true, true]
  if (['spawn_failed', 'not_started'].includes(outcome)) return [false, true]
  return [false, false]
}

function requireNonemptyDiagnostic(value, label) {
  if (!validNFCText(value, false) || Buffer.byteLength(value, 'utf8') > MAXIMUM_DIAGNOSTIC_BYTES) {
    throw new Error(`${label} is invalid`)
  }
  return value
}

function canonicalEnvironmentEntries(environment) {
  requireRecord(environment, 'test process owner environment')
  const entries = Object.entries(environment)
    .filter(([, value]) => value !== undefined)
    .map(([name, value]) => {
      const foldedName = asciiFold(name)
      if (!validNFCText(name, false) || name.includes('=') || !validNFCText(value, true) ||
          isProcessOwnerReservedEnvironmentName(name)) {
        throw new Error('test process owner environment contains an invalid entry')
      }
      return Object.freeze({ name, value })
    })
    .sort((left, right) => compareUtf8(asciiFold(left.name), asciiFold(right.name)))
  for (let index = 1; index < entries.length; index += 1) {
    if (asciiFold(entries[index - 1].name) === asciiFold(entries[index].name)) {
      throw new Error('test process owner environment contains an ASCII-fold duplicate name')
    }
  }
  return Object.freeze(entries)
}

function controlRecord(identity, signal) {
  return Object.freeze({
    schema_version: CONTROL_SCHEMA,
    run_id: identity.runId,
    operation_id: identity.operationId,
    scenario: identity.scenario,
    reason: signal?.reason instanceof TestProcessOwnerDeadlineError ? 'deadline' : 'stop',
  })
}

function asciiFold(value) {
  return value.replace(/[A-Z]/gu, (character) => character.toLowerCase())
}

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'))
}

function drainPipe(pipe, channel, label) {
  if (pipe === null) throw new Error(`${label} pipe is unavailable`)
  return new Promise((resolveDrain, rejectDrain) => {
    let failure
    let ended = false
    pipe.on('data', (chunk) => {
      // Capture is owner-created and bounded. It never calls or awaits consumer
      // code, so output observation cannot become target backpressure.
      channel.append(Buffer.from(chunk))
    })
    pipe.once('error', (cause) => {
      failure ??= new Error(`${label} failed`, { cause })
      channel.fail(failure)
    })
    pipe.once('end', () => {
      ended = true
    })
    pipe.once('close', () => {
      if (!ended) {
        failure ??= new Error(`${label} closed before EOF`)
        channel.fail(failure)
      }
      channel.finish()
      const drainFailure = aggregateErrors(`${label} did not drain cleanly`, [failure, channel.failure()])
      if (drainFailure === undefined) resolveDrain()
      else rejectDrain(drainFailure)
    })
  })
}

function drainDiscardPipe(pipe, label) {
  if (pipe === null) throw new Error(`${label} pipe is unavailable`)
  return new Promise((resolveDrain, rejectDrain) => {
    let failure
    let ended = false
    pipe.on('data', () => {
      // The descriptor remains live to preserve the fixed owner ABI, but a
      // disabled capture grants no retention authority for event bytes.
    })
    pipe.once('error', (cause) => {
      failure ??= new Error(`${label} failed`, { cause })
    })
    pipe.once('end', () => {
      ended = true
    })
    pipe.once('close', () => {
      if (!ended) failure ??= new Error(`${label} closed before EOF`)
      if (failure === undefined) resolveDrain()
      else rejectDrain(failure)
    })
  })
}

function drainWindowsOwnerOutput(pipe, channel) {
  if (pipe === null) throw new Error('test process owner stdout pipe is unavailable')
  let resolveReady
  let rejectReady
  const ready = new Promise((resolve, reject) => {
    resolveReady = resolve
    rejectReady = reject
  })
  const completion = new Promise((resolveDrain, rejectDrain) => {
    let readyObserved = false
    let failure
    let ended = false
    pipe.on('data', (value) => {
      let chunk = Buffer.from(value)
      if (!readyObserved && chunk.byteLength > 0) {
        readyObserved = true
        if (chunk[0] !== OWNER_READY_BYTE) {
          failure = new Error('Windows test process owner readiness byte is invalid')
          channel.fail(failure)
          rejectReady(failure)
        } else {
          resolveReady()
        }
        chunk = chunk.subarray(1)
      }
      if (chunk.byteLength !== 0 && failure === undefined) channel.append(chunk)
    })
    pipe.once('error', (cause) => {
      failure ??= new Error('Windows test process owner stdout failed', { cause })
      channel.fail(failure)
      if (!readyObserved) rejectReady(failure)
    })
    pipe.once('end', () => {
      ended = true
    })
    pipe.once('close', () => {
      if (!readyObserved) {
        failure ??= new Error('Windows test process owner closed before readiness')
        channel.fail(failure)
        rejectReady(failure)
      }
      if (!ended) {
        failure ??= new Error('Windows test process owner stdout closed before EOF')
        channel.fail(failure)
      }
      channel.finish()
      const drainFailure = aggregateErrors(
        'Windows test process owner stdout did not drain cleanly',
        [failure, channel.failure()],
      )
      if (drainFailure === undefined) resolveDrain()
      else rejectDrain(drainFailure)
    })
  })
  return Object.freeze({ ready, completion })
}

function drainEventPipe(pipe, identity, channel) {
  if (pipe === null) throw new Error('private test-event pipe is unavailable')
  const decoder = new TestEventDecoder(identity, channel)
  return new Promise((resolveDrain, rejectDrain) => {
    let ended = false
    pipe.on('data', (chunk) => decoder.push(Buffer.from(chunk)))
    pipe.once('error', (cause) => decoder.fail(new Error('private test-event pipe failed', { cause })))
    pipe.once('end', () => {
      ended = true
    })
    pipe.once('close', () => {
      try {
        if (!ended) decoder.fail(new Error('private test-event pipe closed before EOF'))
        decoder.finish()
        resolveDrain()
      } catch (cause) {
        rejectDrain(cause)
      }
    })
  })
}

async function openWindowsEndpointSet(withInput, identity, captures) {
  const endpoints = []
  try {
    const status = await openWindowsStatusEndpoint()
    endpoints.push(status)
    const control = await openWindowsPipeEndpoint('control')
    endpoints.push(control)
    const parent = await openWindowsPipeEndpoint('parent-liveness')
    endpoints.push(parent)
    const startEvidence = await openWindowsStartEvidenceEndpoint()
    endpoints.push(startEvidence)
    const startDecision = await openWindowsPipeEndpoint('start-decision')
    endpoints.push(startDecision)
    const input = withInput ? await openWindowsPipeEndpoint('raw-input') : undefined
    if (input !== undefined) endpoints.push(input)
    const event = !captures.eventEnabled
      ? undefined
      : await openWindowsEventEndpoint(identity, captures.events)
    if (event !== undefined) endpoints.push(event)
    return Object.freeze({
      status,
      control,
      parent,
      input,
      event,
      startEvidence,
      startDecision,
      endpoints: Object.freeze(endpoints),
    })
  } catch (cause) {
    const cleanup = await Promise.allSettled(endpoints.map((endpoint) => endpoint.close()))
    throw aggregateErrors(
      'acquire invocation-private Windows process-owner endpoints',
      [cause, ...cleanup.filter((result) => result.status === 'rejected').map((result) => result.reason)],
    )
  }
}

async function openWindowsStatusEndpoint() {
  const chunks = []
  let byteLength = 0
  const endpoint = await openWindowsPipeEndpoint('settlement', {
    onData: (chunk) => {
      byteLength += chunk.byteLength
      if (byteLength > MAXIMUM_DOCUMENT_BYTES + 1) {
        chunks.length = 0
        throw new Error('test process owner settlement exceeds its byte limit')
      }
      chunks.push(Buffer.from(chunk))
    },
  })
  return Object.freeze({
    ...endpoint,
    bytes: () => Buffer.concat(chunks),
  })
}

async function openWindowsStartEvidenceEndpoint() {
  const chunks = []
  let byteLength = 0
  const endpoint = await openWindowsPipeEndpoint('start-evidence', {
    requireEOF: true,
    onData: (chunk) => {
      byteLength += chunk.byteLength
      if (byteLength > MAXIMUM_DOCUMENT_BYTES + 5) {
        chunks.length = 0
        throw new Error('test process owner start evidence exceeds its byte limit')
      }
      chunks.push(Buffer.from(chunk))
    },
  })
  return Object.freeze({
    ...endpoint,
    bytes: () => Buffer.concat(chunks),
  })
}

async function openWindowsPipeEndpoint(label, { onData, onFinish, requireEOF = false } = {}) {
  const path = `\\\\.\\pipe\\windshare-owner-${label}-${process.pid}-${randomBytes(16).toString('hex')}`
  let socket
  let outbound
  let failure
  let ended = false
  let resolveConnected
  let rejectConnected
  let resolveCompletion
  let rejectCompletion
  let connectionSettled = false
  let completionSettled = false
  const connected = new Promise((resolve, reject) => {
    resolveConnected = resolve
    rejectConnected = reject
  })
  const completion = new Promise((resolve, reject) => {
    resolveCompletion = resolve
    rejectCompletion = reject
  })
  observePromise(connected)
  observePromise(completion)
  const server = createServer((candidate) => {
    if (socket !== undefined) {
      candidate.destroy()
      return
    }
    socket = candidate
    connectionSettled = true
    resolveConnected()
    // A random per-invocation name is a rendezvous, not ambient authority.
    // Retiring the listener after one connection fences accidental reuse.
    server.close()
    candidate.on('data', (chunk) => {
      if (failure !== undefined) return
      try {
        if (onData === undefined && chunk.byteLength !== 0) {
          throw new Error(`private Windows ${label} pipe carried unexpected data`)
        }
        onData?.(Buffer.from(chunk))
      } catch (cause) {
        failure = new Error(`private Windows ${label} pipe failed`, { cause })
      }
    })
    candidate.once('error', (cause) => {
      failure ??= new Error(`private Windows ${label} pipe failed`, { cause })
    })
    candidate.once('end', () => {
      ended = true
    })
    candidate.once('close', () => {
      if (completionSettled) return
      completionSettled = true
      try {
        if (requireEOF && !ended) {
          failure ??= new Error(`private Windows ${label} pipe closed before EOF`)
        }
        onFinish?.()
        if (failure === undefined) resolveCompletion()
        else rejectCompletion(failure)
      } catch (cause) {
        rejectCompletion(new Error(`private Windows ${label} pipe failed`, { cause }))
      }
    })
    if (outbound !== undefined) candidate.end(outbound)
  })
  const listening = new Promise((resolveListen, rejectListen) => {
    server.once('error', rejectListen)
    server.listen(path, () => {
      server.off('error', rejectListen)
      resolveListen()
    })
  })
  server.on('error', (cause) => {
    if (!connectionSettled) {
      connectionSettled = true
      rejectConnected(new Error(`private Windows ${label} listener failed`, { cause }))
    }
    if (!completionSettled) {
      completionSettled = true
      rejectCompletion(new Error(`private Windows ${label} listener failed`, { cause }))
    }
  })
  await listening
  return Object.freeze({
    label,
    path,
    connected,
    completion,
    end: (bytes) => {
      if (!(bytes instanceof Uint8Array)) {
        throw new Error(`private Windows ${label} output must be bytes`)
      }
      if (outbound !== undefined) {
        throw new Error(`private Windows ${label} output was already published`)
      }
      outbound = Buffer.from(bytes)
      if (socket !== undefined) socket.end(outbound)
    },
    close: async () => {
      socket?.destroy()
      if (!connectionSettled) {
        connectionSettled = true
        rejectConnected(new Error(`test process owner never connected its private ${label} endpoint`))
      }
      if (!completionSettled) {
        completionSettled = true
        rejectCompletion(new Error(`private Windows ${label} stream did not settle`))
      }
      await closeServer(server)
    },
  })
}

async function openWindowsEventEndpoint(identity, channel) {
  const path = `\\\\.\\pipe\\windshare-test-event-${process.pid}-${randomBytes(16).toString('hex')}`
  const decoder = new TestEventDecoder(identity, channel)
  let socket
  let resolveConnected
  let rejectConnected
  let resolveCompletion
  let rejectCompletion
  let connectionSettled = false
  let completionSettled = false
  let ended = false
  const connected = new Promise((resolve, reject) => {
    resolveConnected = resolve
    rejectConnected = reject
  })
  const completion = new Promise((resolve, reject) => {
    resolveCompletion = resolve
    rejectCompletion = reject
  })
  observePromise(connected)
  observePromise(completion)
  const server = createServer((candidate) => {
    if (socket !== undefined) {
      candidate.destroy()
      return
    }
    socket = candidate
    connectionSettled = true
    resolveConnected()
    // Exactly one owner is authorized to connect to an invocation-private name.
    // Closing the listener prevents a same-account peer from racing a second stream.
    server.close()
    candidate.on('data', (chunk) => decoder.push(Buffer.from(chunk)))
    candidate.once('error', (cause) => {
      decoder.fail(new Error('private Windows test-event pipe failed', { cause }))
    })
    candidate.once('end', () => {
      ended = true
    })
    candidate.once('close', () => {
      if (completionSettled) return
      completionSettled = true
      try {
        if (!ended) decoder.fail(new Error('private Windows test-event pipe closed before EOF'))
        decoder.finish()
        resolveCompletion()
      } catch (cause) {
        rejectCompletion(cause)
      }
    })
  })
  const listening = new Promise((resolveListen, rejectListen) => {
    server.once('error', rejectListen)
    server.listen(path, () => {
      server.off('error', rejectListen)
      resolveListen()
    })
  })
  server.on('error', (cause) => {
    decoder.fail(new Error('private Windows test-event listener failed', { cause }))
    if (!connectionSettled) {
      connectionSettled = true
      rejectConnected(new Error('private Windows test-event listener failed', { cause }))
    }
    if (!completionSettled) {
      completionSettled = true
      rejectCompletion(new Error('private Windows test-event listener failed', { cause }))
    }
  })
  await listening
  return Object.freeze({
    label: 'test-event',
    path,
    connected,
    completion,
    close: async () => {
      socket?.destroy()
      if (!connectionSettled) {
        decoder.fail(new Error('test process owner never connected its private event endpoint'))
        connectionSettled = true
        rejectConnected(new Error('test process owner never connected its private event endpoint'))
      }
      if (!completionSettled) {
        completionSettled = true
        rejectCompletion(new Error('private Windows test-event stream did not settle'))
      }
      await closeServer(server)
    },
  })
}

class TestEventDecoder {
  #buffer = Buffer.alloc(0)
  #failure

  constructor(identity, channel) {
    this.identity = identity
    this.channel = channel
  }

  push(chunk) {
    if (this.#failure !== undefined) return
    this.#buffer = Buffer.concat([this.#buffer, chunk])
    while (true) {
      const terminator = this.#buffer.indexOf(0x0a)
      if (terminator < 0) break
      const line = this.#buffer.subarray(0, terminator)
      this.#buffer = this.#buffer.subarray(terminator + 1)
      if (line.byteLength < 1 || line.byteLength > MAXIMUM_DOCUMENT_BYTES) {
        this.fail(new Error('private test-event line is empty or oversized'))
        return
      }
      try {
        this.channel.append(parseTestEvent(decodeCanonicalDocument(line), this.identity))
      } catch (cause) {
        this.fail(new Error('private test-event capture failed', { cause }))
        return
      }
    }
    if (this.#buffer.byteLength > MAXIMUM_DOCUMENT_BYTES) {
      this.fail(new Error('private test-event line exceeds its byte limit'))
    }
  }

  fail(cause) {
    this.#failure ??= cause
    this.#buffer = Buffer.alloc(0)
    this.channel.fail(this.#failure)
  }

  finish() {
    if (this.#buffer.byteLength !== 0) this.fail(new Error('private test-event stream ended with a truncated line'))
    this.channel.finish()
    const failure = aggregateErrors(
      'private test-event stream did not settle cleanly',
      [this.#failure, this.channel.failure()],
    )
    if (failure !== undefined) throw failure
  }
}

function parseTestEvent(value, identity) {
  requireRecord(value, 'private test event')
  rejectUnknownKeys(value, [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'component', 'milestone', 'outcome',
    'payload',
  ], 'private test event')
  for (const key of [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'component', 'milestone', 'outcome',
  ]) {
    if (!Object.hasOwn(value, key)) throw new Error('private test event is missing a required field')
  }
  if (value.schema_version !== EVENT_SCHEMA || value.run_id !== identity.runId ||
      value.operation_id !== identity.operationId || value.scenario !== identity.scenario) {
    throw new Error('private test event identity differs from its owned process')
  }
  requireEventField(value.component, 'event component')
  requireEventField(value.milestone, 'event milestone')
  if (!['started', 'succeeded', 'failed'].includes(value.outcome)) {
    throw new Error('private test event outcome is unsupported')
  }
  return Object.freeze({
    schemaVersion: EVENT_SCHEMA,
    runId: value.run_id,
    operationId: value.operation_id,
    scenario: value.scenario,
    component: value.component,
    milestone: value.milestone,
    outcome: value.outcome,
    ...(Object.hasOwn(value, 'payload') ? { payload: deepFreeze(value.payload) } : {}),
  })
}

function closeServer(server) {
  return new Promise((resolveClose, rejectClose) => {
    if (!server.listening) {
      resolveClose()
      return
    }
    server.close((cause) => {
      if (cause === undefined) resolveClose()
      else rejectClose(cause)
    })
  })
}

function observePromise(task) {
  if (task !== undefined) void task.catch(() => undefined)
}

function trackTransportTask(label, task) {
  const state = { status: 'pending' }
  const observed = Promise.resolve(task).then(
    (value) => {
      state.status = 'fulfilled'
      state.value = value
      return value
    },
    (cause) => {
      state.status = 'rejected'
      state.reason = cause
      throw cause
    },
  )
  observePromise(observed)
  return Object.freeze({ label, state, task: observed })
}

async function waitForTrackedTransport(tracked, lease) {
  const joined = Promise.all(tracked.map((entry) => entry.task.then(
    () => undefined,
    () => undefined,
  )))
  try {
    await Promise.race([joined, lease.expired])
    return undefined
  } catch (cause) {
    return cause
  }
}

function snapshotStartGate(entry) {
  if (entry === undefined) return undefined
  if (entry.state.status === 'fulfilled') {
    return Object.freeze({ status: 'fulfilled', evidence: entry.state.value })
  }
  return Object.freeze({ status: entry.state.status })
}

function snapshotTrackedTransport(tracked) {
  const failures = []
  const pending = []
  for (const entry of tracked) {
    if (entry.state.status === 'rejected') {
      failures.push(new Error(`${entry.label} failed`, { cause: entry.state.reason }))
    } else if (entry.state.status === 'pending') {
      pending.push(entry.label)
    }
  }
  if (pending.length !== 0) {
    failures.push(new Error(`transport tasks did not join: ${pending.join(', ')}`))
  }
  return Object.freeze({ failures: Object.freeze(failures) })
}

function aggregateErrors(message, values) {
  const failures = []
  const observed = new Set()
  for (const value of values) {
    if (value === undefined || value === null || observed.has(value)) continue
    observed.add(value)
    failures.push(value instanceof Error ? value : new Error(String(value)))
  }
  if (failures.length === 0) return undefined
  return new AggregateError(failures, message)
}

function collectRejectedPromises(destination, outcomes, label) {
  for (const outcome of outcomes) {
    if (outcome.status === 'rejected') {
      destination.push(new Error(label, { cause: outcome.reason }))
    }
  }
}

function boundedPipe(
  pipe,
  label,
  maximumBytes = MAXIMUM_DOCUMENT_BYTES + 1,
) {
  if (pipe === null) throw new Error(`${label} pipe is unavailable`)
  return new Promise((resolvePipe, rejectPipe) => {
    const chunks = []
    let byteLength = 0
    let failure
    let ended = false
    let settled = false
    pipe.on('data', (chunk) => {
      if (failure !== undefined) return
      byteLength += chunk.byteLength
      if (byteLength > maximumBytes) {
        failure = new Error(`${label} exceeds its byte limit`)
        chunks.length = 0
        return
      }
      chunks.push(Buffer.from(chunk))
    })
    pipe.once('error', (cause) => { failure ??= new Error(`${label} failed`, { cause }) })
    pipe.once('end', () => {
      ended = true
      settled = true
      if (failure === undefined) resolvePipe(Buffer.concat(chunks))
      else rejectPipe(failure)
    })
    pipe.once('close', () => {
      if (settled || ended) return
      settled = true
      rejectPipe(failure ?? new Error(`${label} closed before EOF`))
    })
  })
}

async function publishPipeBytesAndClose(pipe, bytes, label) {
  const completion = waitForExactWritableCompletion(pipe, label)
  observePromise(completion)
  try {
    pipe.end(bytes)
  } catch (cause) {
    pipe.destroy(cause instanceof Error ? cause : new Error(String(cause)))
    throw new Error(`${label} failed before publication`, { cause })
  }
  await completion
}

function decodeStatusLine(bytes) {
  if (bytes.byteLength < 2 || bytes[bytes.byteLength - 1] !== 0x0a) {
    throw new Error('test process owner status is not one terminated JSON line')
  }
  return decodeCanonicalDocument(bytes.subarray(0, bytes.byteLength - 1))
}

function decodeExactFrame(bytes, label) {
  if (bytes.byteLength < 4) throw new Error(`${label} frame is truncated`)
  const payloadLength = bytes.readUInt32BE(0)
  if (payloadLength < 1 || payloadLength > MAXIMUM_DOCUMENT_BYTES) {
    throw new Error(`${label} frame length is invalid`)
  }
  const expectedLength = 4 + payloadLength
  if (bytes.byteLength < expectedLength) throw new Error(`${label} frame is truncated`)
  if (bytes.byteLength > expectedLength) throw new Error(`${label} stream contains trailing bytes`)
  return decodeCanonicalDocument(bytes.subarray(4), label)
}

function decodeCanonicalDocument(bytes, label = 'test process owner settlement') {
  if (bytes.byteLength < 1 || bytes.byteLength > MAXIMUM_DOCUMENT_BYTES) {
    throw new Error(`${label} is empty or oversized`)
  }
  const encoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error(`${label} is not JSON`, { cause })
  }
  requireJSONUnicodeScalars(value)
  if (canonicalJSONString(value) !== encoded) {
    throw new Error(`${label} is not canonical JSON`)
  }
  return value
}

function canonicalJSONString(value) {
  const encoded = JSON.stringify(value)
  if (encoded === undefined) throw new Error('test process owner document is not JSON-encodable')
  return encoded
}

function frame(value) {
  const payload = Buffer.from(canonicalJSONString(value), 'utf8')
  if (payload.byteLength < 1 || payload.byteLength > MAXIMUM_DOCUMENT_BYTES) {
    throw new Error('test process owner frame is empty or oversized')
  }
  const header = Buffer.alloc(4)
  header.writeUInt32BE(payload.byteLength)
  return Buffer.concat([header, payload])
}

function listenForAbort(signal, stop) {
  if (signal === undefined) return () => undefined
  const abort = () => stop()
  if (signal.aborted) abort()
  else signal.addEventListener('abort', abort, { once: true })
  return () => signal.removeEventListener('abort', abort)
}

function childTerminal(child) {
  return new Promise((resolveTerminal, rejectTerminal) => {
    child.once('error', rejectTerminal)
    child.once('close', (code, signal) => resolveTerminal(Object.freeze({ code, signal })))
  })
}

function rejectOwnerExitBeforeReadiness(terminalTask) {
  return terminalTask.then((terminal) => {
    throw new Error(ownerTerminalFailureMessage(terminal) + ' before readiness')
  })
}

function ownerTransportLease(milliseconds, message = 'test process owner exceeded its bounded transport lease') {
  let active = true
  let timer
  const expired = new Promise((_, rejectExpired) => {
    timer = setTimeout(() => {
      if (!active) return
      active = false
      rejectExpired(new Error(message))
    }, milliseconds)
  })
  observePromise(expired)
  return Object.freeze({
    expired,
    cancel: () => {
      if (!active) return
      active = false
      clearTimeout(timer)
    },
  })
}

async function retireLinuxOwner(
  child,
  terminalTask,
  control,
  existingControlCompletion,
  endControl,
  terminationGraceMilliseconds,
) {
  const retirementMilliseconds =
    (2 * terminationGraceMilliseconds) + LINUX_GUARDIAN_MARGIN_MILLISECONDS
  if (child.exitCode !== null || child.signalCode !== null) {
    control.destroy()
    if (existingControlCompletion === undefined) return undefined
    const joined = await Promise.race([
      Promise.allSettled([existingControlCompletion]).then(() => true),
      delay(retirementMilliseconds).then(() => false),
    ])
    return joined ? undefined : new Error('Linux test process owner control publication did not join')
  }
  // EOF transfers retirement to the independent guardian. Killing that guardian
  // would discard its only authenticated settlement and empty-tree proof.
  const controlCompletion = endControl()
  const retired = await Promise.race([
    Promise.allSettled([terminalTask, controlCompletion]).then(() => true),
    delay(retirementMilliseconds).then(() => false),
  ])
  if (retired) {
    control.destroy()
    return undefined
  }
  for (const pipe of child.stdio) pipe?.destroy()
  child.unref()
  return new Error('Linux test process guardian exceeded its bounded retirement lease')
}

function delay(milliseconds) {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds))
}

function requireOwner(value) {
  if (typeof value !== 'object' || value === null) {
    throw new Error('test process owner artifact is invalid')
  }
  exactKeys(value, ['path'], 'test process owner artifact')
  if (!isAbsolute(value.path) || resolve(value.path) !== value.path) {
    throw new Error('test process owner artifact is invalid')
  }
  return Object.freeze({ path: value.path })
}

function requireCommand(value) {
  if (typeof value !== 'object' || value === null || !validNFCText(value.executable, false) ||
      !isAbsolute(value.executable) ||
      resolve(value.executable) !== value.executable || !isAbsolute(value.cwd) ||
      !validNFCText(value.cwd, false) || resolve(value.cwd) !== value.cwd ||
      !Array.isArray(value.arguments) ||
      value.arguments.some((argument) => !validNFCText(argument, true))) {
    throw new Error('test process owner command is invalid')
  }
  if (value.stdin !== undefined && (!(value.stdin instanceof Uint8Array) ||
      value.stdin.byteLength < 1 || value.stdin.byteLength > MAXIMUM_DOCUMENT_BYTES)) {
    throw new Error('test process owner raw input is invalid')
  }
}

function validNFCText(value, allowEmpty) {
  return typeof value === 'string' && (allowEmpty || value !== '') &&
    !value.includes('\0') && hasOnlyUnicodeScalars(value) && value.normalize('NFC') === value
}

function hasOnlyUnicodeScalars(value) {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (index + 1 >= value.length) return false
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return true
}

function requireJSONUnicodeScalars(value) {
  if (typeof value === 'string') {
    if (!hasOnlyUnicodeScalars(value)) {
      throw new Error('test process owner document contains an unpaired UTF-16 surrogate')
    }
    return
  }
  if (Array.isArray(value)) {
    for (const entry of value) requireJSONUnicodeScalars(entry)
    return
  }
  if (typeof value !== 'object' || value === null) return
  for (const [name, entry] of Object.entries(value)) {
    if (!hasOnlyUnicodeScalars(name)) {
      throw new Error('test process owner document contains an unpaired UTF-16 surrogate')
    }
    requireJSONUnicodeScalars(entry)
  }
}

function deepFreeze(value, visited = new Set()) {
  if (value === null || typeof value !== 'object' || visited.has(value)) return value
  visited.add(value)
  for (const nested of Object.values(value)) deepFreeze(nested, visited)
  return Object.freeze(value)
}

function requireEventField(value, label) {
  if (
    typeof value !== 'string' || Buffer.byteLength(value, 'utf8') < 1 ||
    Buffer.byteLength(value, 'utf8') > MAXIMUM_EVENT_FIELD_BYTES ||
    !/^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$/u.test(value)
  ) throw new Error(`test process owner ${label} is invalid`)
  return value
}

function ownerTerminalSucceeded(terminal) {
  return terminal !== undefined && terminal.code === 0 && terminal.signal === null
}

function ownerTerminalFailureMessage(terminal) {
  if (terminal === undefined) return 'test process owner returned no terminal process evidence'
  return terminal.signal === null
    ? `test process owner exited with code ${terminal.code}`
    : `test process owner terminated by signal ${terminal.signal}`
}

function assertOwnerArtifactLive(owner, expectedIdentity) {
  const metadata = lstatSync(owner.path, { bigint: true })
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1n) {
    throw new Error('test process owner artifact is not a stable regular file')
  }
  const identity = Object.freeze({
    dev: metadata.dev,
    ino: metadata.ino,
    size: metadata.size,
    mtimeNs: metadata.mtimeNs,
    ctimeNs: metadata.ctimeNs,
  })
  if (expectedIdentity !== undefined && (
    identity.dev !== expectedIdentity.dev || identity.ino !== expectedIdentity.ino ||
    identity.size !== expectedIdentity.size || identity.mtimeNs !== expectedIdentity.mtimeNs ||
    identity.ctimeNs !== expectedIdentity.ctimeNs
  )) {
    throw new Error('test process owner artifact changed while used')
  }
  return identity
}

function requireBoundedPositiveInteger(value, maximum, label) {
  if (!Number.isSafeInteger(value) || value < 1 || value > maximum) {
    throw new Error(`${label} must be an integer in [1, ${maximum}]`)
  }
}

function requireCanonicalPositiveUint64(value, label) {
  if (typeof value !== 'string' || !/^[1-9][0-9]*$/u.test(value) ||
      value.length > 20 || BigInt(value) > MAXIMUM_UINT64) {
    throw new Error(`${label} must be a canonical positive uint64`)
  }
  return value
}

function requireNonnegativeInteger(value, label, maximum = Number.MAX_SAFE_INTEGER) {
  if (!Number.isSafeInteger(value) || value < 0 || value > maximum) {
    throw new Error(`${label} must be an integer in [0, ${maximum}]`)
  }
  return value
}

function requireRecord(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
}

function exactKeys(value, keys, label) {
  requireRecord(value, label)
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function exactOptionalKeys(value, required, optional, label) {
  requireRecord(value, label)
  const allowed = new Set([...required, ...optional])
  if (required.some((key) => !Object.hasOwn(value, key)) ||
      Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error(`${label} does not have its required exact key set`)
  }
}

function rejectUnknownKeys(value, keys, label) {
  const allowed = new Set(keys)
  if (Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error(`${label} contains unknown keys`)
  }
}
