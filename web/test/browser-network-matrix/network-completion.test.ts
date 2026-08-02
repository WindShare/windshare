import { createHash } from 'node:crypto'
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, join, resolve } from 'node:path'

import { afterEach, describe, expect, test } from 'vitest'

import {
  aggregateNetworkMatrix,
  canonicalNetworkMatrixAggregateJson,
} from '../../scripts/browser-network-matrix/aggregate.ts'
import {
  loadNetworkMatrixRegistry,
} from '../../scripts/browser-network-matrix/manifest.ts'
import {
  canonicalNetworkRunResultJson,
} from '../../scripts/browser-network-matrix/result.ts'
import {
  consumeNetworkCompletion,
  NETWORK_COMPLETION_SCHEMA,
  parseNetworkCompletion,
  parseNetworkCompletionJson,
  publishNetworkCompletion,
} from '../../../scripts/ci/browsergate/network-completion.mjs'
import { makeRun, makeRunRaw } from './fixtures.ts'

const SOURCE_ROOT = resolve(import.meta.dirname, '..', '..', '..')
const CHECKOUT_SHA = '0123456789abcdef0123456789abcdef01234567'
const RUN_ID = 'gha-123-1-browser-network'
const PROFILE_NAMES = [
  'scheduled-coturn.v2.json',
  'scheduled-public-stun.v2.json',
  'scheduled-restricted-udp.v2.json',
] as const

const roots: string[] = []

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true })
})

describe('browser network completion', () => {
  test('publishes and independently consumes the exact 45-identity evidence', async () => {
    const fixture = await createFixture()
    const completion = await publishFixture(fixture)

    await expect(consumeNetworkCompletion({
      completionPath: fixture.completionPath,
      checkoutSha: CHECKOUT_SHA,
      repositoryRoot: fixture.root,
    })).resolves.toMatchObject({
      schemaVersion: NETWORK_COMPLETION_SCHEMA,
      runId: RUN_ID,
      expectedIdentities: 45,
      outcome: 'accepted',
    })
    expect(completion.runtimeConfigSha256).toBe(sha256(Buffer.from('runtime-config-bytes')))
  })

  test('rejects extra evidence and mixed run/aggregate generations', async () => {
    const fixture = await createFixture()
    await publishFixture(fixture)
    writeFileSync(join(fixture.outputRoot, 'unexpected.json'), '{}\n')
    await expect(consumeFixture(fixture)).rejects.toThrow(/inventory/u)

    rmSync(join(fixture.outputRoot, 'unexpected.json'))
    const secondRun = makeRun(fixture.registry, 'scheduled', { runId: 'second-network-run' })
    const secondAggregate = aggregateNetworkMatrix(fixture.registry, [secondRun])
    const aggregateBytes = Buffer.from(canonicalNetworkMatrixAggregateJson(
      secondAggregate,
      fixture.registry,
      [secondRun],
    ))
    writeFileSync(join(fixture.outputRoot, 'aggregate.json'), aggregateBytes)
    rewriteCompletion(fixture, { aggregateSha256: sha256(aggregateBytes) })
    await expect(consumeFixture(fixture)).rejects.toThrow()
  })

  test('rejects a 44-sample run even when the envelope digest is recomputed', async () => {
    const fixture = await createFixture()
    await publishFixture(fixture)
    let retained = 0
    const incompleteRun = makeRunRaw(fixture.registry, 'scheduled', {
      runId: RUN_ID,
      sampleFilter: () => (retained += 1) < 45,
    })
    const runBytes = Buffer.from(`${JSON.stringify(incompleteRun)}\n`)
    writeFileSync(join(fixture.outputRoot, 'run.json'), runBytes)
    rewriteCompletion(fixture, { runSha256: sha256(runBytes) })
    await expect(consumeFixture(fixture)).rejects.toThrow()
  })

  test('rejects stale checkout, altered producer identity, and config-binding drift', async () => {
    const fixture = await createFixture()
    await publishFixture(fixture)
    await expect(consumeNetworkCompletion({
      completionPath: fixture.completionPath,
      checkoutSha: 'f'.repeat(40),
      repositoryRoot: fixture.root,
    })).rejects.toThrow(/another checkout/u)

    writeFileSync(
      join(fixture.resultsRoot, 'browser-network-producer-manifest.json'),
      `${readFileSync(fixture.producerManifestPath, 'utf8')} `,
    )
    await expect(consumeFixture(fixture)).rejects.toThrow(/producer manifest/u)

    copyFileSync(
      fixture.producerManifestPath,
      join(fixture.resultsRoot, 'browser-network-producer-manifest.json'),
    )
    rewriteCompletion(fixture, { runtimeConfigSha256: 'a'.repeat(64) })
    await expect(consumeFixture(fixture)).rejects.toThrow(/execution binding contradicts/u)
  })

  test('rejects contradictory cleanup settlement even with a matching binding digest', async () => {
    const fixture = await createFixture()
    await publishFixture(fixture)
    const bindingPath = join(fixture.resultsRoot, 'browser-network-execution-binding.json')
    const binding = JSON.parse(readFileSync(bindingPath, 'utf8')) as Record<string, unknown>
    binding.runtimeCleanupOutcome = 'failed'
    const bindingBytes = Buffer.from(`${JSON.stringify(binding)}\n`)
    writeFileSync(bindingPath, bindingBytes)
    rewriteCompletion(fixture, { executionBindingSha256: sha256(bindingBytes) })
    await expect(consumeFixture(fixture)).rejects.toThrow(/runtime cleanup/u)
  })

  test('rejects noncanonical, extra, getter, and Proxy envelopes without executing traps', async () => {
    const fixture = await createFixture()
    const completion = await publishFixture(fixture)
    const encoded = `${JSON.stringify(completion)}\n`
    expect(() => parseNetworkCompletionJson(` ${encoded}`)).toThrow(/canonical/u)
    expect(() => parseNetworkCompletionJson(`${JSON.stringify({ ...completion, extra: true })}\n`))
      .toThrow(/fields/u)

    let traps = 0
    const proxy = new Proxy(completion, {
      ownKeys() { traps += 1; return [] },
    })
    expect(() => parseNetworkCompletion(proxy)).toThrow(/inert/u)
    expect(traps).toBe(0)

    const getter = { ...completion }
    Object.defineProperty(getter, 'runtimeConfigSha256', {
      enumerable: true,
      get() { traps += 1; return '0'.repeat(64) },
    })
    expect(() => parseNetworkCompletion(getter)).toThrow(/active/u)
    expect(traps).toBe(0)
  })
})

