import { createHash } from 'node:crypto'
import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
  parseStabilityResultArchive,
  sha256ArtifactDigest,
} from './artifact.mjs'
import {
  STABILITY_EVIDENCE_EPOCH,
  STABILITY_WORKFLOW_JOBS,
  writeCanonicalJSON,
} from './result.mjs'

export const STABILITY_RELEASE_VERDICT_SCHEMA_VERSION =
  'windshare.stability-release-verdict/v7'

const STABILITY_FINDING_POLICY_SCHEMA_VERSION =
  'windshare.stability-finding-policy/v2'
const STABILITY_FINDING_KEY_SCHEMA_VERSION =
  'windshare.stability-finding-key/v2'
// Reproduction needs two independently published observations. Resolution uses
// the same 20-sample correction authority as the refactor acceptance plan, so
// one green sample can never erase a historical correctness failure.
export const REQUIRED_REPRODUCTION_OBSERVATIONS = 2
export const REQUIRED_RESOLUTION_PASS_SAMPLES = 20
const FINDING_POLICY = Object.freeze({
  schema_version: STABILITY_FINDING_POLICY_SCHEMA_VERSION,
  finding_key_schema_version: STABILITY_FINDING_KEY_SCHEMA_VERSION,
  reproducibility_observation_count: REQUIRED_REPRODUCTION_OBSERVATIONS,
  resolution_pass_count: REQUIRED_RESOLUTION_PASS_SAMPLES,
  reproducibility_authority: 'distinct-valid-run-artifact-observations',
  resolution_authority: 'newer-valid-passes-on-strict-descendant-commits',
  unresolved_reproduced_disposition: 'release-blocking',
  resolved_disposition: 'trend-only',
  insufficient_history_disposition: 'non-blocking-resolution-not-evaluated',
  release_blocking_authority: 'historical-findings-and-sha-bound-current-evidence',
})

const GITHUB_API_VERSION = '2026-03-10'
const OPERATING_SYSTEMS = Object.freeze(Object.keys(STABILITY_WORKFLOW_JOBS))
const MAXIMUM_REQUIRED_SAMPLES = 100
const PAGE_SIZE = 100
const MAXIMUM_LIST_PAGES = 100
const HISTORY_QUERY_CONCURRENCY = 8
const INTEGRATION_STEP_NAME = 'run native integration exactly once'
const UPLOAD_STEP_NAME = 'publish authenticated stability evidence'
const INFRASTRUCTURE_JOB_CONCLUSIONS = new Set([
  'action_required',
  'cancelled',
  'failure',
  'stale',
  'startup_failure',
  'timed_out',
])
const WORKFLOW_STEP_CONCLUSIONS = new Set([
  ...INFRASTRUCTURE_JOB_CONCLUSIONS,
  'neutral',
  'skipped',
  'success',
])
const WORKFLOW_RUN_CONCLUSIONS = new Set([
  'action_required',
  'cancelled',
  'failure',
  'neutral',
  'skipped',
  'stale',
  'startup_failure',
  'success',
  'timed_out',
])

class EvidenceInvalidError extends Error {
  constructor(code, message, options) {
    super(message, options)
    this.name = 'EvidenceInvalidError'
    this.code = code
  }
}

class GitHubRequestError extends Error {
  constructor(path, status) {
    super(`GitHub API request failed for ${path} with status ${status}`)
    this.name = 'GitHubRequestError'
    this.path = path
    this.status = status
  }
}

