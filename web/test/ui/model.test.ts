import { describe, expect, it } from 'vitest'

import type { ManifestEntry } from '../../src/contracts'
import { EntrySelectionModel, SELECTION_PAGE_ROWS } from '../../src/ui/model'

const entries = [
  { kind: 'directory', path: 'folder', mtime: 0 },
  { kind: 'file', path: 'folder/a.txt', size: 3, mtime: 0 },
  { kind: 'directory', path: 'folder/nested', mtime: 0 },
  { kind: 'file', path: 'folder/nested/b.txt', size: 5, mtime: 0 },
  { kind: 'directory', path: 'empty', mtime: 0 },
  { kind: 'file', path: 'lone.txt', size: 7, mtime: 0 },
] as unknown as readonly ManifestEntry[]

describe('entry selection model', () => {
  it('defaults to every entry and reports exact selected bytes', () => {
    const model = new EntrySelectionModel(entries)
    const selection = model.defaultSelection()

    expect(model.selectors(selection)).toBeNull()
    expect(model.selectedBytes(selection)).toBe(15)
    expect(model.selectedEntryCount(selection)).toBe(6)
    expect(
      model.rowsWindow(selection, 0, entries.length).every((row) => row.selected && !row.partial),
    ).toBe(true)
  })

  it('cascades directories and derives partial ancestor state', () => {
    const model = new EntrySelectionModel(entries)
    const withoutNestedFile = model.toggle(model.defaultSelection(), 'folder/nested/b.txt')
    const rows = new Map(
      model.rowsWindow(withoutNestedFile, 0, entries.length).map((row) => [row.path, row]),
    )

    expect(rows.get('folder')).toMatchObject({ selected: false, partial: true })
    expect(rows.get('folder/nested')).toMatchObject({ selected: false, partial: false })
    expect(model.selectedBytes(withoutNestedFile)).toBe(10)
    expect(model.selectors(withoutNestedFile)).toEqual([
      'empty',
      'folder/a.txt',
      'lone.txt',
    ])

    const restored = model.toggle(withoutNestedFile, 'folder')
    expect(model.selectors(restored)).toBeNull()
  })

  it('collapses fully selected subtrees to their smallest directory selector', () => {
    const model = new EntrySelectionModel(entries)
    const withoutFolder = model.toggle(model.defaultSelection(), 'folder')
    const nestedOnly = model.toggle(withoutFolder, 'folder/nested')

    expect(model.selectors(nestedOnly)).toEqual([
      'empty',
      'folder/nested',
      'lone.txt',
    ])
    expect(model.selectedBytes(nestedOnly)).toBe(12)
  })

  it('is deterministic across manifest order and does not recurse through deep paths', () => {
    const reversed = new EntrySelectionModel([...entries].reverse())
    const regular = new EntrySelectionModel(entries)
    const regularSelection = regular.toggle(regular.defaultSelection(), 'folder/nested/b.txt')
    const reversedSelection = reversed.toggle(
      reversed.defaultSelection(),
      'folder/nested/b.txt',
    )

    expect(reversed.selectors(reversedSelection)).toEqual(regular.selectors(regularSelection))
    expect(reversed.selectedBytes(reversedSelection)).toBe(regular.selectedBytes(regularSelection))

    const segmentCount = 20_000
    const deepPath = `${new Array<string>(segmentCount).fill('d').join('/')}/file.txt`
    const deep = new EntrySelectionModel([
      { kind: 'file', path: deepPath, size: 1, mtime: 0 },
    ] as unknown as readonly ManifestEntry[])
    const selected = deep.defaultSelection()

    const deepRows = deep.rowsWindow(selected, 0, 1)
    expect(deepRows).toEqual([
      expect.objectContaining({ name: 'file.txt', indentLevel: 16, selected: true }),
    ])
    expect(deepRows[0]?.accessibleLabel.length).toBeLessThan(300)
    expect(deep.selectors(deep.toggle(selected, deepPath))).toEqual([])
  })

  it('projects only the requested row window for a wide authenticated manifest', () => {
    const entryCount = SELECTION_PAGE_ROWS * 3 + 17
    const wideEntries = Array.from({ length: entryCount }, (_, index) => ({
      kind: 'file' as const,
      path: `file-${index.toString().padStart(4, '0')}`,
      size: 1,
      mtime: 0,
    })) as unknown as readonly ManifestEntry[]
    const model = new EntrySelectionModel(wideEntries)
    const selection = model.defaultSelection()

    const page = model.rowsWindow(selection, SELECTION_PAGE_ROWS * 2, SELECTION_PAGE_ROWS)
    expect(model.entryCount).toBe(entryCount)
    expect(page).toHaveLength(SELECTION_PAGE_ROWS)
    expect(page[0]?.path).toBe('file-0400')
    expect(page.at(-1)?.path).toBe('file-0599')
  })

  it('does not cache aggregate state for a caller-owned mutable selection', () => {
    const model = new EntrySelectionModel(entries)
    const selection = [...model.defaultSelection()]

    expect(model.rowsWindow(selection, 0, 1)[0]?.selected).toBe(true)
    selection[1] = false
    expect(model.rowsWindow(selection, 0, 1)[0]?.selected).toBe(false)
  })
})
