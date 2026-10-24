import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import ts from 'typescript'
import { findBoundaryViolations, loadSourceGraph } from './graph.mjs'
import { BOUNDARY_POLICIES } from './policies.mjs'

function violations(entries) {
  const graph = new Map(entries)
  return BOUNDARY_POLICIES.flatMap(policy => findBoundaryViolations(graph, policy))
}

test('application and reusable modules reject presentation even through a shared barrel', () => {
  for (const root of ['application/receive/runtime.ts', 'output/store.ts', 'diagnostics/context.ts']) {
    assert.ok(violations([
      [root, ['contracts/barrel.ts']],
      ['contracts/barrel.ts', ['ui/model.ts']],
      ['ui/model.ts', []],
    ]).some(chain => chain[0] === root && chain.at(-1) === 'ui/model.ts'))
  }
})

test('protocol sessions and wire codecs cannot reach connectivity implementations', () => {
  for (const root of ['session/runtime.ts', 'protocol/turn-control.ts']) {
    assert.ok(violations([
      [root, ['contracts/barrel.ts']],
      ['contracts/barrel.ts', ['connectivity/peer.ts']],
      ['connectivity/peer.ts', []],
    ]).some(chain => chain[0] === root && chain.at(-1) === 'connectivity/peer.ts'))
  }
  assert.deepEqual(violations([
    ['session/runtime.ts', ['protocol/turn-control.ts', 'contracts/relay-delivery-observation.ts']],
    ['connectivity/peer.ts', ['session/runtime.ts', 'protocol/turn-control.ts']],
    ['protocol/turn-control.ts', []],
    ['contracts/relay-delivery-observation.ts', []],
  ]), [])
})

test('UI may invoke application commands, but cannot assemble output routes or access storage', () => {
  assert.deepEqual(violations([
    ['ui/controller.ts', ['application/receive/application.ts', 'transfer/lifecycle/state.ts']],
    ['application/receive/application.ts', ['output/workspace/repository.ts']],
    ['output/workspace/repository.ts', ['transfer/lifecycle/state.ts']],
    ['transfer/lifecycle/state.ts', []],
  ]), [])
  for (const target of [
    'application/browser-receive/composition.ts', 'output/workspace/repository.ts',
    'output/browser-delivery/session.ts', 'output/file-system-access/execution/resource-owner.ts',
  ]) {
    assert.ok(violations([
      ['ui/controller.ts', ['contracts/barrel.ts']],
      ['contracts/barrel.ts', [target]],
      [target, []],
    ]).some(chain => chain[0] === 'ui/controller.ts' && chain.at(-1) === target))
  }
})

test('resolved type imports still prevent React from entering application code', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'windshare-layer-packages-'))
  try {
    fs.mkdirSync(path.join(directory, 'application'))
    fs.writeFileSync(path.join(directory, 'application/runtime.ts'),
      "import type { ReactNode } from 'react'\nexport type { Root } from 'react-dom/client'")
    const graph = loadSourceGraph(directory, { moduleResolution: ts.ModuleResolutionKind.Bundler })
    const policy = BOUNDARY_POLICIES.find(value => value.roots.length === 1 && value.roots[0] === 'application')
    assert.deepEqual(findBoundaryViolations(graph, policy), [
      ['application/runtime.ts', 'package:react'],
      ['application/runtime.ts', 'package:react-dom/client'],
    ])
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

test('bundled assets must exist and cannot hide a forbidden dependency', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'windshare-layer-assets-'))
  try {
    for (const name of ['transfer', 'output', 'ui']) fs.mkdirSync(path.join(directory, name))
    fs.writeFileSync(path.join(directory, 'output/restore.ps1'), 'Write-Output ready')
    fs.writeFileSync(path.join(directory, 'output/template.ts'), "import template from './restore.ps1?raw'")
    fs.writeFileSync(path.join(directory, 'transfer/job.ts'), "import template from '../output/restore.ps1?raw'")
    fs.writeFileSync(path.join(directory, 'ui/view.ts'), "import './view.css'\nimport missing from './missing.ps1?raw'")
    fs.writeFileSync(path.join(directory, 'ui/view.css'), ':root { color: black; }')
    const graph = loadSourceGraph(directory, { moduleResolution: ts.ModuleResolutionKind.Bundler })
    assert.deepEqual(graph.get('output/template.ts'), ['output/restore.ps1?raw'])
    assert.deepEqual(findBoundaryViolations(graph), [['transfer/job.ts', 'output/restore.ps1?raw']])
    assert.deepEqual(findBoundaryViolations(graph, {
      roots: ['transfer'], forbidden: ['output/restore.ps1'],
    }), [['transfer/job.ts', 'output/restore.ps1?raw']])
    assert.deepEqual(graph.get('ui/view.ts'), ['ui/view.css', '<unresolved import: ./missing.ps1?raw>'])
  } finally {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})
