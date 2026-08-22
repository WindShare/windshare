import { encodeBase64Url } from '../../../crypto/bytes'
import { sha256 } from '../../../crypto/digest'
import type {
  DirectZipSupportFacts,
  ReviewedDirectZipSupportFacts,
  ZipRouteRecommendationPolicyV1,
} from '../../planning'
import { zipRouteRecommendationPolicyDigestV1 } from '../../planning/zip-route-recommendation'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU8,
} from '../../workspace/canonical'
import { directZipPolicyDigestsV2 } from '../format'
import { directZipJournalBudgetDigestV1 } from '../journal'
import {
  directZipEpochPolicyDigestV1,
  type DirectZipAutomaticEpochBudgetV1,
} from '../writer'

const MATRIX_SCHEMA = 'windshare/browser-fsa-resumable-zip-support-matrix/v1'
const CANDIDATE_SCHEMA =
  'windshare/browser-fsa-resumable-zip-support-matrix-candidate/v1'
const CHECKPOINT_POLICY_DOMAIN = 'windshare/direct-zip-checkpoint-policy/v1'
const SHA256_HEX = /^[0-9a-f]{64}$/u
const SHA256_BASE64URL = /^[A-Za-z0-9_-]{43}$/u
const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })

export interface DirectZipRequiredFeatureFactsV1 {
  readonly createWritable: 'function' | 'missing'
  readonly handleIsSameEntry: 'function' | 'missing'
  readonly handleQueryPermission: 'function' | 'missing'
  readonly handleRequestPermission: 'function' | 'missing'
  readonly indexedDB: 'object' | 'missing'
  readonly isSecureContext: boolean
  readonly locks: 'object' | 'missing'
  readonly showDirectoryPicker: 'function' | 'missing'
}

export interface DirectZipRuntimePlatformFactsV1 {
  readonly browserExecutableSha256: string
  readonly browserVersion: string
  /** Canonical JSON of the exact matrix `platform` object. */
  readonly operatingSystemBuild: string
  /** Canonical JSON of the exact matrix `filesystem` object. */
  readonly filesystemProfile: string
  readonly featureFacts: DirectZipRequiredFeatureFactsV1
}

export interface DirectZipSupportMatrixArtifactV1 {
  /** These bytes are evidence-owner input; importing a JSON object would lose byte identity. */
  readonly canonicalBytes: Uint8Array
  readonly detachedSha256: string
}

export interface ReviewedDirectZipRuntimeFactsV1 {
  readonly support: ReviewedDirectZipSupportFacts
  readonly recommendationPolicy: Extract<ZipRouteRecommendationPolicyV1, { kind: 'available' }>
  readonly automaticEpochBudget: DirectZipAutomaticEpochBudgetV1
}

export type DirectZipSupportLookupV1 =
  | Readonly<{ readonly kind: 'available'; readonly facts: ReviewedDirectZipRuntimeFactsV1 }>
  | Readonly<{ readonly kind: 'unavailable'; readonly support: DirectZipSupportFacts }>

interface ReviewedMatrixRow {
  readonly browser: Readonly<{
    channel: string
    executableSha256: string
    version: string
  }>
  readonly entryId: string
  readonly featureFacts: DirectZipRequiredFeatureFactsV1
  readonly filesystem: Record<string, unknown>
  readonly platform: Record<string, unknown>
  readonly rawEvidenceSha256: string
  readonly repositoryCommit: string
  readonly review: Readonly<{
    directZipEpochPolicyDigest: string
    epochPolicyReviewSha256: string
    independentReviewSha256: string
    rationale: string
    reviewedAt: string
    reviewer: string
    workspaceRecommendationArtifactManifestSha256: string
    workspaceRecommendationCandidateSha256: string
    workspaceRecommendationPolicyDigest: string
    workspaceRecommendationPolicyReviewSha256: string
    workspaceRecommendationRawEvidenceSha256: string
    workspaceRecommendationSourceBindingSha256: string
  }>
  readonly runConfigSha256: string
  readonly scenarios: Readonly<{
    authorityBinding: true
    externalReplacementLocatorOnly: true
    sameTargetProcessRestartContinuation: number
  }>
  readonly sourceManifestSha256: string
  readonly supportingArtifactManifestSha256: string
  readonly verdict: Readonly<{ directLocalRoute: 'supported'; processRestart: true }>
}

