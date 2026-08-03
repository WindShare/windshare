import assert from 'node:assert/strict'

import {
  BROWSER_PROCESS_INTEGRATION_TARGET,
  GENERATED_SEMANTIC_PROCESS_TARGET,
  PLATFORM_ENTRYPOINTS,
} from './workflow-policy.ts'
import { escapeRegExp } from './yaml-document.ts'

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
