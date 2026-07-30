import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  closeSync,
  existsSync,
  fsyncSync,
  mkdtempSync,
  openSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, isAbsolute, join, relative, sep } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { types as nodeTypes } from 'node:util'

import { inheritedSampleEnvironment } from './scripts/browser-evidence/process/sample-environment.ts'
import {
  playwrightDiscoveryProjectPattern,
  PLAYWRIGHT_SUITE_PARTITIONS,
  PLAYWRIGHT_SUITES,
  type PlaywrightSuite,
  type PlaywrightSuitePartition,
} from './playwright.suite-config.ts'

export interface PlaywrightDiscoveryCommand {
  readonly suite: PlaywrightSuite
  readonly partition: PlaywrightSuitePartition
  readonly executable: string
  readonly arguments: readonly string[]
  readonly cwd: string
}

export interface PlaywrightDiscoverySpawnOptions {
  readonly cwd: string
  readonly encoding: 'utf8'
  readonly env: Readonly<Record<string, string>>
  readonly timeout: number
  readonly windowsHide: true
}

export interface PlaywrightDiscoveryExecution {
  readonly status: number | null
  readonly stdout: string
  readonly stderr: string
  readonly error?: Error
}

export type PlaywrightDiscoverySpawn = (
  executable: string,
  arguments_: readonly string[],
  options: PlaywrightDiscoverySpawnOptions,
) => PlaywrightDiscoveryExecution

const WEB_ROOT = dirname(fileURLToPath(import.meta.url))
const REPOSITORY_ROOT = dirname(WEB_ROOT)
const PLAYWRIGHT_CLI = join(
  WEB_ROOT,
  'node_modules',
  '@playwright',
  'test',
  'cli.js',
)
const DISCOVERY_FACTORY = fileURLToPath(new URL('./playwright.discovery.config.ts', import.meta.url))
const DISCOVERY_TIMEOUT_MS = 60_000
const TEMPORARY_DIRECTORY_PREFIX = 'windshare-playwright-discovery-'
const WRAPPER_CONFIG_NAME = 'playwright.config.mts'
const MAXIMUM_CALLER_ARGUMENTS = 16
const COMMAND_FIELD_NAMES = Object.freeze([
  'suite',
  'partition',
  'executable',
  'arguments',
  'cwd',
] as const)
const WRAPPER_CONFIG_SOURCE = [
  'import { createPlaywrightDiscoveryConfig } from ' +
    JSON.stringify(pathToFileURL(DISCOVERY_FACTORY).href),
  'export default createPlaywrightDiscoveryConfig()',
  '',
].join('\n')

export const PLAYWRIGHT_DISCOVERY_COMMANDS: readonly PlaywrightDiscoveryCommand[] =
  Object.freeze(PLAYWRIGHT_SUITES.flatMap((suite) =>
    PLAYWRIGHT_SUITE_PARTITIONS.map((partition) =>
      createPlaywrightDiscoveryCommand(suite, partition))))

/**
 * The command is copied from own data descriptors before validation. This keeps
 * accessors and caller mutation from changing which process receives authority.
 */
export function launchPlaywrightDiscovery(
  callerCommand: unknown,
  hostEnvironment: Readonly<Record<string, string | undefined>>,
  spawn: PlaywrightDiscoverySpawn = spawnPlaywrightDiscovery,
): PlaywrightDiscoveryExecution {
  const command = snapshotDiscoveryCommand(callerCommand)
  validateRawDiscoveryCommand(command)
  const environment = inheritedSampleEnvironment(hostEnvironment)
  return launchWithOneShotConfig(command, environment, spawn)
}

export function playwrightDiscoveryEnvironment(
  hostEnvironment: Readonly<Record<string, string | undefined>>,
): Readonly<Record<string, string>> {
  return inheritedSampleEnvironment(hostEnvironment)
}

function createPlaywrightDiscoveryCommand(
  suite: PlaywrightSuite,
  partition: PlaywrightSuitePartition,
): PlaywrightDiscoveryCommand {
  return Object.freeze({
    suite,
    partition,
    executable: process.execPath,
    arguments: Object.freeze([
      PLAYWRIGHT_CLI,
      'test',
      '--project=' + playwrightDiscoveryProjectPattern(suite, partition),
      '--workers=1',
      '--retries=0',
      '--list',
    ]),
    cwd: WEB_ROOT,
  })
}

function snapshotDiscoveryCommand(candidate: unknown): PlaywrightDiscoveryCommand {
  const fields = snapshotPlainRecord(candidate, COMMAND_FIELD_NAMES, 'Playwright discovery command')
  return Object.freeze({
    suite: requireString(fields.suite, 'Playwright discovery suite') as PlaywrightSuite,
    partition: requireString(
      fields.partition,
      'Playwright discovery partition',
    ) as PlaywrightSuitePartition,
    executable: requireString(fields.executable, 'Node executable'),
    arguments: snapshotStringArray(fields.arguments, 'Playwright discovery argv'),
    cwd: requireString(fields.cwd, 'Playwright discovery working directory'),
  })
}

