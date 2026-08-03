import assert from 'node:assert/strict'

import {
  BROWSER_PROCESS_INTEGRATION_COMMAND,
  BROWSER_PROCESS_INTEGRATION_TARGET,
  FULL_GATES,
  GENERATED_SEMANTIC_PROCESS_COMMAND,
  GENERATED_SEMANTIC_PROCESS_TARGET,
  PLATFORM_ENTRYPOINTS,
  PR_GATES,
} from './workflow-policy.ts'
import { escapeRegExp } from './yaml-document.ts'

const WINDOWS_NETWORK_MAKE_COMMAND =
  '"$(WINDSHARE_PWSH_EXECUTABLE)" -NoLogo -NoProfile -NonInteractive -File scripts/ci/windows/browser-network.ps1'
const LINUX_NETWORK_MAKE_COMMAND =
  '"$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/browser-network.sh'

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
    'ifneq ($(filter default environment,$(origin SHELL)),$(origin SHELL))',
    'ifneq ($(origin SHELL):$(SHELL),file:/bin/sh)',
    'ifneq ($(origin .SHELLFLAGS),default)',
    'override SHELL := /bin/sh',
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

export function packageTargetCount(command: string, target: string): number {
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

export function literalCount(source: string, literal: string): number {
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

export function commandConsumesWeb(command: string): boolean {
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
