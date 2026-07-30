import { chmod, mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  aggregateBrowserEvidence,
  aggregateInfrastructureBrowserEvidence,
  parseBrowserEvidenceVerdict,
  type BrowserEvidenceSuiteUploadInput,
} from '../../scripts/browser-evidence/verdict/index.ts'
import { guardArtifactSuite, type GuardArtifactSuiteResult } from '../../scripts/browser-evidence/artifact/guard.ts'
import { guardUploadSampleContractPaths } from '../../scripts/browser-evidence/artifact/sealed-suite.ts'
import { browserRunPolicy, validatePolicySampleIndex } from '../../scripts/browser-evidence/run-policy.ts'
import { BROWSER_ENGINES, BROWSER_SUITES } from '../../scripts/browser-evidence/vocabulary.ts'
import {
  createAlternateFrameworkTopology,
  createFrameworkWorkspace,
  createFrameworkGuardAuthority,
  artifactRootForOutcome,
  FRAMEWORK_CHECKOUT_SHA,
  FRAMEWORK_RUN_ID,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample,
  type FrameworkTopology,
  type FrameworkGuardAuthority,
} from './framework-fixtures.ts'

let topology: FrameworkTopology
const workspaces: string[] = []
const CLOSURE_POLICY = browserRunPolicy('closure')
const CLOSURE_SAMPLE_COUNT = CLOSURE_POLICY.sampleCount
const FRAMEWORK_AGGREGATE_TEST_TIMEOUT_MS = 60_000

beforeAll(async () => { topology = await loadFrameworkTopology() })
afterEach(async () => {
  await Promise.all(workspaces.splice(0).map(removeFrameworkWorkspace))
})

