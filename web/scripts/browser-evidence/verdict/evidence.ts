import { dirname, isAbsolute, resolve } from 'node:path'

import { sha256Bytes } from '../artifact/manifest.ts'
import {
  guardUploadSampleContractPaths,
  resolveGuardUpload,
  type GuardUploadSelection,
} from '../artifact/sealed-suite.ts'
import { requireGuardUploadDirectoryPublisher } from '../artifact/directory-publisher.ts'
import {
  parseArtifactGuardResult,
  validateArtifactGuardForSample,
  type ArtifactGuardResult,
} from '../artifact/guard-result.ts'
import {
  requireCheckoutSha,
  requireEnum,
  requireRecord,
  requireString,
} from '../contract/json.ts'
import {
  parseBrowserSampleResult,
  validateMainAcceptance,
  validatePionAcceptance,
  type BrowserSampleResult,
} from '../result.ts'
import {
  assertBrowserRunPolicyEqual,
  parseBrowserRunPolicy,
} from '../run-policy.ts'
import { parseCanonicalJsonText } from '../contract/strict-json.ts'
import {
  readVerifiedTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from '../test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserSuite,
} from '../vocabulary.ts'
import {
  BROWSER_VERDICT_SCHEMA_VERSION,
  MAXIMUM_EVIDENCE_CONTRACT_BYTES,
  expectedSampleKeys,
  keyOf,
  normalizedViolations,
  sampleKey,
  suiteTopologyAuthority,
  type BrowserEvidenceAggregateOptions,
  type BrowserEvidenceEvaluatedSampleSummary,
  type BrowserEvidenceEvaluationExpectation,
  type BrowserEvidenceEvidenceVerdict,
  type BrowserEvidenceExpectation,
  type BrowserEvidenceSampleInput,
  type BrowserEvidenceSuiteUploadInput,
  type ExpectedSample,
} from './contract.ts'

interface CollectedResult {
  readonly result: BrowserSampleResult
  readonly sha256: string
  readonly path: string
}

interface CollectedGuard {
  readonly guard: ArtifactGuardResult
  readonly path: string
}

interface ContractFileSnapshot {
  readonly value: unknown
  readonly sha256: string
}

interface ResolvedContractInput {
  readonly path: string
  readonly present: true
  readonly sha256: string
  readonly bytes: Uint8Array
}

interface ResolvedBrowserEvidenceSampleInput extends Omit<BrowserEvidenceSampleInput, 'result' | 'guard'> {
  readonly result: ResolvedContractInput
  readonly guard: ResolvedContractInput
}

export interface BrowserEvidenceEvaluation {
  readonly violations: readonly string[]
  readonly samples: readonly BrowserEvidenceEvaluatedSampleSummary[]
}

export async function aggregateBrowserEvidence(
  options: BrowserEvidenceAggregateOptions,
): Promise<BrowserEvidenceEvidenceVerdict> {
  const expectation = validateExpectation(options)
  const evaluated = await evaluateBrowserEvidence(
    expectation,
    BROWSER_SUITES,
    options.suiteUploads,
    options.preexistingViolations ?? [],
  )
  return Object.freeze({
    schemaVersion: BROWSER_VERDICT_SCHEMA_VERSION,
    verdictKind: 'evidence',
    runId: expectation.runId,
    checkoutSha: expectation.checkoutSha,
    topologyAuthority: Object.freeze({
      main: suiteTopologyAuthority(expectation.topologyLocks.main),
      pion: suiteTopologyAuthority(expectation.topologyLocks.pion),
    }),
    infrastructureCauses: Object.freeze([]) as readonly [],
    infrastructureDiagnostics: Object.freeze([]) as readonly [],
    browsers: expectation.browsers,
    runPolicy: expectation.runPolicy,
    verdict: evaluated.violations.length === 0 ? 'passed' : 'failed',
    violations: evaluated.violations,
    samples: evaluated.samples,
  })
}