function snapshotPlainRecord(
  candidate: unknown,
  expectedFields: readonly string[],
  label: string,
): Readonly<Record<string, unknown>> {
  if (candidate === null || typeof candidate !== 'object') {
    throw new Error(label + ' must be a plain object')
  }
  rejectProxy(candidate, label)
  const prototype = safelyReadPrototype(candidate, label)
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error(label + ' must be a plain object')
  }
  const keys = safelyReadOwnKeys(candidate, label)
  if (keys.some((key) => typeof key === 'symbol')) {
    throw new Error(label + ' must not have symbol fields')
  }
  const names = keys as string[]
  if (
    names.length !== expectedFields.length ||
    expectedFields.some((field) => !names.includes(field))
  ) throw new Error(label + ' has an invalid field set')

  const snapshot: Record<string, unknown> = {}
  for (const field of expectedFields) {
    snapshot[field] = ownDataValue(candidate, field, label + '.' + field)
  }
  return Object.freeze(snapshot)
}

function snapshotStringArray(candidate: unknown, label: string): readonly string[] {
  if (candidate === null || typeof candidate !== 'object') {
    throw new Error(label + ' must be a plain array')
  }
  rejectProxy(candidate, label)
  if (!Array.isArray(candidate)) throw new Error(label + ' must be a plain array')
  if (safelyReadPrototype(candidate, label) !== Array.prototype) {
    throw new Error(label + ' must be a plain array')
  }
  const length = ownDataValue(candidate, 'length', label + '.length')
  if (
    typeof length !== 'number' ||
    !Number.isSafeInteger(length) ||
    length < 0 ||
    length > MAXIMUM_CALLER_ARGUMENTS
  ) throw new Error(label + ' length is invalid')

  const keys = safelyReadOwnKeys(candidate, label)
  const expectedKeys = Array.from({ length }, (_, index) => String(index))
  if (
    keys.some((key) => typeof key === 'symbol') ||
    keys.length !== expectedKeys.length + 1 ||
    !keys.includes('length') ||
    expectedKeys.some((key) => !keys.includes(key))
  ) throw new Error(label + ' must be a dense plain array')

  const snapshot = expectedKeys.map((key) =>
    requireString(ownDataValue(candidate, key, label + '[' + key + ']'), label + '[' + key + ']'))
  return Object.freeze(snapshot)
}

function rejectProxy(candidate: object, label: string): void {
  try {
    if (nodeTypes.isProxy(candidate)) throw new Error(label + ' must not be a Proxy')
  } catch (cause) {
    if (cause instanceof Error && cause.message.includes('must not be a Proxy')) throw cause
    throw new Error(label + ' Proxy inspection failed', { cause })
  }
}

function safelyReadPrototype(candidate: object, label: string): object | null {
  try {
    return Object.getPrototypeOf(candidate)
  } catch (cause) {
    throw new Error(label + ' prototype inspection failed', { cause })
  }
}

function safelyReadOwnKeys(candidate: object, label: string): readonly PropertyKey[] {
  try {
    return Reflect.ownKeys(candidate)
  } catch (cause) {
    throw new Error(label + ' field inspection failed', { cause })
  }
}

function ownDataValue(candidate: object, field: PropertyKey, label: string): unknown {
  let descriptor: PropertyDescriptor | undefined
  try {
    descriptor = Object.getOwnPropertyDescriptor(candidate, field)
  } catch (cause) {
    throw new Error(label + ' descriptor inspection failed', { cause })
  }
  if (descriptor === undefined || !Object.hasOwn(descriptor, 'value')) {
    throw new Error(label + ' must be an own data property; accessors are forbidden')
  }
  return descriptor.value
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== 'string') throw new Error(label + ' must be text')
  return value
}

function validateRawDiscoveryCommand(command: PlaywrightDiscoveryCommand): void {
  if (
    !PLAYWRIGHT_SUITES.includes(command.suite) ||
    !PLAYWRIGHT_SUITE_PARTITIONS.includes(command.partition)
  ) throw new Error('Playwright discovery command has an unknown partition')
  const expected = createPlaywrightDiscoveryCommand(command.suite, command.partition)
  assertExactRawPath(command.executable, expected.executable, 'Node executable')
  assertExactRawPath(command.cwd, expected.cwd, 'working directory')
  for (const [index, argument] of command.arguments.entries()) {
    if (hasNavigationSegment(argument)) {
      throw new Error('Playwright discovery argv[' + index + '] contains raw navigation')
    }
  }
  assertExactRawPath(command.arguments[0], PLAYWRIGHT_CLI, 'Playwright CLI')
  assertRepositoryLocalPlaywrightCli(command.arguments[0])
  if (
    command.arguments.length !== expected.arguments.length ||
    command.arguments.some((argument, index) => argument !== expected.arguments[index])
  ) throw new Error('Playwright discovery argv is not one of the six list-only commands')
}

