import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { EventEmitter } from 'node:events'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'

import {
  buildInvocationRuntime,
  createBootstrapProcessOwnerAuthority,
} from '../../orchestrator.mjs'
import {
  BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION,
  assertBootstrapBuildOutput,
  goModuleDownloadClosure,
  runSealedBootstrapProcess,
} from '../../process/bootstrap-build-authority.mjs'
import { resolveHostExecutable } from '../../process/runtime-command-owner.mjs'
import {
  GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
  GENERATED_SEMANTIC_DIGEST,
  GENERATED_SEMANTIC_EXPORTS,
} from '../../generated-semantic/build/artifact-policy.mjs'
import { GENERATED_SEMANTIC_FILENAME } from '../../generated-semantic/build/config.mjs'
import {
  createGeneratedSemanticResult,
  encodeGeneratedSemanticResult,
} from '../../generated-semantic/build/result-contract.mjs'
import { GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS } from '../../generated-semantic/build/tool-authorization.mjs'
import { GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID } from '../../generated-semantic/runtime-preflight.mjs'
import {
  buildBrowsergateRuntime,
  disposeBrowsergateRuntime,
  loadBrowsergateRuntime,
} from '../../runtime-build.mjs'
import { readPinnedNodeVersion } from '../../../node-version.mjs'

