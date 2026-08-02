import assert from 'node:assert/strict'

import { parseDocument } from 'yaml'

export type WorkflowValue = string | number | boolean | null | WorkflowValue[] | WorkflowMapping
export type WorkflowMapping = Map<string, WorkflowValue>

export type WorkflowSet = Readonly<{
  ci: WorkflowMapping
  currentCommit: WorkflowMapping
  stability: WorkflowMapping
  releaseReadiness: WorkflowMapping
  browserFull: WorkflowMapping
}>

export type WorkflowSources = Readonly<{
  ci: string
  currentCommit: string
  stability: string
  releaseReadiness: string
  browserFull: string
}>

export type LocalContractSources = Readonly<{
  makefile: string
  packageManifest: string
  platformScripts: Readonly<Record<string, string>>
  fullBrowserOperationPlan: readonly string[]
}>

export const WORKFLOW_ALIAS_EXPANSION_LIMIT = 100

export const GENERATED_SEMANTIC_PROCESS_TARGET = 'test:browser:generated-semantic:process'
export const GENERATED_SEMANTIC_PROCESS_ENTRY =
  '../scripts/ci/browsergate/tests/process/generated-semantic.tests.mjs'
export const GENERATED_SEMANTIC_PROCESS_COMMAND = `node ${GENERATED_SEMANTIC_PROCESS_ENTRY}`
export const BROWSER_PROCESS_INTEGRATION_TARGET = 'test:browser:process:integration'
export const BROWSER_PROCESS_INTEGRATION_COMMAND = [
  'vitest run',
  'test/browser-evidence/native-directory-publisher.test.ts',
  'test/browser-evidence/process-runner.test.ts',
  'test/browser-evidence/test-process-owner-client.test.ts',
].join(' ')

export const PLATFORM_ENTRYPOINTS = Object.freeze([
  'browser-contract',
  'browser-generated',
  'browser-local',
  'browser-network',
  'browser-process',
  'browser-stability',
  'check',
  'core-release',
  'coverage',
  'e2e-go',
  'hygiene',
  'integration',
  'lint',
  'race',
  'sloc',
  'vectors',
  'vet',
  'web',
  'web-dependencies',
  'workflow-lint',
])

export const PR_GATES = Object.freeze([
  'vet',
  'core-release',
  'race',
  'vectors',
  'coverage',
  'integration',
  'e2e',
  'web',
  'browser-contract',
  'browser-generated',
  'browser-process',
  'hygiene',
  'lint',
  'sloc',
])

export const FULL_GATES = Object.freeze([
  'vet',
  'core-release',
  'race',
  'vectors',
  'coverage',
  'integration',
  'e2e',
  'web',
  'browser-contract',
  'browser-generated',
  'browser-process',
  'hygiene',
  'lint',
  'sloc',
  'browser',
])

const NODE_VERSION_FILE = '.node-version'
const CHECKOUT_ACTION = 'actions/checkout@v7'
const PNPM_SETUP_ACTION = 'pnpm/action-setup@v6'
const NODE_SETUP_ACTION = 'actions/setup-node@v6'
const UPLOAD_ACTION = 'actions/upload-artifact@v7'
const GITHUB_SHA_EXPRESSION = '${{ github.sha }}'
const ALWAYS_EXPRESSION = 'always()'
const PROTECTED_NETWORK_ENVIRONMENT = 'browser-network-matrix'
const PROTECTED_NETWORK_RUNNER = Object.freeze([
  'self-hosted',
  'linux',
  'x64',
  'windshare-network-matrix',
])
const FULL_BROWSER_COMMAND =
  'node scripts/ci/makeauthority/entry.mjs browser "BROWSER_NETWORK_COMPLETION=${{ github.workspace }}/test-results/browser-network-completion.json"'
const CURRENT_COMMIT_WORKFLOW = './.github/workflows/current-commit.yml'
const FULL_AUTHORITY_EXPRESSION = "${{ inputs.browser-authority == 'full' }}"
const WINDOWS_NETWORK_MAKE_COMMAND =
  '"$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/browser-network.ps1'
const LINUX_NETWORK_MAKE_COMMAND =
  '"$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/browser/network-completion.sh'
const OCI_RUNTIME_PATTERN = /\b(?:docker|podman|oci)\b/iu
const SECRET_EXPRESSION_PATTERN = /\bsecrets\.\w+\b/iu
const NODE_ENTRYPOINT_PATTERN =
  /scripts\/ci\/(?:stability\/|linux\/(?:browser|web|hygiene)|windows\/(?:browser|web|hygiene))/u

const CI_AUTHORITIES = Object.freeze([
  scriptAuthority('repository hygiene', 'bash scripts/ci/linux/hygiene.sh', 'ubuntu-latest'),
  scriptAuthority('workflow and Go lint', 'bash scripts/ci/linux/lint.sh', 'ubuntu-latest'),
  scriptAuthority('golden vectors', 'bash scripts/ci/linux/vectors.sh', 'ubuntu-latest'),
  scriptAuthority(
    'Linux core release',
    'bash scripts/ci/linux/core-release.sh "$CORE_ARTIFACT_VERSION" "$GITHUB_SHA" linux-ext4',
    'ubuntu-latest',
  ),
  scriptAuthority('browser contract', 'bash scripts/ci/linux/browser-contract.sh', 'ubuntu-latest'),
  scriptAuthority('generated browser semantics', 'bash scripts/ci/linux/browser-generated.sh', 'ubuntu-latest'),
  scriptAuthority('Linux vet', 'bash scripts/ci/linux/vet.sh', 'ubuntu-latest'),
  scriptAuthority('Windows vet', './scripts/ci/windows/vet.ps1', 'windows-latest'),
  scriptAuthority('Linux browser process', 'bash scripts/ci/linux/browser-process.sh', 'ubuntu-latest'),
  scriptAuthority('Windows browser process', './scripts/ci/windows/browser-process.ps1', 'windows-latest'),
  scriptAuthority('Linux native integration', 'bash scripts/ci/linux/integration.sh', 'ubuntu-latest'),
  scriptAuthority('Windows native integration', './scripts/ci/windows/integration.ps1', 'windows-latest'),
  scriptAuthority('Linux Go E2E', 'bash scripts/ci/linux/e2e-go.sh', 'ubuntu-latest'),
  scriptAuthority('Windows Go E2E', './scripts/ci/windows/e2e-go.ps1', 'windows-latest'),
  scriptAuthority('Windows Chromium critical-path sample', './scripts/ci/windows/browser/smoke.ps1', 'windows-latest'),
  scriptAuthority('Linux race', 'bash scripts/ci/linux/race.sh', 'ubuntu-latest'),
  scriptAuthority('Windows race', './scripts/ci/windows/race.ps1', 'windows-latest'),
  scriptAuthority('Linux coverage', 'bash scripts/ci/linux/coverage.sh', 'ubuntu-latest'),
  scriptAuthority('web validation', 'bash scripts/ci/linux/web.sh', 'ubuntu-latest'),
  scriptAuthority(
    'Windows nested reparse authority adversary',
    './scripts/ci/makeauthority/windows-network-path.tests.ps1',
    'windows-latest',
  ),
])

const STABILITY_AUTHORITIES = Object.freeze([
  Object.freeze({
    ...scriptAuthority('Linux stability integration', [
      'node scripts/ci/stability/result.mjs run',
      '--output test-results/stability/integration-linux/result.json',
      '--started-output test-results/stability/integration-linux/started.json',
      '--run-id "${{ github.run_id }}"',
      '--run-attempt "${{ github.run_attempt }}"',
      '--commit-sha "${{ github.sha }}"',
      '--workflow-job "${{ github.job }}"',
      '--suite integration',
      '--entrypoint "bash scripts/ci/linux/integration.sh"',
    ].join(' '), 'ubuntu-latest'),
    identity: /--output test-results\/stability\/integration-linux\/result\.json/u,
    operatingSystem: 'linux',
  }),
  Object.freeze({
    ...scriptAuthority('Windows stability integration', [
      'node scripts/ci/stability/result.mjs run',
      '--output test-results/stability/integration-windows/result.json',
      '--started-output test-results/stability/integration-windows/started.json',
      '--run-id "${{ github.run_id }}"',
      '--run-attempt "${{ github.run_attempt }}"',
      '--commit-sha "${{ github.sha }}"',
      '--workflow-job "${{ github.job }}"',
      '--suite integration',
      '--entrypoint "./scripts/ci/windows/integration.ps1"',
    ].join(' '), 'windows-latest'),
    identity: /--output test-results\/stability\/integration-windows\/result\.json/u,
    operatingSystem: 'windows',
  }),
])

const RELEASE_REDUCER_COMMAND = [
  'node scripts/ci/stability/release-reducer.mjs',
  '--repository "$GITHUB_REPOSITORY"',
  '--workflow stability.yml',
  '--required-runs 100',
  '--output test-results/stability/release-verdict.json',
].join(' ')

