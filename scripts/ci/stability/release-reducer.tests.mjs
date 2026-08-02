import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { crc32, deflateRawSync } from 'node:zlib'

import {
  MAXIMUM_ARTIFACT_ARCHIVE_BYTES,
  sha256ArtifactDigest,
} from './artifact.mjs'
import {
  STABILITY_WORKFLOW_JOBS,
  createProductVerdictForTermination,
  createStabilityResult,
  createStabilityStartedEvent,
  stabilityEvidenceDigest,
} from './result.mjs'
import {
  createStabilityExecutionContract,
  loadCurrentStabilityExecutionContract,
  loadCurrentStabilityExecutionSources,
} from './execution-contract.mjs'
import {
  STABILITY_RELEASE_VERDICT_SCHEMA_VERSION,
  REQUIRED_REPRODUCTION_OBSERVATIONS,
  REQUIRED_RESOLUTION_PASS_SAMPLES,
  releaseReducerProcessResult,
  reduceStabilityHistory,
} from './release-reducer.mjs'

const repository = 'windshare/windshare'
const workflow = 'stability.yml'
const repositoryRoot = resolve('.')
const commit = 'a'.repeat(40)
const failureCommit = 'b'.repeat(40)
const correctedCommit = 'c'.repeat(40)
const currentContracts = new Map(['linux', 'windows'].map((operatingSystem) => [
  operatingSystem,
  loadCurrentStabilityExecutionContract({ operatingSystem, repositoryRoot }),
]))
const currentSources = new Map(['linux', 'windows'].flatMap((operatingSystem) =>
  loadCurrentStabilityExecutionSources({ operatingSystem, repositoryRoot })
    .map(({ path, source }) => [path, Buffer.from(source)])))

const asymmetricRuns = [workflowRun(303), workflowRun(302), workflowRun(301)]
const asymmetric = fixture(asymmetricRuns)
removeArtifact(asymmetric, 303, 'linux')
asymmetric.runs[0].conclusion = 'failure'
replaceWithInvalidEvidence(asymmetric, 302, 'windows')
const asymmetricVerdict = await reduce(asymmetric, 2)
assert.equal(asymmetricVerdict.schema_version, STABILITY_RELEASE_VERDICT_SCHEMA_VERSION)
assert.equal(asymmetricVerdict.outcome, 'passed')
assert.deepEqual(asymmetricVerdict.sample_counts, { linux: 2, windows: 2 })
assert.deepEqual(
  asymmetricVerdict.samples.linux.map(({ workflow_run_id: runID }) => runID),
  ['302', '301'],
)
assert.deepEqual(
  asymmetricVerdict.samples.windows.map(({ workflow_run_id: runID }) => runID),
  ['303', '301'],
)
assert.equal(asymmetricVerdict.invalid_samples.linux[0].reason_code, 'missing-artifact')
assert.equal(
  asymmetricVerdict.invalid_samples.windows[0].reason_code,
  'invalid-structured-evidence',
)

const insufficient = fixture([workflowRun(202), workflowRun(201)])
removeArtifact(insufficient, 202, 'windows')
insufficient.jobs.get(202).jobs.find(({ name }) =>
  name === STABILITY_WORKFLOW_JOBS.windows.jobName).steps[0].conclusion = 'failure'
const insufficientVerdict = await reduce(insufficient, 2)
assert.equal(insufficientVerdict.outcome, 'insufficient-history')
assert.equal(insufficientVerdict.failure, null)
assert.deepEqual(insufficientVerdict.sample_counts, { linux: 2, windows: 1 })
assert.deepEqual(insufficientVerdict.insufficient_history, [{
  operating_system: 'windows',
  valid_sample_count: 1,
  required_sample_count: 2,
}])
const insufficientProcess = releaseReducerProcessResult(insufficientVerdict)
assert.equal(insufficientProcess.exitCode, 0)
assert.equal(insufficientProcess.stream, 'stdout')
assert.match(insufficientProcess.message, /INSUFFICIENT HISTORY.*resolution not evaluated.*current-commit/u)

const productFailure = fixture([workflowRun(401, { conclusion: 'failure' })])
setProductFailure(productFailure, 401, 'linux', 23)
const productFailureVerdict = await reduce(productFailure, 1)
assert.equal(productFailureVerdict.outcome, 'passed')
assert.equal(productFailureVerdict.failure, null)
assert.deepEqual(productFailureVerdict.finding_policy, {
  schema_version: 'windshare.stability-finding-policy/v2',
  finding_key_schema_version: 'windshare.stability-finding-key/v1',
  reproducibility_observation_count: REQUIRED_REPRODUCTION_OBSERVATIONS,
  resolution_pass_count: REQUIRED_RESOLUTION_PASS_SAMPLES,
  reproducibility_authority: 'distinct-valid-run-artifact-observations',
  resolution_authority: 'newer-valid-passes-on-strict-descendant-commits',
  unresolved_reproduced_disposition: 'release-blocking',
  resolved_disposition: 'trend-only',
  insufficient_history_disposition: 'non-blocking-resolution-not-evaluated',
  release_blocking_authority: 'historical-findings-and-current-commit-validation',
})
assert.deepEqual(productFailureVerdict.sample_counts, { linux: 1, windows: 1 })
assert.equal(productFailureVerdict.findings.length, 1)
assert.equal(productFailureVerdict.findings[0].operating_system, 'linux')
assert.equal(productFailureVerdict.findings[0].failure_class, 'product')
assert.equal(productFailureVerdict.findings[0].exit_code, 23)
assert.equal(productFailureVerdict.findings[0].resolution_state, 'unresolved')
assert.equal(productFailureVerdict.findings[0].reproducibility_state, 'not-reproduced')
assert.equal(productFailureVerdict.findings[0].release_disposition, 'tracked-single-observation')
assert.equal(productFailureVerdict.samples.linux[0].product_verdict.outcome, 'failed')

