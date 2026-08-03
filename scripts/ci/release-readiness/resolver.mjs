import { appendFile, mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  AUTHORITY_SPECS,
  RELEASE_EVIDENCE_SCHEMA_VERSION,
  ReleaseEvidenceContractError,
  WORKFLOW_OUTPUT_KEYS,
  decimalID,
  deepFreeze,
  expectedArtifactDescriptors,
  normalizeResolutionManifest,
  releaseWorkflowOutputs,
  requireCanonicalTimestamp,
  requireDefaultBranch,
  requirePositiveSafeInteger,
  requireRepository,
  requireSha256Digest,
  requireTargetSha,
  requireToken,
  serializeResolutionManifest,
} from './contract.mjs'
import {
  GitHubActionsAPIError,
  createGitHubActionsClient,
} from './github-actions.mjs'

const EXPECTED_OPTION_TO_OUTPUT_KEY = Object.freeze({
  '--expect-ci-run-id': 'ci_run_id',
  '--expect-ci-run-attempt': 'ci_run_attempt',
  '--expect-browser-run-id': 'browser_run_id',
  '--expect-browser-run-attempt': 'browser_run_attempt',
  '--expect-browser-artifact-id': 'browser_artifact_id',
  '--expect-stability-run-id': 'stability_run_id',
  '--expect-stability-run-attempt': 'stability_run_attempt',
  '--expect-stability-linux-artifact-id': 'stability_linux_artifact_id',
  '--expect-stability-windows-artifact-id': 'stability_windows_artifact_id',
})

export class ReleaseEvidenceResolutionError extends Error {
  constructor(code, message, options) {
    super(message, options)
    this.name = 'ReleaseEvidenceResolutionError'
    this.code = code
  }
}

export async function resolveReleaseEvidence({
  repository,
  defaultBranch,
  targetSha,
  token,
  fetchImpl = globalThis.fetch,
  traceImpl = emitStructuredTrace,
}) {
  const canonicalRepository = requireRepository(repository)
  const canonicalDefaultBranch = requireDefaultBranch(defaultBranch)
  const canonicalTargetSha = requireTargetSha(targetSha)
  const canonicalToken = requireToken(token)
  if (typeof fetchImpl !== 'function') {
    throw new ReleaseEvidenceResolutionError(
      'invalid-fetch-implementation',
      'GitHub API fetch implementation is missing',
    )
  }
  if (typeof traceImpl !== 'function') {
    throw new ReleaseEvidenceResolutionError(
      'invalid-trace-implementation',
      'release resolver trace implementation is missing',
    )
  }

  const client = createGitHubActionsClient({
    repository: canonicalRepository,
    token: canonicalToken,
    fetchImpl,
  })
  const repositoryMetadata = validateRepositoryMetadata(
    await client.getRepository(),
    canonicalRepository,
    canonicalDefaultBranch,
  )
  traceImpl({
    milestone: 'repository-metadata-validated',
    target_sha: canonicalTargetSha,
    repository_id: String(repositoryMetadata.id),
  })

  const authorities = {}
  for (const [authorityKey, spec] of Object.entries(AUTHORITY_SPECS)) {
    authorities[authorityKey] = await resolveAuthority({
      authorityKey,
      spec,
      client,
      repositoryMetadata,
      defaultBranch: canonicalDefaultBranch,
      targetSha: canonicalTargetSha,
      traceImpl,
    })
  }

  const manifest = normalizeResolutionManifest({
    schema_version: RELEASE_EVIDENCE_SCHEMA_VERSION,
    target_sha: canonicalTargetSha,
    repository: {
      id: decimalID(repositoryMetadata.id, 'repository ID'),
      full_name: canonicalRepository,
      default_branch: canonicalDefaultBranch,
    },
    authorities,
  })
  traceImpl({
    milestone: 'release-evidence-resolution-complete',
    target_sha: canonicalTargetSha,
    authority_runs: Object.fromEntries(
      Object.entries(manifest.authorities).map(([key, authority]) => [
        key,
        {
          run_id: authority.run_id,
          run_attempt: authority.run_attempt,
        },
      ]),
    ),
  })
  return manifest
}

