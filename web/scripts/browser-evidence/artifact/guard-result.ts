import {
  contractError,
  freezeRecord,
  optionalField,
  requireArray,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from '../contract/json.ts'
import { artifactManifestSha256 as digestArtifactManifest } from './manifest.ts'
import { requirePortableRelativePath } from '../filesystem/portable-path.ts'
import {
  assertBrowserRunPolicyEqual,
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
import type { BrowserSampleResult } from '../result.ts'

export const ARTIFACT_GUARD_SCHEMA_VERSION = 2 as const
export const GUARD_OUTCOMES = Object.freeze(['not-started', 'passed', 'quarantined', 'failed'] as const)
export const GUARD_SCAN_TERMINALS = Object.freeze(['not-started', 'completed', 'failed'] as const)
export const GUARD_FAILURE_CODES = Object.freeze([
  'scanner-crashed',
  'invalid-archive',
  'archive-byte-limit',
  'archive-entry-limit',
  'archive-expanded-byte-limit',
  'archive-nesting-limit',
  'archive-path',
  'contract',
  'unexpected',
] as const)
export const GUARD_MATCH_LOCATIONS = Object.freeze(['file', 'archive-entry'] as const)
export const GUARD_DETECTORS = Object.freeze(['explicit-secret', 'github-token-pattern'] as const)
export const GUARD_MAXIMUM_ARTIFACT_FILES = 10_000 as const
export const GUARD_MAXIMUM_ARTIFACT_FILE_BYTES = 536_870_912 as const
export const GUARD_MAXIMUM_TOTAL_ARTIFACT_BYTES = 2_147_483_648 as const
export const GUARD_MAXIMUM_ARCHIVE_BYTES = 536_870_912 as const
export const GUARD_MAXIMUM_ARCHIVE_ENTRIES = 10_000 as const
export const GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES = 2_147_483_648 as const
export const GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH = 1 as const

export type GuardOutcome = (typeof GUARD_OUTCOMES)[number]

export interface GuardScanEvidence {
  readonly terminal: (typeof GUARD_SCAN_TERMINALS)[number]
  readonly scannedFileCount: number
  readonly scannedArchiveEntryCount: number
  readonly observedArchiveBytes: number
  readonly expandedArchiveBytes: number
  readonly observedMaximumArchiveDepth: number
  readonly maximumArchiveBytes: typeof GUARD_MAXIMUM_ARCHIVE_BYTES
  readonly maximumArchiveEntries: typeof GUARD_MAXIMUM_ARCHIVE_ENTRIES
  readonly maximumExpandedArchiveBytes: typeof GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES
  readonly maximumArchiveNestingDepth: typeof GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH
}

export interface GuardMatchEvidence {
  readonly artifactId: string
  readonly location: (typeof GUARD_MATCH_LOCATIONS)[number]
  readonly archiveEntryPath: string | null
  readonly detector: (typeof GUARD_DETECTORS)[number]
}

export interface ArtifactGuardResult {
  readonly schemaVersion: typeof ARTIFACT_GUARD_SCHEMA_VERSION
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly sampleResultSha256: string
  readonly artifactManifestSha256: string
  readonly guardOutcome: GuardOutcome
  readonly scanEvidence: GuardScanEvidence
  readonly checkedArtifactIds: readonly string[]
  readonly uploadableArtifactIds: readonly string[]
  readonly quarantinedArtifactIds: readonly string[]
  readonly matches: readonly GuardMatchEvidence[]
  readonly failureCode?: (typeof GUARD_FAILURE_CODES)[number]
  readonly failureMessage?: string
}

export function parseArtifactGuardResult(value: unknown): ArtifactGuardResult {
  const result = requireRecord(value, 'artifact guard result')
  requireExactKeys(
    result,
    [
      'schemaVersion',
      'runId',
      'runPolicy',
      'suite',
      'browser',
      'sampleIndex',
      'checkoutSha',
      'sampleResultSha256',
      'artifactManifestSha256',
      'guardOutcome',
      'scanEvidence',
      'checkedArtifactIds',
      'uploadableArtifactIds',
      'quarantinedArtifactIds',
      'matches',
    ],
    ['failureCode', 'failureMessage'],
    'artifact guard result',
  )
  const guardOutcome = requireEnum(result.guardOutcome, GUARD_OUTCOMES, 'artifact guard outcome')
  const scanEvidence = parseGuardScanEvidence(result.scanEvidence)
  const checkedArtifactIds = parseUniqueArtifactIds(result.checkedArtifactIds, 'checked artifact IDs')
  const uploadableArtifactIds = parseUniqueArtifactIds(result.uploadableArtifactIds, 'uploadable artifact IDs')
  const quarantinedArtifactIds = parseUniqueArtifactIds(
    result.quarantinedArtifactIds,
    'quarantined artifact IDs',
  )
  const matches = Object.freeze(requireArray(result.matches, 'guard matches')
    .map(parseGuardMatch)
    .sort(compareGuardMatches))
  if (new Set(matches.map(guardMatchKey)).size !== matches.length) {
    contractError('artifact guard matches contain duplicates')
  }
  const failureCodeValue = optionalField(result, 'failureCode')
  const failureMessageValue = optionalField(result, 'failureMessage')
  const failureCode = failureCodeValue === undefined
    ? undefined
    : requireEnum(failureCodeValue, GUARD_FAILURE_CODES, 'artifact guard failure code')
  const failureMessage = failureMessageValue === undefined
    ? undefined
    : requireString(failureMessageValue, 'artifact guard failure message', 512)
  validateGuardCombination(
    guardOutcome,
    scanEvidence,
    checkedArtifactIds,
    uploadableArtifactIds,
    quarantinedArtifactIds,
    matches,
    failureCode,
    failureMessage,
  )
  const runPolicy = parseBrowserRunPolicy(result.runPolicy, 'artifact guard run policy')
  const sampleIndex = validatePolicySampleIndex(requireSafeInteger(
    result.sampleIndex,
    1,
    runPolicy.sampleCount,
    'artifact guard sample index',
  ), runPolicy, 'artifact guard sample index')
  return freezeRecord({
    schemaVersion: requireLiteral(
      result.schemaVersion,
      ARTIFACT_GUARD_SCHEMA_VERSION,
      'artifact guard schema version',
    ),
    runId: requirePortableToken(result.runId, 'artifact guard run ID'),
    runPolicy,
    suite: requireEnum(result.suite, BROWSER_SUITES, 'artifact guard suite'),
    browser: requireEnum(result.browser, BROWSER_ENGINES, 'artifact guard browser'),
    sampleIndex,
    checkoutSha: requireCheckoutSha(result.checkoutSha, 'artifact guard checkout SHA'),
    sampleResultSha256: requireSha256(
      result.sampleResultSha256,
      'artifact guard sample result SHA-256',
    ),
    artifactManifestSha256: requireSha256(
      result.artifactManifestSha256,
      'artifact guard manifest SHA-256',
    ),
    guardOutcome,
    scanEvidence,
    checkedArtifactIds,
    uploadableArtifactIds,
    quarantinedArtifactIds,
    matches,
    ...(failureCode === undefined ? {} : { failureCode }),
    ...(failureMessage === undefined ? {} : { failureMessage }),
  })
}

function parseGuardScanEvidence(value: unknown): GuardScanEvidence {
  const scan = requireRecord(value, 'guard scan evidence')
  requireExactKeys(
    scan,
    [
      'terminal',
      'scannedFileCount',
      'scannedArchiveEntryCount',
      'observedArchiveBytes',
      'expandedArchiveBytes',
      'observedMaximumArchiveDepth',
      'maximumArchiveBytes',
      'maximumArchiveEntries',
      'maximumExpandedArchiveBytes',
      'maximumArchiveNestingDepth',
    ],
    [],
    'guard scan evidence',
  )
  return freezeRecord({
    terminal: requireEnum(scan.terminal, GUARD_SCAN_TERMINALS, 'guard scan terminal'),
    scannedFileCount: requireCounter(scan.scannedFileCount, 'guard scanned file count'),
    scannedArchiveEntryCount: requireCounter(
      scan.scannedArchiveEntryCount,
      'guard scanned archive entry count',
    ),
    observedArchiveBytes: requireCounter(scan.observedArchiveBytes, 'guard observed archive bytes'),
    expandedArchiveBytes: requireCounter(scan.expandedArchiveBytes, 'guard expanded archive bytes'),
    observedMaximumArchiveDepth: requireCounter(
      scan.observedMaximumArchiveDepth,
      'guard observed maximum archive depth',
    ),
    maximumArchiveBytes: requireLiteral(
      scan.maximumArchiveBytes,
      GUARD_MAXIMUM_ARCHIVE_BYTES,
      'guard maximum archive bytes',
    ),
    maximumArchiveEntries: requireLiteral(
      scan.maximumArchiveEntries,
      GUARD_MAXIMUM_ARCHIVE_ENTRIES,
      'guard maximum archive entries',
    ),
    maximumExpandedArchiveBytes: requireLiteral(
      scan.maximumExpandedArchiveBytes,
      GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
      'guard maximum expanded archive bytes',
    ),
    maximumArchiveNestingDepth: requireLiteral(
      scan.maximumArchiveNestingDepth,
      GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
      'guard maximum archive nesting depth',
    ),
  })
}

function parseGuardMatch(value: unknown, index: number): GuardMatchEvidence {
  const match = requireRecord(value, `guard match ${index}`)
  requireExactKeys(
    match,
    ['artifactId', 'location', 'archiveEntryPath', 'detector'],
    [],
    `guard match ${index}`,
  )
  const location = requireEnum(match.location, GUARD_MATCH_LOCATIONS, `guard match ${index} location`)
  const archiveEntryPath = match.archiveEntryPath === null
    ? null
    : requireRelativePath(match.archiveEntryPath, `guard match ${index} archive entry path`)
  if ((location === 'file') !== (archiveEntryPath === null)) {
    contractError(`guard match ${index} location contradicts its archive entry path`)
  }
  return freezeRecord({
    artifactId: requirePortableToken(match.artifactId, `guard match ${index} artifact ID`),
    location,
    archiveEntryPath,
    detector: requireEnum(match.detector, GUARD_DETECTORS, `guard match ${index} detector`),
  })
}

function validateGuardCombination(
  outcome: GuardOutcome,
  scan: GuardScanEvidence,
  checked: readonly string[],
  uploadable: readonly string[],
  quarantined: readonly string[],
  matches: readonly GuardMatchEvidence[],
  failureCode: (typeof GUARD_FAILURE_CODES)[number] | undefined,
  failureMessage: string | undefined,
): void {
  validateGuardFailureAuthority(outcome, failureCode, failureMessage)
  if (outcome === 'not-started') {
    validateNotStartedGuard(scan, checked, uploadable, quarantined, matches)
    return
  }
  if (outcome === 'failed') {
    validateFailedGuard(scan, checked, uploadable, quarantined, matches, failureCode)
    return
  }
  validateCompletedGuard(outcome, scan, checked, uploadable, quarantined, matches)
}

function validateGuardFailureAuthority(
  outcome: GuardOutcome,
  failureCode: (typeof GUARD_FAILURE_CODES)[number] | undefined,
  failureMessage: string | undefined,
): void {
  const hasFailure = failureCode !== undefined || failureMessage !== undefined
  if ((outcome === 'failed') !== (failureCode !== undefined && failureMessage !== undefined)) {
    contractError('only failed artifact guard result must carry complete failure authority')
  }
  if (outcome !== 'failed' && hasFailure) {
    contractError('non-failed artifact guard result cannot carry failure authority')
  }
}

function validateNotStartedGuard(
  scan: GuardScanEvidence,
  checked: readonly string[],
  uploadable: readonly string[],
  quarantined: readonly string[],
  matches: readonly GuardMatchEvidence[],
): void {
  const allZero = [
    scan.scannedFileCount,
    scan.scannedArchiveEntryCount,
    scan.observedArchiveBytes,
    scan.expandedArchiveBytes,
    scan.observedMaximumArchiveDepth,
  ].every((count) => count === 0)
  if (
    scan.terminal !== 'not-started' || !allZero || checked.length !== 0 ||
    uploadable.length !== 0 || quarantined.length !== 0 || matches.length !== 0
  ) {
    contractError('not-started artifact guard result cannot assert scan or upload evidence')
  }
}

function validateFailedGuard(
  scan: GuardScanEvidence,
  checked: readonly string[],
  uploadable: readonly string[],
  quarantined: readonly string[],
  matches: readonly GuardMatchEvidence[],
  failureCode: (typeof GUARD_FAILURE_CODES)[number] | undefined,
): void {
  const checkedSet = new Set(checked)
  if (
    scan.terminal !== 'failed' || uploadable.length !== 0 ||
    !sameStringSet(checkedSet, new Set(quarantined)) ||
    scan.scannedFileCount > checked.length ||
    matches.some((match) => !checkedSet.has(match.artifactId))
  ) {
    contractError('failed artifact guard must fail closed and authorize no upload')
  }
  validateGuardFailureCause(scan, failureCode)
}

function validateCompletedGuard(
  outcome: Extract<GuardOutcome, 'passed' | 'quarantined'>,
  scan: GuardScanEvidence,
  checked: readonly string[],
  uploadable: readonly string[],
  quarantined: readonly string[],
  matches: readonly GuardMatchEvidence[],
): void {
  if (scan.terminal !== 'completed') {
    contractError('passed or quarantined artifact guard requires completed scan evidence')
  }
  if (
    scan.scannedFileCount !== checked.length ||
    scan.scannedArchiveEntryCount > scan.maximumArchiveEntries ||
    scan.observedArchiveBytes > scan.maximumArchiveBytes ||
    scan.expandedArchiveBytes > scan.maximumExpandedArchiveBytes ||
    scan.observedMaximumArchiveDepth > scan.maximumArchiveNestingDepth
  ) {
    contractError('completed artifact guard scan exceeds its frozen authority or omits checked files')
  }
  const checkedSet = new Set(checked)
  const uploadableSet = new Set(uploadable)
  const quarantinedSet = new Set(quarantined)
  if (
    [...uploadableSet].some((id) => quarantinedSet.has(id)) ||
    [...uploadableSet, ...quarantinedSet].some((id) => !checkedSet.has(id)) ||
    uploadableSet.size + quarantinedSet.size !== checkedSet.size
  ) {
    contractError('artifact guard upload and quarantine sets must partition checked artifacts')
  }
  const matchedIds = new Set(matches.map((match) => match.artifactId))
  if ([...matchedIds].some((id) => !checkedSet.has(id))) {
    contractError('artifact guard match names an unchecked artifact')
  }
  if (
    (outcome === 'passed' && (matches.length !== 0 || quarantined.length !== 0)) ||
    (outcome === 'quarantined' &&
      (matches.length === 0 || !sameStringSet(matchedIds, quarantinedSet)))
  ) {
    contractError('artifact guard outcome does not match secret-match quarantine evidence')
  }
}

function parseUniqueArtifactIds(value: unknown, label: string): readonly string[] {
  const ids = requireArray(value, label).map((item, index) => requirePortableToken(item, `${label} ${index}`))
  if (new Set(ids).size !== ids.length) contractError(`${label} contains duplicates`)
  return Object.freeze(ids.sort(compareStrings))
}

function requireCounter(value: unknown, label: string): number {
  return requireSafeInteger(value, 0, Number.MAX_SAFE_INTEGER, label)
}

function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) contractError(`${label} contains non-portable characters`)
  return token
}

