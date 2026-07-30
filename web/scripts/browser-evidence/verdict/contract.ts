import {
  requireArray,
  requireBoolean,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from '../contract/json.ts'
import type { VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'
import type { GuardUploadDirectoryPublisher } from '../artifact/directory-publisher.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from '../run-policy.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserEngine,
  type BrowserSuite,
} from '../vocabulary.ts'

export const BROWSER_VERDICT_SCHEMA_VERSION = 3 as const
export const MAXIMUM_EVIDENCE_CONTRACT_BYTES = 16_777_216 as const

export const BROWSER_VERDICT_INFRASTRUCTURE_CAUSE_CODES = [
  'setup-failed',
  'suite-job-failed',
  'suite-download-unavailable',
  'suite-context-invalid',
  'external-verdict-failed',
  'budget-exhausted',
] as const

export const BROWSER_VERDICT_DOWNLOAD_KINDS = ['contracts', 'publications'] as const

export type NonemptyReadonlyArray<T> = readonly [T, ...T[]]
export type BrowserVerdictDownloadKind = typeof BROWSER_VERDICT_DOWNLOAD_KINDS[number]

export interface BrowserVerdictSetupCause {
  readonly code: 'setup-failed' | 'budget-exhausted'
  readonly suite: BrowserSuite | null
}

export interface BrowserVerdictSuiteCause {
  readonly code: 'suite-job-failed' | 'suite-context-invalid'
  readonly suite: BrowserSuite
}

export interface BrowserVerdictSuiteDownloadCause {
  readonly code: 'suite-download-unavailable'
  readonly suite: BrowserSuite
  readonly downloadKind: BrowserVerdictDownloadKind
}

export interface BrowserVerdictExternalCause {
  readonly code: 'external-verdict-failed'
  readonly suite: null
}

export type BrowserVerdictInfrastructureCause =
  | BrowserVerdictSetupCause
  | BrowserVerdictSuiteCause
  | BrowserVerdictSuiteDownloadCause
  | BrowserVerdictExternalCause

export type BrowserVerdictInfrastructureCauseInput = BrowserVerdictInfrastructureCause & {
  readonly detail?: string
}

export interface BrowserVerdictInfrastructureDiagnostic {
  readonly cause: BrowserVerdictInfrastructureCause
  readonly detail: string
}

export interface BrowserEvidenceTopologyLocks {
  readonly main: VerifiedTestIceTopologyLock
  readonly pion: VerifiedTestIceTopologyLock
}

export interface BrowserEvidenceIdentityExpectation {
  readonly runId: string
  readonly checkoutSha: string
  readonly runPolicy: BrowserRunPolicy
  readonly browsers: readonly BrowserEngine[]
}

export interface BrowserEvidenceExpectation extends BrowserEvidenceIdentityExpectation {
  readonly topologyLocks: BrowserEvidenceTopologyLocks
}

export interface BrowserEvidenceEvaluationExpectation extends BrowserEvidenceIdentityExpectation {
  readonly topologyLocks: Partial<Readonly<Record<BrowserSuite, VerifiedTestIceTopologyLock>>>
}

export interface BrowserEvidenceAggregateOptions extends BrowserEvidenceExpectation {
  readonly suiteUploads: readonly BrowserEvidenceSuiteUploadInput[]
  readonly preexistingViolations?: readonly string[]
}

export interface BrowserEvidenceAvailableSuite {
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly upload: BrowserEvidenceSuiteUploadInput
  readonly preexistingViolations?: readonly string[]
}

export interface BrowserEvidenceContractInput {
  readonly path: string
  readonly present: boolean
  readonly sha256: string | null
}

export interface BrowserEvidenceSuiteUploadInput {
  readonly suite: BrowserSuite
  readonly uploadDirectory: string
  readonly manifestByteLength: string
  readonly manifestSha256: string
  readonly directoryPublisher: GuardUploadDirectoryPublisher
}

export interface BrowserEvidenceSampleInput extends BrowserEvidenceSampleIdentity {
  readonly result: BrowserEvidenceContractInput
  readonly guard: BrowserEvidenceContractInput
}

export interface BrowserEvidenceInfrastructureAggregateOptions {
  readonly runId: string
  readonly checkoutSha: string
  readonly runPolicy: BrowserRunPolicy
  readonly suiteEvidence: Readonly<Record<BrowserSuite, BrowserEvidenceAvailableSuite | null>>
  readonly causes: NonemptyReadonlyArray<BrowserVerdictInfrastructureCauseInput>
}

interface BrowserEvidenceSampleIdentity {
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
}

export interface BrowserEvidenceEvaluatedSampleSummary extends BrowserEvidenceSampleIdentity {
  readonly summaryKind: 'evidence'
  readonly resultPresent: boolean
  readonly guardPresent: boolean
  readonly accepted: boolean
}

export interface BrowserEvidenceUnavailableSampleSummary extends BrowserEvidenceSampleIdentity {
  readonly summaryKind: 'infrastructure-unavailable'
}

export type BrowserEvidenceSampleSummary =
  | BrowserEvidenceEvaluatedSampleSummary
  | BrowserEvidenceUnavailableSampleSummary

export interface BrowserEvidenceSuiteTopologyAuthority {
  readonly topologyId: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
}

interface BrowserEvidenceVerdictBase {
  readonly schemaVersion: typeof BROWSER_VERDICT_SCHEMA_VERSION
  readonly runId: string
  readonly checkoutSha: string
  readonly browsers: readonly BrowserEngine[]
  readonly runPolicy: BrowserRunPolicy
  readonly verdict: 'passed' | 'failed'
  readonly violations: readonly string[]
}

export interface BrowserEvidenceEvidenceVerdict extends BrowserEvidenceVerdictBase {
  readonly verdictKind: 'evidence'
  readonly topologyAuthority: Readonly<Record<BrowserSuite, BrowserEvidenceSuiteTopologyAuthority>>
  readonly infrastructureCauses: readonly []
  readonly infrastructureDiagnostics: readonly []
  readonly samples: readonly BrowserEvidenceEvaluatedSampleSummary[]
}

export interface BrowserEvidenceInfrastructureVerdict extends BrowserEvidenceVerdictBase {
  readonly verdictKind: 'infrastructure'
  readonly topologyAuthority: null
  readonly observedSuiteAuthorities: Readonly<Record<BrowserSuite, BrowserEvidenceSuiteTopologyAuthority | null>>
  readonly infrastructureCauses: NonemptyReadonlyArray<BrowserVerdictInfrastructureCause>
  readonly infrastructureDiagnostics: readonly BrowserVerdictInfrastructureDiagnostic[]
  readonly verdict: 'failed'
  readonly samples: readonly BrowserEvidenceSampleSummary[]
}

export type BrowserEvidenceVerdict = BrowserEvidenceEvidenceVerdict | BrowserEvidenceInfrastructureVerdict

export interface ExpectedSample {
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly key: string
}

export function parseBrowserEvidenceVerdict(value: unknown): BrowserEvidenceVerdict {
  const record = requireRecord(value, 'browser evidence verdict')
  const verdictKind = requireEnum(
    record.verdictKind,
    ['evidence', 'infrastructure'] as const,
    'browser evidence verdict kind',
  )
  return verdictKind === 'evidence'
    ? parseEvidenceVerdict(record)
    : parseInfrastructureVerdict(record)
}

function parseEvidenceVerdict(record: Record<string, unknown>): BrowserEvidenceEvidenceVerdict {
  requireExactKeys(record, [
    'schemaVersion',
    'verdictKind',
    'runId',
    'checkoutSha',
    'topologyAuthority',
    'infrastructureCauses',
    'infrastructureDiagnostics',
    'browsers',
    'runPolicy',
    'verdict',
    'violations',
    'samples',
  ], [], 'browser evidence verdict')
  const common = parseVerdictCommon(record)
  if (!sameOrderedStrings(common.browsers, BROWSER_ENGINES)) {
    throw new Error('evidence verdict must summarize the canonical browser vocabulary')
  }
  const topologyAuthority = parseCompleteTopologyAuthority(record.topologyAuthority)
  if (
    topologyAuthority.main.topologyId !== topologyAuthority.pion.topologyId ||
    topologyAuthority.main.topologyProfileSha256 !== topologyAuthority.pion.topologyProfileSha256
  ) throw new Error('evidence verdict topology authorities do not share one profile')
  requireEmptyArray(record.infrastructureCauses, 'evidence verdict infrastructure causes')
  requireEmptyArray(record.infrastructureDiagnostics, 'evidence verdict infrastructure diagnostics')
  const samples = parseVerdictSamples(record.samples, common.runPolicy, common.browsers, {
    main: 'evidence',
    pion: 'evidence',
  })
  const evaluated = samples.map((sample) => {
    if (sample.summaryKind !== 'evidence') {
      throw new Error('evidence verdict contains an infrastructure-unavailable sample')
    }
    return sample
  })
  if ((common.verdict === 'passed') !== (common.violations.length === 0)) {
    throw new Error('evidence verdict outcome does not match its violations')
  }
  if (
    common.verdict === 'passed' &&
    !evaluated.every((sample) => sample.resultPresent && sample.guardPresent && sample.accepted)
  ) {
    throw new Error('passed evidence verdict contains incomplete or unaccepted sample evidence')
  }
  return Object.freeze({
    ...common,
    verdictKind: 'evidence',
    topologyAuthority,
    infrastructureCauses: Object.freeze([]) as readonly [],
    infrastructureDiagnostics: Object.freeze([]) as readonly [],
    samples: Object.freeze(evaluated),
  })
}

function parseInfrastructureVerdict(
  record: Record<string, unknown>,
): BrowserEvidenceInfrastructureVerdict {
  requireExactKeys(record, [
    'schemaVersion',
    'verdictKind',
    'runId',
    'checkoutSha',
    'topologyAuthority',
    'observedSuiteAuthorities',
    'infrastructureCauses',
    'infrastructureDiagnostics',
    'browsers',
    'runPolicy',
    'verdict',
    'violations',
    'samples',
  ], [], 'browser infrastructure verdict')
  const common = parseVerdictCommon(record)
  if (record.topologyAuthority !== null) {
    throw new Error('infrastructure verdict cannot claim cross-suite topology authority')
  }
  if (common.verdict !== 'failed' || common.violations.length === 0) {
    throw new Error('infrastructure verdict must be a failed verdict with violations')
  }
  if (!sameOrderedStrings(common.browsers, BROWSER_ENGINES)) {
    throw new Error('infrastructure verdict must summarize the canonical browser vocabulary')
  }
  const observedSuiteAuthorities = parseObservedSuiteAuthorities(record.observedSuiteAuthorities)
  const infrastructureCauses = parseInfrastructureCauseArray(
    record.infrastructureCauses,
    'browser verdict infrastructure causes',
  )
  const infrastructureDiagnostics = parseInfrastructureDiagnostics(
    record.infrastructureDiagnostics,
    infrastructureCauses,
  )
  const samples = parseVerdictSamples(record.samples, common.runPolicy, common.browsers, {
    main: observedSuiteAuthorities.main === null ? 'infrastructure-unavailable' : 'evidence',
    pion: observedSuiteAuthorities.pion === null ? 'infrastructure-unavailable' : 'evidence',
  })
  validateUnavailableSampleCauses(samples, infrastructureCauses)
  return Object.freeze({
    ...common,
    verdictKind: 'infrastructure',
    verdict: 'failed',
    topologyAuthority: null,
    observedSuiteAuthorities,
    infrastructureCauses,
    infrastructureDiagnostics,
    samples,
  })
}

function validateUnavailableSampleCauses(
  samples: readonly BrowserEvidenceSampleSummary[],
  infrastructureCauses: readonly BrowserVerdictInfrastructureCause[],
): void {
  for (const sample of samples) {
    if (sample.summaryKind !== 'infrastructure-unavailable') continue
    const relevant = infrastructureCauses.some((cause) =>
      cause.suite === null || cause.suite === sample.suite)
    if (!relevant) {
      throw new Error('unavailable sample has no relevant top-level infrastructure cause')
    }
  }
}

interface ParsedVerdictCommon {
  readonly schemaVersion: typeof BROWSER_VERDICT_SCHEMA_VERSION
  readonly runId: string
  readonly checkoutSha: string
  readonly browsers: readonly BrowserEngine[]
  readonly runPolicy: BrowserRunPolicy
  readonly verdict: 'passed' | 'failed'
  readonly violations: readonly string[]
}

function parseVerdictCommon(record: Record<string, unknown>): ParsedVerdictCommon {
  const browsers = requireArray(record.browsers, 'browser verdict browsers').map((browser) =>
    requireEnum(browser, BROWSER_ENGINES, 'browser verdict browser'))
  if (
    browsers.length === 0 || new Set(browsers).size !== browsers.length ||
    !sameOrderedStrings(browsers, [...browsers].sort(compareStrings))
  ) throw new Error('browser verdict browsers must be non-empty, unique, and sorted')
  const violations = requireArray(record.violations, 'browser verdict violations').map(
    (violation) => requireString(violation, 'browser verdict violation', 4_096),
  )
  if (!sameOrderedStrings(violations, [...new Set(violations)].sort(compareStrings))) {
    throw new Error('browser verdict violations must be unique and sorted')
  }
  const identity = validateVerdictIdentity({
    runId: record.runId,
    checkoutSha: record.checkoutSha,
    runPolicy: record.runPolicy,
  })
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      BROWSER_VERDICT_SCHEMA_VERSION,
      'browser verdict schema version',
    ),
    runId: identity.runId,
    checkoutSha: identity.checkoutSha,
    browsers: Object.freeze(browsers),
    runPolicy: identity.runPolicy,
    verdict: requireEnum(record.verdict, ['passed', 'failed'] as const, 'browser verdict outcome'),
    violations: Object.freeze(violations),
  })
}

