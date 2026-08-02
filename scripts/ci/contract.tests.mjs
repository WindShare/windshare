import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import {
  inspectCIContract,
  inspectCodeDirectoryBoundaries,
  parsePlatformEntrypointNames,
} from './contract.mjs'

const fixtureRoots = []

try {
  const root = createFixture()
  assert.deepEqual(inspectLocal(root), [])
  assert.deepEqual(parsePlatformEntrypointNames('PLATFORM_ENTRYPOINTS := alpha beta'), ['alpha', 'beta'])
  assert.throws(
    () => parsePlatformEntrypointNames('PLATFORM_ENTRYPOINTS := alpha alpha'),
    /duplicate names/u,
  )

  const boundaryDirectory = resolve(root, 'bounded-module')
  mkdirSync(boundaryDirectory)
  for (let index = 1; index <= 20; index += 1) {
    writeFileSync(resolve(boundaryDirectory, `source-${index}.mjs`), '')
    writeFileSync(resolve(boundaryDirectory, `source-${index}.tests.mjs`), '')
  }
  assert.deepEqual(inspectCodeDirectoryBoundaries(root), [])
  writeFileSync(resolve(boundaryDirectory, 'source-21.mjs'), '')
  const boundaryViolation = 'bounded-module contains 21 non-test code files; maximum is 20'
  assert.deepEqual(inspectCodeDirectoryBoundaries(root), [boundaryViolation])
  assert(inspectLocal(root).includes(boundaryViolation))
  rmSync(boundaryDirectory, { recursive: true })

  writeFileSync(resolve(root, 'scripts/ci/alpha.sh'), '#!/usr/bin/env bash\n')
  assert(
    inspectLocal(root).includes('Makefile entrypoint alpha forbids legacy wrapper scripts/ci/alpha.sh'),
  )
  rmSync(resolve(root, 'scripts/ci/alpha.sh'))

  writeFileSync(resolve(root, 'scripts/ci/windows/undeclared.ps1'), '')
  writeFileSync(resolve(root, 'scripts/ci/linux/undeclared.sh'), '#!/usr/bin/env bash\n')
  const undeclared = inspectLocal(root)
  assert(undeclared.includes(
    'Makefile PLATFORM_ENTRYPOINTS must declare scripts/ci/windows/undeclared.ps1',
  ))
  assert(undeclared.includes(
    'Makefile PLATFORM_ENTRYPOINTS must declare scripts/ci/linux/undeclared.sh',
  ))
  rmSync(resolve(root, 'scripts/ci/windows/undeclared.ps1'))
  rmSync(resolve(root, 'scripts/ci/linux/undeclared.sh'))

  writeFileSync(resolve(root, 'Makefile'), 'PLATFORM_ENTRYPOINTS := alpha missing\n')
  const missing = inspectLocal(root)
  assert(missing.includes(
    'Makefile entrypoint missing requires existing scripts/ci/windows/missing.ps1',
  ))
  assert(missing.includes(
    'Makefile entrypoint missing requires existing scripts/ci/linux/missing.sh',
  ))

  writeFileSync(resolve(root, 'Makefile'), 'PLATFORM_ENTRYPOINTS := alpha coverage\n')
  writeFileSync(resolve(root, 'scripts/ci/windows/coverage.ps1'), '')
  writeFileSync(resolve(root, 'scripts/ci/linux/coverage.sh'), '#!/usr/bin/env bash\n')
  writeFileSync(resolve(root, '.gitignore'), 'coverage.*\n')
  const ignored = inspectLocal(root)
  assert(ignored.includes(
    'Makefile entrypoint coverage requires non-ignored scripts/ci/windows/coverage.ps1',
  ))
  assert(ignored.includes(
    'Makefile entrypoint coverage requires non-ignored scripts/ci/linux/coverage.sh',
  ))

  writeFileSync(
    resolve(root, '.gitignore'),
    'coverage.*\n!scripts/ci/windows/coverage.ps1\n!scripts/ci/linux/coverage.sh\n',
  )
  assert.deepEqual(inspectLocal(root), [])
  const actions = inspectCIContract(root, { requireTracked: true }).violations
  assert(actions.includes(
    'Makefile entrypoint coverage requires tracked scripts/ci/windows/coverage.ps1 in GitHub Actions',
  ))
  assert(actions.includes(
    'Makefile entrypoint coverage requires tracked scripts/ci/linux/coverage.sh in GitHub Actions',
  ))

  rmSync(resolve(root, 'scripts/ci/windows/coverage.ps1'))
  rmSync(resolve(root, 'scripts/ci/linux/coverage.sh'))
  writeFileSync(resolve(root, 'Makefile'), 'PLATFORM_ENTRYPOINTS := alpha\n')
  const workflow = resolve(root, '.github/workflows/ci.yml')
  writeFileSync(workflow, 'steps:\n  - run: scripts/ci/linux/alpha.sh\n')
  assert.deepEqual(inspectLocal(root), [
    '.github/workflows/ci.yml:2 invokes scripts/ci/linux/alpha.sh without an explicit shell',
  ])

  writeFileSync(workflow, 'steps:\n  - run: |\n      ./scripts/ci/linux/alpha.sh --verify\n')
  assert.deepEqual(inspectLocal(root), [
    '.github/workflows/ci.yml:3 invokes scripts/ci/linux/alpha.sh without an explicit shell',
  ])

  writeFileSync(workflow, 'steps:\n  - run: bash scripts/ci/linux/alpha.sh\n')
  assert.deepEqual(inspectLocal(root), [])

  const shellContract = resolve(root, 'scripts/ci/content-contract.tests.sh')
  writeFileSync(
    shellContract,
    '#!/usr/bin/env bash\nassert_contains "moved/source.go" \'required contract\'\n',
  )
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/content-contract.tests.sh:2 asserts content of missing repository file moved/source.go',
  ])
  mkdirSync(resolve(root, 'moved'))
  writeFileSync(resolve(root, 'moved/source.go'), 'required contract\n')
  assert.deepEqual(inspectLocal(root), [])

  writeFileSync(resolve(root, 'moved/source.go'), 'drifted contract\n')
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/content-contract.tests.sh:2 expects moved/source.go to contain literal "required contract"',
  ])

  writeFileSync(
    shellContract,
    '#!/usr/bin/env bash\nassert_not_contains "moved/source.go" \'forbidden contract\'\n',
  )
  writeFileSync(resolve(root, 'moved/source.go'), 'forbidden contract\n')
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/content-contract.tests.sh:2 expects moved/source.go to not contain literal "forbidden contract"',
  ])

  console.log('ci-contract tests: PASS')
} finally {
  for (const root of fixtureRoots) rmSync(root, { recursive: true, force: true })
}