describe('cross-suite browser evidence aggregation', { timeout: FRAMEWORK_AGGREGATE_TEST_TIMEOUT_MS }, () => {
  it('binds sample bounds to the selected versioned policy', async () => {
    expect(CLOSURE_POLICY).toEqual({
      schemaVersion: 1,
      policyId: 'closure',
      policyVersion: 1,
      sampleCount: 3,
    })
    expect(() => validatePolicySampleIndex(4, CLOSURE_POLICY))
      .toThrow(/must be in \[1, 3\]/u)
  })

  it('accepts exactly three samples per suite with different valid machine resolutions', async () => {
    const workspace = await trackedWorkspace()
    const pionTopology = await createAlternateFrameworkTopology(workspace)
    const matrix = await runMatrix(workspace, pionTopology, 'main-pass', 'pion-pass')
    const verdict = await aggregateBrowserEvidence({
      ...expectation(pionTopology),
      suiteUploads: matrix.suiteUploads,
    })
    expect(matrix.main[0]?.result.rtcCapability).toBe('available')
    expect(matrix.pion[0]?.result.rtcCapability).toBe('available')
    expect(verdict).toMatchObject({
      schemaVersion: 3,
      verdictKind: 'evidence',
      verdict: 'passed',
      violations: [],
      runPolicy: CLOSURE_POLICY,
      topologyAuthority: {
        main: { topologyResolutionSha256: topology.lock.resolutionSha256 },
        pion: { topologyResolutionSha256: pionTopology.lock.resolutionSha256 },
      },
      infrastructureCauses: [],
    })
    expect(verdict.samples).toHaveLength(
      BROWSER_SUITES.length * BROWSER_ENGINES.length * CLOSURE_SAMPLE_COUNT,
    )
    expect(verdict.samples.every((sample) =>
      sample.summaryKind === 'evidence' && sample.accepted)).toBe(true)
    expect(parseBrowserEvidenceVerdict(JSON.parse(JSON.stringify(verdict)))).toEqual(verdict)
    await expect(Promise.resolve().then(() => parseBrowserEvidenceVerdict({
      ...JSON.parse(JSON.stringify(verdict)) as object,
      observedSuiteAuthorities: { main: null, pion: null },
    }))).rejects.toThrow(/unknown field/u)
  })

  it('accepts Pion N/A only with each paired main sample proving exact relay fallback', async () => {
    const workspace = await trackedWorkspace()
    const pionTopology = await createAlternateFrameworkTopology(workspace)
    const matrix = await runMatrix(workspace, pionTopology, 'main-unavailable', 'pion-unavailable')
    expect((await aggregateMatrix(matrix, pionTopology)).verdict).toBe('passed')

    const mismatchWorkspace = await trackedWorkspace()
    const mismatchTopology = await createAlternateFrameworkTopology(mismatchWorkspace)
    const mismatch = await runMatrix(mismatchWorkspace, mismatchTopology, 'main-unavailable', 'pion-pass')
    const mismatchVerdict = await aggregateMatrix(mismatch, mismatchTopology)
    expect(mismatchVerdict.verdict).toBe('failed')
    expect(mismatchVerdict.violations.join(' ')).toMatch(/classifications disagree/u)
  })

  it('rejects duplicate, missing, and mixed-within-suite upload authorities', async () => {
    const workspace = await trackedWorkspace()
    const pionTopology = await createAlternateFrameworkTopology(workspace)
    const matrix = await runMatrix(workspace, pionTopology, 'main-pass', 'pion-pass')
    const duplicateInputs = [...matrix.suiteUploads]
    duplicateInputs[1] = duplicateInputs[0]!
    await expect(aggregateBrowserEvidence({
      ...expectation(pionTopology),
      suiteUploads: duplicateInputs,
    })).rejects.toThrow(/canonical identity order/u)

    await expect(aggregateBrowserEvidence({
      ...expectation(pionTopology),
      suiteUploads: matrix.suiteUploads.slice(0, 1),
    })).rejects.toThrow(/wrong available suite upload count/u)

    const mixedWorkspace = await trackedWorkspace()
    const mixedPionTopology = await createAlternateFrameworkTopology(mixedWorkspace)
    const mixed = await runMatrix(
      mixedWorkspace,
      mixedPionTopology,
      'main-pass',
      'pion-pass',
      { mixedMainSample: CLOSURE_SAMPLE_COUNT },
    )
    expect(mixed.mainGuard.upload).toBeNull()
    expect(mixed.mainGuard.guards.every(({ failureMessage }) =>
      failureMessage?.includes('topology snapshots do not bind every sample result'))).toBe(true)
    await expect(aggregateMatrix(mixed, mixedPionTopology))
      .rejects.toThrow(/wrong available suite upload count/u)
  })

  it('blocks a failed guard even when every runtime sample independently passes', async () => {
    const workspace = await trackedWorkspace()
    const pionTopology = await createAlternateFrameworkTopology(workspace)
    const matrix = await runMatrix(
      workspace,
      pionTopology,
      'main-pass',
      'pion-pass',
      { failMainGuardSample: 1 },
    )
    expect(matrix.main.every(({ acceptedBeforeGuard }) => acceptedBeforeGuard)).toBe(true)
    expect(matrix.mainGuard.upload).toBeNull()
    expect(matrix.mainGuard.guards[0]?.guardOutcome).toBe('failed')
    const pionUpload = matrix.suiteUploads.find(({ suite }) => suite === 'pion')
    if (pionUpload === undefined) throw new Error('Pion guard unexpectedly withheld its suite upload')
    const verdict = await aggregateInfrastructureBrowserEvidence({
      runId: FRAMEWORK_RUN_ID,
      checkoutSha: FRAMEWORK_CHECKOUT_SHA,
      runPolicy: CLOSURE_POLICY,
      suiteEvidence: Object.freeze({
        main: null,
        pion: Object.freeze({ topologyLock: pionTopology.lock, upload: pionUpload }),
      }),
      causes: [{ code: 'suite-context-invalid', suite: 'main' }],
    })
    expect(verdict.verdict).toBe('failed')
    expect(verdict.infrastructureCauses).toContainEqual({
      code: 'suite-context-invalid',
      suite: 'main',
    })
  })

  it('rejects post-seal result and attachment mutation by exact digest', async () => {
    const resultWorkspace = await trackedWorkspace()
    const resultTopology = await createAlternateFrameworkTopology(resultWorkspace)
    const resultMatrix = await runMatrix(resultWorkspace, resultTopology, 'main-pass', 'pion-pass')
    const resultUpload = requiredSuiteUpload(resultMatrix.suiteUploads, 'main')
    const resultPath = guardUploadSampleContractPaths(
      resultUpload.uploadDirectory,
      { browser: 'chromium', sampleIndex: 1 },
    ).resultPath
    await chmod(resultPath, 0o600)
    await writeFile(resultPath, `\n${await readFile(resultPath, 'utf8')}`, 'utf8')
    const resultVerdict = await aggregateMatrix(resultMatrix, resultTopology)
    expect(resultVerdict.verdict).toBe('failed')
    expect(resultVerdict.violations.join(' '))
      .toMatch(/native artifact publisher verify failed: publication-unsafe/u)

    const artifactWorkspace = await trackedWorkspace()
    const artifactTopology = await createAlternateFrameworkTopology(artifactWorkspace)
    const artifactMatrix = await runMatrix(
      artifactWorkspace,
      artifactTopology,
      'main-pass',
      'pion-pass',
    )
    const artifactUpload = requiredSuiteUpload(artifactMatrix.suiteUploads, 'main')
    const attachmentsDirectory = guardUploadSampleContractPaths(
      artifactUpload.uploadDirectory,
      { browser: 'chromium', sampleIndex: 2 },
    ).attachmentsDirectory
    const mutatedArtifact = join(attachmentsDirectory, 'runner', 'stdout.log')
    await chmod(mutatedArtifact, 0o600)
    await writeFile(mutatedArtifact, 'post-seal replacement', 'utf8')
    const artifactVerdict = await aggregateMatrix(artifactMatrix, artifactTopology)
    expect(artifactVerdict.verdict).toBe('failed')
    expect(artifactVerdict.violations.join(' '))
      .toMatch(/native artifact publisher verify failed: publication-unsafe/u)
  })

  it('emits a typed all-unavailable infrastructure verdict without claiming topology authority', async () => {
    const verdict = await aggregateInfrastructureBrowserEvidence({
      runId: FRAMEWORK_RUN_ID,
      checkoutSha: FRAMEWORK_CHECKOUT_SHA,
      runPolicy: CLOSURE_POLICY,
      suiteEvidence: Object.freeze({ main: null, pion: null }),
      causes: [
        {
          code: 'setup-failed',
          suite: null,
          detail: 'sentinel infrastructure detail must not escape',
        },
      ],
    })

    expect(verdict).toMatchObject({
      schemaVersion: 3,
      verdictKind: 'infrastructure',
      verdict: 'failed',
      topologyAuthority: null,
      observedSuiteAuthorities: { main: null, pion: null },
      infrastructureCauses: [{ code: 'setup-failed', suite: null }],
    })
    expect(verdict.samples).toHaveLength(
      BROWSER_SUITES.length * BROWSER_ENGINES.length * CLOSURE_SAMPLE_COUNT,
    )
    expect(verdict.samples.every((sample) =>
      sample.summaryKind === 'infrastructure-unavailable')).toBe(true)
    expect(verdict.infrastructureDiagnostics[0]?.detail).toMatch(/^redacted-sha256:[a-f0-9]{64}$/u)
    expect(JSON.stringify(verdict)).not.toContain('sentinel infrastructure detail')
    expect(parseBrowserEvidenceVerdict(JSON.parse(JSON.stringify(verdict)))).toEqual(verdict)
    const impossibleAuthority = {
      ...JSON.parse(JSON.stringify(verdict)) as Record<string, unknown>,
      topologyAuthority: {
        main: { topologyId: 'fabricated' },
        pion: { topologyId: 'fabricated' },
      },
    }
    expect(() => parseBrowserEvidenceVerdict(impossibleAuthority))
      .toThrow(/cannot claim cross-suite topology authority/u)
    const impossibleSample = JSON.parse(JSON.stringify(verdict)) as Record<string, unknown>
    const samples = impossibleSample.samples as Record<string, unknown>[]
    samples[0] = {
      summaryKind: 'evidence',
      suite: 'main',
      browser: 'chromium',
      sampleIndex: 1,
      resultPresent: false,
      guardPresent: false,
      accepted: false,
    }
    expect(() => parseBrowserEvidenceVerdict(impossibleSample))
      .toThrow(/contradicts its suite authority/u)
  })

  it('rejects unredacted detail fields in every serialized finite-cause branch', async () => {
    const verdict = await aggregateInfrastructureBrowserEvidence({
      runId: FRAMEWORK_RUN_ID,
      checkoutSha: FRAMEWORK_CHECKOUT_SHA,
      runPolicy: CLOSURE_POLICY,
      suiteEvidence: Object.freeze({ main: null, pion: null }),
      causes: [{ code: 'setup-failed', suite: null, detail: 'constructor-only secret' }],
    })
    const causeWithDetail = JSON.parse(JSON.stringify(verdict)) as Record<string, unknown>
    const causes = causeWithDetail.infrastructureCauses as Record<string, unknown>[]
    causes[0]!.detail = 'hostile top-level free text'
    expect(() => parseBrowserEvidenceVerdict(causeWithDetail))
      .toThrow(/infrastructure cause contains unknown field "detail"/u)

    const diagnosticCauseWithDetail = JSON.parse(JSON.stringify(verdict)) as Record<string, unknown>
    const diagnostics = diagnosticCauseWithDetail.infrastructureDiagnostics as Record<string, unknown>[]
    const diagnosticCause = diagnostics[0]!.cause as Record<string, unknown>
    diagnosticCause.detail = 'hostile nested free text'
    expect(() => parseBrowserEvidenceVerdict(diagnosticCauseWithDetail))
      .toThrow(/infrastructure cause contains unknown field "detail"/u)
  })

  it('rejects corrupted child verdicts that do not prove the canonical evidence matrix', () => {
    const canonical = canonicalEvidenceVerdictValue()
    expect(parseBrowserEvidenceVerdict(canonical).samples).toHaveLength(
      BROWSER_SUITES.length * BROWSER_ENGINES.length * CLOSURE_SAMPLE_COUNT,
    )

    const missingEngine = structuredClone(canonical)
    missingEngine.browsers.pop()
    expect(() => parseBrowserEvidenceVerdict(missingEngine))
      .toThrow(/canonical browser vocabulary/u)

    const missingIdentity = structuredClone(canonical)
    missingIdentity.samples.pop()
    expect(() => parseBrowserEvidenceVerdict(missingIdentity))
      .toThrow(/wrong sample summary count/u)

    for (const mutation of [
      { resultPresent: true, guardPresent: true, accepted: false },
      { resultPresent: false, guardPresent: true, accepted: false },
      { resultPresent: true, guardPresent: false, accepted: false },
    ]) {
      const incompletePass = structuredClone(canonical)
      Object.assign(incompletePass.samples[0]!, mutation)
      expect(() => parseBrowserEvidenceVerdict(incompletePass))
        .toThrow(/incomplete or unaccepted sample evidence/u)
    }

    const duplicateIdentity = structuredClone(canonical)
    duplicateIdentity.samples[1] = structuredClone(duplicateIdentity.samples[0]!)
    expect(() => parseBrowserEvidenceVerdict(duplicateIdentity))
      .toThrow(/canonical identity order/u)

    const reordered = structuredClone(canonical)
    ;[reordered.samples[0], reordered.samples[1]] = [reordered.samples[1]!, reordered.samples[0]!]
    expect(() => parseBrowserEvidenceVerdict(reordered))
      .toThrow(/canonical identity order/u)

    const failedWithoutViolations = structuredClone(canonical)
    failedWithoutViolations.verdict = 'failed'
    expect(() => parseBrowserEvidenceVerdict(failedWithoutViolations))
      .toThrow(/outcome does not match its violations/u)

    const passedWithViolations = {
      ...structuredClone(canonical),
      violations: ['synthetic evidence failure'],
    }
    expect(() => parseBrowserEvidenceVerdict(passedWithViolations))
      .toThrow(/outcome does not match its violations/u)

    const tamperedAuthority = structuredClone(canonical)
    tamperedAuthority.topologyAuthority.main.topologyProfileSha256 = 'f'.repeat(64)
    expect(() => parseBrowserEvidenceVerdict(tamperedAuthority))
      .toThrow(/do not share one profile/u)
  })

  it('preserves validated suite evidence while the other suite is infrastructure-unavailable', async () => {
    const workspace = await trackedWorkspace()
    const pionTopology = await createAlternateFrameworkTopology(workspace)
    const matrix = await runMatrix(workspace, pionTopology, 'main-pass', 'pion-pass')
    const verdict = await aggregateInfrastructureBrowserEvidence({
      runId: FRAMEWORK_RUN_ID,
      checkoutSha: FRAMEWORK_CHECKOUT_SHA,
      runPolicy: CLOSURE_POLICY,
      suiteEvidence: Object.freeze({
        main: Object.freeze({
          topologyLock: topology.lock,
          upload: requiredSuiteUpload(matrix.suiteUploads, 'main'),
        }),
        pion: null,
      }),
      causes: [
        {
          code: 'suite-download-unavailable',
          suite: 'pion',
          downloadKind: 'publications',
          detail: 'synthetic missing Pion publication download',
        },
      ],
    })

    expect(verdict.topologyAuthority).toBeNull()
    expect(verdict.observedSuiteAuthorities.main?.topologyResolutionSha256)
      .toBe(topology.lock.resolutionSha256)
    expect(verdict.observedSuiteAuthorities.pion).toBeNull()
    const chromiumMain = verdict.samples.filter((sample) =>
      sample.suite === 'main' && sample.browser === 'chromium')
    expect(chromiumMain).toHaveLength(CLOSURE_SAMPLE_COUNT)
    expect(chromiumMain.every((sample) =>
      sample.summaryKind === 'evidence' && sample.accepted)).toBe(true)
    const unavailablePion = verdict.samples.filter((sample) => sample.suite === 'pion')
    expect(unavailablePion).toHaveLength(BROWSER_ENGINES.length * CLOSURE_SAMPLE_COUNT)
    expect(unavailablePion.every((sample) =>
      sample.summaryKind === 'infrastructure-unavailable')).toBe(true)
    expect(verdict.infrastructureCauses).toEqual([{
      code: 'suite-download-unavailable',
      suite: 'pion',
      downloadKind: 'publications',
    }])
  })
})

