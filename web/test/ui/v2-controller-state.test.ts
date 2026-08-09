import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { projectBrowsePage } from '../../src/ui/v2-controller-state'
import type { V2BrowseDirectory, V2BrowsePage } from '../../src/ui/v2-gateway'

describe('v2 browse page presentation', () => {
  it('reports only visible selection facts and leaves artifact semantics to projection', () => {
    const root = directory()
    const selected = file(2, 'selected.txt', 1_024n)
    const notSelected = file(3, 'other.txt', 2_048n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(selected, root.ancestry)

    const projection = projectBrowsePage(page(root, [selected, notSelected]), selection, [root])

    expect(projection.snapshot).toMatchObject({
      phase: 'browsing',
      selectedVisibleFiles: 1,
      selectedVisibleBytes: 1_024n,
    })
    expect(projection.snapshot.rows.map((row) => row.selection)).toEqual(['selected', 'unselected'])
  })
})

function directory(): V2BrowseDirectory {
  return Object.freeze({
    id: identity(1),
    idText: identityText(1),
    name: 'Shared files',
    path: Object.freeze([]),
    ancestry: Object.freeze([identityText(1)]),
  })
}

function page(
  root: V2BrowseDirectory,
  entries: readonly V2CatalogEntry[],
): V2BrowsePage {
  return Object.freeze({
    directory: root,
    pageIndex: 0,
    pageCount: 2,
    entryCount: entries.length,
    omittedCount: 0n,
    entries,
  })
}

function file(
  first: number,
  name: string,
  expectedSize: bigint,
): Extract<V2CatalogEntry, { kind: 'file' }> {
  return Object.freeze({
    kind: 'file',
    id: identity(first),
    idText: identityText(first),
    name,
    expectedSize,
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}
