import { createHash } from 'node:crypto'
import {
  chmodSync,
  closeSync,
  fstatSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'

import { sampleProcessEnvironment } from '../../../web/scripts/browser-evidence/process/sample-environment.ts'

export const BROWSERGATE_RUNTIME_MANIFEST_ENV = 'WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST'
export const BROWSERGATE_RUNTIME_MANIFEST_SHA256_ENV =
  'WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST_SHA256'
export const PION_SERVER_EXECUTABLE_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE'
export const PION_SERVER_EXECUTABLE_SHA256_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE_SHA256'

const RUNTIME_SCHEMA_VERSION = 2
const RUNTIME_MANIFEST_FILENAME = 'runtime-manifest.json'
const RUNTIME_DIRECTORY_PREFIX = 'windshare-browsergate-runtime-'
const RUNTIME_DIRECTORY_PATTERN = /^windshare-browsergate-runtime-[A-Za-z0-9]{6}$/u
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const SUITES = Object.freeze(['main', 'pion'])
const SUPPORTED_PLATFORMS = Object.freeze(['darwin', 'linux', 'win32'])
const SAMPLE_DRIVER_RELATIVE_PATH = join(
  'web',
  'scripts',
  'browser-evidence',
  'sample-driver.ts',
)
const PLAYWRIGHT_CLI_RELATIVE_PATH = join(
  'web',
  'node_modules',
  '@playwright',
  'test',
  'cli.js',
)
const MAXIMUM_SAMPLE_SOURCE_BYTES = 16 * 1_024 * 1_024
const MAXIMUM_SAMPLE_EXECUTABLE_BYTES = 512 * 1_024 * 1_024
const ARTIFACT_SPECS = Object.freeze({
  'topology-materializer': Object.freeze({
    packagePath: './web/scripts/browser-evidence/topology-resolution',
    filename: 'topology-materializer',
  }),
  'artifact-publisher': Object.freeze({
    packagePath: './osfs/cmd/browsermatrixpublish',
    moduleDirectory: 'core',
    filename: 'artifact-publisher',
  }),
  'pion-server': Object.freeze({
    packagePath: './transport/webrtc/testdata/browser/server',
    filename: 'pion-browser-server',
  }),
  'windows-job': Object.freeze({
    packagePath: './web/scripts/browser-evidence/windowsjob',
    filename: 'browser-evidence-windowsjob',
  }),
  'linux-process-owner': Object.freeze({
    packagePath: './web/scripts/browser-evidence/linuxprocessowner',
    filename: 'browser-evidence-linux-process-owner',
  }),
})
const SELF_CHECK_LINES = Object.freeze({
  'topology-materializer':
    '{"schemaVersion":1,"component":"browser-evidence-topology-resolution","outcome":"ready"}\n',
  'artifact-publisher':
    '{"schemaVersion":"windshare.artifact-publisher/v2","outcome":"ready"}\n',
  'pion-server':
    '{"schemaVersion":1,"component":"pion-browser-interop-server","outcome":"ready"}\n',
  'windows-job':
    '{"schemaVersion":1,"component":"browser-evidence-windows-job","outcome":"ready"}\n',
  'linux-process-owner':
    '{"schemaVersion":1,"component":"browser-evidence-linux-process-owner","outcome":"ready"}\n',
})
const OWNED_EXECUTION_KEYS = Object.freeze([
  'processEvidence',
  'timedOut',
  'launched',
  'treeEmpty',
  'inputEvidence',
  'clientIoEvidence',
  'ownershipEvidence',
  'stdout',
  'stderr',
])
const INPUT_EVIDENCE_KEYS = Object.freeze(['outcome', 'failureCode', 'failureMessage'])
const CLIENT_IO_EVIDENCE_KEYS = Object.freeze([
  'requestOutcome',
  'rawInputOutcome',
  'controlOutcome',
  'outputOutcome',
  'failureCode',
  'failureMessage',
])
const WINDOWS_OWNERSHIP_EVIDENCE_KEYS = Object.freeze([
  'supervisionOutcome',
  'terminationReason',
  'activeProcessCount',
  'root',
  'spawnFailure',
])
const LINUX_OWNERSHIP_EVIDENCE_KEYS = Object.freeze([
  'ownerPid',
  'rootPid',
  'rootStartTimeTicks',
  'inventoryScans',
  'maximumObservedDescendants',
  'quietInventoryCount',
  'controlOutcome',
  'cleanupOutcome',
  'failureCode',
  'failureMessage',
])

export async function buildBrowsergateRuntime({
  repositoryRoot,
  suites,
  platform = process.platform,
  outputParent = tmpdir(),
  inheritedEnvironment = process.env,
  nodeExecutable = process.execPath,
  executeBuild,
  executePreflight,
  trace = () => undefined,
  preserveRuntimeRoot = false,
}) {
  const root = requireCanonicalAbsolutePath(repositoryRoot, 'repository root')
  const runtimePlatform = requirePlatform(platform, 'runtime build platform')
  const canonicalOutputParent = requireCanonicalAbsolutePath(outputParent, 'runtime output parent')
  const selectedSuites = canonicalSuites(suites)
  const sampleCommand = buildSampleCommandCapability({
    repositoryRoot: root,
    nodeExecutable,
    inheritedEnvironment,
    platform: runtimePlatform,
  })
  if (typeof executeBuild !== 'function') throw new Error('runtime build executor is required')
  if (typeof executePreflight !== 'function') throw new Error('runtime preflight executor is required')
  mkdirSync(canonicalOutputParent, { recursive: true, mode: 0o700 })
  const runtimeRoot = resolve(mkdtempSync(join(canonicalOutputParent, RUNTIME_DIRECTORY_PREFIX)))
  const kinds = [
    ...(runtimePlatform === 'win32' ? ['windows-job'] : []),
    ...(runtimePlatform === 'linux' ? ['linux-process-owner'] : []),
    'topology-materializer',
    'artifact-publisher',
    ...(selectedSuites.includes('pion') ? ['pion-server'] : []),
  ]
  const startedAtMs = Date.now()
  try {
    emitTrace(trace, Object.freeze({ milestone: 'runtime-build-started', context: { kinds } }))
    const artifacts = []
    for (const kind of kinds) {
      const spec = ARTIFACT_SPECS[kind]
      const executablePath = join(
        runtimeRoot,
        spec.filename + (runtimePlatform === 'win32' ? '.exe' : ''),
      )
      const buildExecution = await executeBuild(Object.freeze({
        kind,
        packagePath: spec.packagePath,
        outputPath: executablePath,
        repositoryRoot: root,
        cwd: spec.moduleDirectory === undefined ? root : join(root, spec.moduleDirectory),
        platform: runtimePlatform,
        availableArtifacts: Object.freeze([...artifacts]),
      }))
      requireSuccessfulOwnedExecution(buildExecution, `runtime build ${kind}`, '', runtimePlatform)
      const snapshot = executableSnapshot(executablePath, `runtime ${kind}`)
      const artifact = Object.freeze({
        kind,
        path: executablePath,
        byteLength: snapshot.byteLength,
        sha256: snapshot.sha256,
        preflightSha256: sha256Bytes(Buffer.from(SELF_CHECK_LINES[kind], 'utf8')),
      })
      const preflight = await executePreflight(Object.freeze({
        kind,
        executablePath,
        executableByteLength: snapshot.byteLength,
        executableSha256: snapshot.sha256,
        arguments: Object.freeze(['self-check']),
        cwd: spec.moduleDirectory === undefined ? root : join(root, spec.moduleDirectory),
        platform: runtimePlatform,
        availableArtifacts: Object.freeze([...artifacts, artifact]),
      }))
      requireSuccessfulOwnedExecution(
        preflight,
        `runtime preflight ${kind}`,
        SELF_CHECK_LINES[kind],
        runtimePlatform,
      )
      artifacts.push(artifact)
    }
    const manifest = Object.freeze({
      schemaVersion: RUNTIME_SCHEMA_VERSION,
      suites: selectedSuites,
      platform: runtimePlatform,
      sampleCommand,
      artifacts: Object.freeze(artifacts),
    })
    const manifestPath = join(runtimeRoot, RUNTIME_MANIFEST_FILENAME)
    const manifestBytes = Buffer.from(JSON.stringify(manifest), 'utf8')
    writeFileSync(manifestPath, manifestBytes, { flag: 'wx', mode: 0o400 })
    chmodSync(manifestPath, 0o400)
    const manifestSha256 = sha256Bytes(manifestBytes)
    emitTrace(trace, Object.freeze({
      milestone: 'runtime-build-completed',
      context: {
        kinds,
        elapsedMs: Date.now() - startedAtMs,
        manifestPath,
        manifestSha256,
      },
    }))
    return openRuntimeAuthority({
      manifestPath,
      expectedManifestSha256: manifestSha256,
      ownsRuntimeRoot: !preserveRuntimeRoot,
      expectedPlatform: runtimePlatform,
    })
  } catch (cause) {
    emitTrace(trace, Object.freeze({
      milestone: 'runtime-build-failed',
      context: { elapsedMs: Date.now() - startedAtMs, error: errorMessage(cause) },
    }))
    rmSync(runtimeRoot, { force: true, recursive: true })
    throw cause
  }
}

export function loadBrowsergateRuntime({
  manifestPath,
  manifestSha256,
  expectedPlatform = process.platform,
}) {
  return openRuntimeAuthority({
    manifestPath,
    expectedManifestSha256: manifestSha256,
    ownsRuntimeRoot: false,
    expectedPlatform,
  })
}

export function disposeBrowsergateRuntime({
  manifestPath,
  manifestSha256,
  expectedPlatform = process.platform,
}) {
  const canonicalManifestPath = requireCanonicalAbsolutePath(
    manifestPath,
    'browsergate runtime manifest',
  )
  if (canonicalManifestPath !== join(dirname(canonicalManifestPath), RUNTIME_MANIFEST_FILENAME)) {
    throw new Error('browsergate runtime manifest must be the direct canonical manifest child')
  }
  const runtimeRoot = dirname(canonicalManifestPath)
  const runtimeRootMetadata = lstatSync(runtimeRoot)
  if (
    !runtimeRootMetadata.isDirectory() || runtimeRootMetadata.isSymbolicLink() ||
    !RUNTIME_DIRECTORY_PATTERN.test(basename(runtimeRoot))
  ) throw new Error('browsergate runtime cleanup root is not an invocation-private runtime directory')
  const authority = openRuntimeAuthority({
    manifestPath: canonicalManifestPath,
    expectedManifestSha256: manifestSha256,
    ownsRuntimeRoot: true,
    expectedPlatform,
  })
  authority.dispose()
  return Object.freeze({ runtimeRoot })
}

function openRuntimeAuthority({
  manifestPath,
  expectedManifestSha256,
  ownsRuntimeRoot,
  expectedPlatform,
}) {
  const canonicalManifestPath = requireCanonicalAbsolutePath(
    manifestPath,
    'browsergate runtime manifest',
  )
  const runtimePlatform = requirePlatform(expectedPlatform, 'expected runtime platform')
  requireSha256(expectedManifestSha256, 'browsergate runtime manifest SHA-256')
  const manifestSnapshot = regularFileSnapshot(
    canonicalManifestPath,
    1_048_576,
    'browsergate runtime manifest',
  )
  if (manifestSnapshot.sha256 !== expectedManifestSha256) {
    throw new Error('browsergate runtime manifest differs from its injected digest')
  }
  const encoded = manifestSnapshot.bytes.toString('utf8')
  const manifest = parseRuntimeManifest(
    encoded,
    dirname(canonicalManifestPath),
    runtimePlatform,
  )
  const descriptor = openSync(canonicalManifestPath, 'r')
  const heldIdentity = fstatSync(descriptor, { bigint: true })
  let disposed = false

  function assertAuthorityLive() {
    if (disposed) throw new Error('browsergate runtime authority is disposed')
    const named = lstatSync(canonicalManifestPath, { bigint: true })
    const opened = fstatSync(descriptor, { bigint: true })
    if (!sameIdentity(heldIdentity, opened) || !sameIdentity(opened, named)) {
      throw new Error('browsergate runtime manifest identity changed while held')
    }
    const current = regularFileSnapshot(
      canonicalManifestPath,
      1_048_576,
      'browsergate runtime manifest',
    )
    if (current.sha256 !== expectedManifestSha256) {
      throw new Error('browsergate runtime manifest changed while held')
    }
  }

  return Object.freeze({
    manifestPath: canonicalManifestPath,
    manifestSha256: expectedManifestSha256,
    manifest,
    artifact(kind) {
      assertAuthorityLive()
      const artifact = manifest.artifacts.find((candidate) => candidate.kind === kind)
      if (artifact === undefined) throw new Error(`browsergate runtime lacks ${kind}`)
      const current = executableSnapshot(artifact.path, `runtime ${kind}`)
      if (current.byteLength !== artifact.byteLength || current.sha256 !== artifact.sha256) {
        throw new Error(`browsergate runtime ${kind} differs from its manifest`)
      }
      return artifact
    },
    environmentForSuite(suite) {
      requireSuite(suite)
      assertAuthorityLive()
      const environment = {
        [BROWSERGATE_RUNTIME_MANIFEST_ENV]: canonicalManifestPath,
        [BROWSERGATE_RUNTIME_MANIFEST_SHA256_ENV]: expectedManifestSha256,
      }
      if (suite === 'pion') {
        const pion = this.artifact('pion-server')
        environment[PION_SERVER_EXECUTABLE_ENV] = pion.path
        environment[PION_SERVER_EXECUTABLE_SHA256_ENV] = pion.sha256
      }
      return Object.freeze(environment)
    },
    sampleCommandCapability() {
      assertAuthorityLive()
      assertSampleCommandCapabilityLive(manifest.sampleCommand)
      return manifest.sampleCommand
    },
    dispose() {
      if (disposed) return
      disposed = true
      closeSync(descriptor)
      if (ownsRuntimeRoot) {
        rmSync(dirname(canonicalManifestPath), { force: true, recursive: true })
      }
    },
  })
}

function parseRuntimeManifest(encoded, runtimeRoot, expectedPlatform) {
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('browsergate runtime manifest is invalid JSON', { cause })
  }
  if (JSON.stringify(value) !== encoded || !isRecord(value)) {
    throw new Error('browsergate runtime manifest is not canonical JSON')
  }
  exactKeys(
    value,
    ['schemaVersion', 'suites', 'platform', 'sampleCommand', 'artifacts'],
    'runtime manifest',
  )
  if (value.schemaVersion !== RUNTIME_SCHEMA_VERSION) {
    throw new Error('browsergate runtime manifest schema is unsupported')
  }
  const suites = canonicalSuites(value.suites)
  const platform = requirePlatform(value.platform, 'browsergate runtime platform')
  if (platform !== expectedPlatform) {
    throw new Error('browsergate runtime platform differs from the current runner')
  }
  const sampleCommand = parseSampleCommandCapability(value.sampleCommand, platform)
  if (!Array.isArray(value.artifacts)) throw new Error('runtime artifacts must be an array')
  const kinds = new Set()
  const paths = new Set()
  const artifacts = value.artifacts.map((artifact, index) => {
    exactKeys(
      artifact,
      ['kind', 'path', 'byteLength', 'sha256', 'preflightSha256'],
      `runtime artifact ${index}`,
    )
    if (!Object.hasOwn(ARTIFACT_SPECS, artifact.kind) || kinds.has(artifact.kind)) {
      throw new Error('runtime artifact kind is invalid or repeated')
    }
    const path = requireCanonicalAbsolutePath(artifact.path, `runtime artifact ${index}`)
    if (dirname(path) !== runtimeRoot || paths.has(path)) {
      throw new Error('runtime artifact path escapes or repeats its private root')
    }
    if (!Number.isSafeInteger(artifact.byteLength) || artifact.byteLength < 1) {
      throw new Error('runtime artifact byte length is invalid')
    }
    requireSha256(artifact.sha256, `runtime artifact ${index} SHA-256`)
    requireSha256(artifact.preflightSha256, `runtime artifact ${index} preflight SHA-256`)
    if (
      artifact.preflightSha256 !== sha256Bytes(Buffer.from(SELF_CHECK_LINES[artifact.kind], 'utf8'))
    ) throw new Error('runtime artifact preflight proof is invalid')
    kinds.add(artifact.kind)
    paths.add(path)
    return Object.freeze({ ...artifact, path })
  })
  const expectedKinds = [
    ...(platform === 'win32' ? ['windows-job'] : []),
    ...(platform === 'linux' ? ['linux-process-owner'] : []),
    'topology-materializer',
    'artifact-publisher',
    ...(suites.includes('pion') ? ['pion-server'] : []),
  ]
  if (
    artifacts.length !== expectedKinds.length ||
    artifacts.some((artifact, index) => artifact.kind !== expectedKinds[index])
  ) throw new Error('runtime manifest does not contain its exact ordered artifact set')
  return Object.freeze({
    ...value,
    suites,
    platform,
    sampleCommand,
    artifacts: Object.freeze(artifacts),
  })
}

