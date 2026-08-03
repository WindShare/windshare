import assert from 'node:assert/strict'

import { parseDocument } from 'yaml'

export type WorkflowValue = string | number | boolean | null | WorkflowValue[] | WorkflowMapping
export type WorkflowMapping = Map<string, WorkflowValue>

export type WorkflowSet = Readonly<{
  ci: WorkflowMapping
  browserFull: WorkflowMapping
}>

export type WorkflowSources = Readonly<{
  ci: string
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
export const BROWSER_PROCESS_INTEGRATION_TARGET = 'test:browser:process:integration'

export const PLATFORM_ENTRYPOINTS = Object.freeze([
  'browser-local',
  'browser-network',
  'browser-preflight',
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

const GITHUB_SHA_EXPRESSION = '${{ github.sha }}'
const TARGET_SHA_EXPRESSION = '${{ needs.changes.outputs.target_sha }}'
const BROWSER_NETWORK_COMPLETION_EXPRESSION =
  '${{ github.workspace }}/test-results/browser-network-completion.json'
const ORDINARY_BROWSER_JOBS = Object.freeze([
  'web',
  'browser-preflight',
  'linux-browser-process',
  'windows-browser-process',
  'windows-browser-smoke',
])

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
    browserFull: parseWorkflowYaml(sources.browserFull),
  })
}

export function validateRepositoryContracts(
  workflowSources: WorkflowSources,
  localSources: LocalContractSources,
): void {
  const workflows = parseWorkflowSet(workflowSources)
  validateCIWorkflow(workflows.ci)
  validateBrowserFullWorkflow(workflows.browserFull)
  validateMakefileContract(localSources.makefile, localSources.fullBrowserOperationPlan)
  validateLocalEntrypointContract(localSources.packageManifest, localSources.platformScripts)
}

export function validateCIWorkflow(workflow: WorkflowMapping): void {
  const jobs = workflowJobs(workflow)
  for (const name of ORDINARY_BROWSER_JOBS) {
    assert(jobs.has(name), `ordinary CI is missing browser owner ${name}`)
  }
  for (const retired of ['browser-contract', 'browser-generated', 'current-commit']) {
    assert(!jobs.has(retired), `ordinary CI must not retain the ${retired} job`)
  }

  assert.deepEqual(
    jobNeeds(requiredJob(jobs, 'browser-preflight')),
    ['changes', 'linux-preflight', 'web'],
    'browser-preflight must wait for change selection, cheap Linux checks, and Web validation',
  )
  assert.deepEqual(
    jobNeeds(requiredJob(jobs, 'linux-browser-process')),
    ['changes', 'linux-preflight', 'browser-preflight'],
  )
  assert.deepEqual(
    jobNeeds(requiredJob(jobs, 'windows-browser-process')),
    ['changes', 'windows-preflight', 'browser-preflight'],
  )
  assert.deepEqual(
    jobNeeds(requiredJob(jobs, 'windows-browser-smoke')),
    ['changes', 'windows-preflight', 'browser-preflight'],
  )

  const commands = workflowRunCommands(jobs)
  assert.equal(
    exactCommandCount(commands, 'make browser-preflight'),
    1,
    'ordinary CI must invoke one browser-preflight orchestrator',
  )
  assert.equal(exactCommandCount(commands, 'make web'), 1, 'ordinary CI must invoke one Web owner')
  for (const forbidden of [
    'make browser-contract',
    'make browser-generated',
    'test:browser:evidence:contract',
    GENERATED_SEMANTIC_PROCESS_TARGET,
  ]) {
    assert.equal(
      commands.filter((command) => command.includes(forbidden)).length,
      0,
      `ordinary CI must not create a second browser evidence owner with ${forbidden}`,
    )
  }

  for (const value of allStrings(workflow)) {
    assert.notEqual(value, 'self-hosted', 'ordinary CI must not declare a self-hosted runner')
  }
  assertNoIdentityTokenPermission(workflow, 'ordinary CI')
  assertCheckoutRef(requiredJob(jobs, 'browser-preflight'), TARGET_SHA_EXPRESSION)
}