interface SupportMatrix {
  readonly policyConstants: Readonly<{
    directZipAutomaticCheckpointMaximumCumulativeCopyBytes: number | null
    directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes: number | null
    directZipAutomaticCheckpointMaximumPrefixCopyBytes: number | null
    zipWorkspaceRecommendationMaximumPeakBytes: number | null
  }>
  readonly reviewedPlatforms: readonly ReviewedMatrixRow[]
}

/**
 * Positive support is an exact evidence lookup, never a capability probe. Any byte,
 * schema, browser, platform, filesystem, or feature mismatch returns the planning
 * layer's closed unavailable vocabulary.
 */
export async function lookupReviewedDirectZipSupportV1(input: Readonly<{
  readonly artifact?: DirectZipSupportMatrixArtifactV1
  readonly runtime: DirectZipRuntimePlatformFactsV1
}>): Promise<DirectZipSupportLookupV1> {
  if (input.artifact === undefined) return unavailable('support-evidence-missing')
  const matrix = await decodeVerifiedMatrix(input.artifact)
  if (matrix === undefined) return unavailable('support-evidence-missing')
  const matches = matrix.reviewedPlatforms.filter(candidate => exactRuntimeMatch(candidate, input.runtime))
  if (matches.length !== 1) return unavailable('platform-not-reviewed')
  const row = matches[0]!
  const epochConstants = epochPolicyConstants(matrix.policyConstants)
  if (epochConstants === undefined) return unavailable('policy-digests-unavailable')

  const format = await directZipPolicyDigestsV2()
  const checkpoint = await canonicalDigest(canonicalRecord(CHECKPOINT_POLICY_DOMAIN, 1, [
    canonicalFrame(format.encodingPolicy),
    canonicalFrame(format.layoutPolicy),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(2)),
    canonicalFrame(canonicalU8(3)),
  ]))
  const automaticEpochBudget = Object.freeze({
    maximumPrefixCopyBytes: epochConstants.maximumPrefixCopyBytes,
    maximumCumulativePrefixCopyBytes: epochConstants.maximumCumulativePrefixCopyBytes,
    maximumModeledPeakTemporaryBytes: epochConstants.maximumModeledPeakTemporaryBytes,
  })
  const epoch = await directZipEpochPolicyDigestV1(automaticEpochBudget)
  if (epoch !== row.review.directZipEpochPolicyDigest) return unavailable('support-evidence-missing')
  const workspacePeakBytesThreshold = workspaceRecommendationThreshold(matrix.policyConstants)
  if (workspacePeakBytesThreshold === undefined) return unavailable('policy-digests-unavailable')
  const recommendationPolicyDigest = await zipRouteRecommendationPolicyDigestV1(
    workspacePeakBytesThreshold,
  )
  if (recommendationPolicyDigest !== row.review.workspaceRecommendationPolicyDigest) {
    return unavailable('support-evidence-missing')
  }
  const matrixDigest = encodeBase64Url(await sha256(input.artifact.canonicalBytes))
  const featureFactsDigest = encodeBase64Url(await sha256(
    TEXT_ENCODER.encode(canonicalJson(row.featureFacts)),
  ))
  const support: ReviewedDirectZipSupportFacts = Object.freeze({
    kind: 'reviewed-supported',
    supportMatrixDigest: matrixDigest,
    browserBinaryDigest: hexDigestToBase64Url(row.browser.executableSha256),
    browserVersion: row.browser.version,
    operatingSystemBuild: canonicalJson(row.platform),
    filesystemProfile: canonicalJson(row.filesystem),
    rawEvidenceDigest: hexDigestToBase64Url(row.rawEvidenceSha256),
    requiredFeatureFactsDigest: featureFactsDigest,
    recommendationPolicyDigest,
    policies: Object.freeze({
      zipEncoding: encodeBase64Url(format.encodingPolicy),
      layout: encodeBase64Url(format.layoutPolicy),
      checkpoint,
      journalBudget: await directZipJournalBudgetDigestV1(),
      epoch,
    }),
  })
  return Object.freeze({
    kind: 'available',
    facts: Object.freeze({
      support,
      recommendationPolicy: Object.freeze({
        version: 1,
        kind: 'available',
        workspacePeakBytesThreshold,
        policyDigest: recommendationPolicyDigest,
      }),
      automaticEpochBudget,
    }),
  })
}

