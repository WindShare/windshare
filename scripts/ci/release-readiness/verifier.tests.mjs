import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, rmSync, unlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterEach, test } from 'node:test'
import { crc32, deflateRawSync } from 'node:zlib'

import { NETWORK_COMPLETION_SCHEMA } from '../browsergate/network-completion.mjs'
import {
  MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
  parseStabilityResultArchive,
  sha256ArtifactDigest,
} from '../stability/artifact.mjs'
import {
  STABILITY_WORKFLOW_JOBS,
  createProductVerdictForTermination,
  createStabilityResult,
  createStabilityStartedEvent,
  stabilityEvidenceDigest,
} from '../stability/result.mjs'
import {
  normalizeResolutionManifest,
  serializeResolutionManifest,
} from './contract.mjs'
import {
  STABILITY_ARCHIVE_LAYOUT_ERROR_CODE,
  prevalidateStabilityArchiveLayout,
} from './stability-archive-layout.mjs'
import {
  MAXIMUM_BROWSER_VERDICT_BYTES,
  loadResolutionManifest,
  verifyDownloadedReleaseEvidence,
} from './verifier.mjs'

const TARGET_SHA = 'a'.repeat(40)
const OTHER_SHA = 'b'.repeat(40)
const REPOSITORY = 'WindShare/windshare'
const BROWSER_RUN_ID = '2001'
const BROWSER_RUN_ATTEMPT = 2
const STABILITY_RUN_ID = '3001'
const STABILITY_RUN_ATTEMPT = 3
const LOCAL_BROWSER_RUN_ID = 'local-release-verifier-fixture'
const PASSING_PRODUCT_VERDICT = createProductVerdictForTermination(0, null)
const roots = []

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true })
})

test('accepts one byte-exact browser verdict and two metadata-bound raw stability ZIPs', async () => {
  const fixture = createFixture()
  const milestones = []
  const accepted = await verifyFixture(fixture, {}, { trace: (event) => milestones.push(event) })

  assert.equal(accepted.outcome, 'accepted')
  assert.equal(accepted.target_sha, TARGET_SHA)
  assert.equal(accepted.browser.local_run_id, LOCAL_BROWSER_RUN_ID)
  assert.equal(
    accepted.browser.network_run_id,
    `gha-${BROWSER_RUN_ID}-${BROWSER_RUN_ATTEMPT}-browser-network`,
  )
  assert.equal(accepted.stability.linux.invocation_id, fixture.stability.linux.result.invocation_id)
  assert.equal(accepted.stability.windows.invocation_id, fixture.stability.windows.result.invocation_id)
  assert.deepEqual(
    milestones.map((event) => event.milestone),
    [
      'browser-content-accepted',
      'stability-content-accepted',
      'stability-content-accepted',
      'downloaded-evidence-accepted',
    ],
  )
})

test('loads only canonical resolution JSON bound to the independently supplied repository and SHA', () => {
  const fixture = createFixture()
  const resolutionPath = join(fixture.repositoryRoot, 'resolution.json')
  writeFileSync(resolutionPath, serializeResolutionManifest(fixture.resolution))
  assert.deepEqual(loadResolutionManifest({
    path: resolutionPath,
    repository: REPOSITORY,
    targetSha: TARGET_SHA,
  }), fixture.resolution)

  assert.throws(
    () => loadResolutionManifest({ path: resolutionPath, repository: REPOSITORY, targetSha: OTHER_SHA }),
    /target SHA does not match/u,
  )
  writeFileSync(resolutionPath, JSON.stringify(fixture.resolution, null, 2))
  assert.throws(
    () => loadResolutionManifest({ path: resolutionPath, repository: REPOSITORY, targetSha: TARGET_SHA }),
    /not canonical JSON/u,
  )
})

test('rejects a resolution manifest transplanted across a target or repository', async () => {
  const fixture = createFixture()
  await assert.rejects(
    verifyFixture(fixture, { targetSha: OTHER_SHA }),
    hasCode('resolution-target-mismatch'),
  )
  await assert.rejects(
    verifyFixture(fixture, { repository: 'other/project' }),
    hasCode('resolution-repository-mismatch'),
  )
})

