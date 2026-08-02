import { spawn } from 'node:child_process'
import { isAbsolute, resolve } from 'node:path'
import { finished } from 'node:stream/promises'

import {
  createOwnedByteChannel,
  createOwnedEventChannel,
  normalizeOwnedProcessCapture,
} from './owned-process-channel.mjs'
import { parseTestIdentity } from './test-identity.mjs'
import { drainTestEventStream } from './test-event-channel.mjs'

const EVENT_DESCRIPTOR = 3
const EVENT_DESCRIPTOR_ENVIRONMENT = 'WINDSHARE_TEST_EVENT_FD'
const EVENT_HANDLE_ENVIRONMENT = 'WINDSHARE_TEST_EVENT_HANDLE'
const RUN_ID_ENVIRONMENT = 'WINDSHARE_TEST_RUN_ID'
const OPERATION_ID_ENVIRONMENT = 'WINDSHARE_TEST_OPERATION_ID'
const SCENARIO_ENVIRONMENT = 'WINDSHARE_TEST_SCENARIO'
const SUPPORTED_PLATFORMS = Object.freeze(['linux', 'win32'])

export class InheritedChildProcessError extends Error {
  constructor(message, terminal, output, events, cause) {
    super(message, { cause })
    this.name = 'InheritedChildProcessError'
    this.terminal = terminal
    this.output = output
    this.events = events
  }
}

export function createInheritedChildProcessBackend({
  platform = process.platform,
  spawnProcess = spawn,
} = {}) {
  if (!SUPPORTED_PLATFORMS.includes(platform)) {
    throw new Error(`inherited child processes are unsupported on ${JSON.stringify(platform)}`)
  }
  if (typeof spawnProcess !== 'function') {
    throw new Error('inherited child process spawner must be a function')
  }
  return Object.freeze({
    kind: 'inherited-descendant',
    launch: (request) => launchInheritedChild(request, platform, spawnProcess),
  })
}

export function inheritedChildEnvironment(environment, identity, eventsEnabled) {
  const parsedIdentity = parseTestIdentity(identity)
  requireRecord(environment, 'inherited child environment')
  const result = {}
  for (const [name, value] of Object.entries(environment)) {
    if (name === '' || name.includes('=') || name.includes('\0') ||
        typeof value !== 'string' || value.includes('\0')) {
      throw new Error('inherited child environment contains an invalid entry')
    }
    const folded = asciiFold(name)
    if (folded === asciiFold(EVENT_DESCRIPTOR_ENVIRONMENT) ||
        folded === asciiFold(EVENT_HANDLE_ENVIRONMENT)) {
      throw new Error('inherited child environment attempts to reuse an outer event capability')
    }
    if (
      (folded === asciiFold(RUN_ID_ENVIRONMENT) && value !== parsedIdentity.runId) ||
      (folded === asciiFold(OPERATION_ID_ENVIRONMENT) && value !== parsedIdentity.operationId) ||
      (folded === asciiFold(SCENARIO_ENVIRONMENT) && value !== parsedIdentity.scenario)
    ) throw new Error('inherited child environment contradicts its process identity')
    if (![RUN_ID_ENVIRONMENT, OPERATION_ID_ENVIRONMENT, SCENARIO_ENVIRONMENT]
      .some((reserved) => folded === asciiFold(reserved))) {
      if (Object.keys(result).some((existing) => asciiFold(existing) === folded)) {
        throw new Error('inherited child environment contains duplicate folded names')
      }
      result[name] = value
    }
  }
  result[RUN_ID_ENVIRONMENT] = parsedIdentity.runId
  result[OPERATION_ID_ENVIRONMENT] = parsedIdentity.operationId
  result[SCENARIO_ENVIRONMENT] = parsedIdentity.scenario
  if (eventsEnabled) result[EVENT_DESCRIPTOR_ENVIRONMENT] = String(EVENT_DESCRIPTOR)
  return Object.freeze(result)
}

function launchInheritedChild(request, platform, spawnProcess) {
  const parsed = parseLaunchRequest(request)
  const eventsEnabled = parsed.events !== undefined
  const stdout = createOwnedByteChannel(parsed.capture.stdoutBytes, 'inherited child stdout')
  const stderr = createOwnedByteChannel(parsed.capture.stderrBytes, 'inherited child stderr')
  const environment = inheritedChildEnvironment(parsed.environment, parsed.identity, eventsEnabled)
  const stdio = eventsEnabled
    ? Object.freeze(['ignore', 'pipe', 'pipe', 'pipe'])
    : Object.freeze(['ignore', 'pipe', 'pipe'])
  const child = spawnProcess(parsed.command.executable, [...parsed.command.arguments], {
    cwd: parsed.command.cwd,
    detached: false,
    env: environment,
    shell: false,
    stdio,
    windowsHide: true,
  })
  if (child?.stdout === null || child?.stdout === undefined ||
      child.stderr === null || child.stderr === undefined) {
    child?.kill?.('SIGTERM')
    throw new Error('inherited child diagnostic pipes were not created')
  }
  const stdoutDrain = drainDiagnosticStream(child.stdout, stdout, 'inherited child stdout')
  const stderrDrain = drainDiagnosticStream(child.stderr, stderr, 'inherited child stderr')
  let eventDrain = Promise.resolve(0)
  let eventChannel
  if (eventsEnabled) {
    const eventStream = child.stdio?.[EVENT_DESCRIPTOR]
    if (eventStream === null || eventStream === undefined) {
      child.kill?.('SIGTERM')
      throw new Error('inherited child private event pipe was not created')
    }
    const eventSession = drainTestEventStream(eventStream, {
      identity: parsed.identity,
      minimumEvents: parsed.events.minimumEvents,
      maximumEvents: parsed.events.maximumEvents,
    })
    eventChannel = eventSession.events
    eventDrain = eventSession.completion
  } else {
    const disabledEvents = createOwnedEventChannel(0, 'disabled inherited child events')
    disabledEvents.finish()
    eventChannel = disabledEvents.view
  }
  observePromise(stdoutDrain)
  observePromise(stderrDrain)
  observePromise(eventDrain)
  const terminal = childTerminal(child)
  const closed = childClosed(child)
  observePromise(terminal)
  observePromise(closed)
  const completion = settleInheritedChild(
    [terminal, closed, stdoutDrain, stderrDrain, eventDrain],
    stdout,
    stderr,
    eventChannel,
  )
  observePromise(completion)
  let stopRequested = false
  return Object.freeze({
    kind: 'inherited-descendant',
    platform,
    terminal,
    stdout: stdout.view,
    stderr: stderr.view,
    events: eventChannel,
    completion,
    requestStop() {
      if (stopRequested || child.exitCode !== null || child.signalCode !== null) return
      stopRequested = true
      if (child.kill('SIGTERM') !== true) {
        throw new Error('inherited child rejected its direct stop request')
      }
    },
  })
}