async function resolveAuthority({
  authorityKey,
  spec,
  client,
  repositoryMetadata,
  defaultBranch,
  targetSha,
  traceImpl,
}) {
  const workflow = validateWorkflowMetadata(
    await client.getWorkflow(spec.workflowFile),
    authorityKey,
    spec,
  )

  const candidates = []
  const seenRunIDs = new Set()
  for (const event of spec.allowedEvents) {
    const returnedRuns = await client.listWorkflowRuns({
      workflowId: workflow.id,
      defaultBranch,
      event,
      targetSha,
    })
    for (const value of returnedRuns) {
      const run = validateRunIdentity(value, {
        authorityKey,
        spec,
        workflowId: workflow.id,
        repositoryId: repositoryMetadata.id,
        defaultBranch,
        targetSha,
        expectedEvent: event,
      })
      if (seenRunIDs.has(run.id)) {
        throw new ReleaseEvidenceResolutionError(
          'duplicate-run',
          `${authorityKey} workflow run ${run.id} appears more than once`,
        )
      }
      seenRunIDs.add(run.id)
      candidates.push(run)
    }
  }
  if (candidates.length === 0) {
    throw new ReleaseEvidenceResolutionError(
      'missing-run',
      `no successful exact-SHA run exists for ${authorityKey}`,
    )
  }
  candidates.sort((left, right) => {
    if (left.createdAtMilliseconds !== right.createdAtMilliseconds) {
      return left.createdAtMilliseconds > right.createdAtMilliseconds ? -1 : 1
    }
    if (left.id === right.id) return 0
    return left.id > right.id ? -1 : 1
  })
  const selected = candidates[0]

  // Workflow metadata is logged only after a run is selected so every authority
  // milestone carries the immutable run/attempt correlation used by operators.
  traceAuthority(traceImpl, 'workflow-metadata-validated', {
    authorityKey,
    targetSha,
    selected,
    workflow_id: String(workflow.id),
  })
  traceAuthority(traceImpl, 'workflow-run-selected', {
    authorityKey,
    targetSha,
    selected,
    workflow_id: String(workflow.id),
  })

  const attemptRun = validateRunIdentity(
    await client.getRunAttempt({
      runId: selected.id,
      runAttempt: selected.runAttempt,
    }),
    {
      authorityKey,
      spec,
      workflowId: workflow.id,
      repositoryId: repositoryMetadata.id,
      defaultBranch,
      targetSha,
      expectedEvent: selected.event,
      expectedRunId: selected.id,
      expectedRunAttempt: selected.runAttempt,
      expectedCreatedAt: selected.createdAt,
    },
  )
  traceAuthority(traceImpl, 'workflow-run-attempt-validated', {
    authorityKey,
    targetSha,
    selected: attemptRun,
  })

  const jobs = await client.listAttemptJobs({
    runId: selected.id,
    runAttempt: selected.runAttempt,
  })
  const terminalJobs = spec.terminalJobNames.map((jobName) =>
    selectTerminalJob({
      values: jobs,
      jobName,
      authorityKey,
      spec,
      selected,
      defaultBranch,
      targetSha,
    }))
  traceAuthority(traceImpl, 'terminal-jobs-validated', {
    authorityKey,
    targetSha,
    selected,
    terminal_job_ids: terminalJobs.map((job) => String(job.id)),
  })

  const artifacts = []
  for (const expected of expectedArtifactDescriptors(
    authorityKey,
    targetSha,
    String(selected.id),
    selected.runAttempt,
  )) {
    const candidatesForName = await client.listRunArtifacts({
      runId: selected.id,
      artifactName: expected.name,
    })
    artifacts.push(selectArtifact({
      values: candidatesForName,
      expected,
      authorityKey,
      selected,
      repositoryId: repositoryMetadata.id,
      defaultBranch,
      targetSha,
    }))
  }
  traceAuthority(traceImpl, 'artifacts-associated', {
    authorityKey,
    targetSha,
    selected,
    artifact_ids: artifacts.map((artifact) => artifact.id),
  })

  validateRunIdentity(await client.getRun(selected.id), {
    authorityKey,
    spec,
    workflowId: workflow.id,
    repositoryId: repositoryMetadata.id,
    defaultBranch,
    targetSha,
    expectedEvent: selected.event,
    expectedRunId: selected.id,
    expectedRunAttempt: selected.runAttempt,
    expectedCreatedAt: selected.createdAt,
  })
  traceAuthority(traceImpl, 'workflow-run-final-state-validated', {
    authorityKey,
    targetSha,
    selected,
  })

  return deepFreeze({
    workflow_id: String(workflow.id),
    run_id: String(selected.id),
    run_attempt: selected.runAttempt,
    event: selected.event,
    terminal_job_ids: terminalJobs.map((job) => String(job.id)),
    artifacts: artifacts.map((artifact) => ({
      role: artifact.role,
      id: String(artifact.id),
      name: artifact.name,
      size_in_bytes: artifact.sizeInBytes,
      digest: artifact.digest,
    })),
  })
}