export async function reduceStabilityHistory({
  repository,
  workflow,
  targetSha,
  requiredRuns,
  token,
  fetchImpl = globalThis.fetch,
}) {
  const repositoryPath = requireRepository(repository)
  requireWorkflow(workflow)
  const canonicalTargetSha = requireTargetSHA(targetSha)
  requireRequiredSampleCount(requiredRuns)
  if (typeof token !== 'string' || token.trim() === '' || token.trim() !== token) {
    throw new Error('GitHub API token is missing or non-canonical')
  }
  if (typeof fetchImpl !== 'function') {
    throw new Error('GitHub API fetch implementation is missing')
  }

  const repositoryMetadata = await getJSON(fetchImpl, token, `/repos/${repositoryPath}`)
  const defaultBranch = requireDefaultBranch(repositoryMetadata.default_branch)
  const context = {
    repositoryPath,
    token,
    fetchImpl,
    compareCache: new Map(),
  }
  const samples = new Map(OPERATING_SYSTEMS.map((operatingSystem) => [operatingSystem, []]))
  const invalidSamples = new Map(OPERATING_SYSTEMS.map((operatingSystem) => [operatingSystem, []]))
  const seenRunIDs = new Set()
  const seenInvocationIDs = new Set()
  let observedRunCount = 0
  let totalRunCount
  let historyExhausted = false
  let previousWorkflowRunOrder

  for (let page = 1; page <= MAXIMUM_LIST_PAGES; page += 1) {
    const response = await getJSON(
      fetchImpl,
      token,
      `/repos/${repositoryPath}/actions/workflows/${encodeURIComponent(workflow)}/runs?event=schedule&status=completed&per_page=${PAGE_SIZE}&page=${page}`,
    )
    if (
      !Number.isSafeInteger(response.total_count) ||
      response.total_count < 0 ||
      !Array.isArray(response.workflow_runs)
    ) {
      throw new Error('GitHub workflow-runs response is malformed')
    }
    if (totalRunCount === undefined) totalRunCount = response.total_count
    else if (response.total_count !== totalRunCount) {
      throw new Error('GitHub workflow-runs pagination changed while reducing stability history')
    }
    if (response.workflow_runs.length > PAGE_SIZE) {
      throw new Error('GitHub workflow-runs page exceeds its requested size')
    }
    for (const value of response.workflow_runs) {
      const order = workflowRunOrder(value)
      if (
        order !== undefined && previousWorkflowRunOrder !== undefined &&
        !workflowRunPrecedes(previousWorkflowRunOrder, order)
      ) throw new Error('GitHub workflow-runs response is not in stable newest-first order')
      if (order !== undefined) previousWorkflowRunOrder = order
    }
    if (response.workflow_runs.length === 0) {
      if (observedRunCount !== totalRunCount) {
        throw new Error('GitHub workflow-runs pagination ended before its declared history')
      }
      historyExhausted = true
      break
    }

    const neededOperatingSystems = new Set(OPERATING_SYSTEMS.filter(
      (operatingSystem) => samples.get(operatingSystem).length < requiredRuns,
    ))
    const evaluations = await mapWithConcurrency(
      response.workflow_runs,
      HISTORY_QUERY_CONCURRENCY,
      async (value, pageIndex) => {
        let run
        try {
          run = validateWorkflowRun(value, observedRunCount + pageIndex, defaultBranch)
        } catch (cause) {
          if (!(cause instanceof EvidenceInvalidError)) throw cause
          return invalidRunEvaluations(value, neededOperatingSystems, cause)
        }
        if (seenRunIDs.has(run.workflow_run_id)) {
          throw new Error('stability history contains duplicate workflow runs')
        }
        seenRunIDs.add(run.workflow_run_id)
        return evaluateWorkflowRun(context, run, neededOperatingSystems)
      },
    )

    for (const evaluation of evaluations) {
      observedRunCount += 1
      for (const operatingSystem of OPERATING_SYSTEMS) {
        const selected = samples.get(operatingSystem)
        if (selected.length >= requiredRuns) continue
        const candidate = evaluation.get(operatingSystem)
        if (candidate === undefined) continue
        if (candidate.kind === 'valid') {
          if (seenInvocationIDs.has(candidate.sample.invocation_id)) {
            invalidSamples.get(operatingSystem).push(invalidObservation(
              candidate.sample,
              operatingSystem,
              new EvidenceInvalidError(
                'duplicate-invocation',
                `stability invocation ${candidate.sample.invocation_id} appears more than once`,
              ),
            ))
          } else {
            seenInvocationIDs.add(candidate.sample.invocation_id)
            selected.push(candidate.sample)
          }
        } else {
          invalidSamples.get(operatingSystem).push(candidate.observation)
        }
      }
    }
    if (observedRunCount > totalRunCount) {
      throw new Error('GitHub workflow-runs response exceeds its total count')
    }
    if (OPERATING_SYSTEMS.every(
      (operatingSystem) => samples.get(operatingSystem).length >= requiredRuns,
    )) {
      historyExhausted = observedRunCount === totalRunCount
      break
    }
    if (observedRunCount === totalRunCount) {
      historyExhausted = true
      break
    }
  }

  if (
    !historyExhausted &&
    !OPERATING_SYSTEMS.every(
      (operatingSystem) => samples.get(operatingSystem).length >= requiredRuns,
    )
  ) {
    throw new Error('GitHub workflow-runs pagination exceeded its safety limit')
  }

  const canonicalSamples = Object.freeze(Object.fromEntries(OPERATING_SYSTEMS.map(
    (operatingSystem) => [
      operatingSystem,
      Object.freeze(samples.get(operatingSystem).slice(0, requiredRuns)),
    ],
  )))
  const canonicalInvalidSamples = Object.freeze(Object.fromEntries(OPERATING_SYSTEMS.map(
    (operatingSystem) => [
      operatingSystem,
      Object.freeze(invalidSamples.get(operatingSystem)),
    ],
  )))
  const sampleCounts = Object.freeze(Object.fromEntries(OPERATING_SYSTEMS.map(
    (operatingSystem) => [operatingSystem, canonicalSamples[operatingSystem].length],
  )))
  const insufficient = Object.freeze(OPERATING_SYSTEMS
    .filter((operatingSystem) => sampleCounts[operatingSystem] < requiredRuns)
    .map((operatingSystem) => Object.freeze({
      operating_system: operatingSystem,
      valid_sample_count: sampleCounts[operatingSystem],
      required_sample_count: requiredRuns,
    })))
  const findings = await reduceHistoricalFindings(
    context,
    canonicalSamples,
    insufficient.length === 0,
  )
  const blockingFindings = findings.filter((finding) =>
    finding.reproducibility_state === 'reproduced' && finding.resolution_state === 'unresolved')
  const outcome = insufficient.length > 0
    ? 'insufficient-history'
    : blockingFindings.length > 0 ? 'failed' : 'passed'
  const failure = blockingFindings.length === 0 || insufficient.length > 0
    ? null
    : Object.freeze({
        code: 'unresolved-reproducible-correctness-findings',
        message: `${blockingFindings.length} unresolved reproducible correctness finding(s) block release`,
        operating_systems: Object.freeze([...new Set(blockingFindings.map(
          ({ operating_system: operatingSystem }) => operatingSystem,
        ))].sort()),
        finding_ids: Object.freeze(blockingFindings.map(({ finding_id: findingId }) => findingId)),
      })

  return Object.freeze({
    schema_version: STABILITY_RELEASE_VERDICT_SCHEMA_VERSION,
    repository,
    workflow,
    target_sha: canonicalTargetSha,
    required_sample_count: requiredRuns,
    observed_run_count: observedRunCount,
    history_exhausted: historyExhausted,
    outcome,
    failure,
    insufficient_history: insufficient,
    finding_policy: FINDING_POLICY,
    sample_counts: sampleCounts,
    samples: canonicalSamples,
    invalid_samples: canonicalInvalidSamples,
    findings,
  })
}

