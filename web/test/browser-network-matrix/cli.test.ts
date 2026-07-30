import { createHash } from 'node:crypto'
import { access, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { parseNetworkMatrixAggregateJson } from '../../scripts/browser-network-matrix/aggregate.ts'
import { aggregateNetworkMatrixFiles } from '../../scripts/browser-network-matrix/cli/aggregate-files.ts'
import { HELPER_BUILD_MANIFEST_SCHEMA_VERSION } from '../../scripts/browser-network-matrix/cli/build-helpers.mjs'
import type { ImmutableTextFilePublisher } from '../../scripts/browser-network-matrix/cli/immutable-file-publication.ts'
import type { NetworkMatrixPublisherHelperAuthority } from '../../scripts/browser-network-matrix/cli/publisher-helper.ts'
import {
  browserNetworkMatrixCli,
  NetworkMatrixRuntimeNotWiredError,
} from '../../scripts/browser-network-matrix/cli/main.ts'
import {
  canonicalNetworkRunResultJson,
  parseNetworkRunResultJson,
} from '../../scripts/browser-network-matrix/result.ts'
import { loadRegistry, makeRun, MANIFEST_PATH } from './fixtures.ts'

const temporaryRoots: string[] = []

afterEach(async () => {
  vi.unstubAllEnvs()
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('local browser network matrix CLI substrate', () => {
  it('aggregates one explicit canonical run without fabricating the absent mode', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'manual.run.json')
    const outputPath = join(root, 'manual.aggregate.json')
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'manual', { runId: 'manual-local-run' }), registry),
      'utf8',
    )

    const result = await aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [runPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })

    expect(result.aggregate).toMatchObject({
      evidenceOutcome: 'incomplete',
      runs: [{ executionMode: 'manual', runId: 'manual-local-run' }],
    })
    const parsedRun = parseNetworkRunResultJson(await readFile(runPath, 'utf8'), registry)
    expect(parseNetworkMatrixAggregateJson(await readFile(outputPath, 'utf8'), registry, [parsedRun]))
      .toEqual(result.aggregate)
  })

  it('combines two explicit distinct modes and publishes an immutable aggregate', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const scheduledPath = join(root, 'scheduled.run.json')
    const manualPath = join(root, 'manual.run.json')
    const outputPath = join(root, 'combined.aggregate.json')
    await Promise.all([
      writeFile(
        scheduledPath,
        canonicalNetworkRunResultJson(
          makeRun(registry, 'scheduled', { runId: 'scheduled-local-run' }),
          registry,
        ),
        'utf8',
      ),
      writeFile(
        manualPath,
        canonicalNetworkRunResultJson(makeRun(registry, 'manual', { runId: 'manual-local-run' }), registry),
        'utf8',
      ),
    ])

    const result = await aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [manualPath, scheduledPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })
    expect(result.aggregate.evidenceOutcome).toBe('complete')
    expect(result.aggregate.runs.map(({ executionMode }) => executionMode))
      .toEqual(['scheduled', 'manual'])

    await expect(aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [manualPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })).rejects.toThrow(/already exists/u)
    expect(await readFile(outputPath, 'utf8')).toBe(result.publication.encoded)
  })

  it('rejects duplicate or non-canonical run inputs before publication', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'manual.run.json')
    const outputPath = join(root, 'aggregate.json')
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'manual'), registry),
      'utf8',
    )
    await expect(aggregateNetworkMatrixFiles({
      registry,
      inputPaths: [runPath, runPath],
      outputPath,
      publisher: testAggregatePublisher(),
    })).rejects.toThrow(/paths must be distinct/u)

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
      '--mode', 'manual',
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
      Promise.reject(new Error('publisher executable authentication failed')))

    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'manual',
      '--run-id', 'publisher-bootstrap-failure',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'must-not-exist'),
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher-helper.exe'),
    ], {
      platform: 'linux',
      loadRegistry,
      runtimeBootstrap: { bootstrap },
      openPublisherHelper,
    })).rejects.toThrow(/publisher executable authentication failed/u)
    expect(openPublisherHelper).toHaveBeenCalledOnce()
    expect(openPublisherHelper).toHaveBeenCalledWith({
      helperManifestPath: join(root, 'helper-manifest.json'),
      platform: 'linux',
      publisherHelperPath: join(root, 'publisher-helper.exe'),
    })
    expect(loadRegistry).not.toHaveBeenCalled()
    expect(bootstrap).not.toHaveBeenCalled()
    await expect(access(join(root, 'must-not-exist'))).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('exposes explicit aggregate inputs without requiring execution composition', async () => {
    const registry = await loadRegistry()
    const root = await temporaryRoot()
    const runPath = join(root, 'manual.run.json')
    const outputPath = join(root, 'manual.aggregate.json')
    const summaries: string[] = []
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'manual'), registry),
      'utf8',
    )

    await expect(browserNetworkMatrixCli([
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--run', runPath,
      '--output', outputPath,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher-helper.exe'),
    ], {
      platform: 'linux',
      openPublisherHelper: testPublisherHelperOpener(),
      writeSummary: (encoded) => summaries.push(encoded),
    })).resolves.toBe(0)

    expect(JSON.parse(summaries[0] as string)).toMatchObject({
      command: 'aggregate',
      modes: ['manual'],
      evidenceOutcome: 'incomplete',
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
    const runPath = join(root, 'manual.run.json')
    const outputPath = join(root, 'manual.aggregate.json')
    const helperManifestPath = join(root, 'helper-manifest.json')
    const publisherHelperPath = join(root, 'browsermatrixpublish.exe')
    const windowsJobHelperPath = join(root, 'windowsjob.exe')
    const openPublisherHelper = vi.fn(testPublisherHelperOpener())
    await writeFile(
      runPath,
      canonicalNetworkRunResultJson(makeRun(registry, 'manual'), registry),
      'utf8',
    )

    await expect(browserNetworkMatrixCli([
      'aggregate',
      '--manifest', MANIFEST_PATH,
      '--run', runPath,
      '--output', outputPath,
      '--helper-manifest', helperManifestPath,
      '--publisher-helper', publisherHelperPath,
      '--windows-job-helper', windowsJobHelperPath,
    ], {
      platform: 'win32',
      openPublisherHelper,
      writeSummary: () => undefined,
    })).resolves.toBe(0)
    expect(openPublisherHelper).toHaveBeenCalledWith({
      helperManifestPath,
      platform: 'win32',
      publisherHelperPath,
      windowsJobHelperPath,
    })
  })

  it('requires each helper authority exactly once and enforces platform-exact flags', async () => {
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
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --helper-manifest must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --publisher-helper must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher'),
    ], { platform: 'win32', openPublisherHelper })).rejects.toThrow(
      /option --windows-job-helper must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher'),
      '--windows-job-helper', join(root, 'windowsjob.exe'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /--windows-job-helper is only valid on Windows/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'one.json'),
      '--helper-manifest', join(root, 'two.json'),
      '--publisher-helper', join(root, 'publisher'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --helper-manifest must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'one-publisher'),
      '--publisher-helper', join(root, 'two-publisher'),
    ], { platform: 'linux', openPublisherHelper })).rejects.toThrow(
      /option --publisher-helper must appear exactly once/u,
    )
    await expect(browserNetworkMatrixCli([
      ...base,
      '--helper-manifest', join(root, 'helper-manifest.json'),
      '--publisher-helper', join(root, 'publisher.exe'),
      '--windows-job-helper', join(root, 'one-windowsjob.exe'),
      '--windows-job-helper', join(root, 'two-windowsjob.exe'),
    ], { platform: 'win32', openPublisherHelper })).rejects.toThrow(
      /option --windows-job-helper must appear at most once/u,
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
      '--mode', 'manual',
      '--run-id', 'no-ambient-helper',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
      '--helper', join(root, 'browsermatrixpublish'),
    ], {
      platform: 'linux',
      runtimeBootstrap: { bootstrap: vi.fn() },
      openPublisherHelper,
    })).rejects.toThrow(/unknown browser network matrix option --helper/u)
    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'manual',
      '--run-id', 'no-ambient-helper',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
    ], {
      platform: 'linux',
      runtimeBootstrap: { bootstrap: vi.fn() },
      openPublisherHelper,
    })).rejects.toThrow(/option --helper-manifest must appear exactly once/u)
    expect(openPublisherHelper).not.toHaveBeenCalled()
  })

  it('authenticates real manifest path and SHA before registry or runtime bootstrap', async () => {
    const root = await temporaryRoot()
    const helperManifestPath = join(root, 'helper-manifest.json')
    const publisherHelperPath = join(root, 'browsermatrixpublish.exe')
    const windowsJobHelperPath = join(root, 'windowsjob.exe')
    await Promise.all([
      writeFile(publisherHelperPath, 'publisher bytes', 'utf8'),
      writeFile(windowsJobHelperPath, 'Windows Job bytes', 'utf8'),
    ])
    await writeFile(helperManifestPath, `${JSON.stringify({
      schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
      platform: 'win32',
      architecture: runtimeGoArchitecture(),
      helpers: [
        { role: 'artifact-publisher', path: publisherHelperPath, sha256: '0'.repeat(64) },
        {
          role: 'windows-job',
          path: windowsJobHelperPath,
          sha256: createHash('sha256').update('Windows Job bytes').digest('hex'),
        },
      ],
    })}\n`, 'utf8')
    const loadRegistry = vi.fn()
    const bootstrap = vi.fn()

    await expect(browserNetworkMatrixCli([
      'execute',
      '--mode', 'manual',
      '--run-id', 'manifest-sha-failure',
      '--manifest', MANIFEST_PATH,
      '--output-root', join(root, 'output'),
      '--helper-manifest', helperManifestPath,
      '--publisher-helper', publisherHelperPath,
      '--windows-job-helper', windowsJobHelperPath,
    ], {
      platform: 'win32',
      loadRegistry,
      runtimeBootstrap: { bootstrap },
    })).rejects.toThrow(/publisher helper bytes differ from the held helper manifest/u)
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

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  return process.arch === 'arm64' ? 'arm64' : 'amd64'
}
