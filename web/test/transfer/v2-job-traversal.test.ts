import { describe, expect, it, vi } from 'vitest'

import { MAXIMUM_DIRECTORY_PAGES } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import {
  CATALOG_DIRECTORY_ENTRIES,
  type CatalogEntry,
  type CatalogPage,
} from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  CatalogTraversalError,
  type DirectoryCursor,
} from '../../src/transfer/job/contract'
import { createAuthenticatedLogicalSiblingMembership } from '../../src/transfer/job/discovery'
import { CatalogPageCursor, CatalogTraversalGuard } from '../../src/transfer/job/traversal'

const SHARE = identity(1)
const DIRECTORY = identity(2)
const GENERATION = identity(3)
const FIRST_COMMITMENT = commitment(1)
const TERMINAL_COMMITMENT = commitment(2)

describe('v2 catalog generation cursor', () => {
  it('retains exact committed logical-sibling authority without eagerly querying the store', async () => {
    const directory = committed(1, 1)
    const signal = new AbortController().signal
    const hasCommittedName = vi.fn(async () => true)
    const membership = createAuthenticatedLogicalSiblingMembership(
      { hasCommittedName },
      directory,
      signal,
    )

    expect(membership).toMatchObject({
      directoryId: directory.directoryIdText,
      generation: directory.generationText,
    })
    expect(hasCommittedName).not.toHaveBeenCalled()

    await expect(membership.hasCommittedName('candidate.windshare-aaaaaa')).resolves.toBe(true)
    expect(hasCommittedName).toHaveBeenCalledWith(
      directory,
      'candidate.windshare-aaaaaa',
      signal,
    )
  })

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
      .toThrow(CatalogTraversalError)
    expect(consumed).toEqual([first.name])
  })

  it.each([
    fileEntry(4, 'e\u0301'),
    fileEntry(4, '../outside'),
  ])('rejects an invalid catalog sibling name before traversal', (entry) => {
    const pages = new CatalogPageCursor(SHARE, directoryCursor(), committed(1, 1))
    expect(() => pages.accept(singlePage([entry])))
      .toThrow(CatalogTraversalError)
  })

  it.each(['CON', 'question?.txt', 'trailing-space ', 'trailing-dot.'])(
    'retains %s for destination-specific output handling', (name) => {
      const pages = new CatalogPageCursor(SHARE, directoryCursor(), committed(1, 1))
      expect(() => pages.accept(singlePage([fileEntry(4, name)]))).not.toThrow()
      expect(() => pages.finish()).not.toThrow()
    },
  )

  it('applies the reserved output namespace policy while composing entry paths', () => {
    const guard = new CatalogTraversalGuard(SHARE, 1)
    expect(() => guard.entryPath(directoryCursor(), fileEntry(4, '.WSRESUME-state')))
      .toThrow(/path policy/u)
  })

  it.each([
    { pageCount: 0 },
    { pageCount: MAXIMUM_DIRECTORY_PAGES + 1 },
    { entryCount: CATALOG_DIRECTORY_ENTRIES + 1 },
    { terminalCommitment: new Uint8Array(32) },
    { omittedCount: BigInt(CATALOG_DIRECTORY_ENTRIES) },
  ])('rejects malformed committed cursor authority %#', (override) => {
    expect(() => new CatalogPageCursor(
      SHARE,
      directoryCursor(),
      { ...committed(1, 1), ...override },
    )).toThrow(CatalogTraversalError)
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
    const pages = new CatalogPageCursor(SHARE, directoryCursor(), committed(1, 1))
    expect(() => pages.accept({ ...singlePage([fileEntry(4, 'alpha')]), ...override }))
      .toThrow(CatalogTraversalError)
  })
})

function cursor(): CatalogPageCursor {
  return new CatalogPageCursor(SHARE, directoryCursor(), committed(2, 2))
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

function committed(pageCount: number, entryCount: number): CommittedDirectory {
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
  entries: readonly CatalogEntry[],
): CatalogPage {
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

function singlePage(entries: readonly CatalogEntry[]): CatalogPage {
  return page(0, true, entries)
}

function fileEntry(first: number, name: string): Extract<CatalogEntry, { kind: 'file' }> {
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