export async function evaluateBrowserEvidence(
  expectation: BrowserEvidenceEvaluationExpectation,
  suites: readonly BrowserSuite[],
  suiteUploadInputs: readonly BrowserEvidenceSuiteUploadInput[],
  preexistingViolations: readonly string[],
): Promise<BrowserEvidenceEvaluation> {
  const violations: string[] = [...preexistingViolations]
  const failedKeys = new Set<string>()
  const expected = expectedSampleKeys(expectation.runPolicy, expectation.browsers, suites)
  const uploads = validateSuiteUploadInputs(suiteUploadInputs, suites)
  const inputs = await collectSuiteUploadSamples(
    uploads,
    expectation,
    expected,
    violations,
    failedKeys,
  )
  const inputsByKey = new Map(inputs.map((input) => [sampleKey(input), input]))
  const results = await collectResults(inputs, expectation, violations, failedKeys)
  const guards = await collectGuards(inputs, expectation, violations, failedKeys)
  await validateExpectedSamples(
    expected,
    inputsByKey,
    results,
    guards,
    expectation,
    violations,
    failedKeys,
  )
  if (suites.includes('main') && suites.includes('pion')) {
    validateCrossSuiteCorrelation(expectation, results, violations, failedKeys)
  }
  const normalized = normalizedViolations(violations)
  const samples = expected.map(({ suite, browser, sampleIndex, key }) => Object.freeze({
    summaryKind: 'evidence' as const,
    suite,
    browser,
    sampleIndex,
    resultPresent: results.has(key),
    guardPresent: guards.has(key),
    accepted: results.has(key) && guards.has(key) && !failedKeys.has(key),
  }))
  return Object.freeze({
    violations: normalized,
    samples: Object.freeze(samples),
  })
}

async function collectSuiteUploadSamples(
  uploads: readonly BrowserEvidenceSuiteUploadInput[],
  expectation: BrowserEvidenceEvaluationExpectation,
  expected: readonly ExpectedSample[],
  violations: string[],
  failedKeys: Set<string>,
): Promise<readonly ResolvedBrowserEvidenceSampleInput[]> {
  const samples: ResolvedBrowserEvidenceSampleInput[] = []
  for (const upload of uploads) {
    try {
      const selection = await resolveGuardUpload(upload)
      assertSuiteUploadIdentity(selection, upload, expectation)
      for (const snapshot of selection.sampleSnapshots) {
        const manifestSample = snapshot.manifest
        const paths = guardUploadSampleContractPaths(selection.uploadDirectory, manifestSample)
        samples.push(Object.freeze({
          suite: upload.suite,
          browser: manifestSample.browser,
          sampleIndex: manifestSample.sampleIndex,
          result: Object.freeze({
            path: paths.resultPath,
            present: true,
            sha256: manifestSample.sampleResultSha256,
            bytes: snapshot.resultBytes,
          }),
          guard: Object.freeze({
            path: paths.guardPath,
            present: true,
            sha256: manifestSample.guardResultSha256,
            bytes: snapshot.guardBytes,
          }),
        }))
      }
    } catch (cause) {
      violations.push(`${upload.suite} guard upload is invalid: ${errorMessage(cause)}`)
      for (const sample of expected) {
        if (sample.suite === upload.suite) failedKeys.add(sample.key)
      }
    }
  }
  return validateDerivedSampleInputs(samples)
}

function assertSuiteUploadIdentity(
  selection: GuardUploadSelection,
  input: BrowserEvidenceSuiteUploadInput,
  expectation: BrowserEvidenceEvaluationExpectation,
): void {
  const { manifest } = selection
  if (manifest.suite !== input.suite) throw new Error('guard upload suite contradicts its ledger slot')
  if (manifest.runId !== expectation.runId) throw new Error('guard upload run ID contradicts the verdict')
  if (manifest.checkoutSha !== expectation.checkoutSha) {
    throw new Error('guard upload checkout SHA contradicts the verdict')
  }
  assertBrowserRunPolicyEqual(manifest.runPolicy, expectation.runPolicy, 'guard upload run policy')
}

async function collectResults(
  samples: readonly ResolvedBrowserEvidenceSampleInput[],
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): Promise<Map<string, CollectedResult>> {
  const results = new Map<string, CollectedResult>()
  for (const sample of samples) {
    const key = sampleKey(sample)
    if (!sample.result.present) continue
    const path = sample.result.path
    try {
      const snapshot = readCanonicalContract(sample.result, 'browser result')
      requireContractDigest(snapshot.sha256, sample.result.sha256, 'browser result')
      const value = snapshot.value
      const suite = requireEnum(
        requireRecord(value, 'browser result').suite,
        BROWSER_SUITES,
        'browser result suite',
      )
      const result = parseBrowserSampleResult(value, requiredTopologyLock(expectation, suite))
      requireSampleIdentity(result, sample, 'browser result')
      results.set(key, Object.freeze({ result, sha256: snapshot.sha256, path }))
      validateResultIdentity(result, expectation, violations, failedKeys)
    } catch (cause) {
      violations.push(`browser result ${path} is invalid: ${errorMessage(cause)}`)
      failedKeys.add(key)
    }
  }
  return results
}