const signalFailure = fixture([workflowRun(411, { conclusion: 'failure' })])
const signalVerdict = createProductVerdictForTermination(null, 'SIGTERM')
replaceEvidenceArchive(signalFailure, 411, 'windows', { verdict: signalVerdict })
replaceJob(signalFailure, 411, 'windows', signalVerdict)
const signalFailureVerdict = await reduce(signalFailure, 1)
assert.equal(signalFailureVerdict.outcome, 'passed')
assert.equal(signalFailureVerdict.sample_counts.windows, 1)
assert.equal(signalFailureVerdict.findings[0].termination_kind, 'signal')
assert.equal(signalFailureVerdict.findings[0].signal, 'SIGTERM')

const reproducedFailure = fixture([
  workflowRun(481, { conclusion: 'failure', head_sha: failureCommit }),
  workflowRun(480, { conclusion: 'failure', head_sha: failureCommit }),
])
setProductFailure(reproducedFailure, 481, 'linux', 23)
setProductFailure(reproducedFailure, 480, 'linux', 23)
const reproducedFailureVerdict = await reduce(reproducedFailure, 2)
assert.equal(reproducedFailureVerdict.outcome, 'failed')
assert.equal(reproducedFailureVerdict.failure.code, 'unresolved-reproducible-correctness-findings')
assert.deepEqual(reproducedFailureVerdict.failure.operating_systems, ['linux'])
const [blockingFinding] = reproducedFailureVerdict.findings
assert.match(blockingFinding.finding_id, /^[a-f0-9]{64}$/u)
assert.deepEqual(blockingFinding.finding_key, {
  schema_version: 'windshare.stability-finding-key/v1',
  execution_contract_schema_version: currentContracts.get('linux').schema_version,
  execution_contract_semantic_sha256: currentContracts.get('linux').semantic_contract_sha256,
  operating_system: 'linux',
  suite: 'integration',
  failure_class: 'product',
  termination_kind: 'exit-code',
  exit_code: 23,
  signal: null,
})
assert.equal(blockingFinding.observation_count, REQUIRED_REPRODUCTION_OBSERVATIONS)
assert.deepEqual(
  blockingFinding.observations.map(({ workflow_run_id: runID }) => runID),
  ['481', '480'],
)
assert.equal(new Set(blockingFinding.observations.map(
  ({ artifact_id: artifactID }) => artifactID,
)).size, REQUIRED_REPRODUCTION_OBSERVATIONS)
assert.equal(blockingFinding.reproducibility_state, 'reproduced')
assert.equal(blockingFinding.resolution_state, 'unresolved')
assert.equal(blockingFinding.release_disposition, 'release-blocking')
assert.deepEqual(reproducedFailureVerdict.failure.finding_ids, [blockingFinding.finding_id])
assert.equal(releaseReducerProcessResult(reproducedFailureVerdict).exitCode, 1)
assert.deepEqual(reproducedFailure.compareRequests, [])

const distinctFailureSignatures = fixture([
  workflowRun(486, { conclusion: 'failure', head_sha: failureCommit }),
  workflowRun(485, { conclusion: 'failure', head_sha: failureCommit }),
])
setProductFailure(distinctFailureSignatures, 486, 'linux', 23)
setProductFailure(distinctFailureSignatures, 485, 'linux', 24)
const distinctFailureVerdict = await reduce(distinctFailureSignatures, 2)
assert.equal(distinctFailureVerdict.outcome, 'passed')
assert.deepEqual(
  distinctFailureVerdict.findings.map(({ exit_code: exitCode }) => exitCode),
  [23, 24],
)
assert(distinctFailureVerdict.findings.every(
  ({ reproducibility_state: state }) => state === 'not-reproduced',
))

const repeatedFailureWithInsufficientHistory = fixture([
  workflowRun(491, { conclusion: 'failure', head_sha: failureCommit }),
  workflowRun(490, { conclusion: 'failure', head_sha: failureCommit }),
])
setProductFailure(repeatedFailureWithInsufficientHistory, 491, 'linux', 23)
setProductFailure(repeatedFailureWithInsufficientHistory, 490, 'linux', 23)
const repeatedInsufficientVerdict = await reduce(repeatedFailureWithInsufficientHistory, 3)
assert.equal(repeatedInsufficientVerdict.outcome, 'insufficient-history')
assert.equal(repeatedInsufficientVerdict.failure, null)
assert.equal(repeatedInsufficientVerdict.findings[0].reproducibility_state, 'reproduced')
assert.equal(
  repeatedInsufficientVerdict.findings[0].resolution_state,
  'not-evaluated-insufficient-history',
)
assert.equal(
  repeatedInsufficientVerdict.findings[0].release_disposition,
  'non-blocking-insufficient-history',
)
assert.deepEqual(repeatedFailureWithInsufficientHistory.compareRequests, [])

