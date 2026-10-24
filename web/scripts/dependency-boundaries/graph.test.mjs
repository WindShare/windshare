import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import ts from 'typescript'
import { findBoundaryViolations, loadSourceGraph, moduleReferences } from './graph.mjs'

test('collects type imports, re-exports, import types and dynamic imports', () => {
  const source = ts.createSourceFile('contract.ts', [
    "import type { Backend } from '../output/backend'",
    "export * from '../output/barrel'",
    "type Deferred = import('../output/deferred').Backend",
    "const native = import('../output/native')",
    "const legacy = require('../output/legacy')",
    "const unknown = import(selectedModule)",
  ].join('\n'), ts.ScriptTarget.Latest, true)
  assert.deepEqual(moduleReferences(source), [
    '../output/backend', '../output/barrel', '../output/deferred',
    '../output/native', '../output/legacy', null,
  ])
})

test('rejects implementation dependencies hidden behind shared contracts', () => {
  const graph = new Map([
    ['transfer/job.ts', ['contracts/public.ts']],
    ['contracts/public.ts', ['contracts/nested.ts']],
    ['contracts/nested.ts', ['output/store.ts']],
    ['output/store.ts', []],
  ])
  assert.deepEqual(findBoundaryViolations(graph), [
    ['transfer/job.ts', 'contracts/public.ts', 'contracts/nested.ts', 'output/store.ts'],
  ])
})

test('allows adapters to depend on transfer and handles cycles without broad path matches', () => {
  const graph = new Map([
    ['output/adapter.ts', ['transfer/intent.ts']],
    ['transfer/intent.ts', ['catalog/path.ts', 'output-format/value.ts']],
    ['catalog/path.ts', ['transfer/intent.ts']],
    ['output-format/value.ts', []],
  ])
  assert.deepEqual(findBoundaryViolations(graph), [])
})

test('rejects UI, application, computed imports and unresolved relative imports', () => {
  for (const dependency of ['ui/view.ts', 'application/controller.ts', '<computed import>', '<unresolved import: ./missing>']) {
    assert.deepEqual(findBoundaryViolations(new Map([['transfer/port.ts', [dependency]]])), [
      ['transfer/port.ts', dependency],
    ])
  }
})

test('uses TypeScript resolution for aliases and type re-exports', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'windshare-dependencies-'))
  try {
    const source = path.join(directory, 'src')
    for (const name of ['transfer', 'contracts', 'output']) fs.mkdirSync(path.join(source, name), { recursive: true })
    fs.writeFileSync(path.join(source, 'transfer/job.ts'), "import type { Backend } from '@contracts/public'")
    fs.writeFileSync(path.join(source, 'contracts/public.ts'), "export type { Backend } from '../output/store.js'")
    fs.writeFileSync(path.join(source, 'output/store.ts'), 'export interface Backend {}')
    const graph = loadSourceGraph(source, {
      moduleResolution: ts.ModuleResolutionKind.Bundler,
      module: ts.ModuleKind.ESNext,
      paths: { '@contracts/*': [path.join(source, 'contracts/*')] },
    })
    assert.deepEqual(findBoundaryViolations(graph), [
      ['transfer/job.ts', 'contracts/public.ts', 'output/store.ts'],
    ])
  } finally {
    // Only the exact directory created by this fixture is removed.
    fs.rmSync(directory, { recursive: true, force: true })
  }
})
