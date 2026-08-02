#!/usr/bin/env node
import { spawn } from 'node:child_process'
import { createHash, randomUUID } from 'node:crypto'
import {
  accessSync,
  chmodSync,
  closeSync,
  constants as fsConstants,
  existsSync,
  fstatSync,
  fsyncSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  opendirSync,
  readFileSync,
  realpathSync,
  rmSync,
  unlinkSync,
  writeSync,
} from 'node:fs'
import { request as httpsRequest } from 'node:https'
import { basename, isAbsolute, join, relative, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { types as nodeTypes } from 'node:util'

const MINTED_OIDC_PROTOCOL = 'windshare.browser-network-matrix.minted-oidc/v1'
const PREPARED_INPUT_SCHEMA = 'windshare.browser-network-matrix.prepared-input/v1'
const MAXIMUM_REQUEST_URL_BYTES = 4096
const MAXIMUM_REQUEST_TOKEN_BYTES = 16_384
const MAXIMUM_OIDC_RESPONSE_BYTES = 1_048_576
const MAXIMUM_MINTED_ASSERTION_BYTES = 65_536
const MAXIMUM_RUNTIME_CONFIG_BYTES = 1_048_576
const MAXIMUM_PREPARED_INPUT_BYTES = 256 * 1024 * 1024
const MAXIMUM_MANIFEST_BYTES = 1024 * 1024
const OIDC_MINT_DEADLINE_MS = 15_000
const OIDC_ASSERTION_PATTERN = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/u
const OIDC_AUDIENCE_PATTERN = /^[A-Za-z0-9._:/-]{8,512}$/u
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const DECIMAL_ID_PATTERN = /^[1-9][0-9]{0,19}$/u
const CANONICAL_RUN_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const MAXIMUM_RUN_ID_BYTES = 96
const EXPECTED_NODE_VERSION = 'v24.16.0'
const EXPECTED_MANIFEST_NODE_VERSION = '24.16.0'
const TRUSTED_EXECUTABLE_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
const REQUEST_URL_NAME = 'ACTIONS_ID_TOKEN_REQUEST_URL'
const REQUEST_TOKEN_NAME = 'ACTIONS_ID_TOKEN_REQUEST_TOKEN'
const RUNTIME_CONFIG_NAME = 'BROWSER_NETWORK_RUNTIME_CONFIG'
const PREPARED_DIRECTORY_NAME = 'BROWSER_NETWORK_PREPARED_DIRECTORY'
const PRODUCER_MANIFEST_SHA_NAME = 'BROWSER_NETWORK_PRODUCER_MANIFEST_SHA256'
const OIDC_AUDIENCE_NAME = 'WINDSHARE_OIDC_AUDIENCE'
const BLANK_STARTUP_ENVIRONMENT_NAMES = Object.freeze([
  'BASH_ENV',
  'ENV',
  'LD_AUDIT',
  'LD_LIBRARY_PATH',
  'LD_PRELOAD',
  'NODE_EXTRA_CA_CERTS',
  'NODE_OPTIONS',
  'NODE_PATH',
  'NODE_TLS_REJECT_UNAUTHORIZED',
  'OPENSSL_CONF',
  'SSL_CERT_DIR',
  'SSL_CERT_FILE',
  'SSLKEYLOGFILE',
])
const PRODUCER_MANIFEST_KEYS = Object.freeze([
  'schemaVersion',
  'checkoutSha',
  'nodeVersion',
  'broker',
  'runtimeBundle',
  'completionBundle',
  'scheduledManifest',
  'scheduledProfiles',
  'publisherHelper',
  'processOwner',
])
const EXPECTED_PROFILE_FILE_NAMES = Object.freeze([
  'profiles/scheduled-coturn.v2.json',
  'profiles/scheduled-public-stun.v2.json',
  'profiles/scheduled-restricted-udp.v2.json',
])

// This capture is the first authority-bearing operation after stdlib module
// loading. Repository and artifact bytes remain unreachable while the request
// URL and bearer are ambient environment capabilities.
const requestAuthority = captureAndScrubOidcRequest(process.env)
scrubProcessStartupEnvironment(process.env)
const operationId = `oidc-network-${randomUUID()}`

run(requestAuthority, operationId).then(
  (exitCode) => { process.exitCode = exitCode },
  () => {
    process.stderr.write(`${JSON.stringify({
      schemaVersion: 'windshare.browser-network-matrix.oidc-broker/v1',
      operationId,
      milestone: 'oidc-network-terminal',
      outcome: 'failed',
      failureCode: 'oidc-network-broker-failed',
    })}\n`)
    process.exitCode = 1
  },
)

async function run(captured, stableOperationId) {
  emit(stableOperationId, 'oidc-request-captured', 'completed')
  let launch
  let minted
  try {
    const audience = takeRequiredEnvironment(process.env, OIDC_AUDIENCE_NAME, OIDC_AUDIENCE_PATTERN)
    launch = capturePreparedLaunchAuthority(process.env)
    minted = await mintOidcAssertion(captured, audience)
    emit(stableOperationId, 'oidc-assertion-minted', 'completed')
    captured.requestToken.fill(0)
    emit(stableOperationId, 'oidc-request-authority-closed', 'completed')

    const exitCode = await executeNetworkChild(launch, minted, audience, stableOperationId)
    if (exitCode !== 0) {
      emit(stableOperationId, 'oidc-network-terminal', 'failed')
      return exitCode
    }

    // The completion publisher is a producer-built private bundle. It is loaded
    // only after request and minted credentials have been zeroed and the network
    // child (including its cleanup/identity leases) has exited successfully.
    const completionModule = await import(pathToFileURL(launch.completionBundle).href)
    if (typeof completionModule.publishNetworkCompletion !== 'function') {
      throw new Error('prepared completion bundle lacks its publisher')
    }
    await completionModule.publishNetworkCompletion({
      repositoryRoot: launch.repositoryRoot,
      outputRoot: launch.outputRoot,
      runId: launch.runId,
      checkoutSha: launch.checkoutSha,
      producerManifestPath: launch.producerManifest,
      producerManifestSha256: launch.producerManifestSha256,
      runtimeHelperManifestPath: launch.helperManifest,
      runtimeHelperManifestSha256: launch.runtimeHelperManifestSha256,
      runtimeConfigSha256: launch.runtimeConfigSha256,
      childExitCode: 0,
      childSignal: null,
    })
    emit(stableOperationId, 'network-completion-published', 'completed')
    emit(stableOperationId, 'oidc-network-terminal', 'completed')
    return 0
  } finally {
    captured.requestToken.fill(0)
    minted?.assertion.fill(0)
    launch?.runtimeConfigBytes.fill(0)
    launch?.close()
  }
}

async function executeNetworkChild(launch, minted, audience, stableOperationId) {
  const runtimeConfigDescriptor = createUnlinkedRuntimeConfig(
    launch.runtimeConfigBytes,
    launch.runtimeRoot,
  )
  launch.runtimeConfigBytes.fill(0)
  let child
  try {
    child = spawn(process.execPath, [
      '--experimental-strip-types',
      launch.runtimeBundle,
      'execute',
      '--mode', 'scheduled',
      '--run-id', launch.runId,
      '--manifest', launch.manifestPath,
      '--output-root', launch.outputRoot,
      '--helper-manifest', launch.helperManifest,
      '--publisher-helper', launch.publisherHelper,
      '--process-owner', launch.processOwner,
      '--checkout-sha', launch.checkoutSha,
      '--repository-root', launch.repositoryRoot,
    ], {
      cwd: launch.repositoryRoot,
      env: scrubbedChildEnvironment(process.env),
      shell: false,
      stdio: ['ignore', 'inherit', 'inherit', 'pipe', runtimeConfigDescriptor],
      windowsHide: true,
    })
  } finally {
    closeSync(runtimeConfigDescriptor)
  }
  const childSettlement = new Promise((resolveExit, rejectExit) => {
    child.once('error', rejectExit)
    child.once('exit', (code, signal) => resolveExit({ code, signal }))
  })
  emit(stableOperationId, 'oidc-network-child-started', 'completed')
  const descriptor = child.stdio[3]
  if (descriptor === null || typeof descriptor.end !== 'function') {
    minted.assertion.fill(0)
    child.kill()
    await childSettlement.catch(() => undefined)
    throw new Error('OIDC network child descriptor was not created')
  }
  const frame = Buffer.from(JSON.stringify({
    protocolVersion: MINTED_OIDC_PROTOCOL,
    audience,
    requestOrigin: minted.requestOrigin,
    requestPath: minted.requestPath,
    requestQuery: minted.requestQuery,
    assertion: minted.assertion.toString('ascii'),
  }), 'utf8')
  let writeFailure
  try {
    await new Promise((resolveWrite, rejectWrite) => {
      descriptor.once('error', rejectWrite)
      descriptor.end(frame, resolveWrite)
    })
  } catch (cause) {
    writeFailure = cause
    child.kill()
  } finally {
    frame.fill(0)
    minted.assertion.fill(0)
  }
  const settlement = await childSettlement
  if (writeFailure !== undefined) throw new Error('OIDC network child rejected its minted authority')
  if (settlement.signal !== null) throw new Error('OIDC network child terminated by signal')
  return settlement.code ?? 1
}

function capturePreparedLaunchAuthority(environment) {
  let runtimeConfigBytes
  let runtimeRoot
  try {
    const repositoryRoot = requireCanonicalDirectory(
      takeRequiredEnvironment(environment, 'GITHUB_WORKSPACE'),
      'repository root',
    )
    if (requireCanonicalDirectory(resolve('.'), 'working directory') !== repositoryRoot) {
      throw new Error('OIDC broker working directory differs from the trusted checkout')
    }
    const runnerToolCache = requireCanonicalDirectory(
      takeRequiredEnvironment(environment, 'RUNNER_TOOL_CACHE'),
      'runner tool cache',
    )
    const expectedNode = realpathSync(join(runnerToolCache, 'node/24.16.0/x64/bin/node'))
    if (process.version !== EXPECTED_NODE_VERSION || realpathSync(process.execPath) !== expectedNode) {
      throw new Error('OIDC broker Node authority is invalid')
    }
    const runnerTemporary = requireCanonicalDirectory(
      takeRequiredEnvironment(environment, 'RUNNER_TEMP'),
      'runner temporary directory',
    )
    const githubRunId = takeRequiredEnvironment(environment, 'GITHUB_RUN_ID', DECIMAL_ID_PATTERN)
    const githubRunAttempt = takeRequiredEnvironment(environment, 'GITHUB_RUN_ATTEMPT', DECIMAL_ID_PATTERN)
    const checkoutSha = takeRequiredEnvironment(environment, 'GITHUB_SHA', CHECKOUT_SHA_PATTERN)
    const preparedDirectory = requireCanonicalDirectory(
      takeRequiredEnvironment(environment, PREPARED_DIRECTORY_NAME),
      'prepared network input directory',
    )
    if (preparedDirectory !== join(repositoryRoot, '.browser-network-prepared')) {
      throw new Error('prepared network inputs are outside the workflow authority')
    }
    if (takeRequiredEnvironment(environment, 'PATH') !== TRUSTED_EXECUTABLE_PATH) {
      throw new Error('OIDC broker executable search path is invalid')
    }
    requireExactPreparedInventory(preparedDirectory)
    const expectedProducerManifestSha256 = takeRequiredEnvironment(
      environment,
      PRODUCER_MANIFEST_SHA_NAME,
      SHA256_PATTERN,
    )
    runtimeConfigBytes = takeRequiredEnvironmentBytes(
      environment,
      RUNTIME_CONFIG_NAME,
      MAXIMUM_RUNTIME_CONFIG_BYTES,
    )
    const runtimeConfigSha256 = sha256(runtimeConfigBytes)

    const producerManifestSource = join(preparedDirectory, 'producer-manifest.json')
    const producerManifestBytes = readStableFile(
      producerManifestSource,
      'prepared producer manifest',
      MAXIMUM_MANIFEST_BYTES,
    )
    const producerManifestSha256 = sha256(producerManifestBytes)
    if (producerManifestSha256 !== expectedProducerManifestSha256) {
      throw new Error('prepared producer manifest lacks its workflow binding')
    }
    const producerManifest = parseProducerManifest(producerManifestBytes)
    if (producerManifest.checkoutSha !== checkoutSha) {
      throw new Error('prepared network inputs belong to another checkout')
    }

    const invokedBroker = requireCanonicalFile(resolve(process.argv[1] ?? ''), 'OIDC broker entrypoint')
    if (invokedBroker !== join(preparedDirectory, producerManifest.broker.fileName)) {
      throw new Error('OIDC broker entrypoint is outside the prepared artifact')
    }
    requireIdentityBytes(producerManifest.broker, readStableFile(
      invokedBroker,
      'prepared OIDC broker',
      MAXIMUM_PREPARED_INPUT_BYTES,
    ), 'prepared OIDC broker')
    requireIdentityBytes(producerManifest.broker, readStableFile(
      join(repositoryRoot, 'scripts/ci/browsergate/oidc-network-broker.mjs'),
      'checkout OIDC broker',
      MAXIMUM_PREPARED_INPUT_BYTES,
    ), 'checkout OIDC broker')

    const preparedBytes = new Map()
    for (const identity of [
      producerManifest.runtimeBundle,
      producerManifest.completionBundle,
      producerManifest.scheduledManifest,
      ...producerManifest.scheduledProfiles,
      producerManifest.publisherHelper,
      producerManifest.processOwner,
    ]) {
      const path = containedPreparedPath(preparedDirectory, identity.fileName)
      const bytes = readStableFile(path, `prepared ${identity.fileName}`, MAXIMUM_PREPARED_INPUT_BYTES)
      requireIdentityBytes(identity, bytes, `prepared ${identity.fileName}`)
      preparedBytes.set(identity.fileName, bytes)
    }
    verifyCheckoutScheduledRegistry(repositoryRoot, producerManifest)

    const runIdAuthority = takeRequiredEnvironment(environment, 'WINDSHARE_TEST_RUN_ID')
    const runId = `${runIdAuthority}-browser-network`
    if (
      Buffer.byteLength(runId, 'utf8') > MAXIMUM_RUN_ID_BYTES ||
      !CANONICAL_RUN_ID_PATTERN.test(runId)
    ) throw new Error('browser network run identity is invalid')
    const resultsRoot = join(repositoryRoot, 'test-results')
    if (!existsSync(resultsRoot)) mkdirSync(resultsRoot, { recursive: false, mode: 0o700 })
    requireCanonicalDirectory(resultsRoot, 'network result parent')
    const outputRoot = join(resultsRoot, 'browser-network')
    for (const path of [
      outputRoot,
      join(resultsRoot, 'browser-network-completion.json'),
      join(resultsRoot, 'browser-network-producer-manifest.json'),
      join(resultsRoot, 'browser-network-runtime-helper-manifest.json'),
    ]) {
      if (existsSync(path)) throw new Error('browser network completion output must be new')
    }

    runtimeRoot = mkdtempSync(join(
      runnerTemporary,
      `windshare-browser-network-runtime-${githubRunId}-${githubRunAttempt}-`,
    ))
    chmodSync(runtimeRoot, 0o700)
    mkdirSync(join(runtimeRoot, 'profiles'), { recursive: false, mode: 0o700 })
    for (const [fileName, bytes] of preparedBytes) {
      const mode = fileName === 'browsermatrixpublish' || fileName === 'testprocessowner'
        ? 0o500
        : 0o400
      writePrivateFile(join(runtimeRoot, ...fileName.split('/')), bytes, mode)
    }
    const producerManifestPath = join(runtimeRoot, 'producer-manifest.json')
    writePrivateFile(producerManifestPath, producerManifestBytes, 0o400)
    const publisherHelper = join(runtimeRoot, 'browsermatrixpublish')
    const processOwner = join(runtimeRoot, 'testprocessowner')
    const helperManifestBytes = Buffer.from(`${JSON.stringify({
      schemaVersion: 'windshare.browser-network-matrix.helper-build/v2',
      platform: 'linux',
      architecture: 'amd64',
      helpers: [
        { role: 'artifact-publisher', path: publisherHelper },
        { role: 'test-process-owner', path: processOwner },
      ],
    })}\n`, 'utf8')
    const helperManifest = join(runtimeRoot, 'helper-manifest.json')
    writePrivateFile(helperManifest, helperManifestBytes, 0o400)
    accessSync(publisherHelper, fsConstants.X_OK)
    accessSync(processOwner, fsConstants.X_OK)

    let closed = false
    return Object.freeze({
      checkoutSha,
      completionBundle: join(runtimeRoot, 'network-completion-bundle.mjs'),
      helperManifest,
      manifestPath: join(runtimeRoot, 'scheduled-hard.manifest.v2.json'),
      outputRoot,
      processOwner,
      producerManifest: producerManifestPath,
      producerManifestSha256,
      publisherHelper,
      repositoryRoot,
      runId,
      runtimeBundle: join(runtimeRoot, 'network-entry-bundle.mjs'),
      runtimeConfigBytes,
      runtimeConfigSha256,
      runtimeHelperManifestSha256: sha256(helperManifestBytes),
      runtimeRoot,
      close() {
        if (closed) return
        closed = true
        runtimeConfigBytes.fill(0)
        rmSync(runtimeRoot, { recursive: true, force: true })
      },
    })
  } catch (cause) {
    runtimeConfigBytes?.fill(0)
    if (runtimeRoot !== undefined) rmSync(runtimeRoot, { recursive: true, force: true })
    throw cause
  }
}

function parseProducerManifest(bytes) {
  const encoded = decodeUtf8(bytes, 'prepared producer manifest')
  let value
  try {
    value = JSON.parse(encoded)
  } catch {
    throw new Error('prepared producer manifest JSON is invalid')
  }
  const record = exactDataRecord(value, PRODUCER_MANIFEST_KEYS, 'prepared producer manifest')
  const profiles = denseDataArray(record.scheduledProfiles, 'prepared scheduled profile registry')
  if (profiles.length !== EXPECTED_PROFILE_FILE_NAMES.length) {
    throw new Error('prepared scheduled profile registry has the wrong size')
  }
  const parsed = Object.freeze({
    schemaVersion: requireLiteral(record.schemaVersion, PREPARED_INPUT_SCHEMA, 'prepared manifest schema'),
    checkoutSha: requireString(record.checkoutSha, CHECKOUT_SHA_PATTERN, 'prepared checkout SHA'),
    nodeVersion: requireLiteral(record.nodeVersion, EXPECTED_MANIFEST_NODE_VERSION, 'prepared Node version'),
    broker: parseIdentity(record.broker, 'oidc-network-broker.mjs', 'prepared broker'),
    runtimeBundle: parseIdentity(
      record.runtimeBundle,
      'network-entry-bundle.mjs',
      'prepared runtime bundle',
    ),
    completionBundle: parseIdentity(
      record.completionBundle,
      'network-completion-bundle.mjs',
      'prepared completion bundle',
    ),
    scheduledManifest: parseIdentity(
      record.scheduledManifest,
      'scheduled-hard.manifest.v2.json',
      'prepared scheduled manifest',
    ),
    scheduledProfiles: Object.freeze(profiles.map((entry, index) => parseIdentity(
      entry,
      EXPECTED_PROFILE_FILE_NAMES[index],
      `prepared scheduled profile ${index}`,
    ))),
    publisherHelper: parseIdentity(
      record.publisherHelper,
      'browsermatrixpublish',
      'prepared publisher helper',
    ),
    processOwner: parseIdentity(record.processOwner, 'testprocessowner', 'prepared process owner'),
  })
  if (encoded !== `${JSON.stringify(parsed)}\n`) {
    throw new Error('prepared producer manifest is not canonical JSON')
  }
  return parsed
}

function parseIdentity(value, expectedFileName, label) {
  const record = exactDataRecord(value, ['fileName', 'byteLength', 'sha256'], label)
  if (
    record.fileName !== expectedFileName || !Number.isSafeInteger(record.byteLength) ||
    record.byteLength < 1 || record.byteLength > MAXIMUM_PREPARED_INPUT_BYTES
  ) throw new Error(`${label} identity is invalid`)
  return Object.freeze({
    fileName: expectedFileName,
    byteLength: record.byteLength,
    sha256: requireString(record.sha256, SHA256_PATTERN, `${label} digest`),
  })
}

function verifyCheckoutScheduledRegistry(repositoryRoot, manifest) {
  const currentManifest = readStableFile(
    join(repositoryRoot, 'testdata/browser-network-matrix/scheduled-hard.manifest.v2.json'),
    'checkout scheduled manifest',
    MAXIMUM_MANIFEST_BYTES,
  )
  requireIdentityBytes(manifest.scheduledManifest, currentManifest, 'checkout scheduled manifest')
  for (const identity of manifest.scheduledProfiles) {
    const bytes = readStableFile(
      join(repositoryRoot, 'testdata/browser-network-matrix', ...identity.fileName.split('/')),
      `checkout ${identity.fileName}`,
      MAXIMUM_MANIFEST_BYTES,
    )
    requireIdentityBytes(identity, bytes, `checkout ${identity.fileName}`)
  }
}

function containedPreparedPath(root, relativeName) {
  const path = resolve(root, ...relativeName.split('/'))
  const relationship = relative(root, path)
  if (relationship.length === 0 || relationship.startsWith('..') || isAbsolute(relationship)) {
    throw new Error('prepared input path escapes its artifact root')
  }
  return path
}

function readStableFile(pathValue, label, maximumBytes) {
  const canonical = requireCanonicalFile(pathValue, label)
  const namedBefore = lstatSync(canonical, { bigint: true })
  const descriptor = openSync(canonical, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0))
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    if (
      !sameFileRevision(namedBefore, openedBefore) || openedBefore.size < 1n ||
      openedBefore.size > BigInt(maximumBytes)
    ) throw new Error(`${label} does not retain one bounded file revision`)
    const bytes = readFileSync(descriptor)
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(canonical, { bigint: true })
    if (
      bytes.byteLength !== Number(openedAfter.size) ||
      !sameFileRevision(openedBefore, openedAfter) || !sameFileRevision(openedAfter, namedAfter)
    ) throw new Error(`${label} changed while it was read`)
    return bytes
  } finally {
    closeSync(descriptor)
  }
}