async function reduceHistoricalFindings(context, samplesByOperatingSystem, evaluateResolution) {
  const findings = []
  for (const operatingSystem of OPERATING_SYSTEMS) {
    const samples = samplesByOperatingSystem[operatingSystem]
    requireDistinctSampleEvidence(samples, `${operatingSystem} stability history`)
    const groups = new Map()
    for (const [sampleIndex, sample] of samples.entries()) {
      if (sample.product_verdict.outcome !== 'failed') continue
      const key = findingKey(sample)
      const serializedKey = JSON.stringify(key)
      let group = groups.get(serializedKey)
      if (group === undefined) {
        group = { key, latestFailureIndex: sampleIndex, latestFailure: sample, observations: [] }
        groups.set(serializedKey, group)
      }
      group.observations.push(findingEvidence(sample))
    }

    for (const group of groups.values()) {
      const reproduced = group.observations.length >= REQUIRED_REPRODUCTION_OBSERVATIONS
      const resolutionPasses = evaluateResolution
        ? await verifiedResolutionPasses(context, samples, group)
        : Object.freeze([])
      const resolutionState = evaluateResolution
        ? resolutionPasses.length >= REQUIRED_RESOLUTION_PASS_SAMPLES ? 'resolved' : 'unresolved'
        : 'not-evaluated-insufficient-history'
      const releaseDisposition = !evaluateResolution
        ? 'non-blocking-insufficient-history'
        : resolutionState === 'resolved'
          ? 'trend-only-resolved'
          : reproduced ? 'release-blocking' : 'tracked-single-observation'
      findings.push(Object.freeze({
        finding_id: createHash('sha256').update(JSON.stringify(group.key)).digest('hex'),
        finding_key: group.key,
        operating_system: operatingSystem,
        suite: group.key.suite,
        failure_class: group.key.failure_class,
        termination_kind: group.key.termination_kind,
        exit_code: group.key.exit_code,
        signal: group.key.signal,
        observation_count: group.observations.length,
        observations: Object.freeze(group.observations),
        reproducibility_state: reproduced ? 'reproduced' : 'not-reproduced',
        resolution_state: resolutionState,
        resolution_pass_count: resolutionPasses.length,
        resolution_passes: resolutionPasses,
        release_disposition: releaseDisposition,
      }))
    }
  }
  return Object.freeze(findings)
}

function findingKey(sample) {
  const verdict = sample.product_verdict
  // Volatile paths, timestamps, commit IDs, and diagnostics stay in observations.
  // The epoch makes comparability explicit while the product termination tuple
  // remains stable across independent runs.
  return Object.freeze({
    schema_version: STABILITY_FINDING_KEY_SCHEMA_VERSION,
    evidence_epoch: sample.evidence_epoch,
    operating_system: sample.operating_system,
    suite: sample.suite,
    failure_class: verdict.failure_class,
    termination_kind: verdict.termination_kind,
    exit_code: verdict.exit_code,
    signal: verdict.signal,
  })
}

function findingEvidence(sample) {
  return Object.freeze({
    workflow_run_id: sample.workflow_run_id,
    workflow_run_created_at: sample.workflow_run_created_at,
    commit_sha: sample.commit_sha,
    artifact_id: sample.artifact_id,
    artifact_digest: sample.artifact_digest,
  })
}

function requireDistinctSampleEvidence(samples, label) {
  const runIdentities = new Set()
  const artifactIdentities = new Set()
  for (const sample of samples) {
    const artifactIdentity = `${sample.artifact_id}\0${sample.artifact_digest}`
    if (
      runIdentities.has(sample.workflow_run_id) || artifactIdentities.has(artifactIdentity)
    ) throw new Error(`${label} repeats one run or artifact identity`)
    runIdentities.add(sample.workflow_run_id)
    artifactIdentities.add(artifactIdentity)
  }
}

async function verifiedResolutionPasses(context, samples, group) {
  const failureTime = Date.parse(group.latestFailure.workflow_run_created_at)
  // Starting at the latest matching failure makes another observation reset the
  // resolution sequence without conflating unrelated termination signatures.
  const candidates = samples
    .slice(0, group.latestFailureIndex)
    .filter(({ product_verdict: verdict }) => verdict.outcome === 'passed')
  const verified = []
  for (const sample of candidates) {
    if (Date.parse(sample.workflow_run_created_at) <= failureTime) {
      throw new Error('stability resolution evidence does not follow its finding observation')
    }
    if (!await isStrictDescendantCommit(
      context,
      group.latestFailure.commit_sha,
      sample.commit_sha,
    )) continue
    verified.push(findingEvidence(sample))
    if (verified.length === REQUIRED_RESOLUTION_PASS_SAMPLES) break
  }
  return Object.freeze(verified)
}

async function isStrictDescendantCommit(context, baseCommit, headCommit) {
  if (baseCommit === headCommit) return false
  const key = `${baseCommit}\0${headCommit}`
  let operation = context.compareCache.get(key)
  if (operation === undefined) {
    operation = loadStrictDescendantCommit(context, baseCommit, headCommit)
      .catch((cause) => {
        if (context.compareCache.get(key) === operation) context.compareCache.delete(key)
        throw cause
      })
    context.compareCache.set(key, operation)
  }
  return operation
}

