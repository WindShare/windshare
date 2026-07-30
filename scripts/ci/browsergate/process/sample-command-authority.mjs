import { createHash } from 'node:crypto'
import { isAbsolute, join, resolve } from 'node:path'

import { BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION } from '../../../../web/scripts/browser-evidence/sample-driver.ts'
import { D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES } from '../../../../web/scripts/browser-evidence/process/d5-ownership.ts'
import { parseBrowserRunPolicy } from '../../../../web/scripts/browser-evidence/run-policy.ts'

export { D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES }

export const PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION =
  'windshare.sample-command-authority/v4'

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]{1,256}$/u
const MAXIMUM_ARGUMENTS = 4_096
const MAXIMUM_ARGUMENT_BYTES = 65_536
const SAMPLE_DRIVER_RELATIVE_PATH = join(
  'web',
  'scripts',
  'browser-evidence',
  'sample-driver.ts',
)

/**
 * The launched command and the guard digest share this single semantic parser.
 * D5 capability values are accepted only at launch and are replaced by stable
 * name-bound sentinels in the signed command authority.
 */
export function sampleDriverCommand(authority, { ownershipEnvironment = {} } = {}) {
  const parsed = parseAuthority(authority)
  const actualOwnership = actualOwnershipEnvironment(
    parsed.ownership.insideWindowsD5,
    ownershipEnvironment,
  )
  const requestBytes = driverRequestBytes(parsed)
  return Object.freeze({
    executable: parsed.driver.node.path,
    executableByteLength: parsed.driver.node.byteLength,
    executableSha256: parsed.driver.node.sha256,
    arguments: Object.freeze([parsed.driver.source.path]),
    cwd: parsed.driver.cwd,
    environment: Object.freeze({
      ...parsed.driver.environment,
      ...actualOwnership,
    }),
    stdin: requestBytes,
    stdinAuthority: Object.freeze({
      channelId: 'browser-sample-driver-request',
      runId: parsed.identity.runId,
      profileId: parsed.topology.topologyId,
      attemptId: `${parsed.identity.suite}-${parsed.identity.browser}-${parsed.identity.sampleIndex}`,
    }),
  })
}

export function canonicalSampleCommandSha256(authority) {
  const command = canonicalSampleCommandRecord(authority)
  return createHash('sha256').update(Buffer.from(JSON.stringify(command), 'utf8')).digest('hex')
}

export function canonicalSampleCommandComponentSha256(authority) {
  const command = canonicalSampleCommandRecord(authority)
  return Object.freeze(Object.fromEntries([
    'repository', 'driver', 'identity', 'topology', 'runtime',
    'output', 'ownership', 'leaf', 'launch',
  ].map((name) => [
    name,
    createHash('sha256').update(Buffer.from(JSON.stringify(command[name]), 'utf8')).digest('hex'),
  ])))
}

function canonicalSampleCommandRecord(authority) {
  const parsed = parseAuthority(authority)
  const ownershipEnvironment = parsed.ownership.insideWindowsD5
    ? Object.fromEntries(D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES.map((name) => [
        name,
        'ownership-bound',
      ]))
    : {}
  const launch = sampleDriverCommand(parsed, { ownershipEnvironment })
  const command = Object.freeze({
    schemaVersion: PROCESS_SETTLEMENT_COMMAND_SCHEMA_VERSION,
    repository: parsed.repository,
    driver: parsed.driver,
    identity: parsed.identity,
    topology: parsed.topology,
    runtime: parsed.runtime,
    output: parsed.output,
    ownership: Object.freeze({
      ...parsed.ownership,
      ownershipEnvironmentNames: parsed.ownership.insideWindowsD5
        ? D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES
        : [],
    }),
    leaf: parsed.leaf,
    launch: Object.freeze({
      executable: launch.executable,
      executableByteLength: launch.executableByteLength,
      executableSha256: launch.executableSha256,
      arguments: launch.arguments,
      cwd: launch.cwd,
      environment: launch.environment,
      stdin: JSON.parse(Buffer.from(launch.stdin).toString('utf8')),
      stdinAuthority: launch.stdinAuthority,
    }),
  })
  launch.stdin.fill(0)
  return command
}

function parseAuthority(value) {
  exactKeys(value, [
    'repository', 'driver', 'identity', 'topology', 'runtime', 'output', 'ownership', 'leaf',
  ], 'sample command authority')
  const repository = parseRepository(value.repository)
  const driver = parseDriver(value.driver, repository)
  const identity = parseIdentity(value.identity, repository)
  const topology = parseTopology(value.topology)
  const runtime = parseRuntime(value.runtime)
  const output = parseOutput(value.output, identity)
  const ownership = parseOwnership(value.ownership, identity, runtime)
  const leaf = parseLeaf(value.leaf, driver)
  return Object.freeze({
    repository,
    driver,
    identity,
    topology,
    runtime,
    output,
    ownership,
    leaf,
  })
}

function parseRepository(value) {
  exactKeys(value, ['root', 'checkoutSha'], 'sample repository authority')
  return Object.freeze({
    root: canonicalAbsolutePath(value.root, 'sample repository root'),
    checkoutSha: checkoutSha(value.checkoutSha, 'sample repository checkout SHA'),
  })
}

