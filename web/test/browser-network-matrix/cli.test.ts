import { access, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { parseNetworkMatrixAggregateJson } from '../../scripts/browser-network-matrix/aggregate.ts'
import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import { aggregateNetworkMatrixFiles } from '../../scripts/browser-network-matrix/cli/aggregate-files.ts'
import type { ImmutableTextFilePublisher } from '../../scripts/browser-network-matrix/cli/immutable-file-publication.ts'
import type { NetworkMatrixPublisherHelperAuthority } from '../../scripts/browser-network-matrix/cli/publisher-helper.ts'
import type { NetworkMatrixRuntimeBootstrap } from '../../scripts/browser-network-matrix/cli/execute.ts'
import {
  browserNetworkMatrixCli,
  NetworkMatrixRuntimeNotWiredError,
} from '../../scripts/browser-network-matrix/cli/main.ts'
import { completedOwnedOperation } from '../../scripts/browser-network-matrix/owned-operation.ts'
import {
  canonicalNetworkRunResultJson,
  parseNetworkRunResultJson,
} from '../../scripts/browser-network-matrix/result.ts'
import type {
  NetworkMatrixSampleExecutionContext,
} from '../../scripts/browser-network-matrix/runner.ts'
import type {
  NetworkMatrixAuthorityPreparationContext,
  PreparedNetworkMatrixAuthority,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import {
  loadRegistry,
  makeRun,
  MANIFEST_PATH,
  matchedAttemptEvidence,
  rawAttestation,
} from './fixtures.ts'

const temporaryRoots: string[] = []

afterEach(async () => {
  vi.unstubAllEnvs()
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('local browser network matrix CLI substrate', () => {
  it('fails closed when invoked without an explicit command', async () => {
    await expect(browserNetworkMatrixCli([])).rejects.toThrow(
      /execute --mode scheduled/u,
    )
  })

  it('aggregates exactly one explicit canonical scheduled run', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'scheduled.run.json')
    const outputPath = join(root, 'scheduled.aggregate.json')
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'scheduled', { runId: 'scheduled-local-run' }), registry),
      'utf8',
    )

    const result = await aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [runPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })

    expect(result.aggregate).toMatchObject({
      evidenceOutcome: 'complete',
      runs: [{ executionMode: 'scheduled', runId: 'scheduled-local-run' }],
    })
    const parsedRun = parseNetworkRunResultJson(await readFile(runPath, 'utf8'), registry)
    expect(parseNetworkMatrixAggregateJson(await readFile(outputPath, 'utf8'), registry, [parsedRun]))
      .toEqual(result.aggregate)
  })

  it('publishes an immutable scheduled aggregate', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const scheduledPath = join(root, 'scheduled.run.json')
    const outputPath = join(root, 'scheduled.aggregate.json')
    await writeFile(
      scheduledPath,
      canonicalNetworkRunResultJson(
        makeRun(registry, 'scheduled', { runId: 'scheduled-local-run' }),
        registry,
      ),
      'utf8',
    )

    const result = await aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [scheduledPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })
    expect(result.aggregate.evidenceOutcome).toBe('complete')
    expect(result.aggregate.runs.map(({ executionMode }) => executionMode))
      .toEqual(['scheduled'])

    await expect(aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [scheduledPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })).rejects.toThrow(/already exists/u)
    expect(await readFile(outputPath, 'utf8')).toBe(result.publication.encoded)
  })

  it('rejects incorrect run cardinality or non-canonical input before publication', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'scheduled.run.json')
    const outputPath = join(root, 'aggregate.json')
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'scheduled'), registry),
      'utf8',
    )
    await expect(aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [runPath, runPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })).rejects.toThrow(/requires exactly one scheduled run file/u)

    await writeFile(runPath, `\n${await readFile(runPath, 'utf8')}`, 'utf8')
    await expect(aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [runPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })).rejects.toThrow(/canonical minified JSON/u)
    await expect(access(outputPath)).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('refuses execute before registry loading when concrete local composition is absent', async () => {
    const loadRegistry = vi.fn()
    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'scheduled',
      '--run-id', 'unwired-local-run',
      '--manifest', MANIFEST_PATH,
      '--output-root', 'unused-output',
      '--helper-manifest', 'unused-helper-manifest',
      '--publisher-helper', 'unused-publisher-helper',
    ], { loadRegistry, platform: 'linux' })).rejects.toBeInstanceOf(NetworkMatrixRuntimeNotWiredError)
    expect(loadRegistry).not.toHaveBeenCalled()
  })

  it('opens publication authority before registry or runtime can fabricate a run', async () => {
    const root = await temporaryRoot()
    const loadRegistry = vi.fn()
    const bootstrap = vi.fn(() => { throw new Error('runtime must remain unreachable') })
    const openPublisherHelper = vi.fn(async () =>
      Promise.reject(new Error('publisher helper open failed')))

    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'scheduled',
      '--run-id', 'publisher-bootstrap-failure',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'must-not-exist'),
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher-helper.exe'),
      '--process-owner', join(root, 'testprocessowner'),
    ], {
      platform: 'linux',
      loadRegistry,
      runtimeBootstrap: { bootstrap },
      openPublisherHelper,
    })).rejects.toThrow(/publisher helper open failed/u)
    expect(openPublisherHelper).toHaveBeenCalledOnce()
    expect(openPublisherHelper).toHaveBeenCalledWith({
      helperManifestPath: join(root, 'helper-manifest.json'),
      platform: 'linux',
      publisherHelperPath: join(root, 'publisher-helper.exe'),
      processOwnerPath: join(root, 'testprocessowner'),
    })
    expect(loadRegistry).not.toHaveBeenCalled()
    expect(bootstrap).not.toHaveBeenCalled()
    await expect(access(join(root, 'must-not-exist'))).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('publishes settled execution and runner journals exactly once after cleanup', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const summaries: string[] = []
    const encodedTraces: string[] = []
    const stderr = vi.spyOn(process.stderr, 'write').mockImplementation((chunk) => {
      encodedTraces.push(String(chunk))
      return true
    })
    try {
      await expect(browserNetworkMatrixCli([
        'execute',
        '--mode', 'scheduled',
        '--run-id', 'cli-owned-trace-publication',
        '--manifest', MANIFEST_PATH,
        '--output-root', join(root, 'output'),
        '--helper-manifest', join(root, 'helper-manifest.json'),
        '--publisher-helper', join(root, 'publisher-helper'),
        '--process-owner', join(root, 'testprocessowner'),
      ], {
        platform: 'linux',
        loadRegistry: async () => registry,
        runtimeBootstrap: successfulCliBootstrap(),
        openPublisherHelper: successfulExecutionPublisherHelperOpener(),
        writeSummary: (encoded) => summaries.push(encoded),
      })).resolves.toBe(0)
    } finally {
      stderr.mockRestore()
    }

    const traces = encodedTraces.map((encoded) => JSON.parse(encoded) as {
      readonly component: string
      readonly milestone: string
    })
    expect(traces.filter(({ milestone }) => milestone === 'execution-started')).toHaveLength(1)
    expect(traces.filter(({ milestone }) => milestone === 'run-started')).toHaveLength(1)
    expect(traces.filter(({ milestone }) => milestone === 'sample-started')).toHaveLength(45)
    expect(traces.filter(({ milestone }) => milestone === 'sample-terminal')).toHaveLength(45)
    expect(new Set(encodedTraces).size).toBe(encodedTraces.length)
    const components = traces.map(({ component }) => component)
    const firstRunner = components.indexOf('browser-network-matrix-runner')
    expect(firstRunner).toBeGreaterThan(0)
    expect(components.slice(0, firstRunner).every(
      (component) => component === 'browser-network-matrix-execute',
    )).toBe(true)
    expect(components.slice(firstRunner).every(
      (component) => component === 'browser-network-matrix-runner',
    )).toBe(true)
    expect(JSON.parse(summaries[0] as string)).toMatchObject({
      acceptanceOutcome: 'passed',
      command: 'execute',
      commandOutcome: 'completed',
    })
  })

  it('exposes explicit aggregate inputs without requiring execution composition', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'scheduled.run.json')
    const outputPath = join(root, 'scheduled.aggregate.json')
    const summaries: string[] = []
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'scheduled'), registry),
      'utf8',
    )

    await expect(browserNetworkMatrixCli([
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--run', runPath,
      '--output', outputPath,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher-helper.exe'),
      '--process-owner', join(root, 'testprocessowner'),
    ], {
      platform: 'linux',
      openPublisherHelper: testPublisherHelperOpener(),
      writeSummary: (encoded) => summaries.push(encoded),
    })).resolves.toBe(0)

    expect(JSON.parse(summaries[0] as string)).toMatchObject({
      command: 'aggregate',
      modes: ['scheduled'],
      evidenceOutcome: 'complete',
      outputPath,
    })
    await expect(browserNetworkMatrixCli([
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--input', runPath,
      '--output', join(root, 'retired-input-flag.json'),
    ])).rejects.toThrow(/unknown browser network matrix option --input/u)
  })

  it('forwards all three Windows helper authorities without aliases or path inference', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'scheduled.run.json')
    const outputPath = join(root, 'scheduled.aggregate.json')
    const helperManifestPath = join(root, 'helper-manifest.json')
    const publisherHelperPath = join(root, 'browsermatrixpublish.exe')
    const processOwnerPath = join(root, 'testprocessowner.exe')
    const openPublisherHelper = vi.fn(testPublisherHelperOpener())
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'scheduled'), registry),
      'utf8',
    )

    await expect(browserNetworkMatrixCli([
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--run', runPath,
      '--output', outputPath,
      '--helper-manifest', helperManifestPath,
      '--publisher-helper', publisherHelperPath,
      '--process-owner', processOwnerPath,
    ], {
      platform: 'win32',
      openPublisherHelper,
      writeSummary: () => undefined,
    })).resolves.toBe(0)
    expect(openPublisherHelper).toHaveBeenCalledWith({
      helperManifestPath,
      platform: 'win32',
      publisherHelperPath,
      processOwnerPath,
    })
  })

  it('requires each helper authority exactly once on every supported platform', async () => {
    const root = await temporaryRoot()
    const base = [
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--run', join(root, 'run.json'),
      '--output', join(root, 'aggregate.json'),
    ] as const
    const openPublisherHelper = vi.fn(testPublisherHelperOpener())

    await expect(browserNetworkMatrixCli([
      ...base,
      '--publisher-helper', join(root, 'publisher'),
      '--process-owner', join(root, 'testprocessowner'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --helper-manifest must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--process-owner', join(root, 'testprocessowner'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --publisher-helper must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --process-owner must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'one.json'),
      '--helper-manifest', join(root, 'two.json'),
      '--publisher-helper', join(root, 'publisher'),
      '--process-owner', join(root, 'testprocessowner'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --helper-manifest must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'one-publisher'),
      '--publisher-helper', join(root, 'two-publisher'),
      '--process-owner', join(root, 'testprocessowner'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --publisher-helper must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher.exe'),
      '--process-owner', join(root, 'one-testprocessowner.exe'),
      '--process-owner', join(root, 'two-testprocessowner.exe'),
    ], { platform: 'win32', openPublisherHelper })).rejects.toThrow(
      /option --process-owner must appear exactly once/u,
    )
    expect(openPublisherHelper).not.toHaveBeenCalled()
  })

  it('does not discover omitted helper authority from aliases, environment, PATH, or adjacent names', async () => {
    const root = await temporaryRoot()
    const openPublisherHelper = vi.fn(testPublisherHelperOpener())
    vi.stubEnv('WINDSHARE_HELPER_MANIFEST', join(root, 'helper-manifest.json'))
    vi.stubEnv('WINDSHARE_PUBLISHER_HELPER', join(root, 'browsermatrixpublish'))
    vi.stubEnv('PATH', root)
    await Promise.all([
      writeFile(join(root, 'helper-manifest.json'), '{}\n', 'utf8'),
      writeFile(join(root, 'browsermatrixpublish'), 'adjacent helper', 'utf8'),
    ])

    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'scheduled',
      '--run-id', 'no-ambient-helper',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
      '--process-owner', join(root, 'testprocessowner'),
      '--helper', join(root, 'browsermatrixpublish'),
    ], {
      platform: 'linux',
      runtimeBootstrap: { bootstrap: vi.fn() },
      openPublisherHelper,
    })).rejects.toThrow(/unknown browser network matrix option --helper/u)
    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'scheduled',
      '--run-id', 'no-ambient-helper',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
      '--process-owner', join(root, 'testprocessowner'),
    ], {
      platform: 'linux',
      runtimeBootstrap: { bootstrap: vi.fn() },
      openPublisherHelper,
    })).rejects.toThrow(/option --helper-manifest must appear exactly once/u)
    expect(openPublisherHelper).not.toHaveBeenCalled()
  })

  it('validates the helper manifest before registry or runtime bootstrap', async () => {
    const root = await temporaryRoot()
    const helperManifestPath = join(root, 'helper-manifest.json')
    const publisherHelperPath = join(root, 'browsermatrixpublish.exe')
    const processOwnerPath = join(root, 'testprocessowner.exe')
    await Promise.all([
      writeFile(publisherHelperPath, 'publisher bytes', 'utf8'),
      writeFile(processOwnerPath, 'test process owner bytes', 'utf8'),
    ])
    await writeFile(helperManifestPath, `${JSON.stringify({
      schemaVersion: 'windshare.browser-network-matrix.helper-build/unsupported',
      platform: 'win32',
      architecture: runtimeGoArchitecture(),
      helpers: [
        { role: 'artifact-publisher', path: publisherHelperPath },
        {
          role: 'test-process-owner',
          path: processOwnerPath,
        },
      ],
    })}\n`, 'utf8')
    const loadRegistry = vi.fn()
    const bootstrap = vi.fn()

    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'scheduled',
      '--run-id', 'manifest-schema-failure',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
      '--helper-manifest', helperManifestPath,
      '--publisher-helper', publisherHelperPath,
      '--process-owner', processOwnerPath,
    ], {
      platform: 'win32',
      loadRegistry,
      runtimeBootstrap: { bootstrap },
    })).rejects.toThrow(/schema version/u)
    expect(loadRegistry).not.toHaveBeenCalled()
    expect(bootstrap).not.toHaveBeenCalled()
    await expect(access(join(root, 'output'))).rejects.toMatchObject({ code: 'ENOENT' })
  })
})

