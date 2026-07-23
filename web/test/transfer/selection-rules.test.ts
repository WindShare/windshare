import { describe, expect, it } from 'vitest'

import {
  directoryId,
  fileId,
  structuralCatalogNamePolicy,
} from '../../src/catalog/model'
import { ProgressiveCatalogTree } from '../../src/catalog/tree'
import { SelectionRules } from '../../src/transfer/selection-rules'

describe('inherited selection rules', () => {
  it('selects descendants discovered after a directory rule without expanding it', () => {
    const root = directoryId('root')
    const folder = directoryId('folder')
    const file = fileId('file')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    tree.publishDirectory({
      directoryId: root,
      generation: 'root',
      children: [{ kind: 'directory', id: folder, name: 'folder' }],
    })
    const rules = new SelectionRules(false).withDirectory(folder, true)
    expect(tree.isExpanded(folder)).toBe(false)

    tree.publishDirectory({
      directoryId: folder,
      generation: 'folder',
      children: [{ kind: 'file', id: file, name: 'later.bin', expectedSize: 1n }],
    })
    expect(rules.selected(tree, file)).toBe(true)
    expect(rules.state(tree, folder)).toBe('selected')
    expect(tree.isExpanded(folder)).toBe(false)
  })

  it('keeps immutable snapshots and reports a discovered override as mixed', () => {
    const { tree, folder, file } = fixture()
    const selected = new SelectionRules(true)
    const overridden = selected.withFile(file, false)

    expect(selected.selected(tree, file)).toBe(true)
    expect(selected.state(tree, folder)).toBe('selected')
    expect(overridden.selected(tree, file)).toBe(false)
    expect(overridden.state(tree, folder)).toBe('mixed')
  })

  it('discovers an unselected branch only when a known descendant override selects it', () => {
    const { tree, folder, file } = fixture()
    const none = new SelectionRules(false)
    expect(none.shouldDiscoverDirectory(tree, folder)).toBe(false)

    const oneFile = none.withFile(file, true)
    expect(oneFile.shouldDiscoverDirectory(tree, folder)).toBe(true)
    expect(oneFile.selected(tree, file)).toBe(true)
  })

  it('resolves unknown descendants from the nearest directory rule', () => {
    const root = directoryId('root')
    const parent = directoryId('parent')
    const child = directoryId('child')
    const rules = new SelectionRules(false)
      .withDirectory(root, true)
      .withDirectory(parent, false)
      .withDirectory(child, true)

    expect(rules.inheritedBy([root])).toBe(true)
    expect(rules.inheritedBy([root, parent])).toBe(false)
    expect(rules.inheritedBy([root, parent, child])).toBe(true)
  })
})

function fixture() {
  const root = directoryId('root')
  const folder = directoryId('folder')
  const file = fileId('file')
  const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
  tree.publishDirectory({
    directoryId: root,
    generation: 'root',
    children: [{ kind: 'directory', id: folder, name: 'folder' }],
  })
  tree.publishDirectory({
    directoryId: folder,
    generation: 'folder',
    children: [{ kind: 'file', id: file, name: 'file', expectedSize: 1n }],
  })
  return { tree, folder, file }
}
