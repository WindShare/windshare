import { fileURLToPath } from 'node:url'
import { join, resolve } from 'node:path'

import { readStableRegularFileSnapshot } from './filesystem/snapshot.ts'
import {
  browserRunPolicy,
  parseBrowserRunPolicyId,
  type BrowserRunPolicy,
} from './run-policy.ts'
import {
  runBrowserSample,
  type BrowserSampleCommand,
} from './sample-runner.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
  verifyTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from './test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserEngine,
  type BrowserSuite,
} from './vocabulary.ts'

type CliOptions = ReadonlyMap<string, readonly string[]>

export async function browserEvidenceCli(arguments_: readonly string[]): Promise<number> {
  const [command, ...optionArguments] = arguments_
  if (command === undefined) throw new Error(cliUsage())
  const options = parseOptions(optionArguments)
  if (command === 'run-sample') return runSampleCommand(options)
  throw new Error(`unknown browser evidence command ${JSON.stringify(command)}\n${cliUsage()}`)
}

async function runSampleCommand(options: CliOptions): Promise<number> {
  assertOnlyOptions(options, [
    'run-id', 'suite', 'browser', 'sample', 'checkout-sha', 'output-root',
    'run-policy',
    'profile', 'profile-sha256', 'resolution', 'resolution-sha256',
    'executable', 'arg', 'cwd', 'env', 'capture-bytes', 'deadline-ms',
    'windows-job-helper',
  ])
  const topology = await loadTopologyLock(options)
  const suite = browserSuite(requiredOption(options, 'suite'))
  const browser = browserEngine(requiredOption(options, 'browser'))
  const runPolicy = browserRunPolicy(parseBrowserRunPolicyId(requiredOption(options, 'run-policy')))
  const sampleIndex = policySampleIndex(requiredOption(options, 'sample'), runPolicy)
  const outputRoot = resolve(requiredOption(options, 'output-root'))
  const command = childCommand(options)
  const windowsJobHelper = optionalOption(options, 'windows-job-helper')
  const outcome = await runBrowserSample({
    runId: requiredOption(options, 'run-id'),
    runPolicy,
    suite,
    browser,
    sampleIndex,
    checkoutSha: requiredOption(options, 'checkout-sha'),
    sampleDirectory: join(outputRoot, suite, browser, `sample-${sampleIndex}`),
    topologyLock: topology.lock,
    topologyProfilePath: topology.profilePath,
    topologyResolutionPath: topology.resolutionPath,
    command,
    ...(windowsJobHelper === undefined ? {} : { windowsJobHelperPath: resolve(windowsJobHelper) }),
    ...(optionalOption(options, 'capture-bytes') === undefined
      ? {}
      : { maximumCapturedStreamBytes: positiveInteger(requiredOption(options, 'capture-bytes'), 'capture-bytes') }),
    ...(optionalOption(options, 'deadline-ms') === undefined
      ? {}
      : { processDeadlineMs: positiveInteger(requiredOption(options, 'deadline-ms'), 'deadline-ms') }),
  })
  process.stdout.write(`${JSON.stringify({
    command: 'run-sample',
    resultPath: outcome.resultPath,
    artifactRoot: outcome.artifactRoot,
    resultStatus: outcome.result.resultStatus,
    acceptedBeforeGuard: outcome.acceptedBeforeGuard,
  })}\n`)
  return outcome.acceptedBeforeGuard ? 0 : 1
}

function decodeUtf8(encoded: Uint8Array, label: string): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(encoded)
  } catch {
    throw new Error(`${label} is not valid UTF-8`)
  }
}

async function loadTopologyLock(options: CliOptions): Promise<{
  readonly lock: VerifiedTestIceTopologyLock
  readonly profilePath: string
  readonly resolutionPath: string
}> {
  const profile = await loadTopologyProfile(options)
  return loadTopologyResolution(options, profile, 'resolution', 'resolution-sha256')
}

interface LoadedTopologyProfile {
  readonly profilePath: string
  readonly profile: ReturnType<typeof parseTestIceTopologyJson>
  readonly profileSha256: string
}

async function loadTopologyProfile(options: CliOptions): Promise<LoadedTopologyProfile> {
  const profilePath = resolve(requiredOption(options, 'profile'))
  const expectedProfileSha256 = requiredOption(options, 'profile-sha256')
  const profileSnapshot = await readStableRegularFileSnapshot(
    profilePath,
    TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
    'topology profile',
  )
  const profile = parseTestIceTopologyJson(decodeUtf8(profileSnapshot.bytes, 'topology profile'))
  const actualProfileSha256 = profileSnapshot.sha256
  if (actualProfileSha256 !== expectedProfileSha256) {
    throw new Error('topology profile does not match --profile-sha256')
  }
  return Object.freeze({ profilePath, profile, profileSha256: actualProfileSha256 })
}

