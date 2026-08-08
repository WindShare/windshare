import { describe, expect, it } from 'vitest'

import { V2_MAXIMUM_DIRECTORY_PAGES } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import {
  V2_CATALOG_DIRECTORY_ENTRIES,
  type V2CatalogEntry,
  type V2CatalogPage,
} from '../../src/catalog/v2-records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  V2CatalogTraversalError,
  type DirectoryCursor,
} from '../../src/transfer/job/contract'
import { V2CatalogPageCursor, V2CatalogTraversalGuard } from '../../src/transfer/job/traversal'

const SHARE = identity(1)
const DIRECTORY = identity(2)
const GENERATION = identity(3)
const FIRST_COMMITMENT = commitment(1)
const TERMINAL_COMMITMENT = commitment(2)

describe('v2 catalog generation cursor', () => {
  it('accepts canonical sibling order across a committed page boundary', () => {
    const pages = cursor()

    pages.accept(page(0, false, [fileEntry(4, 'alpha')]))
    pages.accept(page(1, true, [fileEntry(5, 'zeta')]))

    expect(() => pages.finish()).not.toThrow()
  })

  it.each([
    {
      name: 'cross-page byte order',
      first: fileEntry(4, 'zeta'),
      second: fileEntry(5, 'alpha'),
    },
    {
      name: 'portable case collision',
      first: fileEntry(4, 'Alpha'),
      second: fileEntry(5, 'alpha'),
    },
    {
      name: 'repeated node identity',
      first: fileEntry(4, 'alpha'),
      second: fileEntry(4, 'zeta'),
    },
  ])('rejects $name before the second page can be consumed', ({ first, second }) => {
    const pages = cursor()
    const consumed: string[] = []
    pages.accept(page(0, false, [first]))
    consumed.push(first.name)

    expect(() => pages.accept(page(1, true, [second])))
      .toThrow(V2CatalogTraversalError)
    expect(consumed).toEqual([first.name])
  })

  it.each([
    fileEntry(4, 'e\u0301'),
    fileEntry(4, 'CON'),
  ])('rejects a non-portable sibling name before traversal', (entry) => {
    const pages = new V2CatalogPageCursor(SHARE, directoryCursor(), committed(1, 1))
    expect(() => pages.accept(singlePage([entry])))
      .toThrow(V2CatalogTraversalError)
  })

  it('applies the reserved output namespace policy while composing entry paths', () => {
    const guard = new V2CatalogTraversalGuard(SHARE, 1)
    expect(() => guard.entryPath(directoryCursor(), fileEntry(4, '.WSRESUME-state')))
      .toThrow(/path policy/u)
  })

  it.each([
    { pageCount: 0 },
    { pageCount: V2_MAXIMUM_DIRECTORY_PAGES + 1 },
    { entryCount: V2_CATALOG_DIRECTORY_ENTRIES + 1 },
    { terminalCommitment: new Uint8Array(32) },
    { omittedCount: BigInt(V2_CATALOG_DIRECTORY_ENTRIES) },
  ])('rejects malformed committed cursor authority %#', (override) => {
    expect(() => new V2CatalogPageCursor(
      SHARE,
      directoryCursor(),
      { ...committed(1, 1), ...override },
    )).toThrow(V2CatalogTraversalError)
  })

  it.each([
    { pageIndex: 1 },
    { terminal: false },
    { shareInstance: identity(9) },
    { directoryId: identity(9) },
    { generation: identity(9) },
    { previousCommitment: commitment(9) },
    { objectCommitment: new Uint8Array(32) },
    { entries: [] },
  ])('rejects a page that changes committed cursor authority %#', (override) => {
    const pages = new V2CatalogPageCursor(SHARE, directoryCursor(), committed(1, 1))
    expect(() => pages.accept({ ...singlePage([fileEntry(4, 'alpha')]), ...override }))
      .toThrow(V2CatalogTraversalError)
  })
})

function cursor(): V2CatalogPageCursor {
  return new V2CatalogPageCursor(SHARE, directoryCursor(), committed(2, 2))
}

function directoryCursor(): DirectoryCursor {
  return Object.freeze({
    id: DIRECTORY,
    idText: encodeBase64Url(DIRECTORY),
    path: Object.freeze([]),
    ancestry: Object.freeze([encodeBase64Url(DIRECTORY)]),
    selected: true,
  })
}

function committed(pageCount: number, entryCount: number): V2CommittedDirectory {
  return Object.freeze({
    directoryId: DIRECTORY,
    directoryIdText: encodeBase64Url(DIRECTORY),
    generation: GENERATION,
    generationText: encodeBase64Url(GENERATION),
    pageCount,
    entryCount,
    omittedCount: 0n,
    terminalCommitment: TERMINAL_COMMITMENT,
  })
}

function page(
  pageIndex: number,
  terminal: boolean,
  entries: readonly V2CatalogEntry[],
): V2CatalogPage {
  return Object.freeze({
    shareInstance: SHARE,
    directoryId: DIRECTORY,
    directoryIdText: encodeBase64Url(DIRECTORY),
    generation: GENERATION,
    generationText: encodeBase64Url(GENERATION),
    pageIndex,
    terminal,
    previousCommitment: pageIndex === 0 ? new Uint8Array(32) : FIRST_COMMITMENT,
    entries,
    omittedCount: 0n,
    objectCommitment: terminal ? TERMINAL_COMMITMENT : FIRST_COMMITMENT,
    senderObjectBytes: 1,
  })
}

function singlePage(entries: readonly V2CatalogEntry[]): V2CatalogPage {
  return page(0, true, entries)
}

function fileEntry(first: number, name: string): Extract<V2CatalogEntry, { kind: 'file' }> {
  const id = identity(first)
  return Object.freeze({
    kind: 'file',
    id,
    idText: encodeBase64Url(id),
    name,
    expectedSize: 0n,
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function commitment(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(32)
  value[0] = first
  return value
}