function unavailable(
  reason: Extract<DirectZipSupportFacts, { kind: 'unavailable' }>['reason'],
): DirectZipSupportLookupV1 {
  return Object.freeze({ kind: 'unavailable', support: Object.freeze({ kind: 'unavailable', reason }) })
}

async function decodeVerifiedMatrix(
  artifact: DirectZipSupportMatrixArtifactV1,
): Promise<SupportMatrix | undefined> {
  if (!(artifact.canonicalBytes instanceof Uint8Array) ||
      !SHA256_HEX.test(artifact.detachedSha256)) return undefined
  if (hex(await sha256(artifact.canonicalBytes)) !== artifact.detachedSha256) return undefined
  let parsed: unknown
  try {
    parsed = JSON.parse(TEXT_DECODER.decode(artifact.canonicalBytes))
  } catch {
    return undefined
  }
  let canonical: string
  try {
    canonical = canonicalJson(parsed)
  } catch {
    return undefined
  }
  if (!isObject(parsed) || canonical !== TEXT_DECODER.decode(artifact.canonicalBytes) ||
      !exactKeys(parsed, [
        'candidateSchema', 'defaultVerdict', 'matrixSchema', 'notes', 'policyConstants',
        'reviewStatus', 'reviewedPlatforms', 'schema',
      ]) || parsed.schema !== MATRIX_SCHEMA || parsed.matrixSchema !== MATRIX_SCHEMA ||
      parsed.candidateSchema !== CANDIDATE_SCHEMA || !Array.isArray(parsed.notes) ||
      parsed.notes.length < 3 || !parsed.notes.every(note => typeof note === 'string') ||
      !isObject(parsed.defaultVerdict) ||
      !exactKeys(parsed.defaultVerdict, ['directLocalRoute', 'processRestart']) ||
      parsed.defaultVerdict.directLocalRoute !== 'unsupported' ||
      parsed.defaultVerdict.processRestart !== 'unproven' ||
      !Array.isArray(parsed.reviewedPlatforms) || !isObject(parsed.policyConstants)) return undefined
  if ((parsed.reviewedPlatforms.length === 0) !==
      (parsed.reviewStatus === 'no-reviewed-local-evidence')) return undefined
  if (parsed.reviewedPlatforms.length > 0 && parsed.reviewStatus !== 'reviewed-local-evidence') {
    return undefined
  }
  if (!exactKeys(parsed.policyConstants, [
    'directZipAutomaticCheckpointMaximumCumulativeCopyBytes',
    'directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes',
    'directZipAutomaticCheckpointMaximumPrefixCopyBytes',
    'zipWorkspaceRecommendationMaximumPeakBytes',
  ])) return undefined
  const rows = parsed.reviewedPlatforms.map(validateReviewedRow)
  if (rows.some(row => row === undefined)) return undefined
  return Object.freeze({
    policyConstants: parsed.policyConstants as unknown as SupportMatrix['policyConstants'],
    reviewedPlatforms: Object.freeze(rows as ReviewedMatrixRow[]),
  })
}

