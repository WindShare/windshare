import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import test from 'node:test'

import { productionGraph, resolveSource } from './web-source-graph.mjs'

test('source resolution removes Vite query and hash qualifiers from the filesystem path', () => {
  withFixture((root) => {
    const asset = join(root, 'restoration', 'windows-v1.ps1')
    mkdirSync(join(root, 'restoration'))
    writeFileSync(asset, 'Write-Output "fixture"')

    assert.deepEqual(resolveSource(root, './restoration/windows-v1.ps1?raw'), {
      file: asset,
      loadMode: 'opaque-resource',
    })
    assert.deepEqual(resolveSource(root, './restoration/windows-v1.ps1?raw#immutable'), {
      file: asset,
      loadMode: 'opaque-resource',
    })
    assert.deepEqual(resolveSource(root, './restoration/windows-v1.ps1#immutable'), {
      file: asset,
      loadMode: 'source',
    })
    assert.deepEqual(resolveSource(root, './restoration/windows-v1.ps1?worker#immutable'), {
      file: asset,
      loadMode: 'source',
    })
  })
})

test('raw assets are reachable production dependencies but their bytes are not parsed as modules', () => {
  withFixture((root) => {
    const entry = join(root, 'main.ts')
    const asset = join(root, 'windows-v1.ps1')
    writeFileSync(entry, "import script from './windows-v1.ps1?raw#immutable'\nvoid script\n")
    writeFileSync(asset, "import './not-a-real-powershell-dependency'\n")

    const graph = productionGraph(entry, root)

    assert.deepEqual([...graph.dependencies].sort(), [asset, entry].sort())
    assert.deepEqual(graph.unresolved, [])
  })
})

test('hash-qualified source modules retain their transitive graph', () => {
  withFixture((root) => {
    const entry = join(root, 'main.ts')
    const child = join(root, 'child.ts')
    const leaf = join(root, 'leaf.ts')
    writeFileSync(entry, "import './child#stable'\n")
    writeFileSync(child, "import './leaf'\n")
    writeFileSync(leaf, 'export const value = true\n')

    const graph = productionGraph(entry, root)

    assert.deepEqual([...graph.dependencies].sort(), [child, entry, leaf].sort())
    assert.deepEqual(graph.unresolved, [])
  })
})

function withFixture(run) {
  const root = resolve(mkdtempSync(join(tmpdir(), 'windshare-web-graph-')))
  try {
    run(root)
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
}
