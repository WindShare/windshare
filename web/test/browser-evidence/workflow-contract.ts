import assert from 'node:assert/strict'

import { parseDocument } from 'yaml'

import { createGithubSuiteJobDeadlinePolicy } from '../../../scripts/ci/browsergate/operation-deadlines.mjs'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

export type WorkflowValue = string | number | boolean | null | WorkflowValue[] | WorkflowMapping
export type WorkflowMapping = Map<string, WorkflowValue>

export const WORKFLOW_ALIAS_EXPANSION_LIMIT = 100

const GITHUB_EXPRESSION_OPEN = '${{'
const NODE_VERSION_FILE = '.node-version'
const GENERATED_SEMANTIC_PROCESS_JOB = 'browser-generated-semantic-process'
const WINDOWS_BROWSER_PROCESS_JOB = 'windows-browser-process'
const CHECKOUT_ACTION = 'actions/checkout@v7'
const PNPM_SETUP_ACTION = 'pnpm/action-setup@v6'
const NODE_SETUP_ACTION = 'actions/setup-node@v6'
export const GENERATED_SEMANTIC_PROCESS_TARGET = 'test:browser:generated-semantic:process'
export const GENERATED_SEMANTIC_PROCESS_ENTRY =
  '../scripts/ci/browsergate/tests/process/generated-semantic.tests.mjs'
export const GENERATED_SEMANTIC_PROCESS_COMMAND = `node ${GENERATED_SEMANTIC_PROCESS_ENTRY}`
export const BROWSER_PROCESS_INTEGRATION_TARGET = 'test:browser:process:integration'
export const BROWSER_PROCESS_INTEGRATION_COMMAND = [
  'vitest run',
  'test/browser-evidence/native-directory-publisher.test.ts',
  'test/browser-evidence/process-runner.test.ts',
  'test/browser-evidence/windows-job-backend.test.ts',
  'test/browser-evidence/windows-job-client.test.ts',
].join(' ')
export const BROWSER_PROCESS_TARGET = 'test:browser:process'
export const BROWSER_PROCESS_COMMAND =
  `pnpm run ${BROWSER_PROCESS_INTEGRATION_TARGET} && pnpm run ${GENERATED_SEMANTIC_PROCESS_TARGET}`
const EXPECTED_SETUP_NODE_JOBS = Object.freeze([
  'browser-contract',
  GENERATED_SEMANTIC_PROCESS_JOB,
  'browser-main',
  'browser-pion',
  'hygiene',
  'web',
  WINDOWS_BROWSER_PROCESS_JOB,
])
const BROWSER_VERDICT_BUDGET_MINUTES = Object.freeze({
  job: 15,
  checkout: 2,
  download: 2,
  reducer: 3,
  reserve: 5,
})
export const CRITICAL_JOB_RESERVE_MINUTES = Object.freeze({
  'browser-contract': 5,
  [GENERATED_SEMANTIC_PROCESS_JOB]: 5,
  'browser-main': 10,
  'browser-pion': 10,
  'browser-verdict': BROWSER_VERDICT_BUDGET_MINUTES.reserve,
  [WINDOWS_BROWSER_PROCESS_JOB]: 5,
})

const OCI_RUNTIME_PATTERN = /\b(?:docker|podman|oci)\b/iu
const SECRET_EXPRESSION_PATTERN = /\bsecrets\.\w+\b/iu
const CREDENTIAL_AUTHORITY_PATTERN = /\bGITHUB_TOKEN\b|\bgithub\.token\b|\bsecrets\.\w+\b/iu

type CriticalJobName = keyof typeof CRITICAL_JOB_RESERVE_MINUTES
type CriticalJobs = Record<CriticalJobName, WorkflowMapping>
type BrowserSuite = 'main' | 'pion'

export class WorkflowYamlError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'WorkflowYamlError'
  }
}