function parseCompleteTopologyAuthority(
  value: unknown,
): Readonly<Record<BrowserSuite, BrowserEvidenceSuiteTopologyAuthority>> {
  const record = requireRecord(value, 'browser verdict topology authority')
  requireExactKeys(record, BROWSER_SUITES, [], 'browser verdict topology authority')
  return Object.freeze({
    main: parseSuiteTopologyAuthority(record.main),
    pion: parseSuiteTopologyAuthority(record.pion),
  })
}

function parseObservedSuiteAuthorities(
  value: unknown,
): Readonly<Record<BrowserSuite, BrowserEvidenceSuiteTopologyAuthority | null>> {
  const record = requireRecord(value, 'browser verdict observed suite authorities')
  requireExactKeys(record, BROWSER_SUITES, [], 'browser verdict observed suite authorities')
  return Object.freeze({
    main: record.main === null ? null : parseSuiteTopologyAuthority(record.main),
    pion: record.pion === null ? null : parseSuiteTopologyAuthority(record.pion),
  })
}

function parseSuiteTopologyAuthority(value: unknown): BrowserEvidenceSuiteTopologyAuthority {
  const record = requireRecord(value, 'browser verdict suite topology authority')
  requireExactKeys(record, [
    'topologyId',
    'topologyProfileSha256',
    'topologyResolutionSha256',
  ], [], 'browser verdict suite topology authority')
  return Object.freeze({
    topologyId: requireString(record.topologyId, 'browser verdict topology ID', 128),
    topologyProfileSha256: requireSha256(
      record.topologyProfileSha256,
      'browser verdict topology profile SHA-256',
    ),
    topologyResolutionSha256: requireSha256(
      record.topologyResolutionSha256,
      'browser verdict topology resolution SHA-256',
    ),
  })
}