async function loadStrictDescendantCommit(context, baseCommit, headCommit) {
  let response
  try {
    response = await getJSON(
      context.fetchImpl,
      context.token,
      `/repos/${context.repositoryPath}/compare/${baseCommit}...${headCommit}?per_page=1&page=1`,
    )
  } catch (cause) {
    // A missing or non-comparable commit cannot prove correction. It is evidence
    // of an unresolved finding, while transport/API failures remain reducer-fatal.
    if (cause instanceof GitHubRequestError && [404, 409].includes(cause.status)) return false
    throw cause
  }
  if (
    response === null || typeof response !== 'object' || Array.isArray(response) ||
    !['ahead', 'behind', 'diverged', 'identical'].includes(response.status) ||
    !Number.isSafeInteger(response.ahead_by) || response.ahead_by < 0 ||
    !Number.isSafeInteger(response.behind_by) || response.behind_by < 0 ||
    response.base_commit === null || typeof response.base_commit !== 'object' ||
    response.base_commit.sha !== baseCommit ||
    response.merge_base_commit === null || typeof response.merge_base_commit !== 'object' ||
    typeof response.merge_base_commit.sha !== 'string' ||
    !/^[a-f0-9]{40}$/u.test(response.merge_base_commit.sha)
  ) throw new Error('GitHub compare response cannot prove stability finding ancestry')
  return response.status === 'ahead' && response.ahead_by > 0 && response.behind_by === 0 &&
    response.merge_base_commit.sha === baseCommit
}

async function evaluateWorkflowRun(context, run, neededOperatingSystems) {
  let jobs
  let artifacts
  try {
    [jobs, artifacts] = await Promise.all([
      listRunJobs(context, run),
      listRunArtifacts(context, run),
    ])
  } catch (cause) {
    if (!(cause instanceof EvidenceInvalidError)) throw cause
    return invalidRunEvaluations(run, neededOperatingSystems, cause)
  }

  const evaluations = await Promise.all([...neededOperatingSystems].map(async (operatingSystem) => {
    try {
      const sample = await evaluateOperatingSystemSample(
        context,
        run,
        operatingSystem,
        jobs,
        artifacts,
      )
      return [operatingSystem, Object.freeze({ kind: 'valid', sample })]
    } catch (cause) {
      if (cause instanceof GitHubRequestError && ![404, 410].includes(cause.status)) throw cause
      const invalid = cause instanceof EvidenceInvalidError
        ? cause
        : new EvidenceInvalidError(
            'invalid-evidence',
            cause instanceof Error ? cause.message : String(cause),
            { cause },
          )
      return [operatingSystem, Object.freeze({
        kind: 'invalid',
        observation: invalidObservation(run, operatingSystem, invalid),
      })]
    }
  }))
  return new Map(evaluations)
}

async function evaluateOperatingSystemSample(context, run, operatingSystem, jobs, artifacts) {
  const authority = STABILITY_WORKFLOW_JOBS[operatingSystem]
  const job = selectJobCandidate(jobs, run, operatingSystem, authority)
  const artifact = selectArtifactCandidate(artifacts, run, operatingSystem)

  const archive = await getBytes(
    context.fetchImpl,
    context.token,
    `/repos/${context.repositoryPath}/actions/artifacts/${artifact.id}/zip`,
  )
  if (archive.byteLength !== artifact.sizeInBytes) {
    throw new EvidenceInvalidError(
      'artifact-size-mismatch',
      `stability artifact ${artifact.name} size disagrees with its API descriptor`,
    )
  }
  if (sha256ArtifactDigest(archive) !== artifact.digest) {
    throw new EvidenceInvalidError(
      'artifact-digest-mismatch',
      `stability artifact ${artifact.name} digest does not match its download`,
    )
  }

  let result
  try {
    result = parseStabilityResultArchive(archive)
  } catch (cause) {
    throw new EvidenceInvalidError(
      'invalid-structured-evidence',
      `stability artifact ${artifact.name} has invalid structured evidence: ${canonicalFailureMessage(cause)}`,
      { cause },
    )
  }
  validateArtifactResult(result, run, operatingSystem, authority)
  validateJobSettlement(job, result, run)

  return Object.freeze({
    workflow_run_id: run.workflow_run_id,
    workflow_run_attempt: run.workflow_run_attempt,
    workflow_run_created_at: run.workflow_run_created_at,
    workflow_run_conclusion: run.workflow_run_conclusion,
    commit_sha: run.commit_sha,
    invocation_id: result.invocation_id,
    job_id: String(job.id),
    workflow_job: authority.workflowJob,
    job_name: authority.jobName,
    runner_label: authority.runnerLabel,
    artifact_id: String(artifact.id),
    artifact_name: artifact.name,
    artifact_digest: artifact.digest,
    evidence_epoch: result.evidence_epoch,
    operating_system: operatingSystem,
    suite: 'integration',
    product_verdict: result.product_verdict,
  })
}

