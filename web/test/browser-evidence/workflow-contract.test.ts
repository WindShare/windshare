import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { describe, expect, it } from 'vitest'

import {
  FULL_GATES,
  PR_GATES,
  WORKFLOW_ALIAS_EXPANSION_LIMIT,
  parseWorkflowYaml,
  validateBrowserFullWorkflow,
  validateCIWorkflow,
  validateCurrentCommitWorkflow,
  validateLocalEntrypointContract,
  validateMakefileContract,
  validateReleaseWorkflow,
  validateRepositoryContracts,
  validateStabilityWorkflow,
  type WorkflowMapping,
  type WorkflowSources,
  type WorkflowValue,
} from './workflow-contract.ts'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const orchestratorModule: unknown = await import(pathToFileURL(resolve(
  REPOSITORY_ROOT,
  'scripts/ci/browsergate/orchestrator.mjs',
)).href)
const makeAuthorityModule = await import(pathToFileURL(resolve(
  REPOSITORY_ROOT,
  'scripts/ci/makeauthority/entry.mjs',
)).href) as {
  validateMakeInvocation: (arguments_: string[], environment: Record<string, string>) => unknown
}
const makeValidationEnvironment = Object.freeze({ PATH: process.env.PATH ?? '' })
const workflowSources: WorkflowSources = Object.freeze({
  ci: repositoryFile('.github/workflows/ci.yml'),
  currentCommit: repositoryFile('.github/workflows/current-commit.yml'),
  stability: repositoryFile('.github/workflows/stability.yml'),
  releaseReadiness: repositoryFile('.github/workflows/release-readiness.yml'),
  browserFull: repositoryFile('.github/workflows/browser-full.yml'),
})
const makefileSource = repositoryFile('Makefile')
const packageManifest = repositoryFile('web/package.json')
const platformScripts = Object.freeze(Object.fromEntries([
  ...['windows', 'linux'].flatMap((platform) =>
    [
      'browser-local', 'browser-generated', 'browser-process',
      'e2e-go', 'integration', 'web', 'web-dependencies',
    ].map((name) => [
      `${platform}/${name}`,
      repositoryFile(`scripts/ci/${platform}/${name}.${platform === 'windows' ? 'ps1' : 'sh'}`),
    ])),
  ['windows/browser-smoke', repositoryFile('scripts/ci/windows/browser/smoke.ps1')],
  ['windows/browser-network', repositoryFile('scripts/ci/windows/browser-network.ps1')],
  ['linux/browser-network', repositoryFile('scripts/ci/linux/browser/network-completion.sh')],
  ['linux/browser-network-prepare', repositoryFile('scripts/ci/linux/browser/prepare.sh')],
]))
const fullBrowserOperationPlan = readFullBrowserOperationPlan(orchestratorModule)
const FULL_BROWSER_COMMAND =
  'node scripts/ci/makeauthority/entry.mjs browser "BROWSER_NETWORK_COMPLETION=${{ github.workspace }}/test-results/browser-network-completion.json"'
const PROTECTED_BROKER_COMMAND = [
  'exec "$RUNNER_TOOL_CACHE/node/24.16.0/x64/bin/node" \\',
  '  --experimental-strip-types \\',
  '  "$BROWSER_NETWORK_PREPARED_DIRECTORY/oidc-network-broker.mjs"',
].join('\n')
const RELEASE_REDUCER_COMMAND = [
  'node scripts/ci/stability/release-reducer.mjs',
  '--repository "$GITHUB_REPOSITORY"',
  '--workflow stability.yml',
  '--required-runs 100',
  '--output test-results/stability/release-verdict.json',
].join(' ')