function writePrivateFile(path, bytes, mode) {
  const descriptor = openSync(path, fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL, mode)
  try {
    let offset = 0
    while (offset < bytes.byteLength) offset += writeSync(descriptor, bytes, offset)
    fsyncSync(descriptor)
  } finally {
    closeSync(descriptor)
  }
  chmodSync(path, mode)
  const copy = readStableFile(path, `private ${basename(path)}`, MAXIMUM_PREPARED_INPUT_BYTES)
  if (copy.byteLength !== bytes.byteLength || !createHash('sha256').update(copy).digest().equals(
    createHash('sha256').update(bytes).digest(),
  )) throw new Error(`private ${basename(path)} changed during publication`)
}

function createUnlinkedRuntimeConfig(bytes, runtimeRoot) {
  const path = join(runtimeRoot, `.runtime-config-${randomUUID()}.json`)
  let writeDescriptor
  try {
    writeDescriptor = openSync(
      path,
      fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL,
      0o400,
    )
    let offset = 0
    while (offset < bytes.byteLength) offset += writeSync(writeDescriptor, bytes, offset)
    fsyncSync(writeDescriptor)
  } finally {
    if (writeDescriptor !== undefined) closeSync(writeDescriptor)
  }
  let readDescriptor
  try {
    readDescriptor = openSync(path, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0))
    const metadata = fstatSync(readDescriptor)
    if (!metadata.isFile() || metadata.size !== bytes.byteLength) {
      throw new Error('runtime config descriptor changed before retention')
    }
    unlinkSync(path)
    return readDescriptor
  } catch (cause) {
    if (readDescriptor !== undefined) closeSync(readDescriptor)
    try { unlinkSync(path) } catch {}
    throw cause
  }
}

