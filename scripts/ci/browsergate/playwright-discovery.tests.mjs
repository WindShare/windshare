import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  launchPlaywrightDiscovery,
  playwrightDiscoveryEnvironment,
  PLAYWRIGHT_DISCOVERY_COMMANDS,
} from '../../../web/playwright.discovery.ts'
import * as discoveryConfigFactory from '../../../web/playwright.discovery.config.ts'
import {
  focusedPlaywrightSpec,
  playwrightDiscoveryProjectName,
  PLAYWRIGHT_SUITE_PARTITIONS,
  PLAYWRIGHT_SUITES,
} from '../../../web/playwright.suite-config.ts'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const PLAYWRIGHT_WEB_ROOT = join(REPOSITORY_ROOT, 'web')
const HOSTILE_DISCOVERY_ENVIRONMENT = Object.freeze({
  ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'forbidden-actions-oidc-token-sentinel',
  ACTIONS_RUNTIME_TOKEN: 'forbidden-actions-runtime-token-sentinel',
  ALL_PROXY: 'http://forbidden-all-proxy-sentinel.invalid',
  AWS_SECRET_ACCESS_KEY: 'forbidden-aws-secret-sentinel',
  AWS_SESSION_TOKEN: 'forbidden-aws-session-sentinel',
  AZURE_CLIENT_SECRET: 'forbidden-azure-secret-sentinel',
  GH_TOKEN: 'forbidden-gh-token-sentinel',
  GITHUB_REPOSITORY: 'forbidden/repository-metadata-sentinel',
  GITHUB_TOKEN: 'forbidden-github-token-sentinel',
  GOOGLE_APPLICATION_CREDENTIALS: 'forbidden-google-credential-path-sentinel',
  HTTPS_PROXY: 'http://forbidden-https-proxy-sentinel.invalid',
  HTTP_PROXY: 'http://forbidden-http-proxy-sentinel.invalid',
  NODE_OPTIONS: '--require=forbidden-node-options-sentinel',
  NO_PROXY: 'forbidden-no-proxy-sentinel.invalid',
  REPOSITORY_SECRET: 'forbidden-repository-secret-sentinel',
  WINDSHARE_BROWSER_EVIDENCE_CONTEXT: 'forbidden-browser-context-sentinel',
  WINDSHARE_D5_E2E_LEASE_TOKEN: 'forbidden-d5-lease-sentinel',
  WINDSHARE_REPOSITORY_SECRET: 'forbidden-windshare-repository-secret-sentinel',
  WINDSHARE_R8_PERFORMANCE_SAMPLES: 'forbidden-performance-state-sentinel',
  WINDSHARE_WINDOWS_OS_NETWORK: 'forbidden-windows-network-authority-sentinel',
})
const HOSTILE_DISCOVERY_SENTINELS = Object.freeze(
  Object.values(HOSTILE_DISCOVERY_ENVIRONMENT),
)

export function verifyPlaywrightDiscoveryContract(productionPartitionPlan) {
  verifyProductionConfigBoundary(productionPartitionPlan)
  verifyDiscoveryConfigBoundary()
  verifyDiscoveryEnvironmentProjection()
  const hostEnvironment = hostileDiscoveryHostEnvironment()
  const environment = playwrightDiscoveryEnvironment(hostEnvironment)
  assertDiscoveryEnvironmentBoundary(environment)
  verifyPreSpawnDiscoveryBoundary(hostEnvironment)
  verifySnapshotBoundary(hostEnvironment)
  verifyWrapperFailureCleanup(hostEnvironment)
}

export function verifyPlaywrightDiscoveryIntegration(productionPartitionPlan, suite) {
  verifyProductionConfigBoundary(productionPartitionPlan)
  assert(PLAYWRIGHT_SUITES.includes(suite), 'Playwright discovery integration suite is invalid')
  const hostEnvironment = hostileDiscoveryHostEnvironment()
  const environment = playwrightDiscoveryEnvironment(hostEnvironment)
  assertDiscoveryEnvironmentBoundary(environment)
  if (suite === 'main') {
    verifyDiscoveryNodeChild(environment)
    verifyStaticDiscoveryModulesAreNotRunnable(hostEnvironment, suite)
  }
  verifyExactProductionPartition(hostEnvironment, [suite])
}