const resolutionRuns = [
  ...Array.from({ length: REQUIRED_RESOLUTION_PASS_SAMPLES }, (_, index) =>
    workflowRun(600 - index, { head_sha: correctedCommit })),
  workflowRun(580, { conclusion: 'failure', head_sha: failureCommit }),
  workflowRun(579, { conclusion: 'failure', head_sha: failureCommit }),
]
const resolvedFailure = fixture(resolutionRuns)
setProductFailure(resolvedFailure, 580, 'linux', 23)
setProductFailure(resolvedFailure, 579, 'linux', 23)
markStrictDescendant(resolvedFailure, failureCommit, correctedCommit)
const resolvedFailureVerdict = await reduce(resolvedFailure, resolutionRuns.length)
assert.equal(resolvedFailureVerdict.outcome, 'passed')
const [resolvedFinding] = resolvedFailureVerdict.findings
assert.equal(resolvedFinding.reproducibility_state, 'reproduced')
assert.equal(resolvedFinding.resolution_state, 'resolved')
assert.equal(resolvedFinding.release_disposition, 'trend-only-resolved')
assert.equal(resolvedFinding.resolution_pass_count, REQUIRED_RESOLUTION_PASS_SAMPLES)
assert.equal(new Set(resolvedFinding.resolution_passes.map(
  ({ workflow_run_id: runID, artifact_id: artifactID }) => `${runID}:${artifactID}`,
)).size, REQUIRED_RESOLUTION_PASS_SAMPLES)
assert(resolvedFinding.resolution_passes.every(
  ({ commit_sha: commitSha }) => commitSha === correctedCommit,
))
assert.deepEqual(resolvedFailure.compareRequests, [compareKey(failureCommit, correctedCommit)])
assert.match(releaseReducerProcessResult(resolvedFailureVerdict).message, /1 resolved trend finding/u)

const unverifiedResolution = fixture(resolutionRuns)
setProductFailure(unverifiedResolution, 580, 'linux', 23)
setProductFailure(unverifiedResolution, 579, 'linux', 23)
const unverifiedResolutionVerdict = await reduce(unverifiedResolution, resolutionRuns.length)
assert.equal(unverifiedResolutionVerdict.outcome, 'failed')
assert.equal(unverifiedResolutionVerdict.findings[0].resolution_state, 'unresolved')
assert.equal(unverifiedResolutionVerdict.findings[0].resolution_pass_count, 0)
assert.deepEqual(unverifiedResolution.compareRequests, [compareKey(failureCommit, correctedCommit)])

const unavailableResolutionCommit = fixture(resolutionRuns)
setProductFailure(unavailableResolutionCommit, 580, 'linux', 23)
setProductFailure(unavailableResolutionCommit, 579, 'linux', 23)
unavailableResolutionCommit.compareOverrides.set(compareKey(failureCommit, correctedCommit), {
  status: 404,
  value: {},
})
const unavailableResolutionVerdict = await reduce(
  unavailableResolutionCommit,
  resolutionRuns.length,
)
assert.equal(unavailableResolutionVerdict.outcome, 'failed')
assert.equal(unavailableResolutionVerdict.findings[0].resolution_state, 'unresolved')

const duplicateResolutionArtifact = fixture(resolutionRuns)
setProductFailure(duplicateResolutionArtifact, 580, 'linux', 23)
setProductFailure(duplicateResolutionArtifact, 579, 'linux', 23)
const duplicatedResolutionArtifact = artifactFor(duplicateResolutionArtifact, 600, 'linux')
duplicateResolutionArtifact.artifacts.get(600).artifacts.push({
  ...duplicatedResolutionArtifact,
  id: 992_600,
})
duplicateResolutionArtifact.artifacts.get(600).total_count += 1
markStrictDescendant(duplicateResolutionArtifact, failureCommit, correctedCommit)
const duplicateResolutionVerdict = await reduce(
  duplicateResolutionArtifact,
  resolutionRuns.length - 1,
)
assert.equal(duplicateResolutionVerdict.outcome, 'failed')
assert.equal(
  duplicateResolutionVerdict.findings[0].resolution_pass_count,
  REQUIRED_RESOLUTION_PASS_SAMPLES - 1,
)
assert.equal(
  duplicateResolutionVerdict.invalid_samples.linux[0].reason_code,
  'duplicate-artifact-candidate',
)

const latestFailureCommit = 'd'.repeat(40)
const resetResolutionRuns = [
  ...Array.from({ length: 10 }, (_, index) =>
    workflowRun(700 - index, { head_sha: correctedCommit })),
  workflowRun(690, { conclusion: 'failure', head_sha: latestFailureCommit }),
  ...Array.from({ length: 10 }, (_, index) =>
    workflowRun(689 - index, { head_sha: correctedCommit })),
  workflowRun(679, { conclusion: 'failure', head_sha: failureCommit }),
]
const resetResolution = fixture(resetResolutionRuns)
setProductFailure(resetResolution, 690, 'linux', 23)
setProductFailure(resetResolution, 679, 'linux', 23)
markStrictDescendant(resetResolution, latestFailureCommit, correctedCommit)
const resetResolutionVerdict = await reduce(resetResolution, resetResolutionRuns.length)
assert.equal(resetResolutionVerdict.outcome, 'failed')
assert.equal(resetResolutionVerdict.findings[0].resolution_pass_count, 10)
assert.equal(resetResolutionVerdict.findings[0].resolution_state, 'unresolved')

const malformedCompare = fixture(resolutionRuns)
setProductFailure(malformedCompare, 580, 'linux', 23)
setProductFailure(malformedCompare, 579, 'linux', 23)
malformedCompare.compareOverrides.set(compareKey(failureCommit, correctedCommit), {
  status: 200,
  value: { status: 'ahead', ahead_by: '1' },
})
await assert.rejects(
  () => reduce(malformedCompare, resolutionRuns.length),
  /compare response cannot prove stability finding ancestry/u,
)

const duplicateArtifactObservation = fixture([
  workflowRun(611, { conclusion: 'failure', head_sha: failureCommit }),
  workflowRun(610, { conclusion: 'failure', head_sha: failureCommit }),
])
setProductFailure(duplicateArtifactObservation, 611, 'linux', 23)
setProductFailure(duplicateArtifactObservation, 610, 'linux', 23)
const duplicatedArtifact = artifactFor(duplicateArtifactObservation, 611, 'linux')
duplicateArtifactObservation.artifacts.get(611).artifacts.push({
  ...duplicatedArtifact,
  id: 991_611,
})
duplicateArtifactObservation.artifacts.get(611).total_count += 1
const duplicateArtifactObservationVerdict = await reduce(duplicateArtifactObservation, 1)
assert.equal(duplicateArtifactObservationVerdict.outcome, 'passed')
assert.equal(duplicateArtifactObservationVerdict.findings[0].observation_count, 1)
assert.equal(duplicateArtifactObservationVerdict.findings[0].reproducibility_state, 'not-reproduced')
assert.equal(
  duplicateArtifactObservationVerdict.invalid_samples.linux[0].reason_code,
  'duplicate-artifact-candidate',
)