test('rejects ambient OIDC request authority before invoking protected evidence consumers', async () => {
  const fixture = createFixture()
  let invoked = false
  await assert.rejects(
    verifyFixture(fixture, {
      environment: { ACTIONS_ID_TOKEN_REQUEST_URL: 'https://issuer.invalid/request' },
    }, {
      evaluateBrowserGateImpl: async () => {
        invoked = true
        return fixture.browser.verdict
      },
    }),
    hasCode('ambient-oidc-authority'),
  )
  assert.equal(invoked, false)
})

test('rejects ambient repository and Actions runtime tokens', async (context) => {
  for (const name of ['GITHUB_TOKEN', 'GH_TOKEN', 'ACTIONS_RUNTIME_TOKEN']) {
    await context.test(name, async () => {
      const fixture = createFixture()
      await assert.rejects(
        verifyFixture(fixture, { environment: { [name]: 'secret-token' } }),
        hasCode('ambient-repository-token'),
      )
    })
  }
})

test('rejects missing, duplicate, and unsupported browser verdict entries', async (context) => {
  await context.test('missing direct verdict', async () => {
    const fixture = createFixture()
    unlinkSync(fixture.browser.verdictPath)
    await assert.rejects(verifyFixture(fixture), hasCode('missing-browser-verdict'))
  })
  await context.test('two direct verdicts', async () => {
    const fixture = createFixture()
    const secondRoot = join(fixture.browser.evidenceRoot, 'local-second-run')
    mkdirSync(secondRoot)
    writeFileSync(
      join(secondRoot, 'verdict.json'),
      `${JSON.stringify({ ...fixture.browser.verdict, runId: 'local-second-run' })}\n`,
    )
    await assert.rejects(verifyFixture(fixture), hasCode('duplicate-browser-verdict'))
  })
  await context.test('non-directory evidence root entry', async () => {
    const fixture = createFixture()
    writeFileSync(join(fixture.browser.evidenceRoot, 'unexpected.txt'), 'unexpected')
    await assert.rejects(
      verifyFixture(fixture),
      hasCode('unsupported-browser-evidence-entry'),
    )
  })
  await context.test('verdict path is a directory', async () => {
    const fixture = createFixture()
    unlinkSync(fixture.browser.verdictPath)
    mkdirSync(fixture.browser.verdictPath)
    await assert.rejects(
      verifyFixture(fixture),
      hasCode('unsupported-browser-verdict-entry'),
    )
  })
})

test('rejects wrong browser run and checkout identities', async (context) => {
  await context.test('document run differs from containing directory', async () => {
    const fixture = createFixture()
    rewriteVerdict(fixture, { ...fixture.browser.verdict, runId: 'local-another-run' })
    await assert.rejects(verifyFixture(fixture), hasCode('browser-run-mismatch'))
  })
  await context.test('persisted checkout differs from independently supplied target', async () => {
    const fixture = createFixture()
    rewriteVerdict(fixture, { ...fixture.browser.verdict, checkoutSha: OTHER_SHA })
    await assert.rejects(verifyFixture(fixture), hasCode('browser-verdict-byte-mismatch'))
  })
  await context.test('network completion belongs to another selected attempt', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {}, {
      consumeNetworkCompletionImpl: async () => ({
        schemaVersion: NETWORK_COMPLETION_SCHEMA,
        runId: `gha-${BROWSER_RUN_ID}-1-browser-network`,
        checkoutSha: TARGET_SHA,
        expectedIdentities: 45,
        outcome: 'accepted',
      }),
    }), hasCode('browser-network-run-mismatch'))
  })
})

