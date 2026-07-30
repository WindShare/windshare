import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { join, resolve } from 'node:path'

import {
  launchPlaywrightDiscovery,
  playwrightDiscoveryEnvironment,
  PLAYWRIGHT_DISCOVERY_COMMANDS,
} from '../../../../../web/playwright.discovery.ts'
import {
  PLAYWRIGHT_SUITE_PARTITIONS,
  PLAYWRIGHT_SUITES,
} from '../../../../../web/playwright.suite-config.ts'
import { suiteExecutionPlan } from '../../orchestrator.mjs'
import {
  assertDiscoveryEnvironmentBoundary,
  assertNoDiscoverySentinelOutput,
  assertWrapperRetired,
  environmentValue,
  HOSTILE_DISCOVERY_ENVIRONMENT,
  hostileDiscoveryHostEnvironment,
  observeWrapperConfig,
  verifyProductionConfigBoundary,
} from '../../testsupport/playwright-discovery.assertions.mjs'

const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const PLAYWRIGHT_WEB_ROOT = join(REPOSITORY_ROOT, 'web')
const DISCOVERY_CHILD_TIMEOUT_MS = 30_000

const [suite, ...unexpected] = process.argv.slice(2)
assert.equal(unexpected.length, 0, 'Playwright discovery integration accepts exactly one suite')
assert(PLAYWRIGHT_SUITES.includes(suite), 'Playwright discovery integration suite is invalid')

verifyProductionConfigBoundary(suiteExecutionPlan)
const hostEnvironment = hostileDiscoveryHostEnvironment()
const environment = playwrightDiscoveryEnvironment(hostEnvironment)
assertDiscoveryEnvironmentBoundary(environment)
if (suite === 'main') {
  verifyDiscoveryNodeChild(environment)
  verifyStaticDiscoveryModulesAreNotRunnable(hostEnvironment, suite)
  verifyRetiredWrapperCannotExecute(hostEnvironment)
}
verifyExactProductionPartition(hostEnvironment, suite)

process.stdout.write(`browsergate ${suite} Playwright discovery integration: PASS\n`)

function verifyStaticDiscoveryModulesAreNotRunnable(hostEnvironment, suiteName) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS.find((command) => command.suite === suiteName)
  assert.notEqual(canonical, undefined)
  const childEnvironment = playwrightDiscoveryEnvironment(hostEnvironment)
  for (const modulePath of [
    join(PLAYWRIGHT_WEB_ROOT, 'playwright.discovery.config.ts'),
    join(PLAYWRIGHT_WEB_ROOT, 'playwright.suite-config.ts'),
  ]) {
    const execution = spawnSync(canonical.executable, [
      canonical.arguments[0],
      'test',
      '--config',
      modulePath,
      '__windshare_discovery_config_must_not_execute__',
      '--pass-with-no-tests',
    ], {
      cwd: canonical.cwd,
      encoding: 'utf8',
      env: childEnvironment,
      timeout: DISCOVERY_CHILD_TIMEOUT_MS,
      windowsHide: true,
    })
    const diagnostic = childDiagnostic(execution)
    assertNoDiscoverySentinelOutput(diagnostic)
    assert.equal(execution.error, undefined, diagnostic)
    assert.notEqual(execution.status, 0, modulePath + ' became a runnable static config')
    assert.match(diagnostic, /config|export|property/u)
  }
}

function verifyExactProductionPartition(hostEnvironment, suiteName) {
  const discovered = new Map()
  let actualListProcesses = 0
  const commands = PLAYWRIGHT_DISCOVERY_COMMANDS.filter((command) => command.suite === suiteName)
  for (const command of commands) {
    assert.equal(command.arguments.filter((argument) => argument === '--list').length, 1)
    let wrapperConfig
    const execution = launchPlaywrightDiscovery(
      command,
      hostEnvironment,
      (executable, arguments_, options) => {
        actualListProcesses += 1
        wrapperConfig = observeWrapperConfig(arguments_)
        return spawnSync(executable, [...arguments_], options)
      },
    )
    assertWrapperRetired(wrapperConfig)
    const diagnostic = childDiagnostic(execution)
    assertNoDiscoverySentinelOutput(diagnostic)
    assert.equal(execution.error, undefined, diagnostic)
    assert.equal(execution.status, 0, diagnostic)
    discovered.set(
      command.partition,
      discoveryTestIdentities(command, execution.stdout, diagnostic),
    )
  }
  assert.equal(
    actualListProcesses,
    PLAYWRIGHT_SUITE_PARTITIONS.length,
    'discovery must issue one list-only process per requested suite partition',
  )

  const base = requiredDiscoverySet(discovered, suiteName, 'base')
  const focused = requiredDiscoverySet(discovered, suiteName, 'focused')
  const remainder = requiredDiscoverySet(discovered, suiteName, 'remainder')
  assert.equal(
    focused.size,
    3,
    suiteName + ' focused discovery must resolve one test for each production browser',
  )
  assert(remainder.size > 0, suiteName + ' remainder discovery cannot be empty')
  assert.deepEqual(
    [...focused].filter((identity) => remainder.has(identity)),
    [],
    suiteName + ' production focused and remainder partitions must be disjoint',
  )
  assert.deepEqual(
    [...new Set([...focused, ...remainder])].sort(),
    [...base].sort(),
    suiteName + ' discovery union must exactly equal the production partition',
  )
}

