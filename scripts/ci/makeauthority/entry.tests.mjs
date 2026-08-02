import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { delimiter, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  createRetainedMakefileAuthority,
  createRetainedProtectedPathAuthority,
  runMake,
  validateMakeInvocation,
} from './entry.mjs'

const forbiddenEnvironment = new Set([
  'MAKEFLAGS', 'MFLAGS', 'GNUMAKEFLAGS', 'MAKEFILES', 'MAKEOVERRIDES',
  'MAKE_RESTARTS', 'MAKELEVEL', 'MAKESHELL', 'BASH_ENV', 'ENV',
  'GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT',
  'BROWSER_NETWORK_COMPLETION',
])
const cleanEnvironment = Object.fromEntries(Object.entries(process.env).filter(([name]) => {
  const folded = name.toUpperCase()
  return !forbiddenEnvironment.has(folded) &&
    !folded.startsWith('WINDSHARE_MAKE') && !folded.startsWith('WINDSHARE_GIT') &&
    !['WINDSHARE_HOST_GOOS', 'WINDSHARE_CORE_ARTIFACT_COMMIT_SHA',
      'WINDSHARE_RECIPE_SHELL', 'WINDSHARE_BASH_EXECUTABLE', 'WINDSHARE_PWSH_EXECUTABLE'].includes(folded)
}))

// The real host environment is a Node-owned exotic object on Windows. Its exact
// identity is accepted, while all attacker-supplied records below remain inert.
assert.deepEqual(validateMakeInvocation(['integration']).targets, ['integration'])

