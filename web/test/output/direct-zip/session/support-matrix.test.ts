import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { sha256 } from '../../../../src/crypto/digest'
import {
  lookupReviewedDirectZipSupportV1,
  type DirectZipRuntimePlatformFactsV1,
} from '../../../../src/output/direct-zip/session'
import { directZipEpochPolicyDigestV1 } from '../../../../src/output/direct-zip/writer'

const FEATURES = Object.freeze({
  createWritable: 'function' as const,
  handleIsSameEntry: 'function' as const,
  handleQueryPermission: 'function' as const,
  handleRequestPermission: 'function' as const,
  indexedDB: 'object' as const,
  isSecureContext: true,
  locks: 'object' as const,
  showDirectoryPicker: 'function' as const,
})
const PLATFORM = Object.freeze({
  arch: 'x64', platform: 'win32', release: '10.0.1', type: 'Windows_NT', version: 'build-1',
})
const FILESYSTEM = Object.freeze({
  allocationUnitBytes: 4096,
  blockSize: '4096',
  driveTypeCode: 3,
  fileSystem: 'NTFS',
  operatorAttestation: 'local-fixed-volume',
})
const BROWSER_SHA = '11'.repeat(32)
const EVIDENCE_SHA = '22'.repeat(32)

interface PolicyConstants {
  directZipAutomaticCheckpointMaximumCumulativeCopyBytes: number | null
  directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes: number | null
  directZipAutomaticCheckpointMaximumPrefixCopyBytes: number | null
  zipWorkspaceRecommendationMaximumPeakBytes: number | null
}

