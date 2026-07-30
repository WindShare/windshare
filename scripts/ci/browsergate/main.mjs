import { resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { assertPinnedNodeVersion, readPinnedNodeVersion } from '../node-version.mjs'

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL('../../../', import.meta.url)))

const BROWSERGATE_COMMANDS = Object.freeze([
  'local',
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
  return handler(optionArguments)
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
    '  local [--run-policy blocking|closure|stability] [--output-root DIR] [--plan] [--skip-dependency-install]',
    '  build-runtime --output-parent DIR --suite main|pion [--suite main|pion] [--github-output FILE]',
    '  dispose-runtime --runtime-manifest FILE --runtime-manifest-sha256 SHA',
    '  hosted-produce --output-root DIR --context FILE --run-id ID --checkout-sha SHA --run-policy blocking|closure|stability --suite main|pion --runtime-manifest FILE --runtime-manifest-sha256 SHA',
    '  prepare --context FILE --run-id ID --checkout-sha SHA --run-policy blocking|closure|stability --runtime-manifest FILE --runtime-manifest-sha256 SHA',
    '  samples --context FILE --suite main|pion --runtime-manifest FILE --runtime-manifest-sha256 SHA [--inside-windows-d5]',
    '  full --context FILE --suite main|pion --runtime-manifest FILE --runtime-manifest-sha256 SHA [--inside-windows-d5]',
    '  guard-suite --context FILE --suite main|pion --runtime-manifest FILE --runtime-manifest-sha256 SHA [--secret-env NAME] [--github-output FILE]',
    '  context-environment --context FILE',
    '  plan [--platform win32|linux|darwin] [--run-policy blocking|closure|stability]',
  ].join('\n')
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    process.exitCode = await runBrowserGateCli(process.argv.slice(2))
  } catch (cause) {
    process.stderr.write(JSON.stringify({
      component: 'browser-orchestration',
      milestone: 'failed',
      error: cause instanceof Error ? cause.message : String(cause),
    }) + '\n')
    process.exitCode = 1
  }
}