describe('strict reusable workflow DAG', () => {
  it('validates every checked-in workflow and local authority boundary', () => {
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
    expect(() => parseWorkflowYaml('jobs: {}\n---\njobs: {}\n')).toThrow(/MULTIPLE_DOCS/u)
    expect(() => parseWorkflowYaml(aliasDocument(WORKFLOW_ALIAS_EXPANSION_LIMIT)))
      .toThrow(/Excessive alias count/u)
  })

  it('keeps thin callers bound to one exact reusable mode', () => {
    const ci = cloneWorkflow('ci')
    mappingField(job(ci, 'current-commit'), 'with').set('browser-authority', 'full')
    expect(() => validateCIWorkflow(ci)).toThrow(/select exactly the pr/u)

    const full = cloneWorkflow('browserFull')
    mappingField(job(full, 'current-commit'), 'with').set('browser-authority', 'pr')
    expect(() => validateBrowserFullWorkflow(full)).toThrow(/select exactly the full/u)

    const release = cloneWorkflow('releaseReadiness')
    job(release, 'current-commit').delete('needs')
    expect(() => validateReleaseWorkflow(release)).toThrow(/dependencies must be exact/u)
  })

  it('retains every PR browser obligation before adding the protected full-browser tuple', () => {
    const current = cloneWorkflow('currentCommit')
    job(current, 'browser-contract').set('if', "${{ inputs.browser-authority == 'pr' }}")
    expect(() => validateCurrentCommitWorkflow(current)).toThrow(/must not be conditionally suppressed/u)

    const omitted = cloneWorkflow('currentCommit')
    const producerNeeds = field(job(omitted, 'full-browser-network-prepare'), 'needs')
    if (!Array.isArray(producerNeeds)) throw new Error('network producer fixture needs must be a sequence')
    producerNeeds.splice(producerNeeds.indexOf('browser-contract'), 1)
    expect(() => validateCurrentCommitWorkflow(omitted)).toThrow(/PR-equivalent current-commit owner/u)

    const masked = cloneWorkflow('currentCommit')
    commandStep(masked, FULL_BROWSER_COMMAND).set('run', 'make browser || true')
    expect(() => validateCurrentCommitWorkflow(masked)).toThrow(/semantic command owner|exact and unmasked/u)

    expect(PR_GATES).toContain('browser-contract')
    expect(PR_GATES).toContain('browser-generated')
    expect(FULL_GATES).toContain('browser-contract')
    expect(FULL_GATES).toContain('browser-generated')
    expect(FULL_GATES).toContain('browser')
  })

  it('requires native checkout, permissions, runner, and Make sanitation authority', () => {
    const stale = cloneWorkflow('currentCommit')
    mappingField(checkoutStep(job(stale, 'windows-integration')), 'with').set('ref', 'main')
    expect(() => validateCurrentCommitWorkflow(stale)).toThrow(/github\.sha/u)

    const unprotected = cloneWorkflow('currentCommit')
    commandOwner(unprotected, FULL_BROWSER_COMMAND).set('runs-on', ['self-hosted', 'linux'])
    expect(() => validateCurrentCommitWorkflow(unprotected)).toThrow(/runner labels must be exact/u)

    const inheritedFlags = cloneWorkflow('currentCommit')
    mappingField(inheritedFlags, 'env').set('MAKEFLAGS', '-n')
    expect(() => validateCurrentCommitWorkflow(inheritedFlags)).toThrow(/MAKEFLAGS must remain absent/u)
  })

  it('rejects inherited Bash functions, option/path state, and dynamic-loader audit authority', () => {
    const unprivilegedShell = cloneWorkflow('currentCommit')
    commandStep(unprivilegedShell, PROTECTED_BROKER_COMMAND).set(
      'shell',
      '/bin/bash --noprofile --norc -euo pipefail {0}',
    )
    expect(() => validateCurrentCommitWorkflow(unprivilegedShell)).toThrow(
      /privileged Bash.*shell functions.*SHELLOPTS.*BASHOPTS.*CDPATH.*GLOBIGNORE/su,
    )

    const missingLoaderGuard = cloneWorkflow('currentCommit')
    mappingField(commandStep(missingLoaderGuard, PROTECTED_BROKER_COMMAND), 'env').delete('LD_AUDIT')
    expect(() => validateCurrentCommitWorkflow(missingLoaderGuard)).toThrow(
      /manifest-bound runtime authority/u,
    )

    const activeLoaderGuard = cloneWorkflow('currentCommit')
    mappingField(commandStep(activeLoaderGuard, PROTECTED_BROKER_COMMAND), 'env')
      .set('LD_AUDIT', '/attacker-controlled/hostile-audit.so')
    expect(() => validateCurrentCommitWorkflow(activeLoaderGuard)).toThrow(
      /manifest-bound runtime authority/u,
    )
  })

  it('publishes a terminal reducer verdict without masking its failure', () => {
    const missingAlways = cloneWorkflow('releaseReadiness')
    actionStep(commandOwner(missingAlways, RELEASE_REDUCER_COMMAND), 'actions/upload-artifact@').delete('if')
    expect(() => validateReleaseWorkflow(missingAlways)).toThrow(/upload on reducer success and failure|\.if is missing/u)

    const lostFailure = cloneWorkflow('releaseReadiness')
    const finalizer = steps(commandOwner(lostFailure, RELEASE_REDUCER_COMMAND))
      .find((step) => step.get('run') === 'node -e "process.exit(1)"')
    if (finalizer === undefined) throw new Error('release finalizer fixture is missing')
    finalizer.set('if', 'always()')
    expect(() => validateReleaseWorkflow(lostFailure)).toThrow(/steps\.reducer\.outcome/u)

    const skippedCurrentCommit = cloneWorkflow('releaseReadiness')
    job(skippedCurrentCommit, 'current-commit').delete('if')
    expect(() => validateReleaseWorkflow(skippedCurrentCommit)).toThrow(
      /execute independently of dependency outcome/u,
    )
  })

  it('keeps scheduled integration exact, native, once-only, and verdict preserving', () => {
    const stability = cloneWorkflow('stability')
    const stabilityRun = steps(job(stability, 'linux-integration-stability'))
      .find((step) => String(step.get('run')).includes('scripts/ci/stability/result.mjs run'))
    if (stabilityRun === undefined) throw new Error('Linux stability runner fixture is missing')
    stabilityRun.set('run', `${String(stabilityRun.get('run'))} || true`)
    expect(() => validateStabilityWorkflow(stability)).toThrow(/exact and unmasked/u)

    const retriedScripts = { ...platformScripts,
      'linux/integration': platformScripts['linux/integration']?.replace(
        'windshare_go_test_json -count=1 ./integration/...',
        'for attempt in 1 2; do windshare_go_test_json -count=1 ./integration/...; done',
      ) ?? '',
    }
    expect(() => validateLocalEntrypointContract(packageManifest, retriedScripts))
      .toThrow(/exact historical command once|must not retry internally/u)
  })
})