export function parseWorkflowYaml(source: string): WorkflowMapping {
  const document = parseDocument(source, {
    merge: false,
    strict: true,
    stringKeys: true,
    uniqueKeys: true,
    version: '1.2',
  })
  if (document.errors.length > 0) {
    const details = document.errors
      .map((error) => `${error.code ?? 'YAML_ERROR'}: ${firstLine(error.message)}`)
      .join('; ')
    throw new WorkflowYamlError(`workflow YAML parse failed: ${details}`)
  }

  let value: unknown
  try {
    value = document.toJS({
      mapAsMap: true,
      maxAliasCount: WORKFLOW_ALIAS_EXPANSION_LIMIT,
    })
  } catch (cause) {
    throw new WorkflowYamlError(
      `workflow YAML alias expansion failed: ${errorMessage(cause)}`,
      { cause },
    )
  }
  if (!(value instanceof Map)) {
    throw new WorkflowYamlError('workflow YAML root must be a mapping')
  }
  return value as WorkflowMapping
}

export function validateWorkflowSource(source: string): void {
  validateWorkflowContract(parseWorkflowYaml(source))
}

export function validateWorkflowContract(workflow: WorkflowMapping): void {
  validateTriggers(workflow)
  validateExecutionSurface(workflow)
  validateNodeVersionAuthority(workflow)

  const jobs = criticalJobs(workflow)
  assert.deepEqual(
    jobNeeds(jobs['browser-contract']),
    [],
    'browser contract must remain the root of only the browser evidence DAG',
  )
  assert.deepEqual(
    jobNeeds(jobs[GENERATED_SEMANTIC_PROCESS_JOB]),
    [],
    'generated-semantic process isolation must remain outside the browser evidence DAG',
  )
  assert.deepEqual(
    jobNeeds(jobs['browser-main']),
    ['browser-contract'],
    'browser main must depend only on the contract gate',
  )
  assert.deepEqual(
    jobNeeds(jobs['browser-pion']),
    ['browser-contract'],
    'browser Pion must depend only on the contract gate',
  )
  assert.deepEqual(
    jobNeeds(jobs['browser-verdict']),
    ['browser-main', 'browser-pion'],
    'browser verdict must depend only on the two artifact producers',
  )

  for (const [jobName, reserveMinutes] of Object.entries(CRITICAL_JOB_RESERVE_MINUTES)) {
    const criticalJobName = jobName as CriticalJobName
    if (criticalJobName === 'browser-main' || criticalJobName === 'browser-pion') {
      validateSuiteDeadlineLease(
        jobs[criticalJobName],
        criticalJobName === 'browser-main' ? 'main' : 'pion',
      )
    } else {
      validateDeadlineLease(jobs[criticalJobName], reserveMinutes, criticalJobName)
    }
    validateCheckoutIsolation(jobs[criticalJobName], criticalJobName)
  }

  validateContractOwner(jobs['browser-contract'])
  validateGeneratedSemanticProcessBoundary(jobs[GENERATED_SEMANTIC_PROCESS_JOB])
  validateSuiteProducer(jobs['browser-main'], 'main')
  validateSuiteProducer(jobs['browser-pion'], 'pion')
  validateVerdict(jobs['browser-verdict'])
  validateTokenIsolation(jobs)
  validateWindowsBoundary(jobs[WINDOWS_BROWSER_PROCESS_JOB])
  validateWorkflowTargetOwner(
    workflow,
    GENERATED_SEMANTIC_PROCESS_TARGET,
    GENERATED_SEMANTIC_PROCESS_JOB,
  )
  validateWorkflowTargetOwner(workflow, BROWSER_PROCESS_TARGET, WINDOWS_BROWSER_PROCESS_JOB)
}

function validateTriggers(workflow: WorkflowMapping): void {
  const triggers = requiredMappingField(workflow, 'on', 'workflow')
  const expectedIgnoredPaths = ['docs/**', '**/*.md']
  assert.deepEqual(
    stringListField(requiredMappingField(triggers, 'push', 'workflow.on'), 'paths-ignore', 'workflow.on.push'),
    expectedIgnoredPaths,
    'push must exclude documentation-only changes',
  )
  assert.deepEqual(
    stringListField(
      requiredMappingField(triggers, 'pull_request', 'workflow.on'),
      'paths-ignore',
      'workflow.on.pull_request',
    ),
    expectedIgnoredPaths,
    'pull requests must exclude documentation-only changes',
  )
  assert(!triggers.has('schedule'), 'ordinary CI must not schedule a network matrix')
  assert(!triggers.has('workflow_dispatch'), 'ordinary CI must not expose a network dispatch')
}