function parseDriver(value, repository) {
  exactKeys(value, ['node', 'source', 'cwd', 'environment'], 'sample driver authority')
  const node = parseArtifact(value.node, 'sample driver Node executable')
  const source = parseArtifact(value.source, 'sample driver source')
  if (source.path !== join(repository.root, SAMPLE_DRIVER_RELATIVE_PATH)) {
    throw new Error('sample driver source differs from the repository authority')
  }
  const cwd = canonicalAbsolutePath(value.cwd, 'sample driver working directory')
  if (cwd !== repository.root) throw new Error('sample driver working directory differs from repository root')
  return Object.freeze({
    node,
    source,
    cwd,
    environment: canonicalEnvironment(value.environment),
  })
}

function parseIdentity(value, repository) {
  exactKeys(value, [
    'runId', 'runPolicy', 'suite', 'browser', 'sampleIndex', 'checkoutSha',
  ], 'sample command identity')
  const runId = portableToken(value.runId, 'sample command run ID')
  const runPolicy = parseBrowserRunPolicy(value.runPolicy, 'sample command run policy')
  if (!['main', 'pion'].includes(value.suite)) throw new Error('sample command suite is invalid')
  if (!['chromium', 'firefox', 'webkit'].includes(value.browser)) {
    throw new Error('sample command browser is invalid')
  }
  if (
    !Number.isSafeInteger(value.sampleIndex) || value.sampleIndex < 1 ||
    value.sampleIndex > runPolicy.sampleCount
  ) throw new Error('sample command index exceeds its run policy')
  const identityCheckout = checkoutSha(value.checkoutSha, 'sample identity checkout SHA')
  if (identityCheckout !== repository.checkoutSha) {
    throw new Error('sample identity checkout differs from repository authority')
  }
  return Object.freeze({
    runId,
    runPolicy,
    suite: value.suite,
    browser: value.browser,
    sampleIndex: value.sampleIndex,
    checkoutSha: identityCheckout,
  })
}

function parseTopology(value) {
  exactKeys(value, [
    'topologyId', 'profilePath', 'profileSha256', 'resolutionPath', 'resolutionSha256',
  ], 'sample topology authority')
  return Object.freeze({
    topologyId: portableToken(value.topologyId, 'sample topology ID'),
    profilePath: canonicalAbsolutePath(value.profilePath, 'sample topology profile'),
    profileSha256: sha256(value.profileSha256, 'sample topology profile digest'),
    resolutionPath: canonicalAbsolutePath(value.resolutionPath, 'sample topology resolution'),
    resolutionSha256: sha256(value.resolutionSha256, 'sample topology resolution digest'),
  })
}

function parseRuntime(value) {
  exactKeys(value, ['manifest', 'processOwner'], 'sample runtime authority')
  const manifest = parseArtifact(value.manifest, 'sample runtime manifest')
  const processOwner = parseArtifact(value.processOwner, 'sample process owner')
  exactKeys(value.processOwner, ['kind', 'path', 'byteLength', 'sha256'], 'sample process owner')
  if (!['linux-process-owner', 'windows-job'].includes(value.processOwner.kind)) {
    throw new Error('sample process owner kind is invalid')
  }
  return Object.freeze({
    manifest,
    processOwner: Object.freeze({ kind: value.processOwner.kind, ...processOwner }),
  })
}

function parseOutput(value, identity) {
  exactKeys(value, ['root', 'sampleDirectory', 'resultPath'], 'sample output authority')
  const root = canonicalAbsolutePath(value.root, 'sample output root')
  const sampleDirectory = canonicalAbsolutePath(value.sampleDirectory, 'sample output directory')
  const expected = join(root, identity.suite, identity.browser, `sample-${identity.sampleIndex}`)
  if (sampleDirectory !== expected || value.resultPath !== join(expected, 'result.json')) {
    throw new Error('sample output authority differs from its identity slot')
  }
  return Object.freeze({ root, sampleDirectory, resultPath: value.resultPath })
}

function parseOwnership(value, identity, runtime) {
  exactKeys(value, [
    'platform', 'insideWindowsD5', 'backend', 'operationClass',
    'classDeadlineMs', 'childDeadlineMs',
  ], 'sample process ownership authority')
  if (!['linux', 'win32'].includes(value.platform)) {
    throw new Error('sample ownership platform is unsupported')
  }
  if (typeof value.insideWindowsD5 !== 'boolean') {
    throw new Error('sample D5 ownership evidence is invalid')
  }
  const expectedBackend = value.platform === 'linux' ? 'linux-subreaper' : 'windows-job'
  const expectedOwner = value.platform === 'linux' ? 'linux-process-owner' : 'windows-job'
  if (
    value.backend !== expectedBackend || runtime.processOwner.kind !== expectedOwner ||
    value.operationClass !== 'browser-sample'
  ) throw new Error('sample ownership backend or operation class is invalid')
  if (value.insideWindowsD5 && (value.platform !== 'win32' || identity.suite !== 'main')) {
    throw new Error('sample D5 ownership is valid only for Windows main')
  }
  if (
    !Number.isSafeInteger(value.classDeadlineMs) ||
    !Number.isSafeInteger(value.childDeadlineMs) ||
    value.childDeadlineMs < 1 || value.classDeadlineMs <= value.childDeadlineMs
  ) throw new Error('sample owner deadline must strictly outlive its leaf deadline')
  return Object.freeze({
    platform: value.platform,
    insideWindowsD5: value.insideWindowsD5,
    backend: value.backend,
    operationClass: value.operationClass,
    classDeadlineMs: value.classDeadlineMs,
    childDeadlineMs: value.childDeadlineMs,
  })
}

