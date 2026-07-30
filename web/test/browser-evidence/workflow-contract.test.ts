import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  BROWSER_PROCESS_COMMAND,
  BROWSER_PROCESS_INTEGRATION_COMMAND,
  BROWSER_PROCESS_INTEGRATION_TARGET,
  BROWSER_PROCESS_TARGET,
  CRITICAL_JOB_RESERVE_MINUTES,
  GENERATED_SEMANTIC_PROCESS_COMMAND,
  GENERATED_SEMANTIC_PROCESS_TARGET,
  WORKFLOW_ALIAS_EXPANSION_LIMIT,
  parseWorkflowYaml,
  validateWorkflowContract,
  validateWorkflowSource,
  type WorkflowMapping,
  type WorkflowValue,
} from './workflow-contract.ts'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const WORKFLOW_PATH = resolve(REPOSITORY_ROOT, '.github/workflows/ci.yml')
const WEB_PACKAGE_PATH = resolve(REPOSITORY_ROOT, 'web/package.json')
const workflowSource = readFileSync(WORKFLOW_PATH, 'utf8')
const webPackageSource = readFileSync(WEB_PACKAGE_PATH, 'utf8')

describe('browser workflow YAML boundary', () => {
  it('parses and validates the checked-in workflow as one semantic document', () => {
    expect(() => validateWorkflowSource(workflowSource)).not.toThrow()
  })

  it('rejects malformed YAML before semantic validation can inspect a partial tree', () => {
    expect(() => validateWorkflowSource('jobs:\n  browser-contract: [\n'))
      .toThrow(/BAD_INDENT/u)
  })

  it('rejects duplicate keys independently of actionlint', () => {
    expect(() => parseWorkflowYaml('jobs:\n  browser-contract: 1\n  browser-contract: 2\n'))
      .toThrow(/DUPLICATE_KEY/u)
  })

  it('rejects multiple documents and non-mapping roots', () => {
    expect(() => parseWorkflowYaml('jobs: {}\n---\njobs: {}\n')).toThrow(/MULTIPLE_DOCS/u)
    expect(() => parseWorkflowYaml('- jobs\n')).toThrow(/root must be a mapping/u)
  })

  it('applies an explicit alias expansion ceiling', () => {
    expect(() => parseWorkflowYaml(aliasDocument(WORKFLOW_ALIAS_EXPANSION_LIMIT - 1))).not.toThrow()
    expect(() => parseWorkflowYaml(aliasDocument(WORKFLOW_ALIAS_EXPANSION_LIMIT)))
      .toThrow(/Excessive alias count/u)
  })

  it('ignores comments when evaluating executable authority', () => {
    const hostileComment = '# docker secrets.ADMIN browser-network matrix.topology container:'
    expect(() => validateWorkflowSource(`${workflowSource}\n${hostileComment}\n`)).not.toThrow()
  })
})