function requireCanonicalDirectory(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} path is not absolute and canonical`)
  }
  const metadata = lstatSync(value)
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || realpathSync(value) !== value) {
    throw new Error(`${label} is not a canonical real directory`)
  }
  return value
}

function requireCanonicalFile(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} path is not absolute and canonical`)
  }
  const metadata = lstatSync(value)
  if (!metadata.isFile() || metadata.isSymbolicLink() || realpathSync(value) !== value) {
    throw new Error(`${label} is not a canonical real file`)
  }
  return value
}

function requireExactPreparedInventory(preparedDirectory) {
  const expectedRoot = new Map([
    ['browsermatrixpublish', 'file'],
    ['network-completion-bundle.mjs', 'file'],
    ['network-entry-bundle.mjs', 'file'],
    ['oidc-network-broker.mjs', 'file'],
    ['producer-manifest.json', 'file'],
    ['profiles', 'directory'],
    ['scheduled-hard.manifest.v2.json', 'file'],
    ['testprocessowner', 'file'],
  ])
  requireExactDirectoryEntries(preparedDirectory, expectedRoot, 'prepared network input')
  const profilesDirectory = join(preparedDirectory, 'profiles')
  requireCanonicalDirectory(profilesDirectory, 'prepared profile directory')
  requireExactDirectoryEntries(profilesDirectory, new Map(EXPECTED_PROFILE_FILE_NAMES.map((fileName) => [
    basename(fileName),
    'file',
  ])), 'prepared profile')
}