const duplicateRunIdentity = fixture([workflowRun(621), workflowRun(621)])
await assert.rejects(() => reduce(duplicateRunIdentity, 1), /duplicate workflow runs/u)

const outOfOrderRuns = fixture([
  workflowRun(631),
  workflowRun(630, { created_at: workflowRunCreatedAt(632) }),
])
await assert.rejects(() => reduce(outOfOrderRuns, 1), /stable newest-first order/u)

const failedEvidenceWithSuccessfulStep = fixture([
  workflowRun(421, { conclusion: 'failure' }),
  workflowRun(420),
])
setProductFailure(failedEvidenceWithSuccessfulStep, 421, 'linux', 17)
jobFor(failedEvidenceWithSuccessfulStep, 421, 'linux').steps
  .find(({ name }) => name === 'run native integration exactly once').conclusion = 'success'
const successfulStepVerdict = await reduce(failedEvidenceWithSuccessfulStep, 1)
assert.equal(successfulStepVerdict.samples.linux[0].workflow_run_id, '420')
assert.equal(
  successfulStepVerdict.invalid_samples.linux[0].reason_code,
  'job-verdict-mismatch',
)

const failedEvidenceWithSuccessfulJob = fixture([
  workflowRun(426, { conclusion: 'failure' }),
  workflowRun(425),
])
setProductFailure(failedEvidenceWithSuccessfulJob, 426, 'windows', 19)
jobFor(failedEvidenceWithSuccessfulJob, 426, 'windows').conclusion = 'success'
const successfulJobVerdict = await reduce(failedEvidenceWithSuccessfulJob, 1)
assert.equal(successfulJobVerdict.samples.windows[0].workflow_run_id, '425')
assert.equal(
  successfulJobVerdict.invalid_samples.windows[0].reason_code,
  'job-verdict-mismatch',
)

const failedEvidenceWithSuccessfulRun = fixture([workflowRun(431), workflowRun(430)])
setProductFailure(failedEvidenceWithSuccessfulRun, 431, 'linux', 29)
const successfulRunVerdict = await reduce(failedEvidenceWithSuccessfulRun, 1)
assert.equal(successfulRunVerdict.samples.linux[0].workflow_run_id, '430')
assert.equal(
  successfulRunVerdict.invalid_samples.linux[0].reason_code,
  'run-verdict-mismatch',
)

const failedPostAction = fixture([
  workflowRun(436, { conclusion: 'failure' }),
  workflowRun(435),
])
setProductFailure(failedPostAction, 436, 'windows', 31)
jobFor(failedPostAction, 436, 'windows').steps
  .find(({ name }) => name === 'Complete job').conclusion = 'failure'
const failedPostActionVerdict = await reduce(failedPostAction, 1)
assert.equal(failedPostActionVerdict.samples.windows[0].workflow_run_id, '435')
assert.equal(
  failedPostActionVerdict.invalid_samples.windows[0].reason_code,
  'infrastructure-step-failure',
)

const malformedPostAction = fixture([workflowRun(438), workflowRun(437)])
jobFor(malformedPostAction, 438, 'linux').steps
  .find(({ name }) => name === 'Complete job').conclusion = 'unexpected'
const malformedPostActionVerdict = await reduce(malformedPostAction, 1)
assert.equal(malformedPostActionVerdict.samples.linux[0].workflow_run_id, '437')
assert.equal(
  malformedPostActionVerdict.invalid_samples.linux[0].reason_code,
  'infrastructure-step-failure',
)

const automaticRetry = fixture([
  workflowRun(441, { run_attempt: 2 }),
  workflowRun(440),
])
const automaticRetryVerdict = await reduce(automaticRetry, 1)
assert.deepEqual(
  automaticRetryVerdict.samples.linux.map(({ workflow_run_id: runID }) => runID),
  ['440'],
)
assert.deepEqual(
  automaticRetryVerdict.samples.windows.map(({ workflow_run_id: runID }) => runID),
  ['440'],
)
assert.equal(automaticRetryVerdict.invalid_samples.linux[0].reason_code, 'invalid-run-identity')
assert.equal(automaticRetryVerdict.invalid_samples.windows[0].reason_code, 'invalid-run-identity')

const jobAttemptMismatch = fixture([workflowRun(451), workflowRun(450)])
jobFor(jobAttemptMismatch, 451, 'linux').run_attempt = 2
const jobAttemptVerdict = await reduce(jobAttemptMismatch, 1)
assert.equal(jobAttemptVerdict.samples.linux[0].workflow_run_id, '450')
assert.equal(jobAttemptVerdict.invalid_samples.linux[0].reason_code, 'invalid-job-identity')

const duplicateInvocation = fixture([
  workflowRun(461),
  workflowRun(460),
  workflowRun(459),
])
replaceEvidenceArchive(duplicateInvocation, 460, 'linux', {
  invocationId: invocationID(461, 'linux'),
})
const duplicateInvocationVerdict = await reduce(duplicateInvocation, 2)
assert.deepEqual(
  duplicateInvocationVerdict.samples.linux.map(({ workflow_run_id: runID }) => runID),
  ['461', '459'],
)
assert.deepEqual(
  duplicateInvocationVerdict.samples.windows.map(({ workflow_run_id: runID }) => runID),
  ['461', '460'],
)
assert.equal(
  duplicateInvocationVerdict.invalid_samples.linux[0].reason_code,
  'duplicate-invocation',
)

