import { STABILITY_WORKFLOW_JOBS } from '../stability/result.mjs'

export const RELEASE_EVIDENCE_SCHEMA_VERSION =
  'windshare.release-readiness-resolution/v1'

export const AUTHORITY_SPECS = deepFreeze({
  ci: {
    workflowFile: 'ci.yml',
    workflowName: 'CI',
    workflowPath: '.github/workflows/ci.yml',
    allowedEvents: ['push', 'workflow_dispatch'],
    terminalJobNames: ['CI Required Verdict'],
    artifactRoles: [],
  },
  full_browser: {
    workflowFile: 'browser-full.yml',
    workflowName: 'Full Browser',
    workflowPath: '.github/workflows/browser-full.yml',
    allowedEvents: ['schedule', 'workflow_dispatch'],
    terminalJobNames: ['Run the token-free full browser orchestrator'],
    artifactRoles: ['browser'],
  },
  stability: {
    workflowFile: 'stability.yml',
    workflowName: 'Native Integration Stability',
    workflowPath: '.github/workflows/stability.yml',
    allowedEvents: ['schedule', 'workflow_dispatch'],
    terminalJobNames: [
      STABILITY_WORKFLOW_JOBS.linux.jobName,
      STABILITY_WORKFLOW_JOBS.windows.jobName,
    ],
    artifactRoles: ['linux', 'windows'],
  },
})

export const WORKFLOW_OUTPUT_KEYS = Object.freeze([
  'ci_run_id',
  'ci_run_attempt',
  'browser_run_id',
  'browser_run_attempt',
  'browser_artifact_id',
  'stability_run_id',
  'stability_run_attempt',
  'stability_linux_artifact_id',
  'stability_windows_artifact_id',
])

const SHA_PATTERN = /^[a-f0-9]{40}$/u
const SHA256_DIGEST_PATTERN = /^sha256:[a-f0-9]{64}$/u
const DECIMAL_ID_PATTERN = /^[1-9][0-9]*$/u
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u
const DEFAULT_BRANCH_PATTERN = /^[A-Za-z0-9_./-]+$/u
const CANONICAL_TIMESTAMP_PATTERN =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/u

export class ReleaseEvidenceContractError extends Error {
  constructor(message, options) {
    super(message, options)
    this.name = 'ReleaseEvidenceContractError'
    this.code = 'invalid-resolution-contract'
  }
}

export function requireRepository(value) {
  if (typeof value !== 'string' || !REPOSITORY_PATTERN.test(value)) {
    throw new ReleaseEvidenceContractError('GitHub repository must be canonical owner/name')
  }
  if (value.split('/').some((segment) => segment === '.' || segment === '..')) {
    throw new ReleaseEvidenceContractError('GitHub repository contains a relative path segment')
  }
  return value
}

export function encodedRepositoryPath(value) {
  return requireRepository(value).split('/').map(encodeURIComponent).join('/')
}

export function requireDefaultBranch(value) {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    value.length > 255 ||
    !DEFAULT_BRANCH_PATTERN.test(value) ||
    value.startsWith('/') ||
    value.endsWith('/') ||
    value.includes('//') ||
    value.split('/').some((segment) => segment === '.' || segment === '..')
  ) {
    throw new ReleaseEvidenceContractError('GitHub repository default branch is invalid')
  }
  return value
}

export function requireTargetSha(value) {
  if (typeof value !== 'string' || !SHA_PATTERN.test(value)) {
    throw new ReleaseEvidenceContractError(
      'release target SHA must be a canonical lowercase SHA-1 object ID',
    )
  }
  return value
}

export function requireToken(value) {
  if (typeof value !== 'string' || value.length === 0 || value.trim() !== value) {
    throw new ReleaseEvidenceContractError('GitHub API token is missing or non-canonical')
  }
  if (value.includes('\r') || value.includes('\n')) {
    throw new ReleaseEvidenceContractError('GitHub API token contains a line break')
  }
  return value
}

export function requirePositiveSafeInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) {
    throw new ReleaseEvidenceContractError(`${label} must be a positive safe integer`)
  }
  return value
}

export function decimalID(value, label) {
  return String(requirePositiveSafeInteger(value, label))
}

export function requireDecimalID(value, label) {
  if (typeof value !== 'string' || !DECIMAL_ID_PATTERN.test(value)) {
    throw new ReleaseEvidenceContractError(`${label} must be a canonical decimal ID`)
  }
  const numeric = Number(value)
  if (!Number.isSafeInteger(numeric) || numeric < 1 || String(numeric) !== value) {
    throw new ReleaseEvidenceContractError(`${label} must encode a positive safe integer`)
  }
  return value
}