function requireExactDirectoryEntries(directory, expected, label) {
  const authority = opendirSync(directory)
  const observed = new Map()
  try {
    while (true) {
      const entry = authority.readSync()
      if (entry === null) break
      // Reading only N+1 entries makes a hostile large directory fail with bounded work.
      if (observed.size === expected.size) throw new Error(`${label} inventory contains an extra entry`)
      observed.set(entry.name, entry)
    }
  } finally {
    authority.closeSync()
  }
  if (observed.size !== expected.size) throw new Error(`${label} inventory is incomplete`)
  for (const [name, kind] of expected) {
    const entry = observed.get(name)
    if (
      entry === undefined || entry.isSymbolicLink() ||
      kind === 'file' && !entry.isFile() || kind === 'directory' && !entry.isDirectory()
    ) throw new Error(`${label} inventory entry is invalid: ${name}`)
  }
}

function captureAndScrubOidcRequest(environment) {
  const selected = new Map()
  for (const name of Object.keys(environment)) {
    const folded = name.toUpperCase()
    if (folded === REQUEST_URL_NAME || folded === REQUEST_TOKEN_NAME) {
      if (selected.has(folded)) throw new Error('OIDC request environment contains duplicate names')
      const descriptor = Object.getOwnPropertyDescriptor(environment, name)
      if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || typeof descriptor.value !== 'string') {
        throw new Error('OIDC request environment is active')
      }
      selected.set(folded, Object.freeze({ name, value: descriptor.value }))
    }
  }
  for (const { name } of selected.values()) delete environment[name]
  const requestUrl = selected.get(REQUEST_URL_NAME)?.value
  const requestTokenValue = selected.get(REQUEST_TOKEN_NAME)?.value
  const requestToken = typeof requestTokenValue === 'string'
    ? Buffer.from(requestTokenValue, 'utf8')
    : Buffer.alloc(0)
  if (
    typeof requestUrl !== 'string' || requestUrl.length === 0 || requestUrl.includes('\0') ||
    Buffer.byteLength(requestUrl, 'utf8') > MAXIMUM_REQUEST_URL_BYTES ||
    typeof requestTokenValue !== 'string' || requestTokenValue.length === 0 ||
    requestTokenValue.includes('\0') || requestToken.byteLength > MAXIMUM_REQUEST_TOKEN_BYTES
  ) {
    requestToken.fill(0)
    throw new Error('OIDC request environment is unavailable')
  }
  return Object.freeze({ requestUrl, requestToken })
}