function requireRelativePath(value: unknown, label: string): string {
  try {
    return requirePortableRelativePath(value, label)
  } catch (cause) {
    contractError(cause instanceof Error ? cause.message : String(cause))
  }
}

function sameStringSet(left: ReadonlySet<string>, right: ReadonlySet<string>): boolean {
  return left.size === right.size && [...left].every((item) => right.has(item))
}

function validateGuardFailureCause(
  scan: GuardScanEvidence,
  failureCode: (typeof GUARD_FAILURE_CODES)[number] | undefined,
): void {
  if (failureCode === undefined) contractError('failed artifact guard lacks a failure code')
  let limitExceeded = true
  if (failureCode === 'archive-byte-limit') {
    limitExceeded = scan.observedArchiveBytes > scan.maximumArchiveBytes
  } else if (failureCode === 'archive-entry-limit') {
    limitExceeded = scan.scannedArchiveEntryCount > scan.maximumArchiveEntries
  } else if (failureCode === 'archive-expanded-byte-limit') {
    limitExceeded = scan.expandedArchiveBytes > scan.maximumExpandedArchiveBytes
  } else if (failureCode === 'archive-nesting-limit') {
    limitExceeded = scan.observedMaximumArchiveDepth > scan.maximumArchiveNestingDepth
  }
  if (!limitExceeded) contractError(`artifact guard failure ${failureCode} lacks causal limit evidence`)
}