const RELEASE_HISTORY_AUTHORITY = Object.freeze({
  label: 'scheduled stability history',
  command: RELEASE_REDUCER_COMMAND,
  runner: 'ubuntu-latest',
  identity: /scripts\/ci\/stability\/release-reducer\.mjs/u,
})
const BROWSER_FULL_AUTHORITY = Object.freeze({
  label: 'token-free full browser consumer',
  command: FULL_BROWSER_COMMAND,
  runner: PROTECTED_NETWORK_RUNNER,
  identity: /scripts\/ci\/makeauthority\/entry\.mjs\s+browser\b/u,
})
const BROWSER_NETWORK_PREPARE_AUTHORITY = Object.freeze({
  label: 'token-free browser network producer',
  command: 'node scripts/ci/browsergate/build-protected-network-inputs.mjs',
  runner: PROTECTED_NETWORK_RUNNER,
  identity: /scripts\/ci\/browsergate\/build-protected-network-inputs\.mjs/u,
})
const BROWSER_NETWORK_BROKER_AUTHORITY = Object.freeze({
  label: 'one-use browser network OIDC broker',
  command: [
    'exec "$RUNNER_TOOL_CACHE/node/24.16.0/x64/bin/node" \\',
    '  --experimental-strip-types \\',
    '  "$BROWSER_NETWORK_PREPARED_DIRECTORY/oidc-network-broker.mjs"',
  ].join('\n'),
  runner: PROTECTED_NETWORK_RUNNER,
  identity: /BROWSER_NETWORK_PREPARED_DIRECTORY\/oidc-network-broker\.mjs/u,
})

type CommandAuthority = Readonly<{
  label: string
  command: string
  runner: string | readonly string[]
  identity: RegExp
}>

type CommandOwner = Readonly<{
  jobName: string
  job: WorkflowMapping
  step: WorkflowMapping
}>

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
  if (!(value instanceof Map)) throw new WorkflowYamlError('workflow YAML root must be a mapping')
  return value as WorkflowMapping
}

export function parseWorkflowSet(sources: WorkflowSources): WorkflowSet {
  return Object.freeze({
    ci: parseWorkflowYaml(sources.ci),
    currentCommit: parseWorkflowYaml(sources.currentCommit),
    stability: parseWorkflowYaml(sources.stability),
    releaseReadiness: parseWorkflowYaml(sources.releaseReadiness),
    browserFull: parseWorkflowYaml(sources.browserFull),
  })
}

export function validateRepositoryContracts(
  workflowSources: WorkflowSources,
  localSources: LocalContractSources,
): void {
  validateWorkflowSet(parseWorkflowSet(workflowSources))
  validateMakefileContract(localSources.makefile, localSources.fullBrowserOperationPlan)
  validateLocalEntrypointContract(localSources.packageManifest, localSources.platformScripts)
}

export function validateWorkflowSet(workflows: WorkflowSet): void {
  validateCIWorkflow(workflows.ci)
  validateCurrentCommitWorkflow(workflows.currentCommit)
  const stabilityCapacity = validateStabilityWorkflow(workflows.stability)
  const releaseRequirement = validateReleaseWorkflow(workflows.releaseReadiness)
  assert(
    stabilityCapacity >= releaseRequirement,
    `stability retention can preserve ${stabilityCapacity} runs; release requires ${releaseRequirement}`,
  )
  validateBrowserFullWorkflow(workflows.browserFull)
}

export function validateCIWorkflow(workflow: WorkflowMapping): void {
  validateCITriggers(workflow)
  validateExecutionSurface(workflow)
  const jobs = workflowJobs(workflow)
  validateWorkflowPermissions(workflow, jobs, { contents: 'read' }, 'CI', true)
  assert.equal(jobs.size, 1, 'CI workflow delegates to one reusable current-commit DAG')
  validateReusableCurrentCommitCall(
    firstJob(jobs, 'CI reusable current-commit authority'),
    'pr',
    [],
    // GitHub validates every job in a reusable workflow before evaluating its
    // `if`; advertise the broker's OIDC capability at the call boundary while
    // the called workflow keeps it absent from all PR-executed jobs.
    { contents: 'read', 'id-token': 'write' },
  )
}

export function validateCurrentCommitWorkflow(workflow: WorkflowMapping): void {
  validateCurrentCommitTrigger(workflow)
  const jobs = workflowJobs(workflow)
  validateWorkflowPermissions(workflow, jobs, { contents: 'read' }, 'current commit', true)
  validateMakeEnvironmentAuthority(workflow)

  const modeAuthority = Object.freeze({
    label: 'browser authority selection',
    command: [
      'set -eu',
      'test "$BROWSER_AUTHORITY" = pr || test "$BROWSER_AUTHORITY" = full',
      'if test "$BROWSER_AUTHORITY" = full; then',
      '  test "$AUTHORITY_EVENT" = schedule || test "$AUTHORITY_EVENT" = workflow_dispatch',
      '  test "$AUTHORITY_REF_PROTECTED" = true',
      '  test "$AUTHORITY_REF" = "$AUTHORITY_DEFAULT_REF"',
      'fi',
    ].join('\n'),
    runner: 'ubuntu-latest',
    identity: /test "\$BROWSER_AUTHORITY" = pr \|\| test "\$BROWSER_AUTHORITY" = full/u,
  })
  const modeOwner = findUniqueCommandOwner(jobs, modeAuthority)
  validateBlockingOwner(modeOwner, modeAuthority, [])
  assert.deepEqual(
    [...requiredMappingField(modeOwner.step, 'env', modeAuthority.label).entries()],
    [
      ['BROWSER_AUTHORITY', '${{ inputs.browser-authority }}'],
      ['AUTHORITY_EVENT', '${{ github.event_name }}'],
      ['AUTHORITY_REF', '${{ github.ref }}'],
      ['AUTHORITY_DEFAULT_REF', 'refs/heads/${{ github.event.repository.default_branch }}'],
      ['AUTHORITY_REF_PROTECTED', '${{ github.ref_protected }}'],
    ],
    'browser authority selection must bind full mode to a protected default-branch dispatch',
  )

  const executableJobs = new Map([...jobs].filter(([name]) => name !== modeOwner.jobName))
  validateCheckoutIsolation(executableJobs, 'github-sha')
  validateNodeVersionAuthority(executableJobs)
  validatePnpmSetupAuthority(executableJobs)

  for (const authority of CI_AUTHORITIES) {
    const owner = findUniqueCommandOwner(jobs, authority)
    if (['browser contract', 'generated browser semantics', 'Windows Chromium critical-path sample']
      .includes(authority.label)) {
      validateBlockingOwner(owner, authority, [modeOwner.jobName])
      if (authority.label === 'Windows Chromium critical-path sample') validateBrowserSmokeArtifact(owner)
    } else {
      validateBlockingOwner(owner, authority, [])
    }
  }
  validateSlocAuthority(jobs)
  validatePackageTargetEncapsulation(executableJobs)

  const producer = findUniqueCommandOwner(jobs, BROWSER_NETWORK_PREPARE_AUTHORITY)
  validateConditionalOwner(producer, BROWSER_NETWORK_PREPARE_AUTHORITY, undefined, FULL_AUTHORITY_EXPRESSION)
  const broker = findUniqueCommandOwner(jobs, BROWSER_NETWORK_BROKER_AUTHORITY)
  validateConditionalOwner(broker, BROWSER_NETWORK_BROKER_AUTHORITY, producer.jobName, FULL_AUTHORITY_EXPRESSION)
  const full = findUniqueCommandOwner(jobs, BROWSER_FULL_AUTHORITY)
  validateConditionalOwner(full, BROWSER_FULL_AUTHORITY, broker.jobName, FULL_AUTHORITY_EXPRESSION)
  const expectedFullNeeds = [...jobs.keys()].filter((name) =>
    ![producer.jobName, broker.jobName, full.jobName].includes(name))
  assert.deepEqual(
    [...jobNeeds(producer.job)].sort(),
    expectedFullNeeds.sort(),
    'browser network producer must depend on every PR-equivalent current-commit owner exactly once',
  )
  validateJobPermissions(broker, { contents: 'read', 'id-token': 'write' })
  validateJobPermissions(full, { contents: 'read' })
  validateProtectedNetworkOwner(broker, BROWSER_NETWORK_BROKER_AUTHORITY.label)
  validateFullBrowserTemporalContract(jobs, producer, broker, full)
  validateFullBrowserArtifact(full)
  assert.equal(jobs.size, 23, 'current-commit workflow must have one owner per semantic tuple')
}

export function validateStabilityWorkflow(workflow: WorkflowMapping): number {
  const triggers = requiredMappingField(workflow, 'on', 'stability workflow')
  assert(triggers.has('workflow_dispatch'), 'stability workflow requires manual dispatch')
  const schedules = cronValues(triggers)
  assert.deepEqual(
    schedules,
    ['41 2 * * *', '41 14 * * *'],
    'stability workflow requires two fixed daily schedules',
  )

  const jobs = workflowJobs(workflow)
  validateWorkflowPermissions(workflow, jobs, { contents: 'read' }, 'stability')
  validateCheckoutIsolation(jobs, 'github-sha')
  validateNodeVersionAuthority(jobs)
  validatePnpmSetupAuthority(jobs)
  const retentions: number[] = []
  for (const authority of STABILITY_AUTHORITIES) {
    const owner = findUniqueCommandOwner(jobs, authority)
    validateStabilityOwner(owner, authority)
    retentions.push(stabilityArtifactRetention(owner, authority.operatingSystem))
  }
  assert.equal(jobs.size, STABILITY_AUTHORITIES.length, 'stability workflow has exactly two native owners')
  return schedules.length * Math.min(...retentions)
}