describe('reviewed Direct ZIP support lookup', () => {
  it('admits only an exact canonical matrix row and derives every policy digest', async () => {
    const artifact = await matrixArtifact()
    const result = await lookupReviewedDirectZipSupportV1({ artifact, runtime: runtimeFacts() })

    expect(result.kind).toBe('available')
    if (result.kind !== 'available') return
    expect(result.facts.support.browserVersion).toBe('123.0.1')
    expect(Object.values(result.facts.support.policies)).toHaveLength(5)
    expect(Object.values(result.facts.support.policies).every(value => value.length === 43)).toBe(true)
    expect(result.facts.automaticEpochBudget.maximumPrefixCopyBytes).toBe(1024n)
    expect(result.facts.automaticEpochBudget.maximumModeledPeakTemporaryBytes).toBe(4096n)
    expect(result.facts.recommendationPolicy).toEqual({
      version: 1,
      kind: 'available',
      workspacePeakBytesThreshold: 8192n,
      policyDigest: 'QHRTz1DNDw172338CZLZxbWnuFomsV8_UHv7kXzngBA',
    })
  })

  it.each([
    ['detached digest mismatch', async () => ({
      ...(await matrixArtifact()),
      detachedSha256: '00'.repeat(32),
    })],
    ['non-canonical bytes', async () => {
      const matrix = supportMatrix()
      const canonicalBytes = new TextEncoder().encode(JSON.stringify(matrix, null, 2))
      return { canonicalBytes, detachedSha256: hex(await sha256(canonicalBytes)) }
    }],
  ])('fails closed for %s', async (_label, createArtifact) => {
    const result = await lookupReviewedDirectZipSupportV1({
      artifact: await createArtifact(),
      runtime: runtimeFacts(),
    })
    expect(result).toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason: 'support-evidence-missing' },
    })
  })

  it('does not approximate browser, platform, filesystem, or feature facts', async () => {
    const artifact = await matrixArtifact()
    for (const runtime of [
      { ...runtimeFacts(), browserVersion: '123.0.2' },
      { ...runtimeFacts(), operatingSystemBuild: canonicalJson({ ...PLATFORM, release: '10.0.2' }) },
      { ...runtimeFacts(), filesystemProfile: canonicalJson({ ...FILESYSTEM, fileSystem: 'ReFS' }) },
      { ...runtimeFacts(), featureFacts: { ...FEATURES, handleIsSameEntry: 'missing' as const } },
    ]) {
      const result = await lookupReviewedDirectZipSupportV1({ artifact, runtime })
      expect(result).toEqual({
        kind: 'unavailable',
        support: { kind: 'unavailable', reason: 'platform-not-reviewed' },
      })
    }
  })

  it('refuses positive evidence without all reviewed numeric policy constants', async () => {
    const artifact = await matrixArtifact({
      ...supportMatrix().policyConstants,
      directZipAutomaticCheckpointMaximumPrefixCopyBytes: null,
    })
    const result = await lookupReviewedDirectZipSupportV1({ artifact, runtime: runtimeFacts() })
    expect(result).toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason: 'policy-digests-unavailable' },
    })
  })

  it('rejects epoch constants that do not reproduce the reviewed row digest', async () => {
    const matrix = supportMatrix()
    matrix.reviewedPlatforms[0]!.review.directZipEpochPolicyDigest = 'A'.repeat(43)
    const canonicalBytes = new TextEncoder().encode(canonicalJson(matrix))
    const result = await lookupReviewedDirectZipSupportV1({
      artifact: { canonicalBytes, detachedSha256: hex(await sha256(canonicalBytes)) },
      runtime: runtimeFacts(),
    })

    expect(result).toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason: 'support-evidence-missing' },
    })
  })

  it('rejects recommendation constants that do not reproduce the reviewed row digest', async () => {
    const matrix = supportMatrix()
    matrix.reviewedPlatforms[0]!.review.workspaceRecommendationPolicyDigest = 'A'.repeat(43)
    const canonicalBytes = new TextEncoder().encode(canonicalJson(matrix))
    const result = await lookupReviewedDirectZipSupportV1({
      artifact: { canonicalBytes, detachedSha256: hex(await sha256(canonicalBytes)) },
      runtime: runtimeFacts(),
    })

    expect(result).toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason: 'support-evidence-missing' },
    })
  })

  it('binds both reviewed policy decisions only for the exact native support row', async () => {
    const matrixPath = fileURLToPath(new URL(
      '../../../../../testdata/browser-evidence/v1/fsa-resumable-zip-support-matrix.json',
      import.meta.url,
    ))
    const digestPath = fileURLToPath(new URL(
      '../../../../../testdata/browser-evidence/v1/fsa-resumable-zip-support-matrix.sha256',
      import.meta.url,
    ))
    const canonicalBytes = new Uint8Array(readFileSync(matrixPath))
    const detachedSha256 = readFileSync(digestPath, 'utf8').split(/\s/u)[0]!
    const matrix = JSON.parse(new TextDecoder().decode(canonicalBytes))
    const row = matrix.reviewedPlatforms[0]
    expect(matrix.policyConstants).toEqual({
      directZipAutomaticCheckpointMaximumCumulativeCopyBytes: 536_870_912,
      directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes: 268_435_456,
      directZipAutomaticCheckpointMaximumPrefixCopyBytes: 268_435_456,
      zipWorkspaceRecommendationMaximumPeakBytes: 1_073_744_986,
    })
    expect(row.review).toMatchObject({
      directZipEpochPolicyDigest: 'dVc_DFPK_50xrZ7_GK0oQ9noWgHhb-2eZEnl4-0kUOo',
      epochPolicyReviewSha256: 'e3faacd98460b5d4977a9ce65b19d8c4b596dfd1f1fe757cb3ad58297312162a',
      workspaceRecommendationPolicyDigest: 'zHRGRc5-OvZ4Z8U2E1ORwNWnccnf_p35QB8iSXlixqI',
      workspaceRecommendationPolicyReviewSha256: 'dc9d4fe8945dbe445e8bc90c430ca57777163607c43bfd82b09ae7d48a2ae484',
      workspaceRecommendationRawEvidenceSha256: 'f47ea879a70395d4e4dac6b67cffee43b3cb06923ce310ec78fe09170dde7906',
    })
    await expect(directZipEpochPolicyDigestV1({
      maximumPrefixCopyBytes: BigInt(
        matrix.policyConstants.directZipAutomaticCheckpointMaximumPrefixCopyBytes,
      ),
      maximumCumulativePrefixCopyBytes: BigInt(
        matrix.policyConstants.directZipAutomaticCheckpointMaximumCumulativeCopyBytes,
      ),
      maximumModeledPeakTemporaryBytes: BigInt(
        matrix.policyConstants.directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes,
      ),
    })).resolves.toBe('dVc_DFPK_50xrZ7_GK0oQ9noWgHhb-2eZEnl4-0kUOo')
    const result = await lookupReviewedDirectZipSupportV1({
      artifact: { canonicalBytes, detachedSha256 },
      runtime: {
        browserExecutableSha256: row.browser.executableSha256,
        browserVersion: row.browser.version,
        operatingSystemBuild: canonicalJson(row.platform),
        filesystemProfile: canonicalJson(row.filesystem),
        featureFacts: row.featureFacts,
      },
    })

    expect(result.kind).toBe('available')
    if (result.kind !== 'available') return
    expect(result.facts.recommendationPolicy).toEqual({
      version: 1,
      kind: 'available',
      workspacePeakBytesThreshold: 1_073_744_986n,
      policyDigest: 'zHRGRc5-OvZ4Z8U2E1ORwNWnccnf_p35QB8iSXlixqI',
    })
  })
})

