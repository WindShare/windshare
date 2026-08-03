import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
  readdirSync,
  realpathSync,
} from 'node:fs'
import { basename, isAbsolute, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { TextDecoder } from 'node:util'

import { consumeNetworkCompletion, NETWORK_COMPLETION_SCHEMA } from '../browsergate/network-completion.mjs'
import { evaluateBrowserGate } from '../browsergate/verdict.mjs'
import {
  MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
  parseStabilityResultArchive,
  sha256ArtifactDigest,
} from '../stability/artifact.mjs'
import {
  STABILITY_EVIDENCE_EPOCH,
  STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION,
  STABILITY_RESULT_SCHEMA_VERSION,
  STABILITY_WORKFLOW_JOBS,
} from '../stability/result.mjs'
import {
  normalizeResolutionManifest,
  parseResolutionManifestJSON,
  requireRepository,
  requireTargetSha,
} from './contract.mjs'
import { prevalidateStabilityArchiveLayout } from './stability-archive-layout.mjs'

export const MAXIMUM_RESOLUTION_MANIFEST_BYTES = 64 * 1024
export const MAXIMUM_BROWSER_VERDICT_BYTES = 256 * 1024
export const RELEASE_EVIDENCE_VERIFICATION_SCHEMA =
  'windshare.release-readiness-evidence-verification/v1'

const VERIFIER_EVENT_SCHEMA = 'windshare.release-readiness-verifier-event/v1'
const BROWSER_SUITES = Object.freeze(['main', 'pion'])
const EXPECTED_NETWORK_IDENTITIES = 45
const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..')

export class ReleaseEvidenceVerificationError extends Error {
  constructor(code, message, options) {
    super(message, options)
    this.name = 'ReleaseEvidenceVerificationError'
    this.code = code
  }
}

export function loadResolutionManifest({ path, repository, targetSha }) {
  const bytes = readStableRegularFile(
    path,
    MAXIMUM_RESOLUTION_MANIFEST_BYTES,
    'release evidence resolution manifest',
  )
  return parseResolutionManifestJSON(
    decodeUtf8(bytes, 'release evidence resolution manifest'),
    { repository, targetSha },
  )
}

export async function verifyDownloadedReleaseEvidence(options, dependencies = {}) {
  const repository = requireRepository(options.repository)
  const targetSha = requireTargetSha(options.targetSha)
  const resolution = normalizeResolutionManifest(options.resolution)
  if (resolution.repository.full_name !== repository) {
    throw verificationError('resolution-repository-mismatch', 'resolution manifest belongs to another repository')
  }
  if (resolution.target_sha !== targetSha) {
    throw verificationError('resolution-target-mismatch', 'resolution manifest belongs to another target SHA')
  }

  const repositoryRoot = requireCanonicalDirectory(options.repositoryRoot, 'release repository root')
  const browserRoot = requireCanonicalDirectory(options.browserRoot, 'downloaded browser artifact root')
  const expectedBrowserRoot = join(repositoryRoot, 'test-results')
  if (browserRoot !== expectedBrowserRoot) {
    throw verificationError(
      'browser-root-mismatch',
      'downloaded browser artifact must be rooted at repository test-results',
    )
  }
  rejectAmbientCredentials(options.environment ?? process.env)

  const trace = typeof dependencies.trace === 'function' ? dependencies.trace : () => {}
  const browserAuthority = resolution.authorities.full_browser
  const browser = await verifyBrowserEvidence({
    browserRoot,
    repositoryRoot,
    targetSha,
    runId: browserAuthority.run_id,
    runAttempt: browserAuthority.run_attempt,
    artifact: browserAuthority.artifacts[0],
  }, {
    evaluateBrowserGateImpl: dependencies.evaluateBrowserGateImpl ?? evaluateBrowserGate,
    consumeNetworkCompletionImpl:
      dependencies.consumeNetworkCompletionImpl ?? consumeNetworkCompletion,
  })
  traceEvent(trace, 'browser-content-accepted', {
    target_sha: targetSha,
    run_id: browserAuthority.run_id,
    run_attempt: browserAuthority.run_attempt,
    artifact_id: browserAuthority.artifacts[0].id,
    local_run_id: browser.local_run_id,
    network_run_id: browser.network_run_id,
  })

  const stabilityAuthority = resolution.authorities.stability
  const archives = {
    linux: options.stabilityLinuxArchive,
    windows: options.stabilityWindowsArchive,
  }
  const canonicalArchivePaths = {}
  for (const operatingSystem of ['linux', 'windows']) {
    canonicalArchivePaths[operatingSystem] = requireCanonicalFilePath(
      archives[operatingSystem],
      `downloaded ${operatingSystem} stability archive`,
    )
  }
  if (canonicalArchivePaths.linux === canonicalArchivePaths.windows) {
    throw verificationError(
      'duplicate-stability-archive',
      'Linux and Windows stability evidence must use distinct archives',
    )
  }

  const stability = {}
  for (const [index, operatingSystem] of ['linux', 'windows'].entries()) {
    const artifact = stabilityAuthority.artifacts[index]
    try {
      stability[operatingSystem] = verifyStabilityArchive({
        archivePath: canonicalArchivePaths[operatingSystem],
        artifact,
        operatingSystem,
        runAttempt: stabilityAuthority.run_attempt,
        runId: stabilityAuthority.run_id,
        targetSha,
      })
    } catch (cause) {
      throw verificationError(
        `stability-${operatingSystem}-rejected`,
        `${operatingSystem} stability evidence is invalid`,
        cause,
      )
    }
    traceEvent(trace, 'stability-content-accepted', {
      target_sha: targetSha,
      run_id: stabilityAuthority.run_id,
      run_attempt: stabilityAuthority.run_attempt,
      artifact_id: artifact.id,
      operating_system: operatingSystem,
      invocation_id: stability[operatingSystem].invocation_id,
    })
  }
  if (stability.linux.invocation_id === stability.windows.invocation_id) {
    throw verificationError(
      'duplicate-stability-invocation',
      'Linux and Windows stability evidence reuse one invocation ID',
    )
  }

  const accepted = Object.freeze({
    schema_version: RELEASE_EVIDENCE_VERIFICATION_SCHEMA,
    target_sha: targetSha,
    repository,
    browser: Object.freeze({
      run_id: browserAuthority.run_id,
      run_attempt: browserAuthority.run_attempt,
      artifact_id: browserAuthority.artifacts[0].id,
      local_run_id: browser.local_run_id,
      network_run_id: browser.network_run_id,
      outcome: 'accepted',
    }),
    stability: Object.freeze({
      run_id: stabilityAuthority.run_id,
      run_attempt: stabilityAuthority.run_attempt,
      linux: Object.freeze({
        artifact_id: stabilityAuthority.artifacts[0].id,
        invocation_id: stability.linux.invocation_id,
        outcome: 'accepted',
      }),
      windows: Object.freeze({
        artifact_id: stabilityAuthority.artifacts[1].id,
        invocation_id: stability.windows.invocation_id,
        outcome: 'accepted',
      }),
    }),
    outcome: 'accepted',
  })
  traceEvent(trace, 'downloaded-evidence-accepted', {
    target_sha: targetSha,
    browser_run_id: browserAuthority.run_id,
    stability_run_id: stabilityAuthority.run_id,
  })
  return accepted
}

export async function verifyBrowserEvidence(options, dependencies = {}) {
  const evaluateBrowserGateImpl = dependencies.evaluateBrowserGateImpl ?? evaluateBrowserGate
  const consumeNetworkCompletionImpl =
    dependencies.consumeNetworkCompletionImpl ?? consumeNetworkCompletion
  const expectedArtifactName =
    `browser-full-${options.targetSha}-${options.runId}-${options.runAttempt}`
  if (options.artifact.role !== 'browser' || options.artifact.name !== expectedArtifactName) {
    throw verificationError(
      'browser-artifact-identity-mismatch',
      'downloaded browser artifact identity is invalid',
    )
  }
  const evidenceRoot = requireCanonicalDirectory(
    join(options.browserRoot, 'browser-evidence'),
    'downloaded browser evidence root',
  )
  const verdict = discoverBrowserVerdict(evidenceRoot)
  const verdictBytes = readStableRegularFile(
    verdict.path,
    MAXIMUM_BROWSER_VERDICT_BYTES,
    'downloaded browser verdict',
  )
  let persisted
  try {
    persisted = JSON.parse(decodeUtf8(verdictBytes, 'downloaded browser verdict'))
  } catch (cause) {
    throw verificationError('invalid-browser-verdict-json', 'downloaded browser verdict is not JSON', cause)
  }
  if (
    persisted === null || typeof persisted !== 'object' || Array.isArray(persisted) ||
    persisted.runId !== verdict.runId
  ) {
    throw verificationError(
      'browser-run-mismatch',
      'browser verdict run ID does not match its containing directory',
    )
  }

  const suiteOutcomes = inertRecordOrEmpty(persisted.suiteOutcomes)
  const suites = {}
  for (const suite of BROWSER_SUITES) {
    const outcome = inertRecordOrEmpty(suiteOutcomes[suite])
    suites[suite] = Object.freeze({
      root: join(verdict.root, suite, '.guard-uploads', 'sealed'),
      jobOutcome: outcome.jobOutcome,
      guardOutcome: outcome.guardOutcome,
      downloadOutcome: outcome.downloadOutcome,
      manifestSha256: outcome.manifestSha256,
      manifestByteLength: outcome.manifestByteLength,
    })
  }

  let recomputed
  try {
    recomputed = await evaluateBrowserGateImpl({
      runId: persisted.runId,
      checkoutSha: options.targetSha,
      suites,
    })
  } catch (cause) {
    throw verificationError(
      'browser-verdict-recomputation-failed',
      'browser verdict could not be recomputed from sealed evidence',
      cause,
    )
  }
  if (
    recomputed?.runId !== persisted.runId || recomputed?.checkoutSha !== options.targetSha ||
    recomputed?.verdict !== 'passed' || !Array.isArray(recomputed?.violations) ||
    recomputed.violations.length !== 0
  ) {
    throw verificationError(
      'browser-verdict-not-passed',
      'recomputed browser evidence does not produce one passed target-bound verdict',
    )
  }
  const recomputedBytes = Buffer.from(`${JSON.stringify(recomputed)}\n`, 'utf8')
  if (!verdictBytes.equals(recomputedBytes)) {
    throw verificationError(
      'browser-verdict-byte-mismatch',
      'persisted browser verdict differs from the evaluator result',
    )
  }

  const completionPath = join(options.browserRoot, 'browser-network-completion.json')
  let acceptedNetwork
  try {
    acceptedNetwork = await consumeNetworkCompletionImpl({
      repositoryRoot: options.repositoryRoot,
      completionPath,
      checkoutSha: options.targetSha,
    })
  } catch (cause) {
    throw verificationError(
      'browser-network-completion-rejected',
      'protected browser network completion is invalid',
      cause,
    )
  }
  const expectedNetworkRunId = `gha-${options.runId}-${options.runAttempt}-browser-network`
  if (
    acceptedNetwork?.schemaVersion !== NETWORK_COMPLETION_SCHEMA ||
    acceptedNetwork?.runId !== expectedNetworkRunId ||
    acceptedNetwork?.checkoutSha !== options.targetSha ||
    acceptedNetwork?.expectedIdentities !== EXPECTED_NETWORK_IDENTITIES ||
    acceptedNetwork?.outcome !== 'accepted'
  ) {
    throw verificationError(
      'browser-network-run-mismatch',
      'protected browser network completion is bound to another run attempt',
    )
  }
  return Object.freeze({
    local_run_id: persisted.runId,
    network_run_id: acceptedNetwork.runId,
  })
}

export function verifyStabilityArchive(options) {
  const expectedName =
    `stability-integration-${options.operatingSystem}-${options.targetSha}-${options.runId}-${options.runAttempt}`
  if (options.artifact.role !== options.operatingSystem || options.artifact.name !== expectedName) {
    throw verificationError(
      'stability-artifact-identity-mismatch',
      `${options.operatingSystem} stability artifact identity is invalid`,
    )
  }
  const archive = readStableRegularFile(
    options.archivePath,
    MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
    `downloaded ${options.operatingSystem} stability archive`,
  )
  if (archive.byteLength !== options.artifact.size_in_bytes) {
    throw verificationError(
      'stability-artifact-size-mismatch',
      `${options.operatingSystem} stability archive size differs from GitHub metadata`,
    )
  }
  if (sha256ArtifactDigest(archive) !== options.artifact.digest) {
    throw verificationError(
      'stability-artifact-digest-mismatch',
      `${options.operatingSystem} stability archive digest differs from GitHub metadata`,
    )
  }

  // Digest identity authenticates the bytes, but not an ambiguous ZIP layout.
  // Reject structural overlap before the semantic parser reaches an inflater
  // that may legally ignore trailing central-directory bytes.
  prevalidateStabilityArchiveLayout(archive)
  const result = parseStabilityResultArchive(archive)
  const expectedJob = STABILITY_WORKFLOW_JOBS[options.operatingSystem]?.workflowJob
  const product = result.product_verdict
  if (
    result.schema_version !== STABILITY_RESULT_SCHEMA_VERSION ||
    result.evidence_epoch !== STABILITY_EVIDENCE_EPOCH ||
    result.workflow_run_id !== options.runId ||
    result.workflow_run_attempt !== options.runAttempt ||
    result.commit_sha !== options.targetSha ||
    result.workflow_job !== expectedJob ||
    result.operating_system !== options.operatingSystem ||
    result.suite !== 'integration' ||
    result.retry_count !== options.runAttempt - 1 ||
    product?.schema_version !== STABILITY_PRODUCT_VERDICT_SCHEMA_VERSION ||
    product?.outcome !== 'passed' || product?.failure_class !== 'none' ||
    product?.termination_kind !== 'exit-code' || product?.exit_code !== 0 ||
    product?.signal !== null
  ) {
    throw verificationError(
      'stability-result-identity-mismatch',
      `${options.operatingSystem} stability result does not prove the selected passed run attempt`,
    )
  }
  return result
}

function discoverBrowserVerdict(evidenceRoot) {
  const entries = readdirSync(evidenceRoot, { withFileTypes: true })
  const candidates = []
  for (const entry of entries) {
    if (entry.isSymbolicLink() || !entry.isDirectory()) {
      throw verificationError(
        'unsupported-browser-evidence-entry',
        'browser evidence root contains an unsupported filesystem entry',
      )
    }
    const runRoot = requireCanonicalDirectory(
      join(evidenceRoot, entry.name),
      `browser evidence run ${entry.name}`,
    )
    for (const child of readdirSync(runRoot, { withFileTypes: true })) {
      if (child.name !== 'verdict.json') continue
      if (child.isSymbolicLink() || !child.isFile()) {
        throw verificationError(
          'unsupported-browser-verdict-entry',
          'browser verdict is not a regular non-symbolic file',
        )
      }
      candidates.push({
        path: requireCanonicalFilePath(join(runRoot, child.name), 'downloaded browser verdict'),
        root: runRoot,
        runId: basename(runRoot),
      })
    }
  }
  if (candidates.length !== 1) {
    throw verificationError(
      candidates.length === 0 ? 'missing-browser-verdict' : 'duplicate-browser-verdict',
      'downloaded browser artifact must contain exactly one direct verdict',
    )
  }
  return candidates[0]
}

function rejectAmbientCredentials(environment) {
  const observed = new Set()
  for (const name of Object.keys(environment)) {
    const folded = name.toUpperCase()
    const oidc = folded === 'ACTIONS_ID_TOKEN_REQUEST_URL' ||
      folded === 'ACTIONS_ID_TOKEN_REQUEST_TOKEN'
    const repositoryToken = folded === 'GITHUB_TOKEN' || folded === 'GH_TOKEN' ||
      folded === 'ACTIONS_RUNTIME_TOKEN'
    if (!oidc && !repositoryToken) continue
    if (observed.has(folded)) {
      throw verificationError(
        oidc ? 'ambient-oidc-authority' : 'ambient-repository-token',
        'token-free verifier credential authority is duplicated',
      )
    }
    observed.add(folded)
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (
      descriptor === undefined || !Object.hasOwn(descriptor, 'value') ||
      typeof descriptor.value !== 'string' || descriptor.value !== ''
    ) {
      throw verificationError(
        oidc ? 'ambient-oidc-authority' : 'ambient-repository-token',
        'credential authority reached token-free evidence verification',
      )
    }
  }
}

function inertRecordOrEmpty(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : Object.freeze({})
}

function requireCanonicalDirectory(pathValue, label) {
  if (typeof pathValue !== 'string' || !isAbsolute(pathValue) || resolve(pathValue) !== pathValue) {
    throw verificationError('noncanonical-evidence-path', `${label} path is not absolute and canonical`)
  }
  let metadata
  try {
    metadata = lstatSync(pathValue)
  } catch (cause) {
    throw verificationError('missing-evidence-path', `${label} is unavailable`, cause)
  }
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || realpathSync(pathValue) !== pathValue) {
    throw verificationError('noncanonical-evidence-path', `${label} is not a canonical real directory`)
  }
  return pathValue
}