const prestartSetupFailure = fixture([workflowRun(501, { conclusion: 'failure' })])
const prestartLinuxJob = jobFor(prestartSetupFailure, 501, 'linux')
prestartLinuxJob.conclusion = 'failure'
prestartLinuxJob.steps[0].conclusion = 'failure'
prestartLinuxJob.steps.find(({ name }) =>
  name === 'run native integration exactly once').conclusion = 'skipped'
prestartLinuxJob.steps.find(({ name }) =>
  name === 'publish authenticated stability evidence').conclusion = 'skipped'
removeArtifact(prestartSetupFailure, 501, 'linux')
const prestartVerdict = await reduce(prestartSetupFailure, 1)
assert.equal(prestartVerdict.outcome, 'insufficient-history')
assert.equal(prestartVerdict.failure, null)
assert.equal(prestartVerdict.insufficient_history[0].operating_system, 'linux')
assert.deepEqual(prestartVerdict.sample_counts, { linux: 0, windows: 1 })
assert.equal(prestartVerdict.invalid_samples.linux[0].reason_code, 'missing-artifact')
assert.equal(prestartVerdict.findings.length, 0)

const uploadFailure = fixture([workflowRun(511, { conclusion: 'failure' })])
const uploadLinuxJob = jobFor(uploadFailure, 511, 'linux')
uploadLinuxJob.conclusion = 'failure'
uploadLinuxJob.steps.find(({ name }) =>
  name === 'publish authenticated stability evidence').conclusion = 'failure'
const uploadVerdict = await reduce(uploadFailure, 1)
assert.equal(uploadVerdict.sample_counts.linux, 0)
assert.equal(uploadVerdict.invalid_samples.linux[0].reason_code, 'upload-failure')

const extras = fixture([workflowRun(601)])
extras.jobs.get(601).jobs.push({
  id: 999_001,
  name: 'Unrelated diagnostics',
  labels: ['self-hosted'],
})
extras.jobs.get(601).total_count += 1
extras.artifacts.get(601).artifacts.push({
  id: 999_002,
  name: 'unrelated-debug-bundle',
  expired: true,
})
extras.artifacts.get(601).total_count += 1
const extrasVerdict = await reduce(extras, 1)
assert.equal(extrasVerdict.outcome, 'passed')
assert.deepEqual(extrasVerdict.sample_counts, { linux: 1, windows: 1 })

const duplicateArtifact = fixture([workflowRun(701), workflowRun(700)])
const duplicateSourceArtifact = artifactFor(duplicateArtifact, 701, 'linux')
duplicateArtifact.artifacts.get(701).artifacts.push({
  ...duplicateSourceArtifact,
  id: 777_001,
})
duplicateArtifact.artifacts.get(701).total_count += 1
const duplicateArtifactVerdict = await reduce(duplicateArtifact, 1)
assert.equal(duplicateArtifactVerdict.outcome, 'passed')
assert.equal(duplicateArtifactVerdict.samples.linux[0].workflow_run_id, '700')
assert.equal(
  duplicateArtifactVerdict.invalid_samples.linux[0].reason_code,
  'duplicate-artifact-candidate',
)
assert.equal(duplicateArtifactVerdict.samples.windows[0].workflow_run_id, '701')

const duplicateArchiveCandidate = fixture([workflowRun(706), workflowRun(705)])
replaceEvidenceArchive(duplicateArchiveCandidate, 706, 'linux', { duplicateFinished: true })
const duplicateArchiveVerdict = await reduce(duplicateArchiveCandidate, 1)
assert.equal(duplicateArchiveVerdict.samples.linux[0].workflow_run_id, '705')
assert.equal(
  duplicateArchiveVerdict.invalid_samples.linux[0].reason_code,
  'invalid-structured-evidence',
)

const duplicateJob = fixture([workflowRun(711), workflowRun(710)])
duplicateJob.jobs.get(711).jobs.push({
  ...jobFor(duplicateJob, 711, 'windows'),
  id: 777_002,
})
duplicateJob.jobs.get(711).total_count += 1
const duplicateJobVerdict = await reduce(duplicateJob, 1)
assert.equal(duplicateJobVerdict.samples.windows[0].workflow_run_id, '710')
assert.equal(
  duplicateJobVerdict.invalid_samples.windows[0].reason_code,
  'duplicate-job-candidate',
)
assert.equal(duplicateJobVerdict.samples.linux[0].workflow_run_id, '711')

const digestMismatch = fixture([workflowRun(721), workflowRun(720)])
artifactFor(digestMismatch, 721, 'linux').digest = `sha256:${'0'.repeat(64)}`
const digestVerdict = await reduce(digestMismatch, 1)
assert.equal(digestVerdict.samples.linux[0].workflow_run_id, '720')
assert.equal(
  digestVerdict.invalid_samples.linux[0].reason_code,
  'artifact-digest-mismatch',
)

const contractDrift = fixture([workflowRun(801)])
const linuxIntegrationPath = currentContracts.get('linux').sources
  .find(({ role }) => role === 'integration-entrypoint').path
contractDrift.sources.set(
  linuxIntegrationPath,
  Buffer.from(contractDrift.sources.get(linuxIntegrationPath).toString('utf8').replace(
    '"revision":3',
    '"revision":4',
  )),
)
const driftContract = contractFromFixtureSources(contractDrift, 'linux')
replaceEvidenceArchive(contractDrift, 801, 'linux', { executionContract: driftContract })
const contractDriftVerdict = await reduce(contractDrift, 1)
assert.equal(contractDriftVerdict.sample_counts.linux, 0)
assert.equal(contractDriftVerdict.sample_counts.windows, 1)
assert.equal(
  contractDriftVerdict.invalid_samples.linux[0].reason_code,
  'execution-contract-drift',
)

