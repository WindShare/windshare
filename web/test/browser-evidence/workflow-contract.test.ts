import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  WORKFLOW_ALIAS_EXPANSION_LIMIT,
  parseWorkflowYaml,
  validateBrowserFullWorkflow,
  validateCIWorkflow,
  validateLocalEntrypointContract,
  validateMakefileContract,
  validateRepositoryContracts,
  type WorkflowMapping,
  type WorkflowSources,
  type WorkflowValue,
} from './workflow-contract.ts'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const orchestratorModule: unknown = await import(pathToFileURL(resolve(
  REPOSITORY_ROOT,
  'scripts/ci/browsergate/orchestrator.mjs',
)).href)
const workflowSources: WorkflowSources = Object.freeze({
  ci: repositoryFile('.github/workflows/ci.yml'),
  browserFull: repositoryFile('.github/workflows/browser-full.yml'),
})
const makefileSource = repositoryFile('Makefile')
const packageManifest = repositoryFile('web/package.json')
const platformScripts = Object.freeze(Object.fromEntries([
  ...['windows', 'linux'].flatMap((platform) =>
    [
      'browser-local',
      'browser-network',
      'browser-preflight',
      'browser-process',
      'browser-stability',
      'web',
      'web-dependencies',
    ].map((name) => [
      `${platform}/${name}`,
      repositoryFile(`scripts/ci/${platform}/${name}.${platform === 'windows' ? 'ps1' : 'sh'}`),
    ])),
  ['windows/browser-smoke', repositoryFile('scripts/ci/windows/browser/smoke.ps1')],
  ['linux/browser-network-prepare', repositoryFile('scripts/ci/linux/browser/prepare.sh')],
]))
const fullBrowserOperationPlan = readFullBrowserOperationPlan(orchestratorModule)

describe('slim browser workflow contract', () => {
  it('validates the checked-in ordinary/protected owners and local entrypoints', () => {
    expect(() => validateRepositoryContracts(workflowSources, {
      makefile: makefileSource,
      packageManifest,
      platformScripts,
      fullBrowserOperationPlan,
    })).not.toThrow()
  })

  it('rejects malformed YAML and excessive alias expansion', () => {
    expect(() => parseWorkflowYaml('jobs:\n  owner: [\n')).toThrow()
    expect(() => parseWorkflowYaml('jobs:\n  owner: 1\n  owner: 2\n')).toThrow(/DUPLICATE_KEY/u)
    expect(() => parseWorkflowYaml(aliasDocument(WORKFLOW_ALIAS_EXPANSION_LIMIT + 1)))
      .toThrow(/alias expansion/u)
  })

  it('requires one ordinary browser preflight owner with no legacy replay jobs', () => {
    const splitOwner = parseWorkflowYaml(workflowSources.ci)
    runStep(job(splitOwner, 'browser-preflight'), 'make browser-preflight')
      .set('run', 'make browser-contract')
    expect(() => validateCIWorkflow(splitOwner)).toThrow(/one browser-preflight orchestrator/u)

    const replay = parseWorkflowYaml(workflowSources.ci)
    workflowJobs(replay).set('browser-generated', new Map<string, WorkflowValue>([
      ['runs-on', 'ubuntu-latest'],
      ['steps', []],
    ]))
    expect(() => validateCIWorkflow(replay)).toThrow(/must not retain/u)

    const privileged = parseWorkflowYaml(workflowSources.ci)
    job(privileged, 'browser-preflight').set('runs-on', ['self-hosted', 'linux'])
    expect(() => validateCIWorkflow(privileged)).toThrow(/self-hosted/u)
  })

  it('binds the one protected browser owner to completion and target SHA', () => {
    const missingIdentity = parseWorkflowYaml(workflowSources.browserFull)
    const owner = runStep(job(missingIdentity, 'full-browser'), 'make browser')
    mappingField(owner, 'env').delete('WINDSHARE_TARGET_SHA')
    expect(() => validateBrowserFullWorkflow(missingIdentity)).toThrow(/WINDSHARE_TARGET_SHA/u)

    const replay = parseWorkflowYaml(workflowSources.browserFull)
    const fullSteps = sequenceField(job(replay, 'full-browser'), 'steps')
    fullSteps.push(new Map([['run', 'make browser-preflight']]))
    expect(() => validateBrowserFullWorkflow(replay)).toThrow(/must not replay/u)
  })

  it('keeps one TypeScript build and one cross-platform preflight entrypoint', () => {
    const duplicatedBuild = packageManifest.replace(
      '"build": "tsc -b && vite build"',
      '"build": "tsc -b && tsc -b && vite build"',
    )
    expect(() => validateLocalEntrypointContract(duplicatedBuild, platformScripts))
      .toThrow(/Expected values to be strictly equal/u)

    const repeatedTypecheck = {
      ...platformScripts,
      'linux/web': `${platformScripts['linux/web']}\npnpm -C web exec tsc -b --force\n`,
    }
    expect(() => validateLocalEntrypointContract(packageManifest, repeatedTypecheck))
      .toThrow(/must not repeat typecheck/u)

    const missingTargetIdentity = {
      ...platformScripts,
      'windows/browser-network':
        platformScripts['windows/browser-network']?.replaceAll('WINDSHARE_TARGET_SHA', 'LEGACY_SHA') ?? '',
    }
    expect(() => validateLocalEntrypointContract(packageManifest, missingTargetIdentity))
      .toThrow(/target SHA binding/u)
  })

  it('keeps full Browsergate instrumentation single-owned in the local plan', () => {
    expect(() => validateMakefileContract(makefileSource, [
      ...fullBrowserOperationPlan,
      'browser-contract',
    ])).toThrow()
  })
})