interface Fixture {
  readonly root: string
  readonly resultsRoot: string
  readonly outputRoot: string
  readonly completionPath: string
  readonly producerManifestPath: string
  readonly runtimeHelperManifestPath: string
  readonly producerManifestSha256: string
  readonly runtimeHelperManifestSha256: string
  readonly registry: Awaited<ReturnType<typeof loadNetworkMatrixRegistry>>
}

async function createFixture(): Promise<Fixture> {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'windshare-network-completion-')))
  roots.push(root)
  const registryRoot = join(root, 'testdata', 'browser-network-matrix')
  const profileRoot = join(registryRoot, 'profiles')
  const resultsRoot = join(root, 'test-results')
  const outputRoot = join(resultsRoot, 'browser-network')
  const runtimeRoot = join(root, 'prepared-runtime')
  mkdirSync(profileRoot, { recursive: true })
  mkdirSync(outputRoot, { recursive: true })
  mkdirSync(runtimeRoot)
  copyFileSync(
    join(SOURCE_ROOT, 'testdata', 'browser-network-matrix', 'scheduled-hard.manifest.v2.json'),
    join(registryRoot, 'scheduled-hard.manifest.v2.json'),
  )
  for (const profileName of PROFILE_NAMES) {
    copyFileSync(
      join(SOURCE_ROOT, 'testdata', 'browser-network-matrix', 'profiles', profileName),
      join(profileRoot, profileName),
    )
  }
  const registry = await loadNetworkMatrixRegistry(join(registryRoot, 'scheduled-hard.manifest.v2.json'))
  const run = makeRun(registry, 'scheduled', { runId: RUN_ID })
  const aggregate = aggregateNetworkMatrix(registry, [run])
  writeFileSync(join(outputRoot, 'run.json'), canonicalNetworkRunResultJson(run, registry))
  writeFileSync(
    join(outputRoot, 'aggregate.json'),
    canonicalNetworkMatrixAggregateJson(aggregate, registry, [run]),
  )

  const files = new Map<string, string>([
    ['oidc-network-broker.mjs', 'broker'],
    ['network-entry-bundle.mjs', 'runtime-bundle'],
    ['network-completion-bundle.mjs', 'completion-bundle'],
    ['browsermatrixpublish', 'publisher-helper'],
    ['testprocessowner', 'process-owner'],
  ])
  for (const [name, value] of files) writeFileSync(join(runtimeRoot, name), value)
  const producerManifest = {
    schemaVersion: 'windshare.browser-network-matrix.prepared-input/v1',
    checkoutSha: CHECKOUT_SHA,
    nodeVersion: '24.16.0',
    broker: fileIdentity(join(runtimeRoot, 'oidc-network-broker.mjs'), 'oidc-network-broker.mjs'),
    runtimeBundle: fileIdentity(join(runtimeRoot, 'network-entry-bundle.mjs'), 'network-entry-bundle.mjs'),
    completionBundle: fileIdentity(
      join(runtimeRoot, 'network-completion-bundle.mjs'),
      'network-completion-bundle.mjs',
    ),
    scheduledManifest: fileIdentity(
      join(registryRoot, 'scheduled-hard.manifest.v2.json'),
      'scheduled-hard.manifest.v2.json',
    ),
    scheduledProfiles: PROFILE_NAMES.map((name) =>
      fileIdentity(join(profileRoot, name), `profiles/${name}`)),
    publisherHelper: fileIdentity(join(runtimeRoot, 'browsermatrixpublish'), 'browsermatrixpublish'),
    processOwner: fileIdentity(join(runtimeRoot, 'testprocessowner'), 'testprocessowner'),
  }
  const producerManifestPath = join(runtimeRoot, 'producer-manifest.json')
  writeFileSync(producerManifestPath, `${JSON.stringify(producerManifest)}\n`)
  const runtimeHelperManifest = {
    schemaVersion: 'windshare.browser-network-matrix.helper-build/v2',
    platform: 'linux',
    architecture: 'amd64',
    helpers: [
      { role: 'artifact-publisher', path: join(runtimeRoot, 'browsermatrixpublish') },
      { role: 'test-process-owner', path: join(runtimeRoot, 'testprocessowner') },
    ],
  }
  const runtimeHelperManifestPath = join(runtimeRoot, 'helper-manifest.json')
  writeFileSync(runtimeHelperManifestPath, `${JSON.stringify(runtimeHelperManifest)}\n`)
  return {
    root,
    resultsRoot,
    outputRoot,
    completionPath: join(resultsRoot, 'browser-network-completion.json'),
    producerManifestPath,
    runtimeHelperManifestPath,
    producerManifestSha256: sha256(readFileSync(producerManifestPath)),
    runtimeHelperManifestSha256: sha256(readFileSync(runtimeHelperManifestPath)),
    registry,
  }
}