describe('browser workflow semantic contract', () => {
  it('keeps the generated-semantic process gate independent of the evidence artifact DAG', () => {
    const processDependsOnContract = cloneWorkflow()
    job(processDependsOnContract, 'browser-generated-semantic-process').set('needs', 'browser-contract')
    expect(() => validateWorkflowContract(processDependsOnContract))
      .toThrow(/outside the browser evidence DAG/u)

    const mainDependsOnProcess = cloneWorkflow()
    job(mainDependsOnProcess, 'browser-main').set(
      'needs',
      ['browser-contract', 'browser-generated-semantic-process'],
    )
    expect(() => validateWorkflowContract(mainDependsOnProcess))
      .toThrow(/main must depend only on the contract gate/u)

    const verdictDependsOnProcess = cloneWorkflow()
    job(verdictDependsOnProcess, 'browser-verdict').set(
      'needs',
      ['browser-main', 'browser-pion', 'browser-generated-semantic-process'],
    )
    expect(() => validateWorkflowContract(verdictDependsOnProcess))
      .toThrow(/verdict must depend only on the two artifact producers/u)
  })

  it('requires a blocking Ubuntu owner for the dedicated generated-semantic target', () => {
    const wrongRunner = cloneWorkflow()
    job(wrongRunner, 'browser-generated-semantic-process').set('runs-on', 'windows-latest')
    expect(() => validateWorkflowContract(wrongRunner)).toThrow(/ubuntu-latest/u)

    const nonBlocking = cloneWorkflow()
    job(nonBlocking, 'browser-generated-semantic-process').set('continue-on-error', true)
    expect(() => validateWorkflowContract(nonBlocking)).toThrow(/must remain blocking/u)

    const wrongTarget = cloneWorkflow()
    const processStep = stepMappings(job(wrongTarget, 'browser-generated-semantic-process'))
      .find((step) => stringField(step, 'run')?.includes(GENERATED_SEMANTIC_PROCESS_TARGET) === true)
    if (processStep === undefined) throw new Error('generated-semantic process step fixture is missing')
    processStep.set('run', `pnpm -C web run ${BROWSER_PROCESS_TARGET}`)
    expect(() => validateWorkflowContract(wrongTarget)).toThrow(/dedicated package target exactly once/u)

    const wrongSetup = cloneWorkflow()
    mappingField(setupNodeStep(wrongSetup, 'browser-generated-semantic-process'), 'with')
      .set('cache-dependency-path', 'pnpm-lock.yaml')
    expect(() => validateWorkflowContract(wrongSetup)).toThrow(/web\/pnpm-lock\.yaml/u)

    const extraAuthority = cloneWorkflow()
    sequenceField(job(extraAuthority, 'browser-generated-semantic-process'), 'steps').push(
      new Map<string, WorkflowValue>([
        ['name', 'unexpected authority'],
        ['timeout-minutes', 1],
        ['run', 'node --version'],
      ]),
    )
    job(extraAuthority, 'browser-generated-semantic-process').set('timeout-minutes', 52)
    expect(() => validateWorkflowContract(extraAuthority)).toThrow(/exactly five steps/u)

    const duplicateOwner = cloneWorkflow()
    sequenceField(job(duplicateOwner, 'web'), 'steps').push(new Map<string, WorkflowValue>([
      ['name', 'duplicate generated-semantic owner'],
      ['run', `pnpm -C web run ${GENERATED_SEMANTIC_PROCESS_TARGET}`],
    ]))
    expect(() => validateWorkflowContract(duplicateOwner)).toThrow(/exactly one workflow owner/u)
  })

  it('rejects trigger drift', () => {
    const missingDocumentationExclusion = cloneWorkflow()
    const push = mappingField(mappingField(missingDocumentationExclusion, 'on'), 'push')
    const ignoredPaths = sequenceField(push, 'paths-ignore')
    ignoredPaths.splice(ignoredPaths.indexOf('docs/**'), 1)
    expect(() => validateWorkflowContract(missingDocumentationExclusion))
      .toThrow(/documentation-only changes/u)

    const dispatchEnabled = cloneWorkflow()
    mappingField(dispatchEnabled, 'on').set('workflow_dispatch', new Map())
    expect(() => validateWorkflowContract(dispatchEnabled)).toThrow(/network dispatch/u)
  })

  it('rejects forbidden execution and credential surfaces from semantic values', () => {
    const mutations: ReadonlyArray<Readonly<{
      name: string
      expected: RegExp
      mutate: (workflow: WorkflowMapping) => void
    }>> = [
      {
        name: 'container authority',
        expected: /container authority/u,
        mutate: (workflow) => job(workflow, 'browser-contract').set('container', 'node:24'),
      },
      {
        name: 'OCI runtime',
        expected: /OCI runtime/u,
        mutate: (workflow) => workflow.set('forbidden-runtime', 'docker'),
      },
      {
        name: 'network job',
        expected: /network evidence/u,
        mutate: (workflow) => mappingField(workflow, 'jobs').set('browser-network', new Map()),
      },
      {
        name: 'topology matrix',
        expected: /topology placeholders/u,
        mutate: (workflow) => workflow.set('forbidden-matrix', '${{ matrix.topology }}'),
      },
      {
        name: 'repository secret',
        expected: /repository secrets/u,
        mutate: (workflow) => workflow.set('forbidden-secret', '${{ secrets.ADMIN }}'),
      },
    ]

    for (const mutation of mutations) {
      const workflow = cloneWorkflow()
      mutation.mutate(workflow)
      expect(() => validateWorkflowContract(workflow), mutation.name).toThrow(mutation.expected)
    }
  })

  it('requires every setup-node step to consume the single root version authority', () => {
    const floatingVersion = cloneWorkflow()
    const floatingInputs = mappingField(setupNodeStep(floatingVersion, 'web'), 'with')
    floatingInputs.delete('node-version-file')
    floatingInputs.set('node-version', 24)
    expect(() => validateWorkflowContract(floatingVersion)).toThrow(/floating node-version/u)

    const wrongVersionFile = cloneWorkflow()
    mappingField(setupNodeStep(wrongVersionFile, 'browser-main'), 'with')
      .set('node-version-file', 'web/.node-version')
    expect(() => validateWorkflowContract(wrongVersionFile)).toThrow(/root \.node-version/u)

    const missingSetup = cloneWorkflow()
    const processSteps = sequenceField(job(missingSetup, 'browser-generated-semantic-process'), 'steps')
    const setupIndex = processSteps.findIndex((step) =>
      step instanceof Map && stringField(step, 'uses')?.startsWith('actions/setup-node@') === true)
    if (setupIndex < 0) throw new Error('generated-semantic setup-node fixture is missing')
    processSteps.splice(setupIndex, 1)
    expect(() => validateWorkflowContract(missingSetup)).toThrow(/exact Node-consuming job set/u)

    const verdictSetup = cloneWorkflow()
    sequenceField(job(verdictSetup, 'browser-verdict'), 'steps').splice(1, 0, new Map<string, WorkflowValue>([
      ['uses', 'actions/setup-node@v6'],
      ['timeout-minutes', 1],
      ['with', new Map([['node-version-file', '.node-version']])],
    ]))
    expect(() => validateWorkflowContract(verdictSetup)).toThrow(/exact Node-consuming job set/u)
  })

  it('rejects deadline equality, underflow, and missing step ceilings', () => {
    const reserveMinutes = CRITICAL_JOB_RESERVE_MINUTES['browser-contract']

    const equality = cloneWorkflow()
    const equalityJob = job(equality, 'browser-contract')
    const serialMinutes = stepMappings(equalityJob)
      .reduce((total, step) => total + numberField(step, 'timeout-minutes'), 0)
    equalityJob.set('timeout-minutes', serialMinutes + reserveMinutes)
    expect(() => validateWorkflowContract(equality)).toThrow(/must exceed/u)

    const underflow = cloneWorkflow()
    job(underflow, 'browser-contract').set(
      'timeout-minutes',
      serialMinutes + reserveMinutes - 1,
    )
    expect(() => validateWorkflowContract(underflow)).toThrow(/must exceed/u)

    const missingStepTimeout = cloneWorkflow()
    stepMappings(job(missingStepTimeout, 'browser-contract'))[0]?.delete('timeout-minutes')
    expect(() => validateWorkflowContract(missingStepTimeout)).toThrow(/timeout-minutes is missing/u)
  })

  it('rejects suite lifecycle reordering and additional artifact authority', () => {
    const reordered = cloneWorkflow()
    swapStepsById(job(reordered, 'browser-main'), 'producer', 'guard')
    expect(() => validateWorkflowContract(reordered)).toThrow(/failure-safe order/u)

    const additionalUpload = cloneWorkflow()
    const mainSteps = stepMappings(job(additionalUpload, 'browser-main'))
    const setupGo = mainSteps.find((step) => stringField(step, 'uses')?.startsWith('actions/setup-go@') === true)
    if (setupGo === undefined) throw new Error('browser-main setup-go fixture is missing')
    setupGo.set('uses', 'actions/upload-artifact@v7')
    expect(() => validateWorkflowContract(additionalUpload)).toThrow(/exactly one step/u)
  })

  it('rejects credentials outside the two suite guards', () => {
    const directTokenLeak = cloneWorkflow()
    stepWithId(job(directTokenLeak, 'browser-main'), 'producer').set(
      'env',
      new Map([['GITHUB_TOKEN', '${{ github.token }}']]),
    )
    expect(() => validateWorkflowContract(directTokenLeak)).toThrow(/only the two suite guards/u)

    const expressionLeak = cloneWorkflow()
    stepWithId(job(expressionLeak, 'browser-main'), 'producer').set(
      'env',
      new Map([['PRODUCER_CREDENTIAL', '${{ github.token }}']]),
    )
    expect(() => validateWorkflowContract(expressionLeak)).toThrow(/only the two suite guards/u)
  })

  it('rejects non-blocking suite production', () => {
    const workflow = cloneWorkflow()
    stepWithId(job(workflow, 'browser-main'), 'producer').set('continue-on-error', true)
    expect(() => validateWorkflowContract(workflow)).toThrow(/must remain blocking/u)
  })

  it('rejects verdict republication and a non-authoritative reducer', () => {
    const republished = cloneWorkflow()
    const firstDownload = stepMappings(job(republished, 'browser-verdict'))
      .find((step) => stringField(step, 'uses')?.startsWith('actions/download-artifact@') === true)
    if (firstDownload === undefined) throw new Error('browser-verdict download fixture is missing')
    firstDownload.set('uses', 'actions/upload-artifact@v7')
    expect(() => validateWorkflowContract(republished)).toThrow(/unguarded publication authority/u)

    const nonBlockingReducer = cloneWorkflow()
    stepWithId(job(nonBlockingReducer, 'browser-verdict'), 'verdict').set('continue-on-error', true)
    expect(() => validateWorkflowContract(nonBlockingReducer)).toThrow(/must remain the job conclusion/u)

    const expandedAuthority = cloneWorkflow()
    sequenceField(job(expandedAuthority, 'browser-verdict'), 'steps').splice(1, 0, new Map<string, WorkflowValue>([
      ['name', 'unexpected authority'],
      ['timeout-minutes', 1],
      ['run', 'node --version'],
    ]))
    job(expandedAuthority, 'browser-verdict').set('timeout-minutes', 16)
    expect(() => validateWorkflowContract(expandedAuthority)).toThrow(/exactly checkout, two sealed downloads/u)

    const expandedDeadline = cloneWorkflow()
    job(expandedDeadline, 'browser-verdict').set('timeout-minutes', 16)
    expect(() => validateWorkflowContract(expandedDeadline)).toThrow(/15-minute hard ceiling/u)
  })
})

