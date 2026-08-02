import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { createHash } from 'node:crypto'
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
  RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
  resolveHostExecutable,
} from '../../process/runtime-command-owner.mjs'
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
import { createOwnedTraceJournal } from '../../owned-trace-journal.mjs'

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
  'test-process-owner':
    '{"schema_version":"windshare.process-owner-self-check/v1","component":"testprocessowner","milestone":"self_check","outcome":"ready"}\n',
})

verifyWindowsNativeExecutableResolution()
await exactOwnedExecutionCounts()
await generatedSemanticFailureMatrixStopsNativeBuilds()
await hostileGeneratedSemanticFailuresSettleOnce()
await hostileRuntimeCommandBoundariesSettleExactlyOnce()
await manifestRejectsPlatformAndRelativePath()
await sampleCommandManifestSelectsEnvironmentAndDynamicPaths()
await buildEvidenceMustProveTreeEmpty()
await preflightMustProveEmptyStderr()
await clippedOwnershipLeaseNeverLaunches()
await bootstrapClosesAfterRuntimeBuildFailure()
await bootstrapFailureRemovesItsPrivateRoot()

function verifyWindowsNativeExecutableResolution() {
  const commandRoot = mkdtempSync(resolve(tmpdir(), 'windshare-host-executable-'))
  try {
    writeFileSync(resolve(commandRoot, 'pnpm.cmd'), '@exit /b 0\r\n')
    assert.throws(
      () => resolveHostExecutable('pnpm', {
        platform: 'win32',
        environment: { Path: commandRoot },
      }),
      /cannot resolve host executable pnpm/u,
      'a shell command shim must not resolve as a native process executable',
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

async function captureRuntimeFailure(operation, pattern, label = 'runtime failure') {
  try {
    await operation()
  } catch (cause) {
    assert(cause instanceof Error, label)
    assert.match(cause.message, pattern, label)
    assert.notEqual(cause.generatedSemanticTraces, undefined, label)
    return cause
  }
  assert.fail(`${label} did not reject`)
}

async function exactOwnedExecutionCounts() {
  const outputParent = mkdtempSync(join(tmpdir(), 'windshare-runtime-counts-'))
  const calls = []
  let resolveCount = 0
  const bootstrapEvents = []
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
      executeOwnedCommand: (request) => {
        calls.push(request)
        if (request.operationId === GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID) {
          return runtimeCommandChannel(
            successfulExecution(successfulGeneratedSemanticRecord()),
            request.operationId,
            request.platform,
          )
        }
        if (request.operationId.startsWith('browser-runtime-build-')) {
          const outputIndex = request.command.arguments.indexOf('-o')
          assert.notEqual(outputIndex, -1)
          writeFileSync(request.command.arguments[outputIndex + 1], request.operationId, { flag: 'wx' })
          return runtimeCommandChannel(successfulExecution(''), request.operationId, request.platform)
        }
        const kind = request.operationId.slice('browser-runtime-preflight-'.length)
        return runtimeCommandChannel(
          successfulExecution(SELF_CHECK_BY_KIND[kind]),
          request.operationId,
          request.platform,
        )
      },
    })
    const generatedSemanticEvents = runtime.generatedSemanticTraces.events
    assert.equal(resolveCount, 1)
    assert.deepEqual(runtime.manifest.artifacts.map(({ kind }) => kind), [
      'test-process-owner',
      'topology-materializer',
      'artifact-publisher',
      'pion-server',
    ])
    const processOwnerCapability = runtime.artifactCapability('test-process-owner')
    assert.deepEqual(Object.keys(processOwnerCapability), ['path'])
    assert.equal(
      processOwnerCapability.path,
      runtime.manifest.artifacts.find(({ kind }) => kind === 'test-process-owner').path,
    )
    assert.equal(calls.length, 9)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('generated-semantic')).length, 1)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('-build-')).length, 4)
    assert.equal(calls.filter(({ operationId }) => operationId.includes('-preflight-')).length, 4)
    assert.equal(calls[0].command.executable, process.execPath)
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
    assert.equal(calls[0].processOwner.path, process.execPath)
    assert.equal(calls[1].processOwner.path, process.execPath)
    for (const { processOwner } of calls) {
      assert.deepEqual(Object.keys(processOwner), ['path'])
    }
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
    assert.equal(generatedSemanticEvents[2].context.cleanupOutcome, 'completed')
    assert.equal(generatedSemanticEvents[2].context.treeEmpty, true)
    assert(Number.isSafeInteger(generatedSemanticEvents[2].context.elapsedMs))
    assert.deepEqual(bootstrapEvents, ['assert-live', 'assert-live', 'close'])
    const manifestPath = runtime.manifestPath
    runtime.dispose()
    const cleanup = disposeBrowsergateRuntime({
      manifestPath,
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
      treeEmpty: false,
      processEvidence: Object.freeze({
        terminal: 'spawn-failed',
        errorCode: 'ENOENT',
        errorMessage: 'injected spawn failure',
      }),
    })],
    ['deadline-failure', () => Object.freeze({
      ...successfulExecution(successfulRecord),
      ownershipEvidence: Object.freeze({
        ...successfulExecution(successfulRecord).ownershipEvidence,
        terminationReason: 'deadline',
      }),
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
    let nativeBuildCount = 0
    try {
      const failure = await captureRuntimeFailure(
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
          executeOwnedCommand: (request) => {
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
              return runtimeCommandChannel(execution(), request.operationId, request.platform)
            }
            nativeBuildCount += 1
            return runtimeCommandChannel(successfulExecution(''), request.operationId, request.platform)
          },
        }),
        /generated semantic verifier failed before runtime batch build/u,
        name,
      )
      const events = failure.generatedSemanticTraces.events
      assert.deepEqual(operations, [GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID], name)
      assert.equal(nativeBuildCount, 0, name)
      assert.deepEqual(events.map(({ operationId, milestone }) => ({ operationId, milestone })), [
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
        { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
      ], name)
      assert.equal(events[1].context.mode, 'verify', name)
      assert.equal(events[1].context.outcome, 'failed', name)
      const expectedExecution = execution()
      const ownershipSettled = expectedExecution.treeEmpty === true &&
        expectedExecution.cleanupOutcome === 'completed'
      assert.equal(
        events[1].context.cleanupOutcome,
        ownershipSettled ? expectedExecution.cleanupOutcome : 'not-observed',
        name,
      )
      assert.equal(events[1].context.treeEmpty, ownershipSettled && expectedExecution.treeEmpty, name)
      assert.equal(typeof events[1].context.failureCode, 'string', name)
      assert(Number.isSafeInteger(events[1].context.elapsedMs), name)
      if (name === 'malformed-record') {
        const outputEvidence = events[1].context.outputEvidence
        assert.equal(outputEvidence.stdout.stream, 'stdout')
        assert.equal(outputEvidence.stderr.stream, 'stderr')
        assert.deepEqual(outputEvidence.stdout.segments.map(({ sequence, offset }) => ({ sequence, offset })), [
          { sequence: 0, offset: 0 },
        ])
        assert.equal(
          Buffer.from(outputEvidence.stdout.segments[0].base64, 'base64').toString('utf8'),
          '{not-json}\n',
        )
      }
      if (name === 'typed-failure') {
        assert.equal(events[1].context.failureCode, 'reported-failure')
        assert.deepEqual(JSON.parse(JSON.stringify(events[1].context.failures)), [{
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
  let launchCount = 0
  try {
    const failure = await captureRuntimeFailure(() => buildInvocationRuntime({
      suites: ['main'],
      platform: 'linux',
      outputParent,
      inheritedEnvironment: Object.freeze({ PATH: '/ambient/not-authority' }),
      resolveExecutable: () => process.execPath,
      createBootstrapOwner: async () => { throw new Error('injected bootstrap owner failure') },
      deadlineAuthority: fullLeaseAuthority(),
      buildLeaseId: 'runtime/batch-build',
      preflightLeaseId: 'runtime/manifest-preflight',
      executeOwnedCommand: () => {
        launchCount += 1
        return successfulExecution(successfulRecord)
      },
    }), /generated semantic verifier failed before runtime batch build/u)
    const events = failure.generatedSemanticTraces.events
    assert.equal(launchCount, 0)
    assert.deepEqual(events.map(({ operationId, milestone }) => ({ operationId, milestone })), [
      { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'started' },
      { operationId: GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID, milestone: 'settled' },
    ])
    assert.equal(events[1].context.failureCode, 'unexpected-preflight-failure')
    assert.equal(events[1].context.cleanupOutcome, 'not-observed')
    assert.equal(events[1].context.treeEmpty, false)
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
    let launchCount = 0
    try {
      const failure = await captureRuntimeFailure(() => buildInvocationRuntime({
        suites: ['main'],
        platform: 'linux',
        outputParent,
        inheritedEnvironment: Object.freeze({ PATH: '/ambient/not-authority' }),
        resolveExecutable: () => process.execPath,
        createBootstrapOwner: async () => { throw hostileCause },
        deadlineAuthority: fullLeaseAuthority(),
        buildLeaseId: 'runtime/batch-build',
        preflightLeaseId: 'runtime/manifest-preflight',
        executeOwnedCommand: () => {
          launchCount += 1
          throw new Error('hostile bootstrap failure must not launch a verifier')
        },
      }), /generated semantic verifier failed before runtime batch build/u)
      const events = failure.generatedSemanticTraces.events
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

async function hostileRuntimeCommandBoundariesSettleExactlyOnce() {
  let rejectionTraceGetterCalls = 0
  let fulfilledEvidenceGetterCalls = 0
  const decoratedRejection = new Error('injected owner rejection')
  const retainedOperationTraces = Object.freeze({ retained: true })
  Object.defineProperty(decoratedRejection, 'operationTraces', {
    value: retainedOperationTraces,
    enumerable: true,
    configurable: false,
    writable: false,
  })
  Object.defineProperty(decoratedRejection, 'traces', {
    get() {
      rejectionTraceGetterCalls += 1
      throw new Error('arbitrary rejection trace getter executed')
    },
    enumerable: true,
    configurable: false,
  })

  const { proxy: revokedRejection, revoke } = Proxy.revocable(new Error('hidden'), {})
  revoke()
  const fulfilledBase = successfulExecution(successfulGeneratedSemanticRecord(), 'linux')
  const fulfilledSettlement = runtimeCommandSettlement(fulfilledBase)
  const hostileFulfilled = { ...fulfilledBase }
  Object.defineProperty(hostileFulfilled, 'ownershipEvidence', {
    get() {
      fulfilledEvidenceGetterCalls += 1
      throw new Error('fulfilled ownership evidence getter executed')
    },
    enumerable: true,
    configurable: false,
  })

  const cases = [
    Object.freeze({
      name: 'synchronous-revoked-rejection',
      expectedFailureKind: 'runtime-command-contract-failed',
      expectedRuntimeOutcome: null,
      executeOwnedCommand() {
        throw revokedRejection
      },
    }),
    Object.freeze({
      name: 'decorated-result-rejection',
      expectedFailureKind: 'runtime-command-failed',
      expectedRuntimeOutcome: 'execution-rejected',
      executeOwnedCommand(request) {
        const traces = runtimeCommandTraceFixture({
          operationId: request.operationId,
          platform: request.platform,
          outcomeCode: 'execution-rejected',
          settlement: emptyRuntimeCommandSettlement(),
        })
        return Object.freeze({
          result: Promise.reject(decoratedRejection),
          traces: Object.freeze({ snapshot: () => traces }),
        })
      },
    }),
    Object.freeze({
      name: 'fulfilled-nonconfig-accessor',
      expectedFailureKind: 'runtime-command-contract-failed',
      expectedRuntimeOutcome: 'succeeded',
      executeOwnedCommand(request) {
        const traces = runtimeCommandTraceFixture({
          operationId: request.operationId,
          platform: request.platform,
          outcomeCode: 'succeeded',
          settlement: fulfilledSettlement,
        })
        return Object.freeze({
          result: Promise.resolve(hostileFulfilled),
          traces: Object.freeze({ snapshot: () => traces }),
        })
      },
    }),
  ]

  for (const testCase of cases) {
    const outputParent = mkdtempSync(join(tmpdir(), `windshare-runtime-hostile-${testCase.name}-`))
    try {
      const failure = await captureRuntimeFailure(() => buildInvocationRuntime({
        suites: ['main'],
        platform: 'linux',
        outputParent,
        inheritedEnvironment: Object.freeze({ PATH: '/ambient/not-authority' }),
        resolveExecutable: () => process.execPath,
        createBootstrapOwner: async () => fakeBootstrapOwner('linux'),
        deadlineAuthority: fullLeaseAuthority(),
        buildLeaseId: 'runtime/batch-build',
        preflightLeaseId: 'runtime/manifest-preflight',
        executeOwnedCommand: testCase.executeOwnedCommand,
      }), /generated semantic verifier failed before runtime batch build/u, testCase.name)
      const ownedFailure = failure.cause.cause
      assert(ownedFailure instanceof Error, testCase.name)
      assert.equal(ownedFailure.message, 'owned runtime operation failed', testCase.name)
      assert.deepEqual(
        ownedFailure.operationTraces.events.map(({ milestone }) => milestone),
        ['started', 'failed'],
        testCase.name,
      )
      assert.equal(ownedFailure.operationTraces.events.length, 2, testCase.name)
      assert.equal(ownedFailure.operationTraces.completed, true, testCase.name)
      assert.equal(
        ownedFailure.operationTraces.events.at(-1).context.failureKind,
        testCase.expectedFailureKind,
        testCase.name,
      )
      assert.equal(
        Object.getOwnPropertyDescriptor(ownedFailure, 'operationTraces').configurable,
        false,
        testCase.name,
      )
      if (testCase.expectedRuntimeOutcome === null) {
        assert.equal(ownedFailure.runtimeCommandTraces, null, testCase.name)
      } else {
        assert.equal(ownedFailure.runtimeCommandTraces.events.length, 2, testCase.name)
        assert.equal(
          ownedFailure.runtimeCommandTraces.events.at(-1).outcomeCode,
          testCase.expectedRuntimeOutcome,
          testCase.name,
        )
      }
      assert.deepEqual(readdirSync(outputParent), [], testCase.name)
    } finally {
      rmSync(outputParent, { recursive: true, force: true })
    }
  }

  assert.equal(decoratedRejection.operationTraces, retainedOperationTraces)
  assert.equal(rejectionTraceGetterCalls, 0)
  assert.equal(fulfilledEvidenceGetterCalls, 0)
}

async function manifestRejectsPlatformAndRelativePath() {
  const fixture = await buildFixture('linux')
  try {
    assert.throws(() => loadBrowsergateRuntime({
      manifestPath: fixture.runtime.manifestPath,
      expectedPlatform: 'win32',
    }), /platform differs/u)
    assert.throws(() => loadBrowsergateRuntime({
      manifestPath: basename(fixture.runtime.manifestPath),
      expectedPlatform: 'linux',
    }), /absolute and canonical/u)
  } finally {
    fixture.runtime.dispose()
    rmSync(fixture.outputParent, { recursive: true, force: true })
  }
}

async function sampleCommandManifestSelectsEnvironmentAndDynamicPaths() {
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
  const playwrightRunner = join(
    repositoryRoot,
    'web',
    'scripts',
    'browser-evidence',
    'playwright-owned-runner.mjs',
  )
  const nodeExecutable = join(fixtureRoot, 'node.exe')
  mkdirSync(join(driverSource, '..'), { recursive: true })
  mkdirSync(join(playwrightCli, '..'), { recursive: true })
  mkdirSync(outputParent, { recursive: true })
  writeFileSync(driverSource, 'sample driver capability')
  writeFileSync(playwrightCli, 'playwright capability')
  writeFileSync(playwrightRunner, 'playwright runner capability')
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
        PATH: '/manifest/selected/bin',
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
      PATH: '/manifest/selected/bin',
    })
    process.env.PATH = '/ambient/changed/after-build'
    assert.deepEqual(runtime.sampleCommandCapability().environment, {
      PATH: '/manifest/selected/bin',
    })

    writeFileSync(driverSource, 'tampered sample driver capability')
    assert.equal(
      runtime.sampleCommandCapability().driverSource,
      driverSource,
      'current-worktree helper sources remain ordinary dynamic test inputs',
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
      executeOwnedCommand: () => {
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
      executeOwnedCommand: (request) => {
        if (request.operationId === GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID) {
          return runtimeCommandChannel(
            successfulExecution(successfulGeneratedSemanticRecord()),
            request.operationId,
            request.platform,
          )
        }
        return runtimeCommandChannel(Object.freeze({
          ...successfulExecution(''),
          processEvidence: Object.freeze({ terminal: 'exited', exitCode: 1 }),
        }), request.operationId, request.platform)
      },
    }), /runtime build test-process-owner/u)
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
  const backend = platform === 'win32' ? 'windows_job' : 'linux_subreaper'
  const ownershipEvidence = Object.freeze({
    kind: 'test-process-owner',
    backend,
    terminationReason: 'natural',
    platform: Object.freeze({ kind: backend }),
  })
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    treeEmpty: true,
    cleanupOutcome: 'completed',
    inputEvidence: Object.freeze({
      outcome: 'not_requested',
      failureCode: '',
      failureMessage: '',
    }),
    ownershipEvidence,
    stdout,
    stderr: '',
    traces: completeTraceSnapshot(),
    runtimeCommandTraces: completeTraceSnapshot(),
  })
}

function runtimeCommandChannel(execution, operationId, platform) {
  const result = Object.freeze({
    processEvidence: execution.processEvidence,
    treeEmpty: execution.treeEmpty,
    cleanupOutcome: execution.cleanupOutcome,
    inputEvidence: execution.inputEvidence,
    ownershipEvidence: execution.ownershipEvidence,
    stdout: execution.stdout,
    stderr: execution.stderr,
  })
  const settlement = runtimeCommandSettlement(result)
  const ownershipSettled = result.treeEmpty === true && result.cleanupOutcome === 'completed'
  const traces = runtimeCommandTraceFixture({
    operationId,
    platform,
    outcomeCode: ownershipSettled ? 'succeeded' : 'ownership-rejected',
    settlement,
  })
  return Object.freeze({
    result: ownershipSettled
      ? Promise.resolve(result)
      : Promise.reject(new Error('runtime command ownership did not settle an empty tree')),
    traces: Object.freeze({ snapshot: () => traces }),
  })
}

function runtimeCommandSettlement(execution) {
  return Object.freeze({
    processEvidence: execution.processEvidence,
    inputEvidence: execution.inputEvidence,
    ownerFailure: null,
    treeEmpty: execution.treeEmpty,
    cleanupOutcome: execution.cleanupOutcome,
    ownershipEvidence: execution.ownershipEvidence,
    transportOutcome: 'completed',
    controlOutcome: 'completed',
    transportEvidence: null,
  })
}

function emptyRuntimeCommandSettlement() {
  return Object.freeze({
    processEvidence: null,
    inputEvidence: null,
    ownerFailure: null,
    treeEmpty: null,
    cleanupOutcome: 'not-observed',
    ownershipEvidence: null,
    transportOutcome: 'not-observed',
    controlOutcome: 'not-observed',
    transportEvidence: null,
  })
}

function runtimeCommandTraceFixture({ operationId, platform, outcomeCode, settlement }) {
  const journal = createOwnedTraceJournal({
    label: 'runtime command build fixture trace',
    maximumEvents: 2,
    maximumBytes: 256 * 1024,
  })
  assert.equal(journal.append(Object.freeze({
    schemaVersion: RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
    sequence: 0,
    milestone: 'runtime-command-started',
    outcomeCode: 'started',
    context: Object.freeze({ operationId, platform }),
  })), true)
  assert.equal(journal.append(Object.freeze({
    schemaVersion: RUNTIME_COMMAND_TRACE_SCHEMA_VERSION,
    sequence: 1,
    milestone: 'runtime-command-terminal',
    outcomeCode,
    context: Object.freeze({ operationId, platform, settlement }),
  })), true)
  journal.finish()
  return journal.view.snapshot()
}

function completeTraceSnapshot(events = Object.freeze([])) {
  const capturedBytes = events.reduce(
    (total, event) => total + Buffer.byteLength(JSON.stringify(event), 'utf8'),
    0,
  )
  return Object.freeze({
    events,
    observedEvents: events.length,
    capturedEvents: events.length,
    observedBytes: capturedBytes,
    capturedBytes,
    truncated: false,
    completed: true,
    failure: null,
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
  const kind = 'test-process-owner'
  return Object.freeze({
    artifact: Object.freeze({
      kind,
      path: process.execPath,
    }),
    async assertLive() { events.push('assert-live') },
    async close() { events.push('close') },
  })
}