export function validateReleaseWorkflow(workflow: WorkflowMapping): number {
  validateDispatchOnlyWorkflow(workflow, 'release readiness')
  const jobs = workflowJobs(workflow)
  validateWorkflowPermissions(workflow, jobs, { contents: 'read' }, 'release readiness', true)
  const directJobs = directWorkflowJobs(jobs)
  validateCheckoutIsolation(directJobs, 'github-sha')
  validateNodeVersionAuthority(directJobs)
  validatePnpmSetupAuthority(directJobs)
  const history = findUniqueCommandOwner(jobs, RELEASE_HISTORY_AUTHORITY)
  validateJobPermissions(history, { actions: 'read', contents: 'read' })
  validateReleaseHistoryOwner(history)
  validateReleaseHistoryArtifact(history)
  const reusable = [...jobs.entries()].filter(([, value]) =>
    requireMapping(value, 'release job').has('uses'))
  assert.equal(reusable.length, 1, 'release readiness has one reusable current-commit owner')
  validateReusableCurrentCommitCall(
    reusable[0] as [string, WorkflowValue],
    'full',
    [history.jobName],
    { contents: 'read', 'id-token': 'write' },
    '${{ always() }}',
  )
  assert.equal(jobs.size, 2, 'release readiness has one history reducer and one current-commit DAG')
  return requiredPositiveArgument(RELEASE_HISTORY_AUTHORITY.command, '--required-runs')
}

export function validateBrowserFullWorkflow(workflow: WorkflowMapping): void {
  const triggers = requiredMappingField(workflow, 'on', 'full browser workflow')
  assert(triggers.has('workflow_dispatch'), 'full browser workflow requires manual dispatch')
  assert.deepEqual(cronValues(triggers), ['17 3 * * *'], 'full browser workflow requires one daily schedule')
  assert(!triggers.has('push') && !triggers.has('pull_request'), 'full browser workflow is not a PR authority')

  const jobs = workflowJobs(workflow)
  validateWorkflowPermissions(workflow, jobs, { contents: 'read' }, 'full browser', true)
  assert.equal(jobs.size, 1, 'full browser workflow delegates to one current-commit DAG')
  validateReusableCurrentCommitCall(
    firstJob(jobs, 'full browser current-commit authority'),
    'full',
    [],
    { contents: 'read', 'id-token': 'write' },
  )
}

function validateCurrentCommitTrigger(workflow: WorkflowMapping): void {
  const triggers = requiredMappingField(workflow, 'on', 'current-commit workflow')
  assert.deepEqual([...triggers.keys()], ['workflow_call'], 'current-commit workflow is reusable only')
  const call = requiredMappingField(triggers, 'workflow_call', 'current-commit workflow_call')
  const inputs = requiredMappingField(call, 'inputs', 'current-commit workflow_call')
  assert.deepEqual([...inputs.keys()], ['browser-authority'], 'current-commit workflow has one mode input')
  const authority = requiredMappingField(inputs, 'browser-authority', 'browser authority input')
  assert.equal(requiredBooleanField(authority, 'required', 'browser authority input'), true)
  assert.equal(requiredStringField(authority, 'type', 'browser authority input'), 'string')
  assert.deepEqual(
    [...authority.keys()].sort(),
    ['description', 'required', 'type'],
    'browser authority input fields must be exact',
  )
}

function validateMakeEnvironmentAuthority(workflow: WorkflowMapping): void {
  const env = requiredMappingField(workflow, 'env', 'current-commit workflow')
  for (const name of [
    'MAKEFLAGS', 'MFLAGS', 'GNUMAKEFLAGS', 'MAKEFILES',
    'GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT',
  ]) assert(!env.has(name), `${name} must remain absent until its retained launcher settles authority`)
}

function firstJob(jobs: WorkflowMapping, label: string): [string, WorkflowValue] {
  const entries = [...jobs.entries()]
  assert.equal(entries.length, 1, `${label} must be unique`)
  return entries[0] as [string, WorkflowValue]
}

function validateReusableCurrentCommitCall(
  entry: [string, WorkflowValue],
  mode: 'pr' | 'full',
  expectedNeeds: readonly string[],
  permissions: Readonly<Record<string, string>>,
  expectedCondition?: string,
): void {
  const [jobName, value] = entry
  const job = requireMapping(value, `${jobName} reusable job`)
  assert.equal(requiredStringField(job, 'uses', `${jobName} reusable job`), CURRENT_COMMIT_WORKFLOW)
  assert(!job.has('steps') && !job.has('runs-on') && !job.has('continue-on-error'),
    `${jobName} must remain one unmasked reusable-workflow call`)
  if (expectedCondition === undefined) {
    assert(!job.has('if'), `${jobName} must remain an unconditional reusable-workflow call`)
  } else {
    assert(job.has('if'), `${jobName} must execute independently of dependency outcome`)
    assert.equal(requiredStringField(job, 'if', `${jobName} reusable job`), expectedCondition,
      `${jobName} must execute independently of dependency outcome`)
  }
  assert.deepEqual(jobNeeds(job), expectedNeeds, `${jobName} dependencies must be exact`)
  validatePermissionMapping(
    requiredMappingField(job, 'permissions', `${jobName} reusable job`),
    permissions,
    `${jobName} reusable job`,
  )
  const inputs = requiredMappingField(job, 'with', `${jobName} reusable job`)
  assert.deepEqual([...inputs.entries()], [['browser-authority', mode]],
    `${jobName} must select exactly the ${mode} browser authority`)
}

function directWorkflowJobs(jobs: WorkflowMapping): WorkflowMapping {
  return new Map([...jobs].filter(([, value]) => !requireMapping(value, 'workflow job').has('uses')))
}

function validateConditionalOwner(
  owner: CommandOwner,
  authority: CommandAuthority,
  expectedNeed: string | undefined,
  condition: string,
): void {
  validateRunner(owner, authority)
  if (expectedNeed !== undefined) {
    assert.deepEqual(jobNeeds(owner.job), [expectedNeed], `${authority.label} dependencies must be exact`)
  }
  assert.equal(requiredStringField(owner.job, 'if', authority.label), condition,
    `${authority.label} job condition must be exact`)
  assert(!owner.job.has('continue-on-error'), `${authority.label} job must remain blocking`)
  assert(!owner.job.has('strategy'), `${authority.label} job must have one non-matrix owner`)
  assert(!owner.step.has('if'), `${authority.label} command step must not be conditionally suppressed`)
  assert(!owner.step.has('continue-on-error'), `${authority.label} command step must remain blocking`)
}

function validateSlocAuthority(jobs: WorkflowMapping): void {
  const owners = [...jobs.entries()].flatMap(([jobName, value]) => {
    const job = requireMapping(value, jobName)
    if (!job.has('steps')) return []
    return stepMappings(job, jobName)
      .filter((step) => optionalStringField(step, 'uses')?.startsWith('doraemonkeys/sloc-guard/') === true)
      .map((step) => ({ jobName, job, step }))
  })
  assert.equal(owners.length, 1, 'SLOC guard must have one semantic owner')
  const owner = owners[0] as { jobName: string, job: WorkflowMapping, step: WorkflowMapping }
  assert.equal(requiredStringField(owner.step, 'uses', 'SLOC guard'),
    'doraemonkeys/sloc-guard/.github/action@master')
  assert.equal(requiredStringField(owner.job, 'runs-on', 'SLOC guard'), 'ubuntu-latest')
  assert.deepEqual(jobNeeds(owner.job), [])
  assert(!owner.job.has('if') && !owner.job.has('continue-on-error'))
}