function verifyProductionConfigBoundary(productionPartitionPlan) {
  assert.equal(typeof productionPartitionPlan, 'function')
  const mainConfig = readFileSync(join(PLAYWRIGHT_WEB_ROOT, 'playwright.config.ts'), 'utf8')
  const pionConfig = readFileSync(
    join(PLAYWRIGHT_WEB_ROOT, 'test', 'transport', 'webrtc', 'browser.playwright.config.ts'),
    'utf8',
  )
  assert.match(mainConfig, /createPlaywrightSuiteDeclarations/u)
  assert.match(pionConfig, /createPlaywrightSuiteDeclarations/u)
  assert(!mainConfig.includes('playwright.discovery'))
  assert.match(mainConfig, /process\.platform === 'win32'/u)

  const remainderSources = {
    main: readFileSync(join(PLAYWRIGHT_WEB_ROOT, 'playwright.remainder.config.ts'), 'utf8'),
    pion: readFileSync(
      join(
        PLAYWRIGHT_WEB_ROOT,
        'test',
        'transport',
        'webrtc',
        'browser.remainder.playwright.config.ts',
      ),
      'utf8',
    ),
  }
  for (const suite of PLAYWRIGHT_SUITES) {
    const focusedSpec = focusedPlaywrightSpec(suite)
    assert.equal(productionPartitionPlan(suite).focused.specPath, focusedSpec)
    assert(
      remainderSources[suite].includes(`testIgnore: ['${focusedSpec}']`),
      suite + ' production remainder does not exclude its focused spec',
    )
  }
}

function verifyDiscoveryConfigBoundary() {
  assert.deepEqual(Object.keys(discoveryConfigFactory), ['createPlaywrightDiscoveryConfig'])
  const config = discoveryConfigFactory.createPlaywrightDiscoveryConfig()
  assert.deepEqual(Object.keys(config), ['projects'])
  const projects = config.projects ?? []
  const expectedNames = PLAYWRIGHT_SUITES.flatMap((suite) =>
    PLAYWRIGHT_SUITE_PARTITIONS.flatMap((partition) =>
      ['chromium', 'firefox', 'webkit'].map((browser) =>
        playwrightDiscoveryProjectName(suite, partition, browser))))
  assert.deepEqual(projects.map((project) => project.name), expectedNames)
  assert.equal(projects.length, 18)
  const allowedProjectFields = new Set(['name', 'testDir', 'testMatch', 'testIgnore'])
  for (const project of projects) {
    assert.deepEqual(
      Object.keys(project).filter((name) => !allowedProjectFields.has(name)),
      [],
      project.name + ' discovery project inherited product-execution semantics',
    )
  }
}

function verifyStaticDiscoveryModulesAreNotRunnable(hostEnvironment, suite) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS.find((command) => command.suite === suite)
  assert.notEqual(canonical, undefined)
  const environment = playwrightDiscoveryEnvironment(hostEnvironment)
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
      env: environment,
      timeout: 30_000,
      windowsHide: true,
    })
    const diagnostic = [execution.stdout, execution.stderr].filter(Boolean).join('\n')
    assertNoDiscoverySentinelOutput(diagnostic)
    assert.equal(execution.error, undefined, diagnostic)
    assert.notEqual(execution.status, 0, modulePath + ' became a runnable static config')
    assert.match(diagnostic, /config|export|property/u)
  }
}

function verifyExactProductionPartition(hostEnvironment, suites) {
  const discovered = new Map()
  let actualListProcesses = 0
  const commands = PLAYWRIGHT_DISCOVERY_COMMANDS.filter((command) => suites.includes(command.suite))
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
    const diagnostic = [execution.stdout, execution.stderr].filter(Boolean).join('\n')
    assertNoDiscoverySentinelOutput(diagnostic)
    assert.equal(execution.error, undefined, diagnostic)
    assert.equal(execution.status, 0, diagnostic)
    discovered.set(
      command.suite + '/' + command.partition,
      discoveryTestIdentities(command, execution.stdout, diagnostic),
    )
  }
  assert.equal(
    actualListProcesses,
    suites.length * PLAYWRIGHT_SUITE_PARTITIONS.length,
    'discovery must issue one list-only process per requested suite partition',
  )

  for (const suite of suites) {
    const base = requiredDiscoverySet(discovered, suite, 'base')
    const focused = requiredDiscoverySet(discovered, suite, 'focused')
    const remainder = requiredDiscoverySet(discovered, suite, 'remainder')
    assert.equal(
      focused.size,
      3,
      suite + ' focused discovery must resolve one test for each production browser',
    )
    assert(remainder.size > 0, suite + ' remainder discovery cannot be empty')
    assert.deepEqual(
      [...focused].filter((identity) => remainder.has(identity)),
      [],
      suite + ' production focused and remainder partitions must be disjoint',
    )
    assert.deepEqual(
      [...new Set([...focused, ...remainder])].sort(),
      [...base].sort(),
      suite + ' discovery union must exactly equal the production partition',
    )
  }
}