const fixture = mkdtempSync(join(tmpdir(), 'windshare-make-authority-'))
const completionPath = resolve('test-results/browser-network-completion.json')
const completionExisted = existsSync(completionPath)
try {
  const missingCompletion = resolve(fixture, 'missing-completion.json')
  assert.throws(
    () => validateMakeInvocation([
      'browser-network',
      `BROWSER_NETWORK_COMPLETION=${missingCompletion}`,
    ], cleanEnvironment),
    /ENOENT|no such file|cannot find/u,
  )

  mkdirSync(resolve('test-results'), { recursive: true })
  if (!completionExisted) writeFileSync(completionPath, '{}\n')

  const validated = validateMakeInvocation([
    'browser-network',
    `BROWSER_NETWORK_COMPLETION=${completionPath}`,
  ], cleanEnvironment)
  assert.deepEqual(validated.targets, ['browser-network'])
  assert.deepEqual(validated.assignments, [
    ['BROWSER_NETWORK_COMPLETION', completionPath],
  ])
  assert.ok(Object.isFrozen(validated) && Object.isFrozen(validated.assignments))
  assert.ok(validated.assignments.every((entry) => Object.isFrozen(entry)))
  assert.throws(() => { validated.assignments[0][1] = 'changed' }, TypeError)

  assert.throws(
    () => validateMakeInvocation(['browser-network'], cleanEnvironment),
    /must exactly match/u,
  )
  assert.throws(
    () => validateMakeInvocation([
      'integration',
      `BROWSER_NETWORK_COMPLETION=${completionPath}`,
    ], cleanEnvironment),
    /must exactly match/u,
  )

  for (const option of [
    '-n', '-t', '-q', '-i', '--eval=integration: ; @:', '-f', 'evil.mk',
  ]) {
    assert.throws(
      () => validateMakeInvocation(['integration', option], cleanEnvironment),
      /forbidden|unsupported/u,
      option,
    )
  }
  for (const assignment of ['MFLAGS=', 'MAKEFLAGS=', 'GNUMAKEFLAGS=', 'SHELL=sh', 'GOFLAGS=-run=^$']) {
    assert.throws(
      () => validateMakeInvocation(['integration', assignment], cleanEnvironment),
      /unsupported/u,
      assignment,
    )
  }

  for (const name of [
    'makeFlags', 'mAkElEvEl', 'gOfLaGs', 'gOwOrK', 'gOoS', 'gOaRcH', 'gOeNv',
    'gOtOoLcHaIn', 'gOrOoT', 'WindShare_Retained_Makefile', 'browser_network_completion',
  ]) {
    assert.throws(
      () => validateMakeInvocation(['integration'], { ...cleanEnvironment, [name]: 'hostile' }),
      /must be absent|explicit protected operand/u,
      name,
    )
  }
  assert.throws(
    () => validateMakeInvocation(['integration'], { Path: 'a', PATH: 'b' }),
    /case-insensitive duplicate/u,
  )
  assert.throws(
    () => validateMakeInvocation(['integration'], Object.create({ PATH: 'active' })),
    /inert data record/u,
  )

  let traps = 0
  const environmentProxy = new Proxy({}, { ownKeys() { traps += 1; return [] } })
  assert.throws(() => validateMakeInvocation(['integration'], environmentProxy), /inert data record/u)
  assert.equal(traps, 0)
  const revokedEnvironment = Proxy.revocable({}, {})
  revokedEnvironment.revoke()
  assert.throws(() => validateMakeInvocation(['integration'], revokedEnvironment.proxy), /inert data record/u)

  const getterEnvironment = {}
  Object.defineProperty(getterEnvironment, 'PATH', {
    enumerable: true,
    get() { traps += 1; return 'active' },
  })
  assert.throws(() => validateMakeInvocation(['integration'], getterEnvironment), /active or invalid/u)
  assert.equal(traps, 0)
  const symbolEnvironment = { PATH: 'inert' }
  symbolEnvironment[Symbol('hostile')] = 'active'
  assert.throws(() => validateMakeInvocation(['integration'], symbolEnvironment), /symbol entries/u)

  const argumentProxy = new Proxy(['integration'], { get() { traps += 1; return undefined } })
  assert.throws(() => validateMakeInvocation(argumentProxy, cleanEnvironment), /inert array/u)
  assert.equal(traps, 0)
  const revokedArguments = Proxy.revocable(['integration'], {})
  revokedArguments.revoke()
  assert.throws(() => validateMakeInvocation(revokedArguments.proxy, cleanEnvironment), /inert array/u)
  const getterArguments = []
  Object.defineProperty(getterArguments, '0', {
    enumerable: true,
    get() { traps += 1; return 'integration' },
  })
  assert.throws(() => validateMakeInvocation(getterArguments, cleanEnvironment), /inert strings/u)
  assert.equal(traps, 0)
  const symbolArguments = ['integration']
  symbolArguments[Symbol('hostile')] = 'active'
  assert.throws(() => validateMakeInvocation(symbolArguments, cleanEnvironment), /symbol fields/u)
  const decoratedArguments = ['integration']
  Object.defineProperty(decoratedArguments, 'hidden', { value: 'active' })
  assert.throws(() => validateMakeInvocation(decoratedArguments, cleanEnvironment), /at least one explicit target/u)

  const mutableArguments = ['integration']
  const mutableEnvironment = { PATH: 'original' }
  const immutableValidation = validateMakeInvocation(mutableArguments, mutableEnvironment)
  mutableArguments[0] = 'hygiene'
  mutableEnvironment.PATH = 'changed'
  assert.deepEqual(immutableValidation.targets, ['integration'])

  for (const payload of ['"', "'", '`', '$()', '$value', '\n']) {
    assert.throws(
      () => validateMakeInvocation([
        'browser-network',
        `BROWSER_NETWORK_COMPLETION=${completionPath}${payload}`,
      ], cleanEnvironment),
      /injection-safe/u,
      JSON.stringify(payload),
    )
  }

  if (process.platform === 'win32') {
    const oddRepository = resolve(fixture, 'workspace with $ and #')
    mkdirSync(join(oddRepository, '.git'), { recursive: true })
    const makefileAuthority = createRetainedMakefileAuthority(Buffer.from('probe:\n\t@:\n'), oddRepository)
    try {
      assert.match(makefileAuthority.path, /^\.git\/[a-zA-Z0-9_-]+\/Makefile$/u)
      assert.doesNotMatch(makefileAuthority.path, /[\s$#]/u)
    } finally {
      makefileAuthority.close()
    }
  } else if (process.platform === 'linux') {
    const protectedAuthority = createRetainedProtectedPathAuthority(validated.assignments)
    try {
      const retainedCompletion = protectedAuthority.assignments[0][1]
      if (!completionExisted) {
        renameSync(completionPath, `${completionPath}.original`)
        writeFileSync(completionPath, '{"replacement":true}\n')
        assert.equal(new TextDecoder().decode(readFileSync(retainedCompletion)), '{}\n')
        rmSync(completionPath)
        renameSync(`${completionPath}.original`, completionPath)
      } else {
        assert.deepEqual(readFileSync(retainedCompletion), readFileSync(completionPath))
      }
    } finally {
      protectedAuthority.close()
    }
  }

  const marker = resolve(fixture, 'hostile-recipe-ran')
  const evilMakefile = resolve(fixture, 'evil.mk')
  writeFileSync(evilMakefile, `integration:\n\t@echo hostile > "${marker}"\n`)
  for (const hostileArguments of [
    ['integration', '-f', evilMakefile],
    ['integration', `--eval=integration: ; @echo hostile > "${marker}"`],
  ]) {
    const rejected = runEntry(hostileArguments, cleanEnvironment)
    assert.equal(rejected.status, 2, rejected.stderr)
    assert.equal(existsSync(marker), false, 'hostile Make recipe must never execute')
    assert.deepEqual(JSON.parse(rejected.stderr.trim()), {
      failureCode: 'validation-make-authority-failed',
    })
  }

  const fakeDirectory = resolve(fixture, 'fake-path')
  mkdirSync(fakeDirectory)
  const fakeScript = process.platform === 'win32'
    ? resolve(fakeDirectory, 'make.cmd')
    : resolve(fakeDirectory, 'make')
  writeFileSync(fakeScript, process.platform === 'win32' ? '@exit /b 0\r\n' : '#!/bin/sh\nexit 0\n', {
    mode: 0o755,
  })
  const fakeScriptResult = runEntry(['plan-browser'], {
    ...cleanEnvironment,
    PATH: `${fakeDirectory}${delimiter}${cleanEnvironment.PATH}`,
  })
  assert.notEqual(fakeScriptResult.status, 0, fakeScriptResult.stdout)

  const fakeNative = process.platform === 'win32'
    ? resolve(fakeDirectory, 'make.exe')
    : fakeScript
  copyFileSync(process.execPath, fakeNative)
  const fakeNativeResult = runEntry(['plan-browser'], {
    ...cleanEnvironment,
    PATH: `${fakeDirectory}${delimiter}${cleanEnvironment.PATH}`,
  })
  assert.notEqual(fakeNativeResult.status, 0, fakeNativeResult.stdout)

  if (process.platform === 'win32') {
    const hostileSystemRoot = resolve(fixture, 'hostile-system-root')
    mkdirSync(hostileSystemRoot)
    const hostileSystemEnvironment = { ...cleanEnvironment }
    for (const name of Object.keys(hostileSystemEnvironment)) {
      if (['SYSTEMROOT', 'WINDIR'].includes(name.toUpperCase())) delete hostileSystemEnvironment[name]
    }
    hostileSystemEnvironment.SystemRoot = hostileSystemRoot
    hostileSystemEnvironment.WINDIR = hostileSystemRoot
    const nativeShellStatus = runMake(['plan-browser'], hostileSystemEnvironment)
    assert.equal(nativeShellStatus, 0)
  }

  const directMake = spawnSync('make', ['plan-ci', 'plan-ci-full', 'plan-browser'], {
    cwd: resolve('.'), env: cleanEnvironment, encoding: 'utf8', shell: false,
  })
  assert.equal(directMake.status, 0, directMake.stderr)
  const publicPlan = directMake.stdout.trim().split(/\r?\n/u)
  assert.ok(publicPlan.includes('gate:vet'))
  assert.ok(publicPlan.includes('gate:browser'))
  assert.ok(publicPlan.includes('gate:browser-local'))
  assert.ok(publicPlan.includes('gate:browser-network'))

  for (const [arguments_, environment] of [
    [['plan-ci', 'CI_GATES='], cleanEnvironment],
    [['plan-ci', `BROWSER_NETWORK_COMPLETION=${completionPath}`], cleanEnvironment],
    [['-n', 'plan-ci'], cleanEnvironment],
    [['-f', 'Makefile', '-f', evilMakefile, 'plan-ci'], cleanEnvironment],
    [['plan-ci'], { ...cleanEnvironment, MAKEFLAGS: '-n' }],
    [['plan-ci'], { ...cleanEnvironment, GOENV: resolve(fixture, 'hostile-go.env') }],
  ]) {
    const rejectedPublicMake = spawnSync('make', arguments_, {
      cwd: resolve('.'), env: environment, encoding: 'utf8', shell: false,
    })
    assert.notEqual(rejectedPublicMake.status, 0, `public Make accepted ${arguments_.join(' ')}`)
  }
} finally {
  rmSync(fixture, { recursive: true, force: true })
  if (!completionExisted) rmSync(completionPath, { force: true })
}

console.log('make authority entry contracts: PASS')

function runEntry(arguments_, environment) {
  return spawnSync(process.execPath, [
    fileURLToPath(new URL('./entry.mjs', import.meta.url)),
    ...arguments_,
  ], {
    cwd: resolve('.'),
    env: environment,
    encoding: 'utf8',
  })
}
