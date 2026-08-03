import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  inspectCIContract,
  inspectCodeDirectoryBoundaries,
  LOCAL_CI_GATES,
  parsePlatformEntrypointNames,
  parsePublicLocalTargetNames,
  PUBLIC_LOCAL_TARGETS,
  REQUIRED_PLATFORM_ENTRYPOINTS,
} from './contract.mjs'

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const fixtureRoots = []

try {
  const root = createFixture()
  assert.deepEqual(inspectCIContract(root).violations, [])
  assert.deepEqual(
    parsePublicLocalTargetNames(`PUBLIC_TARGETS := ${PUBLIC_LOCAL_TARGETS.join(' ')}`),
    PUBLIC_LOCAL_TARGETS,
  )
  assert.throws(
    () => parsePlatformEntrypointNames('PLATFORM_ENTRYPOINTS := alpha alpha'),
    /duplicate names/u,
  )

  writeFixtureMakefile(root, {
    publicTargets: PUBLIC_LOCAL_TARGETS.slice(1),
  })
  assert(findViolation(root, /PUBLIC_TARGETS must be/u))

  writeFixtureMakefile(root, {
    ciGates: [...LOCAL_CI_GATES, 'integration'],
  })
  assert(findViolation(root, /CI_GATES must be/u))

  writeFixtureMakefile(root, {
    entrypoints: REQUIRED_PLATFORM_ENTRYPOINTS.filter((name) => name !== 'browser-preflight'),
  })
  assert(findViolation(root, /must include public gate browser-preflight/u))

  writeFixtureMakefile(root, {
    entrypoints: [...REQUIRED_PLATFORM_ENTRYPOINTS, 'browser-contract'],
  })
  assert(findViolation(root, /contains unknown gate browser-contract/u))

  writeFixtureMakefile(root)
  rmSync(resolve(root, 'scripts/ci/windows/browser-preflight.ps1'))
  assert(findViolation(
    root,
    /browser-preflight requires existing scripts\/ci\/windows\/browser-preflight\.ps1/u,
  ))
  createPlatformScripts(root, ['browser-preflight'])

  const boundaryDirectory = resolve(root, 'bounded-module')
  mkdirSync(boundaryDirectory)
  for (let index = 1; index <= 20; index += 1) {
    writeFileSync(resolve(boundaryDirectory, `source-${index}.mjs`), '')
    writeFileSync(resolve(boundaryDirectory, `source-${index}.tests.mjs`), '')
  }
  assert.deepEqual(inspectCodeDirectoryBoundaries(root), [])
  writeFileSync(resolve(boundaryDirectory, 'source-21.mjs'), '')
  assert.deepEqual(inspectCodeDirectoryBoundaries(root), [
    'bounded-module contains 21 non-test code files; maximum is 20',
  ])

  verifyMakePlans()
  console.log('ci-contract tests: PASS')
} finally {
  for (const root of fixtureRoots) rmSync(root, { recursive: true, force: true })
}

function createFixture() {
  const root = mkdtempSync(join(tmpdir(), 'windshare-ci-contract-'))
  fixtureRoots.push(root)
  mkdirSync(resolve(root, 'scripts/ci/windows/browser'), { recursive: true })
  mkdirSync(resolve(root, 'scripts/ci/linux'), { recursive: true })
  createPlatformScripts(root, REQUIRED_PLATFORM_ENTRYPOINTS)
  writeFileSync(resolve(root, 'scripts/ci/windows/browser/smoke.ps1'), '')
  writeFixtureMakefile(root)
  runGit(root, ['init', '--quiet'])
  return root
}

function writeFixtureMakefile(root, {
  publicTargets = PUBLIC_LOCAL_TARGETS,
  entrypoints = REQUIRED_PLATFORM_ENTRYPOINTS,
  ciGates = LOCAL_CI_GATES,
} = {}) {
  writeFileSync(
    resolve(root, 'Makefile'),
    [
      `PUBLIC_TARGETS := ${publicTargets.join(' ')}`,
      `PLATFORM_ENTRYPOINTS := ${entrypoints.join(' ')}`,
      `CI_GATES := ${ciGates.join(' ')}`,
      '',
    ].join('\n'),
  )
}