function requireCanonicalFilePath(pathValue, label) {
  if (typeof pathValue !== 'string' || !isAbsolute(pathValue) || resolve(pathValue) !== pathValue) {
    throw verificationError('noncanonical-evidence-path', `${label} path is not absolute and canonical`)
  }
  let metadata
  try {
    metadata = lstatSync(pathValue)
  } catch (cause) {
    throw verificationError('missing-evidence-path', `${label} is unavailable`, cause)
  }
  if (!metadata.isFile() || metadata.isSymbolicLink() || realpathSync(pathValue) !== pathValue) {
    throw verificationError('noncanonical-evidence-path', `${label} is not a canonical real file`)
  }
  return pathValue
}

function readStableRegularFile(pathValue, maximumBytes, label) {
  const canonical = requireCanonicalFilePath(pathValue, label)
  const namedBefore = lstatSync(canonical, { bigint: true })
  const descriptor = openSync(canonical, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0))
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    if (
      !openedBefore.isFile() || openedBefore.size < 1n || openedBefore.size > BigInt(maximumBytes) ||
      !sameFileRevision(namedBefore, openedBefore)
    ) {
      throw verificationError('unstable-evidence-file', `${label} is not one bounded file revision`)
    }
    const bytes = readFileSync(descriptor)
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(canonical, { bigint: true })
    if (
      bytes.byteLength !== Number(openedAfter.size) ||
      !sameFileRevision(openedBefore, openedAfter) ||
      !sameFileRevision(openedAfter, namedAfter)
    ) {
      throw verificationError('unstable-evidence-file', `${label} changed while it was read`)
    }
    return bytes
  } finally {
    closeSync(descriptor)
  }
}

