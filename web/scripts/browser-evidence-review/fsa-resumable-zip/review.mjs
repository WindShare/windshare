import { createHash } from 'node:crypto'
import { readFileSync, realpathSync } from 'node:fs'
import { relative, resolve, sep } from 'node:path'
import { pathToFileURL } from 'node:url'

const ARTIFACT_ROOT = 'testdata/browser-evidence/v1'
const MATRIX_FILE = 'fsa-resumable-zip-support-matrix.json'
const MATRIX_SCHEMA_FILE = 'fsa-resumable-zip-support-matrix.schema.json'
const CANDIDATE_SCHEMA_FILE = 'fsa-resumable-zip-support-candidate.schema.json'
const DETACHED_DIGEST_FILE = 'fsa-resumable-zip-support-matrix.sha256'
const SHA256_HEX = /^[0-9a-f]{64}$/u
const FROZEN_SCHEMA_SHA256 = Object.freeze({
  [MATRIX_SCHEMA_FILE]: 'b6332cf96fad1e9668e1eefa4eb7095ed373b39a61a39a5ed758b56d9ded58e0',
  [CANDIDATE_SCHEMA_FILE]: 'a1212be1d35e6e681d0c32f76405d8f97d25c77f554f706f7c0af13c10e52e9e',
})

export function reviewFrozenSupportMatrix(repositoryRoot) {
  const root = realpathSync.native(resolve(repositoryRoot))
  const matrixBytes = readArtifact(root, MATRIX_FILE)
  const actualDigest = createHash('sha256').update(matrixBytes).digest('hex')
  const detachedDigest = readArtifact(root, DETACHED_DIGEST_FILE).toString('utf8')
  if (detachedDigest !== `${actualDigest}  ${MATRIX_FILE}\n`) {
    throw new Error('Reviewed support matrix detached digest does not match its exact bytes')
  }

  const matrix = parseJson(matrixBytes, MATRIX_FILE)
  if (matrixBytes.toString('utf8') !== canonicalJson(matrix)) {
    throw new Error('Reviewed support matrix is not canonical JSON')
  }
  const matrixSchema = readFrozenSchema(root, MATRIX_SCHEMA_FILE)
  const candidateSchema = readFrozenSchema(root, CANDIDATE_SCHEMA_FILE)
  if (matrix.schema !== matrix.matrixSchema || matrix.schema !== matrixSchema.$id ||
      matrix.candidateSchema !== candidateSchema.$id) {
    throw new Error('Reviewed support matrix and schema identities disagree')
  }
  if (matrix.reviewStatus !== 'reviewed-local-evidence' ||
      !Array.isArray(matrix.reviewedPlatforms) || matrix.reviewedPlatforms.length === 0) {
    throw new Error('Reviewed support matrix contains no positive reviewed row')
  }

  const policyConstants = matrix.policyConstants
  const policyNames = [
    'directZipAutomaticCheckpointMaximumCumulativeCopyBytes',
    'directZipAutomaticCheckpointMaximumModeledPeakTemporaryBytes',
    'directZipAutomaticCheckpointMaximumPrefixCopyBytes',
    'zipWorkspaceRecommendationMaximumPeakBytes',
  ]
  if (!isRecord(policyConstants) || !exactKeys(policyConstants, policyNames) ||
      !policyNames.every(name => Number.isSafeInteger(policyConstants[name]) && policyConstants[name] >= 0)) {
    throw new Error('Reviewed support matrix policy constants are incomplete or inexact')
  }
  for (const row of matrix.reviewedPlatforms) validateReviewedRow(row)

  return Object.freeze({
    matrixSha256: actualDigest,
    reviewedEntryIds: Object.freeze(matrix.reviewedPlatforms.map(row => row.entryId)),
    policyConstants: Object.freeze({ ...policyConstants }),
  })
}

function readFrozenSchema(repositoryRoot, name) {
  const bytes = readArtifact(repositoryRoot, name)
  if (createHash('sha256').update(bytes).digest('hex') !== FROZEN_SCHEMA_SHA256[name]) {
    throw new Error(`Reviewed support frozen schema digest changed: ${name}`)
  }
  return parseJson(bytes, name)
}

function validateReviewedRow(row) {
  const digests = [
    row?.browser?.executableSha256,
    row?.rawEvidenceSha256,
    row?.runConfigSha256,
    row?.sourceManifestSha256,
    row?.supportingArtifactManifestSha256,
    row?.review?.epochPolicyReviewSha256,
    row?.review?.independentReviewSha256,
    row?.review?.workspaceRecommendationArtifactManifestSha256,
    row?.review?.workspaceRecommendationCandidateSha256,
    row?.review?.workspaceRecommendationPolicyReviewSha256,
    row?.review?.workspaceRecommendationRawEvidenceSha256,
    row?.review?.workspaceRecommendationSourceBindingSha256,
  ]
  if (typeof row?.entryId !== 'string' || !digests.every(value => SHA256_HEX.test(value ?? '')) ||
      row?.verdict?.directLocalRoute !== 'supported' || row?.verdict?.processRestart !== true) {
    throw new Error('Reviewed support matrix row is incomplete')
  }
}

function readArtifact(repositoryRoot, name) {
  const path = resolve(repositoryRoot, ...ARTIFACT_ROOT.split('/'), name)
  const fromRoot = relative(repositoryRoot, path)
  if (fromRoot === '..' || fromRoot.startsWith(`..${sep}`)) {
    throw new Error(`Reviewed support artifact escaped the repository: ${name}`)
  }
  return readFileSync(path)
}

function parseJson(bytes, name) {
  try {
    return JSON.parse(bytes.toString('utf8'))
  } catch (error) {
    throw new Error(`Reviewed support artifact is not JSON: ${name}`, { cause: error })
  }
}

function canonicalJson(value) {
  return `${JSON.stringify(sortValue(value), null, 2)}\n`
}

function sortValue(value) {
  if (Array.isArray(value)) return value.map(sortValue)
  if (!isRecord(value)) {
    if (typeof value === 'number' && !Number.isSafeInteger(value)) {
      throw new Error('Reviewed support matrix contains an inexact number')
    }
    return value
  }
  return Object.fromEntries(Object.keys(value).sort().map(key => [key, sortValue(value[key])]))
}

function exactKeys(value, expected) {
  const actual = Object.keys(value).sort()
  const sortedExpected = [...expected].sort()
  return actual.length === sortedExpected.length && actual.every((key, index) => key === sortedExpected[index])
}

function isRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  const repositoryRoot = resolve(import.meta.dirname, '..', '..', '..', '..')
  process.stdout.write(`${JSON.stringify(reviewFrozenSupportMatrix(repositoryRoot))}\n`)
}