async function collectGuards(
  samples: readonly ResolvedBrowserEvidenceSampleInput[],
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): Promise<Map<string, CollectedGuard>> {
  const guards = new Map<string, CollectedGuard>()
  for (const sample of samples) {
    const key = sampleKey(sample)
    if (!sample.guard.present) continue
    const path = sample.guard.path
    try {
      const snapshot = readCanonicalContract(sample.guard, 'artifact guard')
      requireContractDigest(snapshot.sha256, sample.guard.sha256, 'artifact guard')
      const guard = parseArtifactGuardResult(snapshot.value)
      requireSampleIdentity(guard, sample, 'artifact guard')
      guards.set(key, Object.freeze({ guard, path }))
      validateGuardIdentity(guard, expectation, violations, failedKeys)
    } catch (cause) {
      violations.push(`artifact guard ${path} is invalid: ${errorMessage(cause)}`)
      failedKeys.add(key)
    }
  }
  return guards
}

async function validateExpectedSamples(
  expected: readonly ExpectedSample[],
  sampleInputs: ReadonlyMap<string, ResolvedBrowserEvidenceSampleInput>,
  results: ReadonlyMap<string, CollectedResult>,
  guards: ReadonlyMap<string, CollectedGuard>,
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): Promise<void> {
  for (const sample of expected) {
    const input = sampleInputs.get(sample.key)
    await validateExpectedSample(sample, input, results, guards, expectation, violations, failedKeys)
  }
}

async function validateExpectedSample(
  sample: ExpectedSample,
  input: ResolvedBrowserEvidenceSampleInput | undefined,
  results: ReadonlyMap<string, CollectedResult>,
  guards: ReadonlyMap<string, CollectedGuard>,
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): Promise<void> {
  const collectedResult = results.get(sample.key)
  const collectedGuard = guards.get(sample.key)
  if (collectedResult === undefined) markMissing(sample.key, 'result', violations, failedKeys)
  if (collectedGuard === undefined) markMissing(sample.key, 'guard', violations, failedKeys)
  if (input === undefined || collectedResult === undefined || collectedGuard === undefined) return
  const { result } = collectedResult
  try {
    requireGuardAuthorization(collectedResult, collectedGuard)
  } catch (cause) {
    violations.push(`${sample.key} guard rejected: ${errorMessage(cause)}`)
    failedKeys.add(sample.key)
  }
  try {
    validateSampleAcceptance(result, expectation)
  } catch (cause) {
    violations.push(`${sample.key} sample rejected: ${errorMessage(cause)}`)
    failedKeys.add(sample.key)
  }
}

function requireGuardAuthorization(result: CollectedResult, guard: CollectedGuard): void {
  validateArtifactGuardForSample(guard.guard, result.result, result.sha256)
  if (dirname(result.path) !== dirname(guard.path)) {
    throw new Error('result and guard files do not share one sealed upload sample directory')
  }
}

function validateSampleAcceptance(
  result: BrowserSampleResult,
  expectation: BrowserEvidenceEvaluationExpectation,
): void {
  if (result.suite === 'main') validateMainAcceptance(result, requiredTopologyLock(expectation, 'main'))
  else validatePionAcceptance(result)
}

function validateCrossSuiteCorrelation(
  expectation: BrowserEvidenceEvaluationExpectation,
  results: ReadonlyMap<string, CollectedResult>,
  violations: string[],
  failedKeys: Set<string>,
): void {
  for (const browser of expectation.browsers) {
    for (let sampleIndex = 1; sampleIndex <= expectation.runPolicy.sampleCount; sampleIndex += 1) {
      const mainKey = keyOf('main', browser, sampleIndex)
      const pionKey = keyOf('pion', browser, sampleIndex)
      const main = results.get(mainKey)?.result
      const pion = results.get(pionKey)?.result
      if (main?.suite !== 'main' || pion?.suite !== 'pion') continue
      if (
        main.rtcCapability !== pion.rtcCapability ||
        main.capabilityEvidence.apiPresence !== pion.capabilityEvidence.apiPresence
      ) {
        violations.push(`${browser}/${sampleIndex} main and Pion capability classifications disagree`)
        failedKeys.add(mainKey)
        failedKeys.add(pionKey)
        continue
      }
      if (
        pion.applicability === 'not-applicable' &&
        (main.rtcCapability !== 'unavailable' || main.peerAttemptOutcome !== 'not-started' ||
          main.routeEvidence?.mode !== 'relay-only')
      ) {
        violations.push(`${browser}/${sampleIndex} Pion N/A lacks correlated main relay fallback`)
        failedKeys.add(mainKey)
        failedKeys.add(pionKey)
      }
    }
  }
}