function buildSampleCommandCapability({
  repositoryRoot,
  nodeExecutable,
  inheritedEnvironment,
  platform,
}) {
  const nodePath = requireCanonicalAbsolutePath(nodeExecutable, 'sample command Node executable')
  const driverSourcePath = join(repositoryRoot, SAMPLE_DRIVER_RELATIVE_PATH)
  const playwrightCliPath = join(repositoryRoot, PLAYWRIGHT_CLI_RELATIVE_PATH)
  return Object.freeze({
    repositoryRoot,
    node: fileAuthority(
      nodePath,
      MAXIMUM_SAMPLE_EXECUTABLE_BYTES,
      'sample command Node executable',
    ),
    driverSource: fileAuthority(
      driverSourcePath,
      MAXIMUM_SAMPLE_SOURCE_BYTES,
      'sample command driver source',
    ),
    playwrightCli: fileAuthority(
      playwrightCliPath,
      MAXIMUM_SAMPLE_SOURCE_BYTES,
      'sample command Playwright CLI',
    ),
    environment: sampleProcessEnvironment({}, {}, inheritedEnvironment, platform),
  })
}

function parseSampleCommandCapability(value, platform) {
  exactKeys(value, [
    'repositoryRoot', 'node', 'driverSource', 'playwrightCli', 'environment',
  ], 'sample command runtime capability')
  const repositoryRoot = requireCanonicalAbsolutePath(
    value.repositoryRoot,
    'sample command repository root',
  )
  const node = parseCapabilityFile(
    value.node,
    'sample command Node executable',
    MAXIMUM_SAMPLE_EXECUTABLE_BYTES,
  )
  const driverSource = parseCapabilityFile(
    value.driverSource,
    'sample command driver source',
    MAXIMUM_SAMPLE_SOURCE_BYTES,
  )
  const playwrightCli = parseCapabilityFile(
    value.playwrightCli,
    'sample command Playwright CLI',
    MAXIMUM_SAMPLE_SOURCE_BYTES,
  )
  if (driverSource.path !== join(repositoryRoot, SAMPLE_DRIVER_RELATIVE_PATH)) {
    throw new Error('sample command driver source escapes its repository capability')
  }
  if (playwrightCli.path !== join(repositoryRoot, PLAYWRIGHT_CLI_RELATIVE_PATH)) {
    throw new Error('sample command Playwright CLI escapes its repository capability')
  }
  const environment = canonicalSampleEnvironment(value.environment, platform)
  return Object.freeze({ repositoryRoot, node, driverSource, playwrightCli, environment })
}