interface MatrixOptions {
  readonly mixedMainSample?: number
  readonly failMainGuardSample?: number
}

async function runMatrix(
  workspace: string,
  pionTopology: FrameworkTopology,
  mainMode: string,
  pionMode: string,
  options: MatrixOptions = {},
) {
  const identities = BROWSER_ENGINES.flatMap((browser) =>
    Array.from({ length: CLOSURE_SAMPLE_COUNT }, (_, index) => ({
      browser,
      sampleIndex: index + 1,
    })))
  const main = await Promise.all(identities.map(({ browser, sampleIndex }) => runSyntheticSample({
    workspace,
    topology: sampleIndex === options.mixedMainSample ? pionTopology : topology,
    suite: 'main',
    browser,
    sampleIndex,
    mode: mainMode,
  })))
  const pion = await Promise.all(identities.map(({ browser, sampleIndex }) => runSyntheticSample({
    workspace,
    topology: pionTopology,
    suite: 'pion',
    browser,
    sampleIndex,
    mode: pionMode,
  })))
  const [mainGuard, pionGuard] = await Promise.all([
    guardSyntheticSuite(workspace, topology, 'main', main, options.failMainGuardSample),
    guardSyntheticSuite(workspace, pionTopology, 'pion', pion),
  ])
  const suiteUploads = [suiteUploadInput(mainGuard), suiteUploadInput(pionGuard)]
    .filter((upload): upload is BrowserEvidenceSuiteUploadInput => upload !== null)
  return Object.freeze({
    main: Object.freeze(main),
    pion: Object.freeze(pion),
    mainGuard,
    pionGuard,
    suiteUploads: Object.freeze(suiteUploads),
  })
}