function validateResultIdentity(
  result: BrowserSampleResult,
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): void {
  const key = sampleKey(result)
  if (result.runId !== expectation.runId) markIdentityMismatch(key, 'run ID', violations, failedKeys)
  if (result.checkoutSha !== expectation.checkoutSha) {
    markIdentityMismatch(key, 'checkout SHA', violations, failedKeys)
  }
  try {
    assertBrowserRunPolicyEqual(result.runPolicy, expectation.runPolicy, 'sample run policy')
  } catch {
    markIdentityMismatch(key, 'run policy', violations, failedKeys)
  }
  const topologyLock = requiredTopologyLock(expectation, result.suite)
  if (result.topologyProfileSha256 !== topologyLock.profileSha256) {
    markIdentityMismatch(key, 'topology profile SHA-256', violations, failedKeys)
  }
  if (result.topologyResolutionSha256 !== topologyLock.resolutionSha256) {
    markIdentityMismatch(key, 'topology resolution SHA-256', violations, failedKeys)
  }
}

function validateGuardIdentity(
  guard: ArtifactGuardResult,
  expectation: BrowserEvidenceEvaluationExpectation,
  violations: string[],
  failedKeys: Set<string>,
): void {
  const key = sampleKey(guard)
  if (guard.runId !== expectation.runId) markIdentityMismatch(key, 'guard run ID', violations, failedKeys)
  if (guard.checkoutSha !== expectation.checkoutSha) {
    markIdentityMismatch(key, 'guard checkout SHA', violations, failedKeys)
  }
}

function validateExpectation(options: BrowserEvidenceExpectation): BrowserEvidenceExpectation {
  const topologyLocks = Object.freeze({
    main: readVerifiedTestIceTopologyLock(options.topologyLocks.main),
    pion: readVerifiedTestIceTopologyLock(options.topologyLocks.pion),
  })
  if (
    topologyLocks.main.profileSha256 !== topologyLocks.pion.profileSha256 ||
    topologyLocks.main.profile.topologyId !== topologyLocks.pion.profile.topologyId
  ) {
    throw new Error('browser verdict suite topology locks must share one profile authority')
  }
  const runId = requireString(options.runId, 'browser verdict run ID', 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(runId)) throw new Error('browser verdict run ID is not portable')
  const browsers = options.browsers.map((browser) =>
    requireEnum(browser, BROWSER_ENGINES, 'browser verdict engine'))
  if (
    browsers.length !== BROWSER_ENGINES.length ||
    browsers.some((browser, index) => browser !== BROWSER_ENGINES[index])
  ) {
    throw new Error('browser verdict requires the canonical ordered engine set')
  }
  return Object.freeze({
    runId,
    checkoutSha: requireCheckoutSha(options.checkoutSha, 'browser verdict checkout SHA'),
    runPolicy: parseBrowserRunPolicy(options.runPolicy, 'browser verdict run policy'),
    browsers: BROWSER_ENGINES,
    topologyLocks,
  })
}

function requiredTopologyLock(
  expectation: BrowserEvidenceEvaluationExpectation,
  suite: BrowserSuite,
): VerifiedTestIceTopologyLock {
  const lock = expectation.topologyLocks[suite]
  if (lock === undefined) throw new Error(`${suite} topology authority is unavailable`)
  return lock
}