function parseCapabilityFile(value, label, maximumBytes) {
  exactKeys(value, ['path', 'byteLength', 'sha256'], label)
  const path = requireCanonicalAbsolutePath(value.path, label)
  if (
    !Number.isSafeInteger(value.byteLength) || value.byteLength < 1 ||
    value.byteLength > maximumBytes
  ) throw new Error(`${label} byte length is invalid`)
  requireSha256(value.sha256, `${label} SHA-256`)
  return Object.freeze({ path, byteLength: value.byteLength, sha256: value.sha256 })
}

function canonicalSampleEnvironment(value, platform) {
  if (!isRecord(value)) throw new Error('sample command runtime environment must be an object')
  const selected = sampleProcessEnvironment({}, {}, value, platform)
  const actualNames = Object.keys(value)
  const selectedNames = Object.keys(selected)
  if (
    actualNames.length !== selectedNames.length ||
    selectedNames.some((name) => !Object.hasOwn(value, name) || value[name] !== selected[name])
  ) throw new Error('sample command runtime environment is not canonical')
  return selected
}

function assertSampleCommandCapabilityLive(capability) {
  for (const [name, maximumBytes] of [
    ['node', MAXIMUM_SAMPLE_EXECUTABLE_BYTES],
    ['driverSource', MAXIMUM_SAMPLE_SOURCE_BYTES],
    ['playwrightCli', MAXIMUM_SAMPLE_SOURCE_BYTES],
  ]) {
    const expected = capability[name]
    const current = regularFileSnapshot(
      expected.path,
      maximumBytes,
      `sample command ${name}`,
    )
    if (current.byteLength !== expected.byteLength || current.sha256 !== expected.sha256) {
      throw new Error(`sample command ${name} differs from its runtime manifest capability`)
    }
  }
}