function validateReviewedRow(value: unknown): ReviewedMatrixRow | undefined {
  if (!isObject(value) || !exactKeys(value, [
    'browser', 'entryId', 'featureFacts', 'filesystem', 'platform', 'rawEvidenceSha256',
    'repositoryCommit', 'review', 'runConfigSha256', 'scenarios', 'sourceManifestSha256',
    'supportingArtifactManifestSha256', 'verdict',
  ]) || typeof value.entryId !== 'string' ||
      !/^[a-z0-9][a-z0-9._-]{2,127}$/u.test(value.entryId) || !isObject(value.browser) ||
      !exactKeys(value.browser, ['channel', 'executableSha256', 'version']) ||
      typeof value.browser.channel !== 'string' || typeof value.browser.version !== 'string' ||
      !isSha256(value.browser.executableSha256) || !isSha256(value.rawEvidenceSha256) ||
      !isSha256(value.runConfigSha256) || !isSha256(value.sourceManifestSha256) ||
      !isSha256(value.supportingArtifactManifestSha256) ||
      typeof value.repositoryCommit !== 'string' || !/^[0-9a-f]{40}$/u.test(value.repositoryCommit) ||
      !validFeatureFacts(value.featureFacts) || !isObject(value.filesystem) ||
      !validFilesystem(value.filesystem) || !isObject(value.platform) ||
      !validPlatform(value.platform) || !isObject(value.review) ||
      !exactKeys(value.review, [
        'directZipEpochPolicyDigest', 'epochPolicyReviewSha256', 'independentReviewSha256',
        'rationale', 'reviewedAt', 'reviewer', 'workspaceRecommendationArtifactManifestSha256',
        'workspaceRecommendationCandidateSha256', 'workspaceRecommendationPolicyDigest',
        'workspaceRecommendationPolicyReviewSha256', 'workspaceRecommendationRawEvidenceSha256',
        'workspaceRecommendationSourceBindingSha256',
      ]) || !isPolicyDigest(value.review.directZipEpochPolicyDigest) ||
      !isSha256(value.review.epochPolicyReviewSha256) ||
      !isSha256(value.review.independentReviewSha256) ||
      !isSha256(value.review.workspaceRecommendationArtifactManifestSha256) ||
      !isSha256(value.review.workspaceRecommendationCandidateSha256) ||
      !isPolicyDigest(value.review.workspaceRecommendationPolicyDigest) ||
      !isSha256(value.review.workspaceRecommendationPolicyReviewSha256) ||
      !isSha256(value.review.workspaceRecommendationRawEvidenceSha256) ||
      !isSha256(value.review.workspaceRecommendationSourceBindingSha256) ||
      !nonEmptyStrings(value.review, ['rationale', 'reviewedAt', 'reviewer']) ||
      !isCanonicalDateTime(value.review.reviewedAt) ||
      !isObject(value.scenarios) || !exactKeys(value.scenarios, [
        'authorityBinding', 'externalReplacementLocatorOnly',
        'sameTargetProcessRestartContinuation',
      ]) || value.scenarios.authorityBinding !== true ||
      value.scenarios.externalReplacementLocatorOnly !== true ||
      !positiveSafeInteger(value.scenarios.sameTargetProcessRestartContinuation) ||
      !isObject(value.verdict) || !exactKeys(value.verdict, ['directLocalRoute', 'processRestart']) ||
      value.verdict.directLocalRoute !== 'supported' || value.verdict.processRestart !== true) {
    return undefined
  }
  return value as unknown as ReviewedMatrixRow
}

function exactRuntimeMatch(
  row: ReviewedMatrixRow,
  runtime: DirectZipRuntimePlatformFactsV1,
): boolean {
  return row.browser.executableSha256 === runtime.browserExecutableSha256 &&
    row.browser.version === runtime.browserVersion &&
    canonicalJson(row.platform) === runtime.operatingSystemBuild &&
    canonicalJson(row.filesystem) === runtime.filesystemProfile &&
    canonicalJson(row.featureFacts) === canonicalJson(runtime.featureFacts)
}

function epochPolicyConstants(input: SupportMatrix['policyConstants']): Readonly<{
  maximumPrefixCopyBytes: bigint
  maximumCumulativePrefixCopyBytes: bigint
  maximumModeledPeakTemporaryBytes: bigint
}> | undefined {
  const values = [
    input.directZipAutomaticCheckpointMaximumPrefixCopyBytes,
    input.directZipAutomaticCheckpointMaximumCumulativeCopyBytes,
    input.directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes,
  ]
  if (!values.every(nonNegativeSafeInteger)) return undefined
  return Object.freeze({
    maximumPrefixCopyBytes: BigInt(values[0]!),
    maximumCumulativePrefixCopyBytes: BigInt(values[1]!),
    maximumModeledPeakTemporaryBytes: BigInt(values[2]!),
  })
}