function scrubProcessStartupEnvironment(environment) {
  const selected = new Map()
  for (const name of Object.keys(environment)) {
    const folded = name.toUpperCase()
    if (!BLANK_STARTUP_ENVIRONMENT_NAMES.includes(folded)) continue
    if (selected.has(folded)) throw new Error('process startup environment contains duplicate names')
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.value !== '') {
      throw new Error('process startup environment must be explicitly blank')
    }
    selected.set(folded, name)
  }
  if (selected.size !== BLANK_STARTUP_ENVIRONMENT_NAMES.length) {
    throw new Error('process startup environment must be explicitly blank')
  }
  for (const name of selected.values()) delete environment[name]
}

async function mintOidcAssertion(captured, audience) {
  const base = new URL(captured.requestUrl)
  if (
    base.protocol !== 'https:' || base.username !== '' || base.password !== '' ||
    base.hash !== '' || base.origin === 'null'
  ) throw new Error('OIDC request URL is invalid')
  const requestOrigin = base.origin
  const requestPath = base.pathname
  const requestQuery = base.search
  base.searchParams.set('audience', audience)
  let authorization = `Bearer ${new TextDecoder('utf-8', { fatal: true }).decode(captured.requestToken)}`
  try {
    const assertion = await new Promise((resolveAssertion, rejectAssertion) => {
      let chunks = []
      let length = 0
      let settled = false
      let responseAuthority
      let requestAuthority
      let deadline
      const fail = () => {
        if (settled) return
        settled = true
        if (deadline !== undefined) clearTimeout(deadline)
        for (const chunk of chunks) chunk.fill(0)
        chunks = []
        responseAuthority?.destroy()
        requestAuthority?.destroy()
        rejectAssertion(new Error('OIDC mint request failed'))
      }
      requestAuthority = httpsRequest(base, {
        method: 'GET',
        headers: { Authorization: authorization, Accept: 'application/json' },
        agent: false,
        minVersion: 'TLSv1.2',
      }, (response) => {
        responseAuthority = response
        if (response.statusCode !== 200 || response.headers.location !== undefined) {
          response.destroy()
          fail()
          return
        }
        response.on('data', (chunk) => {
          if (settled) { chunk.fill(0); return }
          length += chunk.byteLength
          if (length > MAXIMUM_OIDC_RESPONSE_BYTES) {
            chunk.fill(0)
            response.destroy()
            fail()
            return
          }
          chunks.push(chunk)
        })
        response.once('aborted', fail)
        response.once('error', fail)
        response.once('end', () => {
          if (settled) return
          const bytes = Buffer.concat(chunks, length)
          for (const chunk of chunks) chunk.fill(0)
          chunks = []
          try {
            const value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
            const record = exactDataRecord(value, ['value'], 'OIDC response')
            if (
              typeof record.value !== 'string' ||
              Buffer.byteLength(record.value, 'ascii') > MAXIMUM_MINTED_ASSERTION_BYTES ||
              !OIDC_ASSERTION_PATTERN.test(record.value)
            ) throw new Error('OIDC response is invalid')
            settled = true
            if (deadline !== undefined) clearTimeout(deadline)
            resolveAssertion(Buffer.from(record.value, 'ascii'))
          } catch {
            fail()
          } finally {
            bytes.fill(0)
          }
        })
      })
      deadline = setTimeout(fail, OIDC_MINT_DEADLINE_MS)
      requestAuthority.once('error', fail)
      requestAuthority.end()
    })
    return Object.freeze({ requestOrigin, requestPath, requestQuery, assertion })
  } finally {
    authorization = undefined
  }
}

