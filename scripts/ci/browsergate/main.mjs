import { resolve } from 'node:path'
import { types as nodeTypes } from 'node:util'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { assertPinnedNodeVersion, readPinnedNodeVersion } from '../node-version.mjs'
import { createBrowsergateTraceEvent } from './trace-event.mjs'
import { requireCompleteOwnedTraceSnapshot } from './owned-trace-journal.mjs'

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL('../../../', import.meta.url)))

const BROWSERGATE_COMMANDS = Object.freeze([
  'local',
  'preflight',
  'smoke',
  'build-runtime',
  'dispose-runtime',
  'hosted-produce',
  'prepare',
  'samples',
  'full',
  'guard-suite',
  'context-environment',
  'plan',
])

/**
 * Command identity and option token shape settle before dependency ports are
 * observed. This keeps usage and hostile input independent of local runtime state.
 */
export async function runBrowserGateCli(arguments_, ports = {}) {
  const { command, optionArguments } = parseCommandInvocation(arguments_)
  if (ports === null || typeof ports !== 'object' || Array.isArray(ports)) {
    throw new Error('browser orchestration command ports are invalid')
  }

  const configuredRuntimeAssertion = ports.assertRuntimeNodeVersion
  const assertRuntimeNodeVersion = configuredRuntimeAssertion === undefined
    ? assertRepositoryRuntimeNodeVersion
    : configuredRuntimeAssertion
  if (typeof assertRuntimeNodeVersion !== 'function') {
    throw new Error('browser orchestration runtime Node version assertion is invalid')
  }
  await assertRuntimeNodeVersion()

  const configuredCommandLoader = ports.loadCommand
  const loadCommand = configuredCommandLoader === undefined
    ? loadBrowserGateCommand
    : configuredCommandLoader
  if (typeof loadCommand !== 'function') {
    throw new Error('browser orchestration command loader is invalid')
  }
  const handler = await loadCommand(command)
  if (typeof handler !== 'function') {
    throw new Error(`browser orchestration command ${JSON.stringify(command)} has no handler`)
  }
  return requireBrowserGateExecution(await handler(optionArguments))
}

function requireBrowserGateExecution(value) {
  if (
    value === null || typeof value !== 'object' ||
    !Number.isSafeInteger(value.exitCode) || value.exitCode < 0 || value.exitCode > 255
  ) throw new Error('browser orchestration command returned an invalid execution result')
  const traces = requireCompleteOwnedTraceSnapshot(
    value.traces,
    'browser orchestration command trace',
  )
  return Object.freeze({ exitCode: value.exitCode, traces })
}

export function settledBrowserGateFailureTraces(cause) {
  if (cause === null || typeof cause !== 'object' || nodeTypes.isProxy(cause)) return null
  const descriptor = Object.getOwnPropertyDescriptor(cause, 'traces')
  if (descriptor === undefined || !Object.hasOwn(descriptor, 'value')) return null
  try {
    return requireCompleteOwnedTraceSnapshot(
      descriptor.value,
      'failed browser orchestration command trace',
    )
  } catch {
    return null
  }
}

export function browserGateCliFailureEvent() {
  return createBrowsergateTraceEvent({
    operationId: 'browsergate-cli',
    scenario: 'browsergate-cli',
    milestone: 'failed',
    reportedOutcome: 'failed',
    payload: Object.freeze({ failureCode: 'browsergate-command-failed' }),
  })
}

function writeSettledTraces(snapshot) {
  for (const event of snapshot.events) process.stdout.write(`${JSON.stringify(event)}\n`)
}

function assertRepositoryRuntimeNodeVersion() {
  const pinnedVersion = readPinnedNodeVersion(REPOSITORY_ROOT)
  return assertPinnedNodeVersion({ actualVersion: process.version, pinnedVersion })
}

function parseCommandInvocation(arguments_) {
  if (!Array.isArray(arguments_)) throw new Error('browser orchestration arguments must be an array')
  const command = arguments_[0]
  if (command === undefined) throw new Error(usage())
  if (typeof command !== 'string' || !BROWSERGATE_COMMANDS.includes(command)) {
    const label = typeof command === 'string' ? JSON.stringify(command) : 'with a non-text token'
    throw new Error('unknown browser orchestration command ' + label + '\n' + usage())
  }
  const optionArguments = arguments_.slice(1)
  if (optionArguments.some((argument) => typeof argument !== 'string')) {
    throw new Error('browser orchestration options must be text')
  }
  return Object.freeze({ command, optionArguments: Object.freeze(optionArguments) })
}

async function loadBrowserGateCommand(command) {
  const implementation = await import('./orchestrator.mjs')
  if (typeof implementation.runBrowserGateCommand !== 'function') {
    throw new Error('browser orchestration implementation does not export its command dispatcher')
  }
  return (optionArguments) => implementation.runBrowserGateCommand(command, optionArguments)
}

function usage() {
  return [
    'browser orchestration commands:',
    '  local [--run-policy blocking|closure|stability] [--output-root DIR] [--plan]',
    '  preflight',
    '  smoke [--output-root DIR] [--run-id ID] [--checkout-sha SHA] [--profile FILE]',
    '  build-runtime --output-parent DIR --suite main|pion [--suite main|pion] [--github-output FILE]',
    '  dispose-runtime --runtime-manifest FILE',
    '  hosted-produce --output-root DIR --context FILE --run-id ID --checkout-sha SHA --run-policy blocking|closure|stability --suite main|pion --runtime-manifest FILE',
    '  prepare --context FILE --run-id ID --checkout-sha SHA --run-policy blocking|closure|stability --runtime-manifest FILE',
    '  samples --context FILE --suite main|pion --runtime-manifest FILE',
    '  full --context FILE --suite main|pion --runtime-manifest FILE',
    '  guard-suite --context FILE --suite main|pion --runtime-manifest FILE [--secret-env NAME] [--github-output FILE]',
    '  context-environment --context FILE',
    '  plan [--platform win32|linux|darwin] [--run-policy blocking|closure|stability]',
  ].join('\n')
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    const execution = await runBrowserGateCli(process.argv.slice(2))
    writeSettledTraces(execution.traces)
    process.exitCode = execution.exitCode
  } catch (cause) {
    const traces = settledBrowserGateFailureTraces(cause)
    if (traces !== null) writeSettledTraces(traces)
    // Arbitrary dependency causes are opaque here: inspecting message, code,
    // or toString could prevent the one authoritative terminal record.
    process.stderr.write(JSON.stringify(browserGateCliFailureEvent()) + '\n')
    process.exitCode = 1
  }
}