function createFixture() {
  const root = mkdtempSync(join(tmpdir(), 'windshare-ci-contract-'))
  fixtureRoots.push(root)
  mkdirSync(resolve(root, 'scripts/ci/windows'), { recursive: true })
  mkdirSync(resolve(root, 'scripts/ci/linux'), { recursive: true })
  mkdirSync(resolve(root, '.github/workflows'), { recursive: true })
  writeFileSync(resolve(root, 'Makefile'), 'PLATFORM_ENTRYPOINTS := alpha\n')
  writeFileSync(resolve(root, 'scripts/ci/windows/alpha.ps1'), '')
  writeFileSync(resolve(root, 'scripts/ci/linux/alpha.sh'), '#!/usr/bin/env bash\n')
  writeFileSync(
    resolve(root, '.github/workflows/ci.yml'),
    'steps:\n  - run: bash scripts/ci/linux/alpha.sh\n',
  )
  runGit(root, ['init', '--quiet'])
  runGit(root, ['add', '--', 'Makefile', 'scripts/ci', '.github/workflows'])
  return root
}

function inspectLocal(root) {
  return inspectCIContract(root, { requireTracked: false }).violations
}

function runGit(root, arguments_) {
  const result = spawnSync('git', arguments_, { cwd: root, encoding: 'utf8' })
  if (result.error !== undefined || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    throw new Error(`git ${arguments_.join(' ')} failed: ${detail}`)
  }
}
