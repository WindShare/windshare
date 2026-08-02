import { spawn } from 'node:child_process'
import { lstatSync } from 'node:fs'
import { dirname, isAbsolute, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { createServer } from 'vite'

import {
  PION_SERVER_EXECUTABLE_ENV,
  pionServerCommand,
} from './pion-server-command.mjs'
import {
  createInheritedChildProcessBackend,
  InheritedChildProcessError,
} from './process/inherited-child-process.mjs'
import { TEST_EVENT_SCHEMA_VERSION } from './process/test-event-channel.mjs'
import { parseTestIdentity } from './process/test-identity.mjs'

const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const READY_TIMEOUT_MS = 30_000
const CLEANUP_TIMEOUT_MS = 10_000
const RUN_ID_ENV = 'WINDSHARE_TEST_RUN_ID'
const OPERATION_ID_ENV = 'WINDSHARE_TEST_OPERATION_ID'
const SCENARIO_ENV = 'WINDSHARE_TEST_SCENARIO'
const CHILD_CONTEXT_ENV = 'WINDSHARE_BROWSER_EVIDENCE_CONTEXT'
const EVENT_FD_ENV = 'WINDSHARE_TEST_EVENT_FD'
const EVENT_HANDLE_ENV = 'WINDSHARE_TEST_EVENT_HANDLE'
const MAXIMUM_INHERITED_DIAGNOSTIC_BYTES = 4_194_304

export async function runOwnedPlaywright(arguments_, environment = process.env) {
  const invocation = parseInvocation(arguments_)
  requireRegularFile(invocation.playwrightCli, 'owned Playwright CLI')
  const identity = testIdentity(environment)
  const childEnvironment = { ...environment }
  // The outer owner's event endpoint is an immediate-child capability. Nested
  // services publish through their own channels or explicit stdout contracts.
  removeEnvironmentCapability(childEnvironment, EVENT_FD_ENV)
  removeEnvironmentCapability(childEnvironment, EVENT_HANDLE_ENV)
  const childProcessBackend = createInheritedChildProcessBackend()
  let vite
  let pion
  let playwright
  let primaryFailure
  const cleanupFailures = []

  try {
    if (invocation.suite === 'pion') {
      const started = await startPionServer(childEnvironment, identity, childProcessBackend)
      pion = started
      childEnvironment.WINDSHARE_PION_HTTP_ADDRESS = started.address
      // The prebuilt path is a launch capability owned only by this supervisor;
      // Playwright and browser descendants receive the live listener, not authority
      // to start a second fixture process.
      removeEnvironmentCapability(childEnvironment, PION_SERVER_EXECUTABLE_ENV)
    }
    vite = await startViteServer(invocation.suite, childEnvironment)
    childEnvironment.WINDSHARE_WEB_BASE_URL = vite.origin
    emitEvent(identity, 'playwright-services', 'listeners_ready', 'succeeded', {
      web_origin: vite.origin,
      ...(childEnvironment.WINDSHARE_PION_HTTP_ADDRESS === undefined
        ? {}
        : { pion_origin: `http://${childEnvironment.WINDSHARE_PION_HTTP_ADDRESS}` }),
    })
    playwright = spawn(process.execPath, [invocation.playwrightCli, ...invocation.playwrightArguments], {
      cwd: WEB_ROOT,
      env: childEnvironment,
      shell: false,
      stdio: 'inherit',
      windowsHide: true,
    })
    const terminal = await Promise.race([
      childTerminal(playwright, 'Playwright'),
      ...(pion === undefined ? [] : [pion.unexpectedExit]),
    ])
    if (terminal.code !== 0) {
      throw new Error(terminal.signal === null
        ? `Playwright exited with code ${terminal.code}`
        : `Playwright terminated by ${terminal.signal}`)
    }
  } catch (cause) {
    primaryFailure = cause
  } finally {
    if (playwright !== undefined && playwright.exitCode === null && playwright.signalCode === null) {
      await stopChild(playwright, 'Playwright').catch((cause) => cleanupFailures.push(cause))
    }
    if (vite !== undefined) {
      await vite.server.close().catch((cause) => {
        cleanupFailures.push(new Error('Vite listener cleanup failed', { cause }))
      })
    }
    if (pion !== undefined) {
      await pion.stop().catch((cause) => cleanupFailures.push(cause))
    }
  }

  if (primaryFailure !== undefined || cleanupFailures.length > 0) {
    emitEvent(identity, 'playwright-services', 'direct_children_join', 'failed', {
      primary_failure: primaryFailure === undefined ? null : errorMessage(primaryFailure),
      cleanup_failures: cleanupFailures.map(errorMessage),
    })
    throw new AggregateError(
      [...(primaryFailure === undefined ? [] : [primaryFailure]), ...cleanupFailures],
      'Owned Playwright execution failed',
    )
  }
  emitEvent(identity, 'playwright-services', 'direct_children_join', 'succeeded', {})
  return 0
}

async function startViteServer(suite, environment) {
  const configFile = suite === 'main'
    ? join(WEB_ROOT, 'vite.config.ts')
    : join(WEB_ROOT, 'test', 'transport', 'webrtc', 'vite.config.ts')
  const previousEnvironment = { ...process.env }
  Object.assign(process.env, environment)
  let server
  try {
    server = await createServer({
      root: WEB_ROOT,
      configFile,
      clearScreen: false,
      logLevel: 'error',
      server: { host: '127.0.0.1', port: 0, strictPort: true },
    })
    await server.listen()
  } finally {
    // Vite config is evaluated in-process and reads the environment during
    // createServer. Restore deleted keys as well as changed ones afterwards.
    for (const name of Object.keys(process.env)) {
      if (!Object.hasOwn(previousEnvironment, name)) delete process.env[name]
    }
    Object.assign(process.env, previousEnvironment)
  }
  const address = server.httpServer?.address()
  if (address === null || address === undefined || typeof address === 'string') {
    await server.close()
    throw new Error('Vite did not publish its owned TCP listener')
  }
  return Object.freeze({ server, origin: `http://127.0.0.1:${address.port}` })
}

async function startPionServer(environment, identity, backend) {
  const command = pionServerCommand(environment)
  const session = backend.launch({
    identity,
    command: Object.freeze({
      executable: command.executable,
      arguments: command.arguments,
      cwd: resolve(WEB_ROOT, '..'),
    }),
    environment: Object.freeze({
      ...environment,
      WINDSHARE_D1_BROWSER_ADDR: '127.0.0.1:0',
    }),
    capture: Object.freeze({
      stdoutBytes: MAXIMUM_INHERITED_DIAGNOSTIC_BYTES,
      stderrBytes: MAXIMUM_INHERITED_DIAGNOSTIC_BYTES,
    }),
    events: Object.freeze({
      minimumEvents: 1,
      maximumEvents: 1,
    }),
  })
  const joined = session.completion.then(
    (execution) => {
      publishInheritedDiagnostics(execution.output)
      return execution
    },
    (cause) => {
      if (cause instanceof InheritedChildProcessError) publishInheritedDiagnostics(cause.output)
      throw cause
    },
  )
  observePromise(joined)
  const terminalFailure = session.terminal.then((terminal) => {
    throw new Error(`Pion browser server exited unexpectedly (${formatTerminal(terminal)})`)
  })
  const joinedUnexpectedly = joined.then(() => {
    throw new Error('Pion browser server channels settled before cleanup')
  })
  const unexpectedExit = Promise.race([
    terminalFailure,
    joinedUnexpectedly,
  ])
  try {
    const event = await withTimeout(Promise.race([
      firstPionReadyEvent(session.events),
      terminalFailure,
      joinedUnexpectedly,
    ]), READY_TIMEOUT_MS, 'Pion browser server readiness')
    return Object.freeze({
      address: event.payload.address,
      unexpectedExit,
      stop: () => stopInheritedChild(session, 'Pion browser server'),
    })
  } catch (cause) {
    const cleanupFailure = await stopInheritedChild(session, 'Pion browser server').then(
      () => undefined,
      (error) => error,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError([cause, cleanupFailure], 'Pion browser server startup failed')
    }
    throw cause
  }
}

async function firstPionReadyEvent(events) {
  for await (const event of events) return parsePionReadyEvent(event)
  throw new Error('Pion browser server event channel ended before readiness')
}

function publishInheritedDiagnostics(output) {
  const stdout = output.stdout.bytes()
  const stderr = output.stderr.bytes()
  if (stdout.byteLength !== 0) process.stdout.write(stdout)
  if (stderr.byteLength !== 0) process.stderr.write(stderr)
}

function parsePionReadyEvent(event) {
  exactKeys(event.payload, ['address'], 'Pion ready payload')
  if (
    event.component !== 'pion-browser-interop-server' || event.milestone !== 'listener_ready' ||
    event.outcome !== 'succeeded'
  ) throw new Error('Pion ready event has an unexpected semantic identity')
  const address = parseLoopbackAddress(event.payload.address, 'Pion ready address')
  return Object.freeze({ ...event, payload: Object.freeze({ address }) })
}

function parseInvocation(arguments_) {
  if (!Array.isArray(arguments_)) throw new Error('owned Playwright arguments must be an array')
  const separator = arguments_.indexOf('--')
  if (separator < 0) throw new Error('owned Playwright arguments require -- before Playwright argv')
  const options = new Map()
  for (let index = 0; index < separator; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (!['--suite', '--playwright-cli'].includes(name) ||
        typeof value !== 'string' || options.has(name)) {
      throw new Error('owned Playwright options are invalid or repeated')
    }
    options.set(name, value)
  }
  if (options.size !== 2) throw new Error('owned Playwright options are incomplete')
  const suite = options.get('--suite')
  if (!['main', 'pion'].includes(suite)) throw new Error('owned Playwright suite is invalid')
  const playwrightCli = options.get('--playwright-cli')
  if (!isAbsolute(playwrightCli) || resolve(playwrightCli) !== playwrightCli) {
    throw new Error('owned Playwright CLI path must be absolute and canonical')
  }
  const playwrightArguments = Object.freeze(arguments_.slice(separator + 1))
  if (!playwrightArguments.includes('--workers=1') || !playwrightArguments.includes('--retries=0')) {
    throw new Error('owned Playwright execution must use one worker and no retries')
  }
  return Object.freeze({
    suite,
    playwrightCli,
    playwrightArguments,
  })
}

function requireRegularFile(path, label) {
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error(`${label} must be a regular non-symbolic file`)
  }
}

