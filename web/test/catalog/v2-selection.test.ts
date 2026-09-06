import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'

const MILLION_SIBLING_ENTRIES = 1_000_000

function directory(id: string, name = id): V2CatalogEntry {
  return { kind: 'directory', id: identity(id), idText: id, name }
}

function file(id: string, name = id): V2CatalogEntry {
  return { kind: 'file', id: identity(id), idText: id, name, expectedSize: 1n }
}

function identity(seed: string): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = seed.charCodeAt(0)
  return value
}

describe('v2 progressive selection rules', () => {
  it('applies a directory decision to descendants discovered later', () => {
    const policy = new V2SelectionPolicy(true)
    const folder = directory('folder')
    policy.toggle(folder, ['root'])

    const late = file('late')
    expect(policy.selected(late, ['root', 'folder'])).toBe(false)
    expect(policy.shouldDiscover('folder', ['root'])).toBe(false)
  })

  it('retains a selected known descendant below an unselected directory', () => {
    const policy = new V2SelectionPolicy(true)
    const folder = directory('folder')
    const child = file('child')
    policy.toggle(folder, ['root'])
    policy.toggle(child, ['root', 'folder'])

    expect(policy.state(folder, ['root'])).toBe('mixed')
    expect(policy.shouldDiscover('folder', ['root'])).toBe(true)
  })

  it('does not treat caller-captured ancestry as pruning authority for opaque targets', () => {
    const policy = new V2SelectionPolicy(true)
    const excluded = directory('excluded')
    const selected = file('selected')
    policy.toggle(excluded, ['root'])
    policy.toggle(selected, ['root', 'excluded'])

    const frozen = policy.snapshot()
    expect(frozen.shouldDiscover('candidate', ['root', 'unrelated-hint'])).toBe(true)
  })

  it('selects an entire mixed subtree and discards all descendant overrides', () => {
    const policy = new V2SelectionPolicy(false)
    const parent = directory('parent')
    const child = directory('child')
    policy.set(parent, ['root'], true)
    policy.set(child, ['root', 'parent'], false)
    policy.set(file('kept'), ['root', 'parent', 'child'], true)
    expect(policy.state(parent, ['root'])).toBe('mixed')
    expect(policy.intentSummary()).toEqual({
      allSelected: false, selectedFiles: 0, selectedFolders: 1, excludedItems: 1, empty: false,
    })
    const frozen = policy.snapshot()
    policy.toggle(parent, ['root'])
    expect(policy.state(parent, ['root'])).toBe('selected')
    expect(policy.explicitRuleCount).toBe(1)
    expect(policy.selected(file('late'), ['root', 'parent', 'child'])).toBe(true)
    expect(frozen.selected(file('other'), ['root', 'parent', 'child'])).toBe(false)
    policy.set(parent, ['root'], false)
    expect(policy.intentSummary().empty).toBe(true)
    expect(policy.selected(file('late'), ['root', 'parent', 'child'])).toBe(false)
  })

  it('summarizes selections and exclusions across pages without counting inherited descendants twice', () => {
    const policy = new V2SelectionPolicy(false)
    policy.set(directory('parent'), ['root'], true)
    policy.set(file('selected'), ['root', 'parent'], true)
    policy.set(file('excluded'), ['root', 'parent'], false)
    policy.set(file('elsewhere'), ['root'], true)
    expect(policy.intentSummary()).toEqual({
      allSelected: false, selectedFiles: 1, selectedFolders: 1, excludedItems: 1, empty: false,
    })
  })

  it('retains only explicit rules while evaluating a million sibling identities', () => {
    const policy = new V2SelectionPolicy(true)
    let selectedSiblings = 0
    for (let index = 0; index < MILLION_SIBLING_ENTRIES; index += 1) {
      if (policy.selected(file(`sibling-${index}`), ['root'])) selectedSiblings += 1
    }
    expect(selectedSiblings).toBe(MILLION_SIBLING_ENTRIES)
    expect(policy.explicitRuleCount).toBe(0)

    const selected = file('selected')
    policy.toggle(selected, ['root', 'nested'])
    expect(policy.explicitRuleCount).toBe(1)
    expect(policy.selected(selected, ['root', 'nested'])).toBe(false)
  })
})