const independentRuns = Array.from({ length: 101 }, (_, index) => workflowRun(2_000 - index))
const independent = fixture(independentRuns)
removeArtifact(independent, 2_000, 'linux')
removeArtifact(independent, 1_900, 'windows')
const independentVerdict = await reduce(independent, 100)
assert.equal(independentVerdict.outcome, 'passed')
assert.deepEqual(independentVerdict.sample_counts, { linux: 100, windows: 100 })
assert.equal(independentVerdict.samples.windows[0].workflow_run_id, '2000')
assert.equal(independentVerdict.samples.windows.at(-1).workflow_run_id, '1901')
assert.equal(independentVerdict.samples.linux[0].workflow_run_id, '1999')
assert.equal(independentVerdict.samples.linux.at(-1).workflow_run_id, '1900')
assert.equal(independentVerdict.invalid_samples.linux.length, 1)
assert.equal(independentVerdict.invalid_samples.windows.length, 0)

assert.deepEqual(releaseReducerProcessResult({
  outcome: 'failed',
  failure: { message: 'GitHub API unavailable' },
}), {
  exitCode: 1,
  stream: 'stderr',
  message: 'stability-release-reducer: GitHub API unavailable',
})
assert.throws(() => releaseReducerProcessResult({ outcome: 'unknown' }), /unsupported/u)

await assert.rejects(() => reduceStabilityHistory({
  repository,
  workflow,
  requiredRuns: 1,
  token: 'test-token',
  fetchImpl: async () => jsonResponse({}, 503),
}), /status 503/u)

const terminalRoot = mkdtempSync(join(tmpdir(), 'windshare-stability-reducer-'))
try {
  const output = join(terminalRoot, 'verdict.json')
  const child = spawnSync(process.execPath, [
    fileURLToPath(new URL('./release-reducer.mjs', import.meta.url)),
    '--repository', repository,
    '--workflow', workflow,
    '--required-runs', '100',
    '--output', output,
  ], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: { ...process.env, GITHUB_TOKEN: '' },
  })
  assert.equal(child.status, 1)
  const verdict = JSON.parse(readFileSync(output, 'utf8'))
  assert.equal(verdict.schema_version, STABILITY_RELEASE_VERDICT_SCHEMA_VERSION)
  assert.equal(verdict.failure.code, 'stability-reducer-error')
  assert.equal(verdict.finding_policy.resolved_disposition, 'trend-only')
  assert.equal(
    verdict.finding_policy.release_blocking_authority,
    'historical-findings-and-current-commit-validation',
  )
} finally {
  rmSync(terminalRoot, { recursive: true, force: true })
}

console.log('stability release-reducer tests: PASS')

function workflowRun(id, overrides = {}) {
  return {
    id,
    event: 'schedule',
    status: 'completed',
    conclusion: 'success',
    head_branch: 'main',
    head_sha: commit,
    run_attempt: 1,
    created_at: workflowRunCreatedAt(id),
    ...overrides,
  }
}

function workflowRunCreatedAt(id) {
  return new Date(Date.UTC(2026, 0, 1) + id * 60_000).toISOString().replace('.000Z', 'Z')
}

function fixture(runs) {
  const context = {
    runs,
    jobs: new Map(),
    artifacts: new Map(),
    downloads: new Map(),
    sources: new Map([...currentSources].map(([path, bytes]) => [path, Buffer.from(bytes)])),
    commits: new Set(runs.map(({ head_sha: headSha }) => headSha)),
    strictDescendants: new Set(),
    compareOverrides: new Map(),
    compareRequests: [],
  }
  for (const run of runs) {
    const jobs = ['linux', 'windows'].map((operatingSystem) =>
      stabilityJob(run, operatingSystem))
    context.jobs.set(run.id, { total_count: jobs.length, jobs })
    const artifacts = ['linux', 'windows'].map((operatingSystem) => {
      const archive = evidenceArchive(run, operatingSystem)
      const id = artifactID(run.id, operatingSystem)
      context.downloads.set(id, archive)
      return {
        id,
        name: artifactName(run, operatingSystem),
        expired: false,
        size_in_bytes: archive.length,
        digest: sha256ArtifactDigest(archive),
        workflow_run: {
          id: run.id,
          head_sha: run.head_sha,
        },
      }
    })
    context.artifacts.set(run.id, { total_count: artifacts.length, artifacts })
  }
  return context
}

function stabilityJob(run, operatingSystem, verdict = createProductVerdictForTermination(0, null)) {
  const authority = STABILITY_WORKFLOW_JOBS[operatingSystem]
  const conclusion = verdict.outcome === 'passed' ? 'success' : 'failure'
  return {
    id: run.id * 10 + (operatingSystem === 'linux' ? 1 : 2),
    run_id: run.id,
    run_attempt: run.run_attempt,
    head_sha: run.head_sha,
    name: authority.jobName,
    labels: [authority.runnerLabel],
    status: 'completed',
    conclusion,
    steps: [
      { name: 'Set up job', status: 'completed', conclusion: 'success' },
      {
        name: 'run native integration exactly once',
        status: 'completed',
        conclusion,
      },
      {
        name: 'publish authenticated stability evidence',
        status: 'completed',
        conclusion: 'success',
      },
      { name: 'Complete job', status: 'completed', conclusion: 'success' },
    ],
  }
}

