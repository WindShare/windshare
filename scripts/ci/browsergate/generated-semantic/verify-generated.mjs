import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const repositoryRoot = resolve(import.meta.dirname, '..', '..', '..', '..')
const webRoot = join(repositoryRoot, 'web')
const committedPath = join(import.meta.dirname, 'final-semantic-reducer.js')
const outputRoot = mkdtempSync(join(tmpdir(), 'windshare-final-semantic-reducer-'))
const writeGenerated = process.argv.length === 3 && process.argv[2] === '--write'
if (process.argv.length > (writeGenerated ? 3 : 2)) {
  throw new Error('usage: node verify-generated.mjs [--write]')
}

try {
  const viteModulePath = join(webRoot, 'node_modules', 'vite', 'dist', 'node', 'index.js')
  const { build } = await import(pathToFileURL(viteModulePath).href)
  const originalWorkingDirectory = process.cwd()
  try {
    process.chdir(webRoot)
    await build({
      publicDir: false,
      logLevel: 'silent',
      build: {
        ssr: 'scripts/browser-evidence/final-semantic-reducer.ts',
        outDir: outputRoot,
        emptyOutDir: false,
        minify: false,
      },
    })
  } finally {
    process.chdir(originalWorkingDirectory)
  }
  const rebuilt = readFileSync(join(outputRoot, 'final-semantic-reducer.js'))
  if (writeGenerated) writeFileSync(committedPath, rebuilt)
  const committed = readFileSync(committedPath)
  assert.deepEqual(
    rebuilt,
    committed,
    'generated final semantic reducer is stale; rebuild it from the typed source',
  )
  const source = committed.toString('utf8')
  const imports = [...source.matchAll(/\bfrom\s+["']([^"']+)["']/gu)].map((match) => match[1])
  assert(imports.every((specifier) => specifier.startsWith('node:')))
  assert.match(
    source,
    /25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5/u,
  )
  assert.deepEqual(readdirSync(import.meta.dirname).sort(), [
    'final-semantic-reducer.d.mts',
    'final-semantic-reducer.js',
    'verify-generated.mjs',
  ])
  process.stdout.write(
    `generated final semantic reducer ${writeGenerated ? 'rebuilt' : 'is current'} and standard-library-only\n`,
  )
} finally {
  rmSync(outputRoot, { recursive: true, force: true })
}