function parseLeaf(value, driver) {
  exactKeys(value, ['executable', 'entrypoint', 'arguments', 'cwd', 'environment'], 'sample leaf')
  const executable = parseArtifact(value.executable, 'sample leaf executable')
  if (executable.path !== driver.node.path || executable.sha256 !== driver.node.sha256 ||
      executable.byteLength !== driver.node.byteLength) {
    throw new Error('sample leaf executable differs from the authenticated Node runtime')
  }
  const entrypoint = parseArtifact(value.entrypoint, 'sample leaf entrypoint')
  const arguments_ = canonicalArguments(value.arguments)
  if (arguments_[0] !== entrypoint.path) {
    throw new Error('sample leaf argv does not start with its authenticated entrypoint')
  }
  return Object.freeze({
    executable,
    entrypoint,
    arguments: arguments_,
    cwd: canonicalAbsolutePath(value.cwd, 'sample leaf working directory'),
    environment: canonicalEnvironment(value.environment),
  })
}

function driverRequestBytes(authority) {
  const request = Object.freeze({
    schemaVersion: BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION,
    identity: authority.identity,
    output: authority.output,
    topology: Object.freeze({
      profilePath: authority.topology.profilePath,
      profileSha256: authority.topology.profileSha256,
      resolutionPath: authority.topology.resolutionPath,
      resolutionSha256: authority.topology.resolutionSha256,
    }),
    ownership: Object.freeze({
      insideWindowsD5: authority.ownership.insideWindowsD5,
      childDeadlineMs: authority.ownership.childDeadlineMs,
    }),
    leaf: Object.freeze({
      executable: authority.leaf.executable.path,
      arguments: authority.leaf.arguments,
      cwd: authority.leaf.cwd,
      environment: authority.leaf.environment,
    }),
  })
  return Buffer.from(JSON.stringify(request), 'utf8')
}

function parseArtifact(value, label) {
  const record = requireRecord(value, label)
  const allowed = Object.hasOwn(record, 'kind')
    ? ['kind', 'path', 'byteLength', 'sha256']
    : ['path', 'byteLength', 'sha256']
  exactKeys(record, allowed, label)
  if (!Number.isSafeInteger(record.byteLength) || record.byteLength < 1) {
    throw new Error(`${label} byte length is invalid`)
  }
  return Object.freeze({
    path: canonicalAbsolutePath(record.path, label),
    byteLength: record.byteLength,
    sha256: sha256(record.sha256, `${label} digest`),
  })
}

function actualOwnershipEnvironment(insideWindowsD5, value) {
  const environment = canonicalEnvironment(value)
  const names = Object.keys(environment)
  const expected = insideWindowsD5 ? D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES : []
  if (
    names.length !== expected.length ||
    expected.some((name) => !Object.hasOwn(environment, name) || environment[name] === '')
  ) throw new Error('sample D5 ownership environment is missing, unexpected, or duplicated')
  return environment
}

function canonicalEnvironment(value) {
  const record = requireRecord(value, 'sample command environment')
  const result = {}
  for (const name of Object.keys(record).sort()) {
    const entry = record[name]
    if (
      !/^[A-Za-z_][A-Za-z0-9_]*$/u.test(name) ||
      typeof entry !== 'string' || entry.includes('\0')
    ) throw new Error('sample command environment contains an invalid entry')
    result[name] = entry
  }
  return Object.freeze(result)
}

function canonicalArguments(value) {
  if (
    !Array.isArray(value) || value.length > MAXIMUM_ARGUMENTS ||
    value.some((argument) =>
      typeof argument !== 'string' || argument.includes('\0') ||
      Buffer.byteLength(argument, 'utf8') > MAXIMUM_ARGUMENT_BYTES)
  ) throw new Error('sample command arguments are invalid')
  return Object.freeze([...value])
}

function canonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function sha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(`${label} must be lowercase 64-hex`)
  }
  return value
}

function checkoutSha(value, label) {
  if (typeof value !== 'string' || !CHECKOUT_SHA_PATTERN.test(value)) {
    throw new Error(`${label} must be lowercase 40-hex`)
  }
  return value
}

function portableToken(value, label) {
  if (typeof value !== 'string' || !PORTABLE_TOKEN_PATTERN.test(value)) {
    throw new Error(`${label} is invalid`)
  }
  return value
}

function requireRecord(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value
}

function exactKeys(value, keys, label) {
  const record = requireRecord(value, label)
  const actual = Object.keys(record)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(record, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}
