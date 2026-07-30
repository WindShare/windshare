import { sha256Bytes } from '../artifact/manifest.ts'
import { requireEnum, requireExactKeys, requireRecord } from '../contract/json.ts'
import { readVerifiedTestIceTopologyLock, type VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_SUITES,
  type BrowserSuite,
} from '../vocabulary.ts'
import {
  BROWSER_VERDICT_DOWNLOAD_KINDS,
  BROWSER_VERDICT_INFRASTRUCTURE_CAUSE_CODES,
  BROWSER_VERDICT_SCHEMA_VERSION,
  compareStrings,
  expectedSampleKeys,
  infrastructureCauseKey,
  normalizedViolations,
  sampleKey,
  suiteTopologyAuthority,
  validateVerdictIdentity,
  type BrowserEvidenceInfrastructureAggregateOptions,
  type BrowserEvidenceInfrastructureVerdict,
  type BrowserEvidenceSuiteUploadInput,
  type BrowserEvidenceSuiteTopologyAuthority,
  type BrowserVerdictInfrastructureCause,
  type BrowserVerdictInfrastructureCauseInput,
  type BrowserVerdictInfrastructureDiagnostic,
  type NonemptyReadonlyArray,
} from './contract.ts'
import { evaluateBrowserEvidence } from './evidence.ts'

interface NormalizedInfrastructureCauses {
  readonly causes: NonemptyReadonlyArray<BrowserVerdictInfrastructureCause>
  readonly diagnostics: readonly BrowserVerdictInfrastructureDiagnostic[]
}

export async function aggregateInfrastructureBrowserEvidence(
  options: BrowserEvidenceInfrastructureAggregateOptions,
): Promise<BrowserEvidenceInfrastructureVerdict> {
  const identity = validateVerdictIdentity(options)
  const normalizedCauses = normalizeInfrastructureCauses(options.causes)
  const topologyLocks: Partial<Record<BrowserSuite, VerifiedTestIceTopologyLock>> = {}
  const observedSuiteAuthorities: Record<BrowserSuite, BrowserEvidenceSuiteTopologyAuthority | null> = {
    main: null,
    pion: null,
  }
  const suiteUploads: BrowserEvidenceSuiteUploadInput[] = []
  const preexistingViolations: string[] = []
  const availableSuites: BrowserSuite[] = []
  for (const suite of BROWSER_SUITES) {
    const available = options.suiteEvidence[suite]
    if (available === null) continue
    const lock = readVerifiedTestIceTopologyLock(available.topologyLock)
    topologyLocks[suite] = lock
    observedSuiteAuthorities[suite] = suiteTopologyAuthority(lock)
    if (available.upload.suite !== suite) {
      throw new Error('available suite upload contradicts its suite authority slot')
    }
    suiteUploads.push(available.upload)
    preexistingViolations.push(...(available.preexistingViolations ?? []))
    availableSuites.push(suite)
  }
  const evaluated = await evaluateBrowserEvidence(
    Object.freeze({ ...identity, topologyLocks: Object.freeze(topologyLocks) }),
    availableSuites,
    suiteUploads,
    preexistingViolations,
  )
  const violations = [
    ...evaluated.violations,
    ...normalizedCauses.causes.map(infrastructureCauseViolation),
  ]
  const mainLock = topologyLocks.main
  const pionLock = topologyLocks.pion
  if (
    mainLock !== undefined && pionLock !== undefined &&
    (mainLock.profileSha256 !== pionLock.profileSha256 ||
      mainLock.profile.topologyId !== pionLock.profile.topologyId)
  ) violations.push('available suite topology authorities do not share one profile authority')

  const evaluatedByKey = new Map(evaluated.samples.map((sample) => [sampleKey(sample), sample]))
  const samples = expectedSampleKeys(identity.runPolicy, BROWSER_ENGINES).map(({
    suite,
    browser,
    sampleIndex,
    key,
  }) => {
    const evaluatedSample = evaluatedByKey.get(key)
    if (evaluatedSample !== undefined) return evaluatedSample
    const causes = normalizedCauses.causes.filter((cause) => cause.suite === null || cause.suite === suite)
    if (causes.length === 0) {
      throw new Error(`unavailable ${suite} evidence has no suite-applicable infrastructure cause`)
    }
    return Object.freeze({
      summaryKind: 'infrastructure-unavailable' as const,
      suite,
      browser,
      sampleIndex,
    })
  })
  return Object.freeze({
    schemaVersion: BROWSER_VERDICT_SCHEMA_VERSION,
    verdictKind: 'infrastructure',
    runId: identity.runId,
    checkoutSha: identity.checkoutSha,
    topologyAuthority: null,
    observedSuiteAuthorities: Object.freeze(observedSuiteAuthorities),
    infrastructureCauses: normalizedCauses.causes,
    infrastructureDiagnostics: normalizedCauses.diagnostics,
    browsers: BROWSER_ENGINES,
    runPolicy: identity.runPolicy,
    verdict: 'failed',
    violations: normalizedViolations(violations),
    samples: Object.freeze(samples),
  })
}