function parseVerdictSamples(
  value: unknown,
  runPolicy: BrowserRunPolicy,
  browsers: readonly BrowserEngine[],
  expectedKinds: Readonly<Record<BrowserSuite, BrowserEvidenceSampleSummary['summaryKind']>>,
): readonly BrowserEvidenceSampleSummary[] {
  const values = requireArray(value, 'browser verdict samples')
  const expected = expectedSampleKeys(runPolicy, browsers)
  if (values.length !== expected.length) throw new Error('browser verdict has the wrong sample summary count')
  return Object.freeze(values.map((item, index) => {
    const sample = parseVerdictSample(item, runPolicy)
    const identity = expected[index]
    if (
      identity === undefined || sample.suite !== identity.suite ||
      sample.browser !== identity.browser || sample.sampleIndex !== identity.sampleIndex
    ) throw new Error('browser verdict sample summaries are not in canonical identity order')
    if (sample.summaryKind !== expectedKinds[sample.suite]) {
      throw new Error('browser verdict sample summary kind contradicts its suite authority')
    }
    return sample
  }))
}

function parseVerdictSample(
  value: unknown,
  runPolicy: BrowserRunPolicy,
): BrowserEvidenceSampleSummary {
  const record = requireRecord(value, 'browser verdict sample summary')
  const summaryKind = requireEnum(
    record.summaryKind,
    ['evidence', 'infrastructure-unavailable'] as const,
    'browser verdict sample summary kind',
  )
  const identity = {
    suite: requireEnum(record.suite, BROWSER_SUITES, 'browser verdict sample suite'),
    browser: requireEnum(record.browser, BROWSER_ENGINES, 'browser verdict sample browser'),
    sampleIndex: validatePolicySampleIndex(requireSafeInteger(
      record.sampleIndex,
      1,
      runPolicy.sampleCount,
      'browser verdict sample index',
    ), runPolicy, 'browser verdict sample index'),
  }
  if (summaryKind === 'infrastructure-unavailable') {
    requireExactKeys(record, [
      'summaryKind', 'suite', 'browser', 'sampleIndex',
    ], [], 'browser verdict unavailable sample summary')
    return Object.freeze({ summaryKind, ...identity })
  }
  requireExactKeys(record, [
    'summaryKind', 'suite', 'browser', 'sampleIndex',
    'resultPresent', 'guardPresent', 'accepted',
  ], [], 'browser verdict evaluated sample summary')
  const resultPresent = requireBoolean(record.resultPresent, 'browser verdict sample result presence')
  const guardPresent = requireBoolean(record.guardPresent, 'browser verdict sample guard presence')
  const accepted = requireBoolean(record.accepted, 'browser verdict sample acceptance')
  if (accepted && (!resultPresent || !guardPresent)) {
    throw new Error('accepted browser verdict sample lacks result or guard evidence')
  }
  return Object.freeze({ summaryKind, ...identity, resultPresent, guardPresent, accepted })
}