function verifyPreSpawnDiscoveryBoundary(hostEnvironment) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS[0]
  assert.notEqual(canonical, undefined)

  let canonicalLaunches = 0
  let observedEnvironment
  let retiredWrapper
  const injectedSuccess = launchPlaywrightDiscovery(
    canonical,
    hostEnvironment,
    (_executable, arguments_, options) => {
      canonicalLaunches += 1
      assert(Object.isFrozen(options), 'launcher must freeze the final spawn options')
      assert(Object.isFrozen(options.env), 'launcher must freeze the projected child environment')
      observedEnvironment = options.env
      retiredWrapper = observeWrapperConfig(arguments_)
      return { status: 0, stdout: '', stderr: '' }
    },
  )
  assert.equal(injectedSuccess.status, 0)
  assert.equal(canonicalLaunches, 1)
  assertDiscoveryEnvironmentBoundary(observedEnvironment)
  assertWrapperRetired(retiredWrapper)

  let rejectedSpawnCalls = 0
  const rejectedSpawn = () => {
    rejectedSpawnCalls += 1
    return { status: 0, stdout: '', stderr: '' }
  }
  for (const [label, command] of hostileDiscoveryCommands(canonical)) {
    assert.throws(
      () => launchPlaywrightDiscovery(command, hostEnvironment, rejectedSpawn),
      /Playwright discovery|Node executable|working directory|Playwright CLI|plain array/u,
      label + ' reached spawn instead of failing at the issuer boundary',
    )
  }
  verifySymlinkDiscoveryRejection(canonical, hostEnvironment, rejectedSpawn)
  const replayCommand = withArguments(canonical, [
    ...canonical.arguments.slice(0, 2),
    '--config',
    retiredWrapper,
    ...canonical.arguments.slice(2),
  ])
  assert.throws(
    () => launchPlaywrightDiscovery(replayCommand, hostEnvironment, rejectedSpawn),
    /six list-only commands/u,
  )
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
    timeout: 30_000,
    windowsHide: true,
  })
  const replayDiagnostic = [replay.stdout, replay.stderr].filter(Boolean).join('\n')
  assertNoDiscoverySentinelOutput(replayDiagnostic)
  assert.equal(replay.error, undefined, replayDiagnostic)
  assert.notEqual(replay.status, 0, 'a retired wrapper remained executable')
  assert.equal(rejectedSpawnCalls, 0, 'invalid discovery spellings must be rejected pre-spawn')
}