function repositoryFile(path: string): string {
  return readFileSync(resolve(REPOSITORY_ROOT, path), 'utf8')
}

function readFullBrowserOperationPlan(value: unknown): readonly string[] {
  if (value === null || typeof value !== 'object') throw new Error('orchestrator module is invalid')
  const plan = (value as { localOperationPlan?: unknown }).localOperationPlan
  if (typeof plan !== 'function') throw new Error('orchestrator local operation plan is unavailable')
  const operations: unknown = plan(process.platform)
  if (!Array.isArray(operations) || operations.some((entry) => typeof entry !== 'string')) {
    throw new Error('orchestrator local operation plan is invalid')
  }
  return Object.freeze([...operations]) as readonly string[]
}

function workflowJobs(workflow: WorkflowMapping): WorkflowMapping {
  return mappingField(workflow, 'jobs')
}

function job(workflow: WorkflowMapping, name: string): WorkflowMapping {
  const value = workflowJobs(workflow).get(name)
  if (!(value instanceof Map)) throw new Error(`job ${name} is missing`)
  return value
}

function runStep(owner: WorkflowMapping, command: string): WorkflowMapping {
  const matches = sequenceField(owner, 'steps').filter((value): value is WorkflowMapping =>
    value instanceof Map && value.get('run') === command)
  if (matches.length !== 1) throw new Error(`command ${command} does not have one step`)
  return matches[0] as WorkflowMapping
}

function mappingField(mapping: WorkflowMapping, name: string): WorkflowMapping {
  const value = mapping.get(name)
  if (!(value instanceof Map)) throw new Error(`${name} must be a mapping`)
  return value
}

function sequenceField(mapping: WorkflowMapping, name: string): WorkflowValue[] {
  const value = mapping.get(name)
  if (!Array.isArray(value)) throw new Error(`${name} must be a sequence`)
  return value
}

function aliasDocument(count: number): string {
  return [
    'shared: &shared',
    '  runs-on: ubuntu-latest',
    '  steps: []',
    'jobs:',
    ...Array.from({ length: count }, (_, index) => `  owner-${index}: *shared`),
    '',
  ].join('\n')
}
