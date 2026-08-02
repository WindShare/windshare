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
import {
  createOwnedTraceJournal,
  requireCompleteOwnedTraceSnapshot,
} from './owned-trace-journal.mjs'

export const BROWSERGATE_RUNTIME_MANIFEST_ENV = 'WINDSHARE_BROWSERGATE_RUNTIME_MANIFEST'
export const PION_SERVER_EXECUTABLE_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE'

const RUNTIME_SCHEMA_VERSION = 4
const RUNTIME_MANIFEST_FILENAME = 'runtime-manifest.json'
const RUNTIME_DIRECTORY_PREFIX = 'windshare-browsergate-runtime-'
const RUNTIME_DIRECTORY_PATTERN = /^windshare-browsergate-runtime-[A-Za-z0-9]{6}$/u
const SUITES = Object.freeze(['main', 'pion'])
const SUPPORTED_PLATFORMS = Object.freeze(['darwin', 'linux', 'win32'])
const RUNTIME_BUILD_MAXIMUM_TRACE_EVENTS = 64
const RUNTIME_BUILD_MAXIMUM_TRACE_BYTES = 128 * 1024
const SAMPLE_DRIVER_RELATIVE_PATH = join(
  'web',
  'scripts',
  'browser-evidence',
  'sample-driver.ts',
)
const PLAYWRIGHT_RUNNER_RELATIVE_PATH = join(
  'web',
  'scripts',
  'browser-evidence',
  'playwright-owned-runner.mjs',
)
const PLAYWRIGHT_CLI_RELATIVE_PATH = join(
  'web',
  'node_modules',
  '@playwright',
  'test',
  'cli.js',
)
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
  'test-process-owner': Object.freeze({
    packagePath: './cmd/testprocessowner',
    filename: 'testprocessowner',
  }),
})
const SELF_CHECK_LINES = Object.freeze({
  'topology-materializer':
    '{"schemaVersion":1,"component":"browser-evidence-topology-resolution","outcome":"ready"}\n',
  'artifact-publisher':
    '{"schemaVersion":"windshare.artifact-publisher/v2","outcome":"ready"}\n',
  'pion-server':
    '{"schemaVersion":1,"component":"pion-browser-interop-server","outcome":"ready"}\n',
  'test-process-owner':
    '{"schema_version":"windshare.process-owner-self-check/v1","component":"testprocessowner","milestone":"self_check","outcome":"ready"}\n',
})
const OWNED_EXECUTION_KEYS = Object.freeze([
  'processEvidence',
  'treeEmpty',
  'cleanupOutcome',
  'inputEvidence',
  'ownershipEvidence',
  'stdout',
  'stderr',
  'traces',
  'runtimeCommandTraces',
])
const INPUT_EVIDENCE_KEYS = Object.freeze(['outcome', 'failureCode', 'failureMessage'])
const OWNERSHIP_EVIDENCE_KEYS = Object.freeze([
  'kind', 'backend', 'terminationReason', 'platform',
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
  preserveRuntimeRoot = false,
}) {
  const traceJournal = createOwnedTraceJournal({
    label: 'browsergate runtime build trace',
    maximumEvents: RUNTIME_BUILD_MAXIMUM_TRACE_EVENTS,
    maximumBytes: RUNTIME_BUILD_MAXIMUM_TRACE_BYTES,
  })
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
    ...(['win32', 'linux'].includes(runtimePlatform) ? ['test-process-owner'] : []),
    'topology-materializer',
    'artifact-publisher',
    ...(selectedSuites.includes('pion') ? ['pion-server'] : []),
  ]
  const startedAtMs = Date.now()
  try {
    traceJournal.append(Object.freeze({ milestone: 'runtime-build-started', context: { kinds } }))
    const artifacts = []
    const ownedExecutionTraces = []
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
      ownedExecutionTraces.push(Object.freeze({
        kind,
        phase: 'build',
        ...requireSuccessfulOwnedExecution(
          buildExecution,
          `runtime build ${kind}`,
          '',
          runtimePlatform,
        ),
      }))
      assertRuntimeArtifactPath(executablePath, `runtime ${kind}`)
      const artifact = Object.freeze({
        kind,
        path: executablePath,
      })
      const preflight = await executePreflight(Object.freeze({
        kind,
        executablePath,
        arguments: Object.freeze(['self-check']),
        cwd: spec.moduleDirectory === undefined ? root : join(root, spec.moduleDirectory),
        platform: runtimePlatform,
        availableArtifacts: Object.freeze([...artifacts, artifact]),
      }))
      ownedExecutionTraces.push(Object.freeze({
        kind,
        phase: 'preflight',
        ...requireSuccessfulOwnedExecution(
          preflight,
          `runtime preflight ${kind}`,
          SELF_CHECK_LINES[kind],
          runtimePlatform,
        ),
      }))
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
    traceJournal.append(Object.freeze({
      milestone: 'runtime-build-completed',
      context: {
        kinds,
        elapsedMs: Date.now() - startedAtMs,
        manifestPath,
      },
    }))
    traceJournal.finish()
    const traces = requireCompleteOwnedTraceSnapshot(
      traceJournal.view.snapshot(),
      'browsergate runtime build trace',
    )
    const runtime = openBrowsergateRuntime({
      manifestPath,
      ownsRuntimeRoot: !preserveRuntimeRoot,
      expectedPlatform: runtimePlatform,
    })
    return Object.freeze({
      ...runtime,
      traces,
      ownedExecutionTraces: Object.freeze(ownedExecutionTraces),
    })
  } catch (cause) {
    if (!traceJournal.view.snapshot().completed) {
      traceJournal.append(Object.freeze({
        milestone: 'runtime-build-failed',
        context: {
          elapsedMs: Date.now() - startedAtMs,
          failureCode: 'runtime-build-rejected',
        },
      }))
      traceJournal.finish()
    }
    const traces = traceJournal.view.snapshot()
    rmSync(runtimeRoot, { force: true, recursive: true })
    try {
      requireCompleteOwnedTraceSnapshot(traces, 'browsergate runtime build trace')
    } catch (traceFailure) {
      throw new AggregateError(
        [cause, traceFailure],
        'browsergate runtime build and lifecycle trace settlement failed',
      )
    }
    throw cause
  }
}