describe('browser process package ownership', () => {
  it('declares one generated-semantic entry and composes it into the Windows process target', () => {
    const scripts = packageScripts()
    expect(scripts[GENERATED_SEMANTIC_PROCESS_TARGET]).toBe(GENERATED_SEMANTIC_PROCESS_COMMAND)
    expect(scripts[BROWSER_PROCESS_INTEGRATION_TARGET]).toBe(BROWSER_PROCESS_INTEGRATION_COMMAND)
    expect(scripts[BROWSER_PROCESS_TARGET]).toBe(BROWSER_PROCESS_COMMAND)
  })
})

function cloneWorkflow(): WorkflowMapping {
  return structuredClone(parseWorkflowYaml(workflowSource))
}

function packageScripts(): Record<string, string> {
  const manifest: unknown = JSON.parse(webPackageSource)
  if (typeof manifest !== 'object' || manifest === null || Array.isArray(manifest)) {
    throw new Error('web package fixture must be an object')
  }
  const scripts = Reflect.get(manifest, 'scripts')
  if (
    typeof scripts !== 'object' || scripts === null || Array.isArray(scripts)
    || Object.values(scripts).some((command) => typeof command !== 'string')
  ) {
    throw new Error('web package scripts fixture must contain only commands')
  }
  return scripts as Record<string, string>
}