function verifyRetiredWrapperCannotExecute(hostEnvironment) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS[0]
  assert.notEqual(canonical, undefined)
  let retiredWrapper
  const prepared = launchPlaywrightDiscovery(
    canonical,
    hostEnvironment,
    (_executable, arguments_) => {
      retiredWrapper = observeWrapperConfig(arguments_)
      return { status: 0, stdout: '', stderr: '' }
    },
  )
  assert.equal(prepared.status, 0)
  assertWrapperRetired(retiredWrapper)

  const replay = spawnSync(canonical.executable, [
    canonical.arguments[0],
    'test',
    '--config',
    retiredWrapper,
    '__windshare_retired_wrapper_must_not_execute__',
    '--pass-with-no-tests',
  ], {
    cwd: canonical.cwd,
    encoding: 'utf8',
    env: playwrightDiscoveryEnvironment(hostEnvironment),
    timeout: DISCOVERY_CHILD_TIMEOUT_MS,
    windowsHide: true,
  })
  const diagnostic = childDiagnostic(replay)
  assertNoDiscoverySentinelOutput(diagnostic)
  assert.equal(replay.error, undefined, diagnostic)
  assert.notEqual(replay.status, 0, 'a retired wrapper remained executable')
}

function verifyDiscoveryNodeChild(environment) {
  const inspectedNames = [
    'PATH',
    'SYSTEMROOT',
    'TEMP',
    'TMP',
    ...Object.keys(HOSTILE_DISCOVERY_ENVIRONMENT),
  ]
  const program = [
    `const names = ${JSON.stringify(inspectedNames)}`,
    'const entries = Object.entries(process.env)',
    'const read = (name) => entries.find(([candidate]) => candidate.toUpperCase() === name)?.[1] ?? null',
    'process.stdout.write(JSON.stringify({ execPath: process.execPath, values: Object.fromEntries(names.map((name) => [name, read(name)])) }))',
  ].join(';')
  const child = spawnSync(process.execPath, ['--input-type=module', '--eval', program], {
    cwd: PLAYWRIGHT_WEB_ROOT,
    encoding: 'utf8',
    env: environment,
    timeout: DISCOVERY_CHILD_TIMEOUT_MS,
    windowsHide: true,
  })
  const diagnostic = childDiagnostic(child)
  assertNoDiscoverySentinelOutput(diagnostic)
  assert.equal(child.error, undefined, diagnostic)
  assert.equal(child.status, 0, diagnostic)
  const observed = JSON.parse(child.stdout)
  assert.equal(observed.execPath, process.execPath)
  assert.equal(observed.values.PATH, environmentValue(environment, 'PATH'))
  assert.equal(observed.values.TEMP, environmentValue(environment, 'TEMP'))
  assert.equal(observed.values.TMP, environmentValue(environment, 'TMP'))
  if (process.platform === 'win32') {
    assert.equal(observed.values.SYSTEMROOT, environmentValue(environment, 'SYSTEMROOT'))
  }
  for (const name of Object.keys(HOSTILE_DISCOVERY_ENVIRONMENT)) {
    assert.equal(observed.values[name], null, name + ' reached the discovery child')
  }
}

function discoveryTestIdentities(command, output, diagnostic) {
  const identities = new Set()
  const pattern = /^  \[discovery-(main|pion)-(base|focused|remainder)-(chromium|firefox|webkit)\] › (.+)$/gmu
  for (const match of output.matchAll(pattern)) {
    assert.equal(match[1], command.suite, diagnostic)
    assert.equal(match[2], command.partition, diagnostic)
    identities.add('[' + match[3] + '] › ' + match[4])
  }
  const total = /^Total: (\d+) tests? in (\d+) files?$/mu.exec(output)
  assert.notEqual(total, null, diagnostic)
  assert.equal(identities.size, Number(total?.[1]), diagnostic)
  assert(identities.size > 0, 'Playwright discovery returned zero tests\n' + diagnostic)
  return identities
}

function requiredDiscoverySet(discovered, suiteName, partition) {
  const value = discovered.get(partition)
  assert.notEqual(value, undefined, suiteName + '/' + partition + ' discovery is missing')
  return value
}

function childDiagnostic(execution) {
  return [execution.stdout, execution.stderr].filter(Boolean).join('\n')
}