export function loadBrowsergateRuntime({
  manifestPath,
  expectedPlatform = process.platform,
}) {
  return openBrowsergateRuntime({
    manifestPath,
    ownsRuntimeRoot: false,
    expectedPlatform,
  })
}

export function disposeBrowsergateRuntime({
  manifestPath,
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
  const runtime = openBrowsergateRuntime({
    manifestPath: canonicalManifestPath,
    ownsRuntimeRoot: true,
    expectedPlatform,
  })
  runtime.dispose()
  return Object.freeze({ runtimeRoot })
}

function openBrowsergateRuntime({
  manifestPath,
  ownsRuntimeRoot,
  expectedPlatform,
}) {
  const canonicalManifestPath = requireCanonicalAbsolutePath(
    manifestPath,
    'browsergate runtime manifest',
  )
  const runtimePlatform = requirePlatform(expectedPlatform, 'expected runtime platform')
  const manifestSnapshot = openRegularFileSnapshot(
    canonicalManifestPath,
    1_048_576,
    'browsergate runtime manifest',
  )
  const encoded = manifestSnapshot.bytes.toString('utf8')
  let manifest
  try {
    manifest = parseRuntimeManifest(
      encoded,
      dirname(canonicalManifestPath),
      runtimePlatform,
    )
  } catch (cause) {
    closeSync(manifestSnapshot.descriptor)
    throw cause
  }
  const descriptor = manifestSnapshot.descriptor
  const heldRevision = manifestSnapshot.metadata
  let disposed = false

  function assertAuthorityLive() {
    if (disposed) throw new Error('browsergate runtime is disposed')
    const named = lstatSync(canonicalManifestPath, { bigint: true })
    const opened = fstatSync(descriptor, { bigint: true })
    if (
      !sameIdentity(heldRevision, opened) || !sameIdentity(opened, named) ||
      !sameRevision(heldRevision, opened) || !sameRevision(opened, named)
    ) {
      throw new Error('browsergate runtime manifest changed while held')
    }
  }

  return Object.freeze({
    manifestPath: canonicalManifestPath,
    manifest,
    artifactCapability(kind) {
      assertAuthorityLive()
      const artifact = manifest.artifacts.find((candidate) => candidate.kind === kind)
      if (artifact === undefined) throw new Error(`browsergate runtime lacks ${kind}`)
      assertRuntimeArtifactPath(artifact.path, `runtime ${kind}`)
      // Manifest metadata authenticates which artifact was selected. Consumers
      // receive only the path capability they need, keeping kind metadata from
      // accidentally becoming part of an unrelated launch protocol.
      return Object.freeze({ path: artifact.path })
    },
    environmentForSuite(suite) {
      requireSuite(suite)
      assertAuthorityLive()
      const environment = {
        [BROWSERGATE_RUNTIME_MANIFEST_ENV]: canonicalManifestPath,
      }
      if (suite === 'pion') {
        const pion = this.artifactCapability('pion-server')
        environment[PION_SERVER_EXECUTABLE_ENV] = pion.path
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
      ['kind', 'path'],
      `runtime artifact ${index}`,
    )
    if (!Object.hasOwn(ARTIFACT_SPECS, artifact.kind) || kinds.has(artifact.kind)) {
      throw new Error('runtime artifact kind is invalid or repeated')
    }
    const path = requireCanonicalAbsolutePath(artifact.path, `runtime artifact ${index}`)
    if (dirname(path) !== runtimeRoot || paths.has(path)) {
      throw new Error('runtime artifact path escapes or repeats its private root')
    }
    kinds.add(artifact.kind)
    paths.add(path)
    return Object.freeze({ ...artifact, path })
  })
  const expectedKinds = [
    ...(['win32', 'linux'].includes(platform) ? ['test-process-owner'] : []),
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
  const playwrightRunnerPath = join(repositoryRoot, PLAYWRIGHT_RUNNER_RELATIVE_PATH)
  const playwrightCliPath = join(repositoryRoot, PLAYWRIGHT_CLI_RELATIVE_PATH)
  return Object.freeze({
    repositoryRoot,
    node: requireRuntimeInputPath(nodePath, 'sample command Node executable'),
    driverSource: requireRuntimeInputPath(driverSourcePath, 'sample command driver source'),
    playwrightRunner: requireRuntimeInputPath(
      playwrightRunnerPath,
      'sample command owned Playwright runner',
    ),
    playwrightCli: requireRuntimeInputPath(playwrightCliPath, 'sample command Playwright CLI'),
    environment: sampleProcessEnvironment({}, {}, inheritedEnvironment, platform),
  })
}

function parseSampleCommandCapability(value, platform) {
  exactKeys(value, [
    'repositoryRoot', 'node', 'driverSource', 'playwrightRunner', 'playwrightCli', 'environment',
  ], 'sample command runtime capability')
  const repositoryRoot = requireCanonicalAbsolutePath(
    value.repositoryRoot,
    'sample command repository root',
  )
  const node = requireRuntimeInputPath(value.node, 'sample command Node executable')
  const driverSource = requireRuntimeInputPath(value.driverSource, 'sample command driver source')
  const playwrightCli = requireRuntimeInputPath(value.playwrightCli, 'sample command Playwright CLI')
  const playwrightRunner = requireRuntimeInputPath(
    value.playwrightRunner,
    'sample command owned Playwright runner',
  )
  if (driverSource !== join(repositoryRoot, SAMPLE_DRIVER_RELATIVE_PATH)) {
    throw new Error('sample command driver source escapes its repository capability')
  }
  if (playwrightCli !== join(repositoryRoot, PLAYWRIGHT_CLI_RELATIVE_PATH)) {
    throw new Error('sample command Playwright CLI escapes its repository capability')
  }
  if (playwrightRunner !== join(repositoryRoot, PLAYWRIGHT_RUNNER_RELATIVE_PATH)) {
    throw new Error('sample command owned Playwright runner escapes its repository capability')
  }
  const environment = canonicalSampleEnvironment(value.environment, platform)
  return Object.freeze({
    repositoryRoot, node, driverSource, playwrightRunner, playwrightCli, environment,
  })
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
  for (const name of ['node', 'driverSource', 'playwrightRunner', 'playwrightCli']) {
    assertRuntimeArtifactPath(capability[name], `sample command ${name}`)
  }
}

function requireRuntimeInputPath(path, label) {
  const canonical = requireCanonicalAbsolutePath(path, label)
  assertRuntimeArtifactPath(canonical, label)
  return canonical
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
    execution.treeEmpty !== true || execution.cleanupOutcome !== 'completed' ||
    !isRecord(execution.processEvidence) || execution.processEvidence.terminal !== 'exited' ||
    execution.processEvidence.exitCode !== 0
  ) throw new Error(`${label} did not prove a successful empty process tree`)
  requireSuccessfulInputEvidence(execution.inputEvidence, label)
  requireSuccessfulOwnershipEvidence(execution.ownershipEvidence, platform, label)
  if (execution.stdout !== expectedStdout) throw new Error(`${label} emitted unexpected stdout`)
  if (execution.stderr !== '') throw new Error(`${label} emitted unexpected stderr`)
  return Object.freeze({
    traces: requireCompleteOwnedTraceSnapshot(execution.traces, `${label} operation trace`),
    runtimeCommandTraces: requireCompleteOwnedTraceSnapshot(
      execution.runtimeCommandTraces,
      `${label} runtime command trace`,
    ),
  })
}

function requireSuccessfulInputEvidence(input, label) {
  exactKeys(input, INPUT_EVIDENCE_KEYS, `${label} input evidence`)
  if (input.outcome !== 'not_requested' || input.failureCode !== '' || input.failureMessage !== '') {
    throw new Error(`${label} input evidence is not a successful no-input execution`)
  }
}

function requireSuccessfulOwnershipEvidence(ownership, platform, label) {
  exactKeys(ownership, OWNERSHIP_EVIDENCE_KEYS, `${label} ownership evidence`)
  const expectedBackend = platform === 'win32' ? 'windows_job' : 'linux_subreaper'
  if (
    ownership.kind !== 'test-process-owner' || ownership.backend !== expectedBackend ||
    ownership.terminationReason !== 'natural' || !isRecord(ownership.platform) ||
    ownership.platform.kind !== expectedBackend
  ) throw new Error(`${label} process ownership evidence is not successful`)
}

function assertRuntimeArtifactPath(path, label) {
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error(`${label} is not a non-empty regular file`)
  }
}

function openRegularFileSnapshot(path, maximumBytes, label) {
  const namedBefore = lstatSync(path, { bigint: true })
  if (
    !namedBefore.isFile() || namedBefore.isSymbolicLink() ||
    namedBefore.size < 0n || namedBefore.size > BigInt(maximumBytes)
  ) throw new Error(`${label} is not a bounded regular file`)
  const descriptor = openSync(path, 'r')
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    if (
      !openedBefore.isFile() || !sameIdentity(namedBefore, openedBefore) ||
      !sameRevision(namedBefore, openedBefore)
    ) throw new Error(`${label} changed before it could be held`)
    const bytes = readFileSync(descriptor)
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(path, { bigint: true })
    if (
      bytes.byteLength !== Number(openedAfter.size) ||
      !sameIdentity(openedBefore, openedAfter) || !sameIdentity(openedAfter, namedAfter) ||
      !sameRevision(openedBefore, openedAfter) || !sameRevision(openedAfter, namedAfter)
    ) throw new Error(`${label} changed while read`)
    return Object.freeze({
      bytes,
      descriptor,
      metadata: openedAfter,
    })
  } catch (cause) {
    closeSync(descriptor)
    throw cause
  }
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

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