export function requireCanonicalTimestamp(value, label) {
  if (typeof value !== 'string' || !CANONICAL_TIMESTAMP_PATTERN.test(value)) {
    throw new ReleaseEvidenceContractError(`${label} must be a canonical UTC timestamp`)
  }
  const timestamp = Date.parse(value)
  if (!Number.isSafeInteger(timestamp)) {
    throw new ReleaseEvidenceContractError(`${label} must be a valid UTC timestamp`)
  }
  const encoded = new Date(timestamp).toISOString()
  const canonical = value.includes('.') ? encoded : encoded.replace('.000Z', 'Z')
  if (canonical !== value) {
    throw new ReleaseEvidenceContractError(`${label} must be a canonical UTC timestamp`)
  }
  return timestamp
}

export function requireSha256Digest(value, label) {
  if (typeof value !== 'string' || !SHA256_DIGEST_PATTERN.test(value)) {
    throw new ReleaseEvidenceContractError(
      `${label} must be a lowercase sha256 digest`,
    )
  }
  return value
}

export function expectedArtifactDescriptors(authorityKey, targetSha, runId, runAttempt) {
  const spec = AUTHORITY_SPECS[authorityKey]
  if (spec === undefined) {
    throw new ReleaseEvidenceContractError(`unknown release authority ${authorityKey}`)
  }
  const sha = requireTargetSha(targetSha)
  const canonicalRunID = requireDecimalID(String(runId), `${authorityKey} run ID`)
  const attempt = requirePositiveSafeInteger(runAttempt, `${authorityKey} run attempt`)
  const suffix = `${sha}-${canonicalRunID}-${attempt}`

  if (authorityKey === 'ci') return Object.freeze([])
  if (authorityKey === 'full_browser') {
    return deepFreeze([{ role: 'browser', name: `browser-full-${suffix}` }])
  }
  return deepFreeze([
    { role: 'linux', name: `stability-integration-linux-${suffix}` },
    { role: 'windows', name: `stability-integration-windows-${suffix}` },
  ])
}

export function releaseWorkflowOutputs(manifest) {
  const normalized = normalizeResolutionManifest(manifest)
  return Object.freeze({
    ci_run_id: normalized.authorities.ci.run_id,
    ci_run_attempt: String(normalized.authorities.ci.run_attempt),
    browser_run_id: normalized.authorities.full_browser.run_id,
    browser_run_attempt: String(normalized.authorities.full_browser.run_attempt),
    browser_artifact_id: normalized.authorities.full_browser.artifacts[0].id,
    stability_run_id: normalized.authorities.stability.run_id,
    stability_run_attempt: String(normalized.authorities.stability.run_attempt),
    stability_linux_artifact_id: normalized.authorities.stability.artifacts[0].id,
    stability_windows_artifact_id: normalized.authorities.stability.artifacts[1].id,
  })
}

export function serializeResolutionManifest(manifest) {
  return `${JSON.stringify(normalizeResolutionManifest(manifest))}\n`
}

export function parseResolutionManifestJSON(
  source,
  { repository, targetSha } = {},
) {
  if (typeof source !== 'string') {
    throw new ReleaseEvidenceContractError('resolution manifest source must be UTF-8 text')
  }
  let parsed
  try {
    parsed = JSON.parse(source)
  } catch (cause) {
    throw new ReleaseEvidenceContractError('resolution manifest is not valid JSON', { cause })
  }
  const normalized = normalizeResolutionManifest(parsed)
  if (`${JSON.stringify(normalized)}\n` !== source) {
    throw new ReleaseEvidenceContractError('resolution manifest is not canonical JSON')
  }
  if (repository !== undefined && normalized.repository.full_name !== requireRepository(repository)) {
    throw new ReleaseEvidenceContractError('resolution manifest repository does not match')
  }
  if (targetSha !== undefined && normalized.target_sha !== requireTargetSha(targetSha)) {
    throw new ReleaseEvidenceContractError('resolution manifest target SHA does not match')
  }
  return normalized
}

export function normalizeResolutionManifest(value) {
  requireRecord(value, 'resolution manifest')
  requireExactKeys(
    value,
    ['schema_version', 'target_sha', 'repository', 'authorities'],
    'resolution manifest',
  )
  if (value.schema_version !== RELEASE_EVIDENCE_SCHEMA_VERSION) {
    throw new ReleaseEvidenceContractError('resolution manifest schema is unsupported')
  }
  const targetSha = requireTargetSha(value.target_sha)
  const repository = normalizeRepository(value.repository)
  requireRecord(value.authorities, 'resolution authorities')
  requireExactKeys(value.authorities, Object.keys(AUTHORITY_SPECS), 'resolution authorities')

  const authorities = {
    ci: normalizeAuthority('ci', value.authorities.ci, targetSha),
    full_browser: normalizeAuthority(
      'full_browser',
      value.authorities.full_browser,
      targetSha,
    ),
    stability: normalizeAuthority('stability', value.authorities.stability, targetSha),
  }
  requireDistinct(
    Object.values(authorities).map((authority) => authority.workflow_id),
    'resolution workflow IDs',
  )
  requireDistinct(
    Object.values(authorities).map((authority) => authority.run_id),
    'resolution run IDs',
  )
  requireDistinct(
    Object.values(authorities).flatMap((authority) => authority.terminal_job_ids),
    'resolution terminal job IDs',
  )
  requireDistinct(
    Object.values(authorities).flatMap((authority) =>
      authority.artifacts.map((artifact) => artifact.id)),
    'resolution artifact IDs',
  )

  return deepFreeze({
    schema_version: RELEASE_EVIDENCE_SCHEMA_VERSION,
    target_sha: targetSha,
    repository,
    authorities,
  })
}