function validateExecutionSurface(workflow: WorkflowMapping): void {
  const values = semanticStrings(workflow)
  assert(!containsMappingKey(workflow, 'container'), 'ordinary CI must not add a container authority')
  assert(!values.some((value) => OCI_RUNTIME_PATTERN.test(value)), 'ordinary CI must not add an OCI runtime')
  assert(!values.some((value) => value.includes('browser-network')), 'conditional network evidence has no ordinary CI job')
  assert(!values.some((value) => value.includes('matrix.topology')), 'ordinary CI must not synthesize topology placeholders')
  assert(!values.some((value) => SECRET_EXPRESSION_PATTERN.test(value)), 'browser jobs must not inherit repository secrets')
}

function validateNodeVersionAuthority(workflow: WorkflowMapping): void {
  const jobs = requiredMappingField(workflow, 'jobs', 'workflow')
  const setupSteps: Array<Readonly<{ jobName: string; step: WorkflowMapping }>> = []
  for (const [jobName, value] of jobs) {
    const workflowJob = requireMapping(value, `workflow.jobs.${jobName}`)
    for (const step of stepSections(workflowJob, `workflow.jobs.${jobName}`)) {
      if (optionalStringField(step, 'uses')?.startsWith('actions/setup-node@') === true) {
        setupSteps.push(Object.freeze({ jobName, step }))
      }
    }
  }

  assert.deepEqual(
    setupSteps.map(({ jobName }) => jobName).sort(),
    [...EXPECTED_SETUP_NODE_JOBS].sort(),
    'setup-node ownership must match the exact Node-consuming job set',
  )
  for (const { jobName, step } of setupSteps) {
    const inputs = requiredMappingField(step, 'with', `${jobName} setup-node`)
    assert(!inputs.has('node-version'), `${jobName} setup-node must not use a floating node-version`)
    assert.equal(
      requiredStringField(inputs, 'node-version-file', `${jobName} setup-node inputs`),
      NODE_VERSION_FILE,
      `${jobName} setup-node must use the root ${NODE_VERSION_FILE}`,
    )
  }
}

function validateContractOwner(job: WorkflowMapping): void {
  const invocationPattern = /pnpm -C web run test:browser:evidence:contract/gu
  assert.equal(
    jobRunCommands(job).reduce((count, command) => count + countMatches(command, invocationPattern), 0),
    1,
    'browser evidence contract must have exactly one workflow owner',
  )
  assert.equal(artifactSteps(job).length, 0, 'contract job must not transfer artifacts')
}

function validateGeneratedSemanticProcessBoundary(job: WorkflowMapping): void {
  const label = 'generated-semantic process isolation'
  assert.deepEqual(jobNeeds(job), [], `${label} remains an independent gate`)
  assert.equal(requiredStringField(job, 'runs-on', label), 'ubuntu-latest')
  assert(!job.has('if'), `${label} must not be conditionally suppressed`)
  assert(!containsMappingKey(job, 'continue-on-error'), `${label} must remain blocking`)
  assert.equal(artifactSteps(job).length, 0, `${label} must not transfer browser evidence artifacts`)

  const steps = stepSections(job, label)
  assert.equal(steps.length, 5, `${label} authority must be exactly five steps`)
  const [checkout, pnpmSetup, nodeSetup, install, processTest] = steps
  assert.equal(requiredStringField(checkout as WorkflowMapping, 'uses', `${label} checkout`), CHECKOUT_ACTION)
  assert.equal(requiredStringField(pnpmSetup as WorkflowMapping, 'uses', `${label} pnpm setup`), PNPM_SETUP_ACTION)
  assert.equal(requiredStringField(nodeSetup as WorkflowMapping, 'uses', `${label} Node setup`), NODE_SETUP_ACTION)

  const pnpmInputs = requiredMappingField(pnpmSetup as WorkflowMapping, 'with', `${label} pnpm setup`)
  assert.equal(requiredStringField(pnpmInputs, 'package_json_file', `${label} pnpm setup`), 'web/package.json')
  const nodeInputs = requiredMappingField(nodeSetup as WorkflowMapping, 'with', `${label} Node setup`)
  assert.equal(requiredStringField(nodeInputs, 'node-version-file', `${label} Node setup`), NODE_VERSION_FILE)
  assert.equal(requiredStringField(nodeInputs, 'cache', `${label} Node setup`), 'pnpm')
  assert.equal(
    requiredStringField(nodeInputs, 'cache-dependency-path', `${label} Node setup`),
    'web/pnpm-lock.yaml',
  )
  assert.equal(
    requiredStringField(install as WorkflowMapping, 'run', `${label} install`),
    'pnpm -C web install --frozen-lockfile --ignore-scripts',
  )
  assert.equal(
    requiredStringField(processTest as WorkflowMapping, 'run', `${label} process test`),
    `pnpm -C web run ${GENERATED_SEMANTIC_PROCESS_TARGET}`,
    `${label} must invoke its dedicated package target exactly once`,
  )
}