function parseLaunchRequest(request) {
  requireRecord(request, 'inherited child launch request')
  const identity = parseTestIdentity(request.identity)
  requireRecord(request.command, 'inherited child command')
  if (!isAbsolute(request.command.executable) ||
      resolve(request.command.executable) !== request.command.executable) {
    throw new Error('inherited child executable must be an absolute canonical path')
  }
  if (!isAbsolute(request.command.cwd) || resolve(request.command.cwd) !== request.command.cwd) {
    throw new Error('inherited child working directory must be an absolute canonical path')
  }
  if (!Array.isArray(request.command.arguments) || request.command.arguments.some((argument) =>
    typeof argument !== 'string' || argument.includes('\0'))) {
    throw new Error('inherited child arguments must be NUL-free strings')
  }
  requireRecord(request.capture, 'inherited child capture policy')
  let events
  if (request.events !== undefined) {
    requireRecord(request.events, 'inherited child event policy')
    requirePositiveInteger(request.events.minimumEvents, 'minimum inherited child event count')
    requirePositiveInteger(request.events.maximumEvents, 'maximum inherited child event count')
    if (request.events.minimumEvents > request.events.maximumEvents) {
      throw new Error('minimum inherited child event count exceeds its maximum')
    }
    events = Object.freeze({ ...request.events })
  }
  const capture = normalizeOwnedProcessCapture({
    stdoutBytes: request.capture.stdoutBytes,
    stderrBytes: request.capture.stderrBytes,
    eventCount: events?.maximumEvents ?? 0,
  })
  return Object.freeze({
    identity,
    command: Object.freeze({
      executable: request.command.executable,
      arguments: Object.freeze([...request.command.arguments]),
      cwd: request.command.cwd,
    }),
    environment: request.environment,
    capture,
    ...(events === undefined ? {} : { events }),
  })
}

function drainDiagnosticStream(stream, channel, label) {
  const consume = (chunk) => {
    channel.append(Buffer.from(chunk))
  }
  stream.on('data', consume)
  return finished(stream, { cleanup: true }).then(
    () => undefined,
    (cause) => {
      const failure = new Error(`${label} failed`, { cause })
      channel.fail(failure)
      throw failure
    },
  ).finally(() => stream.off('data', consume))
}

async function settleInheritedChild(tasks, stdout, stderr, events) {
  const results = await Promise.allSettled(tasks)
  stdout.finish()
  stderr.finish()
  const terminal = results[0].status === 'fulfilled' ? results[0].value : undefined
  const output = Object.freeze({ stdout: stdout.view.snapshot(), stderr: stderr.view.snapshot() })
  const eventEvidence = events.snapshot()
  const failures = uniqueFailures([
    ...results.flatMap((result) => result.status === 'rejected' ? [result.reason] : []),
    stdout.failure(),
    stderr.failure(),
  ])
  if (failures.length !== 0) {
    const cause = failures.length === 1
      ? failures[0]
      : new AggregateError(failures, 'inherited child process channels did not join')
    throw new InheritedChildProcessError(
      'inherited child process transport failed',
      terminal,
      output,
      eventEvidence,
      cause,
    )
  }
  return Object.freeze({ terminal, output, events: eventEvidence })
}

function uniqueFailures(values) {
  const failures = []
  const seen = new Set()
  for (const value of values) {
    if (value === undefined || seen.has(value)) continue
    seen.add(value)
    failures.push(value instanceof Error ? value : new Error(String(value)))
  }
  return failures
}

function childTerminal(child) {
  return new Promise((resolveTerminal) => {
    let settled = false
    const settle = (outcome) => {
      if (settled) return
      settled = true
      resolveTerminal(Object.freeze(outcome))
    }
    child.once('error', (cause) => settle({
      terminal: 'spawn-failed',
      errorCode: typeof cause?.code === 'string' ? cause.code : 'UNKNOWN',
      errorMessage: cause instanceof Error ? cause.message : String(cause),
    }))
    child.once('exit', (code, signal) => {
      if (code !== null) settle({ terminal: 'exited', exitCode: code })
      else settle({ terminal: 'signaled', signal: signal ?? 'UNKNOWN' })
    })
  })
}

function childClosed(child) {
  return new Promise((resolveClose) => child.once('close', resolveClose))
}

function asciiFold(value) {
  return value.replace(/[A-Z]/gu, (character) => character.toLowerCase())
}

function requirePositiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be a positive integer`)
}

function requireRecord(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
}

function observePromise(task) {
  void task.catch(() => undefined)
}