async function loadTopologyResolution(
  options: CliOptions,
  profileAuthority: LoadedTopologyProfile,
  resolutionOption: string,
  resolutionSha256Option: string,
): Promise<{
  readonly lock: VerifiedTestIceTopologyLock
  readonly profilePath: string
  readonly resolutionPath: string
}> {
  const resolutionPath = resolve(requiredOption(options, resolutionOption))
  const expectedResolutionSha256 = requiredOption(options, resolutionSha256Option)
  const resolutionSnapshot = await readStableRegularFileSnapshot(
    resolutionPath,
    TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
    'topology resolution',
  )
  const resolution = parseTestIceTopologyResolutionJson(
    decodeUtf8(resolutionSnapshot.bytes, 'topology resolution'),
    profileAuthority.profile,
    profileAuthority.profileSha256,
  )
  const actualResolutionSha256 = resolutionSnapshot.sha256
  if (actualResolutionSha256 !== expectedResolutionSha256) {
    throw new Error(`topology resolution does not match --${resolutionSha256Option}`)
  }
  return Object.freeze({
    lock: await verifyTestIceTopologyLock(
      profileAuthority.profile,
      resolution,
      profileAuthority.profileSha256,
      expectedResolutionSha256,
    ),
    profilePath: profileAuthority.profilePath,
    resolutionPath,
  })
}

function childCommand(options: CliOptions): BrowserSampleCommand {
  const environment = Object.fromEntries(optionValues(options, 'env').map((assignment) => {
    const separator = assignment.indexOf('=')
    if (separator < 1) throw new Error('--env values must use NAME=VALUE')
    const name = assignment.slice(0, separator)
    if (!/^[A-Za-z_]\w*$/u.test(name)) throw new Error(`invalid child environment name ${name}`)
    return [name, assignment.slice(separator + 1)]
  }))
  const cwd = optionalOption(options, 'cwd')
  return Object.freeze({
    executable: requiredOption(options, 'executable'),
    arguments: Object.freeze([...optionValues(options, 'arg')]),
    ...(cwd === undefined ? {} : { cwd: resolve(cwd) }),
    ...(Object.keys(environment).length === 0 ? {} : { environment: Object.freeze(environment) }),
  })
}

function parseOptions(arguments_: readonly string[]): CliOptions {
  const options = new Map<string, string[]>()
  for (let index = 0; index < arguments_.length; index += 1) {
    const token = arguments_[index]
    if (token === undefined || !token.startsWith('--') || token.length === 2) {
      throw new Error(`browser evidence CLI expected an option, received ${JSON.stringify(token)}`)
    }
    const equals = token.indexOf('=')
    const name = token.slice(2, equals < 0 ? undefined : equals)
    const inlineValue = equals < 0 ? undefined : token.slice(equals + 1)
    const value = inlineValue ?? arguments_[index + 1]
    if (value === undefined) throw new Error(`browser evidence option --${name} requires a value`)
    if (inlineValue === undefined) index += 1
    const values = options.get(name) ?? []
    values.push(value)
    options.set(name, values)
  }
  return options
}

function assertOnlyOptions(options: CliOptions, allowed: readonly string[]): void {
  const allowedSet = new Set(allowed)
  for (const name of options.keys()) {
    if (!allowedSet.has(name)) throw new Error(`unknown option --${name}`)
  }
}

function requiredOption(options: CliOptions, name: string): string {
  const values = optionValues(options, name)
  if (values.length !== 1 || values[0] === undefined || values[0].length === 0) {
    throw new Error(`browser evidence option --${name} must appear exactly once with a non-empty value`)
  }
  return values[0]
}

function optionalOption(options: CliOptions, name: string): string | undefined {
  const values = optionValues(options, name)
  if (values.length > 1) throw new Error(`browser evidence option --${name} may appear at most once`)
  return values[0]
}

function optionValues(options: CliOptions, name: string): readonly string[] {
  return options.get(name) ?? []
}

function browserSuite(value: string): BrowserSuite {
  if (!BROWSER_SUITES.includes(value as BrowserSuite)) throw new Error(`unknown browser suite ${value}`)
  return value as BrowserSuite
}

function browserEngine(value: string): BrowserEngine {
  if (!BROWSER_ENGINES.includes(value as BrowserEngine)) throw new Error(`unknown browser engine ${value}`)
  return value as BrowserEngine
}

function positiveInteger(value: string, label: string): number {
  if (!/^[1-9]\d*$/u.test(value)) throw new Error(`${label} must be a positive integer`)
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed)) throw new Error(`${label} exceeds the safe integer range`)
  return parsed
}

function policySampleIndex(value: string, runPolicy: BrowserRunPolicy): number {
  const parsed = positiveInteger(value, 'sample')
  if (parsed > runPolicy.sampleCount) {
    throw new Error(`sample must be in [1, ${runPolicy.sampleCount}] for ${runPolicy.policyId}`)
  }
  return parsed
}

function cliUsage(): string {
  return [
    'browser evidence commands:',
    '  run-sample --run-id ID --run-policy blocking|closure|stability --suite main|pion --browser ENGINE --sample N --checkout-sha SHA --output-root DIR --profile FILE --profile-sha256 SHA --resolution FILE --resolution-sha256 SHA --executable FILE [--arg VALUE ...] [--windows-job-helper FILE]',
  ].join('\n')
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && resolve(invokedPath) === fileURLToPath(import.meta.url)) {
  browserEvidenceCli(process.argv.slice(2)).then(
    (exitCode) => { process.exitCode = exitCode },
    (cause: unknown) => {
      process.stderr.write(`${JSON.stringify({
        component: 'browser-evidence-cli',
        outcome: 'failed',
        error: cause instanceof Error ? cause.message : String(cause),
      })}\n`)
      process.exitCode = 1
    },
  )
}