describe('GNU Make authority masking', () => {
  it('keeps graph and shell ownership non-overridable', () => {
    expect(() => validateMakefileContract(makefileSource, fullBrowserOperationPlan)).not.toThrow()
    for (const mutation of [
      [`override CI_GATES := ${PR_GATES.join(' ')}`, 'override CI_GATES :='],
      ['override HOST_GOOS := $(WINDSHARE_HOST_GOOS)', 'override HOST_GOOS := windows'],
      ['ifneq ($(origin SHELL),default)', 'ifeq ($(origin SHELL),never)'],
      ['$(words $(MAKEFILE_LIST))', '1'],
    ] as const) {
      expect(() => validateMakefileContract(
        makefileSource.replace(mutation[0], mutation[1]),
        fullBrowserOperationPlan,
      )).toThrow()
    }
  })

  it('rejects command-line, inherited, control-flag, shell, and extra-makefile adversaries', () => {
    expect(() => validateMake(['plan-ci'])).not.toThrow()
    for (const arguments_ of [
      ['ci', 'CI_GATES='],
      ['ci-full', 'CI_FULL_GATES='],
      ['ci', 'PLATFORM_ENTRYPOINTS='],
      ['ci', 'DISPATCH_ENTRYPOINTS='],
      ['ci', 'DISPATCH=:'],
      ['ci', `CORE_ARTIFACT_COMMIT_SHA=${'0'.repeat(40)}`],
      ['ci', 'HOST_GOOS=linux'],
      ['ci', 'OS=Windows_NT'],
      ['ci', 'SHELL=/bin/true'],
      ['ci', '.SHELLFLAGS=-c'],
      ['ci', 'GOFLAGS=-run=^$'],
      ['ci', 'GOWORK=off'],
      ['ci', 'GOOS=linux'],
      ['ci', 'GOARCH=wasm'],
      ['-n', 'ci'],
      ['-t', 'ci'],
      ['-q', 'ci'],
    ]) {
      expect(() => validateMake(arguments_), arguments_.join(' ')).toThrow()
    }

    for (const [name, value] of ([
      ['MAKEFLAGS', '-n'],
      ['MFLAGS', '-n'],
      ['GNUMAKEFLAGS', '-n'],
      ['MAKEFILES', 'injected.mk'],
      ['MAKEFLAGS', 'CI_GATES='],
      ['GOFLAGS', '-run=^$'],
      ['GOWORK', 'off'],
      ['GOOS', 'linux'],
      ['GOARCH', 'wasm'],
      ['GOENV', 'hostile-go.env'],
      ['GOTOOLCHAIN', 'auto'],
      ['GOROOT', 'hostile-root'],
    ] as const)) {
      expect(() => validateMake(['ci'], { [name]: value }), `${name}=${value}`).toThrow()
    }

    // actions/setup-go exports GOTOOLCHAIN=local on hosted runners; it equals
    // the owned Go default and must settle, while any other value fails closed.
    expect(() => validateMake(['ci'], { GOTOOLCHAIN: 'local' })).not.toThrow()

    expect(() => validateMake(['-f', 'Makefile', '-f', 'injected.mk', 'ci'])).toThrow()
  }, 20_000)
})