function compareGuardMatches(left: GuardMatchEvidence, right: GuardMatchEvidence): number {
  return compareStrings(guardMatchKey(left), guardMatchKey(right))
}

function guardMatchKey(match: GuardMatchEvidence): string {
  return `${match.artifactId}\u0000${match.location}\u0000${match.archiveEntryPath ?? ''}\u0000${match.detector}`
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

export function validateArtifactGuardForSample(
  guard: ArtifactGuardResult,
  sample: BrowserSampleResult,
  sampleResultSha256: string,
): void {
  if (
    guard.runId !== sample.runId || guard.suite !== sample.suite ||
    guard.browser !== sample.browser || guard.sampleIndex !== sample.sampleIndex ||
    guard.checkoutSha !== sample.checkoutSha
  ) {
    contractError('artifact guard identity does not match its browser sample')
  }
  assertBrowserRunPolicyEqual(guard.runPolicy, sample.runPolicy, 'artifact guard run policy')
  if (
    guard.sampleResultSha256 !== requireSha256(sampleResultSha256, 'browser sample result SHA-256')
  ) {
    contractError('artifact guard does not bind the exact browser sample result bytes')
  }
  if (guard.artifactManifestSha256 !== digestArtifactManifest(sample.artifacts)) {
    contractError('artifact guard does not bind the canonical full sample artifact manifest')
  }
  const artifactIds = sample.artifacts.map((artifact) => artifact.artifactId).sort(compareStrings)
  if (!sameOrderedStrings(guard.checkedArtifactIds, artifactIds)) {
    contractError('artifact guard checked set does not match the sample artifact index')
  }
  if (guard.guardOutcome !== 'passed') {
    contractError('artifact guard did not authorize final artifact handling')
  }
}

function sameOrderedStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((item, index) => item === right[index])
}