async function aggregateMatrix(
  matrix: Awaited<ReturnType<typeof runMatrix>>,
  pionTopology: FrameworkTopology,
) {
  return aggregateBrowserEvidence({
    ...expectation(pionTopology),
    suiteUploads: matrix.suiteUploads,
  })
}

async function guardSyntheticSuite(
  workspace: string,
  suiteTopology: FrameworkTopology,
  suite: 'main' | 'pion',
  outcomes: readonly Awaited<ReturnType<typeof runSyntheticSample>>[],
  failedSampleIndex?: number,
): Promise<GuardArtifactSuiteResult & {
  readonly directoryPublisher: FrameworkGuardAuthority['directoryPublisher']
}> {
  const authority = await createFrameworkGuardAuthority(suiteTopology)
  const uploadParent = join(workspace, `${suite}-guard-upload`)
  await mkdir(uploadParent)
  const samples = await Promise.all(outcomes.map(async (outcome) => {
    const sampleResultBytes = await readFile(outcome.resultPath)
    return Object.freeze({
      sample: outcome.result,
      sampleResultBytes,
      artifactRoot: artifactRootForOutcome(outcome),
      commandSha256: authority.commandSha256,
      settlementAttestation: authority.settlementAttestation(outcome, sampleResultBytes),
    })
  }))
  const guarded = await guardArtifactSuite({
    runId: FRAMEWORK_RUN_ID,
    runPolicy: CLOSURE_POLICY,
    suite,
    checkoutSha: FRAMEWORK_CHECKOUT_SHA,
    uploadParent,
    topology: authority.topology,
    settlementTrust: authority.settlementTrust,
    directoryPublisher: authority.directoryPublisher,
    explicitSecrets: [],
    samples,
    ...(failedSampleIndex === undefined
      ? {}
      : {
          hooks: Object.freeze({
            beforeArtifactScan: (sample: { readonly sampleIndex: number }) => {
              if (sample.sampleIndex === failedSampleIndex) {
                throw new Error('synthetic scanner failure')
              }
            },
          }),
        }),
    trace: () => undefined,
  })
  return Object.freeze({ ...guarded, directoryPublisher: authority.directoryPublisher })
}