function parseInfrastructureCauseArray(
  value: unknown,
  label: string,
): NonemptyReadonlyArray<BrowserVerdictInfrastructureCause> {
  const causes = requireArray(value, label).map(parseInfrastructureCause)
  if (causes.length === 0) throw new Error(`${label} must not be empty`)
  const keys = causes.map(infrastructureCauseKey)
  if (!sameOrderedStrings(keys, [...new Set(keys)].sort(compareStrings))) {
    throw new Error(`${label} must be unique and sorted`)
  }
  return asNonempty(causes)
}

function parseInfrastructureCause(value: unknown): BrowserVerdictInfrastructureCause {
  const record = requireRecord(value, 'browser verdict infrastructure cause')
  const code = requireEnum(
    record.code,
    BROWSER_VERDICT_INFRASTRUCTURE_CAUSE_CODES,
    'browser verdict infrastructure cause code',
  )
  if (code === 'suite-download-unavailable') {
    requireExactKeys(record, ['code', 'suite', 'downloadKind'], [], 'browser verdict infrastructure cause')
    return Object.freeze({
      code,
      suite: requireEnum(record.suite, BROWSER_SUITES, 'infrastructure cause suite'),
      downloadKind: requireEnum(
        record.downloadKind,
        BROWSER_VERDICT_DOWNLOAD_KINDS,
        'infrastructure download kind',
      ),
    })
  }
  requireExactKeys(record, ['code', 'suite'], [], 'browser verdict infrastructure cause')
  const suite = record.suite === null
    ? null
    : requireEnum(record.suite, BROWSER_SUITES, 'infrastructure cause suite')
  if ((code === 'suite-job-failed' || code === 'suite-context-invalid') && suite === null) {
    throw new Error(`${code} requires a suite-scoped cause`)
  }
  if (code === 'external-verdict-failed' && suite !== null) {
    throw new Error('external-verdict-failed must be cross-suite')
  }
  if (code === 'suite-job-failed' || code === 'suite-context-invalid') {
    return Object.freeze({ code, suite: suite as BrowserSuite })
  }
  if (code === 'external-verdict-failed') return Object.freeze({ code, suite: null })
  return Object.freeze({ code, suite })
}

