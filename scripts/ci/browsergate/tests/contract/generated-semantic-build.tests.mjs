import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { readFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'

import { parseGeneratedSemanticArguments } from '../../generated-semantic/build/arguments.mjs'
import {
  GENERATED_SEMANTIC_BUILD_ALLOWLIST,
  GENERATED_SEMANTIC_DIGEST,
  GENERATED_SEMANTIC_EXPORTS,
  GENERATED_SEMANTIC_ROOT_ALLOWLIST,
  generatedSemanticArtifactSummary,
  validateGeneratedSemanticArtifact,
} from '../../generated-semantic/build/artifact-policy.mjs'
import {
  GENERATED_SEMANTIC_PATHS,
  executeGeneratedSemanticCli,
  runGeneratedSemanticCli,
} from '../../generated-semantic/build/cli.mjs'
import {
  createGeneratedSemanticBuildConfig,
  GENERATED_SEMANTIC_FILENAME,
} from '../../generated-semantic/build/config.mjs'
import { createGeneratedSemanticEnvironment } from '../../generated-semantic/build/environment.mjs'
import { createGeneratedSemanticFailure } from '../../generated-semantic/build/failure.mjs'
import { publishGeneratedSemanticArtifact } from '../../generated-semantic/build/publisher.mjs'
import {
  createGeneratedSemanticResult,
  encodeGeneratedSemanticResult,
  parseGeneratedSemanticResultRecord,
} from '../../generated-semantic/build/result-contract.mjs'
import {
  assertGeneratedSemanticToolVersions,
  GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS,
  parseGeneratedSemanticToolAuthorization,
} from '../../generated-semantic/build/tool-authorization.mjs'
import {
  assertGeneratedSemanticParentProcessIsolation,
  createGeneratedSemanticSpawnEnvironment,
  createGeneratedSemanticWorkerProcessSpec,
  launchGeneratedSemanticWorker,
  requireSuccessfulGeneratedSemanticWorker,
} from '../../generated-semantic/build/worker-launcher.mjs'
import {
  createGeneratedSemanticWorkerBuiltResult,
  createGeneratedSemanticWorkerRequest,
  encodeGeneratedSemanticWorkerRequest,
  encodeGeneratedSemanticWorkerResult,
  parseGeneratedSemanticWorkerResult,
} from '../../generated-semantic/build/worker-protocol.mjs'
import {
  executeGeneratedSemanticBuildWorker,
  runGeneratedSemanticWorkerMain,
} from '../../generated-semantic/build/worker.mjs'

const PINNED_NODE_VERSION = '24.16.0'
const TOOL_VERSIONS = GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS
const TEST_ROOT = resolve('generated-semantic-contract-root')
const WEB_ROOT = join(TEST_ROOT, 'web')
const SEMANTIC_ENTRY = join(WEB_ROOT, 'scripts', 'final-semantic-reducer.ts')
const CACHE_ROOT = join(TEST_ROOT, 'cache')
const VITE_MODULE_PATH = join(WEB_ROOT, 'node_modules', 'vite', 'index.js')
const WORKER_PATH = join(TEST_ROOT, 'worker.mjs')
const NODE_EXECUTABLE = join(TEST_ROOT, 'node.exe')
const GENERATED_CODE = [
  `const semanticDigest = '${GENERATED_SEMANTIC_DIGEST}'`,
  'function evaluateFinalBrowserSample() {}',
  'function parseFinalGuardUploadManifest() {}',
  'export { evaluateFinalBrowserSample, parseFinalGuardUploadManifest }',
  '',
].join('\n')

const semanticEntrySource = readFileSync(GENERATED_SEMANTIC_PATHS.semanticEntry, 'utf8')
assert.match(
  semanticEntrySource,
  /from '\.\/artifact\/sealed-suite\/manifest-codec\.ts'/u,
)
assert.match(
  semanticEntrySource,
  /from '\.\/artifact\/sealed-suite\/contract\.ts'/u,
)
assert.doesNotMatch(semanticEntrySource, /from '\.\/artifact\/sealed-suite\.ts'/u)

assert.deepEqual(parseGeneratedSemanticArguments([]), { ok: true, mode: 'verify' })
assert.deepEqual(parseGeneratedSemanticArguments(['--write']), { ok: true, mode: 'write' })
for (const invalidArguments of [['--unknown'], ['--write', '--write'], [1], null]) {
  const parsed = parseGeneratedSemanticArguments(invalidArguments)
  assert.equal(parsed.ok, false)
  assert.equal(parsed.failure.kind, 'usage')
  assert.equal(parsed.failure.code, 'invalid-arguments')
}

const expectedConfig = {
  root: WEB_ROOT,
  configFile: false,
  envDir: false,
  mode: 'production',
  publicDir: false,
  cacheDir: CACHE_ROOT,
  clearScreen: false,
  logLevel: 'silent',
  build: {
    target: 'es2023',
    ssr: SEMANTIC_ENTRY,
    write: false,
    minify: false,
    sourcemap: false,
    rolldownOptions: {
      tsconfig: false,
      experimental: { attachDebugInfo: 'none' },
      output: { format: 'es', entryFileNames: GENERATED_SEMANTIC_FILENAME },
    },
  },
}
assert.deepEqual(createGeneratedSemanticBuildConfig({
  webRoot: WEB_ROOT,
  semanticEntry: SEMANTIC_ENTRY,
  isolatedCacheRoot: CACHE_ROOT,
}), expectedConfig)

const hostileEnvironment = Object.assign(Object.create({ INHERITED_SECRET: 'leak' }), {
  PATH: 'host-path',
  NODE_OPTIONS: '--inspect',
  NODE_PATH: 'host-modules',
  NODE_V8_COVERAGE: 'host-coverage',
  VITE_HOSTILE_FLAG: 'host-vite',
})
const isolatedEnvironment = createGeneratedSemanticEnvironment({
  platform: 'linux',
  temporaryRoot: TEST_ROOT,
  inheritedEnvironment: hostileEnvironment,
})
assert.deepEqual(isolatedEnvironment, {
  NODE_ENV: 'production',
  TZ: 'UTC',
  LANG: 'C',
  LC_ALL: 'C',
  TMPDIR: TEST_ROOT,
  TMP: TEST_ROOT,
  TEMP: TEST_ROOT,
})
for (const hostileName of Object.keys(hostileEnvironment).concat('INHERITED_SECRET')) {
  assert.equal(Object.hasOwn(isolatedEnvironment, hostileName), false, hostileName)
}
assert(Object.isFrozen(isolatedEnvironment))
const windowsEnvironment = createGeneratedSemanticEnvironment({
  platform: 'win32',
  temporaryRoot: TEST_ROOT,
  inheritedEnvironment: { systemroot: 'C:\\Windows', PATH: 'host-path' },
})
assert.equal(windowsEnvironment.SystemRoot, 'C:\\Windows')
assert.equal(windowsEnvironment.WINDIR, 'C:\\Windows')
assert.equal(Object.hasOwn(windowsEnvironment, 'PATH'), false)
assert.throws(() => createGeneratedSemanticEnvironment({
  platform: 'win32',
  temporaryRoot: TEST_ROOT,
  inheritedEnvironment: { SystemRoot: 'C:\\Windows', WINDIR: 'D:\\Windows' },
}), /conflicting SystemRoot/u)
const accessorEnvironment = {}
Object.defineProperty(accessorEnvironment, 'SystemRoot', {
  enumerable: true,
  get() {
    throw new Error('host accessor must not execute')
  },
})
assert.throws(() => createGeneratedSemanticEnvironment({
  platform: 'win32',
  temporaryRoot: TEST_ROOT,
  inheritedEnvironment: accessorEnvironment,
}), /must be a data property/u)

const spawnEnvironment = createGeneratedSemanticSpawnEnvironment(isolatedEnvironment)
assert.equal(Object.getPrototypeOf(spawnEnvironment), null)
assert.equal(Object.isFrozen(spawnEnvironment), false)
assert.equal(Object.hasOwn(spawnEnvironment, 'NODE_V8_COVERAGE'), true)
assert.equal(spawnEnvironment.NODE_V8_COVERAGE, undefined)
spawnEnvironment.NODE_V8_COVERAGE = undefined
assert.doesNotThrow(() => assertGeneratedSemanticParentProcessIsolation(undefined))
assert.throws(
  () => assertGeneratedSemanticParentProcessIsolation({ has: () => true }),
  (error) => {
    assert.equal(error.failure.code, 'parent-permission-model-active')
    return true
  },
)

const workerRequest = createGeneratedSemanticWorkerRequest({
  webRoot: WEB_ROOT,
  semanticEntry: SEMANTIC_ENTRY,
  isolatedCacheRoot: CACHE_ROOT,
  viteModulePath: VITE_MODULE_PATH,
})
assert.throws(
  () => createGeneratedSemanticWorkerRequest({
    webRoot: 'relative',
    semanticEntry: SEMANTIC_ENTRY,
    isolatedCacheRoot: CACHE_ROOT,
    viteModulePath: VITE_MODULE_PATH,
  }),
  /absolute and canonical/u,
)

let importedVitePath = null
let observedConfig = null
const workerBuild = await executeGeneratedSemanticBuildWorker(workerRequest, {
  importVite: async (path) => {
    importedVitePath = path
    return {
      ...TOOL_VERSIONS,
      version: TOOL_VERSIONS.vite,
      rolldownVersion: TOOL_VERSIONS.rolldown,
      build: async (config) => {
        observedConfig = config
        return { output: [rawGeneratedChunk()] }
      },
    }
  },
})
assert.equal(importedVitePath, VITE_MODULE_PATH)
assert.deepEqual(observedConfig, expectedConfig)
assert.equal(workerBuild.outcome, 'built')
assert.deepEqual(workerBuild.tools, TOOL_VERSIONS)
assert.deepEqual(workerBuild.builds, validBuilds())

let invalidWorkerImportedVite = false
const invalidWorker = await runGeneratedSemanticWorkerMain(['{}'], {
  importVite: async () => {
    invalidWorkerImportedVite = true
    throw new Error('must not import Vite')
  },
})
assert.equal(invalidWorker.exitCode, 1)
assert.equal(invalidWorkerImportedVite, false)
assert.equal(parseGeneratedSemanticWorkerResult(invalidWorker.record).failure.code, 'worker-request-invalid')

for (const hostileCause of [errorWithUnreadableMessage(), hostileProxyCause()]) {
  const hostileWorker = await runGeneratedSemanticWorkerMain([
    encodeGeneratedSemanticWorkerRequest(workerRequest),
  ], {
    importVite: async () => { throw hostileCause },
  })
  assert.equal(hostileWorker.exitCode, 1)
  const hostileWorkerResult = parseGeneratedSemanticWorkerResult(hostileWorker.record)
  assert.equal(hostileWorkerResult.failure.code, 'vite-build-failed')
  assert.equal(hostileWorkerResult.failure.message, 'generated semantic Vite build failed')
}

const workerRecord = encodeGeneratedSemanticWorkerResult(createGeneratedSemanticWorkerBuiltResult({
  tools: TOOL_VERSIONS,
  builds: validBuilds(),
}))
assert.equal(parseGeneratedSemanticWorkerResult(workerRecord).outcome, 'built')
for (const invalidRecord of [
  workerRecord.slice(0, -1),
  workerRecord + workerRecord,
  `${workerRecord} `,
  '{]\n',
]) assert.throws(() => parseGeneratedSemanticWorkerResult(invalidRecord))

const processSpec = createGeneratedSemanticWorkerProcessSpec({
  nodeExecutable: NODE_EXECUTABLE,
  workerPath: WORKER_PATH,
  request: workerRequest,
  environment: isolatedEnvironment,
  workingDirectory: TEST_ROOT,
})
assert.equal(processSpec.executable, NODE_EXECUTABLE)
assert.equal(Object.hasOwn(processSpec, 'execArgv'), false)
assert.equal(processSpec.arguments[0], WORKER_PATH)
assert.equal(processSpec.arguments.length, 2)
assert.deepEqual(processSpec.environment, isolatedEnvironment)

let spawnCalls = 0
const workingDirectoryBeforeLaunch = process.cwd()
const launched = await launchGeneratedSemanticWorker({
  nodeExecutable: NODE_EXECUTABLE,
  workerPath: WORKER_PATH,
  request: workerRequest,
  environment: isolatedEnvironment,
  workingDirectory: TEST_ROOT,
}, {
  spawnProcess(executable, arguments_, options) {
    spawnCalls += 1
    assert.equal(executable, NODE_EXECUTABLE)
    assert.deepEqual(arguments_, processSpec.arguments)
    assert.equal(options.cwd, TEST_ROOT)
    assert.equal(options.shell, false)
    assert.deepEqual(options.stdio, ['ignore', 'pipe', 'pipe'])
    assert.equal(Object.getPrototypeOf(options.env), null)
    assert.equal(Object.hasOwn(options.env, 'NODE_V8_COVERAGE'), true)
    assert.equal(options.env.NODE_V8_COVERAGE, undefined)
    options.env.NODE_V8_COVERAGE = undefined
    const child = new EventEmitter()
    child.stdout = new EventEmitter()
    child.stderr = new EventEmitter()
    child.kill = () => {}
    queueMicrotask(() => {
      child.stdout.emit('data', Buffer.from(workerRecord))
      child.emit('close', 0, null)
    })
    return child
  },
})
assert.equal(spawnCalls, 1)
assert.equal(process.cwd(), workingDirectoryBeforeLaunch)
assert.equal(requireSuccessfulGeneratedSemanticWorker(launched).outcome, 'built')

await assert.rejects(launchGeneratedSemanticWorker({
  nodeExecutable: NODE_EXECUTABLE,
  workerPath: WORKER_PATH,
  request: workerRequest,
  environment: isolatedEnvironment,
  workingDirectory: TEST_ROOT,
}, {
  spawnProcess() {
    const child = new EventEmitter()
    child.stdout = new EventEmitter()
    child.stderr = new EventEmitter()
    child.kill = () => {}
    queueMicrotask(() => {
      child.stdout.emit('data', Buffer.from([0xff]))
      child.emit('close', 0, null)
    })
    return child
  },
}), (error) => {
  assert.equal(error.failure.code, 'worker-stdout-invalid-utf8')
  return true
})

assert.deepEqual(TOOL_VERSIONS, { vite: '8.1.3', rolldown: '1.1.4' })
const toolLockSource = readFileSync(GENERATED_SEMANTIC_PATHS.toolLockPath, 'utf8')
assert.deepEqual(
  parseGeneratedSemanticToolAuthorization(toolLockSource),
  TOOL_VERSIONS,
)
assert.throws(
  () => parseGeneratedSemanticToolAuthorization(`${toolLockSource}\ndefinitely: [invalid\n`),
  /invalid YAML/u,
)
assert.throws(
  () => parseGeneratedSemanticToolAuthorization(toolLockSource.replace(
    "lockfileVersion: '9.0'",
    "lockfileVersion: '9.0'\nlockfileVersion: '9.0'",
  )),
  /invalid YAML/u,
)
assert.throws(
  () => parseGeneratedSemanticToolAuthorization(
    `${toolLockSource}\nhostileAnchor: &hostile {}\nhostileAlias: *hostile\n`,
  ),
  /cannot be resolved safely/u,
)
const mismatchedViteSnapshot = toolLockSource.replace(
  '  vite@8.1.3(@types/node@24.13.2)(yaml@2.9.0):\n',
  '  vite@8.1.3(@types/node@0.0.0)(yaml@2.9.0):\n',
)
assert.notEqual(mismatchedViteSnapshot, toolLockSource)
assert.throws(
  () => parseGeneratedSemanticToolAuthorization(mismatchedViteSnapshot),
  /Vite snapshot resolution is missing/u,
)
const unmatchedRolldownReference = toolLockSource.replace(
  '      rolldown: 1.1.4\n',
  '      rolldown: 1.1.4(hostile@1.0.0)\n',
)
assert.notEqual(unmatchedRolldownReference, toolLockSource)
assert.throws(
  () => parseGeneratedSemanticToolAuthorization(unmatchedRolldownReference),
  /Rolldown snapshot resolution is missing/u,
)
assert.throws(
  () => assertGeneratedSemanticToolVersions({ vite: '8.1.2', rolldown: '1.1.4' }),
  /not lock-authorized/u,
)

const validatedArtifact = validateGeneratedSemanticArtifact(validArtifactInput())
const artifactSummary = generatedSemanticArtifactSummary(validatedArtifact)
assert.deepEqual(artifactSummary.exports, GENERATED_SEMANTIC_EXPORTS)
assert.deepEqual(artifactSummary.externalImports, ['node:crypto'])
await assert.rejects(
  publishGeneratedSemanticArtifact({
    artifact: { ...validatedArtifact },
    destination: join(TEST_ROOT, GENERATED_SEMANTIC_FILENAME),
  }),
  /requires a validated artifact/u,
)

const twoApprovedImports = validArtifactInput()
twoApprovedImports.builds[0].outputs[0].imports = ['node:path', 'node:crypto']
assert.deepEqual(
  generatedSemanticArtifactSummary(validateGeneratedSemanticArtifact(twoApprovedImports)).externalImports,
  ['node:crypto', 'node:path'],
)
const noExternalImports = validArtifactInput()
noExternalImports.builds[0].outputs[0].imports = []
assert.deepEqual(
  generatedSemanticArtifactSummary(validateGeneratedSemanticArtifact(noExternalImports)).externalImports,
  [],
)

for (const [label, mutate] of [
  ['build count', (input) => input.builds.push(...validBuilds())],
  ['output count', (input) => input.builds[0].outputs.push({ type: 'asset', fileName: 'extra' })],
  ['output name', (input) => { input.builds[0].outputs[0].fileName = 'unexpected.js' }],
  ['dynamic entry', (input) => { input.builds[0].outputs[0].isDynamicEntry = true }],
  ['source map', (input) => { input.builds[0].outputs[0].hasSourceMap = true }],
  ['external import', (input) => { input.builds[0].outputs[0].imports = ['node:os'] }],
  ['duplicate import', (input) => { input.builds[0].outputs[0].imports = ['node:path', 'node:path'] }],
  ['dynamic import', (input) => { input.builds[0].outputs[0].dynamicImports = ['./later.js'] }],
  ['export surface', (input) => { input.builds[0].outputs[0].exports = ['unexpected'] }],
  ['wrong full digest', (input) => {
    input.builds[0].outputs[0].code = GENERATED_CODE.replace(
      GENERATED_SEMANTIC_DIGEST,
      '0'.repeat(64),
    )
  }],
  ['digest prefix only', (input) => {
    input.builds[0].outputs[0].code = GENERATED_CODE.replace(
      GENERATED_SEMANTIC_DIGEST,
      GENERATED_SEMANTIC_DIGEST.slice(0, -1),
    )
  }],
  ['wrong final digest nibble', (input) => {
    input.builds[0].outputs[0].code = GENERATED_CODE.replace(
      GENERATED_SEMANTIC_DIGEST,
      `${GENERATED_SEMANTIC_DIGEST.slice(0, -1)}0`,
    )
  }],
  ['root surface', (input) => input.generatedRootEntries.push(directoryEntry('unexpected.txt'))],
  ['declaration payload', (input) =>
    input.generatedRootEntries.push(directoryEntry('final-semantic-reducer.d.mts'))],
  ['build surface', (input) => input.buildDirectoryEntries.pop()],
  ['root symlink', (input) => {
    input.generatedRootEntries[0] = directoryEntry(input.generatedRootEntries[0].name, {
      symbolicLink: true,
    })
  }],
  ['root wrong type', (input) => {
    const buildIndex = input.generatedRootEntries.findIndex(({ name }) => name === 'build')
    input.generatedRootEntries[buildIndex] = directoryEntry('build')
  }],
]) {
  const input = validArtifactInput()
  mutate(input)
  assertFailureKind(() => validateGeneratedSemanticArtifact(input), 'artifact-policy', label)
}

const destination = join(TEST_ROOT, GENERATED_SEMANTIC_FILENAME)
const successfulFilesystem = publicationFilesystem()
const publication = await publishGeneratedSemanticArtifact({
  artifact: validatedArtifact,
  destination,
  filesystem: successfulFilesystem.api,
  temporaryToken: () => 'contracttoken',
})
assert.equal(publication.ok, true)
assert.equal(dirname(publication.temporaryPath), dirname(destination))
assert.deepEqual(successfulFilesystem.events, [
  ['open', 'wx', 0o600],
  ['write'],
  ['sync'],
  ['close'],
  ['rename'],
])
assert.deepEqual(successfulFilesystem.destinationBytes, Buffer.from(GENERATED_CODE))

const renameFailureFilesystem = publicationFilesystem({ renameFailure: new Error('rename failed') })
const failedPublication = await publishGeneratedSemanticArtifact({
  artifact: validatedArtifact,
  destination,
  filesystem: renameFailureFilesystem.api,
  temporaryToken: () => 'contracttoken',
})
assert.equal(failedPublication.ok, false)
assert.deepEqual(renameFailureFilesystem.destinationBytes, Buffer.from('previous'))
assert.deepEqual(failedPublication.failures.map(({ kind, code }) => ({ kind, code })), [
  { kind: 'publication', code: 'atomic-replace-failed' },
])
assert(renameFailureFilesystem.events.some(([event]) => event === 'unlink'))

const cleanupFailureFilesystem = publicationFilesystem({
  writeFailure: new Error('write failed'),
  closeFailure: new Error('close failed'),
  unlinkFailure: new Error('unlink failed'),
})
const cleanupPublication = await publishGeneratedSemanticArtifact({
  artifact: validatedArtifact,
  destination,
  filesystem: cleanupFailureFilesystem.api,
  temporaryToken: () => 'contracttoken',
})
assert.deepEqual(cleanupPublication.failures.map(({ kind }) => kind), [
  'publication',
  'cleanup',
  'cleanup',
])

const tools = Object.freeze({ node: PINNED_NODE_VERSION, ...TOOL_VERSIONS })
const successResult = createGeneratedSemanticResult({
  mode: 'verify',
  outcome: 'current',
  tools,
  artifact: artifactSummary,
  failures: [],
})
const resultRecord = encodeGeneratedSemanticResult(successResult)
assert.deepEqual(parseGeneratedSemanticResultRecord(resultRecord), successResult)
assert.throws(() => createGeneratedSemanticResult({
  mode: 'verify',
  outcome: 'published',
  tools,
  artifact: artifactSummary,
  failures: [],
}), /requires write mode/u)
for (const externalImports of [
  ['node:path', 'node:crypto'],
  ['node:crypto', 'node:crypto'],
  ['node:os'],
]) {
  assert.throws(() => createGeneratedSemanticResult({
    mode: 'verify',
    outcome: 'current',
    tools,
    artifact: { ...artifactSummary, externalImports },
    failures: [],
  }), /canonical policy-constrained set/u)
}
for (const exports of [
  [...GENERATED_SEMANTIC_EXPORTS].reverse(),
  [GENERATED_SEMANTIC_EXPORTS[0]],
  [GENERATED_SEMANTIC_EXPORTS[0], GENERATED_SEMANTIC_EXPORTS[0]],
]) {
  assert.throws(() => createGeneratedSemanticResult({
    mode: 'verify',
    outcome: 'current',
    tools,
    artifact: { ...artifactSummary, exports },
    failures: [],
  }), /canonical policy-constrained set/u)
}
for (const invalidRecord of [
  '',
  resultRecord.slice(0, -1),
  resultRecord + resultRecord,
  `${resultRecord} `,
  '{]\n',
  resultRecord.replace('"component":', '"component":"duplicate","component":'),
]) assert.throws(() => parseGeneratedSemanticResultRecord(invalidRecord))
for (const mutate of [
  (result) => { result.schemaVersion = 'windshare.generated-semantic-result/v2' },
  (result) => { result.extra = true },
]) {
  const result = JSON.parse(resultRecord)
  mutate(result)
  assert.throws(() => parseGeneratedSemanticResultRecord(`${JSON.stringify(result)}\n`))
}
const retainedFailures = [
  createGeneratedSemanticFailure('publication', 'primary-failure', 'publication failed'),
  createGeneratedSemanticFailure('cleanup', 'cleanup-failure', 'cleanup failed'),
]
const failedResult = createGeneratedSemanticResult({
  mode: 'write',
  outcome: 'failed',
  tools,
  artifact: artifactSummary,
  failures: retainedFailures,
})
assert.deepEqual(parseGeneratedSemanticResultRecord(encodeGeneratedSemanticResult(failedResult)), failedResult)

let invalidCliTouchedDependencies = false
const dependencyTrap = new Proxy({}, {
  ownKeys() {
    invalidCliTouchedDependencies = true
    throw new Error('invalid arguments must not inspect dependencies')
  },
})
const invalidCli = await runGeneratedSemanticCli(['--invalid'], dependencyTrap)
assert.equal(invalidCli.outcome, 'failed')
assert.equal(invalidCli.failures[0].kind, 'usage')
assert.equal(invalidCliTouchedDependencies, false)

for (const hostileCause of [errorWithUnreadableMessage(), hostileProxyCause()]) {
  const hostileCli = await executeGeneratedSemanticCli([], {
    parentPermissionModel: undefined,
    readPinnedNodeVersion: () => { throw hostileCause },
  })
  assert.equal(hostileCli.exitCode, 1)
  assert.deepEqual(parseGeneratedSemanticResultRecord(hostileCli.record), hostileCli.result)
  assert.equal(hostileCli.result.failures[0].code, 'unexpected-build-failure')
  assert.equal(
    hostileCli.result.failures[0].message,
    'generated semantic build failed unexpectedly',
  )
}

const permissionCliHarness = cliHarness(Buffer.from(GENERATED_CODE))
const permissionCli = await runGeneratedSemanticCli([], {
  ...permissionCliHarness.dependencies,
  parentPermissionModel: { has: () => true },
})
assert.equal(permissionCli.outcome, 'failed')
assert.equal(permissionCli.failures[0].code, 'parent-permission-model-active')
assert.equal(permissionCliHarness.launches, 0)
assert.equal(permissionCliHarness.removals, 0)

const currentCliHarness = cliHarness(Buffer.from(GENERATED_CODE))
const currentCli = await runGeneratedSemanticCli([], currentCliHarness.dependencies)
assert.equal(currentCli.outcome, 'current')
assert.equal(currentCliHarness.launches, 1)
assert.equal(currentCliHarness.removals, 1)
assert.equal(currentCliHarness.publications, 0)
assert.equal(currentCli.artifact.externalImports.length, 1)

const currentWriteCliHarness = cliHarness(Buffer.from(GENERATED_CODE))
const currentWriteCli = await runGeneratedSemanticCli(
  ['--write'],
  currentWriteCliHarness.dependencies,
)
assert.equal(currentWriteCli.outcome, 'current')
assert.equal(currentWriteCliHarness.publications, 0)

const productionInventoryHarness = cliHarness(Buffer.from(GENERATED_CODE))
delete productionInventoryHarness.dependencies.readDirectoryEntries
const productionInventoryCli = await runGeneratedSemanticCli(
  [],
  productionInventoryHarness.dependencies,
)
assert.equal(
  productionInventoryCli.outcome,
  'current',
  JSON.stringify(productionInventoryCli.failures),
)

const writeCliHarness = cliHarness(Buffer.from('stale'))
const writtenCli = await runGeneratedSemanticCli(['--write'], writeCliHarness.dependencies)
assert.equal(writtenCli.outcome, 'published')
assert.equal(writeCliHarness.publications, 1)
assert.equal(writeCliHarness.removals, 1)
assert.deepEqual(writeCliHarness.events, ['launch', 'cleanup', 'publish'])

const cleanupCliHarness = cliHarness(Buffer.from('stale'), { cleanupFailure: true })
const cleanupCli = await runGeneratedSemanticCli(['--write'], cleanupCliHarness.dependencies)
assert.equal(cleanupCli.outcome, 'failed')
assert.deepEqual(cleanupCli.failures.map(({ kind }) => kind), ['cleanup'])
assert.notEqual(cleanupCli.tools, null)
assert.notEqual(cleanupCli.artifact, null)
assert.equal(cleanupCliHarness.publications, 0)
assert.deepEqual(cleanupCliHarness.destinationBytes, Buffer.from('stale'))

const malformedPublisherHarness = cliHarness(Buffer.from('stale'), {
  publisherResult: {
    ok: false,
    published: false,
    temporaryPath: join(TEST_ROOT, '.malformed.tmp'),
    failures: [],
  },
})
const malformedPublisherCli = await runGeneratedSemanticCli(
  ['--write'],
  malformedPublisherHarness.dependencies,
)
assert.equal(malformedPublisherCli.outcome, 'failed')
assert.equal(malformedPublisherCli.failures[0].code, 'publisher-result-invalid')

process.stdout.write('generated semantic isolated build contracts: PASS\n')

function errorWithUnreadableMessage() {
  const cause = new Error('hidden')
  Object.defineProperty(cause, 'message', {
    get() {
      throw new Error('message accessor must not control settlement')
    },
  })
  return cause
}

function hostileProxyCause() {
  return new Proxy(new Error('hidden'), {
    getPrototypeOf() {
      throw new Error('prototype trap must not control settlement')
    },
  })
}

function rawGeneratedChunk() {
  return {
    type: 'chunk',
    fileName: GENERATED_SEMANTIC_FILENAME,
    isEntry: true,
    isDynamicEntry: false,
    exports: [...GENERATED_SEMANTIC_EXPORTS],
    imports: ['node:crypto'],
    dynamicImports: [],
    code: GENERATED_CODE,
    map: null,
  }
}

function validBuilds() {
  const chunk = rawGeneratedChunk()
  const { map: _, ...distilled } = chunk
  return [{ outputs: [{ ...distilled, hasSourceMap: false }] }]
}

function validArtifactInput() {
  return {
    builds: validBuilds(),
    generatedRootEntries: directoryEntries(GENERATED_SEMANTIC_ROOT_ALLOWLIST, ['build']),
    buildDirectoryEntries: directoryEntries(GENERATED_SEMANTIC_BUILD_ALLOWLIST),
  }
}

function directoryEntries(names, directoryNames = []) {
  return names.map((name) => directoryEntry(name, { directory: directoryNames.includes(name) }))
}

function directoryEntry(name, { directory = false, symbolicLink = false } = {}) {
  return {
    name,
    isFile: () => !directory && !symbolicLink,
    isDirectory: () => directory && !symbolicLink,
    isSymbolicLink: () => symbolicLink,
  }
}

function assertFailureKind(action, kind, label) {
  assert.throws(action, (error) => {
    assert.equal(error?.failure?.kind, kind, label)
    return true
  })
}

function publicationFilesystem({
  writeFailure = null,
  closeFailure = null,
  renameFailure = null,
  unlinkFailure = null,
} = {}) {
  const state = {
    destinationBytes: Buffer.from('previous'),
    stagedBytes: null,
    events: [],
  }
  state.api = {
    async open(_path, flag, mode) {
      state.events.push(['open', flag, mode])
      return {
        async writeFile(bytes) {
          state.events.push(['write'])
          if (writeFailure !== null) throw writeFailure
          state.stagedBytes = Buffer.from(bytes)
        },
        async sync() {
          state.events.push(['sync'])
        },
        async close() {
          state.events.push(['close'])
          if (closeFailure !== null) throw closeFailure
        },
      }
    },
    async rename() {
      state.events.push(['rename'])
      if (renameFailure !== null) throw renameFailure
      state.destinationBytes = Buffer.from(state.stagedBytes)
    },
    async unlink() {
      state.events.push(['unlink'])
      if (unlinkFailure !== null) throw unlinkFailure
      state.stagedBytes = null
    },
  }
  return state
}

function cliHarness(committedBytes, { cleanupFailure = false, publisherResult = null } = {}) {
  const state = {
    launches: 0,
    removals: 0,
    publications: 0,
    destinationBytes: Buffer.from(committedBytes),
    events: [],
  }
  const temporaryRoot = join(TEST_ROOT, 'cli-temporary')
  state.dependencies = {
    actualNodeVersion: `v${PINNED_NODE_VERSION}`,
    platform: 'linux',
    inheritedEnvironment: {},
    nodeExecutable: NODE_EXECUTABLE,
    readPinnedNodeVersion: () => PINNED_NODE_VERSION,
    readToolAuthorization: async (path) => {
      assert.equal(path, GENERATED_SEMANTIC_PATHS.toolLockPath)
      return TOOL_VERSIONS
    },
    assertPinnedNodeVersion: ({ actualVersion, pinnedVersion }) => {
      assert.equal(actualVersion, `v${PINNED_NODE_VERSION}`)
      assert.equal(pinnedVersion, PINNED_NODE_VERSION)
    },
    createTemporaryRoot: async () => temporaryRoot,
    removeTemporaryRoot: async (path) => {
      state.removals += 1
      state.events.push('cleanup')
      assert.equal(path, temporaryRoot)
      if (cleanupFailure) throw new Error('cleanup failed')
    },
    readDirectoryEntries: async (path) => path === GENERATED_SEMANTIC_PATHS.buildRoot
      ? directoryEntries(GENERATED_SEMANTIC_BUILD_ALLOWLIST)
      : directoryEntries(GENERATED_SEMANTIC_ROOT_ALLOWLIST, ['build']),
    readCommittedArtifact: async () => Buffer.from(committedBytes),
    launchWorker: async ({ environment, workingDirectory }) => {
      state.launches += 1
      state.events.push('launch')
      assert.equal(environment.NODE_ENV, 'production')
      assert.equal(workingDirectory, temporaryRoot)
      return {
        terminal: 'exited',
        exitCode: 0,
        signal: null,
        stdout: workerRecord,
        stderr: '',
      }
    },
    publishArtifact: async ({ artifact, destination: path }) => {
      state.publications += 1
      state.events.push('publish')
      assert.equal(generatedSemanticArtifactSummary(artifact).fileName, GENERATED_SEMANTIC_FILENAME)
      assert.equal(path, GENERATED_SEMANTIC_PATHS.committedPath)
      if (publisherResult !== null) return publisherResult
      state.destinationBytes = Buffer.from(GENERATED_CODE)
      return Object.freeze({
        ok: true,
        published: true,
        temporaryPath: join(TEST_ROOT, '.published.tmp'),
        failures: Object.freeze([]),
      })
    },
  }
  return state
}