function fileAuthority(path, maximumBytes, label) {
  const snapshot = regularFileSnapshot(path, maximumBytes, label)
  if (snapshot.byteLength < 1) throw new Error(`${label} is empty`)
  return Object.freeze({ path, byteLength: snapshot.byteLength, sha256: snapshot.sha256 })
}

function canonicalSuites(value) {
  if (!Array.isArray(value) || value.length < 1) throw new Error('runtime suites are required')
  const canonical = SUITES.filter((suite) => value.includes(suite))
  if (canonical.length !== value.length || canonical.some((suite, index) => suite !== value[index])) {
    throw new Error('runtime suites must be canonical, unique, and ordered')
  }
  return Object.freeze(canonical)
}

function requireSuite(value) {
  if (!SUITES.includes(value)) throw new Error('browsergate runtime suite is invalid')
  return value
}

function requirePlatform(value, label) {
  if (!SUPPORTED_PLATFORMS.includes(value)) {
    throw new Error(`${label} must be one of ${SUPPORTED_PLATFORMS.join(', ')}`)
  }
  return value
}

function requireSuccessfulOwnedExecution(execution, label, expectedStdout, platform) {
  if (!isRecord(execution)) throw new Error(`${label} lacks process ownership evidence`)
  exactKeys(execution, OWNED_EXECUTION_KEYS, `${label} execution`)
  if (
    execution.launched !== true || execution.timedOut !== false || execution.treeEmpty !== true ||
    !isRecord(execution.processEvidence) || execution.processEvidence.terminal !== 'exited' ||
    execution.processEvidence.exitCode !== 0
  ) throw new Error(`${label} did not prove a successful empty process tree`)
  requireSuccessfulInputEvidence(execution.inputEvidence, label)
  requireSuccessfulClientIoEvidence(execution.clientIoEvidence, label)
  requireSuccessfulOwnershipEvidence(execution.ownershipEvidence, platform, label)
  if (execution.stdout !== expectedStdout) throw new Error(`${label} emitted unexpected stdout`)
  if (execution.stderr !== '') throw new Error(`${label} emitted unexpected stderr`)
}