async function publishFixture(fixture: Fixture) {
  return publishNetworkCompletion({
    repositoryRoot: fixture.root,
    outputRoot: fixture.outputRoot,
    runId: RUN_ID,
    checkoutSha: CHECKOUT_SHA,
    producerManifestPath: fixture.producerManifestPath,
    producerManifestSha256: fixture.producerManifestSha256,
    runtimeHelperManifestPath: fixture.runtimeHelperManifestPath,
    runtimeHelperManifestSha256: fixture.runtimeHelperManifestSha256,
    runtimeConfigSha256: sha256(Buffer.from('runtime-config-bytes')),
    childExitCode: 0,
    childSignal: null,
  })
}

function consumeFixture(fixture: Fixture) {
  return consumeNetworkCompletion({
    completionPath: fixture.completionPath,
    checkoutSha: CHECKOUT_SHA,
    repositoryRoot: fixture.root,
  })
}

function rewriteCompletion(fixture: Fixture, changes: Record<string, unknown>): void {
  const completion = JSON.parse(readFileSync(fixture.completionPath, 'utf8')) as Record<string, unknown>
  writeFileSync(fixture.completionPath, `${JSON.stringify({ ...completion, ...changes })}\n`)
}

function fileIdentity(path: string, fileName = basename(path)) {
  const bytes = readFileSync(path)
  return { fileName, byteLength: bytes.byteLength, sha256: sha256(bytes) }
}

function sha256(bytes: Uint8Array): string {
  return createHash('sha256').update(bytes).digest('hex')
}