function evidenceArchive(run, operatingSystem, {
  executionContract = currentContracts.get(operatingSystem),
  verdict = createProductVerdictForTermination(0, null),
  includeFinished = true,
  duplicateFinished = false,
  invocationId = invocationID(run.id, operatingSystem),
} = {}) {
  const authority = STABILITY_WORKFLOW_JOBS[operatingSystem]
  const identity = {
    workflowRunId: String(run.id),
    workflowRunAttempt: run.run_attempt,
    commitSha: run.head_sha,
    workflowJob: authority.workflowJob,
    operatingSystem,
    suite: 'integration',
  }
  const started = createStabilityStartedEvent({
    ...identity,
    invocationId,
    executionContractSemanticSha256: executionContract.semantic_contract_sha256,
  })
  // Pretty/reordered documents prove neither JSON layout nor entry names are authorities.
  const startedDocument = `${JSON.stringify(reverseRecordOrder(started), null, 2)}\n`
  const entries = [
    { name: 'diagnostics/readme.txt', content: 'unrelated diagnostics\n' },
    {
      name: 'metadata/another-schema.json',
      content: JSON.stringify({ schema_version: 'windshare.unrelated/v1', value: true }),
    },
    {
      name: `events/${operatingSystem}-begin.data`,
      content: startedDocument,
      compression: 8,
      dataDescriptor: true,
    },
  ]
  if (includeFinished) {
    const result = createStabilityResult({
      ...identity,
      invocationId,
      startedEventSha256: stabilityEvidenceDigest(startedDocument),
      productVerdict: verdict,
      executionContract,
    })
    const reorderedResult = reverseRecordOrder({
      ...result,
      product_verdict: reverseRecordOrder(result.product_verdict),
      execution_contract: reverseRecordOrder({
        ...result.execution_contract,
        sources: result.execution_contract.sources.map(reverseRecordOrder),
      }),
    })
    const finishedDocument = `${JSON.stringify(reorderedResult, null, 2)}\n`
    entries.push({
      name: `arbitrary/${operatingSystem}-terminal.payload`,
      content: finishedDocument,
      compression: 8,
    })
    if (duplicateFinished) {
      entries.push({
        name: `arbitrary/${operatingSystem}-terminal-copy.payload`,
        content: finishedDocument,
      })
    }
  }
  return zipArchive(entries)
}

function setProductFailure(context, runID, operatingSystem, exitCode) {
  const verdict = createProductVerdictForTermination(exitCode, null)
  replaceEvidenceArchive(context, runID, operatingSystem, { verdict })
  replaceJob(context, runID, operatingSystem, verdict)
}

function replaceJob(context, runID, operatingSystem, verdict) {
  const run = context.runs.find(({ id }) => id === runID)
  const replacement = stabilityJob(run, operatingSystem, verdict)
  const response = context.jobs.get(runID)
  const index = response.jobs.findIndex(({ name }) =>
    name === STABILITY_WORKFLOW_JOBS[operatingSystem].jobName)
  response.jobs[index] = replacement
}

function replaceWithInvalidEvidence(context, runID, operatingSystem) {
  replaceEvidenceArchive(context, runID, operatingSystem, { includeFinished: false })
}

function replaceEvidenceArchive(context, runID, operatingSystem, options) {
  const run = context.runs.find(({ id }) => id === runID)
  const archive = evidenceArchive(run, operatingSystem, options)
  const artifact = artifactFor(context, runID, operatingSystem)
  context.downloads.set(artifact.id, archive)
  artifact.size_in_bytes = archive.length
  artifact.digest = sha256ArtifactDigest(archive)
}

function removeArtifact(context, runID, operatingSystem) {
  const response = context.artifacts.get(runID)
  const expected = artifactName(
    context.runs.find(({ id }) => id === runID),
    operatingSystem,
  )
  response.artifacts = response.artifacts.filter(({ name }) => name !== expected)
  response.total_count = response.artifacts.length
}

function artifactFor(context, runID, operatingSystem) {
  return context.artifacts.get(runID).artifacts.find(({ name }) =>
    name === artifactName(
      context.runs.find(({ id }) => id === runID),
      operatingSystem,
    ))
}

function jobFor(context, runID, operatingSystem) {
  return context.jobs.get(runID).jobs.find(({ name }) =>
    name === STABILITY_WORKFLOW_JOBS[operatingSystem].jobName)
}

function artifactName(run, operatingSystem) {
  return `stability-integration-${operatingSystem}-${run.id}-${run.run_attempt}`
}

function artifactID(runID, operatingSystem) {
  return runID * 10 + (operatingSystem === 'linux' ? 3 : 4)
}

function invocationID(runID, operatingSystem) {
  const suffix = `${operatingSystem === 'linux' ? 'a' : 'b'}${runID.toString(16)}`
    .padStart(12, '0')
    .slice(-12)
  return `00000000-0000-4000-8000-${suffix}`
}

function contractFromFixtureSources(context, operatingSystem) {
  const current = currentContracts.get(operatingSystem)
  return createStabilityExecutionContract({
    operatingSystem,
    sources: current.sources.map(({ role, path }) => ({
      role,
      path,
      source: context.sources.get(path),
    })),
  })
}

async function reduce(context, requiredRuns) {
  return reduceStabilityHistory({
    repository,
    workflow,
    requiredRuns,
    token: 'test-token',
    fetchImpl: mockGitHubAPI(context),
    repositoryRoot,
  })
}