function validateSuiteProducer(job: WorkflowMapping, suite: BrowserSuite): void {
  const producer = stepById(job, 'producer')
  const guard = stepById(job, 'guard')
  const upload = onlyStep(artifactSteps(job, 'upload'), `${suite} sealed upload`)
  const displayName = suite === 'main' ? 'main' : 'Pion'
  const dispose = stepByName(job, `dispose authenticated ${displayName} native runtime`)

  assert.match(requiredStringField(producer, 'run', `${suite} producer`), /node scripts\/ci\/browsergate\/main\.mjs hosted-produce\b/u)
  assert.match(requiredStringField(producer, 'run', `${suite} producer`), new RegExp(`--suite ${suite}\\b`, 'u'))
  assert.equal(requiredStringField(guard, 'if', `${suite} guard`), 'always()')
  assert.match(requiredStringField(guard, 'run', `${suite} guard`), /node scripts\/ci\/browsergate\/main\.mjs guard-suite\b/u)
  assert.match(requiredStringField(guard, 'run', `${suite} guard`), new RegExp(`--suite ${suite}\\b`, 'u'))
  assert.equal(
    requiredStringField(upload, 'if', `${suite} sealed upload`),
    "always() && steps.guard.outputs.guard_outcome == 'passed'",
  )
  assert.match(requiredStringField(dispose, 'if', `${suite} runtime disposal`), /^always\(\) &&/u)
  assert.match(requiredStringField(dispose, 'run', `${suite} runtime disposal`), /node scripts\/ci\/browsergate\/main\.mjs dispose-runtime\b/u)
  assert(
    !containsMappingKey(job, 'continue-on-error'),
    `${suite} producer must remain blocking across every lifecycle step`,
  )

  validateSuiteLifecycleOrder(job)
  validateSuiteArtifactBoundary(job, suite)
  assert.equal(artifactSteps(job, 'download').length, 0, `${suite} producer must not download artifacts`)
}

function validateSuiteLifecycleOrder(job: WorkflowMapping): void {
  const steps = stepSections(job, 'suite job')
  const producer = stepById(job, 'producer')
  const guard = stepById(job, 'guard')
  const upload = onlyStep(artifactSteps(job, 'upload'), 'suite sealed upload')
  const dispose = steps.find((step) => optionalStringField(step, 'run')?.includes('main.mjs dispose-runtime') === true)
  assert.notEqual(dispose, undefined, 'suite runtime disposal step is missing')
  assert(
    steps.indexOf(producer) < steps.indexOf(guard)
      && steps.indexOf(guard) < steps.indexOf(upload)
      && steps.indexOf(upload) < steps.indexOf(dispose as WorkflowMapping),
    'producer, guard, sealed upload, and disposal must retain their failure-safe order',
  )
}

function validateSuiteArtifactBoundary(job: WorkflowMapping, suite: BrowserSuite): void {
  const uploads = artifactSteps(job, 'upload')
  assert.equal(uploads.length, 1, `${suite} must publish exactly one sealed directory`)
  const upload = onlyStep(uploads, `${suite} sealed upload`)
  const inputs = requiredMappingField(upload, 'with', `${suite} sealed upload`)
  assert.equal(requiredStringField(inputs, 'name', `${suite} sealed upload inputs`), `browser-${suite}-guarded`)
  assert.equal(requiredStringField(inputs, 'if-no-files-found', `${suite} sealed upload inputs`), 'error')
  assert.equal(requiredBooleanField(inputs, 'include-hidden-files', `${suite} sealed upload inputs`), true)
  const path = requiredStringField(inputs, 'path', `${suite} sealed upload inputs`)
  assert.equal(path, githubExpression('steps.guard.outputs.sealed_upload_path'))
  assert(!containsGlob(path), `${suite} sealed upload path cannot contain a glob`)
}