function selectJobCandidate(jobs, run, operatingSystem, authority) {
  const candidates = jobs.filter((job) =>
    job !== null &&
    typeof job === 'object' &&
    !Array.isArray(job) &&
    job.name === authority.jobName)
  if (candidates.length === 0) {
    throw new EvidenceInvalidError(
      'missing-job',
      `stability run ${run.workflow_run_id} has no ${operatingSystem} integration job`,
    )
  }
  if (candidates.length > 1) {
    throw new EvidenceInvalidError(
      'duplicate-job-candidate',
      `stability run ${run.workflow_run_id} has duplicate ${operatingSystem} integration job candidates`,
    )
  }
  const [job] = candidates
  if (
    !Number.isSafeInteger(job.id) ||
    job.id < 1 ||
    String(job.run_id) !== run.workflow_run_id ||
    job.run_attempt !== run.workflow_run_attempt ||
    job.head_sha !== run.commit_sha ||
    job.status !== 'completed' ||
    !Array.isArray(job.labels) ||
    job.labels.length !== 1 ||
    job.labels[0] !== authority.runnerLabel ||
    !Array.isArray(job.steps)
  ) {
    throw new EvidenceInvalidError(
      'invalid-job-identity',
      `stability ${operatingSystem} job in run ${run.workflow_run_id} has invalid CI identity`,
    )
  }
  return job
}

function selectArtifactCandidate(artifacts, run, operatingSystem) {
  const expectedName =
    `stability-integration-${operatingSystem}-${run.commit_sha}-${run.workflow_run_id}-${run.workflow_run_attempt}`
  const candidates = artifacts.filter((artifact) =>
    artifact !== null &&
    typeof artifact === 'object' &&
    !Array.isArray(artifact) &&
    artifact.name === expectedName)
  if (candidates.length === 0) {
    throw new EvidenceInvalidError(
      'missing-artifact',
      `stability run ${run.workflow_run_id} has no ${operatingSystem} result artifact`,
    )
  }
  if (candidates.length > 1) {
    throw new EvidenceInvalidError(
      'duplicate-artifact-candidate',
      `stability run ${run.workflow_run_id} has duplicate ${operatingSystem} result artifact candidates`,
    )
  }
  const [artifact] = candidates
  if (
    !Number.isSafeInteger(artifact.id) ||
    artifact.id < 1 ||
    artifact.expired !== false ||
    !Number.isSafeInteger(artifact.size_in_bytes) ||
    artifact.size_in_bytes < 1 ||
    artifact.size_in_bytes > MAXIMUM_ARTIFACT_ARCHIVE_BYTES ||
    typeof artifact.digest !== 'string' ||
    !/^sha256:[a-f0-9]{64}$/u.test(artifact.digest) ||
    artifact.workflow_run === null ||
    typeof artifact.workflow_run !== 'object' ||
    String(artifact.workflow_run.id) !== run.workflow_run_id ||
    artifact.workflow_run.head_sha !== run.commit_sha
  ) {
    throw new EvidenceInvalidError(
      'invalid-artifact-identity',
      `stability artifact ${expectedName} has invalid API identity or digest metadata`,
    )
  }
  return Object.freeze({
    id: artifact.id,
    name: expectedName,
    sizeInBytes: artifact.size_in_bytes,
    digest: artifact.digest,
  })
}

function validateArtifactResult(result, run, operatingSystem, authority) {
  const matches =
    result.evidence_epoch === STABILITY_EVIDENCE_EPOCH &&
    result.workflow_run_id === run.workflow_run_id &&
    result.workflow_run_attempt === run.workflow_run_attempt &&
    result.commit_sha === run.commit_sha &&
    result.workflow_job === authority.workflowJob &&
    result.operating_system === operatingSystem &&
    result.suite === 'integration' &&
    result.retry_count === run.workflow_run_attempt - 1 &&
    result.retry_count === 0
  if (!matches) {
    throw new EvidenceInvalidError(
      'result-identity-mismatch',
      `stability ${operatingSystem} result disagrees with workflow run ${run.workflow_run_id}`,
    )
  }
}

function validateJobSettlement(job, result, run) {
  const integrationSteps = job.steps.filter((step) =>
    step !== null &&
    typeof step === 'object' &&
    step.name === INTEGRATION_STEP_NAME)
  const uploadSteps = job.steps.filter((step) =>
    step !== null &&
    typeof step === 'object' &&
    step.name === UPLOAD_STEP_NAME)
  if (integrationSteps.length !== 1 || uploadSteps.length !== 1) {
    throw new EvidenceInvalidError(
      'invalid-job-steps',
      `stability job ${job.id} lacks unique integration and upload steps`,
    )
  }
  const [integration] = integrationSteps
  const [upload] = uploadSteps
  const integrationIndex = job.steps.indexOf(integration)
  const uploadIndex = job.steps.indexOf(upload)
  if (
    uploadIndex <= integrationIndex ||
    upload.status !== 'completed' ||
    upload.conclusion !== 'success'
  ) {
    throw new EvidenceInvalidError(
      'upload-failure',
      `stability job ${job.id} did not complete its post-integration evidence upload`,
    )
  }

  for (const [index, step] of job.steps.entries()) {
    if (step === integration || step === upload) continue
    const malformedOrFailed =
      step === null ||
      typeof step !== 'object' ||
      step.status !== 'completed' ||
      !WORKFLOW_STEP_CONCLUSIONS.has(step.conclusion) ||
      INFRASTRUCTURE_JOB_CONCLUSIONS.has(step.conclusion)
    const invalidPrerequisite =
      index < integrationIndex &&
      (malformedOrFailed || step.conclusion !== 'success')
    if (invalidPrerequisite || malformedOrFailed) {
      throw new EvidenceInvalidError(
        'infrastructure-step-failure',
        `stability job ${job.id} contains a failed infrastructure step`,
      )
    }
  }

  const expectedConclusion =
    result.product_verdict.outcome === 'passed' ? 'success' : 'failure'
  if (
    integration.status !== 'completed' ||
    integration.conclusion !== expectedConclusion ||
    job.conclusion !== expectedConclusion
  ) {
    throw new EvidenceInvalidError(
      'job-verdict-mismatch',
      `stability job ${job.id} settlement disagrees with its structured product verdict`,
    )
  }
  if (
    result.product_verdict.outcome === 'failed' &&
    run.workflow_run_conclusion !== 'failure'
  ) {
    throw new EvidenceInvalidError(
      'run-verdict-mismatch',
      `stability run ${run.workflow_run_id} does not reflect its structured product failure`,
    )
  }
}