async function temporaryRoot(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-network-matrix-cli-'))
  temporaryRoots.push(root)
  return root
}

function testAggregatePublisher(): ImmutableTextFilePublisher {
  return {
    async publish(path, encoded) {
      await writeFile(path, encoded, { encoding: 'utf8', flag: 'wx' })
      return Object.freeze({ path, encoded })
    },
  }
}

function testPublisherHelperOpener(): () => Promise<NetworkMatrixPublisherHelperAuthority> {
  return async () => Object.freeze({
    artifactPublisher: Object.freeze({
      publish: () => Promise.reject(new Error('artifact publisher is unused in aggregate test')),
    }),
    aggregatePublisher: testAggregatePublisher(),
    close: () => Promise.resolve(),
  })
}

function successfulCliBootstrap(): NetworkMatrixRuntimeBootstrap {
  return Object.freeze({
    bootstrap(context) {
      const authorities = Object.freeze({
        prepare(authorityContext: NetworkMatrixAuthorityPreparationContext) {
          const attestation = parseNetworkRuntimeAttestation(
            rawAttestation(
              authorityContext.registry,
              authorityContext.runId,
              authorityContext.profile.profileId,
              'satisfied',
            ),
            {
              manifest: authorityContext.registry.manifest,
              manifestSha256: authorityContext.registry.manifestSha256,
              runId: authorityContext.runId,
            },
          )
          const prepared: PreparedNetworkMatrixAuthority = Object.freeze({
            attestation,
            execution: Object.freeze({
              profileId: authorityContext.profile.profileId,
              runtimeKind: 'external-fixture',
            }),
            close: () => completedOwnedOperation(undefined),
            forceTerminateAndWait: () => Promise.resolve(),
          })
          return completedOwnedOperation(prepared)
        },
      })
      const samples = Object.freeze({
        execute(sampleContext: NetworkMatrixSampleExecutionContext) {
          const processInstanceId = [
            'cli-sample',
            sampleContext.identity.profileId,
            sampleContext.identity.browser,
            sampleContext.identity.sampleOrdinal,
          ].join('-')
          return completedOwnedOperation(Object.freeze({
            processInstanceId,
            observation: Object.freeze({
              sampleOutcome: 'observed' as const,
              attemptEvidence: matchedAttemptEvidence(
                sampleContext.identity,
                sampleContext.runId,
                { processInstanceId },
              ),
            }),
          }))
        },
      })
      return completedOwnedOperation(Object.freeze({
        authorities,
        samples,
        closeAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
        forceTerminateAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
      }))
    },
  })
}

function successfulExecutionPublisherHelperOpener():
() => Promise<NetworkMatrixPublisherHelperAuthority> {
  return async () => Object.freeze({
    artifactPublisher: Object.freeze({
      async publish(input) {
        const aggregateJson = input.deriveAggregateJson(input.runJson)
        return Object.freeze({
          outputRoot: input.outputRoot,
          runPath: join(input.outputRoot, 'run.json'),
          aggregatePath: join(input.outputRoot, 'aggregate.json'),
          runJson: input.runJson,
          aggregateJson,
        })
      },
    }),
    aggregatePublisher: testAggregatePublisher(),
    close: () => Promise.resolve(),
  })
}

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  return process.arch === 'arm64' ? 'arm64' : 'amd64'
}