function validateVerdict(job: WorkflowMapping): void {
  assert.equal(requiredStringField(job, 'if', 'browser-verdict'), 'always()')
  assert.equal(
    artifactSteps(job, 'upload').length,
    0,
    'verdict must not add an unguarded publication authority after reduction',
  )
  const steps = stepSections(job, 'browser-verdict')
  assert.equal(
    steps.length,
    4,
    'verdict authority is exactly checkout, two sealed downloads, and the semantic reducer',
  )
  const checkout = steps.find((step) => optionalStringField(step, 'uses')?.startsWith('actions/checkout@') === true)
  assert.notEqual(checkout, undefined, 'verdict checkout is missing')
  const downloads = artifactSteps(job, 'download')
  assert.equal(downloads.length, 2, 'verdict must consume exactly two sealed suite artifacts')
  const expected = Object.freeze([
    Object.freeze({ name: 'browser-main-guarded', path: 'browser-verdict-inputs/main' }),
    Object.freeze({ name: 'browser-pion-guarded', path: 'browser-verdict-inputs/pion' }),
  ])
  const actual = downloads.map((step, index) => {
    assert.equal(requiredStringField(step, 'if', `verdict download ${index + 1}`), 'always()')
    assert.equal(requiredBooleanField(step, 'continue-on-error', `verdict download ${index + 1}`), true)
    const inputs = requiredMappingField(step, 'with', `verdict download ${index + 1}`)
    assert(!inputs.has('merge-multiple'), 'verdict downloads must retain separate namespaces')
    assert(!inputs.has('pattern'), 'verdict downloads must name exact artifacts')
    const name = requiredStringField(inputs, 'name', `verdict download ${index + 1} inputs`)
    const path = requiredStringField(inputs, 'path', `verdict download ${index + 1} inputs`)
    assert(!containsGlob(path), 'verdict download path cannot contain a glob')
    return Object.freeze({ name, path })
  })
  assert.deepEqual(actual, expected)
  assert(!pathsOverlap(actual[0]?.path ?? '', actual[1]?.path ?? ''), 'verdict inputs must be disjoint')

  const reducer = stepById(job, 'verdict')
  assert.equal(requiredStringField(reducer, 'if', 'browser-verdict reducer'), 'always()')
  assert.match(requiredStringField(reducer, 'run', 'browser-verdict reducer'), /node scripts\/ci\/browsergate\/verdict\.mjs\b/u)
  assert(!reducer.has('continue-on-error'), 'reducer exit status must remain the job conclusion')
  assert.equal(steps.at(-1), reducer, 'semantic reducer must remain the final job authority')
  assert(
    steps.indexOf(downloads[0] as WorkflowMapping) < steps.indexOf(downloads[1] as WorkflowMapping)
      && steps.indexOf(downloads[1] as WorkflowMapping) < steps.indexOf(reducer),
    'verdict must download both sealed inputs before reducing',
  )
  assert.deepEqual(
    Object.freeze({
      job: deadlineSummary(job, 'browser-verdict').jobMinutes,
      checkout: stepTimeoutMinutes(checkout as WorkflowMapping, 'browser-verdict checkout'),
      download: stepTimeoutMinutes(downloads[0] as WorkflowMapping, 'browser-verdict main download'),
      reducer: stepTimeoutMinutes(reducer, 'browser-verdict reducer'),
      reserve: CRITICAL_JOB_RESERVE_MINUTES['browser-verdict'],
    }),
    BROWSER_VERDICT_BUDGET_MINUTES,
    'verdict budget must retain the 15-minute hard ceiling and its 2+2+2+3 serial plan',
  )
  assert.equal(
    stepTimeoutMinutes(downloads[1] as WorkflowMapping, 'browser-verdict Pion download'),
    BROWSER_VERDICT_BUDGET_MINUTES.download,
  )
}

