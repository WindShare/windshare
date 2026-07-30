import assert from 'node:assert/strict'
import { readdirSync } from 'node:fs'
import { join, relative, resolve, sep } from 'node:path'

const BROWSERGATE_ROOT = resolve(import.meta.dirname, '..', '..')
const TEST_ENTRY_SUFFIX = '.tests.mjs'
const ALLOWED_TEST_ROOTS = Object.freeze([
  'tests/contract',
  'tests/process',
  'tests/suite-discovery',
])
const TESTSUPPORT_CODE_PATTERN = /\.(?:[cm]?[jt]s|jsx|tsx)$/iu
const TESTSUPPORT_HELPER_PATTERN = /\.(?:assertions|fixture|fixtures)\.(?:[cm]?[jt]s|jsx|tsx)$/u

const inventory = browsergateFileInventory(BROWSERGATE_ROOT)
assertBrowsergateTestLayout(inventory)

for (const misplacedTest of [
  'orphan.tests.mjs',
  'process/runtime-owner.tests.mjs',
  'generated-semantic/verifier.tests.mjs',
  'testsupport/empty.tests.mjs',
]) {
  assert.throws(
    () => assertBrowsergateTestLayout([...inventory, misplacedTest]),
    /outside its execution-authority test root/u,
    misplacedTest,
  )
}

for (const runnableSupport of [
  'testsupport/playwright-discovery.mjs',
  'testsupport/process-helper.ts',
]) {
  assert.throws(
    () => assertBrowsergateTestLayout([...inventory, runnableSupport]),
    /must use an assertions or fixture suffix/u,
    runnableSupport,
  )
}

assert.doesNotThrow(() => assertBrowsergateTestLayout([
  ...inventory,
  'tests/process/additional-process-boundary.tests.mjs',
  'testsupport/generated-semantic.fixture.json',
  'testsupport/runtime-owner.fixtures.mjs',
]))

process.stdout.write('browsergate test layout contracts: PASS\n')

function assertBrowsergateTestLayout(relativePaths) {
  assert.equal(Array.isArray(relativePaths), true, 'browsergate layout inventory must be an array')
  const canonicalPaths = relativePaths.map(canonicalRelativePath)
  assert.equal(
    new Set(canonicalPaths).size,
    canonicalPaths.length,
    'browsergate layout inventory contains duplicate files',
  )

  for (const relativePath of canonicalPaths) {
    if (relativePath.toLowerCase().endsWith(TEST_ENTRY_SUFFIX)) {
      assert(
        ALLOWED_TEST_ROOTS.some((root) => relativePath.startsWith(root + '/')),
        `${relativePath} is outside its execution-authority test root`,
      )
    }
    if (
      relativePath.startsWith('testsupport/') &&
      TESTSUPPORT_CODE_PATTERN.test(relativePath)
    ) {
      assert(
        TESTSUPPORT_HELPER_PATTERN.test(relativePath),
        `${relativePath} must use an assertions or fixture suffix`,
      )
    }
  }
}

function browsergateFileInventory(root) {
  const files = []
  visit(root)
  return Object.freeze(files)

  function visit(directory) {
    const entries = readdirSync(directory, { withFileTypes: true })
      .sort((left, right) => compareOrdinal(left.name, right.name))
    for (const entry of entries) {
      const path = join(directory, entry.name)
      const relativePath = portableRelativePath(root, path)
      assert.equal(entry.isSymbolicLink(), false, `${relativePath} must not be a symbolic link`)
      if (entry.isDirectory()) {
        visit(path)
        continue
      }
      assert.equal(entry.isFile(), true, `${relativePath} must be a regular file`)
      files.push(relativePath)
    }
  }
}

function canonicalRelativePath(value) {
  assert.equal(typeof value, 'string', 'browsergate layout paths must be text')
  assert(value !== '' && !value.startsWith('/') && !value.endsWith('/'))
  assert.equal(value.includes('\\'), false, `${value} must use portable separators`)
  assert.equal(value.split('/').some((segment) => segment === '' || segment === '.' || segment === '..'), false)
  return value
}

function portableRelativePath(root, path) {
  return canonicalRelativePath(relative(root, path).split(sep).join('/'))
}

function compareOrdinal(left, right) {
  return left < right ? -1 : left > right ? 1 : 0
}