function takeRequiredEnvironment(environment, expectedName, pattern) {
  let selected
  for (const name of Object.keys(environment)) {
    if (name.toUpperCase() !== expectedName.toUpperCase()) continue
    if (selected !== undefined) throw new Error(`${expectedName} is duplicated`)
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || typeof descriptor.value !== 'string') {
      throw new Error(`${expectedName} is active`)
    }
    selected = Object.freeze({ name, value: descriptor.value })
  }
  if (
    selected === undefined || selected.value.length === 0 || selected.value.includes('\0') ||
    pattern !== undefined && !pattern.test(selected.value)
  ) throw new Error(`${expectedName} is invalid`)
  delete environment[selected.name]
  return selected.value
}

function takeRequiredEnvironmentBytes(environment, expectedName, maximumBytes) {
  const value = takeRequiredEnvironment(environment, expectedName)
  const bytes = Buffer.from(value, 'utf8')
  if (bytes.byteLength === 0 || bytes.byteLength > maximumBytes) {
    bytes.fill(0)
    throw new Error(`${expectedName} exceeds its byte authority`)
  }
  return bytes
}

function scrubbedChildEnvironment(environment) {
  const child = Object.create(null)
  for (const [name, value] of Object.entries(environment)) {
    const folded = name.toUpperCase()
    if (
      folded === REQUEST_URL_NAME || folded === REQUEST_TOKEN_NAME ||
      folded === OIDC_AUDIENCE_NAME || folded === RUNTIME_CONFIG_NAME ||
      folded === PREPARED_DIRECTORY_NAME || folded === PRODUCER_MANIFEST_SHA_NAME ||
      folded === 'GITHUB_ENV' || folded === 'GITHUB_PATH' || folded === 'PATH' ||
      BLANK_STARTUP_ENVIRONMENT_NAMES.includes(folded)
    ) continue
    Object.defineProperty(child, name, {
      value,
      enumerable: true,
      configurable: true,
      writable: true,
    })
  }
  Object.defineProperty(child, 'PATH', {
    value: TRUSTED_EXECUTABLE_PATH,
    enumerable: true,
    configurable: true,
    writable: true,
  })
  return child
}

