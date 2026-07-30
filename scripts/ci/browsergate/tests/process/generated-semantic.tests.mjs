import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { assertPinnedNodeVersion, readPinnedNodeVersion } from '../../../node-version.mjs'
import {
  GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
  GENERATED_SEMANTIC_DIGEST,
  GENERATED_SEMANTIC_EXPORTS,
} from '../../generated-semantic/build/artifact-policy.mjs'
import { GENERATED_SEMANTIC_PATHS } from '../../generated-semantic/build/cli.mjs'
import { GENERATED_SEMANTIC_FILENAME } from '../../generated-semantic/build/config.mjs'
import { createGeneratedSemanticEnvironment } from '../../generated-semantic/build/environment.mjs'
import {
  encodeGeneratedSemanticResult,
  parseGeneratedSemanticResultRecord,
} from '../../generated-semantic/build/result-contract.mjs'
import {
  GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS,
} from '../../generated-semantic/build/tool-authorization.mjs'
import {
  createGeneratedSemanticSpawnEnvironment,
} from '../../generated-semantic/build/worker-launcher.mjs'
import {
  createGeneratedSemanticWorkerRequest,
  encodeGeneratedSemanticWorkerRequest,
  encodeGeneratedSemanticWorkerResult,
  parseGeneratedSemanticWorkerResult,
} from '../../generated-semantic/build/worker-protocol.mjs'
import {
  createHostileNodeOptions,
  HOSTILE_PRELOAD_PATH,
  materializeHostileGeneratedSemanticProject,
  readExactlyOneHostilePreloadRecord,
  runStrictBoundedProcess,
} from '../../testsupport/generated-semantic-hostile/process.fixture.mjs'

const BUILD_PROCESS_DEADLINE_MILLISECONDS = 120_000
const GIT_STATUS_DEADLINE_MILLISECONDS = 30_000
const MAXIMUM_BUILD_STDOUT_BYTES = 8 * 1_024 * 1_024
const MAXIMUM_BUILD_STDERR_BYTES = 256 * 1_024
const MAXIMUM_GIT_STATUS_BYTES = 16 * 1_024 * 1_024
const TEMPORARY_ROOT_PREFIX = 'windshare-generated-semantic-process-'
const HOSTED_CI_ENVIRONMENT_VALUE = 'true'
const VERIFIER_PATH = join(GENERATED_SEMANTIC_PATHS.generatedRoot, 'verify-generated.mjs')
const EXPECTED_TOOLS = Object.freeze({
  node: readPinnedNodeVersion(GENERATED_SEMANTIC_PATHS.repositoryRoot),
  ...GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS,
})

assertPinnedNodeVersion({
  actualVersion: process.version,
  pinnedVersion: EXPECTED_TOOLS.node,
})

const temporaryRoot = await mkdtemp(join(tmpdir(), TEMPORARY_ROOT_PREFIX))
try {
  const hostileRoot = await materializeHostileGeneratedSemanticProject(
    join(temporaryRoot, 'hostile-project'),
  )
  const directWorker = await runHostileWorker({ hostileRoot, temporaryRoot })

  const writeExecution = await runVerifier({
    arguments: ['--write'],
    hostileRoot,
    recordPath: join(temporaryRoot, 'write-preload.jsonl'),
    label: 'generated semantic write verifier',
  })
  const writeResult = parseCanonicalVerifierResult(writeExecution.stdout)
  const readOnlyBaseline = await snapshotReadOnlyState()
  assertSuccessfulVerifierResult(writeResult, 'write', readOnlyBaseline.artifactBytes)
  assert.deepEqual(directWorker.tools, GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS)
  assert.deepEqual(directWorker.exports, writeResult.artifact.exports)
  assert.deepEqual(directWorker.externalImports, writeResult.artifact.externalImports)

  const verifyResults = []
  for (const invocation of ['verify-one', 'verify-two']) {
    const execution = await runVerifier({
      arguments: [],
      hostileRoot,
      recordPath: join(temporaryRoot, `${invocation}-preload.jsonl`),
      label: `generated semantic ${invocation} verifier`,
    })
    const result = parseCanonicalVerifierResult(execution.stdout)
    assertSuccessfulVerifierResult(result, 'verify', readOnlyBaseline.artifactBytes)
    assert.deepEqual(result.tools, writeResult.tools, `${invocation} tool versions changed`)
    assert.deepEqual(result.artifact, writeResult.artifact, `${invocation} artifact summary changed`)
    assertReadOnlyStateEqual(
      await snapshotReadOnlyState(),
      readOnlyBaseline,
      invocation,
    )
    verifyResults.push(result)
  }
  assert.equal(verifyResults.length, 2)

  process.stdout.write('generated semantic hostile real-process regression: PASS\n')
} finally {
  await rm(temporaryRoot, { recursive: true, force: true })
}