function workflowRunOrder(value) {
  if (
    value === null || typeof value !== 'object' || Array.isArray(value) ||
    !Number.isSafeInteger(value.id) || value.id < 1
  ) return undefined
  const createdAtMillis = canonicalWorkflowRunTimestamp(value.created_at)
  return createdAtMillis === undefined
    ? undefined
    : Object.freeze({ createdAtMillis, runId: value.id })
}

function workflowRunPrecedes(previous, current) {
  return previous.createdAtMillis > current.createdAtMillis ||
    previous.createdAtMillis === current.createdAtMillis && previous.runId >= current.runId
}

function canonicalWorkflowRunTimestamp(value) {
  if (
    typeof value !== 'string' ||
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/u.test(value)
  ) return undefined
  const timestamp = Date.parse(value)
  if (!Number.isSafeInteger(timestamp)) return undefined
  const encoded = new Date(timestamp).toISOString()
  const canonical = value.includes('.') ? encoded : encoded.replace('.000Z', 'Z')
  return canonical === value ? timestamp : undefined
}

function validateWorkflowRun(value, index, defaultBranch) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new EvidenceInvalidError('invalid-run', `stability workflow run ${index} is malformed`)
  }
  if (!Number.isSafeInteger(value.id) || value.id < 1) {
    throw new EvidenceInvalidError('invalid-run', `stability workflow run ${index} has an invalid ID`)
  }
  if (
    value.event !== 'schedule' ||
    value.status !== 'completed' ||
    value.head_branch !== defaultBranch ||
    value.run_attempt !== 1 ||
    !WORKFLOW_RUN_CONCLUSIONS.has(value.conclusion) ||
    typeof value.head_sha !== 'string' ||
    !/^[a-f0-9]{40}$/u.test(value.head_sha) ||
    canonicalWorkflowRunTimestamp(value.created_at) === undefined
  ) {
    throw new EvidenceInvalidError(
      'invalid-run-identity',
      `stability workflow run ${value.id} has invalid schedule, branch, attempt, or commit identity`,
    )
  }
  // A failed aggregate run does not invalidate a passing OS peer. Its
  // conclusion is retained so a structured product failure can still require
  // coherent failed job and workflow settlement.
  return Object.freeze({
    workflow_run_id: String(value.id),
    workflow_run_attempt: value.run_attempt,
    workflow_run_created_at: value.created_at,
    workflow_run_conclusion: value.conclusion,
    commit_sha: value.head_sha,
  })
}

function invalidRunEvaluations(value, operatingSystems, cause) {
  const run = {
    workflow_run_id: Number.isSafeInteger(value?.id) && value.id > 0 ? String(value.id) : 'unknown',
    commit_sha: typeof value?.head_sha === 'string' ? value.head_sha : 'unknown',
  }
  return new Map([...operatingSystems].map((operatingSystem) => [
    operatingSystem,
    Object.freeze({
      kind: 'invalid',
      observation: invalidObservation(run, operatingSystem, cause),
    }),
  ]))
}

function invalidObservation(run, operatingSystem, cause) {
  return Object.freeze({
    workflow_run_id: run.workflow_run_id,
    commit_sha: run.commit_sha,
    operating_system: operatingSystem,
    failure_class: 'infrastructure-invalid',
    reason_code: cause.code,
    message: canonicalFailureMessage(cause),
  })
}

async function listRunJobs(context, run) {
  return listRunCollection({
    context,
    run,
    kind: 'jobs',
    path: (page) =>
      `/repos/${context.repositoryPath}/actions/runs/${run.workflow_run_id}/jobs?filter=all&per_page=${PAGE_SIZE}&page=${page}`,
    field: 'jobs',
  })
}

async function listRunArtifacts(context, run) {
  return listRunCollection({
    context,
    run,
    kind: 'artifacts',
    path: (page) =>
      `/repos/${context.repositoryPath}/actions/runs/${run.workflow_run_id}/artifacts?per_page=${PAGE_SIZE}&page=${page}`,
    field: 'artifacts',
  })
}

async function listRunCollection({ context, run, kind, path, field }) {
  const values = []
  let totalCount
  for (let page = 1; page <= MAXIMUM_LIST_PAGES; page += 1) {
    const response = await getJSON(context.fetchImpl, context.token, path(page))
    if (
      !Number.isSafeInteger(response.total_count) ||
      response.total_count < 0 ||
      !Array.isArray(response[field])
    ) {
      throw new EvidenceInvalidError(
        `invalid-${kind}-response`,
        `stability ${kind} for run ${run.workflow_run_id} are malformed`,
      )
    }
    if (totalCount === undefined) totalCount = response.total_count
    else if (response.total_count !== totalCount) {
      throw new EvidenceInvalidError(
        `${kind}-pagination-changed`,
        `stability ${kind} pagination changed for run ${run.workflow_run_id}`,
      )
    }
    values.push(...response[field])
    if (values.length > totalCount) {
      throw new EvidenceInvalidError(
        `${kind}-pagination-overlap`,
        `stability ${kind} pages overlap for run ${run.workflow_run_id}`,
      )
    }
    if (values.length === totalCount) return values
    if (response[field].length === 0) {
      throw new EvidenceInvalidError(
        `${kind}-pagination-ended`,
        `stability ${kind} pagination ended early for run ${run.workflow_run_id}`,
      )
    }
  }
  throw new EvidenceInvalidError(
    `${kind}-pagination-limit`,
    `stability ${kind} pagination exceeded its safety limit for run ${run.workflow_run_id}`,
  )
}

