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