async function runHostileWorker({ hostileRoot, temporaryRoot }) {
  const request = createGeneratedSemanticWorkerRequest({
    webRoot: hostileRoot,
    semanticEntry: GENERATED_SEMANTIC_PATHS.semanticEntry,
    isolatedCacheRoot: join(temporaryRoot, 'hostile-worker-cache'),
    viteModulePath: GENERATED_SEMANTIC_PATHS.viteModulePath,
  })
  const workerEnvironment = createGeneratedSemanticSpawnEnvironment(
    createGeneratedSemanticEnvironment({
      platform: process.platform,
      temporaryRoot,
      inheritedEnvironment: {
        ...process.env,
        NODE_OPTIONS: `--import=${HOSTILE_PRELOAD_PATH}`,
        VITE_GENERATED_SEMANTIC_HOSTILE: 'must-not-reach-worker',
      },
    }),
  )
  const execution = await runStrictBoundedProcess({
    executable: process.execPath,
    arguments: [
      GENERATED_SEMANTIC_PATHS.workerPath,
      encodeGeneratedSemanticWorkerRequest(request),
    ],
    workingDirectory: hostileRoot,
    environment: workerEnvironment,
    deadlineMilliseconds: BUILD_PROCESS_DEADLINE_MILLISECONDS,
    maximumStdoutBytes: MAXIMUM_BUILD_STDOUT_BYTES,
    maximumStderrBytes: MAXIMUM_BUILD_STDERR_BYTES,
    label: 'hostile generated semantic worker',
  })
  const result = parseGeneratedSemanticWorkerResult(execution.stdout)
  assert.equal(execution.stdout, encodeGeneratedSemanticWorkerResult(result))
  assert.equal(result.outcome, 'built')
  assert.deepEqual(result.tools, GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS)
  assert.equal(result.builds.length, 1)
  assert.equal(result.builds[0].outputs.length, 1)
  const chunk = result.builds[0].outputs[0]
  assert.equal(chunk.type, 'chunk')
  assert.equal(chunk.fileName, GENERATED_SEMANTIC_FILENAME)
  assert.equal(chunk.isEntry, true)
  assert.equal(chunk.isDynamicEntry, false)
  assert.equal(chunk.hasSourceMap, false)
  assert.deepEqual(chunk.dynamicImports, [])
  assert(chunk.code.includes(GENERATED_SEMANTIC_DIGEST))
  const exports = canonicalStrings(chunk.exports)
  const externalImports = canonicalStrings(chunk.imports)
  assert.deepEqual(exports, canonicalStrings(GENERATED_SEMANTIC_EXPORTS))
  assert(externalImports.every((specifier) =>
    GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS.includes(specifier)))
  return Object.freeze({ tools: result.tools, exports, externalImports })
}

async function runVerifier({ arguments: arguments_, hostileRoot, recordPath, label }) {
  const execution = await runStrictBoundedProcess({
    executable: process.execPath,
    arguments: [VERIFIER_PATH, ...arguments_],
    workingDirectory: hostileRoot,
    environment: createHostileVerifierEnvironment(recordPath),
    deadlineMilliseconds: BUILD_PROCESS_DEADLINE_MILLISECONDS,
    maximumStdoutBytes: MAXIMUM_BUILD_STDOUT_BYTES,
    maximumStderrBytes: MAXIMUM_BUILD_STDERR_BYTES,
    label,
  })
  const preload = await readExactlyOneHostilePreloadRecord(recordPath)
  assert.equal(preload.pid, execution.pid, `${label} preload did not run in its parent process`)
  assert.equal(preload.entryPoint, resolve(VERIFIER_PATH), `${label} preload reached a worker`)
  assert.equal(preload.workingDirectory, resolve(hostileRoot), `${label} changed ambient cwd`)
  return execution
}