function createPlatformScripts(root, entrypoints) {
  for (const entrypoint of entrypoints) {
    writeFileSync(resolve(root, `scripts/ci/windows/${entrypoint}.ps1`), '')
    writeFileSync(resolve(root, `scripts/ci/linux/${entrypoint}.sh`), '')
  }
}

function findViolation(root, pattern) {
  return inspectCIContract(root).violations.some((violation) => pattern.test(violation))
}

function verifyMakePlans() {
  const windowsCI = makePlan('ci', 'Windows_NT')
  assert.match(windowsCI, /scripts\/ci\/windows\/hygiene\.ps1/u)
  assert.match(windowsCI, /scripts\/ci\/windows\/browser-preflight\.ps1/u)
  assert.match(windowsCI, /scripts\/ci\/windows\/browser\/smoke\.ps1/u)
  assert.equal(literalCount(windowsCI, 'scripts/ci/windows/web-dependencies.ps1'), 1)

  const linuxCI = makePlan('ci')
  assert.match(linuxCI, /scripts\/ci\/linux\/hygiene\.sh/u)
  assert.match(linuxCI, /scripts\/ci\/linux\/browser-preflight\.sh/u)
  assert.doesNotMatch(linuxCI, /browser\/smoke/u)
  assert.equal(literalCount(linuxCI, 'scripts/ci/linux/web-dependencies.sh'), 1)

  for (const plan of [windowsCI, linuxCI]) {
    for (const replay of [
      '/integration.', '/e2e-go.', '/browser-contract.', '/browser-generated.',
      '/browser-local.', '/browser-network.',
    ]) {
      assert(!plan.includes(replay), `make ci must not replay ${replay}`)
    }
  }

  const windowsE2E = makePlan('e2e', 'Windows_NT')
  assert.match(windowsE2E, /scripts\/ci\/windows\/e2e-go\.ps1/u)
  assert.match(windowsE2E, /scripts\/ci\/windows\/browser\/smoke\.ps1/u)

  const linuxE2E = makePlan('e2e')
  assert.match(linuxE2E, /scripts\/ci\/linux\/e2e-go\.sh/u)
  assert.doesNotMatch(linuxE2E, /browser\/smoke/u)

  const browser = makePlan('browser', 'Windows_NT')
  assert.match(browser, /scripts\/ci\/windows\/browser-local\.ps1/u)
  assert.match(browser, /scripts\/ci\/windows\/browser-network\.ps1/u)

  for (const retired of ['ci-full', 'authority-context', 'plan-ci', 'plan-ci-full', 'plan-browser']) {
    const result = runMake(['-n', retired], 'Windows_NT')
    assert.notEqual(result.status, 0, `${retired} must not remain a Make target`)
  }
}

function makePlan(target, operatingSystem = undefined) {
  const result = runMake(['-n', target], operatingSystem)
  if (result.error !== undefined || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    throw new Error(`make -n ${target} failed: ${detail}`)
  }
  return result.stdout.replaceAll('\\', '/')
}

function runMake(arguments_, operatingSystem) {
  const environment = { ...process.env }
  if (operatingSystem === undefined) {
    delete environment.OS
  } else {
    environment.OS = operatingSystem
  }
  return spawnSync('make', arguments_, {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: environment,
  })
}

function literalCount(source, literal) {
  return source.split(literal).length - 1
}

function runGit(root, arguments_) {
  const result = spawnSync('git', arguments_, { cwd: root, encoding: 'utf8' })
  if (result.error !== undefined || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    throw new Error(`git ${arguments_.join(' ')} failed: ${detail}`)
  }
}