function verifySnapshotBoundary(hostEnvironment) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS[0]
  assert.notEqual(canonical, undefined)
  let invalidSpawnCalls = 0
  const neverSpawn = () => {
    invalidSpawnCalls += 1
    return { status: 0, stdout: '', stderr: '' }
  }

  let getterCalls = 0
  const accessorCommand = { ...canonical, arguments: [...canonical.arguments] }
  Object.defineProperty(accessorCommand, 'executable', {
    configurable: true,
    enumerable: true,
    get() {
      getterCalls += 1
      return fileURLToPath(import.meta.url)
    },
  })
  assert.throws(
    () => launchPlaywrightDiscovery(accessorCommand, hostEnvironment, neverSpawn),
    /accessors are forbidden/u,
  )
  assert.equal(getterCalls, 0, 'command snapshot evaluated an executable getter')

  let argumentGetterCalls = 0
  const accessorArguments = [...canonical.arguments]
  Object.defineProperty(accessorArguments, '0', {
    configurable: true,
    enumerable: true,
    get() {
      argumentGetterCalls += 1
      return fileURLToPath(import.meta.url)
    },
  })
  assert.throws(
    () => launchPlaywrightDiscovery(
      { ...canonical, arguments: accessorArguments },
      hostEnvironment,
      neverSpawn,
    ),
    /accessors are forbidden/u,
  )
  assert.equal(argumentGetterCalls, 0, 'command snapshot evaluated an argv getter')

  let proxyTrapCalls = 0
  const hostileProxy = new Proxy(
    { ...canonical, arguments: [...canonical.arguments] },
    {
      ownKeys() {
        proxyTrapCalls += 1
        throw new Error('hostile ownKeys trap executed')
      },
    },
  )
  assert.throws(
    () => launchPlaywrightDiscovery(hostileProxy, hostEnvironment, neverSpawn),
    /must not be a Proxy/u,
  )
  assert.equal(proxyTrapCalls, 0, 'Proxy traps ran before rejection')
  assert.throws(
    () => launchPlaywrightDiscovery(
      { ...canonical, arguments: new Proxy([...canonical.arguments], {}) },
      hostEnvironment,
      neverSpawn,
    ),
    /must not be a Proxy/u,
  )

  class NonPlainCommand {}
  assert.throws(
    () => launchPlaywrightDiscovery(
      Object.assign(new NonPlainCommand(), canonical),
      hostEnvironment,
      neverSpawn,
    ),
    /plain object/u,
  )
  assert.equal(invalidSpawnCalls, 0, 'invalid snapshot input reached spawn')

  const mutableCommand = { ...canonical, arguments: [...canonical.arguments] }
  let snapshotWrapper
  let snapshotSpawnCalls = 0
  launchPlaywrightDiscovery(mutableCommand, hostEnvironment, (executable, arguments_) => {
    snapshotSpawnCalls += 1
    mutableCommand.executable = fileURLToPath(import.meta.url)
    mutableCommand.arguments[0] = fileURLToPath(import.meta.url)
    assert.equal(executable, canonical.executable)
    assert.equal(arguments_[0], canonical.arguments[0])
    assert(Object.isFrozen(arguments_), 'spawn argv must be an immutable snapshot')
    snapshotWrapper = observeWrapperConfig(arguments_)
    return { status: 0, stdout: '', stderr: '' }
  })
  assert.equal(snapshotSpawnCalls, 1)
  assertWrapperRetired(snapshotWrapper)
}

function verifyWrapperFailureCleanup(hostEnvironment) {
  const canonical = PLAYWRIGHT_DISCOVERY_COMMANDS[0]
  assert.notEqual(canonical, undefined)

  let failedWrapper
  const failed = launchPlaywrightDiscovery(canonical, hostEnvironment, (_executable, arguments_) => {
    failedWrapper = observeWrapperConfig(arguments_)
    writeFileSync(join(dirname(failedWrapper), 'child-residue.txt'), 'residue', 'utf8')
    return { status: 7, stdout: '', stderr: 'injected child failure' }
  })
  assert.equal(failed.status, 7)
  assertWrapperRetired(failedWrapper)

  let timedOutWrapper
  const timedOut = launchPlaywrightDiscovery(canonical, hostEnvironment, (_executable, arguments_) => {
    timedOutWrapper = observeWrapperConfig(arguments_)
    writeFileSync(join(dirname(timedOutWrapper), 'timeout-residue.txt'), 'residue', 'utf8')
    return {
      status: null,
      stdout: '',
      stderr: 'injected timeout',
      error: Object.assign(new Error('injected timeout'), { code: 'ETIMEDOUT' }),
    }
  })
  assert.equal(timedOut.status, null)
  assert.equal(timedOut.error?.code, 'ETIMEDOUT')
  assertWrapperRetired(timedOutWrapper)

  let throwingWrapper
  assert.throws(
    () => launchPlaywrightDiscovery(canonical, hostEnvironment, (_executable, arguments_) => {
      throwingWrapper = observeWrapperConfig(arguments_)
      writeFileSync(join(dirname(throwingWrapper), 'throw-residue.txt'), 'residue', 'utf8')
      throw new Error('injected spawn exception')
    }),
    /injected spawn exception/u,
  )
  assertWrapperRetired(throwingWrapper)
  assert.equal(
    new Set([failedWrapper, timedOutWrapper, throwingWrapper].map(dirname)).size,
    3,
    'each discovery launch must receive a fresh private directory',
  )
}