function requireSuccessfulInputEvidence(input, label) {
  exactKeys(input, INPUT_EVIDENCE_KEYS, `${label} input evidence`)
  if (input.outcome !== 'not-requested' || input.failureCode !== '' || input.failureMessage !== '') {
    throw new Error(`${label} input evidence is not a successful no-input execution`)
  }
}

function requireSuccessfulClientIoEvidence(clientIo, label) {
  exactKeys(clientIo, CLIENT_IO_EVIDENCE_KEYS, `${label} client I/O evidence`)
  if (
    clientIo.requestOutcome !== 'delivered' || clientIo.rawInputOutcome !== 'not-requested' ||
    !['not-requested', 'delivered'].includes(clientIo.controlOutcome) ||
    clientIo.outputOutcome !== 'delivered' || clientIo.failureCode !== '' ||
    clientIo.failureMessage !== ''
  ) throw new Error(`${label} client I/O evidence is not successful`)
}

function requireSuccessfulOwnershipEvidence(ownership, platform, label) {
  if (platform === 'win32') {
    exactKeys(ownership, WINDOWS_OWNERSHIP_EVIDENCE_KEYS, `${label} Windows ownership evidence`)
    exactKeys(ownership.root, ['pid', 'exitCode'], `${label} Windows root evidence`)
    if (
      ownership.supervisionOutcome !== 'tree-empty' || ownership.terminationReason !== 'natural' ||
      ownership.activeProcessCount !== 0 || !Number.isSafeInteger(ownership.root.pid) ||
      ownership.root.pid < 1 || ownership.root.exitCode !== 0 || ownership.spawnFailure !== null
    ) throw new Error(`${label} Windows ownership evidence is not successful`)
    return
  }
  if (platform === 'linux') {
    exactKeys(ownership, LINUX_OWNERSHIP_EVIDENCE_KEYS, `${label} Linux ownership evidence`)
    if (
      !Number.isSafeInteger(ownership.ownerPid) || ownership.ownerPid < 1 ||
      !Number.isSafeInteger(ownership.rootPid) || ownership.rootPid < 1 ||
      typeof ownership.rootStartTimeTicks !== 'string' || !/^[1-9][0-9]*$/u.test(ownership.rootStartTimeTicks) ||
      ownership.cleanupOutcome !== 'completed' || ownership.failureCode !== '' ||
      ownership.failureMessage !== ''
    ) throw new Error(`${label} Linux ownership evidence is not successful`)
    return
  }
  throw new Error(`${label} process ownership platform is unsupported`)
}