async function getJSON(fetchImpl, token, path) {
  const response = await request(fetchImpl, token, path)
  try {
    return await response.json()
  } catch (cause) {
    throw new Error(`GitHub API returned invalid JSON for ${path}`, { cause })
  }
}

async function getBytes(fetchImpl, token, path) {
  const response = await request(fetchImpl, token, path)
  const advertisedLength = response.headers?.get?.('content-length')
  if (advertisedLength !== undefined && advertisedLength !== null) {
    if (
      !/^(?:0|[1-9][0-9]*)$/u.test(advertisedLength) ||
      Number(advertisedLength) > MAXIMUM_ARTIFACT_ARCHIVE_BYTES
    ) {
      await cancelResponseBody(response)
      throw new EvidenceInvalidError(
        'invalid-artifact-size',
        `GitHub artifact download for ${path} advertises an invalid size`,
      )
    }
  }

  const reader = response.body?.getReader?.()
  if (reader === undefined) {
    throw new EvidenceInvalidError(
      'invalid-artifact-stream',
      `GitHub artifact download for ${path} is not a bounded byte stream`,
    )
  }
  const chunks = []
  let byteCount = 0
  let completed = false
  let cancelled = false
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) {
        completed = true
        break
      }
      if (!(value instanceof Uint8Array)) {
        throw new EvidenceInvalidError(
          'invalid-artifact-stream',
          `GitHub artifact download for ${path} yielded a non-byte chunk`,
        )
      }
      byteCount += value.byteLength
      if (byteCount > MAXIMUM_ARTIFACT_ARCHIVE_BYTES) {
        cancelled = true
        try {
          await reader.cancel('WindShare stability artifact size limit exceeded')
        } catch {
          // Size overflow already makes the evidence unusable.
        }
        throw new EvidenceInvalidError(
          'artifact-size-limit',
          `GitHub artifact download for ${path} exceeds its size limit`,
        )
      }
      if (value.byteLength > 0) chunks.push(Buffer.from(value))
    }
  } catch (cause) {
    if (!completed && !cancelled) {
      try {
        await reader.cancel('WindShare stability artifact read failed')
      } catch {
        // The original read failure remains the actionable diagnosis.
      }
    }
    if (cause instanceof EvidenceInvalidError) throw cause
    throw new EvidenceInvalidError(
      'invalid-artifact-stream',
      `GitHub API returned invalid artifact bytes for ${path}`,
      { cause },
    )
  } finally {
    try {
      reader.releaseLock()
    } catch {
      // Cancelled streams may already have released their reader.
    }
  }
  if (byteCount === 0) {
    throw new EvidenceInvalidError(
      'empty-artifact',
      `GitHub artifact download for ${path} is empty`,
    )
  }
  return Buffer.concat(chunks, byteCount)
}

async function cancelResponseBody(response) {
  try {
    await response.body?.cancel?.('WindShare stability artifact size header is invalid')
  } catch {
    // Cleanup cannot change the already fail-closed size verdict.
  }
}

async function request(fetchImpl, token, path) {
  const response = await fetchImpl(`https://api.github.com${path}`, {
    method: 'GET',
    headers: {
      Accept: 'application/vnd.github+json',
      Authorization: `Bearer ${token}`,
      'X-GitHub-Api-Version': GITHUB_API_VERSION,
      'User-Agent': 'windshare-stability-release-reducer',
    },
  })
  if (response === null || typeof response !== 'object' || response.ok !== true) {
    const status = Number.isInteger(response?.status) ? response.status : 'unavailable'
    throw new GitHubRequestError(path, status)
  }
  return response
}

async function mapWithConcurrency(values, concurrency, mapper) {
  const results = new Array(values.length)
  let nextIndex = 0
  async function worker() {
    while (true) {
      const index = nextIndex
      nextIndex += 1
      if (index >= values.length) return
      results[index] = await mapper(values[index], index)
    }
  }
  await Promise.all(Array.from({ length: Math.min(concurrency, values.length) }, worker))
  return results
}

function requireRepository(value) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(value)) {
    throw new Error('GitHub repository must be owner/name')
  }
  const segments = value.split('/')
  if (segments.some((segment) => segment === '.' || segment === '..')) {
    throw new Error('GitHub repository contains a relative path segment')
  }
  return segments.map(encodeURIComponent).join('/')
}

function requireWorkflow(value) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_.-]+\.ya?ml$/u.test(value)) {
    throw new Error('stability workflow must be a workflow filename')
  }
}

function requireTargetSHA(value) {
  if (typeof value !== 'string' || !/^[a-f0-9]{40}$/u.test(value)) {
    throw new Error('release target SHA must be a canonical lowercase SHA-1 object ID')
  }
  return value
}

function requireRequiredSampleCount(value) {
  if (
    !Number.isSafeInteger(value) ||
    value < 1 ||
    value > MAXIMUM_REQUIRED_SAMPLES
  ) {
    throw new Error(`required sample count must be between 1 and ${MAXIMUM_REQUIRED_SAMPLES}`)
  }
  return value
}