function sameFileRevision(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw verificationError('invalid-evidence-utf8', `${label} is not UTF-8`, cause)
  }
}

function verificationError(code, message, cause) {
  return new ReleaseEvidenceVerificationError(code, message, cause === undefined ? undefined : { cause })
}

function traceEvent(trace, milestone, fields) {
  trace(Object.freeze({
    schema_version: VERIFIER_EVENT_SCHEMA,
    operation: 'verify-downloaded-release-evidence',
    milestone,
    ...fields,
  }))
}

function parseArguments(arguments_) {
  const allowed = new Set([
    '--repository',
    '--target-sha',
    '--resolution',
    '--browser-root',
    '--stability-linux-archive',
    '--stability-windows-archive',
  ])
  if (arguments_.length !== allowed.size * 2) {
    throw verificationError('invalid-verifier-options', 'release evidence verifier options are incomplete')
  }
  const options = new Map()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (!allowed.has(name) || typeof value !== 'string' || value === '' || options.has(name)) {
      throw verificationError('invalid-verifier-options', 'release evidence verifier options are invalid')
    }
    options.set(name, value)
  }
  return options
}

async function main(arguments_) {
  const options = parseArguments(arguments_)
  const repository = options.get('--repository')
  const targetSha = options.get('--target-sha')
  const resolution = loadResolutionManifest({
    path: options.get('--resolution'),
    repository,
    targetSha,
  })
  const accepted = await verifyDownloadedReleaseEvidence({
    repository,
    targetSha,
    resolution,
    repositoryRoot: REPOSITORY_ROOT,
    browserRoot: options.get('--browser-root'),
    stabilityLinuxArchive: options.get('--stability-linux-archive'),
    stabilityWindowsArchive: options.get('--stability-windows-archive'),
    environment: process.env,
  }, {
    trace: (event) => process.stdout.write(`${JSON.stringify(event)}\n`),
  })
  process.stdout.write(`${JSON.stringify(accepted)}\n`)
}

const invokedPath = process.argv[1] === undefined ? undefined : pathToFileURL(resolve(process.argv[1])).href
if (invokedPath === import.meta.url) {
  try {
    await main(process.argv.slice(2))
  } catch (cause) {
    process.stderr.write(`${JSON.stringify({
      schema_version: VERIFIER_EVENT_SCHEMA,
      operation: 'verify-downloaded-release-evidence',
      milestone: 'rejected',
      failure_code: typeof cause?.code === 'string' ? cause.code : 'release-evidence-rejected',
      message: cause instanceof Error ? cause.message : 'release evidence verification failed',
    })}\n`)
    process.exitCode = 1
  }
}
