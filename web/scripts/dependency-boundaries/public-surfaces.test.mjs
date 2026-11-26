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
const pureContracts = BOUNDARY_POLICIES.find(policy =>
  policy.name === 'pure contracts and security primitives do not depend on runtime implementations')
const networkContracts = BOUNDARY_POLICIES.find(policy =>
  policy.name === 'reusable layers outside network only depend on network contracts')

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

test('presentation services can call output while direct private output imports remain forbidden', () => {
  const graph = new Map([
    ['ui/view.ts', ['application/receive/coordinator.ts', 'diagnostics/service.ts', 'output/backends/fsa.ts']],
    ['application/receive/coordinator.ts', ['output/store/database.ts']],
    ['diagnostics/service.ts', ['output/store/database.ts']],
  ])
  assert.deepEqual(findBoundaryViolations(graph, presentation), [
    ['ui/view.ts', 'output/backends/fsa.ts'],
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

test('pure contracts and security primitives reject runtime implementations and diagnostics', () => {
  assert.deepEqual(findBoundaryViolations(new Map([
    ['protocol/turn-control.ts', ['network/contracts/stun-endpoint.ts']],
    ['crypto/suite02-link.ts', ['network/contracts/alias.ts']],
    ['network/contracts/stun-endpoint.ts', ['network/contracts/relay-endpoint.ts']],
    ['network/contracts/relay-endpoint.ts', ['crypto/suite02-link.ts', 'security/url-sanitizer.ts']],
    ['network/contracts/alias.ts', []],
    ['security/capability-redactor.ts', ['security/diagnostic-formatter.ts']],
    ['security/diagnostic-formatter.ts', ['security/url-sanitizer.ts']],
    ['security/url-sanitizer.ts', []],
  ]), pureContracts), [])

  for (const root of [
    'protocol/turn-control.ts', 'crypto/suite02-link.ts',
    'network/contracts/node-directory.ts', 'security/capability-redactor.ts',
  ]) {
    for (const dependency of ['diagnostics/runtime.ts', 'output/store/database.ts', 'preview/preview.ts']) {
      assert.deepEqual(findBoundaryViolations(new Map([
        [root, ['network/contracts/barrel.ts']],
        ['network/contracts/barrel.ts', [dependency]],
        [dependency, []],
      ]), pureContracts), [
        [root, 'network/contracts/barrel.ts', dependency],
        ['network/contracts/barrel.ts', dependency],
      ])
    }
  }
})

test('reusable layers outside network permit network contracts while rejecting network runtime I/O', () => {
  assert.deepEqual(findBoundaryViolations(new Map([
    ['network/directory.ts', ['network/directory-cache.ts', 'network/contracts/node-directory.ts']],
    ['network/directory-cache.ts', ['network/contracts/node-directory.ts']],
    ['network/contracts/node-directory.ts', ['security/url-sanitizer.ts']],
    ['security/url-sanitizer.ts', []],
  ]), networkContracts), [])

  for (const root of [
    'protocol/turn-control.ts', 'crypto/suite02-link.ts', 'network/contracts/node-directory.ts',
    'security/capability-redactor.ts', 'session/runtime.ts', 'output/model/offers.ts',
    'output/store/database.ts', 'transfer/job.ts', 'catalog/client.ts', 'content/broker.ts',
    'preview/preview.ts', 'connectivity/ice-policy/defaults.ts', 'receiver/browser-join.ts',
    'transport/relay/receiver.ts',
  ]) {
    assert.deepEqual(findBoundaryViolations(new Map([
      [root, ['network/contracts/alias.ts']],
      ['network/contracts/alias.ts', []],
    ]), networkContracts), [])

    for (const dependency of ['network/directory.ts', 'network/directory-cache.ts', 'network/future-runtime.ts']) {
      assert.deepEqual(findBoundaryViolations(new Map([
        [root, ['network/contracts/barrel.ts']],
        ['network/contracts/barrel.ts', [dependency]],
        [dependency, []],
      ]), networkContracts), [
        [root, 'network/contracts/barrel.ts', dependency],
        ['network/contracts/barrel.ts', dependency],
      ])
    }
  }
})

test('presentation delegates network runtime ownership while allowing network contracts', () => {
  assert.deepEqual(findBoundaryViolations(new Map([
    ['ui/controller/share-report.ts', ['application/network/report.ts', 'network/contracts/relay-endpoint.ts']],
    ['application/network/report.ts', ['network/directory.ts']],
    ['network/contracts/relay-endpoint.ts', ['security/url-sanitizer.ts']],
    ['network/directory.ts', ['network/directory-cache.ts']],
    ['network/directory-cache.ts', []],
    ['security/url-sanitizer.ts', []],
  ]), presentation), [])

  for (const dependency of [
    'network/directory.ts', 'network/directory-cache.ts',
    'network/future-runtime.ts', 'network/contracts-private/cache.ts', 'network/contracts.ts',
  ]) {
    assert.deepEqual(findBoundaryViolations(new Map([['ui/view.ts', [dependency]]]), presentation), [
      ['ui/view.ts', dependency],
    ])
  }

  assert.deepEqual(findBoundaryViolations(new Map([
    ['ui/view.ts', ['network/contracts/barrel.ts']],
    ['network/contracts/barrel.ts', ['network/directory.ts']],
    ['network/directory.ts', []],
  ]), presentation), [
    ['ui/view.ts', 'network/contracts/barrel.ts', 'network/directory.ts'],
  ])
})
