import assert from 'node:assert/strict'

import { GENERATED_SEMANTIC_PROCESS_TARGET } from './workflow-policy.ts'
import type { WorkflowMapping, WorkflowValue } from './yaml-document.ts'

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