function suiteUploadInput(result: GuardArtifactSuiteResult & {
  readonly directoryPublisher: FrameworkGuardAuthority['directoryPublisher']
}): BrowserEvidenceSuiteUploadInput | null {
  if (result.upload === null) return null
  return Object.freeze({
    suite: result.upload.manifest.suite,
    uploadDirectory: result.upload.uploadDirectory,
    manifestSha256: result.upload.manifestSha256,
    manifestByteLength: result.upload.manifestByteLength,
    directoryPublisher: result.directoryPublisher,
  })
}

function requiredSuiteUpload(
  uploads: readonly BrowserEvidenceSuiteUploadInput[],
  suite: BrowserEvidenceSuiteUploadInput['suite'],
): BrowserEvidenceSuiteUploadInput {
  const upload = uploads.find((candidate) => candidate.suite === suite)
  if (upload === undefined) throw new Error(`${suite} guard unexpectedly withheld its suite upload`)
  return upload
}

function expectation(pionTopology: FrameworkTopology) {
  return {
    runId: FRAMEWORK_RUN_ID,
    checkoutSha: FRAMEWORK_CHECKOUT_SHA,
    runPolicy: CLOSURE_POLICY,
    browsers: BROWSER_ENGINES,
    topologyLocks: Object.freeze({ main: topology.lock, pion: pionTopology.lock }),
  }
}

