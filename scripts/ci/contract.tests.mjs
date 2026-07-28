import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { inspectCIContract } from './contract.mjs'

const fixtureRoots = []

try {
  const root = createFixture()
  assert.deepEqual(inspectLocal(root), [])

  writeFileSync(resolve(root, 'Makefile'), 'GATES := alpha missing\n')
  const missing = inspectLocal(root)
  assert(missing.includes('Makefile gate missing requires existing scripts/ci/missing.ps1'))
  assert(missing.includes('Makefile gate missing requires existing scripts/ci/missing.sh'))

  writeFileSync(resolve(root, 'Makefile'), 'GATES := alpha coverage\n')
  writeFileSync(resolve(root, 'scripts/ci/coverage.ps1'), '')
  writeFileSync(resolve(root, 'scripts/ci/coverage.sh'), '#!/usr/bin/env bash\n')
  writeFileSync(resolve(root, '.gitignore'), 'coverage.*\n')
  const ignored = inspectLocal(root)
  assert(ignored.includes('Makefile gate coverage requires non-ignored scripts/ci/coverage.ps1'))
  assert(ignored.includes('Makefile gate coverage requires non-ignored scripts/ci/coverage.sh'))

  writeFileSync(resolve(root, '.gitignore'), 'coverage.*\n!scripts/ci/coverage.ps1\n!scripts/ci/coverage.sh\n')
  assert.deepEqual(inspectLocal(root), [])
  const actions = inspectCIContract(root, { requireTracked: true }).violations
  assert(actions.includes('Makefile gate coverage requires tracked scripts/ci/coverage.ps1 in GitHub Actions'))
  assert(actions.includes('Makefile gate coverage requires tracked scripts/ci/coverage.sh in GitHub Actions'))

  writeFileSync(resolve(root, 'Makefile'), 'GATES := alpha\n')
  const workflow = resolve(root, '.github/workflows/ci.yml')
  writeFileSync(workflow, 'steps:\n  - run: scripts/ci/alpha.sh\n')
  assert.deepEqual(inspectLocal(root), [
    '.github/workflows/ci.yml:2 invokes scripts/ci/alpha.sh without an explicit shell',
  ])

  writeFileSync(workflow, 'steps:\n  - run: |\n      ./scripts/ci/alpha.sh --verify\n')
  assert.deepEqual(inspectLocal(root), [
    '.github/workflows/ci.yml:3 invokes scripts/ci/alpha.sh without an explicit shell',
  ])

  writeFileSync(workflow, 'steps:\n  - run: bash scripts/ci/alpha.sh\n')
  assert.deepEqual(inspectLocal(root), [])

  const shellContract = resolve(root, 'scripts/ci/alpha.sh')
  writeFileSync(
    shellContract,
    '#!/usr/bin/env bash\nassert_contains "moved/source.go" \'required contract\'\n',
  )
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/alpha.sh:2 asserts content of missing repository file moved/source.go',
  ])
  mkdirSync(resolve(root, 'moved'))
  writeFileSync(resolve(root, 'moved/source.go'), 'required contract\n')
  assert.deepEqual(inspectLocal(root), [])

  writeFileSync(resolve(root, 'moved/source.go'), 'drifted contract\n')
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/alpha.sh:2 expects moved/source.go to contain literal "required contract"',
  ])

  writeFileSync(
    shellContract,
    '#!/usr/bin/env bash\nassert_not_contains "moved/source.go" \'forbidden contract\'\n',
  )
  writeFileSync(resolve(root, 'moved/source.go'), 'forbidden contract\n')
  assert.deepEqual(inspectLocal(root), [
    'scripts/ci/alpha.sh:2 expects moved/source.go to not contain literal "forbidden contract"',
  ])

  console.log('ci-contract tests: PASS')
} finally {
  for (const root of fixtureRoots) rmSync(root, { recursive: true, force: true })
}

function createFixture() {
  const root = mkdtempSync(join(tmpdir(), 'windshare-ci-contract-'))
  fixtureRoots.push(root)
  mkdirSync(resolve(root, 'scripts/ci'), { recursive: true })
  mkdirSync(resolve(root, '.github/workflows'), { recursive: true })
  writeFileSync(resolve(root, 'Makefile'), 'GATES := alpha\n')
  writeFileSync(resolve(root, 'scripts/ci/alpha.ps1'), '')
  writeFileSync(resolve(root, 'scripts/ci/alpha.sh'), '#!/usr/bin/env bash\n')
  writeFileSync(resolve(root, '.github/workflows/ci.yml'), 'steps:\n  - run: bash scripts/ci/alpha.sh\n')
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
