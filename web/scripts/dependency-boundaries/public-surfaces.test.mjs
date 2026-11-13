import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import ts from 'typescript'
import { findBoundaryViolations, loadSourceGraph } from './graph.mjs'
import { BOUNDARY_POLICIES } from './policies.mjs'

const presentation = BOUNDARY_POLICIES.find(policy => policy.roots.includes('ui'))
const model = BOUNDARY_POLICIES.find(policy => policy.roots.includes('output/model'))

test('a scoped public subtree permits facts without broad prefix matches', () => {
  const policy = {
    roots: ['consumer'],
    forbidden: [],
    publicSurfaces: [{ root: 'domain', publicSubtree: 'domain/model' }],
  }
  const graph = new Map([
    ['consumer/view.ts', ['domain/model/facts.ts', 'domain-format/value.ts']],
    ['domain/model/facts.ts', ['domain/model/nested/value.ts']],
    ['domain/model/nested/value.ts', ['domain/model/facts.ts']],
  ])
  assert.deepEqual(findBoundaryViolations(graph, policy), [])
  for (const dependency of ['domain/new-private.ts', 'domain/model-private/value.ts', 'domain/model.ts', 'domain/private.ts?raw']) {
    graph.set('consumer/view.ts', [dependency])
    assert.deepEqual(findBoundaryViolations(graph, policy), [['consumer/view.ts', dependency]])
  }
})

test('presentation denies existing and newly introduced private output paths by default', () => {
  for (const dependency of [
    'output/streams/single-file.ts', 'output/browser-storage/persistence.ts',
    'output/future-provider/writer.ts', 'output/planning/contracts.ts',
    'output/model-private/facts.ts',
  ]) {
    assert.deepEqual(findBoundaryViolations(new Map([['ui/view.ts', [dependency]]]), presentation), [
      ['ui/view.ts', dependency],
    ])
  }
})

test('public models remain traversable and cannot conceal private dependencies', () => {
  const graph = new Map([
    ['ui/view.ts', ['output/model/facts.ts']],
    ['output/model/facts.ts', ['output/model/nested.ts']],
    ['output/model/nested.ts', ['output/model/facts.ts', 'output/new-provider/store.ts']],
  ])
  assert.deepEqual(findBoundaryViolations(graph, presentation), [
    ['ui/view.ts', 'output/model/facts.ts', 'output/model/nested.ts', 'output/new-provider/store.ts'],
  ])
})

test('forbidden application paths take precedence over presentation service stops', () => {
  const graph = new Map([
    ['ui/view.ts', ['application/receive/commands.ts', 'diagnostics/service.ts', 'application/browser-receive/composition.ts']],
    ['application/receive/commands.ts', ['output/private/writer.ts']],
    ['diagnostics/service.ts', ['output/private/store.ts']],
  ])
  assert.deepEqual(findBoundaryViolations(graph, presentation), [
    ['ui/view.ts', 'application/browser-receive/composition.ts'],
  ])
})

test('output models have no application or diagnostic traversal exemptions', () => {
  for (const dependency of [
    'application/receive/commands.ts', 'ui/view.ts', 'package:react', 'package:react-dom/client',
    'output/browser-storage/persistence.ts', 'connectivity/peer.ts', 'receiver/runtime.ts', 'transport/relay/heartbeat.ts',
  ]) {
    const graph = new Map([
      ['output/model/facts.ts', ['diagnostics/wrapper.ts']],
      ['diagnostics/wrapper.ts', [dependency]],
    ])
    assert.deepEqual(findBoundaryViolations(graph, model), [
      ['output/model/facts.ts', 'diagnostics/wrapper.ts', dependency],
    ])
  }
})

test('resolved aliases, inline types, re-exports and dynamic imports obey the same public surface', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'windshare-public-surface-'))
  try {
    for (const name of ['ui', 'output/model', 'output/private']) {
      fs.mkdirSync(path.join(directory, name), { recursive: true })
    }
    fs.writeFileSync(path.join(directory, 'ui/view.ts'), [
      "import type { Facts } from '@output/model/facts'",
      "type Inline = import('@output/private/store').Store",
      "const load = () => import('@output/private/writer')",
    ].join('\n'))
    fs.writeFileSync(path.join(directory, 'output/model/facts.ts'),
      "export type { Facts } from '../private/leak'")
    for (const file of ['store', 'writer', 'leak']) {
      fs.writeFileSync(path.join(directory, 'output/private/' + file + '.ts'), 'export interface Facts {}')
    }
    const graph = loadSourceGraph(directory, {
      moduleResolution: ts.ModuleResolutionKind.Bundler,
      module: ts.ModuleKind.ESNext,
      paths: { '@output/*': [path.join(directory, 'output/*')] },
    })
    assert.deepEqual(findBoundaryViolations(graph, presentation), [
      ['ui/view.ts', 'output/private/store.ts'],
      ['ui/view.ts', 'output/private/writer.ts'],
      ['ui/view.ts', 'output/model/facts.ts', 'output/private/leak.ts'],
    ])
    assert.deepEqual(findBoundaryViolations(graph, model), [
      ['output/model/facts.ts', 'output/private/leak.ts'],
    ])
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})