function requireDefaultBranch(value) {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    value.length > 255 ||
    !/^[A-Za-z0-9_./-]+$/u.test(value) ||
    value.startsWith('/') ||
    value.endsWith('/') ||
    value.includes('//') ||
    value.split('/').some((segment) => segment === '.' || segment === '..')
  ) {
    throw new Error('GitHub repository default branch is invalid')
  }
  return value
}

function parseOptions(arguments_) {
  const options = new Map()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (typeof name !== 'string' || !name.startsWith('--') || value === undefined) {
      throw new Error('release reducer options must be --name value pairs')
    }
    if (options.has(name)) throw new Error(`duplicate release reducer option ${name}`)
    options.set(name, value)
  }
  return options
}

function requiredOption(options, name) {
  const value = options.get(name)
  if (value === undefined || value === '') throw new Error(`missing required option ${name}`)
  return value
}

async function main() {
  const options = parseOptions(process.argv.slice(2))
  const allowed = new Set([
    '--repository',
    '--workflow',
    '--target-sha',
    '--required-runs',
    '--output',
  ])
  for (const name of options.keys()) if (!allowed.has(name)) throw new Error(`unsupported option ${name}`)
  const requiredRuns = requireRequiredSampleCount(
    Number(requiredOption(options, '--required-runs')),
  )
  const repository = requiredOption(options, '--repository')
  const workflow = requiredOption(options, '--workflow')
  const targetSha = requireTargetSHA(requiredOption(options, '--target-sha'))
  const output = requiredOption(options, '--output')

  // Terminal v7 verdicts are emitted only after every schema-bearing invocation
  // field is canonical, so reducer failures cannot publish malformed evidence.
  requireRepository(repository)
  requireWorkflow(workflow)

  let verdict
  try {
    verdict = await reduceStabilityHistory({
      repository,
      workflow,
      targetSha,
      requiredRuns,
      token: process.env.GITHUB_TOKEN,
    })
  } catch (cause) {
    verdict = terminalReducerFailure(repository, workflow, targetSha, requiredRuns, cause)
    try {
      writeCanonicalJSON(output, verdict)
    } catch (writeFailure) {
      throw new AggregateError(
        [cause, writeFailure],
        'stability history failed and its terminal verdict could not be published',
      )
    }
    throw cause
  }

  writeCanonicalJSON(output, verdict)
  const terminal = releaseReducerProcessResult(verdict)
  process[terminal.stream].write(`${terminal.message}\n`)
  process.exitCode = terminal.exitCode
}

export function releaseReducerProcessResult(verdict) {
  if (verdict.outcome === 'failed') {
    return Object.freeze({
      exitCode: 1,
      stream: 'stderr',
      message: `stability-release-reducer: ${verdict.failure.message}`,
    })
  }
  if (verdict.outcome === 'insufficient-history') {
    const counts = verdict.insufficient_history.map((entry) =>
      `${entry.operating_system} ${entry.valid_sample_count}/${entry.required_sample_count}`).join(', ')
    return Object.freeze({
      exitCode: 0,
      stream: 'stdout',
      message: `stability-release-reducer: INSUFFICIENT HISTORY (${counts}; finding resolution not evaluated; SHA-bound current evidence is still required)`,
    })
  }
  if (verdict.outcome !== 'passed') throw new Error('stability release verdict outcome is unsupported')
  const resolvedFindings = verdict.findings.filter(({ resolution_state: state }) => state === 'resolved').length
  const trackedFindings = verdict.findings.length - resolvedFindings
  return Object.freeze({
    exitCode: 0,
    stream: 'stdout',
    message: `stability-release-reducer: PASS (${verdict.required_sample_count} valid samples per OS; ` +
      `${resolvedFindings} resolved trend finding(s); ${trackedFindings} tracked nonblocking finding(s); ` +
      'SHA-bound current evidence remains required)',
  })
}

function terminalReducerFailure(repository, workflow, targetSha, requiredRuns, cause) {
  const emptyByOS = Object.freeze(Object.fromEntries(OPERATING_SYSTEMS.map(
    (operatingSystem) => [operatingSystem, Object.freeze([])],
  )))
  const zeroByOS = Object.freeze(Object.fromEntries(OPERATING_SYSTEMS.map(
    (operatingSystem) => [operatingSystem, 0],
  )))
  return Object.freeze({
    schema_version: STABILITY_RELEASE_VERDICT_SCHEMA_VERSION,
    repository,
    workflow,
    target_sha: targetSha,
    required_sample_count: requiredRuns,
    observed_run_count: 0,
    history_exhausted: false,
    outcome: 'failed',
    failure: Object.freeze({
      code: 'stability-reducer-error',
      message: canonicalFailureMessage(cause),
      operating_systems: Object.freeze([]),
      finding_ids: Object.freeze([]),
    }),
    insufficient_history: Object.freeze([]),
    finding_policy: FINDING_POLICY,
    sample_counts: zeroByOS,
    samples: emptyByOS,
    invalid_samples: emptyByOS,
    findings: Object.freeze([]),
  })
}

function canonicalFailureMessage(cause) {
  const message = cause instanceof Error ? cause.message : String(cause)
  return message.replace(/[\r\n\u0000-\u001f\u007f]+/gu, ' ').trim().slice(0, 1_024) ||
    'stability history failed without a diagnostic message'
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    await main()
  } catch (cause) {
    process.stderr.write(
      `stability-release-reducer: ${cause instanceof Error ? cause.message : String(cause)}\n`,
    )
    process.exitCode = 1
  }
}
