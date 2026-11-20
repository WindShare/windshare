import assert from 'node:assert/strict'
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { CONSOLE_APP_NAMES, selectConsoleApps } from './applications.mjs'
import { compareDirectories, parseTargetArg, verifyConsoleBundles } from './verify-dist.mjs'

async function writeTree(root, files) {
  await mkdir(root, { recursive: true })
  for (const [relativePath, content] of Object.entries(files)) {
    const fullPath = join(root, relativePath)
    await mkdir(dirname(fullPath), { recursive: true })
    await writeFile(fullPath, content)
  }
}

async function withTempDir(run) {
  const dir = await mkdtemp(join(tmpdir(), 'windshare-console-verify-test-'))
  try {
    return await run(dir)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
}

test('selectConsoleApps resolves all, single apps, and rejects unknown names', () => {
  assert.deepEqual(selectConsoleApps(), [...CONSOLE_APP_NAMES])
  assert.deepEqual(selectConsoleApps('all'), [...CONSOLE_APP_NAMES])
  assert.deepEqual(selectConsoleApps('relay'), ['relay'])
  assert.deepEqual(selectConsoleApps('network'), ['network'])
  assert.throws(() => selectConsoleApps('unknown'), /Select one of relay, network, or all/)
})

test('parseTargetArg strips pnpm separator and rejects extra arguments', () => {
  assert.equal(parseTargetArg([]), 'all')
  assert.equal(parseTargetArg(['--']), 'all')
  assert.equal(parseTargetArg(['--', 'relay']), 'relay')
  assert.equal(parseTargetArg(['network']), 'network')
  assert.throws(() => parseTargetArg(['relay', 'network']), /Expected at most one/)
})

test('compareDirectories detects matching trees, missing, extra, and changed bytes', async () => {
  await withTempDir(async root => {
    const checkedIn = join(root, 'checked-in')
    const rebuilt = join(root, 'rebuilt')
    const tree = {
      'index.html': '<!doctype html>\n',
      'routes.rs': 'match path { _ => None }\n',
      'assets/index-abc.js': 'console.log(1)\n',
    }
    await writeTree(checkedIn, tree)
    await writeTree(rebuilt, tree)

    const identical = await compareDirectories(checkedIn, rebuilt)
    assert.deepEqual(identical, { matches: true, fileCount: 3, missing: [], extra: [], changed: [] })

    await writeFile(join(checkedIn, 'assets/index-abc.js'), 'console.log(2)\n')
    await writeFile(join(checkedIn, 'assets/stale.css'), 'body{}\n')
    await writeFile(join(rebuilt, 'assets/new.css'), 'html{}\n')

    const drifted = await compareDirectories(checkedIn, rebuilt)
    assert.deepEqual(drifted, {
      matches: false,
      fileCount: 4,
      missing: ['assets/new.css'],
      extra: ['assets/stale.css'],
      changed: ['assets/index-abc.js'],
    })
  })
})

test('verifyConsoleBundles checks without mutating checked-in dist and always cleans temp dir', async () => {
  await withTempDir(async root => {
    const distRoot = join(root, 'dist')
    const tempBase = join(root, 'scratch')
    await mkdir(tempBase, { recursive: true })
    await writeTree(join(distRoot, 'relay'), { 'index.html': 'relay\n', 'assets/a.js': '1' })
    await writeTree(join(distRoot, 'network'), { 'index.html': 'network\n' })

    const verified = await verifyConsoleBundles({
      workspace: root,
      target: 'all',
      tempBaseDir: tempBase,
      distDirectory: (_, appName) => join(distRoot, appName),
      buildBundle: async ({ appName, outDir }) => {
        const files = appName === 'relay'
          ? { 'index.html': 'relay\n', 'assets/a.js': '1' }
          : { 'index.html': 'network\n' }
        await writeTree(outDir, files)
      },
    })
    assert.deepEqual(verified, [
      { appName: 'relay', fileCount: 2 },
      { appName: 'network', fileCount: 1 },
    ])
    assert.deepEqual(await readdir(tempBase), [])

    await assert.rejects(
      verifyConsoleBundles({
        workspace: root,
        target: 'relay',
        tempBaseDir: tempBase,
        distDirectory: (_, appName) => join(distRoot, appName),
        buildBundle: async ({ outDir }) => {
          await writeTree(outDir, { 'index.html': 'changed\n', 'assets/a.js': '1' })
        },
      }),
      /changed: index\.html[\s\S]*pnpm -C consoles build:relay/,
    )
    assert.deepEqual(await readdir(tempBase), [])
  })
})