test('rejects noncanonical, forged, oversized, and non-UTF-8 browser verdict bytes', async (context) => {
  await context.test('extra field', async () => {
    const fixture = createFixture()
    rewriteVerdict(fixture, { ...fixture.browser.verdict, forged: true })
    await assert.rejects(verifyFixture(fixture), hasCode('browser-verdict-byte-mismatch'))
  })
  await context.test('pretty printed fields', async () => {
    const fixture = createFixture()
    writeFileSync(fixture.browser.verdictPath, `${JSON.stringify(fixture.browser.verdict, null, 2)}\n`)
    await assert.rejects(verifyFixture(fixture), hasCode('browser-verdict-byte-mismatch'))
  })
  await context.test('oversized document', async () => {
    const fixture = createFixture()
    writeFileSync(fixture.browser.verdictPath, Buffer.alloc(MAXIMUM_BROWSER_VERDICT_BYTES + 1, 0x20))
    await assert.rejects(verifyFixture(fixture), hasCode('unstable-evidence-file'))
  })
  await context.test('invalid UTF-8', async () => {
    const fixture = createFixture()
    writeFileSync(fixture.browser.verdictPath, Buffer.from([0xff, 0xfe, 0xfd]))
    await assert.rejects(verifyFixture(fixture), hasCode('invalid-browser-verdict-json'))
  })
})

test('rejects failed recomputation and protected network consumer failures', async (context) => {
  await context.test('production wiring uses the protected evaluator', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {}, {
      evaluateBrowserGateImpl: undefined,
    }), hasCode('browser-verdict-not-passed'))
  })
  await context.test('production wiring uses the composed completion consumer', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {}, {
      consumeNetworkCompletionImpl: undefined,
    }), hasCode('browser-network-completion-rejected'))
  })
  await context.test('evaluator observes tampered sealed content', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {}, {
      evaluateBrowserGateImpl: async () => { throw new Error('sealed manifest digest mismatch') },
    }), hasCode('browser-verdict-recomputation-failed'))
  })
  await context.test('recomputed verdict is failed even when persisted bytes match', async () => {
    const fixture = createFixture()
    const failed = {
      ...fixture.browser.verdict,
      verdict: 'failed',
      violations: ['synthetic sealed evidence failure'],
    }
    rewriteVerdict(fixture, failed)
    await assert.rejects(verifyFixture(fixture, {}, {
      evaluateBrowserGateImpl: async () => failed,
    }), hasCode('browser-verdict-not-passed'))
  })
  await context.test('composed completion consumer rejects content drift', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {}, {
      consumeNetworkCompletionImpl: async () => { throw new Error('execution binding changed') },
    }), hasCode('browser-network-completion-rejected'))
  })
})