function parseInfrastructureDiagnostics(
  value: unknown,
  causes: readonly BrowserVerdictInfrastructureCause[],
): readonly BrowserVerdictInfrastructureDiagnostic[] {
  const causeKeys = new Set(causes.map(infrastructureCauseKey))
  const diagnostics = requireArray(value, 'browser verdict infrastructure diagnostics').map((item) => {
    const record = requireRecord(item, 'browser verdict infrastructure diagnostic')
    requireExactKeys(record, ['cause', 'detail'], [], 'browser verdict infrastructure diagnostic')
    const cause = parseInfrastructureCause(record.cause)
    if (!causeKeys.has(infrastructureCauseKey(cause))) {
      throw new Error('infrastructure diagnostic cites a cause outside the verdict cause set')
    }
    const detail = requireString(record.detail, 'browser verdict infrastructure diagnostic detail', 96)
    if (!/^redacted-sha256:[a-f0-9]{64}$/u.test(detail)) {
      throw new Error('infrastructure diagnostic detail is not a redacted digest')
    }
    return Object.freeze({ cause, detail })
  })
  const keys = diagnostics.map((diagnostic) =>
    `${infrastructureCauseKey(diagnostic.cause)}/${diagnostic.detail}`)
  if (!sameOrderedStrings(keys, [...new Set(keys)].sort(compareStrings))) {
    throw new Error('browser verdict infrastructure diagnostics must be unique and sorted')
  }
  return Object.freeze(diagnostics)
}