function testIdentity(environment) {
  let childContext = {}
  if (typeof environment[CHILD_CONTEXT_ENV] === 'string') {
    try {
      childContext = JSON.parse(environment[CHILD_CONTEXT_ENV])
    } catch {
      throw new Error('browser evidence context is not JSON')
    }
  }
  try {
    return parseTestIdentity({
      runId: environment[RUN_ID_ENV] ?? childContext.runId,
      operationId: environment[OPERATION_ID_ENV] ?? childContext.operationId,
      scenario: environment[SCENARIO_ENV] ?? childContext.scenario,
    })
  } catch (cause) {
    throw new Error('owned Playwright test identity is invalid', { cause })
  }
}

function childTerminal(child, label) {
  return new Promise((resolveTerminal, rejectTerminal) => {
    child.once('error', (cause) => rejectTerminal(new Error(`${label} failed to spawn`, { cause })))
    child.once('close', (code, signal) => resolveTerminal(Object.freeze({ code, signal })))
  })
}

async function stopChild(child, label) {
  if (child.exitCode !== null || child.signalCode !== null) return
  const terminal = childTerminal(child, label)
  if (child.kill('SIGTERM') !== true) {
    throw new Error(`${label} rejected its direct stop request`)
  }
  await withTimeout(terminal, CLEANUP_TIMEOUT_MS, `${label} cleanup`)
}