function validateRepositoryMetadata(value, repository, defaultBranch) {
  requireRecord(value, 'repository metadata', 'invalid-repository-identity')
  const id = positiveInteger(
    value.id,
    'repository metadata ID',
    'invalid-repository-identity',
  )
  if (value.full_name !== repository || value.default_branch !== defaultBranch) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-repository-identity',
      'GitHub repository metadata does not match the requested repository and branch',
    )
  }
  return Object.freeze({ id })
}

function validateWorkflowMetadata(value, authorityKey, spec) {
  requireRecord(value, `${authorityKey} workflow metadata`, 'invalid-workflow-identity')
  const id = positiveInteger(
    value.id,
    `${authorityKey} workflow ID`,
    'invalid-workflow-identity',
  )
  if (
    value.name !== spec.workflowName ||
    value.path !== spec.workflowPath ||
    value.state !== 'active'
  ) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-workflow-identity',
      `${authorityKey} workflow metadata does not match its fixed descriptor`,
    )
  }
  return Object.freeze({ id })
}

function validateRunIdentity(value, expected) {
  const { authorityKey } = expected
  try {
    requireRecord(value, `${authorityKey} workflow run`, 'invalid-run-identity')
    const id = requirePositiveSafeInteger(value.id, `${authorityKey} run ID`)
    const runAttempt = requirePositiveSafeInteger(
      value.run_attempt,
      `${authorityKey} run attempt`,
    )
    const createdAtMilliseconds = requireCanonicalTimestamp(
      value.created_at,
      `${authorityKey} run created_at`,
    )
    requireRepositoryObject(
      value.repository,
      expected.repositoryId,
      `${authorityKey} run repository`,
    )
    requireRepositoryObject(
      value.head_repository,
      expected.repositoryId,
      `${authorityKey} run head repository`,
    )
    const acceptedPath = value.path === expected.spec.workflowPath ||
      value.path === `${expected.spec.workflowPath}@${expected.defaultBranch}`
    if (
      value.workflow_id !== expected.workflowId ||
      value.name !== expected.spec.workflowName ||
      !acceptedPath ||
      value.head_sha !== expected.targetSha ||
      value.head_branch !== expected.defaultBranch ||
      value.event !== expected.expectedEvent ||
      value.status !== 'completed' ||
      value.conclusion !== 'success' ||
      expected.expectedRunId !== undefined && id !== expected.expectedRunId ||
      expected.expectedRunAttempt !== undefined &&
        runAttempt !== expected.expectedRunAttempt ||
      expected.expectedCreatedAt !== undefined &&
        value.created_at !== expected.expectedCreatedAt
    ) {
      throw new ReleaseEvidenceResolutionError(
        'invalid-run-identity',
        `${authorityKey} workflow run ${id} contradicts the exact-SHA query`,
      )
    }
    return Object.freeze({
      id,
      runAttempt,
      createdAt: value.created_at,
      createdAtMilliseconds,
      event: value.event,
    })
  } catch (cause) {
    if (
      cause instanceof ReleaseEvidenceResolutionError &&
      cause.code === 'invalid-run-identity'
    ) throw cause
    throw new ReleaseEvidenceResolutionError(
      'invalid-run-identity',
      `${authorityKey} workflow run has invalid identity metadata`,
      { cause },
    )
  }
}

function selectTerminalJob({
  values,
  jobName,
  authorityKey,
  spec,
  selected,
  defaultBranch,
  targetSha,
}) {
  const matches = values.filter((value) => value.name === jobName)
  if (matches.length === 0) {
    throw new ReleaseEvidenceResolutionError(
      'missing-terminal-job',
      `${authorityKey} run ${selected.id} is missing terminal job ${jobName}`,
    )
  }
  if (matches.length !== 1) {
    throw new ReleaseEvidenceResolutionError(
      'duplicate-terminal-job',
      `${authorityKey} run ${selected.id} repeats terminal job ${jobName}`,
    )
  }

  const job = matches[0]
  const attemptReturned = Object.hasOwn(job, 'run_attempt')
  if (
    job.run_id !== selected.id ||
    attemptReturned && job.run_attempt !== selected.runAttempt ||
    job.head_sha !== targetSha ||
    job.head_branch !== defaultBranch ||
    job.workflow_name !== spec.workflowName ||
    job.status !== 'completed' ||
    job.conclusion !== 'success'
  ) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-terminal-job',
      `${authorityKey} terminal job ${job.id} is not successful evidence for the selected attempt`,
    )
  }
  return Object.freeze({ id: job.id })
}