function requireEmptyArray(value: unknown, label: string): void {
  if (requireArray(value, label).length !== 0) throw new Error(`${label} must be empty`)
}

export function validateVerdictIdentity(options: {
  readonly runId: unknown
  readonly checkoutSha: unknown
  readonly runPolicy: unknown
}): BrowserEvidenceIdentityExpectation {
  const runId = requireString(options.runId, 'browser verdict run ID', 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(runId)) throw new Error('browser verdict run ID is not portable')
  return Object.freeze({
    runId,
    checkoutSha: requireCheckoutSha(options.checkoutSha, 'browser verdict checkout SHA'),
    runPolicy: parseBrowserRunPolicy(options.runPolicy, 'browser verdict run policy'),
    browsers: BROWSER_ENGINES,
  })
}

export function suiteTopologyAuthority(
  lock: VerifiedTestIceTopologyLock,
): BrowserEvidenceSuiteTopologyAuthority {
  return Object.freeze({
    topologyId: lock.profile.topologyId,
    topologyProfileSha256: lock.profileSha256,
    topologyResolutionSha256: lock.resolutionSha256,
  })
}

export function expectedSampleKeys(
  runPolicy: BrowserRunPolicy,
  browsers: readonly BrowserEngine[],
  suites: readonly BrowserSuite[] = BROWSER_SUITES,
): readonly ExpectedSample[] {
  const result: ExpectedSample[] = []
  for (const suite of suites) {
    for (const browser of browsers) {
      for (let sampleIndex = 1; sampleIndex <= runPolicy.sampleCount; sampleIndex += 1) {
        result.push(Object.freeze({ suite, browser, sampleIndex, key: keyOf(suite, browser, sampleIndex) }))
      }
    }
  }
  return Object.freeze(result)
}

export function sampleKey(identity: {
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
}): string {
  return keyOf(identity.suite, identity.browser, identity.sampleIndex)
}

export function keyOf(suite: BrowserSuite, browser: BrowserEngine, sampleIndex: number): string {
  return `${suite}/${browser}/${sampleIndex}`
}

export function infrastructureCauseKey(cause: BrowserVerdictInfrastructureCause): string {
  return cause.code === 'suite-download-unavailable'
    ? `${cause.code}/${cause.suite}/${cause.downloadKind}`
    : `${cause.code}/${cause.suite ?? 'cross-suite'}`
}

export function normalizedViolations(violations: readonly string[]): readonly string[] {
  return Object.freeze([...new Set(violations)].sort(compareStrings))
}

function asNonempty<T>(values: readonly T[]): NonemptyReadonlyArray<T> {
  if (values.length === 0) throw new Error('expected a non-empty value set')
  return Object.freeze([...values]) as unknown as NonemptyReadonlyArray<T>
}

function sameOrderedStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

export function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