function readCanonicalContract(input: ResolvedContractInput, label: string): ContractFileSnapshot {
  const bytes = new Uint8Array(input.bytes)
  if (bytes.byteLength > MAXIMUM_EVIDENCE_CONTRACT_BYTES) {
    throw new Error(`${label} exceeds the maximum contract size`)
  }
  let decoded: string
  try {
    decoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not valid UTF-8`)
  }
  return Object.freeze({
    value: parseCanonicalJsonText(decoded, label),
    sha256: sha256Bytes(bytes),
  })
}

function validateSuiteUploadInputs(
  inputs: readonly BrowserEvidenceSuiteUploadInput[],
  suites: readonly BrowserSuite[],
): readonly BrowserEvidenceSuiteUploadInput[] {
  const canonicalSuites = BROWSER_SUITES.filter((suite) => suites.includes(suite))
  if (
    canonicalSuites.length !== suites.length ||
    canonicalSuites.some((suite, index) => suite !== suites[index])
  ) throw new Error('browser evidence suites are not canonical, unique, and ordered')
  if (inputs.length !== canonicalSuites.length) {
    throw new Error('browser evidence input has the wrong available suite upload count')
  }
  const uploadDirectories = new Set<string>()
  return Object.freeze(inputs.map((input, index) => {
    const keys = Object.keys(input)
    const expectedKeys = [
      'suite', 'uploadDirectory', 'manifestByteLength', 'manifestSha256', 'directoryPublisher',
    ]
    if (keys.length !== expectedKeys.length || expectedKeys.some((key) => !Object.hasOwn(input, key))) {
      throw new Error('browser evidence suite upload does not have exact authority fields')
    }
    const suite = requireEnum(input.suite, BROWSER_SUITES, 'browser evidence suite upload slot')
    if (suite !== canonicalSuites[index]) {
      throw new Error('browser evidence suite uploads are not in canonical identity order')
    }
    requireCanonicalAbsolutePath(input.uploadDirectory, 'guard upload directory')
    if (uploadDirectories.has(input.uploadDirectory)) {
      throw new Error('browser evidence reuses a guard upload directory')
    }
    uploadDirectories.add(input.uploadDirectory)
    if (!/^[a-f0-9]{64}$/u.test(input.manifestSha256)) {
      throw new Error('guard upload manifest authenticated digest is invalid')
    }
    if (!/^[1-9]\d*$/u.test(input.manifestByteLength) ||
        !Number.isSafeInteger(Number(input.manifestByteLength))) {
      throw new Error('guard upload manifest authenticated byte length is invalid')
    }
    requireGuardUploadDirectoryPublisher(input.directoryPublisher)
    return Object.freeze({
      suite,
      uploadDirectory: input.uploadDirectory,
      manifestByteLength: input.manifestByteLength,
      manifestSha256: input.manifestSha256,
      directoryPublisher: input.directoryPublisher,
    })
  }))
}

function validateDerivedSampleInputs(
  inputs: readonly ResolvedBrowserEvidenceSampleInput[],
): readonly ResolvedBrowserEvidenceSampleInput[] {
  const contractPaths = new Set<string>()
  const sampleKeys = new Set<string>()
  return Object.freeze(inputs.map((input) => {
    const key = sampleKey(input)
    if (sampleKeys.has(key)) throw new Error('browser evidence suite uploads repeat a sample identity')
    sampleKeys.add(key)
    validateContractInput(input.result, 'browser result', contractPaths)
    validateContractInput(input.guard, 'artifact guard', contractPaths)
    if (dirname(input.result.path) !== dirname(input.guard.path)) {
      throw new Error('browser result and artifact guard paths do not share one sample slot')
    }
    return input
  }))
}

function validateContractInput(
  input: ResolvedContractInput,
  label: string,
  observedPaths: Set<string>,
): void {
  requireCanonicalAbsolutePath(input.path, `${label} path`)
  if (observedPaths.has(input.path)) throw new Error(`browser evidence reuses contract path ${input.path}`)
  observedPaths.add(input.path)
  if (input.present !== (input.sha256 !== null)) {
    throw new Error(`${label} presence contradicts its authenticated digest`)
  }
  if (input.sha256 !== null && !/^[a-f0-9]{64}$/u.test(input.sha256)) {
    throw new Error(`${label} authenticated digest is invalid`)
  }
}

function requireCanonicalAbsolutePath(path: string, label: string): void {
  if (!isAbsolute(path) || resolve(path) !== path) throw new Error(`${label} is not absolute and canonical`)
}

function requireContractDigest(actual: string, expected: string | null, label: string): void {
  if (expected === null) throw new Error(`${label} was read without authenticated presence`)
  if (actual !== expected) throw new Error(`${label} bytes do not match the authenticated ledger digest`)
}

function requireSampleIdentity(
  actual: { readonly suite: BrowserSuite; readonly browser: string; readonly sampleIndex: number },
  expected: BrowserEvidenceSampleInput,
  label: string,
): void {
  if (
    actual.suite !== expected.suite || actual.browser !== expected.browser ||
    actual.sampleIndex !== expected.sampleIndex
  ) throw new Error(`${label} identity does not match its authenticated ledger slot`)
}

function markMissing(
  key: string,
  kind: string,
  violations: string[],
  failedKeys: Set<string>,
): void {
  violations.push(`missing ${kind} for ${key}`)
  failedKeys.add(key)
}

function markIdentityMismatch(
  key: string,
  field: string,
  violations: string[],
  failedKeys: Set<string>,
): void {
  violations.push(`${key} ${field} does not match the verdict expectation`)
  failedKeys.add(key)
}

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}