export function validateBrowserFullWorkflow(workflow: WorkflowMapping): void {
  const jobs = workflowJobs(workflow)
  for (const name of [
    'validate-target',
    'full-browser-network-prepare',
    'full-browser-network',
    'full-browser',
  ]) {
    assert(jobs.has(name), `protected browser workflow is missing ${name}`)
  }

  const fullBrowser = requiredJob(jobs, 'full-browser')
  assert.deepEqual(jobNeeds(fullBrowser), ['full-browser-network'])
  const commands = workflowRunCommands(jobs)
  assert.equal(
    exactCommandCount(commands, 'make browser'),
    1,
    'protected browser evidence must have one final Browsergate owner',
  )
  for (const forbidden of [
    'make browser-preflight',
    'make browser-contract',
    'make browser-generated',
    'main.mjs preflight',
  ]) {
    assert.equal(
      commands.filter((command) => command.includes(forbidden)).length,
      0,
      `protected browser workflow must not replay ${forbidden}`,
    )
  }

  const owner = uniqueRunStep(fullBrowser, 'make browser')
  const environment = requiredMappingField(owner, 'env', 'full browser owner')
  assert.equal(
    requiredStringField(environment, 'BROWSER_NETWORK_COMPLETION', 'full browser owner'),
    BROWSER_NETWORK_COMPLETION_EXPRESSION,
  )
  assert.equal(
    requiredStringField(environment, 'WINDSHARE_TARGET_SHA', 'full browser owner'),
    GITHUB_SHA_EXPRESSION,
  )
  assertCheckoutRef(fullBrowser, GITHUB_SHA_EXPRESSION)

  const oidcOwners = [...jobs.entries()].filter(([, value]) => {
    const candidate = requiredMapping(value, 'workflow job')
    const permissions = optionalMappingField(candidate, 'permissions')
    return permissions?.get('id-token') === 'write'
  })
  assert.deepEqual(
    oidcOwners.map(([name]) => name),
    ['full-browser-network'],
    'only the protected network producer may receive identity-token authority',
  )
}

export function validateMakefileContract(
  makefile: string,
  fullBrowserOperationPlan: readonly string[],
): void {
  assert.deepEqual(
    makeWords(makefile, 'PLATFORM_ENTRYPOINTS'),
    PLATFORM_ENTRYPOINTS,
    'native gate entrypoints must remain explicit',
  )
  const ordinaryGates = makeWords(makefile, 'CI_GATES')
  assert.equal(countValue(ordinaryGates, 'browser-preflight'), 1)
  for (const replay of ['browser-contract', 'browser-generated', 'integration', 'e2e-go']) {
    assert.equal(countValue(ordinaryGates, replay), 0, `make ci must not replay ${replay}`)
  }
  assert.deepEqual(
    makeTargetPrerequisites(makefile, 'browser'),
    ['browser-local', 'browser-network'],
    'make browser must compose local and network evidence exactly once',
  )
  for (const retired of ['ci-full', 'authority-context', 'plan-ci', 'plan-ci-full', 'plan-browser']) {
    assert.equal(hasExplicitTarget(makefile, retired), false, `${retired} must remain retired`)
  }
  assert.equal(countValue(makeWords(makefile, 'NODE_GATES'), 'browser-preflight'), 1)
  assert.equal(countValue(fullBrowserOperationPlan, 'browser-contract'), 1)
  assert.equal(countValue(fullBrowserOperationPlan, 'generated-semantic-process'), 1)
}