export function validateMakefileContract(
  makefile: string,
  fullBrowserOperationPlan: readonly string[],
): void {
  const entrypoints = makeWords(makefile, 'PLATFORM_ENTRYPOINTS')
  assert.deepEqual(entrypoints, PLATFORM_ENTRYPOINTS, 'platform entrypoint authority must be explicit and exact')
  for (const assignment of [
    'override HOST_GOOS := $(WINDSHARE_HOST_GOOS)',
    'override HOST_GOOS := windows',
    'override HOST_GOOS := linux',
  ]) assert.equal(literalCount(makefile, assignment), 1,
    'host GOOS must have one retained branch and two explicit public platform branches')
  const pullRequestGates = makeWords(makefile, 'CI_GATES')
  const fullGates = makeWords(makefile, 'CI_FULL_GATES')
  assert.deepEqual(pullRequestGates, PR_GATES, 'CI_GATES must enumerate the PR authority')
  assert.deepEqual(fullGates, FULL_GATES, 'CI_FULL_GATES must enumerate the full authority')
  assert.deepEqual(makeTargetPrerequisites(makefile, 'ci'), ['authority-context', '$(CI_GATES)'])
  assert.deepEqual(makeTargetPrerequisites(makefile, 'ci-full'), ['authority-context', '$(CI_FULL_GATES)'])
  for (const target of ['plan-ci', 'plan-ci-full', 'plan-browser']) {
    assert.deepEqual(makeTargetPrerequisites(makefile, target), ['authority-context'],
      `${target} must remain a safe public Make endpoint`)
  }
  assert.equal(
    literalCount(makefile, '.PHONY: authority-context ci ci-full e2e browser browser-smoke plan-ci plan-ci-full plan-browser $(PLATFORM_ENTRYPOINTS)'),
    1,
    'every public composite and platform leaf must remain phony',
  )

  assert.deepEqual(
    FULL_GATES,
    [...PR_GATES, 'browser'],
    'full validation must retain the exact PR graph before adding the public browser authority',
  )
  assert.equal(new Set(pullRequestGates).size, pullRequestGates.length, 'CI_GATES must not repeat a gate')
  assert.equal(new Set(fullGates).size, fullGates.length, 'CI_FULL_GATES must not repeat a gate')

  for (const consumer of [
    'check',
    'web',
    'browser-contract',
    'browser-generated',
    'browser-local',
    'browser-process',
    'browser-stability',
    'browser-smoke',
  ]) {
    assert.deepEqual(
      makeTargetPrerequisites(makefile, consumer),
      ['authority-context', 'web-dependencies'],
      `${consumer} must have web-dependencies as its exact leaf`,
    )
  }
  assert.deepEqual(
    makeTargetPrerequisites(makefile, 'browser-network'),
    ['authority-context'],
    'browser-network must consume an already-published completion without installing dependencies',
  )
  assert.deepEqual(makeTargetPrerequisites(makefile, 'web-dependencies'), ['authority-context'])
  assert.deepEqual(
    makeTargetPrerequisites(makefile, 'browser'),
    ['authority-context', 'browser-local', 'browser-network'],
    'public browser authority must compose both full browser leaves exactly once',
  )
  assert.equal(
    makeAssignment(makefile, 'DISPATCH_ENTRYPOINTS'),
    '$(filter-out browser-network core-release web-dependencies,$(PLATFORM_ENTRYPOINTS))',
    'browser-network must not inherit the argument-free generic dispatcher',
  )
  for (const variable of [
    'CORE_ARTIFACT_VERSION', 'PLATFORM_ENTRYPOINTS', 'CI_GATES', 'CI_FULL_GATES',
    'DISPATCH_ENTRYPOINTS', 'DISPATCH',
  ]) {
    assert.equal(literalCount(makefile, `$(origin ${variable})`), 1,
      `${variable} must reject every external origin`)
  }
  for (const literal of [
    'ifneq ($(origin SHELL),default)',
    'ifneq ($(origin .SHELLFLAGS),default)',
    'ifneq ($(strip $(MFLAGS)),)',
    'ifneq ($(strip $(GNUMAKEFLAGS)),)',
    'ifneq ($(strip $(MAKEFILES)),)',
    'ifneq ($(strip $(GOFLAGS)),)',
    'ifneq ($(strip $(GOWORK)),)',
    'ifneq ($(strip $(GOOS)),)',
    'ifneq ($(strip $(GOARCH)),)',
    'ifneq ($(strip $(GOENV)),)',
    'ifneq ($(strip $(GOTOOLCHAIN)),)',
    'ifneq ($(strip $(GOROOT)),)',
    'override SHELL := $(WINDSHARE_RECIPE_SHELL)',
    'override .SHELLFLAGS := -eu -c',
    'ifneq ($(origin WINDSHARE_CORE_ARTIFACT_COMMIT_SHA),command line)',
    '$(words $(MAKEFILE_LIST))',
  ]) {
    assert.equal(literalCount(makefile, literal), 1, `Make authority guard requires ${literal}`)
  }
  assert.equal(literalCount(makefile, `\t${WINDOWS_NETWORK_MAKE_COMMAND}`), 1)
  assert.equal(literalCount(makefile, `\t${LINUX_NETWORK_MAKE_COMMAND}`), 1)
  assert.equal(literalCount(
    makefile,
    'filter-out BROWSER_NETWORK_COMPLETION WINDSHARE_HOST_GOOS WINDSHARE_CORE_ARTIFACT_COMMIT_SHA WINDSHARE_RETAINED_MAKEFILE WINDSHARE_RECIPE_SHELL WINDSHARE_BASH_EXECUTABLE WINDSHARE_PWSH_EXECUTABLE',
  ), 1, 'the command-line allowlist must expose only the completion and retained launcher authorities')
  for (const retainedGuard of [
    'override VALIDATION_RETAINED_MODE := $(if $(filter command line,$(origin WINDSHARE_RETAINED_MAKEFILE)),1,0)',
    'ifneq ($(origin WINDSHARE_HOST_GOOS),command line)',
    'ifneq ($(origin WINDSHARE_RETAINED_MAKEFILE),command line)',
    'ifneq ($(origin WINDSHARE_RECIPE_SHELL),command line)',
    '$(error public validation accepts target names, not command-line variable assignments)',
    '$(error $(variable) is reserved for the retained Make launcher)',
    'public validation requires the repository Makefile identity',
  ]) assert(makefile.includes(retainedGuard), `Make dual-mode authority requires ${retainedGuard}`)
  assert(!containsMakeAssignment(makefile, ['BROWSER_NETWORK_COMPLETION']),
    'the completion must remain an explicit launcher operand, not a Makefile default')
  assert.deepEqual(
    makeTargetDeclarations(makefile, 'e2e'),
    [['authority-context', 'e2e-go', 'browser-smoke'], ['authority-context', 'e2e-go']],
    'public e2e must compose Windows smoke without a Linux no-op leaf',
  )

  assert.equal(countValue(fullBrowserOperationPlan, 'browser-contract'), 1)
  assert.equal(countValue(fullBrowserOperationPlan, 'generated-semantic-process'), 1)
  assert.equal(countValue(fullBrowserOperationPlan, 'dependency-install'), 0)
  assert.equal(countValue(fullBrowserOperationPlan, 'dependency-install-reuse'), 0)
  assertBrowserInstrumentationOnce(pullRequestGates, fullGates, fullBrowserOperationPlan)
}