function launchWithOneShotConfig(
  command: PlaywrightDiscoveryCommand,
  environment: Readonly<Record<string, string>>,
  spawn: PlaywrightDiscoverySpawn,
): PlaywrightDiscoveryExecution {
  const temporaryRoot = createPrivateTemporaryRoot()
  const wrapperConfig = join(temporaryRoot, WRAPPER_CONFIG_NAME)
  try {
    writeWrapperConfigExclusive(wrapperConfig)
    const spawnCommand = oneShotSpawnCommand(command, wrapperConfig)
    const spawnOptions = Object.freeze({
      cwd: spawnCommand.cwd,
      encoding: 'utf8',
      env: environment,
      timeout: DISCOVERY_TIMEOUT_MS,
      windowsHide: true,
    })
    return spawn(spawnCommand.executable, spawnCommand.arguments, spawnOptions)
  } finally {
    retireTemporaryRoot(temporaryRoot, wrapperConfig)
  }
}

function createPrivateTemporaryRoot(): string {
  const canonicalTemporaryParent = realpathSync.native(tmpdir())
  if (isPathWithin(REPOSITORY_ROOT, canonicalTemporaryParent)) {
    throw new Error('Playwright discovery temporary storage must be outside the repository')
  }
  const temporaryRoot = mkdtempSync(join(canonicalTemporaryParent, TEMPORARY_DIRECTORY_PREFIX))
  if (process.platform !== 'win32') chmodSync(temporaryRoot, 0o700)
  return temporaryRoot
}

function writeWrapperConfigExclusive(wrapperConfig: string): void {
  let descriptor: number | undefined
  try {
    descriptor = openSync(wrapperConfig, 'wx', 0o600)
    writeFileSync(descriptor, WRAPPER_CONFIG_SOURCE, { encoding: 'utf8' })
    fsyncSync(descriptor)
  } finally {
    if (descriptor !== undefined) closeSync(descriptor)
  }
}

function oneShotSpawnCommand(
  command: PlaywrightDiscoveryCommand,
  wrapperConfig: string,
): PlaywrightDiscoveryCommand {
  const playwrightCli = requireString(command.arguments[0], 'validated Playwright CLI')
  const testCommand = requireString(command.arguments[1], 'validated Playwright command')
  const arguments_ = Object.freeze([
    playwrightCli,
    testCommand,
    '--config',
    wrapperConfig,
    ...command.arguments.slice(2),
  ])
  return Object.freeze({
    suite: command.suite,
    partition: command.partition,
    executable: command.executable,
    arguments: arguments_,
    cwd: command.cwd,
  })
}

function retireTemporaryRoot(temporaryRoot: string, wrapperConfig: string): void {
  try {
    rmSync(temporaryRoot, {
      recursive: true,
      force: true,
      maxRetries: 2,
      retryDelay: 10,
    })
  } catch (cause) {
    throw new Error('Playwright discovery wrapper cleanup failed', { cause })
  }
  if (existsSync(wrapperConfig) || existsSync(temporaryRoot)) {
    throw new Error('Playwright discovery wrapper cleanup left filesystem residue')
  }
}

function assertExactRawPath(
  candidate: unknown,
  expected: string,
  label: string,
): asserts candidate is string {
  if (typeof candidate !== 'string' || !isAbsolute(candidate)) {
    throw new Error(label + ' must be an absolute path')
  }
  if (hasNavigationSegment(candidate)) {
    throw new Error(label + ' contains raw navigation')
  }
  if (candidate !== expected) {
    throw new Error(label + ' must use its exact issuer spelling')
  }
  try {
    if (realpathSync.native(candidate) !== realpathSync.native(expected)) {
      throw new Error(label + ' resolves outside its issuer authority')
    }
  } catch (cause) {
    if (cause instanceof Error && cause.message.includes('issuer authority')) throw cause
    throw new Error(label + ' cannot be resolved', { cause })
  }
}

function assertRepositoryLocalPlaywrightCli(candidate: string): void {
  if (!isPathWithin(WEB_ROOT, candidate) || candidate === WEB_ROOT) {
    throw new Error('Playwright CLI must use the repository-local installation')
  }
}

function isPathWithin(root: string, candidate: string): boolean {
  const repositoryRelative = relative(root, candidate)
  return !(
    repositoryRelative === '..' ||
    repositoryRelative.startsWith('..' + sep) ||
    isAbsolute(repositoryRelative)
  )
}

function hasNavigationSegment(value: string): boolean {
  return value.replaceAll('\\', '/').split('/')
    .some((segment) => segment === '.' || segment === '..')
}

function spawnPlaywrightDiscovery(
  executable: string,
  arguments_: readonly string[],
  options: PlaywrightDiscoverySpawnOptions,
): PlaywrightDiscoveryExecution {
  return spawnSync(executable, [...arguments_], options)
}