function validateMake(
  arguments_: readonly string[],
  environment: Readonly<Record<string, string>> = {},
) {
  // The parser is the public authority boundary for operands and inherited
  // controls; invoking it directly keeps semantic contracts process-free.
  return makeAuthorityModule.validateMakeInvocation(
    [...arguments_],
    { ...makeValidationEnvironment, ...environment },
  )
}

function cloneWorkflow(name: keyof WorkflowSources): WorkflowMapping {
  return structuredClone(parseWorkflowYaml(workflowSources[name]))
}

function job(workflow: WorkflowMapping, name: string): WorkflowMapping {
  return mappingField(mappingField(workflow, 'jobs'), name)
}

function commandOwner(workflow: WorkflowMapping, command: string): WorkflowMapping {
  const owners = [...mappingField(workflow, 'jobs').values()]
    .map((value) => mapping(value))
    .filter((candidate) => candidate.has('steps') && steps(candidate)
      .some((step) => step.get('run') === command))
  if (owners.length !== 1) throw new Error(`command owner fixture is ambiguous: ${command}`)
  return owners[0] as WorkflowMapping
}

function commandStep(workflow: WorkflowMapping, command: string): WorkflowMapping {
  return steps(commandOwner(workflow, command))
    .find((step) => step.get('run') === command) as WorkflowMapping
}

function checkoutStep(owner: WorkflowMapping): WorkflowMapping {
  const checkout = steps(owner).filter((step) =>
    typeof step.get('uses') === 'string' && String(step.get('uses')).startsWith('actions/checkout@'))
  if (checkout.length !== 1) throw new Error('checkout fixture is ambiguous')
  return checkout[0] as WorkflowMapping
}

function actionStep(owner: WorkflowMapping, prefix: string): WorkflowMapping {
  const matches = steps(owner).filter((step) =>
    typeof step.get('uses') === 'string' && String(step.get('uses')).startsWith(prefix))
  if (matches.length !== 1) throw new Error(`action fixture is ambiguous: ${prefix}`)
  return matches[0] as WorkflowMapping
}

function steps(owner: WorkflowMapping): WorkflowMapping[] {
  const value = field(owner, 'steps')
  if (!Array.isArray(value)) throw new Error('steps fixture must be a sequence')
  return value.map(mapping)
}

function mappingField(owner: WorkflowMapping, name: string): WorkflowMapping {
  return mapping(field(owner, name))
}

function field(owner: WorkflowMapping, name: string): WorkflowValue {
  if (!owner.has(name)) throw new Error(`fixture field ${name} is missing`)
  return owner.get(name) as WorkflowValue
}

function mapping(value: WorkflowValue): WorkflowMapping {
  if (!(value instanceof Map)) throw new Error('fixture value must be a mapping')
  return value
}

function repositoryFile(path: string): string {
  return readFileSync(resolve(REPOSITORY_ROOT, path), 'utf8')
}

function readFullBrowserOperationPlan(module: unknown): readonly string[] {
  if (module === null || typeof module !== 'object') throw new Error('orchestrator module is invalid')
  const factory = Reflect.get(module, 'localOperationPlan')
  if (typeof factory !== 'function') throw new Error('full browser operation plan factory is invalid')
  const operationPlan: unknown = Reflect.apply(factory, undefined, ['linux'])
  if (!Array.isArray(operationPlan) || !operationPlan.every((value) => typeof value === 'string')) {
    throw new Error('full browser operation plan is invalid')
  }
  return Object.freeze([...operationPlan])
}

function aliasDocument(referenceCount: number): string {
  return `base: &base { value: 1 }\nvalues: [${Array.from({ length: referenceCount }, () => '*base').join(', ')}]\n`
}