async function stopInheritedChild(session, label) {
  let stopFailure
  try {
    session.requestStop()
  } catch (cause) {
    stopFailure = new Error(`${label} rejected its direct stop request`, { cause })
  }
  let joinFailure
  try {
    await withTimeout(session.completion, CLEANUP_TIMEOUT_MS, `${label} cleanup`)
  } catch (cause) {
    joinFailure = cause
  }
  if (stopFailure !== undefined && joinFailure !== undefined) {
    throw new AggregateError([stopFailure, joinFailure], `${label} cleanup failed`)
  }
  if (stopFailure !== undefined) throw stopFailure
  if (joinFailure !== undefined) throw joinFailure
}

async function withTimeout(task, milliseconds, label) {
  let timer
  const timeout = new Promise((_, rejectTimeout) => {
    timer = setTimeout(() => rejectTimeout(new Error(`${label} timed out`)), milliseconds)
    timer.unref?.()
  })
  try {
    return await Promise.race([task, timeout])
  } finally {
    clearTimeout(timer)
  }
}

function parseLoopbackAddress(value, label) {
  if (typeof value !== 'string' || !/^127\.0\.0\.1:[1-9][0-9]{0,4}$/u.test(value)) {
    throw new Error(`${label} is not a canonical IPv4 loopback address`)
  }
  const port = Number(value.slice(value.lastIndexOf(':') + 1))
  if (!Number.isSafeInteger(port) || port > 65_535) throw new Error(`${label} port is invalid`)
  return value
}

function formatTerminal(terminal) {
  if (terminal.terminal === 'exited') return `code=${terminal.exitCode}`
  if (terminal.terminal === 'signaled') return `signal=${terminal.signal}`
  return `spawn=${terminal.errorCode}:${terminal.errorMessage}`
}

function removeEnvironmentCapability(environment, capability) {
  const foldedCapability = capability.toUpperCase()
  for (const name of Object.keys(environment)) {
    if (name.toUpperCase() === foldedCapability) delete environment[name]
  }
}

function exactKeys(value, keys, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function emitEvent(identity, component, milestone, outcome, payload) {
  process.stderr.write(`${JSON.stringify({
    schema_version: TEST_EVENT_SCHEMA_VERSION,
    run_id: identity.runId,
    operation_id: identity.operationId,
    scenario: identity.scenario,
    component,
    milestone,
    outcome,
    payload,
  })}\n`)
}

function errorMessage(cause) {
  return cause instanceof Error ? cause.message : String(cause)
}

function observePromise(task) {
  void task.catch(() => undefined)
}

if (process.argv[1] !== undefined && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  runOwnedPlaywright(process.argv.slice(2)).then(
    (exitCode) => { process.exitCode = exitCode },
    (cause) => {
      process.stderr.write(`${errorMessage(cause)}\n`)
      process.exitCode = 1
    },
  )
}