function aliasDocument(aliasCount: number): string {
  const aliases = Array.from({ length: aliasCount }, () => '  - *base').join('\n')
  return `base: &base [value]\naliases:\n${aliases}\n`
}

function job(workflow: WorkflowMapping, name: string): WorkflowMapping {
  return mappingField(mappingField(workflow, 'jobs'), name)
}

function mappingField(mapping: WorkflowMapping, key: string): WorkflowMapping {
  const value = mapping.get(key)
  if (!(value instanceof Map)) throw new Error(`${key} fixture must be a mapping`)
  return value
}

function sequenceField(mapping: WorkflowMapping, key: string): WorkflowValue[] {
  const value = mapping.get(key)
  if (!Array.isArray(value)) throw new Error(`${key} fixture must be a sequence`)
  return value
}

function stepMappings(workflowJob: WorkflowMapping): WorkflowMapping[] {
  return sequenceField(workflowJob, 'steps').map((step, index) => {
    if (!(step instanceof Map)) throw new Error(`step ${index + 1} fixture must be a mapping`)
    return step
  })
}

function stepWithId(workflowJob: WorkflowMapping, id: string): WorkflowMapping {
  const step = stepMappings(workflowJob).find((candidate) => stringField(candidate, 'id') === id)
  if (step === undefined) throw new Error(`step fixture ${id} is missing`)
  return step
}

function setupNodeStep(workflow: WorkflowMapping, jobName: string): WorkflowMapping {
  const step = stepMappings(job(workflow, jobName))
    .find((candidate) => stringField(candidate, 'uses')?.startsWith('actions/setup-node@') === true)
  if (step === undefined) throw new Error(`${jobName} setup-node fixture is missing`)
  return step
}

function swapStepsById(workflowJob: WorkflowMapping, leftId: string, rightId: string): void {
  const steps = sequenceField(workflowJob, 'steps')
  const left = steps.findIndex((step) => step instanceof Map && stringField(step, 'id') === leftId)
  const right = steps.findIndex((step) => step instanceof Map && stringField(step, 'id') === rightId)
  if (left < 0 || right < 0) throw new Error('step reorder fixtures are missing')
  const temporary = steps[left] as WorkflowValue
  steps[left] = steps[right] as WorkflowValue
  steps[right] = temporary
}

function stringField(mapping: WorkflowMapping, key: string): string | undefined {
  const value = mapping.get(key)
  return typeof value === 'string' ? value : undefined
}

function numberField(mapping: WorkflowMapping, key: string): number {
  const value = mapping.get(key)
  if (typeof value !== 'number') throw new Error(`${key} fixture must be a number`)
  return value
}