function validateTokenIsolation(jobs: CriticalJobs): void {
  const criticalSteps = Object.entries(jobs).flatMap(([jobName, job]) =>
    stepSections(job, jobName).map((step) => ({ jobName, step })))
  const tokenSteps = criticalSteps.filter(({ step }) => hasCredentialAuthority(step))
  assert.equal(tokenSteps.length, 2, 'only the two suite guards may receive credentials')
  for (const { jobName, step } of tokenSteps) {
    assert(['browser-main', 'browser-pion'].includes(jobName))
    assert.equal(step, stepById(jobs[jobName as 'browser-main' | 'browser-pion'], 'guard'))
    const environment = requiredMappingField(step, 'env', `${jobName} guard`)
    assert.equal(
      requiredStringField(environment, 'GITHUB_TOKEN', `${jobName} guard environment`),
      githubExpression('github.token'),
    )
    assert.match(requiredStringField(step, 'run', `${jobName} guard`), /--secret-env GITHUB_TOKEN/u)
    assert(
      !semanticStrings(step).some((value) => SECRET_EXPRESSION_PATTERN.test(value)),
      'suite guards must use only the explicit workflow token',
    )
  }

  for (const [jobName, job] of Object.entries(jobs)) {
    const permittedGuard = jobName === 'browser-main' || jobName === 'browser-pion'
      ? stepById(job, 'guard')
      : undefined
    const jobAuthority = new Map(job)
    jobAuthority.delete('steps')
    assert(!hasCredentialAuthority(jobAuthority), `${jobName} grants credentials outside its isolated suite guard`)
    for (const step of stepSections(job, jobName)) {
      if (step === permittedGuard) continue
      assert(!hasCredentialAuthority(step), `${jobName} grants credentials outside its isolated suite guard`)
    }
  }
}

function validateCheckoutIsolation(job: WorkflowMapping, label: string): void {
  const checkouts = stepSections(job, label)
    .filter((step) => optionalStringField(step, 'uses')?.startsWith('actions/checkout@') === true)
  assert.equal(checkouts.length, 1, `${label} must have one checkout`)
  const inputs = requiredMappingField(checkouts[0] as WorkflowMapping, 'with', `${label} checkout`)
  assert.equal(requiredBooleanField(inputs, 'persist-credentials', `${label} checkout inputs`), false)
}

function validateWindowsBoundary(job: WorkflowMapping): void {
  assert.deepEqual(jobNeeds(job), [], 'Windows process ownership remains an independent gate')
  assert.equal(requiredStringField(job, 'runs-on', 'Windows process ownership'), 'windows-latest')
  assert(!job.has('if'), 'Windows process ownership must not be conditionally suppressed')
  assert(!containsMappingKey(job, 'continue-on-error'), 'Windows process ownership must remain blocking')
  validateCheckoutIsolation(job, 'Windows process ownership')
  assert.equal(
    targetInvocationCount(job, BROWSER_PROCESS_TARGET),
    1,
    'Windows process ownership must invoke the composite browser process target exactly once',
  )
  assert.equal(
    targetInvocationCount(job, GENERATED_SEMANTIC_PROCESS_TARGET),
    0,
    'Windows process ownership receives generated-semantic coverage through the composite target',
  )
  const commands = jobRunCommands(job)
  assert(commands.some((command) => /go test \.\/web\/scripts\/browser-evidence\/windowsjob/u.test(command)))
}

function validateWorkflowTargetOwner(
  workflow: WorkflowMapping,
  target: string,
  expectedJobName: string,
): void {
  const jobs = requiredMappingField(workflow, 'jobs', 'workflow')
  const owners = [...jobs.entries()].flatMap(([jobName, value]) => {
    const count = targetInvocationCount(requireMapping(value, `workflow.jobs.${jobName}`), target)
    return count === 0 ? [] : [Object.freeze({ jobName, count })]
  })
  assert.deepEqual(
    owners,
    [Object.freeze({ jobName: expectedJobName, count: 1 })],
    `${target} must have exactly one workflow owner: ${expectedJobName}`,
  )
}

function validateDeadlineLease(job: WorkflowMapping, reserveMinutes: number, label: string): void {
  const summary = deadlineSummary(job, label)
  assert(
    summary.jobMinutes > summary.serialStepMinutes + reserveMinutes,
    `${label} job timeout must exceed all serial step ceilings plus its ${reserveMinutes}-minute reserve`,
  )
}

function validateSuiteDeadlineLease(job: WorkflowMapping, suite: BrowserSuite): void {
  const policy = createGithubSuiteJobDeadlinePolicy(suite, browserRunPolicy('blocking'), 'linux')
  const summary = deadlineSummary(job, `browser-${suite}`)
  assert.equal(
    summary.jobMinutes,
    policy.minimumJobTimeoutMinutes,
    `${suite} job timeout must equal the ceiling of its versioned lease graph`,
  )
  assert.equal(
    summary.serialStepMinutes + policy.jobSettlementReserveMs / 60_000,
    policy.minimumJobTimeoutMinutes,
    `${suite} serial steps and settlement reserve must exactly cover the job authority`,
  )
}