const root = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const GENERATED_SEMANTIC_ARTIFACT_PATH = join(
  root,
  'scripts',
  'ci',
  'browsergate',
  'generated-semantic',
  GENERATED_SEMANTIC_FILENAME,
)
const SELF_CHECK_BY_KIND = Object.freeze({
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

verifyWindowsNativeExecutableAuthority()
verifyBootstrapBuildOutputContract()
await verifyBootstrapFailureDiagnostics()
await exactOwnedExecutionCounts()
await generatedSemanticFailureMatrixStopsNativeBuilds()
await hostileGeneratedSemanticFailuresSettleOnce()
await manifestAuthorityRejectsPlatformAndRelativePath()
await sampleCommandManifestPinsAmbientAndFileAuthority()
await buildEvidenceMustProveTreeEmpty()
await preflightMustProveEmptyStderr()
await clippedOwnershipLeaseNeverLaunches()
await bootstrapClosesAfterRuntimeBuildFailure()
await bootstrapFailureRemovesItsPrivateRoot()
await bootstrapReceiptCannotClaimTreeEvidence()

function verifyWindowsNativeExecutableAuthority() {
  const commandRoot = mkdtempSync(resolve(tmpdir(), 'windshare-host-executable-'))
  try {
    writeFileSync(resolve(commandRoot, 'pnpm.cmd'), '@exit /b 0\r\n')
    assert.throws(
      () => resolveHostExecutable('pnpm', {
        platform: 'win32',
        environment: { Path: commandRoot },
      }),
      /cannot resolve host executable pnpm/u,
      'a shell command shim must not become native process authority',
    )

    const nativeExecutable = resolve(commandRoot, 'pnpm.exe')
    writeFileSync(nativeExecutable, 'native executable placeholder')
    assert.equal(
      resolveHostExecutable('pnpm', {
        platform: 'win32',
        environment: { PATH: commandRoot },
      }),
      realpathSync(nativeExecutable),
      'Windows host commands must resolve to canonical native executables',
    )
  } finally {
    rmSync(commandRoot, { recursive: true, force: true })
  }
}

function verifyBootstrapBuildOutputContract() {
  const allowed = goModuleDownloadClosure([
    'example.com/dependency v1.2.3 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=',
    'example.com/dependency v1.2.3/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=',
    'example.com/transitive/module v0.0.0-20260101000000-abcdef123456 h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=',
    '',
  ].join('\n'))
  assert.deepEqual(allowed, [
    'example.com/dependency v1.2.3',
    'example.com/transitive/module v0.0.0-20260101000000-abcdef123456',
  ])
  assert.doesNotThrow(() => assertBootstrapBuildOutput({
    stdout: '',
    stderr: 'go: downloading example.com/dependency v1.2.3\n',
  }, allowed))
  for (const stderr of [
    'go: downloading example.com/unknown v1.2.3\n',
    'go: downloading ../dependency v1.2.3\n',
    'go: downloading example.com/dependency v1.2.3\u001b[31m\n',
    'go: downloading example.com/dependency v1.2.3\ncompiler warning\n',
    'go: downloading example.com/dependency v1.2.3',
  ]) {
    assert.throws(
      () => assertBootstrapBuildOutput({ stdout: '', stderr }, allowed),
      /unexpected output/u,
    )
  }
  assert.throws(
    () => assertBootstrapBuildOutput({ stdout: 'unexpected\n', stderr: '' }, allowed),
    /unexpected output/u,
  )
}

async function verifyBootstrapFailureDiagnostics() {
  const successful = await runSealedBootstrapProcess(bootstrapFixture({
    operation: 'bootstrap-test-success',
    recipeIdentity: 'windshare.bootstrap-test/success/v1',
    stdout: 'success stdout\n',
    stderr: 'success stderr\n',
  }))
  assert.deepEqual(successful, {
    stdout: 'success stdout\n',
    stderr: 'success stderr\n',
  }, 'valid bounded output from exit zero retains its established success contract')

  const argumentSecret = 'bootstrap-argument-secret-must-not-leak'
  const environmentSecret = 'bootstrap-environment-secret-must-not-leak'
  const nonzero = await captureBootstrapFailure(bootstrapFixture({
    operation: 'bootstrap-test-nonzero',
    recipeIdentity: 'windshare.bootstrap-test/nonzero/v1',
    stdout: 'bounded stdout\n',
    stderr: 'bounded stderr\n',
    exitCode: 7,
    extraArguments: [argumentSecret],
    environment: { WINDSHARE_BOOTSTRAP_TEST_SECRET: environmentSecret },
  }))
  const nonzeroDiagnostic = parseBootstrapFailure(nonzero)
  assert.equal(nonzeroDiagnostic.operation, 'bootstrap-test-nonzero')
  assert.equal(nonzeroDiagnostic.recipeIdentity, 'windshare.bootstrap-test/nonzero/v1')
  assert.deepEqual(nonzeroDiagnostic.failureReasons, ['terminal-not-successful'])
  assert.deepEqual(nonzeroDiagnostic.process, {
    terminal: 'exited',
    exitCode: 7,
    timedOut: false,
  })
  assert.deepEqual(nonzeroDiagnostic.stdout, expectedTextDiagnostic('bounded stdout\n'))
  assert.deepEqual(nonzeroDiagnostic.stderr, expectedTextDiagnostic('bounded stderr\n'))
  assert.equal(nonzero.message.includes(argumentSecret), false)
  assert.equal(nonzero.message.includes(environmentSecret), false)

  const oversizedByteLength = 1_048_577
  const oversized = await captureBootstrapFailure(bootstrapFixture({
    operation: 'bootstrap-test-oversized',
    recipeIdentity: 'windshare.bootstrap-test/oversized/v1',
    stdout: Buffer.alloc(oversizedByteLength, 97),
  }))
  const oversizedDiagnostic = parseBootstrapFailure(oversized)
  assert.deepEqual(oversizedDiagnostic.failureReasons, ['stdout-byte-limit-exceeded'])
  assert.deepEqual(oversizedDiagnostic.process, {
    terminal: 'exited',
    exitCode: 0,
    timedOut: false,
  })
  assert.equal(oversizedDiagnostic.stdout.observedByteLength, oversizedByteLength)
  assert.equal(oversizedDiagnostic.stdout.capturedByteLength, 1_048_576)
  assert.equal(oversizedDiagnostic.stdout.truncated, true)
  assert.equal(oversizedDiagnostic.stdout.sha256, sha256(Buffer.alloc(oversizedByteLength, 97)))
  assert.equal(oversizedDiagnostic.stdout.utf8, 'valid')
  assert.equal(oversizedDiagnostic.stdout.preview, 'a'.repeat(512))
  assert.equal(oversizedDiagnostic.stdout.previewTruncated, true)

  const invalidBytes = Buffer.from([0xff, 0xfe, 0xfd])
  const invalidUtf8 = await captureBootstrapFailure(bootstrapFixture({
    operation: 'bootstrap-test-invalid-utf8',
    recipeIdentity: 'windshare.bootstrap-test/invalid-utf8/v1',
    stdout: invalidBytes,
  }))
  const invalidDiagnostic = parseBootstrapFailure(invalidUtf8)
  assert.deepEqual(invalidDiagnostic.failureReasons, ['stdout-invalid-utf8'])
  assert.equal(invalidDiagnostic.stdout.observedByteLength, invalidBytes.byteLength)
  assert.equal(invalidDiagnostic.stdout.capturedByteLength, invalidBytes.byteLength)
  assert.equal(invalidDiagnostic.stdout.truncated, false)
  assert.equal(invalidDiagnostic.stdout.sha256, sha256(invalidBytes))
  assert.equal(invalidDiagnostic.stdout.utf8, 'invalid')
  assert.equal('preview' in invalidDiagnostic.stdout, false)
  assert.equal('previewTruncated' in invalidDiagnostic.stdout, false)
}

function bootstrapFixture({
  operation,
  recipeIdentity,
  stdout = Buffer.alloc(0),
  stderr = Buffer.alloc(0),
  exitCode = 0,
  extraArguments = [],
  environment = {},
}) {
  const stdoutBytes = Buffer.from(stdout)
  const stderrBytes = Buffer.from(stderr)
  // The contract exercises collection and settlement without competing with the
  // gate's own process authority, which is the behavior under test elsewhere.
  const spawnProcess = () => {
    const child = new EventEmitter()
    child.stdout = new EventEmitter()
    child.stderr = new EventEmitter()
    child.kill = () => true
    queueMicrotask(() => {
      if (stdoutBytes.byteLength > 0) child.stdout.emit('data', stdoutBytes)
      if (stderrBytes.byteLength > 0) child.stderr.emit('data', stderrBytes)
      child.emit('close', exitCode, null)
    })
    return child
  }
  return Object.freeze({
    executable: process.execPath,
    arguments: Object.freeze(['sealed-bootstrap-fixture', ...extraArguments]),
    cwd: root,
    environment: Object.freeze(environment),
    deadlineMs: 10_000,
    operation,
    recipeIdentity,
    spawnProcess,
  })
}

async function captureBootstrapFailure(request) {
  let failure
  try {
    await runSealedBootstrapProcess(request)
  } catch (cause) {
    failure = cause
  }
  assert(failure instanceof Error, 'bootstrap fixture must reject')
  return failure
}

function parseBootstrapFailure(failure) {
  const prefix = 'bootstrap sealed process failed: '
  assert(failure.message.startsWith(prefix))
  return JSON.parse(failure.message.slice(prefix.length))
}

function expectedTextDiagnostic(text) {
  return {
    observedByteLength: Buffer.byteLength(text),
    capturedByteLength: Buffer.byteLength(text),
    truncated: false,
    sha256: sha256(Buffer.from(text, 'utf8')),
    utf8: 'valid',
    preview: text,
    previewTruncated: false,
  }
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function successfulGeneratedSemanticRecord() {
  return encodeGeneratedSemanticResult(createGeneratedSemanticResult({
    mode: 'verify',
    outcome: 'current',
    tools: {
      node: readPinnedNodeVersion(root),
      vite: GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.vite,
      rolldown: GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.rolldown,
    },
    artifact: expectedGeneratedSemanticArtifact(),
    failures: [],
  }))
}

function expectedGeneratedSemanticArtifact() {
  const bytes = readFileSync(GENERATED_SEMANTIC_ARTIFACT_PATH)
  const externalImports = ['node:crypto']
  assert(externalImports.every((entry) => GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS.includes(entry)))
  return Object.freeze({
    fileName: GENERATED_SEMANTIC_FILENAME,
    byteLength: bytes.byteLength,
    sha256: sha256(bytes),
    semanticDigest: GENERATED_SEMANTIC_DIGEST,
    exports: GENERATED_SEMANTIC_EXPORTS,
    externalImports: Object.freeze(externalImports),
  })
}

function mutateGeneratedSemanticRecord(record, changes) {
  const value = JSON.parse(record.slice(0, -1))
  return JSON.stringify({ ...value, ...changes }) + '\n'
}

async function exactOwnedExecutionCounts() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-counts-'))
  const calls = []
  let resolveCount = 0
  const bootstrapEvents = []
  const generatedSemanticEvents = []
  try {
    const runtime = await buildInvocationRuntime({
      suites: ['main', 'pion'],
      platform: 'win32',
      outputParent,
      preserveRuntimeRoot: true,
      inheritedEnvironment: Object.freeze({
        PATH: 'not-authority',
        SystemRoot: 'C:\\Windows',
        WINDSHARE_SECRET: 'must-not-cross-generated-semantic-boundary',
      }),
      resolveExecutable: () => {
        resolveCount += 1
        return process.execPath
      },
      createBootstrapOwner: async () => fakeBootstrapOwner('win32', bootstrapEvents),
      deadlineAuthority: fullLeaseAuthority(),
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
      executeOwnedCommand: async (request) => {
        calls.push(request)
        if (request.operationId === GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID) {
          request.trace({
            milestone: 'runtime-command-started',
            context: { operationId: request.operationId },
          })
          return successfulExecution(successfulGeneratedSemanticRecord())
        }
        if (request.operationId.startsWith('browser-runtime-build-')) {
          const outputIndex = request.command.arguments.indexOf('-o')
          assert.notEqual(outputIndex, -1)
          writeFileSync(request.command.arguments[outputIndex + 1], request.operationId, { flag: 'wx' })
          return successfulExecution('')
        }
        const kind = request.operationId.slice('browser-runtime-preflight-'.length)
        return successfulExecution(SELF_CHECK_BY_KIND[kind])
      },
      trace: () => { throw new Error('hostile trace sink') },
      generatedSemanticTrace: (event) => generatedSemanticEvents.push(event),
    })
    assert.equal(resolveCount, 1)
    assert.deepEqual(runtime.manifest.artifacts.map(({ kind }) => kind), [
      'windows-job',
      'topology-materializer',
      'artifact-publisher',
      'pion-server',
    ])
    assert.equal(calls.length, 9)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('generated-semantic')).length, 1)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('-build-')).length, 4)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('-preflight-')).length, 4)
    const nodeBytes = readFileSync(process.execPath)
    assert.equal(calls[0].command.executable, process.execPath)
    assert.equal(calls[0].command.executableByteLength, nodeBytes.byteLength)
    assert.equal(calls[0].command.executableSha256, sha256(nodeBytes))
    assert.deepEqual(calls[0].inheritedEnvironment, {
      NODE_ENV: 'production',
      TZ: 'UTC',
      LANG: 'C',
      LC_ALL: 'C',
      TMPDIR: dirname(process.execPath),
      TMP: dirname(process.execPath),
      TEMP: dirname(process.execPath),
      SystemRoot: 'C:\\Windows',
      WINDIR: 'C:\\Windows',
    })
    assert.equal(calls[0].windowsJobHelper.path, process.execPath)
    assert.equal(calls[1].windowsJobHelper.path, process.execPath)
    assert.equal(calls[2].windowsJobHelper.kind, 'windows-job')
    assert(calls.slice(2).every(({ windowsJobHelper }) => windowsJobHelper?.kind === 'windows-job'))
    assert.deepEqual(
      generatedSemanticEvents.map(({ operationId, milestone }) => ({ operationId, milestone })),
      [
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
        {
          operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID,
          milestone: 'artifact-validated',
        },
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
      ],
    )
    assert.equal(generatedSemanticEvents[0].context.mode, 'verify')
    assert.equal(generatedSemanticEvents[1].context.nodeVersion, readPinnedNodeVersion(root))
    assert.equal(
      generatedSemanticEvents[1].context.artifactSha256,
      expectedGeneratedSemanticArtifact().sha256,
    )
    assert.equal(generatedSemanticEvents[2].context.outcome, 'current')
    assert(Number.isSafeInteger(generatedSemanticEvents[2].context.elapsedMs))
    assert.deepEqual(bootstrapEvents, ['assert-live', 'assert-live', 'close'])
    const manifestPath = runtime.manifestPath
    const manifestSha256 = runtime.manifestSha256
    runtime.dispose()
    const cleanup = disposeBrowsergateRuntime({
      manifestPath,
      manifestSha256,
      expectedPlatform: 'win32',
    })
    assert.equal(existsSync(cleanup.runtimeRoot), false)
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function generatedSemanticFailureMatrixStopsNativeBuilds() {
  const successfulRecord = successfulGeneratedSemanticRecord()
  const failedRecord = encodeGeneratedSemanticResult(createGeneratedSemanticResult({
    mode: 'verify',
    outcome: 'failed',
    tools: null,
    artifact: null,
    failures: [{
      kind: 'stale-output',
      code: 'committed-artifact-stale',
      message: 'generated final semantic reducer is stale',
    }],
  }))
  const cases = [
    ['missing-record', () => successfulExecution('')],
    ['duplicate-record', () => successfulExecution(successfulRecord + successfulRecord)],
    ['malformed-record', () => successfulExecution('{not-json}\n')],
    ['trailing-record-data', () => successfulExecution(successfulRecord + 'trailing')],
    ['schema-mismatch', () => successfulExecution(mutateGeneratedSemanticRecord(
      successfulRecord,
      { schemaVersion: 'windshare.generated-semantic-result/v2' },
    ))],
    ['nonempty-stderr', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      stderr: 'unexpected verifier diagnostic\n',
    })],
    ['wrong-mode', () => successfulExecution(mutateGeneratedSemanticRecord(
      successfulRecord,
      { mode: 'write' },
    ))],
    ['wrong-outcome', () => successfulExecution(mutateGeneratedSemanticRecord(
      successfulRecord,
      { outcome: 'published' },
    ))],
    ['typed-failure', () => Object.freeze({
      ...successfulExecution(failedRecord),
      processEvidence: Object.freeze({ terminal: 'exited', exitCode: 1 }),
    })],
    ['tool-evidence-mismatch', () => successfulExecution(mutateGeneratedSemanticRecord(
      successfulRecord,
      {
        tools: {
          node: readPinnedNodeVersion(root),
          vite: '9.9.9',
          rolldown: GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.rolldown,
        },
      },
    ))],
    ['artifact-evidence-mismatch', () => successfulExecution(mutateGeneratedSemanticRecord(
      successfulRecord,
      {
        artifact: {
          ...expectedGeneratedSemanticArtifact(),
          sha256: 'f'.repeat(64),
        },
      },
    ))],
    ['spawn-failure', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      launched: false,
      treeEmpty: false,
      processEvidence: Object.freeze({
        terminal: 'spawn-failed',
        errorCode: 'ENOENT',
        errorMessage: 'injected spawn failure',
      }),
    })],
    ['deadline-failure', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      timedOut: true,
    })],
    ['tree-settlement-failure', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      treeEmpty: false,
    })],
    ['nonzero-exit', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      processEvidence: Object.freeze({ terminal: 'exited', exitCode: 7 }),
    })],
  ]
  const nodeBytes = readFileSync(process.execPath)
  const nodeAuthority = Object.freeze({
    path: process.execPath,
    byteLength: nodeBytes.byteLength,
    sha256: sha256(nodeBytes),
  })
  const artifactAuthority = Object.freeze({
    path: GENERATED_SEMANTIC_ARTIFACT_PATH,
    ...expectedGeneratedSemanticArtifact(),
  })

  for (const [name, execution] of cases) {
    const outputParent = mkdtempSync(join(tmpdir(), `windshare-semantic-${name}-`))
    const operations = []
    const events = []
    let nativeBuildCount = 0
    try {
      await assert.rejects(
        () => buildInvocationRuntime({
          suites: ['main'],
          platform: 'linux',
          outputParent,
          inheritedEnvironment: Object.freeze({
            PATH: '/ambient/not-authority',
            VITE_HOSTILE: 'must-not-cross-generated-semantic-boundary',
          }),
          resolveExecutable: () => process.execPath,
          createBootstrapOwner: async () => fakeBootstrapOwner('linux'),
          deadlineAuthority: fullLeaseAuthority(),
          buildLeaseId: 'runtime/batch-build',
          preflightLeaseId: 'runtime/manifest-preflight',
          authenticateRuntimeFile: async (path) => {
            if (path === process.execPath) return nodeAuthority
            if (path === GENERATED_SEMANTIC_ARTIFACT_PATH) return artifactAuthority
            throw new Error(`unexpected runtime file authentication request ${path}`)
          },
          executeOwnedCommand: async (request) => {
            operations.push(request.operationId)
            if (request.operationId === GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID) {
              assert.deepEqual(request.inheritedEnvironment, {
                NODE_ENV: 'production',
                TZ: 'UTC',
                LANG: 'C',
                LC_ALL: 'C',
                TMPDIR: dirname(process.execPath),
                TMP: dirname(process.execPath),
                TEMP: dirname(process.execPath),
              })
              request.trace({
                milestone: 'runtime-command-tree-empty',
                context: { operationId: request.operationId },
              })
              return execution()
            }
            nativeBuildCount += 1
            return successfulExecution('')
          },
          generatedSemanticTrace: (event) => events.push(event),
        }),
        /generated semantic verifier failed before runtime batch build/u,
        name,
      )
      assert.deepEqual(operations, [GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID], name)
      assert.equal(nativeBuildCount, 0, name)
      assert.deepEqual(events.map(({ operationId, milestone }) => ({ operationId, milestone })), [
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
      ], name)
      assert.equal(events[1].context.mode, 'verify', name)
      assert.equal(events[1].context.outcome, 'failed', name)
      assert.equal(typeof events[1].context.failureCode, 'string', name)
      assert(Number.isSafeInteger(events[1].context.elapsedMs), name)
      if (name === 'typed-failure') {
        assert.equal(events[1].context.failureCode, 'reported-failure')
        assert.deepEqual(events[1].context.failures, [{
          kind: 'stale-output',
          code: 'committed-artifact-stale',
          message: 'generated final semantic reducer is stale',
        }])
      }
      assert.deepEqual(readdirSync(outputParent), [], name)
    } finally {
      rmSync(outputParent, { recursive: true, force: true })
    }
  }

  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-semantic-bootstrap-owner-failure-'))
  const events = []
  let launchCount = 0
  try {
    await assert.rejects(() => buildInvocationRuntime({
      suites: ['main'],
      platform: 'linux',
      outputParent,
      inheritedEnvironment: Object.freeze({ PATH: '/ambient/not-authority' }),
      resolveExecutable: () => process.execPath,
      createBootstrapOwner: async () => { throw new Error('injected bootstrap owner failure') },
      deadlineAuthority: fullLeaseAuthority(),
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
      executeOwnedCommand: async () => {
        launchCount += 1
        return successfulExecution(successfulRecord)
      },
      generatedSemanticTrace: (event) => events.push(event),
    }), /generated semantic verifier failed before runtime batch build/u)
    assert.equal(launchCount, 0)
    assert.deepEqual(events.map(({ operationId, milestone }) => ({ operationId, milestone })), [
      { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
      { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
    ])
    assert.equal(events[1].context.failureCode, 'unexpected-preflight-failure')
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function hostileGeneratedSemanticFailuresSettleOnce() {
  for (const [name, hostileCause] of [
    ['message-accessor', errorWithUnreadableMessage()],
    ['prototype-proxy', hostileProxyCause()],
  ]) {
    const outputParent = mkdtempSync(join(tmpdir(), `windshare-semantic-hostile-${name}-`))
    const events = []
    let launchCount = 0
    try {
      await assert.rejects(() => buildInvocationRuntime({
        suites: ['main'],
        platform: 'linux',
        outputParent,
        inheritedEnvironment: Object.freeze({ PATH: '/ambient/not-authority' }),
        resolveExecutable: () => process.execPath,
        createBootstrapOwner: async () => { throw hostileCause },
        deadlineAuthority: fullLeaseAuthority(),
        buildLeaseId: 'runtime/batch-build',
        preflightLeaseId: 'runtime/manifest-preflight',
        executeOwnedCommand: async () => {
          launchCount += 1
          throw new Error('hostile bootstrap failure must not launch a verifier')
        },
        generatedSemanticTrace: (event) => events.push(event),
      }), /generated semantic verifier failed before runtime batch build/u)
      assert.equal(launchCount, 0, name)
      assert.deepEqual(events.map(({ operationId, milestone }) => ({ operationId, milestone })), [
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
      ], name)
      assert.equal(events[1].context.failureCode, 'unexpected-preflight-failure', name)
      assert.equal(
        events[1].context.failureMessage,
        'generated semantic runtime preflight failed unexpectedly',
        name,
      )
      assert.deepEqual(readdirSync(outputParent), [], name)
    } finally {
      rmSync(outputParent, { recursive: true, force: true })
    }
  }
}

async function manifestAuthorityRejectsPlatformAndRelativePath() {
  const fixture = await buildFixture('linux')
  try {
    assert.throws(() => loadBrowsergateRuntime({
      manifestPath: fixture.runtime.manifestPath,
      manifestSha256: fixture.runtime.manifestSha256,
      expectedPlatform: 'win32',
    }), /platform differs/u)
    assert.throws(() => loadBrowsergateRuntime({
      manifestPath: basename(fixture.runtime.manifestPath),
      manifestSha256: fixture.runtime.manifestSha256,
      expectedPlatform: 'linux',
    }), /absolute and canonical/u)
  } finally {
    fixture.runtime.dispose()
    rmSync(fixture.outputParent, { recursive: true, force: true })
  }
}

async function sampleCommandManifestPinsAmbientAndFileAuthority() {
  const fixtureRoot = mkdtempSync(join(tmpdir(), 'windshare-runtime-command-'))
  const repositoryRoot = join(fixtureRoot, 'repository')
  const outputParent = join(fixtureRoot, 'runtime')
  const driverSource = join(
    repositoryRoot,
    'web',
    'scripts',
    'browser-evidence',
    'sample-driver.ts',
  )
  const playwrightCli = join(
    repositoryRoot,
    'web',
    'node_modules',
    '@playwright',
    'test',
    'cli.js',
  )
  const nodeExecutable = join(fixtureRoot, 'node.exe')
  mkdirSync(join(driverSource, '..'), { recursive: true })
  mkdirSync(join(playwrightCli, '..'), { recursive: true })
  mkdirSync(outputParent, { recursive: true })
  writeFileSync(driverSource, 'sample driver capability')
  writeFileSync(playwrightCli, 'playwright capability')
  writeFileSync(nodeExecutable, 'node capability')
  let runtime
  const previousPath = process.env.PATH
  try {
    runtime = await buildBrowsergateRuntime({
      repositoryRoot,
      suites: ['main'],
      platform: 'linux',
      outputParent,
      nodeExecutable,
      inheritedEnvironment: Object.freeze({
        PATH: '/manifest/pinned/bin',
        GITHUB_TOKEN: 'ambient-credential-must-not-enter-manifest',
      }),
      executeBuild: async ({ outputPath }) => {
        writeFileSync(outputPath, 'runtime executable', { flag: 'wx' })
        return successfulExecution('', 'linux')
      },
      executePreflight: async ({ kind }) =>
        successfulExecution(SELF_CHECK_BY_KIND[kind], 'linux'),
    })
    assert.deepEqual(runtime.sampleCommandCapability().environment, {
      PATH: '/manifest/pinned/bin',
    })
    process.env.PATH = '/ambient/changed/after-build'
    assert.deepEqual(runtime.sampleCommandCapability().environment, {
      PATH: '/manifest/pinned/bin',
    })

    writeFileSync(driverSource, 'tampered sample driver capability')
    assert.throws(
      () => runtime.sampleCommandCapability(),
      /driverSource differs from its runtime manifest capability/u,
      'a post-build command source mutation must fail before launch or guard acceptance',
    )
  } finally {
    if (previousPath === undefined) delete process.env.PATH
    else process.env.PATH = previousPath
    runtime?.dispose()
    rmSync(fixtureRoot, { recursive: true, force: true })
  }
}

async function buildEvidenceMustProveTreeEmpty() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-tree-proof-'))
  try {
    await assert.rejects(() => buildBrowsergateRuntime({
      repositoryRoot: root,
      suites: ['main'],
      platform: 'linux',
      outputParent,
      executeBuild: async ({ outputPath }) => {
        writeFileSync(outputPath, 'executable', { flag: 'wx' })
        return Object.freeze({ ...successfulExecution('', 'linux'), treeEmpty: false })
      },
      executePreflight: async ({ kind }) => successfulExecution(SELF_CHECK_BY_KIND[kind], 'linux'),
    }), /empty process tree/u)
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function preflightMustProveEmptyStderr() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-stderr-proof-'))
  try {
    await assert.rejects(() => buildBrowsergateRuntime({
      repositoryRoot: root,
      suites: ['main'],
      platform: 'linux',
      outputParent,
      executeBuild: async ({ outputPath }) => {
        writeFileSync(outputPath, 'executable', { flag: 'wx' })
        return successfulExecution('', 'linux')
      },
      executePreflight: async ({ kind }) => Object.freeze({
        ...successfulExecution(SELF_CHECK_BY_KIND[kind], 'linux'),
        stderr: 'warning\n',
      }),
    }), /unexpected stderr/u)
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function clippedOwnershipLeaseNeverLaunches() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-clipped-'))
  let launchCount = 0
  try {
    await assert.rejects(() => buildInvocationRuntime({
      suites: ['main'],
      platform: 'linux',
      outputParent,
      inheritedEnvironment: Object.freeze({ PATH: 'not-authority' }),
      resolveExecutable: () => process.execPath,
      deadlineAuthority: Object.freeze({
        grant: () => Object.freeze({
          outcome: 'authorized',
          classDeadlineMs: 100_000,
          timeoutMs: 99_999,
          remainingBudgetMs: 99_999,
        }),
      }),
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
      executeOwnedCommand: async () => {
        launchCount += 1
        return successfulExecution('')
      },
      createBootstrapOwner: async () => {
        launchCount += 1
        return fakeBootstrapOwner('linux')
      },
    }), /generated semantic verifier failed/u)
    assert.equal(launchCount, 0)
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function bootstrapClosesAfterRuntimeBuildFailure() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-bootstrap-failure-'))
  const bootstrapEvents = []
  try {
    await assert.rejects(() => buildInvocationRuntime({
      suites: ['main'],
      platform: 'win32',
      outputParent,
      inheritedEnvironment: Object.freeze({
        PATH: 'not-authority',
        SystemRoot: 'C:\\Windows',
      }),
      resolveExecutable: () => process.execPath,
      createBootstrapOwner: async () => fakeBootstrapOwner('win32', bootstrapEvents),
      deadlineAuthority: fullLeaseAuthority(),
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
      executeOwnedCommand: async (request) => {
        if (request.operationId === GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID) {
          return successfulExecution(successfulGeneratedSemanticRecord())
        }
        return Object.freeze({
          ...successfulExecution(''),
          processEvidence: Object.freeze({ terminal: 'exited', exitCode: 1 }),
        })
      },
    }), /runtime build windows-job/u)
    assert.deepEqual(bootstrapEvents, ['close'])
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function bootstrapFailureRemovesItsPrivateRoot() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-bootstrap-create-failure-'))
  try {
    await assert.rejects(() => createBootstrapProcessOwnerAuthority({
      repositoryRoot: root,
      outputParent,
      platform: 'win32',
      goExecutable: process.execPath,
      buildOwner: async () => { throw new Error('injected bootstrap build failure') },
    }), /injected bootstrap build failure/u)
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function bootstrapReceiptCannotClaimTreeEvidence() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-bootstrap-tree-claim-'))
  let closeCount = 0
  try {
    await assert.rejects(() => createBootstrapProcessOwnerAuthority({
      repositoryRoot: root,
      outputParent,
      platform: 'win32',
      goExecutable: process.execPath,
      buildOwner: async ({ outputPath }) => {
        writeFileSync(outputPath, 'bootstrap owner', { flag: 'wx' })
        return Object.freeze({
          receipt: Object.freeze({
            schemaVersion: BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION,
            kind: 'windows-job',
            platform: 'win32',
            process: Object.freeze({ terminal: 'exited', exitCode: 0, timedOut: false }),
            output: Object.freeze({
              path: outputPath,
              byteLength: 15,
              sha256: 'a'.repeat(64),
              treeEmpty: true,
            }),
          }),
          async assertLive() {},
          async close() { closeCount += 1 },
        })
      },
    }), /violates its authority contract/u)
    assert.equal(closeCount, 1)
    assert.deepEqual(readdirSync(outputParent), [])
  } finally {
    rmSync(outputParent, { recursive: true, force: true })
  }
}

async function buildFixture(platform) {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-fixture-'))
  const runtime = await buildBrowsergateRuntime({
    repositoryRoot: root,
    suites: ['main'],
    platform,
    outputParent,
    executeBuild: async ({ outputPath }) => {
      writeFileSync(outputPath, 'executable', { flag: 'wx' })
      return successfulExecution('', platform)
    },
    executePreflight: async ({ kind }) => successfulExecution(SELF_CHECK_BY_KIND[kind], platform),
  })
  return { outputParent, runtime }
}

function successfulExecution(stdout, platform = 'win32') {
  const ownershipEvidence = platform === 'win32'
    ? Object.freeze({
        supervisionOutcome: 'tree-empty',
        terminationReason: 'natural',
        activeProcessCount: 0,
        root: Object.freeze({ pid: 42, exitCode: 0 }),
        spawnFailure: null,
      })
    : Object.freeze({
        ownerPid: 41,
        rootPid: 42,
        rootStartTimeTicks: '100',
        inventoryScans: 2,
        maximumObservedDescendants: 1,
        quietInventoryCount: 1,
        controlOutcome: 'target-terminal',
        cleanupOutcome: 'completed',
        failureCode: '',
        failureMessage: '',
      })
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    timedOut: false,
    launched: true,
    treeEmpty: true,
    inputEvidence: Object.freeze({
      outcome: 'not-requested',
      failureCode: '',
      failureMessage: '',
    }),
    clientIoEvidence: Object.freeze({
      requestOutcome: 'delivered',
      rawInputOutcome: 'not-requested',
      controlOutcome: 'not-requested',
      outputOutcome: 'delivered',
      failureCode: '',
      failureMessage: '',
    }),
    ownershipEvidence,
    stdout,
    stderr: '',
  })
}

function fullLeaseAuthority() {
  return Object.freeze({
    grant: () => Object.freeze({
      outcome: 'authorized',
      classDeadlineMs: 600_000,
      timeoutMs: 600_000,
      remainingBudgetMs: 600_000,
    }),
  })
}

function errorWithUnreadableMessage() {
  const cause = new Error('hidden')
  Object.defineProperty(cause, 'message', {
    get() {
      throw new Error('message accessor must not control preflight settlement')
    },
  })
  return cause
}

function hostileProxyCause() {
  return new Proxy(new Error('hidden'), {
    getPrototypeOf() {
      throw new Error('prototype trap must not control preflight settlement')
    },
  })
}

function fakeBootstrapOwner(platform, events = []) {
  const kind = platform === 'win32' ? 'windows-job' : 'linux-process-owner'
  return Object.freeze({
    artifact: Object.freeze({
      kind,
      path: process.execPath,
      byteLength: 1,
      sha256: 'b'.repeat(64),
    }),
    async assertLive() { events.push('assert-live') },
    async close() { events.push('close') },
  })
}