function exactDataRecord(value, expectedKeys, label) {
  if (
    typeof value !== 'object' || value === null || Array.isArray(value) || nodeTypes.isProxy(value) ||
    (Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null)
  ) throw new Error(`${label} must be one inert object`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const keys = Reflect.ownKeys(descriptors)
  if (
    keys.length !== expectedKeys.length || keys.some((key) => typeof key !== 'string') ||
    keys.some((key, index) => key !== expectedKeys[index])
  ) throw new Error(`${label} fields are invalid`)
  const record = Object.create(null)
  for (const key of expectedKeys) {
    const descriptor = descriptors[key]
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true) {
      throw new Error(`${label} contains an active field`)
    }
    record[key] = descriptor.value
  }
  return record
}

function denseDataArray(value, label) {
  if (!Array.isArray(value) || nodeTypes.isProxy(value)) throw new Error(`${label} must be one inert array`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const keys = Reflect.ownKeys(descriptors)
  const length = descriptors.length?.value
  if (
    !Number.isSafeInteger(length) || length < 0 || keys.length !== length + 1 ||
    keys.some((key) => typeof key !== 'string')
  ) throw new Error(`${label} must be dense`)
  const result = []
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true) {
      throw new Error(`${label} contains an active entry`)
    }
    result.push(descriptor.value)
  }
  return result
}

function requireIdentityBytes(identity, bytes, label) {
  if (bytes.byteLength !== identity.byteLength || sha256(bytes) !== identity.sha256) {
    throw new Error(`${label} differs from its prepared identity`)
  }
}

function requireString(value, pattern, label) {
  if (typeof value !== 'string' || !pattern.test(value)) throw new Error(`${label} is invalid`)
  return value
}

function requireLiteral(value, expected, label) {
  if (value !== expected) throw new Error(`${label} is invalid`)
  return expected
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not UTF-8`)
  }
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function sameFileRevision(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function emit(stableOperationId, milestone, outcome) {
  process.stdout.write(`${JSON.stringify({
    schemaVersion: 'windshare.browser-network-matrix.oidc-broker/v1',
    operationId: stableOperationId,
    milestone,
    outcome,
  })}\n`)
}