export function validateLocalEntrypointContract(
  packageManifest: string,
  platformScripts: Readonly<Record<string, string>>,
): void {
  const scripts = packageScripts(packageManifest)
  assert.equal(scripts.build, 'tsc -b && vite build')
  assert.equal(literalCount(scripts.build, 'tsc -b'), 1)
  assert.equal(literalCount(scripts.build, 'vite build'), 1)
  assert.equal(scripts['test:browser'], 'node ../scripts/ci/browsergate/main.mjs local')
  assert.equal(
    scripts[GENERATED_SEMANTIC_PROCESS_TARGET],
    'node ../scripts/ci/browsergate/tests/process/generated-semantic.tests.mjs',
  )
  assert.equal(
    scripts[BROWSER_PROCESS_INTEGRATION_TARGET],
    [
      'vitest run',
      'test/browser-evidence/native-directory-publisher.test.ts',
      'test/browser-evidence/process-runner.test.ts',
      'test/browser-evidence/test-process-owner-client.test.ts',
    ].join(' '),
  )

  for (const platform of ['windows', 'linux']) {
    const web = requiredScript(platformScripts, `${platform}/web`)
    assert.equal(literalCount(web, 'pnpm -C web build'), 1)
    assert.equal(literalCount(web, 'tsc -b'), 0, `${platform} Web wrapper must not repeat typecheck`)

    const dependencies = requiredScript(platformScripts, `${platform}/web-dependencies`)
    assert.equal(literalCount(dependencies, 'pnpm -C web install --frozen-lockfile'), 1)

    const preflight = requiredScript(platformScripts, `${platform}/browser-preflight`)
    assert.equal(literalCount(preflight, 'node scripts/ci/browsergate/main.mjs preflight'), 1)

    const local = requiredScript(platformScripts, `${platform}/browser-local`)
    const portableLocal = local.replaceAll('\\', '/')
    assert.equal(literalCount(portableLocal, 'scripts/ci/browsergate/main.mjs'), 1)
    assert.equal(literalCount(local, "'local'"), platform === 'windows' ? 1 : 0)

    const process = requiredScript(platformScripts, `${platform}/browser-process`)
    assert.equal(literalCount(process, BROWSER_PROCESS_INTEGRATION_TARGET), 1)
    assert.equal(literalCount(process, GENERATED_SEMANTIC_PROCESS_TARGET), 0)

    const stability = requiredScript(platformScripts, `${platform}/browser-stability`)
    assert.equal(literalCount(stability, 'main.mjs local --run-policy stability'), 1)

    const network = requiredScript(platformScripts, `${platform}/browser-network`)
    assert.equal(literalCount(network, 'scripts/ci/browsergate/network-completion.mjs consume'), 1)
    assert(network.includes('WINDSHARE_TARGET_SHA'), `${platform} network consumer needs target SHA binding`)
    assert.equal(literalCount(network, 'git rev-parse --verify HEAD'), 1)
    assert.equal(literalCount(network, 'ValueFromRemainingArguments'), platform === 'windows' ? 1 : 0)
  }

  const smoke = requiredScript(platformScripts, 'windows/browser-smoke')
  assert.equal(literalCount(smoke, 'test:browser:smoke'), 1)

  const prepare = requiredScript(platformScripts, 'linux/browser-network-prepare')
  assert.equal(literalCount(prepare, 'build:browser-network-matrix-helpers'), 1)

  for (const [name, source] of Object.entries(platformScripts)) {
    for (const retiredAuthority of [
      'goauthority',
      'windshare_go',
      'WindShareGo',
      'makeauthority',
    ]) {
      assert.equal(
        literalCount(source, retiredAuthority),
        0,
        `${name} must use the local toolchain instead of ${retiredAuthority}`,
      )
    }
  }
}

function workflowJobs(workflow: WorkflowMapping): WorkflowMapping {
  return requiredMappingField(workflow, 'jobs', 'workflow')
}

function requiredJob(jobs: WorkflowMapping, name: string): WorkflowMapping {
  return requiredMapping(jobs.get(name), `job ${name}`)
}

function jobNeeds(job: WorkflowMapping): string[] {
  const value = job.get('needs')
  if (typeof value === 'string') return [value]
  assert(Array.isArray(value), 'job needs must be a string or sequence')
  return value.map((entry) => {
    assert.equal(typeof entry, 'string', 'job dependency must be text')
    return entry as string
  })
}

function workflowRunCommands(jobs: WorkflowMapping): string[] {
  return [...jobs.values()].flatMap((value) =>
    stepMappings(requiredMapping(value, 'workflow job')).flatMap((step) => {
      const run = step.get('run')
      return typeof run === 'string' ? [run.trim()] : []
    }))
}

function uniqueRunStep(job: WorkflowMapping, command: string): WorkflowMapping {
  const matches = stepMappings(job).filter((step) => step.get('run') === command)
  assert.equal(matches.length, 1, `${command} must have one owning step`)
  return matches[0] as WorkflowMapping
}

function stepMappings(job: WorkflowMapping): WorkflowMapping[] {
  const value = job.get('steps')
  assert(Array.isArray(value), 'workflow job steps must be a sequence')
  return value.map((step) => requiredMapping(step, 'workflow step'))
}