function canonicalEvidenceVerdictValue() {
  const topologyAuthority = {
    topologyId: topology.lock.profile.topologyId,
    topologyProfileSha256: topology.lock.profileSha256,
    topologyResolutionSha256: topology.lock.resolutionSha256,
  }
  return {
    schemaVersion: 3,
    verdictKind: 'evidence',
    runId: FRAMEWORK_RUN_ID,
    checkoutSha: FRAMEWORK_CHECKOUT_SHA,
    topologyAuthority: {
      main: { ...topologyAuthority },
      pion: { ...topologyAuthority },
    },
    infrastructureCauses: [],
    infrastructureDiagnostics: [],
    browsers: [...BROWSER_ENGINES],
    runPolicy: CLOSURE_POLICY,
    verdict: 'passed',
    violations: [],
    samples: BROWSER_SUITES.flatMap((suite) => BROWSER_ENGINES.flatMap((browser) =>
      Array.from({ length: CLOSURE_SAMPLE_COUNT }, (_, index) => ({
        summaryKind: 'evidence',
        suite,
        browser,
        sampleIndex: index + 1,
        resultPresent: true,
        guardPresent: true,
        accepted: true,
      })))),
  }
}

async function trackedWorkspace(): Promise<string> {
  const workspace = await createFrameworkWorkspace()
  workspaces.push(workspace)
  return workspace
}