function workspaceRecommendationThreshold(
  input: SupportMatrix['policyConstants'],
): bigint | undefined {
  const value = input.zipWorkspaceRecommendationMaximumPeakBytes
  return nonNegativeSafeInteger(value) ? BigInt(value) : undefined
}

function validFeatureFacts(value: unknown): value is DirectZipRequiredFeatureFactsV1 {
  return isObject(value) && exactKeys(value, [
    'createWritable', 'handleIsSameEntry', 'handleQueryPermission', 'handleRequestPermission',
    'indexedDB', 'isSecureContext', 'locks', 'showDirectoryPicker',
  ]) && value.createWritable === 'function' && value.handleIsSameEntry === 'function' &&
    value.handleQueryPermission === 'function' && value.handleRequestPermission === 'function' &&
    value.indexedDB === 'object' && value.isSecureContext === true && value.locks === 'object' &&
    value.showDirectoryPicker === 'function'
}

function validFilesystem(value: Record<string, unknown>): boolean {
  return exactKeys(value, [
    'allocationUnitBytes', 'blockSize', 'driveTypeCode', 'fileSystem', 'operatorAttestation',
  ]) && positiveSafeInteger(value.allocationUnitBytes) &&
    typeof value.blockSize === 'string' && /^\d+$/u.test(value.blockSize) &&
    Number.isSafeInteger(value.driveTypeCode) &&
    typeof value.fileSystem === 'string' && typeof value.operatorAttestation === 'string'
}

function validPlatform(value: Record<string, unknown>): boolean {
  return exactKeys(value, ['arch', 'platform', 'release', 'type', 'version']) &&
    ['arch', 'platform', 'release', 'type', 'version'].every(key => typeof value[key] === 'string')
}

function canonicalJson(value: unknown): string {
  return `${JSON.stringify(normalizeCanonicalJson(value), null, 2)}\n`
}

function normalizeCanonicalJson(value: unknown): unknown {
  if (value === null || typeof value === 'boolean' || typeof value === 'string') {
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) throw new TypeError('support matrix number is not exact')
    return value
  }
  if (Array.isArray(value)) return value.map(normalizeCanonicalJson)
  if (!isObject(value)) throw new TypeError('support matrix value is not canonical JSON')
  return Object.fromEntries(Object.keys(value).sort().map(key => [key, normalizeCanonicalJson(value[key])]))
}

function exactKeys(value: Record<string, unknown>, expected: readonly string[]): boolean {
  const actual = Object.keys(value).sort()
  const sortedExpected = [...expected].sort()
  return actual.length === sortedExpected.length &&
    actual.every((key, index) => key === sortedExpected[index])
}

function isObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function isSha256(value: unknown): value is string {
  return typeof value === 'string' && SHA256_HEX.test(value)
}

function isPolicyDigest(value: unknown): value is string {
  return typeof value === 'string' && SHA256_BASE64URL.test(value)
}

function nonEmptyStrings(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return keys.every(key => typeof value[key] === 'string' && value[key]!.length > 0)
}

function isCanonicalDateTime(value: unknown): boolean {
  return typeof value === 'string' && /(?:Z|[+-]\d{2}:\d{2})$/u.test(value) &&
    Number.isFinite(Date.parse(value))
}

function positiveSafeInteger(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 1
}

function nonNegativeSafeInteger(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0
}

function hexDigestToBase64Url(value: string): string {
  if (!SHA256_HEX.test(value)) throw new TypeError('digest is not canonical SHA-256 hex')
  const bytes = new Uint8Array(32)
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number.parseInt(value.slice(index * 2, index * 2 + 2), 16)
  }
  return encodeBase64Url(bytes)
}

function hex(value: Uint8Array): string {
  return [...value].map(byte => byte.toString(16).padStart(2, '0')).join('')
}