function normalizeInfrastructureCauses(
  inputs: NonemptyReadonlyArray<BrowserVerdictInfrastructureCauseInput>,
): NormalizedInfrastructureCauses {
  const causesByKey = new Map<string, BrowserVerdictInfrastructureCause>()
  const diagnosticFingerprints = new Map<string, string>()
  for (const input of inputs) {
    const normalized = normalizeInfrastructureCause(input)
    const key = infrastructureCauseKey(normalized.cause)
    causesByKey.set(key, normalized.cause)
    if (normalized.detail !== undefined) {
      diagnosticFingerprints.set(`${key}/${normalized.detail}`, normalized.detail)
    }
  }
  const causes = [...causesByKey.entries()]
    .sort(([left], [right]) => compareStrings(left, right))
    .map(([, cause]) => cause)
  if (causes.length === 0) throw new Error('infrastructure verdict requires at least one typed cause')
  const causeByKey = new Map(causes.map((cause) => [infrastructureCauseKey(cause), cause]))
  const diagnostics = [...diagnosticFingerprints.entries()]
    .sort(([left], [right]) => compareStrings(left, right))
    .map(([key, detail]) => {
      const causeKey = key.slice(0, key.lastIndexOf('/'))
      const cause = causeByKey.get(causeKey)
      if (cause === undefined) throw new Error('infrastructure diagnostic lost its typed cause')
      return Object.freeze({ cause, detail })
    })
  return Object.freeze({
    causes: asNonempty(causes),
    diagnostics: Object.freeze(diagnostics),
  })
}

function normalizeInfrastructureCause(input: BrowserVerdictInfrastructureCauseInput): {
  readonly cause: BrowserVerdictInfrastructureCause
  readonly detail?: string
} {
  const record = requireRecord(input, 'browser verdict infrastructure cause')
  const code = requireEnum(
    record.code,
    BROWSER_VERDICT_INFRASTRUCTURE_CAUSE_CODES,
    'browser verdict infrastructure cause code',
  )
  const optionalDetail = Object.hasOwn(record, 'detail')
    ? sanitizedInfrastructureDetail(record.detail)
    : undefined
  if (code === 'suite-download-unavailable') {
    requireExactKeys(record, ['code', 'suite', 'downloadKind'], ['detail'], 'browser verdict infrastructure cause')
    return Object.freeze({
      cause: Object.freeze({
        code,
        suite: requireEnum(record.suite, BROWSER_SUITES, 'infrastructure cause suite'),
        downloadKind: requireEnum(
          record.downloadKind,
          BROWSER_VERDICT_DOWNLOAD_KINDS,
          'infrastructure download kind',
        ),
      }),
      ...(optionalDetail === undefined ? {} : { detail: optionalDetail }),
    })
  }
  requireExactKeys(record, ['code', 'suite'], ['detail'], 'browser verdict infrastructure cause')
  const suite = record.suite === null
    ? null
    : requireEnum(record.suite, BROWSER_SUITES, 'infrastructure cause suite')
  if ((code === 'suite-job-failed' || code === 'suite-context-invalid') && suite === null) {
    throw new Error(`${code} requires a suite-scoped cause`)
  }
  if (code === 'external-verdict-failed' && suite !== null) {
    throw new Error('external-verdict-failed must be cross-suite')
  }
  let cause: BrowserVerdictInfrastructureCause
  if (code === 'suite-job-failed' || code === 'suite-context-invalid') {
    cause = Object.freeze({ code, suite: suite as BrowserSuite })
  } else if (code === 'external-verdict-failed') {
    cause = Object.freeze({ code, suite: null })
  } else {
    cause = Object.freeze({ code, suite })
  }
  return Object.freeze({
    cause,
    ...(optionalDetail === undefined ? {} : { detail: optionalDetail }),
  })
}

function sanitizedInfrastructureDetail(value: unknown): string | undefined {
  if (typeof value !== 'string') throw new Error('infrastructure cause detail must be text')
  const encoded = new TextEncoder().encode(value)
  if (encoded.byteLength === 0) return undefined
  const bounded = encoded.subarray(0, 4_096)
  return `redacted-sha256:${sha256Bytes(bounded)}`
}

function infrastructureCauseViolation(cause: BrowserVerdictInfrastructureCause): string {
  const scope = cause.suite ?? 'cross-suite'
  return cause.code === 'suite-download-unavailable'
    ? `browser verdict infrastructure cause ${cause.code} for ${scope} ${cause.downloadKind}`
    : `browser verdict infrastructure cause ${cause.code} for ${scope}`
}

function asNonempty<T>(values: readonly T[]): NonemptyReadonlyArray<T> {
  if (values.length === 0) throw new Error('expected a non-empty value set')
  return Object.freeze([...values]) as unknown as NonemptyReadonlyArray<T>
}