function executableSnapshot(path, label) {
  const snapshot = regularFileSnapshot(path, 512 * 1_024 * 1_024, label)
  if (snapshot.byteLength < 1) throw new Error(`${label} is empty`)
  return snapshot
}

function regularFileSnapshot(path, maximumBytes, label) {
  const metadataBefore = lstatSync(path, { bigint: true })
  if (
    !metadataBefore.isFile() || metadataBefore.isSymbolicLink() ||
    metadataBefore.size < 0n || metadataBefore.size > BigInt(maximumBytes)
  ) throw new Error(`${label} is not a bounded regular file`)
  const bytes = readFileSync(path)
  const metadataAfter = lstatSync(path, { bigint: true })
  if (!sameIdentity(metadataBefore, metadataAfter) || !sameRevision(metadataBefore, metadataAfter)) {
    throw new Error(`${label} changed while read`)
  }
  return Object.freeze({
    bytes,
    byteLength: bytes.byteLength,
    sha256: sha256Bytes(bytes),
  })
}

function sameIdentity(left, right) {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left, right) {
  return left.size === right.size && left.mtimeNs === right.mtimeNs &&
    left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function exactKeys(value, keys, label) {
  if (!isRecord(value)) throw new Error(`${label} must be an object`)
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function requireSha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(`${label} must be lowercase 64-hex`)
  }
  return value
}

function sha256Bytes(value) {
  return createHash('sha256').update(value).digest('hex')
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function errorMessage(cause) {
  return cause instanceof Error ? cause.message : String(cause)
}

function emitTrace(trace, event) {
  try {
    trace(event)
  } catch {
    // Observability cannot outrank cleanup of a private runtime build root.
  }
}