test('checks stability ZIP byte count and digest before parsing', async (context) => {
  await context.test('API byte count mismatch', async () => {
    const fixture = createFixture()
    fixture.resolution = mutateResolution(fixture.resolution, (manifest) => {
      manifest.authorities.stability.artifacts[0].size_in_bytes += 1
    })
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
  await context.test('API digest mismatch', async () => {
    const fixture = createFixture()
    fixture.resolution = mutateResolution(fixture.resolution, (manifest) => {
      manifest.authorities.stability.artifacts[0].digest = `sha256:${'f'.repeat(64)}`
    })
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
  await context.test('archive changed after resolution', async () => {
    const fixture = createFixture()
    const tampered = Buffer.from(fixture.stability.linux.archive)
    tampered[40] ^= 1
    writeFileSync(fixture.stability.linux.path, tampered)
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
})

test('rejects entry payloads that overlap the central directory before semantic parsing', async (context) => {
  for (const compression of [0, 8]) {
    await context.test(compression === 0 ? 'stored payload' : 'deflate payload', () => {
      const fixture = createFixture()
      const valid = archiveFromDocuments(
        fixture.stability.linux.started,
        fixture.stability.linux.result,
        { compression },
      )
      assert.doesNotThrow(() => prevalidateStabilityArchiveLayout(valid))

      const overlap = centralDirectoryOverlapArchive(
        fixture.stability.linux.started,
        fixture.stability.linux.result,
        compression,
      )
      assert.throws(
        () => prevalidateStabilityArchiveLayout(overlap.archive),
        hasCode(STABILITY_ARCHIVE_LAYOUT_ERROR_CODE),
      )
      assert.ok(overlap.payloadEnd > overlap.centralOffset)
    })
  }

  await context.test('the former accepted deflate reproduction is blocked by the verifier', async () => {
    const fixture = createFixture()
    const overlap = centralDirectoryOverlapArchive(
      fixture.stability.linux.started,
      fixture.stability.linux.result,
      8,
    )
    assert.deepEqual(
      parseStabilityResultArchive(overlap.archive),
      fixture.stability.linux.result,
      'the unchanged semantic parser reproduces the reviewed trailing-byte acceptance',
    )
    replaceStabilityArchive(fixture, 'linux', overlap.archive)
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })

  await context.test('signed data-descriptor range ending at the directory boundary remains valid', () => {
    const fixture = createFixture()
    const archive = archiveFromDocuments(
      fixture.stability.linux.started,
      fixture.stability.linux.result,
      { compression: 8, dataDescriptor: true },
    )
    assert.doesNotThrow(() => prevalidateStabilityArchiveLayout(archive))
    assert.deepEqual(parseStabilityResultArchive(archive), fixture.stability.linux.result)
  })
})

test('rejects stability evidence bound to a wrong run, attempt, SHA, OS, or workflow job', async (context) => {
  for (const scenario of [
    { name: 'run', overrides: { workflowRunId: '9999' } },
    { name: 'attempt', overrides: { workflowRunAttempt: 4 } },
    { name: 'SHA', overrides: { commitSha: OTHER_SHA } },
  ]) {
    await context.test(`wrong ${scenario.name}`, async () => {
      const fixture = createFixture()
      replaceStabilityArchive(fixture, 'linux', makeStabilityEvidence({
        operatingSystem: 'linux',
        invocationId: fixture.stability.linux.result.invocation_id,
        ...scenario.overrides,
      }))
      await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
    })
  }
  await context.test('Windows artifact contains internally valid Linux job evidence', async () => {
    const fixture = createFixture()
    replaceStabilityArchive(fixture, 'windows', makeStabilityEvidence({
      operatingSystem: 'linux',
      invocationId: fixture.stability.windows.result.invocation_id,
    }))
    await assert.rejects(verifyFixture(fixture), hasCode('stability-windows-rejected'))
  })
  await context.test('same-OS documents claim the other workflow job', async () => {
    const fixture = createFixture()
    const evidence = fixture.stability.linux
    const started = {
      ...evidence.started,
      workflow_job: STABILITY_WORKFLOW_JOBS.windows.workflowJob,
    }
    const result = {
      ...evidence.result,
      workflow_job: STABILITY_WORKFLOW_JOBS.windows.workflowJob,
      started_event_sha256: stabilityEvidenceDigest(`${JSON.stringify(started)}\n`),
    }
    replaceStabilityArchive(fixture, 'linux', archiveFromDocuments(started, result))
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
})

test('rejects missing and duplicate structured stability artifact content', async (context) => {
  await context.test('missing finished result', async () => {
    const fixture = createFixture()
    replaceStabilityArchive(fixture, 'linux', zipArchive([
      { name: 'untrusted-name/start.payload', content: document(fixture.stability.linux.started) },
    ]))
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
  await context.test('duplicate finished result', async () => {
    const fixture = createFixture()
    replaceStabilityArchive(fixture, 'linux', zipArchive([
      { name: 'one', content: document(fixture.stability.linux.started) },
      { name: 'two', content: document(fixture.stability.linux.result) },
      { name: 'three', content: document(fixture.stability.linux.result) },
    ]))
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
  await context.test('started and result identities disagree', async () => {
    const fixture = createFixture()
    const result = {
      ...fixture.stability.linux.result,
      started_event_sha256: 'f'.repeat(64),
    }
    replaceStabilityArchive(
      fixture,
      'linux',
      archiveFromDocuments(fixture.stability.linux.started, result),
    )
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
})

test('rejects a failed stability product verdict and retry-count disagreement', async (context) => {
  await context.test('product failure', async () => {
    const fixture = createFixture()
    replaceStabilityArchive(fixture, 'linux', makeStabilityEvidence({
      operatingSystem: 'linux',
      invocationId: fixture.stability.linux.result.invocation_id,
      productVerdict: createProductVerdictForTermination(17, null),
    }))
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
  await context.test('retry count', async () => {
    const fixture = createFixture()
    const result = { ...fixture.stability.linux.result, retry_count: 0 }
    replaceStabilityArchive(
      fixture,
      'linux',
      archiveFromDocuments(fixture.stability.linux.started, result),
    )
    await assert.rejects(verifyFixture(fixture), hasCode('stability-linux-rejected'))
  })
})

test('rejects duplicate cross-OS archives and invocation identities', async (context) => {
  await context.test('same archive path', async () => {
    const fixture = createFixture()
    await assert.rejects(verifyFixture(fixture, {
      stabilityWindowsArchive: fixture.stability.linux.path,
    }), hasCode('duplicate-stability-archive'))
  })
  await context.test('same invocation ID', async () => {
    const fixture = createFixture()
    replaceStabilityArchive(fixture, 'windows', makeStabilityEvidence({
      operatingSystem: 'windows',
      invocationId: fixture.stability.linux.result.invocation_id,
    }))
    await assert.rejects(verifyFixture(fixture), hasCode('duplicate-stability-invocation'))
  })
})

function createFixture() {
  const repositoryRoot = mkdtempSync(join(tmpdir(), 'windshare-release-verifier-'))
  roots.push(repositoryRoot)
  const browserRoot = join(repositoryRoot, 'test-results')
  const evidenceRoot = join(browserRoot, 'browser-evidence')
  const runRoot = join(evidenceRoot, LOCAL_BROWSER_RUN_ID)
  mkdirSync(join(runRoot, 'main', '.guard-uploads', 'sealed'), { recursive: true })
  mkdirSync(join(runRoot, 'pion', '.guard-uploads', 'sealed'), { recursive: true })
  const verdict = browserVerdict()
  const verdictPath = join(runRoot, 'verdict.json')
  writeFileSync(verdictPath, document(verdict))

  const stabilityRoot = join(repositoryRoot, 'stability-downloads')
  mkdirSync(stabilityRoot)
  const stability = {
    linux: makeStabilityEvidence({
      operatingSystem: 'linux',
      invocationId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    }),
    windows: makeStabilityEvidence({
      operatingSystem: 'windows',
      invocationId: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
    }),
  }
  for (const operatingSystem of ['linux', 'windows']) {
    stability[operatingSystem].path = join(stabilityRoot, `${operatingSystem}.zip`)
    writeFileSync(stability[operatingSystem].path, stability[operatingSystem].archive)
  }

  const fixture = {
    repositoryRoot: resolve(repositoryRoot),
    browserRoot: resolve(browserRoot),
    browser: { evidenceRoot, runRoot, verdictPath, verdict },
    stability,
  }
  fixture.resolution = buildResolution(stability)
  fixture.dependencies = {
    evaluateBrowserGateImpl: async (options) => {
      assert.equal(options.runId, LOCAL_BROWSER_RUN_ID)
      assert.equal(options.checkoutSha, TARGET_SHA)
      for (const suite of ['main', 'pion']) {
        assert.equal(options.suites[suite].root, join(runRoot, suite, '.guard-uploads', 'sealed'))
      }
      return verdict
    },
    consumeNetworkCompletionImpl: async (options) => {
      assert.equal(options.repositoryRoot, fixture.repositoryRoot)
      assert.equal(options.completionPath, join(browserRoot, 'browser-network-completion.json'))
      assert.equal(options.checkoutSha, TARGET_SHA)
      return {
        schemaVersion: NETWORK_COMPLETION_SCHEMA,
        runId: `gha-${BROWSER_RUN_ID}-${BROWSER_RUN_ATTEMPT}-browser-network`,
        checkoutSha: TARGET_SHA,
        expectedIdentities: 45,
        outcome: 'accepted',
      }
    },
  }
  return fixture
}

function browserVerdict() {
  const suiteOutcome = {
    jobOutcome: 'success',
    guardOutcome: 'passed',
    downloadOutcome: 'success',
    manifestSha256: 'c'.repeat(64),
    manifestByteLength: '1',
  }
  return {
    schemaVersion: 1,
    verdictKind: 'browser-gate',
    runId: LOCAL_BROWSER_RUN_ID,
    checkoutSha: TARGET_SHA,
    browsers: ['chromium', 'firefox', 'webkit'],
    runPolicy: null,
    suiteOutcomes: { main: { ...suiteOutcome }, pion: { ...suiteOutcome } },
    topologyAuthority: { main: null, pion: null },
    verdict: 'passed',
    violations: [],
    samples: [],
  }
}

function makeStabilityEvidence({
  operatingSystem,
  invocationId,
  workflowRunId = STABILITY_RUN_ID,
  workflowRunAttempt = STABILITY_RUN_ATTEMPT,
  commitSha = TARGET_SHA,
  productVerdict = PASSING_PRODUCT_VERDICT,
}) {
  const identity = {
    workflowRunId,
    workflowRunAttempt,
    commitSha,
    workflowJob: STABILITY_WORKFLOW_JOBS[operatingSystem].workflowJob,
    operatingSystem,
    suite: 'integration',
    invocationId,
  }
  const started = createStabilityStartedEvent(identity)
  const result = createStabilityResult({
    ...identity,
    startedEventSha256: stabilityEvidenceDigest(document(started)),
    productVerdict,
  })
  return {
    started,
    result,
    archive: archiveFromDocuments(started, result),
  }
}

function buildResolution(stability) {
  return normalizeResolutionManifest({
    schema_version: 'windshare.release-readiness-resolution/v1',
    target_sha: TARGET_SHA,
    repository: { id: '1', full_name: REPOSITORY, default_branch: 'main' },
    authorities: {
      ci: {
        workflow_id: '10',
        run_id: '1001',
        run_attempt: 1,
        event: 'push',
        terminal_job_ids: ['1002'],
        artifacts: [],
      },
      full_browser: {
        workflow_id: '20',
        run_id: BROWSER_RUN_ID,
        run_attempt: BROWSER_RUN_ATTEMPT,
        event: 'schedule',
        terminal_job_ids: ['2002'],
        artifacts: [{
          role: 'browser',
          id: '2003',
          name: `browser-full-${TARGET_SHA}-${BROWSER_RUN_ID}-${BROWSER_RUN_ATTEMPT}`,
          size_in_bytes: 123,
          digest: `sha256:${'d'.repeat(64)}`,
        }],
      },
      stability: {
        workflow_id: '30',
        run_id: STABILITY_RUN_ID,
        run_attempt: STABILITY_RUN_ATTEMPT,
        event: 'schedule',
        terminal_job_ids: ['3002', '3003'],
        artifacts: ['linux', 'windows'].map((operatingSystem, index) => ({
          role: operatingSystem,
          id: String(3004 + index),
          name:
            `stability-integration-${operatingSystem}-${TARGET_SHA}-${STABILITY_RUN_ID}-${STABILITY_RUN_ATTEMPT}`,
          size_in_bytes: stability[operatingSystem].archive.byteLength,
          digest: sha256ArtifactDigest(stability[operatingSystem].archive),
        })),
      },
    },
  })
}

function verifyFixture(fixture, optionOverrides = {}, dependencyOverrides = {}) {
  return verifyDownloadedReleaseEvidence({
    repository: REPOSITORY,
    targetSha: TARGET_SHA,
    resolution: fixture.resolution,
    repositoryRoot: fixture.repositoryRoot,
    browserRoot: fixture.browserRoot,
    stabilityLinuxArchive: fixture.stability.linux.path,
    stabilityWindowsArchive: fixture.stability.windows.path,
    environment: {},
    ...optionOverrides,
  }, {
    ...fixture.dependencies,
    ...dependencyOverrides,
  })
}

function rewriteVerdict(fixture, value) {
  writeFileSync(fixture.browser.verdictPath, document(value))
}

function replaceStabilityArchive(fixture, operatingSystem, evidence) {
  const archive = Buffer.isBuffer(evidence) ? evidence : evidence.archive
  writeFileSync(fixture.stability[operatingSystem].path, archive)
  if (!Buffer.isBuffer(evidence)) fixture.stability[operatingSystem] = {
    ...evidence,
    path: fixture.stability[operatingSystem].path,
  }
  fixture.resolution = mutateResolution(fixture.resolution, (manifest) => {
    const index = operatingSystem === 'linux' ? 0 : 1
    const artifact = manifest.authorities.stability.artifacts[index]
    artifact.size_in_bytes = archive.byteLength
    artifact.digest = sha256ArtifactDigest(archive)
  })
}

function mutateResolution(resolution, mutate) {
  const value = structuredClone(resolution)
  mutate(value)
  return normalizeResolutionManifest(value)
}

function archiveFromDocuments(started, result, options = {}) {
  return zipArchive([
    { name: 'arbitrary/start.payload', content: document(started), ...options },
    { name: 'another/location/finish.payload', content: document(result), ...options },
  ])
}

function zipArchive(entries) {
  const locals = []
  const centrals = []
  let localOffset = 0
  for (const entry of entries) {
    const nameBytes = Buffer.from(entry.name, 'utf8')
    const uncompressed = Buffer.from(entry.content, 'utf8')
    const compression = entry.compression ?? 0
    assert.ok(compression === 0 || compression === 8)
    const data = compression === 8 ? deflateRawSync(uncompressed) : uncompressed
    const checksum = crc32(uncompressed) >>> 0
    const flags = (1 << 11) | (entry.dataDescriptor === true ? 1 << 3 : 0)

    const local = Buffer.alloc(30)
    local.writeUInt32LE(0x04034b50, 0)
    local.writeUInt16LE(20, 4)
    local.writeUInt16LE(flags, 6)
    local.writeUInt16LE(compression, 8)
    if (entry.dataDescriptor !== true) {
      local.writeUInt32LE(checksum, 14)
      local.writeUInt32LE(data.length, 18)
      local.writeUInt32LE(uncompressed.length, 22)
    }
    local.writeUInt16LE(nameBytes.length, 26)

    const central = Buffer.alloc(46)
    central.writeUInt32LE(0x02014b50, 0)
    central.writeUInt16LE(20, 4)
    central.writeUInt16LE(20, 6)
    central.writeUInt16LE(flags, 8)
    central.writeUInt16LE(compression, 10)
    central.writeUInt32LE(checksum, 16)
    central.writeUInt32LE(data.length, 20)
    central.writeUInt32LE(uncompressed.length, 24)
    central.writeUInt16LE(nameBytes.length, 28)
    central.writeUInt32LE(localOffset, 42)

    let descriptor = Buffer.alloc(0)
    if (entry.dataDescriptor === true) {
      descriptor = Buffer.alloc(16)
      descriptor.writeUInt32LE(0x08074b50, 0)
      descriptor.writeUInt32LE(checksum, 4)
      descriptor.writeUInt32LE(data.length, 8)
      descriptor.writeUInt32LE(uncompressed.length, 12)
    }
    const localRecord = Buffer.concat([local, nameBytes, data, descriptor])
    locals.push(localRecord)
    centrals.push(Buffer.concat([central, nameBytes]))
    localOffset += localRecord.length
  }
  const localBytes = Buffer.concat(locals)
  const centralBytes = Buffer.concat(centrals)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(centralBytes.length, 12)
  end.writeUInt32LE(localBytes.length, 16)
  const archive = Buffer.concat([localBytes, centralBytes, end])
  assert.ok(archive.byteLength <= MAXIMUM_ARTIFACT_ARCHIVE_BYTES)
  return archive
}

function centralDirectoryOverlapArchive(started, result, compression) {
  const archive = Buffer.from(archiveFromDocuments(started, result, { compression }))
  const endOffset = archive.length - 22
  const centralOffset = archive.readUInt32LE(endOffset + 16)
  const firstCentralBytes = 46 + archive.readUInt16LE(centralOffset + 28) +
    archive.readUInt16LE(centralOffset + 30) + archive.readUInt16LE(centralOffset + 32)
  const finalCentralOffset = centralOffset + firstCentralBytes
  const finalLocalOffset = archive.readUInt32LE(finalCentralOffset + 42)
  const finalDataOffset = finalLocalOffset + 30 + archive.readUInt16LE(finalLocalOffset + 26) +
    archive.readUInt16LE(finalLocalOffset + 28)
  const overlappingSize = archive.length - finalDataOffset
  archive.writeUInt32LE(overlappingSize, finalLocalOffset + 18)
  archive.writeUInt32LE(overlappingSize, finalCentralOffset + 20)
  return {
    archive,
    centralOffset,
    payloadEnd: finalDataOffset + overlappingSize,
  }
}

function document(value) {
  return `${JSON.stringify(value)}\n`
}

function hasCode(expected) {
  return (error) => {
    assert.equal(error?.code, expected, error?.stack)
    return true
  }
}