function hostileDiscoveryCommands(canonical) {
  const arguments_ = canonical.arguments
  const runtimeRealpath = realpathSync.native(canonical.executable)
  const cliRealpath = realpathSync.native(arguments_[0])
  const cases = [
    ['missing list', withArguments(canonical, arguments_.slice(0, -1))],
    ['ui mode', withArguments(canonical, [...arguments_.slice(0, -1), '--ui', '--list'])],
    ['ui equals mode', withArguments(canonical, [...arguments_.slice(0, -1), '--ui=hosted', '--list'])],
    ['ui host', withArguments(canonical, [...arguments_.slice(0, -1), '--ui-host', '127.0.0.1', '--list'])],
    ['ui host equals', withArguments(canonical, [...arguments_.slice(0, -1), '--ui-host=127.0.0.1', '--list'])],
    ['ui port', withArguments(canonical, [...arguments_.slice(0, -1), '--ui-port', '0', '--list'])],
    ['ui port equals', withArguments(canonical, [...arguments_.slice(0, -1), '--ui-port=0', '--list'])],
    ['mixed debug mode', withArguments(canonical, [...arguments_.slice(0, -1), '--debug', '--list'])],
    ['delimiter list', withArguments(canonical, [...arguments_.slice(0, -1), '--', '--list'])],
    ['positional list', withArguments(canonical, replaceArgument(arguments_, 2, '--list'))],
    ['duplicate list', withArguments(canonical, [...arguments_, '--list'])],
    ['list false', withArguments(canonical, replaceArgument(arguments_, 5, '--list=false'))],
    ['unexpected headed option', withArguments(canonical, [...arguments_, '--headed'])],
    ['unexpected worker count', withArguments(canonical, replaceArgument(arguments_, 3, '--workers=2'))],
    ['unexpected retry count', withArguments(canonical, replaceArgument(arguments_, 4, '--retries=1'))],
    ['unexpected project', withArguments(canonical, replaceArgument(arguments_, 2, '--project=unknown'))],
    ['unexpected command', withArguments(canonical, replaceArgument(arguments_, 1, 'show-report'))],
    ['alternate static config', withArguments(canonical, [
      ...arguments_.slice(0, 2),
      '--config',
      'playwright.config.ts',
      ...arguments_.slice(2),
    ])],
    ['alternate static config equals', withArguments(canonical, [
      ...arguments_.slice(0, 2),
      '--config=playwright.config.ts',
      ...arguments_.slice(2),
    ])],
    ['navigation static config', withArguments(canonical, [
      ...arguments_.slice(0, 2),
      '--config',
      'test/../playwright.config.ts',
      ...arguments_.slice(2),
    ])],
    ['navigation executable', {
      ...canonical,
      executable: rawCurrentDirectorySpelling(canonical.executable),
    }],
    ['navigation CLI', withArguments(canonical, replaceArgument(
      arguments_,
      0,
      rawCurrentDirectorySpelling(arguments_[0]),
    ))],
    ['navigation cwd', { ...canonical, cwd: canonical.cwd + sep + '.' }],
    ['alien absolute Node', { ...canonical, executable: fileURLToPath(import.meta.url) }],
    ['option in CLI slot', withArguments(canonical, replaceArgument(arguments_, 0, '--ui'))],
    ['unexpected CLI', withArguments(canonical, replaceArgument(
      arguments_,
      0,
      canonical.executable,
    ))],
    ['alien same-suffix CLI', withArguments(canonical, replaceArgument(
      arguments_,
      0,
      join(REPOSITORY_ROOT, 'alien', 'node_modules', '@playwright', 'test', 'cli.js'),
    ))],
  ]
  if (runtimeRealpath !== canonical.executable) {
    cases.push(['alternate runtime realpath', { ...canonical, executable: runtimeRealpath }])
  }
  if (cliRealpath !== arguments_[0]) {
    cases.push(['alternate CLI realpath', withArguments(
      canonical,
      replaceArgument(arguments_, 0, cliRealpath),
    )])
  }
  if (process.platform === 'win32') {
    cases.push(['alternate Windows executable spelling', {
      ...canonical,
      executable: equivalentWindowsPath(canonical.executable),
    }])
    cases.push(['alternate Windows CLI spelling', withArguments(
      canonical,
      replaceArgument(arguments_, 0, equivalentWindowsPath(arguments_[0])),
    )])
    cases.push(['alternate Windows cwd spelling', {
      ...canonical,
      cwd: equivalentWindowsPath(canonical.cwd),
    }])
  }
  return cases
}
function verifySymlinkDiscoveryRejection(canonical, hostEnvironment, rejectedSpawn) {
  const temporaryRoot = resolve(mkdtempSync(join(tmpdir(), 'windshare-discovery-symlink-')))
  try {
    const webAlias = join(temporaryRoot, 'web-alias')
    symlinkSync(
      canonical.cwd,
      webAlias,
      process.platform === 'win32' ? 'junction' : 'dir',
    )
    const comparableRealpath = (value) => process.platform === 'win32'
      ? realpathSync.native(value).toLowerCase()
      : realpathSync.native(value)
    assert.equal(comparableRealpath(webAlias), comparableRealpath(canonical.cwd))

    assert.throws(
      () => launchPlaywrightDiscovery(
        { ...canonical, cwd: webAlias },
        hostEnvironment,
        rejectedSpawn,
      ),
      /working directory/u,
      'a symlinked working directory reached spawn',
    )
    const cliAlias = webAlias + canonical.arguments[0].slice(canonical.cwd.length)
    assert.equal(comparableRealpath(cliAlias), comparableRealpath(canonical.arguments[0]))
    assert.throws(
      () => launchPlaywrightDiscovery(
        withArguments(canonical, replaceArgument(canonical.arguments, 0, cliAlias)),
        hostEnvironment,
        rejectedSpawn,
      ),
      /Playwright CLI|argv/u,
      'a symlinked Playwright CLI reached spawn',
    )
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true })
  }
}