function deadlineSummary(job: WorkflowMapping, label: string): Readonly<{
  jobMinutes: number
  serialStepMinutes: number
}> {
  const jobMinutes = positiveIntegerField(job, 'timeout-minutes', label)
  const timeouts = stepSections(job, label).map((step, index) => {
    assert(
      step.has('uses') || step.has('run'),
      `${label} step ${index + 1} has no executable authority`,
    )
    return stepTimeoutMinutes(step, `${label} step ${index + 1}`)
  })
  return Object.freeze({
    jobMinutes,
    serialStepMinutes: timeouts.reduce((total, value) => total + value, 0),
  })
}

function stepTimeoutMinutes(step: WorkflowMapping, label: string): number {
  return positiveIntegerField(step, 'timeout-minutes', label)
}

function criticalJobs(workflow: WorkflowMapping): CriticalJobs {
  const jobs = requiredMappingField(workflow, 'jobs', 'workflow')
  return {
    'browser-contract': requiredMappingField(jobs, 'browser-contract', 'workflow.jobs'),
    [GENERATED_SEMANTIC_PROCESS_JOB]: requiredMappingField(
      jobs,
      GENERATED_SEMANTIC_PROCESS_JOB,
      'workflow.jobs',
    ),
    'browser-main': requiredMappingField(jobs, 'browser-main', 'workflow.jobs'),
    'browser-pion': requiredMappingField(jobs, 'browser-pion', 'workflow.jobs'),
    'browser-verdict': requiredMappingField(jobs, 'browser-verdict', 'workflow.jobs'),
    [WINDOWS_BROWSER_PROCESS_JOB]: requiredMappingField(
      jobs,
      WINDOWS_BROWSER_PROCESS_JOB,
      'workflow.jobs',
    ),
  }
}

function jobNeeds(job: WorkflowMapping): string[] {
  if (!job.has('needs')) return []
  const value = requiredField(job, 'needs', 'workflow job')
  if (typeof value === 'string') return [value]
  assert(Array.isArray(value), 'workflow job needs must be a string or sequence')
  return value.map((dependency, index) => requireString(dependency, `workflow job needs[${index}]`))
}

function stepSections(job: WorkflowMapping, label: string): WorkflowMapping[] {
  return requiredSequenceField(job, 'steps', label)
    .map((step, index) => requireMapping(step, `${label}.steps[${index}]`))
}

function stepById(job: WorkflowMapping, id: string): WorkflowMapping {
  const step = stepSections(job, 'workflow job')
    .find((candidate) => optionalStringField(candidate, 'id') === id)
  assert.notEqual(step, undefined, `workflow step ${id} is missing`)
  return step as WorkflowMapping
}

function stepByName(job: WorkflowMapping, name: string): WorkflowMapping {
  const step = stepSections(job, 'workflow job')
    .find((candidate) => optionalStringField(candidate, 'name') === name)
  assert.notEqual(step, undefined, `workflow step ${name} is missing`)
  return step as WorkflowMapping
}

function artifactSteps(job: WorkflowMapping, direction?: 'upload' | 'download'): WorkflowMapping[] {
  const prefix = direction === undefined ? undefined : `actions/${direction}-artifact@`
  return stepSections(job, 'workflow job').filter((step) => {
    const uses = optionalStringField(step, 'uses')
    if (uses === undefined) return false
    return prefix === undefined
      ? uses.startsWith('actions/upload-artifact@') || uses.startsWith('actions/download-artifact@')
      : uses.startsWith(prefix)
  })
}

function jobRunCommands(job: WorkflowMapping): string[] {
  return stepSections(job, 'workflow job').flatMap((step) => {
    const run = optionalStringField(step, 'run')
    return run === undefined ? [] : [run]
  })
}

function targetInvocationCount(job: WorkflowMapping, target: string): number {
  const invocationPattern = new RegExp(
    `pnpm\\s+-C\\s+web\\s+run\\s+${escapeRegExp(target)}(?=\\s|$)`,
    'gu',
  )
  return jobRunCommands(job)
    .reduce((count, command) => count + countMatches(command, invocationPattern), 0)
}