function selectArtifact({
  values,
  expected,
  authorityKey,
  selected,
  repositoryId,
  defaultBranch,
  targetSha,
}) {
  const matches = values.filter((value) => value.name === expected.name)
  if (matches.length === 0) {
    throw new ReleaseEvidenceResolutionError(
      'missing-artifact',
      `${authorityKey} run ${selected.id} is missing artifact ${expected.name}`,
    )
  }
  if (matches.length !== 1) {
    throw new ReleaseEvidenceResolutionError(
      'duplicate-artifact',
      `${authorityKey} run ${selected.id} repeats artifact ${expected.name}`,
    )
  }

  const artifact = matches[0]
  if (artifact.expired !== false) {
    throw new ReleaseEvidenceResolutionError(
      'expired-artifact',
      `${authorityKey} artifact ${artifact.id} is expired or has invalid expiry metadata`,
    )
  }
  let sizeInBytes
  let digest
  try {
    sizeInBytes = requirePositiveSafeInteger(
      artifact.size_in_bytes,
      `${authorityKey} artifact size`,
    )
  } catch (cause) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-artifact-size',
      `${authorityKey} artifact ${artifact.id} has an invalid size`,
      { cause },
    )
  }
  try {
    digest = requireSha256Digest(
      artifact.digest,
      `${authorityKey} artifact digest`,
    )
  } catch (cause) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-artifact-digest',
      `${authorityKey} artifact ${artifact.id} has an invalid digest`,
      { cause },
    )
  }

  const workflowRun = artifact.workflow_run
  if (
    workflowRun === null ||
    typeof workflowRun !== 'object' ||
    Array.isArray(workflowRun) ||
    workflowRun.id !== selected.id ||
    workflowRun.repository_id !== repositoryId ||
    workflowRun.head_repository_id !== repositoryId ||
    workflowRun.head_branch !== defaultBranch ||
    workflowRun.head_sha !== targetSha
  ) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-artifact-association',
      `${authorityKey} artifact ${artifact.id} is not associated with the selected run and SHA`,
    )
  }
  return Object.freeze({
    role: expected.role,
    id: artifact.id,
    name: expected.name,
    sizeInBytes,
    digest,
  })
}

function requireRepositoryObject(value, repositoryId, label) {
  if (
    value === null ||
    typeof value !== 'object' ||
    Array.isArray(value) ||
    value.id !== repositoryId
  ) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-run-identity',
      `${label} does not match the selected repository`,
    )
  }
}

function positiveInteger(value, label, code) {
  try {
    return requirePositiveSafeInteger(value, label)
  } catch (cause) {
    throw new ReleaseEvidenceResolutionError(code, `${label} is invalid`, { cause })
  }
}

function requireRecord(value, label, code) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new ReleaseEvidenceResolutionError(code, `${label} must be an object`)
  }
  return value
}

function traceAuthority(traceImpl, milestone, {
  authorityKey,
  targetSha,
  selected,
  ...context
}) {
  traceImpl({
    milestone,
    target_sha: targetSha,
    authority: authorityKey,
    run_id: String(selected.id),
    run_attempt: selected.runAttempt,
    ...context,
  })
}

function emitStructuredTrace(record) {
  const releaseRunID = process.env.GITHUB_RUN_ID
  const releaseRunAttempt = process.env.GITHUB_RUN_ATTEMPT
  process.stdout.write(`${JSON.stringify({
    ...record,
    release_run_id: releaseRunID ?? null,
    release_run_attempt: releaseRunAttempt ?? null,
  })}\n`)
}

function parseOptions(arguments_) {
  if (arguments_.length % 2 !== 0) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-cli-options',
      'resolver options must be --name value pairs',
    )
  }
  const options = new Map()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (typeof name !== 'string' || !name.startsWith('--') || value === '') {
      throw new ReleaseEvidenceResolutionError(
        'invalid-cli-options',
        'resolver options must be non-empty --name value pairs',
      )
    }
    if (options.has(name)) {
      throw new ReleaseEvidenceResolutionError(
        'invalid-cli-options',
        `duplicate resolver option ${name}`,
      )
    }
    options.set(name, value)
  }
  const allowed = new Set([
    '--repository',
    '--default-branch',
    '--target-sha',
    '--output',
    '--github-output',
    ...Object.keys(EXPECTED_OPTION_TO_OUTPUT_KEY),
  ])
  for (const name of options.keys()) {
    if (!allowed.has(name)) {
      throw new ReleaseEvidenceResolutionError(
        'invalid-cli-options',
        `unsupported resolver option ${name}`,
      )
    }
  }
  return options
}