function mockGitHubAPI(context) {
  return async (requestURL) => {
    const url = new URL(requestURL)
    if (url.pathname === `/repos/${repository}`) {
      return jsonResponse({ default_branch: 'main' })
    }
    if (url.pathname === `/repos/${repository}/actions/workflows/${workflow}/runs`) {
      const page = Number(url.searchParams.get('page'))
      const start = (page - 1) * 100
      return jsonResponse({
        total_count: context.runs.length,
        workflow_runs: context.runs.slice(start, start + 100),
      })
    }

    let match = /\/actions\/runs\/(\d+)\/jobs$/u.exec(url.pathname)
    if (match !== null) {
      const response = context.jobs.get(Number(match[1]))
      const page = Number(url.searchParams.get('page'))
      return jsonResponse({
        total_count: response.total_count,
        jobs: page === 1 ? response.jobs : [],
      })
    }
    match = /\/actions\/runs\/(\d+)\/artifacts$/u.exec(url.pathname)
    if (match !== null) {
      const response = context.artifacts.get(Number(match[1]))
      const page = Number(url.searchParams.get('page'))
      return jsonResponse({
        total_count: response.total_count,
        artifacts: page === 1 ? response.artifacts : [],
      })
    }
    match = /\/compare\/([a-f0-9]{40})\.\.\.([a-f0-9]{40})$/u.exec(url.pathname)
    if (match !== null) {
      const [, baseCommit, headCommit] = match
      const key = compareKey(baseCommit, headCommit)
      context.compareRequests.push(key)
      const override = context.compareOverrides.get(key)
      if (override !== undefined) return jsonResponse(override.value, override.status)
      const strictDescendant = context.strictDescendants.has(key)
      return jsonResponse({
        status: strictDescendant ? 'ahead' : 'diverged',
        ahead_by: strictDescendant ? 1 : 1,
        behind_by: strictDescendant ? 0 : 1,
        base_commit: { sha: baseCommit },
        merge_base_commit: { sha: strictDescendant ? baseCommit : 'f'.repeat(40) },
      })
    }
    match = /\/contents\/(.+)$/u.exec(url.pathname)
    if (match !== null) {
      const path = match[1].split('/').map(decodeURIComponent).join('/')
      const bytes = context.sources.get(path)
      if (bytes === undefined || !context.commits.has(url.searchParams.get('ref'))) {
        return jsonResponse({}, 404)
      }
      return jsonResponse({
        type: 'file',
        path,
        encoding: 'base64',
        size: bytes.byteLength,
        sha: gitBlobSha(bytes),
        content: bytes.toString('base64'),
      })
    }
    match = /\/actions\/artifacts\/(\d+)\/zip$/u.exec(url.pathname)
    if (match !== null) {
      return byteResponse(context.downloads.get(Number(match[1])) ?? Buffer.alloc(0))
    }
    return jsonResponse({}, 404)
  }
}

function compareKey(baseCommit, headCommit) {
  return `${baseCommit}\0${headCommit}`
}

function markStrictDescendant(context, baseCommit, headCommit) {
  context.strictDescendants.add(compareKey(baseCommit, headCommit))
}

function gitBlobSha(bytes) {
  return createHash('sha1')
    .update(Buffer.from(`blob ${bytes.byteLength}\0`, 'utf8'))
    .update(bytes)
    .digest('hex')
}

function jsonResponse(value, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() { return value },
  }
}

function byteResponse(value, status = 200) {
  const bytes = Buffer.from(value)
  let delivered = false
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: {
      get(name) {
        return name.toLowerCase() === 'content-length' ? String(bytes.length) : null
      },
    },
    body: {
      async cancel() {},
      getReader() {
        return {
          async read() {
            if (delivered) return { done: true, value: undefined }
            delivered = true
            return { done: false, value: bytes }
          },
          async cancel() {},
          releaseLock() {},
        }
      },
    },
  }
}

function zipArchive(entries) {
  const locals = []
  const centrals = []
  let localOffset = 0
  for (const entry of entries) {
    const nameBytes = Buffer.from(entry.name, 'utf8')
    const data = Buffer.from(entry.content, 'utf8')
    const compression = entry.compression ?? 0
    const compressed = compression === 8 ? deflateRawSync(data) : data
    const checksum = crc32(data) >>> 0
    const descriptorEnabled = entry.dataDescriptor === true
    const flags = (1 << 11) | (descriptorEnabled ? 1 << 3 : 0)

    const local = Buffer.alloc(30)
    local.writeUInt32LE(0x04034b50, 0)
    local.writeUInt16LE(20, 4)
    local.writeUInt16LE(flags, 6)
    local.writeUInt16LE(compression, 8)
    if (!descriptorEnabled) {
      local.writeUInt32LE(checksum, 14)
      local.writeUInt32LE(compressed.length, 18)
      local.writeUInt32LE(data.length, 22)
    }
    local.writeUInt16LE(nameBytes.length, 26)

    const descriptor = descriptorEnabled ? Buffer.alloc(16) : Buffer.alloc(0)
    if (descriptorEnabled) {
      descriptor.writeUInt32LE(0x08074b50, 0)
      descriptor.writeUInt32LE(checksum, 4)
      descriptor.writeUInt32LE(compressed.length, 8)
      descriptor.writeUInt32LE(data.length, 12)
    }

    const central = Buffer.alloc(46)
    central.writeUInt32LE(0x02014b50, 0)
    central.writeUInt16LE(20, 4)
    central.writeUInt16LE(20, 6)
    central.writeUInt16LE(flags, 8)
    central.writeUInt16LE(compression, 10)
    central.writeUInt32LE(checksum, 16)
    central.writeUInt32LE(compressed.length, 20)
    central.writeUInt32LE(data.length, 24)
    central.writeUInt16LE(nameBytes.length, 28)
    central.writeUInt32LE(localOffset, 42)

    const localRecord = Buffer.concat([local, nameBytes, compressed, descriptor])
    const centralRecord = Buffer.concat([central, nameBytes])
    locals.push(localRecord)
    centrals.push(centralRecord)
    localOffset += localRecord.length
  }

  const localBytes = Buffer.concat(locals)
  const centralBytes = Buffer.concat(centrals)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(centralBytes.length, 12)
  end.writeUInt32LE(localBytes.length, 16)
  const archive = Buffer.concat([localBytes, centralBytes, end])
  assert.ok(archive.length <= MAXIMUM_ARTIFACT_ARCHIVE_BYTES)
  return archive
}

function reverseRecordOrder(value) {
  return Object.fromEntries(Object.entries(value).reverse())
}