function verifyDiscoveryEnvironmentProjection() {
  const projected = playwrightDiscoveryEnvironment({
    HOME: '/synthetic/home',
    PATH: '/synthetic/bin',
    PLAYWRIGHT_BROWSERS_PATH: '/synthetic/browsers',
    SYSTEMROOT: 'C:\\SyntheticWindows',
    TEMP: '/synthetic/temp',
    TMP: '/synthetic/tmp',
    ...HOSTILE_DISCOVERY_ENVIRONMENT,
  })
  assert.deepEqual(projected, {
    HOME: '/synthetic/home',
    PATH: '/synthetic/bin',
    PLAYWRIGHT_BROWSERS_PATH: '/synthetic/browsers',
    SYSTEMROOT: 'C:\\SyntheticWindows',
    TEMP: '/synthetic/temp',
    TMP: '/synthetic/tmp',
  })
  assertDiscoveryEnvironmentBoundary(projected)

  const windowsCasing = playwrightDiscoveryEnvironment({
    Path: 'C:\\SyntheticTools',
    SystemRoot: 'C:\\SyntheticWindows',
    Temp: 'C:\\SyntheticTemp',
    Tmp: 'C:\\SyntheticTmp',
  })
  assert.equal(environmentValue(windowsCasing, 'PATH'), 'C:\\SyntheticTools')
  assert.equal(environmentValue(windowsCasing, 'SYSTEMROOT'), 'C:\\SyntheticWindows')
  assert.equal(environmentValue(windowsCasing, 'TEMP'), 'C:\\SyntheticTemp')
  assert.equal(environmentValue(windowsCasing, 'TMP'), 'C:\\SyntheticTmp')
}

function hostileDiscoveryHostEnvironment() {
  const hostPath = environmentValue(process.env, 'PATH')
  assert.notEqual(hostPath, undefined, 'Node discovery requires an inherited executable path')
  const hostSystemRoot = environmentValue(process.env, 'SYSTEMROOT')
  if (process.platform === 'win32') {
    assert.notEqual(hostSystemRoot, undefined, 'Windows Node discovery requires SystemRoot')
  }
  return {
    ...process.env,
    PATH: hostPath,
    SYSTEMROOT: hostSystemRoot,
    TEMP: environmentValue(process.env, 'TEMP') ?? tmpdir(),
    TMP: environmentValue(process.env, 'TMP') ?? tmpdir(),
    ...HOSTILE_DISCOVERY_ENVIRONMENT,
  }
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
    timeout: 30_000,
    windowsHide: true,
  })
  const diagnostic = [child.stdout, child.stderr].filter(Boolean).join('\n')
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