function requiredOption(options, name) {
  const value = options.get(name)
  if (value === undefined || value === '') {
    throw new ReleaseEvidenceResolutionError(
      'invalid-cli-options',
      `missing required resolver option ${name}`,
    )
  }
  return value
}

function expectedOutputs(options) {
  const supplied = Object.keys(EXPECTED_OPTION_TO_OUTPUT_KEY)
    .filter((name) => options.has(name))
  if (supplied.length === 0) return undefined
  if (supplied.length !== WORKFLOW_OUTPUT_KEYS.length) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-cli-options',
      'resolver expected-output options must be supplied as one complete set',
    )
  }
  return Object.freeze(Object.fromEntries(
    Object.entries(EXPECTED_OPTION_TO_OUTPUT_KEY).map(([option, key]) => [
      key,
      requiredOption(options, option),
    ]),
  ))
}

export function requireExpectedWorkflowOutputs(actual, expected) {
  if (expected === undefined) return
  for (const key of WORKFLOW_OUTPUT_KEYS) {
    if (expected[key].includes('\r') || expected[key].includes('\n')) {
      throw new ReleaseEvidenceResolutionError(
        'invalid-expected-output',
        `expected resolver output ${key} contains a line break`,
      )
    }
    if (expected[key] !== actual[key]) {
      throw new ReleaseEvidenceResolutionError(
        'resolution-changed',
        `release evidence selection changed for ${key}`,
      )
    }
  }
}

async function writeExclusiveManifest(outputPath, manifest) {
  const canonicalPath = resolve(outputPath)
  await mkdir(dirname(canonicalPath), { recursive: true })
  await writeFile(canonicalPath, serializeResolutionManifest(manifest), {
    encoding: 'utf8',
    flag: 'wx',
    mode: 0o600,
  })
}

async function appendWorkflowOutputs(outputPath, outputs) {
  const records = WORKFLOW_OUTPUT_KEYS.map((key) => {
    const value = outputs[key]
    if (typeof value !== 'string' || value.includes('\r') || value.includes('\n')) {
      throw new ReleaseEvidenceResolutionError(
        'invalid-workflow-output',
        `resolver workflow output ${key} is not a safe scalar`,
      )
    }
    return `${key}=${value}\n`
  }).join('')
  await appendFile(resolve(outputPath), records, { encoding: 'utf8' })
}

export async function runResolverCLI({
  arguments_ = process.argv.slice(2),
  environment = process.env,
  fetchImpl = globalThis.fetch,
  traceImpl = emitStructuredTrace,
} = {}) {
  const options = parseOptions(arguments_)
  const repository = requiredOption(options, '--repository')
  const defaultBranch = requiredOption(options, '--default-branch')
  const targetSha = requiredOption(options, '--target-sha')
  const outputPath = requiredOption(options, '--output')
  const githubOutput = options.get('--github-output')
  if (
    githubOutput !== undefined &&
    resolve(githubOutput) === resolve(outputPath)
  ) {
    throw new ReleaseEvidenceResolutionError(
      'invalid-cli-options',
      'resolution manifest and GITHUB_OUTPUT paths must be distinct',
    )
  }
  const token = requireToken(environment.GITHUB_TOKEN)
  const manifest = await resolveReleaseEvidence({
    repository,
    defaultBranch,
    targetSha,
    token,
    fetchImpl,
    traceImpl,
  })
  const outputs = releaseWorkflowOutputs(manifest)
  requireExpectedWorkflowOutputs(outputs, expectedOutputs(options))

  await writeExclusiveManifest(outputPath, manifest)
  if (githubOutput !== undefined) await appendWorkflowOutputs(githubOutput, outputs)
  return Object.freeze({ manifest, outputs })
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  runResolverCLI().catch((cause) => {
    const code = cause instanceof ReleaseEvidenceResolutionError ||
      cause instanceof GitHubActionsAPIError ||
      cause instanceof ReleaseEvidenceContractError
      ? cause.code
      : 'resolver-infrastructure-failure'
    process.stderr.write(`${JSON.stringify({
      outcome: 'failed',
      code,
      message: cause instanceof Error ? cause.message : String(cause),
    })}\n`)
    process.exitCode = 1
  })
}