function assertCheckoutRef(job: WorkflowMapping, expected: string): void {
  const checkouts = stepMappings(job).filter((step) => {
    const uses = step.get('uses')
    return typeof uses === 'string' && uses.startsWith('actions/checkout@')
  })
  assert.equal(checkouts.length, 1, 'job must have one checkout')
  const withInputs = requiredMappingField(checkouts[0] as WorkflowMapping, 'with', 'checkout')
  assert.equal(requiredStringField(withInputs, 'ref', 'checkout'), expected)
}

function assertNoIdentityTokenPermission(value: WorkflowValue, label: string): void {
  if (value instanceof Map) {
    for (const [name, child] of value) {
      assert(!(name === 'id-token' && child === 'write'), `${label} must not grant id-token: write`)
      assertNoIdentityTokenPermission(child, label)
    }
    return
  }
  if (Array.isArray(value)) {
    for (const child of value) assertNoIdentityTokenPermission(child, label)
  }
}

function allStrings(value: WorkflowValue): string[] {
  if (typeof value === 'string') return [value]
  if (value instanceof Map) return [...value.values()].flatMap(allStrings)
  if (Array.isArray(value)) return value.flatMap(allStrings)
  return []
}

function exactCommandCount(commands: readonly string[], expected: string): number {
  return commands.filter((command) => command === expected).length
}

function packageScripts(packageManifest: string): Record<string, string> {
  const parsed: unknown = JSON.parse(packageManifest)
  assert(parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed))
  const scripts = (parsed as { scripts?: unknown }).scripts
  assert(scripts !== null && typeof scripts === 'object' && !Array.isArray(scripts))
  for (const [name, value] of Object.entries(scripts)) {
    assert.equal(typeof value, 'string', `package script ${name} must be text`)
  }
  return scripts as Record<string, string>
}

function requiredScript(scripts: Readonly<Record<string, string>>, name: string): string {
  const source = scripts[name]
  assert.notEqual(source, undefined, `platform script ${name} is missing`)
  return source as string
}

function makeWords(makefile: string, variable: string): string[] {
  const assignment = makeAssignment(makefile, variable)
  return assignment.split(/\s+/u).filter((word) => word.length > 0)
}

function makeAssignment(makefile: string, variable: string): string {
  const pattern = new RegExp(`^${escapeRegExp(variable)}\\s*(?::=|=)\\s*(.*)$`, 'mu')
  const match = pattern.exec(makefile)
  assert.notEqual(match, null, `Makefile assignment ${variable} is missing`)
  return (match?.[1] ?? '').trim()
}

function makeTargetPrerequisites(makefile: string, target: string): string[] {
  const pattern = new RegExp(`^${escapeRegExp(target)}\\s*:\\s*([^\\r\\n]*)$`, 'mu')
  const match = pattern.exec(makefile)
  assert.notEqual(match, null, `Makefile target ${target} is missing`)
  return (match?.[1] ?? '').trim().split(/\s+/u).filter((word) => word.length > 0)
}

function hasExplicitTarget(makefile: string, target: string): boolean {
  return new RegExp(`^${escapeRegExp(target)}\\s*:`, 'mu').test(makefile)
}

function countValue(values: readonly string[], expected: string): number {
  return values.filter((value) => value === expected).length
}

function literalCount(source: string, literal: string): number {
  if (literal.length === 0) throw new Error('counted literal must not be empty')
  let count = 0
  let offset = 0
  while (true) {
    const index = source.indexOf(literal, offset)
    if (index < 0) return count
    count += 1
    offset = index + literal.length
  }
}

function requiredMappingField(
  mapping: WorkflowMapping,
  name: string,
  label: string,
): WorkflowMapping {
  return requiredMapping(mapping.get(name), `${label}.${name}`)
}

function optionalMappingField(mapping: WorkflowMapping, name: string): WorkflowMapping | undefined {
  const value = mapping.get(name)
  return value === undefined ? undefined : requiredMapping(value, name)
}

function requiredStringField(mapping: WorkflowMapping, name: string, label: string): string {
  const value = mapping.get(name)
  assert.equal(typeof value, 'string', `${label}.${name} must be text`)
  return value as string
}

function requiredMapping(value: WorkflowValue | undefined, label: string): WorkflowMapping {
  assert(value instanceof Map, `${label} must be a mapping`)
  return value as WorkflowMapping
}

function firstLine(value: string): string {
  return value.split(/\r?\n/u, 1)[0] ?? value
}

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : 'unknown YAML error'
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&')
}