function normalizeRepository(value) {
  requireRecord(value, 'resolution repository')
  requireExactKeys(value, ['id', 'full_name', 'default_branch'], 'resolution repository')
  return {
    id: requireDecimalID(value.id, 'resolution repository ID'),
    full_name: requireRepository(value.full_name),
    default_branch: requireDefaultBranch(value.default_branch),
  }
}

function normalizeAuthority(authorityKey, value, targetSha) {
  const spec = AUTHORITY_SPECS[authorityKey]
  requireRecord(value, `${authorityKey} authority`)
  requireExactKeys(
    value,
    [
      'workflow_id',
      'run_id',
      'run_attempt',
      'event',
      'terminal_job_ids',
      'artifacts',
    ],
    `${authorityKey} authority`,
  )
  const runId = requireDecimalID(value.run_id, `${authorityKey} run ID`)
  const runAttempt = requirePositiveSafeInteger(
    value.run_attempt,
    `${authorityKey} run attempt`,
  )
  if (!spec.allowedEvents.includes(value.event)) {
    throw new ReleaseEvidenceContractError(`${authorityKey} authority event is invalid`)
  }
  if (
    !Array.isArray(value.terminal_job_ids) ||
    value.terminal_job_ids.length !== spec.terminalJobNames.length
  ) {
    throw new ReleaseEvidenceContractError(
      `${authorityKey} authority terminal job IDs are incomplete`,
    )
  }
  const terminalJobIDs = value.terminal_job_ids.map((id, index) =>
    requireDecimalID(id, `${authorityKey} terminal job ID ${index}`))
  requireDistinct(terminalJobIDs, `${authorityKey} terminal job IDs`)

  if (!Array.isArray(value.artifacts)) {
    throw new ReleaseEvidenceContractError(`${authorityKey} authority artifacts are invalid`)
  }
  const expectedArtifacts = expectedArtifactDescriptors(
    authorityKey,
    targetSha,
    runId,
    runAttempt,
  )
  if (value.artifacts.length !== expectedArtifacts.length) {
    throw new ReleaseEvidenceContractError(
      `${authorityKey} authority artifact count is invalid`,
    )
  }
  const artifacts = value.artifacts.map((artifact, index) =>
    normalizeArtifact(authorityKey, artifact, expectedArtifacts[index], index))
  requireDistinct(
    artifacts.map((artifact) => artifact.id),
    `${authorityKey} artifact IDs`,
  )

  return {
    workflow_id: requireDecimalID(value.workflow_id, `${authorityKey} workflow ID`),
    run_id: runId,
    run_attempt: runAttempt,
    event: value.event,
    terminal_job_ids: terminalJobIDs,
    artifacts,
  }
}

function normalizeArtifact(authorityKey, value, expected, index) {
  requireRecord(value, `${authorityKey} artifact ${index}`)
  requireExactKeys(
    value,
    ['role', 'id', 'name', 'size_in_bytes', 'digest'],
    `${authorityKey} artifact ${index}`,
  )
  if (value.role !== expected.role || value.name !== expected.name) {
    throw new ReleaseEvidenceContractError(
      `${authorityKey} artifact ${index} has an invalid role or name`,
    )
  }
  return {
    role: expected.role,
    id: requireDecimalID(value.id, `${authorityKey} artifact ${index} ID`),
    name: expected.name,
    size_in_bytes: requirePositiveSafeInteger(
      value.size_in_bytes,
      `${authorityKey} artifact ${index} size`,
    ),
    digest: requireSha256Digest(
      value.digest,
      `${authorityKey} artifact ${index} digest`,
    ),
  }
}

function requireRecord(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new ReleaseEvidenceContractError(`${label} must be an object`)
  }
  return value
}

function requireExactKeys(value, expected, label) {
  const actual = Object.keys(value)
  if (
    actual.length !== expected.length ||
    expected.some((key) => !Object.hasOwn(value, key))
  ) {
    throw new ReleaseEvidenceContractError(`${label} has unexpected fields`)
  }
}

function requireDistinct(values, label) {
  if (new Set(values).size !== values.length) {
    throw new ReleaseEvidenceContractError(`${label} must be distinct`)
  }
}

export function deepFreeze(value) {
  if (value !== null && typeof value === 'object' && !Object.isFrozen(value)) {
    for (const child of Object.values(value)) deepFreeze(child)
    Object.freeze(value)
  }
  return value
}