function requiredField(mapping: WorkflowMapping, key: string, label: string): WorkflowValue {
  assert(mapping.has(key), `${label}.${key} is missing`)
  return mapping.get(key) as WorkflowValue
}

function requiredMappingField(mapping: WorkflowMapping, key: string, label: string): WorkflowMapping {
  return requireMapping(requiredField(mapping, key, label), `${label}.${key}`)
}

function requiredSequenceField(mapping: WorkflowMapping, key: string, label: string): WorkflowValue[] {
  const value = requiredField(mapping, key, label)
  assert(Array.isArray(value), `${label}.${key} must be a sequence`)
  return value
}

function requiredStringField(mapping: WorkflowMapping, key: string, label: string): string {
  return requireString(requiredField(mapping, key, label), `${label}.${key}`)
}

function optionalStringField(mapping: WorkflowMapping, key: string): string | undefined {
  if (!mapping.has(key)) return undefined
  return requireString(mapping.get(key) as WorkflowValue, key)
}

function requiredBooleanField(mapping: WorkflowMapping, key: string, label: string): boolean {
  const value = requiredField(mapping, key, label)
  assert(typeof value === 'boolean', `${label}.${key} must be a boolean`)
  return value
}

function positiveIntegerField(mapping: WorkflowMapping, key: string, label: string): number {
  const value = requiredField(mapping, key, label)
  assert(typeof value === 'number' && Number.isSafeInteger(value) && value > 0, `${label} must have one positive ${key}`)
  return value
}

function stringListField(mapping: WorkflowMapping, key: string, label: string): string[] {
  return requiredSequenceField(mapping, key, label)
    .map((value, index) => requireString(value, `${label}.${key}[${index}]`))
}

function requireMapping(value: WorkflowValue, label: string): WorkflowMapping {
  assert(value instanceof Map, `${label} must be a mapping`)
  return value
}

function requireString(value: WorkflowValue, label: string): string {
  assert(typeof value === 'string', `${label} must be a string`)
  return value
}

function onlyStep(steps: WorkflowMapping[], label: string): WorkflowMapping {
  assert.equal(steps.length, 1, `${label} must have exactly one step`)
  return steps[0] as WorkflowMapping
}

function githubExpression(value: string): string {
  return `${GITHUB_EXPRESSION_OPEN} ${value} }}`
}

function containsGlob(value: string): boolean {
  return /[*?[\]]/u.test(value)
}

function pathsOverlap(left: string, right: string): boolean {
  return left === right || left.startsWith(`${right}/`) || right.startsWith(`${left}/`)
}

function countMatches(source: string, pattern: RegExp): number {
  return source.match(pattern)?.length ?? 0
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&')
}

function hasCredentialAuthority(value: WorkflowValue | WorkflowMapping): boolean {
  return semanticStrings(value).some((candidate) => CREDENTIAL_AUTHORITY_PATTERN.test(candidate))
}

function semanticStrings(root: WorkflowValue | WorkflowMapping): string[] {
  const strings: string[] = []
  const pending: unknown[] = [root]
  const seen = new WeakSet<object>()
  while (pending.length > 0) {
    const value = pending.pop()
    if (typeof value === 'string') {
      strings.push(value)
    } else if (Array.isArray(value)) {
      if (seen.has(value)) continue
      seen.add(value)
      pending.push(...value)
    } else if (value instanceof Map) {
      if (seen.has(value)) continue
      seen.add(value)
      for (const [key, entry] of value.entries()) pending.push(key, entry)
    }
  }
  return strings
}

function containsMappingKey(root: WorkflowValue | WorkflowMapping, expectedKey: string): boolean {
  const pending: unknown[] = [root]
  const seen = new WeakSet<object>()
  while (pending.length > 0) {
    const value = pending.pop()
    if (Array.isArray(value)) {
      if (seen.has(value)) continue
      seen.add(value)
      pending.push(...value)
    } else if (value instanceof Map) {
      if (seen.has(value)) continue
      seen.add(value)
      for (const [key, entry] of value.entries()) {
        if (typeof key === 'string' && key.toLowerCase() === expectedKey.toLowerCase()) return true
        pending.push(entry)
      }
    }
  }
  return false
}

function firstLine(message: string): string {
  return message.split(/\r?\n/u, 1)[0] ?? message
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