function createHostileVerifierEnvironment(recordPath) {
  const replacements = Object.freeze({
    NODE_OPTIONS: createHostileNodeOptions(recordPath),
    NODE_V8_COVERAGE: undefined,
    VITE_DEPRECATION_TRACE: '1',
    VITE_GENERATED_SEMANTIC_HOSTILE: 'must-not-reach-worker',
  })
  const environment = Object.create(null)
  for (const [name, value] of Object.entries(process.env)) {
    if (!Object.hasOwn(replacements, name.toUpperCase())) environment[name] = value
  }
  return Object.assign(environment, replacements)
}

function parseCanonicalVerifierResult(stdout) {
  const result = parseGeneratedSemanticResultRecord(stdout)
  assert.equal(stdout, encodeGeneratedSemanticResult(result))
  return result
}

function assertSuccessfulVerifierResult(result, expectedMode, artifactBytes) {
  assert.equal(result.mode, expectedMode)
  assert(
    expectedMode === 'write'
      ? ['current', 'published'].includes(result.outcome)
      : result.outcome === 'current',
  )
  if (expectedMode === 'write' && process.env.CI === HOSTED_CI_ENVIRONMENT_VALUE) {
    // Local regeneration may intentionally publish a first artifact. A hosted
    // checkout must instead prove this OS reproduced the committed bytes.
    assert.equal(result.outcome, 'current')
  }
  assert.deepEqual(result.failures, [])
  assert.deepEqual(result.tools, EXPECTED_TOOLS)
  assert.equal(result.artifact.fileName, GENERATED_SEMANTIC_FILENAME)
  assert.equal(result.artifact.byteLength, artifactBytes.byteLength)
  assert.equal(
    result.artifact.sha256,
    createHash('sha256').update(artifactBytes).digest('hex'),
  )
  assert.equal(result.artifact.semanticDigest, GENERATED_SEMANTIC_DIGEST)
  assert.deepEqual(result.artifact.exports, [...GENERATED_SEMANTIC_EXPORTS])
  assert.deepEqual(
    result.artifact.externalImports,
    canonicalStrings(result.artifact.externalImports),
  )
  assert(result.artifact.externalImports.every((specifier) =>
    GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS.includes(specifier)))
}

async function snapshotReadOnlyState() {
  const [artifactBytes, artifactStat, worktreeStatus] = await Promise.all([
    readFile(GENERATED_SEMANTIC_PATHS.committedPath),
    stat(GENERATED_SEMANTIC_PATHS.committedPath, { bigint: true }),
    captureWorktreeStatus(),
  ])
  return Object.freeze({
    artifactBytes,
    artifactIdentity: Object.freeze({
      device: artifactStat.dev,
      inode: artifactStat.ino,
      mode: artifactStat.mode,
      links: artifactStat.nlink,
      size: artifactStat.size,
      modifiedNanoseconds: artifactStat.mtimeNs,
      changedNanoseconds: artifactStat.ctimeNs,
    }),
    worktreeStatus,
  })
}

async function captureWorktreeStatus() {
  const execution = await runStrictBoundedProcess({
    executable: 'git',
    arguments: ['--no-optional-locks', 'status', '--porcelain=v1', '-z', '--untracked-files=all'],
    workingDirectory: GENERATED_SEMANTIC_PATHS.repositoryRoot,
    environment: { ...process.env, GIT_OPTIONAL_LOCKS: '0' },
    deadlineMilliseconds: GIT_STATUS_DEADLINE_MILLISECONDS,
    maximumStdoutBytes: MAXIMUM_GIT_STATUS_BYTES,
    maximumStderrBytes: MAXIMUM_BUILD_STDERR_BYTES,
    label: 'generated semantic worktree snapshot',
  })
  return execution.stdoutBytes
}

function assertReadOnlyStateEqual(actual, expected, label) {
  assert.deepEqual(actual.worktreeStatus, expected.worktreeStatus, `${label} changed tracked status`)
  assert.deepEqual(actual.artifactBytes, expected.artifactBytes, `${label} changed artifact bytes`)
  assert.deepEqual(
    actual.artifactIdentity,
    expected.artifactIdentity,
    `${label} rewrote the artifact even though its bytes compare equal`,
  )
}

function canonicalStrings(values) {
  assert(Array.isArray(values))
  assert(values.every((value) => typeof value === 'string'))
  const canonical = [...values].sort(compareOrdinal)
  assert.equal(new Set(canonical).size, canonical.length)
  return canonical
}

function compareOrdinal(left, right) {
  return left < right ? -1 : left > right ? 1 : 0
}