function requiredDiscoverySet(discovered, suite, partition) {
  const value = discovered.get(suite + '/' + partition)
  assert.notEqual(value, undefined, suite + '/' + partition + ' discovery is missing')
  return value
}

function observeWrapperConfig(arguments_) {
  assert(Object.isFrozen(arguments_), 'launcher must freeze the final spawn argv')
  assert.equal(arguments_.length, 8)
  assert.equal(arguments_[1], 'test')
  assert.equal(arguments_[2], '--config')
  assert.equal(arguments_[7], '--list')
  const wrapperConfig = arguments_[3]
  assert.equal(typeof wrapperConfig, 'string')
  assert.equal(wrapperConfig.endsWith('playwright.config.mts'), true)
  assert(isAbsolute(wrapperConfig), 'one-shot wrapper path must be absolute')
  const repositoryRelative = relative(REPOSITORY_ROOT, wrapperConfig)
  assert(
    isAbsolute(repositoryRelative) ||
      repositoryRelative === '..' ||
      repositoryRelative.startsWith('..' + sep),
    'one-shot wrapper must be outside the repository',
  )
  assert(existsSync(wrapperConfig), 'one-shot wrapper must exist only while the child is active')
  const source = readFileSync(wrapperConfig, 'utf8')
  const lines = source.trimEnd().split(/\r?\n/u)
  assert.equal(lines.length, 2)
  const importMatch = /^import \{ createPlaywrightDiscoveryConfig \} from (.+)$/u.exec(lines[0])
  assert.notEqual(importMatch, null)
  const importedFactory = JSON.parse(importMatch[1])
  assert.equal(
    resolve(fileURLToPath(importedFactory)),
    join(PLAYWRIGHT_WEB_ROOT, 'playwright.discovery.config.ts'),
  )
  assert.equal(lines[1], 'export default createPlaywrightDiscoveryConfig()')
  return wrapperConfig
}

function assertWrapperRetired(wrapperConfig) {
  assert.equal(typeof wrapperConfig, 'string')
  assert.equal(existsSync(wrapperConfig), false, 'one-shot wrapper survived child settlement')
  assert.equal(existsSync(dirname(wrapperConfig)), false, 'private wrapper directory survived cleanup')
  assert.throws(
    () => readFileSync(wrapperConfig, 'utf8'),
    (cause) => cause instanceof Error && cause.code === 'ENOENT',
    'retired wrapper remained replayable',
  )
}

function withArguments(command, arguments_) {
  return { ...command, arguments: arguments_ }
}

function replaceArgument(arguments_, index, replacement) {
  return arguments_.map((argument, argumentIndex) =>
    argumentIndex === index ? replacement : argument)
}

function rawCurrentDirectorySpelling(value) {
  const separatorIndex = value.lastIndexOf(sep)
  assert(separatorIndex >= 0, 'absolute path lacks a platform separator')
  return value.slice(0, separatorIndex + 1) + '.' + sep + value.slice(separatorIndex + 1)
}

function equivalentWindowsPath(value) {
  const portable = value.replaceAll('\\', '/')
  return /^[A-Z]:/u.test(portable)
    ? portable[0].toLowerCase() + portable.slice(1)
    : portable
}

function assertDiscoveryEnvironmentBoundary(environment) {
  assert.notEqual(environment, undefined)
  for (const [name, sentinel] of Object.entries(HOSTILE_DISCOVERY_ENVIRONMENT)) {
    assert.equal(environmentValue(environment, name), undefined, name + ' entered discovery')
    assert(!Object.values(environment).includes(sentinel), name + ' sentinel entered discovery')
  }
}

function assertNoDiscoverySentinelOutput(output) {
  for (const sentinel of HOSTILE_DISCOVERY_SENTINELS) {
    assert(!output.includes(sentinel), 'a hostile environment sentinel reached discovery output')
  }
}

function environmentValue(environment, canonicalName) {
  const entry = Object.entries(environment)
    .find(([name]) => name.toUpperCase() === canonicalName)
  return entry?.[1]
}