export function validateLocalEntrypointContract(
  packageManifest: string,
  platformScripts: Readonly<Record<string, string>>,
): void {
  const scripts = packageScripts(packageManifest)
  assert.equal(scripts[GENERATED_SEMANTIC_PROCESS_TARGET], GENERATED_SEMANTIC_PROCESS_COMMAND)
  assert.equal(scripts[BROWSER_PROCESS_INTEGRATION_TARGET], BROWSER_PROCESS_INTEGRATION_COMMAND)
  assert(!Object.hasOwn(scripts, 'test:browser:process'), 'ambiguous browser process aggregate is forbidden')
  assert.equal(
    scripts['test:browser'],
    'node ../scripts/ci/browsergate/main.mjs local',
    'full browser package command must not bypass Make dependency authority',
  )

  for (const platform of ['windows', 'linux']) {
    const processSource = requiredScript(platformScripts, `${platform}/browser-process`)
    assert.equal(packageTargetCount(processSource, BROWSER_PROCESS_INTEGRATION_TARGET), 1)
    assert.equal(packageTargetCount(processSource, GENERATED_SEMANTIC_PROCESS_TARGET), 0)

    const generatedSource = requiredScript(platformScripts, `${platform}/browser-generated`)
    assert.equal(packageTargetCount(generatedSource, GENERATED_SEMANTIC_PROCESS_TARGET), 1)
    assert.equal(packageTargetCount(generatedSource, BROWSER_PROCESS_INTEGRATION_TARGET), 0)

    const dependencySource = requiredScript(platformScripts, `${platform}/web-dependencies`)
    assert.equal(literalCount(dependencySource, 'pnpm -C web install --frozen-lockfile'), 1)
    assert.equal(literalCount(requiredScript(platformScripts, `${platform}/web`), 'pnpm -C web install'), 0)

    const browserSource = requiredScript(platformScripts, `${platform}/browser-local`)
    assert.equal(literalCount(browserSource, 'skip-dependency-install'), 0)
    assert.equal(literalCount(browserSource, 'pnpm -C web install'), 0)

    const networkSource = requiredScript(platformScripts, `${platform}/browser-network`)
    const networkCommands = compactCommandSource(networkSource)
    assert.equal(literalCount(networkCommands, 'scripts/ci/browsergate/network-completion.mjs consume'), 1,
      `${platform} browser-network must invoke the completion consumer exactly once`)
    for (const forbidden of [
      'network-entry.mjs',
      'build:browser-network-matrix-helpers',
      'scheduled-hard.manifest.v2.json',
      '--runtime-config',
      'ACTIONS_ID_TOKEN_REQUEST_URL',
      'ACTIONS_ID_TOKEN_REQUEST_TOKEN',
      'WINDSHARE_OIDC_AUDIENCE',
      'BROWSER_NETWORK_RUNTIME_CONFIG',
    ]) assert.equal(literalCount(networkSource, forbidden), 0,
      `${platform} browser-network consumer must not reacquire ${forbidden}`)
    assert.equal(literalCount(networkSource, 'ValueFromRemainingArguments'), platform === 'windows' ? 1 : 0,
      `${platform} browser-network must reject, rather than forward, unexpected operands`)
    assert.equal(literalCount(networkSource, '"$@"'), 0)
    assert.equal(literalCount(networkSource, 'not-executed'), 0)
    assert.equal(literalCount(networkSource, '--skip'), 0)
    if (platform === 'windows') {
      assert.match(networkCommands, /UnexpectedArguments\.Count -ne 0/u)
      assert.match(networkSource, /IsNullOrWhiteSpace\(\$env:BROWSER_NETWORK_COMPLETION\)/u)
      assert.match(networkSource, /test-results\/browser-network-completion\.json/u)
    } else {
      assert.match(networkCommands, /\(\( \$# != 0 \)\)/u)
      assert.match(networkSource, /-z "\$\{BROWSER_NETWORK_COMPLETION:-\}"/u)
      assert.match(networkSource, /test-results\/browser-network-completion\.json/u)
    }

    const integrationSource = requiredScript(platformScripts, `${platform}/integration`)
    const integrationCommand = platform === 'windows'
      ? 'Invoke-WindShareGoTestJSON -count=1 ./integration/...'
      : 'windshare_go_test_json -count=1 ./integration/...'
    assert.equal(literalCount(integrationSource, integrationCommand), 1,
      `${platform} integration must own the exact historical command once`)
    assert(!containsRetryLoop(integrationSource),
      `${platform} integration must not retry internally`)
  }
  const windowsSmoke = requiredScript(platformScripts, 'windows/browser-smoke')
  assert.equal(literalCount(windowsSmoke, 'pnpm -C web exec playwright install chromium'), 1)
  assert.equal(packageTargetCount(windowsSmoke, 'test:browser:smoke'), 1)
  for (const platform of ['windows', 'linux']) {
    const e2eGo = requiredScript(platformScripts, `${platform}/e2e-go`)
    const e2eCommand = platform === 'windows'
      ? 'Invoke-WindShareGoTestJSON -count=1 ./e2e'
      : 'windshare_go_test_json -count=1 ./e2e'
    assert.equal(literalCount(e2eGo, e2eCommand), 1,
      `${platform} E2E must preserve one visible JSON test invocation`)
    assert.equal(packageTargetCount(e2eGo, 'test:browser:smoke'), 0)
  }
  const prepare = requiredScript(platformScripts, 'linux/browser-network-prepare')
  assert.equal(literalCount(prepare, 'build:browser-network-matrix-helpers'), 1,
    'the token-free producer must build the native helpers exactly once')
  assert.match(compactCommandSource(prepare), /\(\( \$# != 0 \)\)/u)
  for (const forbidden of [
    'network-entry.mjs',
    'network-completion.mjs',
    'ACTIONS_ID_TOKEN_REQUEST_URL',
    'ACTIONS_ID_TOKEN_REQUEST_TOKEN',
    'WINDSHARE_OIDC_AUDIENCE',
    'BROWSER_NETWORK_RUNTIME_CONFIG',
  ]) assert.equal(literalCount(prepare, forbidden), 0,
    `browser network producer must not acquire ${forbidden}`)
}

function containsRetryLoop(source: string): boolean {
  const loopPrefixes = ['for ', 'for(', 'foreach ', 'foreach(', 'while ', 'while(', 'until ', 'do {']
  return source.split(/\r?\n/u).some((line) => {
    const statement = line.trimStart().toLowerCase()
    return loopPrefixes.some((prefix) => statement.startsWith(prefix))
  })
}

function validateCITriggers(workflow: WorkflowMapping): void {
  const triggers = requiredMappingField(workflow, 'on', 'CI workflow')
  const expectedIgnoredPaths = ['docs/**', '**/*.md']
  assert.deepEqual(
    stringListField(requiredMappingField(triggers, 'push', 'CI workflow.on'), 'paths-ignore', 'CI push'),
    expectedIgnoredPaths,
    'push must exclude documentation-only changes',
  )
  assert.deepEqual(
    stringListField(
      requiredMappingField(triggers, 'pull_request', 'CI workflow.on'),
      'paths-ignore',
      'CI pull request',
    ),
    expectedIgnoredPaths,
    'pull requests must exclude documentation-only changes',
  )
  assert(!triggers.has('schedule'), 'ordinary CI must not schedule a network matrix')
  assert(!triggers.has('workflow_dispatch'), 'ordinary CI must not expose a network dispatch')
}

function validateDispatchOnlyWorkflow(workflow: WorkflowMapping, label: string): void {
  const triggers = requiredMappingField(workflow, 'on', `${label} workflow`)
  assert.deepEqual([...triggers.keys()], ['workflow_dispatch'], `${label} must be dispatch-only`)
}

function validateExecutionSurface(workflow: WorkflowMapping): void {
  const values = semanticStrings(workflow)
  assert(!containsMappingKey(workflow, 'container'), 'ordinary CI must not add a container authority')
  assert(!values.some((value) => OCI_RUNTIME_PATTERN.test(value)), 'ordinary CI must not add an OCI runtime')
  assert(!values.some((value) => value.includes('browser-network')), 'ordinary CI has no network-matrix authority')
  assert(!values.some((value) => value.includes('matrix.topology')), 'ordinary CI must not synthesize topology placeholders')
  assert(!values.some((value) => SECRET_EXPRESSION_PATTERN.test(value)), 'ordinary CI must not inherit repository secrets')
  assert(!values.some((value) => /hosted-produce|guard-suite|browsergate\/verdict\.mjs/u.test(value)),
    'ordinary CI must not embed the full browser artifact DAG')
  assert(!jobRunCommands(workflowJobs(workflow)).some((command) =>
    /(?:^|\s)make\s+ci-full(?=\s|$)/u.test(command)), 'ordinary CI must not invoke full validation')
}

function validateCheckoutIsolation(
  jobs: WorkflowMapping,
  refMode: 'default' | 'github-sha',
): void {
  for (const [jobName, value] of jobs) {
    const job = requireMapping(value, `workflow.jobs.${jobName}`)
    const checkouts = stepMappings(job, `workflow.jobs.${jobName}`)
      .filter((step) => optionalStringField(step, 'uses')?.startsWith('actions/checkout@') === true)
    assert.equal(checkouts.length, 1, `${jobName} must have exactly one checkout`)
    const checkout = checkouts[0] as WorkflowMapping
    assert.equal(requiredStringField(checkout, 'uses', `${jobName} checkout`), CHECKOUT_ACTION)
    const inputs = requiredMappingField(checkout, 'with', `${jobName} checkout`)
    assert.equal(requiredBooleanField(inputs, 'persist-credentials', `${jobName} checkout`), false)
    assert(!inputs.has('repository') && !inputs.has('path'), `${jobName} checkout must remain repository-root local`)
    if (refMode === 'github-sha') {
      assert.equal(requiredStringField(inputs, 'ref', `${jobName} checkout`), GITHUB_SHA_EXPRESSION)
    } else {
      assert(!inputs.has('ref'), `${jobName} checkout must use the event commit`)
    }
  }
}

function validateWorkflowPermissions(
  workflow: WorkflowMapping,
  jobs: WorkflowMapping,
  expected: Readonly<Record<string, string>>,
  label: string,
  allowJobOverrides = false,
): void {
  validatePermissionMapping(requiredMappingField(workflow, 'permissions', `${label} workflow`), expected, label)
  if (allowJobOverrides) return
  for (const [jobName, value] of jobs) {
    const job = requireMapping(value, `${label}.jobs.${jobName}`)
    assert(!job.has('permissions'), `${label} job ${jobName} must not override workflow permissions`)
  }
}

function validateJobPermissions(
  owner: CommandOwner,
  expected: Readonly<Record<string, string>>,
): void {
  validatePermissionMapping(
    requiredMappingField(owner.job, 'permissions', `${owner.jobName} job`),
    expected,
    `${owner.jobName} job`,
  )
}

function validatePermissionMapping(
  permissions: WorkflowMapping,
  expected: Readonly<Record<string, string>>,
  label: string,
): void {
  assert.deepEqual(
    [...permissions.entries()].sort(([left], [right]) => left.localeCompare(right)),
    Object.entries(expected).sort(([left], [right]) => left.localeCompare(right)),
    `${label} permissions must be exact`,
  )
}

function validateNodeVersionAuthority(jobs: WorkflowMapping): void {
  for (const [jobName, value] of jobs) {
    const job = requireMapping(value, `workflow.jobs.${jobName}`)
    const setupSteps = stepMappings(job, `workflow.jobs.${jobName}`)
      .filter((step) => optionalStringField(step, 'uses')?.startsWith('actions/setup-node@') === true)
    const expectedCount = jobRequiresNode(job) ? 1 : 0
    assert.equal(setupSteps.length, expectedCount, `${jobName} setup-node must follow Node consumption`)
    for (const step of setupSteps) {
      assert.equal(requiredStringField(step, 'uses', `${jobName} setup-node`), NODE_SETUP_ACTION)
      const inputs = requiredMappingField(step, 'with', `${jobName} setup-node`)
      if (jobName === 'full-browser-network') {
        assert.deepEqual([...inputs.entries()], [['node-version', '24.16.0']],
          'OIDC broker Node must use the exact manifest-bound toolcache version')
      } else {
        assert(!inputs.has('node-version'), `${jobName} setup-node must not float its version`)
        assert.equal(requiredStringField(inputs, 'node-version-file', `${jobName} setup-node`), NODE_VERSION_FILE)
      }
    }
  }
}

/**
 * pnpm's action is a tool bootstrap, not a dependency-installation authority.
 * Keeping one exact setup step beside each web-consuming job makes a clean
 * checkout reproducible while leaving `web-dependencies` as the only owner of
 * the lockfile install itself.
 */
function validatePnpmSetupAuthority(jobs: WorkflowMapping): void {
  for (const [jobName, value] of jobs) {
    const job = requireMapping(value, `workflow.jobs.${jobName}`)
    const setupSteps = stepMappings(job, `workflow.jobs.${jobName}`)
      .filter((step) => optionalStringField(step, 'uses')?.startsWith('pnpm/action-setup@') === true)
    const webConsumer = jobRunCommands(new Map([[jobName, job]])).some(commandConsumesWeb)
    assert.equal(
      setupSteps.length,
      webConsumer ? 1 : 0,
      `${jobName} pnpm setup must follow web consumption exactly once`,
    )
    for (const step of setupSteps) {
      assert.equal(requiredStringField(step, 'uses', `${jobName} pnpm setup`), PNPM_SETUP_ACTION)
      const inputs = requiredMappingField(step, 'with', `${jobName} pnpm setup`)
      assert.equal(
        requiredStringField(inputs, 'package_json_file', `${jobName} pnpm setup`),
        'web/package.json',
      )
    }
  }
}

function jobRequiresNode(job: WorkflowMapping): boolean {
  const steps = stepMappings(job, 'workflow job')
  if (steps.some((step) => optionalStringField(step, 'uses')?.startsWith('pnpm/action-setup@') === true)) {
    return true
  }
  return steps.some((step) => {
    const command = optionalStringField(step, 'run')
    const shell = optionalStringField(step, 'shell')
    return command !== undefined && (/\b(?:node|pnpm)\b/u.test(command) || NODE_ENTRYPOINT_PATTERN.test(command)) ||
      shell !== undefined && /(?:^|\/)node(?:\s|\/)/u.test(shell)
  })
}

function findUniqueCommandOwner(jobs: WorkflowMapping, authority: CommandAuthority): CommandOwner {
  const owners: CommandOwner[] = []
  for (const [jobName, value] of jobs) {
    const job = requireMapping(value, `workflow.jobs.${jobName}`)
    if (!job.has('steps')) continue
    for (const step of stepMappings(job, `workflow.jobs.${jobName}`)) {
      const command = optionalStringField(step, 'run')
      if (command !== undefined && authority.identity.test(command)) {
        owners.push(Object.freeze({ jobName, job, step }))
      }
    }
  }
  assert.equal(owners.length, 1, `${authority.label} must have exactly one semantic command owner`)
  const owner = owners[0] as CommandOwner
  assert.equal(
    requiredStringField(owner.step, 'run', authority.label),
    authority.command,
    `${authority.label} command must be exact and unmasked`,
  )
  return owner
}

function validateBlockingOwner(
  owner: CommandOwner,
  authority: CommandAuthority,
  expectedNeeds: readonly string[],
): void {
  validateRunner(owner, authority)
  assert.deepEqual(jobNeeds(owner.job), expectedNeeds, `${authority.label} dependencies must be exact`)
  assert(!owner.job.has('if'), `${authority.label} job must not be conditionally suppressed`)
  assert(!owner.job.has('continue-on-error'), `${authority.label} job must remain blocking`)
  assert(!owner.job.has('strategy'), `${authority.label} job must have one non-matrix owner`)
  assert(!owner.step.has('if'), `${authority.label} command step must not be conditionally suppressed`)
  assert(!owner.step.has('continue-on-error'), `${authority.label} command step must remain blocking`)
}

function validateRunner(owner: CommandOwner, authority: CommandAuthority): void {
  const runner = requiredField(owner.job, 'runs-on', authority.label)
  if (typeof authority.runner === 'string') {
    assert.equal(requireString(runner, `${authority.label}.runs-on`), authority.runner)
    return
  }
  assert(Array.isArray(runner), `${authority.label}.runs-on must be a sequence`)
  assert.deepEqual(
    runner.map((label, index) => requireString(label, `${authority.label}.runs-on[${index}]`)),
    authority.runner,
    `${authority.label} runner labels must be exact`,
  )
}

function validateProtectedNetworkOwner(
  owner: CommandOwner,
  label: string,
): void {
  assert.equal(
    requiredStringField(owner.job, 'environment', label),
    PROTECTED_NETWORK_ENVIRONMENT,
    `${label} must use the protected network environment`,
  )
  assert(!owner.job.has('env'), `${label} must not expose OIDC authority at job scope`)
  const commandEnvironment = requiredMappingField(owner.step, 'env', `${label} command`)
  assert.deepEqual([...commandEnvironment.entries()], [
    ['BROWSER_NETWORK_RUNTIME_CONFIG', '${{ vars.WINDSHARE_BROWSER_NETWORK_RUNTIME_CONFIG }}'],
    ['BROWSER_NETWORK_PREPARED_DIRECTORY', '${{ github.workspace }}/.browser-network-prepared'],
    [
      'BROWSER_NETWORK_PRODUCER_MANIFEST_SHA256',
      '${{ needs.full-browser-network-prepare.outputs.producer-manifest-sha256 }}',
    ],
    ['WINDSHARE_OIDC_AUDIENCE', '${{ vars.WINDSHARE_BROWSER_NETWORK_OIDC_AUDIENCE }}'],
    ['BASH_ENV', ''],
    ['ENV', ''],
    ['LD_AUDIT', ''],
    ['LD_LIBRARY_PATH', ''],
    ['LD_PRELOAD', ''],
    ['NODE_EXTRA_CA_CERTS', ''],
    ['NODE_OPTIONS', ''],
    ['NODE_PATH', ''],
    ['NODE_TLS_REJECT_UNAUTHORIZED', ''],
    ['OPENSSL_CONF', ''],
    ['PATH', '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'],
    ['SSL_CERT_DIR', ''],
    ['SSL_CERT_FILE', ''],
    ['SSLKEYLOGFILE', ''],
  ], `${label} must pass only manifest-bound runtime authority to the broker`)
  assert(!requiredStringField(owner.step, 'run', `${label} command`).includes('${{ vars.'),
    `${label} command must not interpolate repository variables into shell source`)
  assert.equal(
    requiredStringField(owner.step, 'shell', `${label} command`),
    '/bin/bash --noprofile --norc -p -euo pipefail {0}',
    `${label} must use privileged Bash so inherited shell functions, SHELLOPTS, BASHOPTS, ` +
      'CDPATH, and GLOBIGNORE cannot alter the setup-node tool-cache exec',
  )
  const command = requiredStringField(owner.step, 'run', `${label} command`)
  assert(!/(?:^|\s)node(?:\s|$)/u.test(command) && !command.includes('$PATH'),
    `${label} must not resolve Node through PATH`)
  assert(command.startsWith('exec "$RUNNER_TOOL_CACHE/node/24.16.0/x64/bin/node"'),
    `${label} must directly exec the exact setup-node tool-cache version`)
  assert.equal(literalCount(command, 'exec '), 1, `${label} must contain exactly one exec statement`)

  const ownerSteps = stepMappings(owner.job, label)
  const brokerIndex = ownerSteps.indexOf(owner.step)
  assert.equal(brokerIndex, 3, `${label} must follow exactly three pinned setup actions`)
  const expectedPreBrokerActions = [CHECKOUT_ACTION, NODE_SETUP_ACTION, 'actions/download-artifact@v8']
  for (let index = 0; index < brokerIndex; index += 1) {
    const step = ownerSteps[index] as WorkflowMapping
    assert(!step.has('run') && !step.has('shell'), `${label} pre-broker step may not execute custom code`)
    assert.equal(requiredStringField(step, 'uses', `${label} pre-broker step ${index}`),
      expectedPreBrokerActions[index])
    assertBlankCredentialAndPreloadEnvironment(step, `${label} pre-broker step ${index}`)
  }
  assert.equal(ownerSteps.length, 5, `${label} must have one broker and one post-broker upload`)
  const upload = ownerSteps[4] as WorkflowMapping
  assert.equal(requiredStringField(upload, 'uses', `${label} upload`), UPLOAD_ACTION)
  assertBlankCredentialAndPreloadEnvironment(upload, `${label} upload`)
}

function validateFullBrowserTemporalContract(
  jobs: WorkflowMapping,
  producer: CommandOwner,
  broker: CommandOwner,
  consumer: CommandOwner,
): void {
  const idTokenOwners = [...jobs.entries()].filter(([, value]) => {
    const job = requireMapping(value, 'current-commit job')
    if (!job.has('permissions')) return false
    return requiredMappingField(job, 'permissions', 'job permissions').get('id-token') === 'write'
  })
  assert.deepEqual(idTokenOwners.map(([name]) => name), [broker.jobName],
    'only the broker job may receive id-token write authority')

  assert.equal(countCommand(jobs, BROWSER_NETWORK_PREPARE_AUTHORITY.command), 1,
    'the content-addressed runtime must be produced exactly once')
  assert.equal(countCommand(jobs, BROWSER_NETWORK_BROKER_AUTHORITY.command), 1,
    'the protected network broker must run exactly once')
  assert.equal(countCommand(jobs, BROWSER_FULL_AUTHORITY.command), 1,
    'the public browser DAG must run exactly once')
  assert.equal(countCommand(jobs, 'node scripts/ci/makeauthority/entry.mjs browser-local'), 0,
    'browser-local must run only inside the final public browser DAG')

  const workflowValues = semanticStrings(jobs)
  assert(!workflowValues.some((value) => /(?:^|\s)tar\s+-[a-z]*[xf]/u.test(value)),
    'protected browser transfer must not restore tar archives')
  assert(!workflowValues.some((value) => value.includes('web-node-modules.tar')),
    'protected browser transfer must not carry node_modules')

  const producerSteps = stepMappings(producer.job, BROWSER_NETWORK_PREPARE_AUTHORITY.label)
  assert.equal(requiredStringField(producer.step, 'id', BROWSER_NETWORK_PREPARE_AUTHORITY.label), 'prepared')
  assert.deepEqual(
    [...requiredMappingField(producer.job, 'outputs', 'browser network producer outputs').entries()],
    [['producer-manifest-sha256', '${{ steps.prepared.outputs.producer_manifest_sha256 }}']],
    'the producer must expose only the manifest digest the broker actually verifies',
  )
  assert(!semanticStrings(producer.job).some((value) => value.includes('artifact-digest')),
    'artifact transit metadata must not masquerade as a broker-verified binding')
  assert(producerSteps.some((step) =>
    optionalStringField(step, 'run') === 'bash scripts/ci/linux/browser/prepare.sh'),
  'producer must build helpers without OIDC authority')
  assert(producerSteps.some((step) =>
    optionalStringField(step, 'uses') === UPLOAD_ACTION),
  'producer must transfer its immutable prepared artifact')

  assertBlankCredentialAndPreloadEnvironment(consumer.job, 'token-free final browser job')
  assert(!requiredStringField(consumer.step, 'run', BROWSER_FULL_AUTHORITY.label)
    .includes('ACTIONS_ID_TOKEN'), 'final browser command may not receive OIDC request authority')
  assert.equal(jobNeeds(broker.job)[0], producer.jobName)
  assert.equal(jobNeeds(consumer.job)[0], broker.jobName)
}

function assertBlankCredentialAndPreloadEnvironment(owner: WorkflowMapping, label: string): void {
  const environment = requiredMappingField(owner, 'env', label)
  for (const name of [
    'ACTIONS_ID_TOKEN_REQUEST_URL',
    'ACTIONS_ID_TOKEN_REQUEST_TOKEN',
    'NODE_OPTIONS',
    'NODE_PATH',
  ]) assert.equal(requiredStringField(environment, name, label), '', `${label} must blank ${name}`)
}

function countCommand(jobs: WorkflowMapping, command: string): number {
  return jobRunCommands(jobs).filter((candidate) => candidate === command).length
}

function validateStabilityOwner(
  owner: CommandOwner,
  authority: typeof STABILITY_AUTHORITIES[number],
): void {
  validateBlockingOwner(owner, authority, [])
  assert.equal(
    jobRunCommands(new Map([[owner.jobName, owner.job]])).length,
    1,
    `${authority.label} must delegate execution and terminal evidence to one owner process`,
  )
}

function validateReleaseHistoryOwner(owner: CommandOwner): void {
  validateRunner(owner, RELEASE_HISTORY_AUTHORITY)
  assert.deepEqual(jobNeeds(owner.job), [], 'stability history reducer must precede all current-commit jobs')
  assert(!owner.job.has('if') && !owner.job.has('continue-on-error') && !owner.job.has('strategy'),
    'stability history job must remain one blocking owner')
  assert.equal(requiredStringField(owner.step, 'id', 'stability history reducer'), 'reducer')
  assert.equal(requiredBooleanField(owner.step, 'continue-on-error', 'stability history reducer'), true)
  assert(!owner.step.has('if'), 'stability history reducer must always execute')
  const finalizer = uniqueRelatedStep(
    owner.job,
    /node\s+-e\s+"process\.exit\(1\)"/u,
    'stability history reducer finalizer',
  )
  assert.equal(requiredStringField(finalizer, 'run', 'stability history reducer finalizer'),
    'node -e "process.exit(1)"')
  assert.equal(requiredStringField(finalizer, 'if', 'stability history reducer finalizer'),
    "${{ always() && steps.reducer.outcome != 'success' }}")
  for (const step of stepMappings(owner.job, 'stability history reducer')) {
    if (step !== owner.step) assert(!step.has('continue-on-error'), 'only the reducer step may tolerate failure')
  }
}

function stabilityArtifactRetention(owner: CommandOwner, operatingSystem: string): number {
  const upload = uniqueActionStep(owner.job, UPLOAD_ACTION, `${operatingSystem} stability artifact`)
  assert.equal(requiredStringField(upload, 'if', `${operatingSystem} stability upload`), ALWAYS_EXPRESSION)
  const inputs = requiredMappingField(upload, 'with', `${operatingSystem} stability upload`)
  assert.deepEqual(
    [...inputs.keys()].sort(),
    ['if-no-files-found', 'name', 'path', 'retention-days'],
    `${operatingSystem} stability artifact inputs must be exact`,
  )
  assert.equal(
    requiredStringField(inputs, 'name', `${operatingSystem} stability upload`),
    `stability-integration-${operatingSystem}-\${{ github.run_id }}-\${{ github.run_attempt }}`,
  )
  assert.equal(
    requiredStringField(inputs, 'path', `${operatingSystem} stability upload`),
    `test-results/stability/integration-${operatingSystem}`,
  )
  assert.equal(requiredStringField(inputs, 'if-no-files-found', `${operatingSystem} stability upload`), 'error')
  return positiveIntegerField(inputs, 'retention-days', `${operatingSystem} stability upload`)
}

function validateBrowserSmokeArtifact(owner: CommandOwner): void {
  validateDiagnosticArtifact(owner, {
    label: 'Windows Chromium smoke diagnostics',
    name: 'browser-smoke-${{ github.run_id }}-${{ github.run_attempt }}',
    path: 'test-results/browser-smoke',
    missing: 'warn',
  })
}

function validateFullBrowserArtifact(owner: CommandOwner): void {
  validateDiagnosticArtifact(owner, {
    label: 'full browser diagnostics',
    name: 'browser-full-${{ github.run_id }}-${{ github.run_attempt }}',
    path: [
      'test-results/browser-evidence',
      'test-results/browser-network',
      'test-results/browser-network-completion.json',
      'test-results/browser-network-execution-binding.json',
      'test-results/browser-network-producer-manifest.json',
      'test-results/browser-network-runtime-helper-manifest.json',
    ].join('\n'),
    missing: 'warn',
  })
}

function validateDiagnosticArtifact(
  owner: CommandOwner,
  expected: Readonly<{ label: string, name: string, path: string, missing: string }>,
): void {
  const upload = uniqueActionStep(owner.job, UPLOAD_ACTION, expected.label)
  assert.equal(requiredStringField(upload, 'if', expected.label), ALWAYS_EXPRESSION)
  const inputs = requiredMappingField(upload, 'with', `${expected.label} upload`)
  assert.deepEqual(
    [...inputs.keys()].sort(),
    ['if-no-files-found', 'include-hidden-files', 'name', 'path'],
    `${expected.label} inputs must be exact`,
  )
  assert.equal(requiredStringField(inputs, 'name', expected.label), expected.name, `${expected.label} name is exact`)
  assert.equal(requiredStringField(inputs, 'path', expected.label), expected.path, `${expected.label} path is exact`)
  assert.equal(
    requiredStringField(inputs, 'if-no-files-found', expected.label),
    expected.missing,
    `${expected.label} missing-file policy is exact`,
  )
  assert.equal(
    requiredBooleanField(inputs, 'include-hidden-files', expected.label),
    true,
    `${expected.label} includes hidden diagnostics`,
  )
}

function validateReleaseHistoryArtifact(owner: CommandOwner): void {
  const upload = uniqueActionStep(owner.job, UPLOAD_ACTION, 'release stability verdict')
  assert.equal(requiredStringField(upload, 'if', 'release stability verdict upload'), ALWAYS_EXPRESSION,
    'release stability verdict must upload on reducer success and failure')
  const inputs = requiredMappingField(upload, 'with', 'release stability verdict upload')
  assert.deepEqual(
    [...inputs.keys()].sort(),
    ['if-no-files-found', 'name', 'path', 'retention-days'],
    'release stability verdict artifact inputs must be exact',
  )
  assert.equal(
    requiredStringField(inputs, 'name', 'release stability verdict upload'),
    'stability-release-verdict-${{ github.run_id }}-${{ github.run_attempt }}',
  )
  assert.equal(
    requiredStringField(inputs, 'path', 'release stability verdict upload'),
    'test-results/stability/release-verdict.json',
  )
  assert.equal(requiredStringField(inputs, 'if-no-files-found', 'release stability verdict upload'), 'error')
  assert.equal(positiveIntegerField(inputs, 'retention-days', 'release stability verdict upload'), 90)
}

function validatePackageTargetEncapsulation(jobs: WorkflowMapping): void {
  for (const target of [
    'test:browser:evidence:contract',
    GENERATED_SEMANTIC_PROCESS_TARGET,
    BROWSER_PROCESS_INTEGRATION_TARGET,
  ]) {
    const count = jobRunCommands(jobs)
      .reduce((total, command) => total + packageTargetCount(command, target), 0)
    assert.equal(count, 0, `${target} must remain behind its platform entrypoint`)
  }
}

function assertBrowserInstrumentationOnce(
  pullRequestGates: readonly string[],
  fullGates: readonly string[],
  operationPlan: readonly string[],
): void {
  assert.equal(countValue(pullRequestGates, 'browser-contract'), 1)
  assert.equal(countValue(pullRequestGates, 'browser-generated'), 1)
  assert.equal(countValue(pullRequestGates, 'browser-process'), 1)
  assert.equal(countValue(pullRequestGates, 'e2e'), 1)
  assert.equal(countValue(pullRequestGates, 'e2e-go'), 0)
  assert.equal(countValue(fullGates, 'browser-contract'), 1)
  assert.equal(countValue(fullGates, 'browser-generated'), 1)
  assert.equal(countValue(fullGates, 'browser-process'), 1)
  assert.equal(countValue(fullGates, 'browser'), 1)
  assert.equal(countValue(fullGates, 'browser-network'), 0)
  assert.equal(countValue(fullGates, 'e2e'), 1)
  assert.equal(countValue(fullGates, 'e2e-go'), 0)
  assert.equal(countValue(operationPlan, 'browser-contract'), 1)
  assert.equal(countValue(operationPlan, 'generated-semantic-process'), 1)
}

function scriptAuthority(label: string, command: string, runner: string): CommandAuthority {
  const scriptPath = command.split(/\s+/u).find((token) => /scripts\/ci\//u.test(token))
  if (scriptPath === undefined) throw new Error(`${label} has no script identity`)
  return Object.freeze({
    label,
    command,
    runner,
    identity: new RegExp(escapeRegExp(scriptPath.replace(/^\.\//u, '')), 'u'),
  })
}

function uniqueRelatedStep(job: WorkflowMapping, identity: RegExp, label: string): WorkflowMapping {
  const matches = stepMappings(job, label).filter((step) => {
    const command = optionalStringField(step, 'run')
    return command !== undefined && identity.test(command)
  })
  assert.equal(matches.length, 1, `${label} must have exactly one owner`)
  return matches[0] as WorkflowMapping
}

function uniqueActionStep(job: WorkflowMapping, action: string, label: string): WorkflowMapping {
  const matches = stepMappings(job, label)
    .filter((step) => optionalStringField(step, 'uses')?.startsWith(action.split('@')[0] as string) === true)
  assert.equal(matches.length, 1, `${label} must have exactly one upload owner`)
  const step = matches[0] as WorkflowMapping
  assert.equal(requiredStringField(step, 'uses', label), action)
  return step
}

function cronValues(triggers: WorkflowMapping): string[] {
  return requiredSequenceField(triggers, 'schedule', 'workflow triggers').map((value, index) =>
    requiredStringField(requireMapping(value, `schedule[${index}]`), 'cron', `schedule[${index}]`))
}

function requiredPositiveArgument(command: string, name: string): number {
  const matches = [...command.matchAll(new RegExp(`${escapeRegExp(name)}\\s+([1-9][0-9]*)`, 'gu'))]
  assert.equal(matches.length, 1, `${name} must have one positive value`)
  return Number(matches[0]?.[1])
}

function workflowJobs(workflow: WorkflowMapping): WorkflowMapping {
  return requiredMappingField(workflow, 'jobs', 'workflow')
}

function jobNeeds(job: WorkflowMapping): string[] {
  if (!job.has('needs')) return []
  const value = requiredField(job, 'needs', 'workflow job')
  if (typeof value === 'string') return [value]
  assert(Array.isArray(value), 'workflow job needs must be a string or sequence')
  return value.map((dependency, index) => requireString(dependency, `workflow job needs[${index}]`))
}

function stepMappings(job: WorkflowMapping, label: string): WorkflowMapping[] {
  return requiredSequenceField(job, 'steps', label)
    .map((step, index) => requireMapping(step, `${label}.steps[${index}]`))
}

function jobRunCommands(jobs: WorkflowMapping): string[] {
  return [...jobs.values()].flatMap((value) => {
    const job = requireMapping(value, 'workflow job')
    if (!job.has('steps')) return []
    return stepMappings(job, 'workflow job').flatMap((step) => {
      const run = optionalStringField(step, 'run')
      return run === undefined ? [] : [run]
    })
  })
}

function packageTargetCount(command: string, target: string): number {
  const pattern = new RegExp(
    `pnpm(?:\\s+-C\\s+web)?\\s+run\\s+${escapeRegExp(target)}(?=\\s|$)`,
    'gu',
  )
  return command.match(pattern)?.length ?? 0
}

function packageScripts(packageManifest: string): Record<string, string> {
  const value: unknown = JSON.parse(packageManifest)
  assert(value !== null && typeof value === 'object' && !Array.isArray(value), 'package manifest must be an object')
  const scripts: unknown = Reflect.get(value, 'scripts')
  assert(scripts !== null && typeof scripts === 'object' && !Array.isArray(scripts), 'package scripts must be an object')
  assert(Object.values(scripts).every((command) => typeof command === 'string'), 'package scripts must contain commands')
  return scripts as Record<string, string>
}

function requiredScript(scripts: Readonly<Record<string, string>>, name: string): string {
  const value = scripts[name]
  if (typeof value !== 'string') throw new Error(`${name} platform script is missing`)
  return value
}

function makeWords(makefile: string, name: string): string[] {
  const value = makeAssignment(makefile, name)
  const words = value === '' ? [] : value.split(/\s+/u)
  assert(words.length > 0, `${name} must not be empty`)
  assert(words.every((word) => /^[a-z0-9]+(?:-[a-z0-9]+)*$/u.test(word)), `${name} must contain literal names`)
  return words
}

function makeAssignment(makefile: string, name: string): string {
  const pattern = new RegExp(`^(?:override\\s+)?${escapeRegExp(name)}\\s*:?=\\s*(.*?)\\s*$`, 'mu')
  const matches = [...makefile.matchAll(new RegExp(pattern.source, 'gmu'))]
  assert.equal(matches.length, 1, `${name} must have one explicit assignment`)
  return matches[0]?.[1] ?? ''
}

function makeTargetPrerequisites(makefile: string, target: string): string[] {
  const owners = makeTargetDeclarations(makefile, target)
  assert.equal(owners.length, 1, `${target} must have one explicit prerequisite declaration`)
  return owners[0] as string[]
}

function makeTargetDeclarations(makefile: string, target: string): string[][] {
  return makefile.split(/\r?\n/u).flatMap((line) => {
    if (line.startsWith(' ') || line.startsWith('\t') || line.startsWith('.') || line.startsWith('#')) return []
    const separator = line.indexOf(':')
    const assignment = line.indexOf('=')
    if (separator < 0 || (assignment >= 0 && assignment < separator)) return []
    const targets = line.slice(0, separator).trim().split(/\s+/u)
    if (!targets.includes(target)) return []
    const rawPrerequisites = line.slice(separator + 1)
    const comment = rawPrerequisites.indexOf(' #')
    const prerequisites = (comment < 0 ? rawPrerequisites : rawPrerequisites.slice(0, comment)).trim()
    return [prerequisites === '' ? [] : prerequisites.split(/\s+/u)]
  })
}

function countValue(values: readonly string[], expected: string): number {
  return values.filter((value) => value === expected).length
}

function literalCount(source: string, literal: string): number {
  return source.split(literal).length - 1
}

function compactCommandSource(source: string): string {
  return source.replace(/[`\\]\r?\n/gu, ' ').replace(/\s+/gu, ' ').trim()
}

function containsMakeAssignment(makefile: string, names: readonly string[]): boolean {
  return makefile.split(/\r?\n/u).some((line) => {
    const trimmed = line.trimStart()
    const assignment = trimmed.startsWith('export ') ? trimmed.slice('export '.length).trimStart() : trimmed
    return names.some((name) => {
      if (!assignment.startsWith(name)) return false
      const operator = assignment.slice(name.length).trimStart()
      return ['=', ':=', '?=', '+='].some((candidate) => operator.startsWith(candidate))
    })
  })
}

function commandConsumesWeb(command: string): boolean {
  const words = command.trim().split(/\s+/u)
  if (words.includes('pnpm')) return true
  if ([
    'scripts/ci/linux/browser',
    'scripts/ci/windows/browser',
    'scripts/ci/linux/web',
    'scripts/ci/windows/web',
  ].some((entrypoint) => command.includes(entrypoint))) return true
  if (/scripts\/ci\/makeauthority\/entry\.mjs\s+(?:ci|ci-full|browser)\b/u.test(command)) return true
  return words.some((word, index) => {
    if (word !== 'make') return false
    const target = words[index + 1]?.split(/[;&|]/u, 1)[0]
    return target === 'ci' || target === 'ci-full' || target === 'browser'
  })
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
  assert(typeof value === 'number' && Number.isSafeInteger(value) && value > 0, `${label}.${key} must be positive`)
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

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&')
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