function runtimeFacts(): DirectZipRuntimePlatformFactsV1 {
  return Object.freeze({
    browserExecutableSha256: BROWSER_SHA,
    browserVersion: '123.0.1',
    operatingSystemBuild: canonicalJson(PLATFORM),
    filesystemProfile: canonicalJson(FILESYSTEM),
    featureFacts: FEATURES,
  })
}

async function matrixArtifact(policyConstants: PolicyConstants = supportMatrix().policyConstants) {
  const canonicalBytes = new TextEncoder().encode(canonicalJson(supportMatrix(policyConstants)))
  return Object.freeze({ canonicalBytes, detachedSha256: hex(await sha256(canonicalBytes)) })
}

function supportMatrix(policyConstants: PolicyConstants = {
  directZipAutomaticCheckpointMaximumCumulativeCopyBytes: 2048,
  directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes: 4096,
  directZipAutomaticCheckpointMaximumPrefixCopyBytes: 1024,
  zipWorkspaceRecommendationMaximumPeakBytes: 8192,
}) {
  return {
    candidateSchema: 'windshare/browser-fsa-resumable-zip-support-matrix-candidate/v1',
    defaultVerdict: { directLocalRoute: 'unsupported', processRestart: 'unproven' },
    matrixSchema: 'windshare/browser-fsa-resumable-zip-support-matrix/v1',
    notes: ['one', 'two', 'three'],
    policyConstants,
    reviewStatus: 'reviewed-local-evidence',
    reviewedPlatforms: [{
      browser: { channel: 'stable', executableSha256: BROWSER_SHA, version: '123.0.1' },
      entryId: 'win-ntfs-stable',
      featureFacts: FEATURES,
      filesystem: FILESYSTEM,
      platform: PLATFORM,
      rawEvidenceSha256: EVIDENCE_SHA,
      repositoryCommit: '33'.repeat(20),
      review: {
        directZipEpochPolicyDigest: 'IWG1Y_z7zzzxFZJ4mgPOxhmf4R8poIFOXcA-NxmXrfs',
        epochPolicyReviewSha256: '88'.repeat(32),
        independentReviewSha256: '77'.repeat(32),
        rationale: 'reviewed exact run',
        reviewedAt: '2026-08-22T00:00:00Z',
        reviewer: 'reviewer',
        workspaceRecommendationArtifactManifestSha256: '99'.repeat(32),
        workspaceRecommendationCandidateSha256: 'aa'.repeat(32),
        workspaceRecommendationPolicyDigest: 'QHRTz1DNDw172338CZLZxbWnuFomsV8_UHv7kXzngBA',
        workspaceRecommendationPolicyReviewSha256: 'bb'.repeat(32),
        workspaceRecommendationRawEvidenceSha256: 'cc'.repeat(32),
        workspaceRecommendationSourceBindingSha256: 'dd'.repeat(32),
      },
      runConfigSha256: '44'.repeat(32),
      scenarios: {
        authorityBinding: true,
        externalReplacementLocatorOnly: true,
        sameTargetProcessRestartContinuation: 1,
      },
      sourceManifestSha256: '55'.repeat(32),
      supportingArtifactManifestSha256: '66'.repeat(32),
      verdict: { directLocalRoute: 'supported', processRestart: true },
    }],
    schema: 'windshare/browser-fsa-resumable-zip-support-matrix/v1',
  }
}

function canonicalJson(value: unknown): string {
  return `${JSON.stringify(normalizeCanonicalJson(value), null, 2)}\n`
}

function normalizeCanonicalJson(value: unknown): unknown {
  if (value === null || typeof value === 'boolean' || typeof value === 'number' ||
      typeof value === 'string') return value
  if (Array.isArray(value)) return value.map(normalizeCanonicalJson)
  const record = value as Record<string, unknown>
  return Object.fromEntries(Object.keys(record).sort().map(key => [key, normalizeCanonicalJson(record[key])]))
}

function hex(value: Uint8Array): string {
  return [...value].map(byte => byte.toString(16).padStart(2, '0')).join('')
}
