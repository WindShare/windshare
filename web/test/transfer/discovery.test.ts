import { describe, expect, it } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { frozenSelectionPolicy, SelectionPolicy, type FrozenSelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { discoverSelection, inspectSelectionShape } from '../../src/transfer/discovery'

interface TreeDirSpec {
  readonly seed: number
  readonly name: string
  readonly pages?: readonly (readonly TreeNodeSpec[])[]
  readonly entries?: readonly TreeNodeSpec[]
  readonly failLoad?: boolean
}

interface TreeFileSpec {
  readonly seed: number
  readonly name: string
  readonly size: bigint
}

type TreeNodeSpec =
  | ({ readonly kind: 'directory' } & TreeDirSpec)
  | ({ readonly kind: 'file' } & TreeFileSpec)

function makeId(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(16)
  bytes[0] = seed & 0xff
  bytes[1] = (seed >> 8) & 0xff
  bytes[15] = 1
  return bytes
}

function makeCatalogTree(rootPages: readonly (readonly TreeNodeSpec[])[]) {
  const shareInstance = makeId(1)
  const rootId = makeId(2)
  const rootIdText = encodeBase64Url(rootId)
  const directories = new Map<string, { readonly committed: CommittedDirectory; readonly pages: readonly CatalogPage[]; readonly failLoad: boolean }>()
  const entriesBySeed = new Map<number, CatalogEntry>()

  function buildDirectory(dirId: Uint8Array<ArrayBuffer>, rawPages: readonly (readonly TreeNodeSpec[])[], failLoad: boolean): void {
    const dirIdText = encodeBase64Url(dirId)
    const generation = makeId(dirId[0]! + 200)
    const generationText = encodeBase64Url(generation)
    const builtPages: CatalogPage[] = []
    let entryCount = 0

    for (let pageIndex = 0; pageIndex < rawPages.length; pageIndex++) {
      const pageSpecs = rawPages[pageIndex]!
      const entries: CatalogEntry[] = pageSpecs.map(spec => {
        const entryId = makeId(spec.seed)
        const idText = encodeBase64Url(entryId)
        if (spec.kind === 'file') {
          const entry: CatalogEntry = Object.freeze({
            kind: 'file',
            id: entryId,
            idText,
            name: spec.name,
            expectedSize: spec.size,
          })
          entriesBySeed.set(spec.seed, entry)
          return entry
        }
        const entry: CatalogEntry = Object.freeze({
          kind: 'directory',
          id: entryId,
          idText,
          name: spec.name,
        })
        entriesBySeed.set(spec.seed, entry)
        const childPages = spec.pages ?? [spec.entries ?? []]
        buildDirectory(entryId, childPages, spec.failLoad ?? false)
        return entry
      })
      entryCount += entries.length
      builtPages.push(Object.freeze({
        shareInstance,
        directoryId: dirId,
        directoryIdText: dirIdText,
        generation,
        generationText,
        pageIndex,
        terminal: pageIndex === rawPages.length - 1,
        previousCommitment: new Uint8Array(32),
        entries: Object.freeze(entries),
        omittedCount: 0n,
        objectCommitment: new Uint8Array(32),
        encodedBytes: 64,
      }))
    }

    const committed: CommittedDirectory = Object.freeze({
      directoryId: dirId,
      directoryIdText: dirIdText,
      generation,
      generationText,
      pageCount: builtPages.length,
      entryCount,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    })
    directories.set(dirIdText, { committed, pages: builtPages, failLoad })
  }

  buildDirectory(rootId, rootPages, false)

  const loadedDirectories: string[] = []
  const loadedPages: string[] = []
  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: rootIdText,
  }) as unknown as ShareDescriptor

  const catalog: CatalogClient = Object.freeze({
    loadDirectory: async (directoryId: Uint8Array) => {
      const idText = encodeBase64Url(directoryId)
      loadedDirectories.push(idText)
      const record = directories.get(idText)
      if (record === undefined) throw new Error(`Missing test directory ${idText}`)
      if (record.failLoad) throw new Error(`Directory ${idText} should not have been loaded`)
      return record.committed
    },
    page: async (committed: CommittedDirectory, pageIndex: number) => {
      loadedPages.push(`${committed.directoryIdText}#${pageIndex}`)
      const record = directories.get(committed.directoryIdText)
      const page = record?.pages[pageIndex]
      if (page === undefined) throw new Error(`Missing page ${pageIndex} for ${committed.directoryIdText}`)
      return page
    },
  }) as unknown as CatalogClient

  return {
    descriptor,
    catalog,
    rootIdText,
    loadedDirectories,
    loadedPages,
    entry: (seed: number) => entriesBySeed.get(seed)!,
  }
}

describe('selection shape probing and progressive discovery', () => {
  it('preserves single-file classification when four unselected sibling directories sit beside the selected file', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 10, name: 'a-dir', entries: [{ kind: 'file', seed: 11, name: 'x.bin', size: 5n }] },
      { kind: 'directory', seed: 12, name: 'b-dir', entries: [] },
      { kind: 'directory', seed: 13, name: 'c-dir', entries: [] },
      { kind: 'directory', seed: 14, name: 'd-dir', entries: [] },
      { kind: 'file', seed: 15, name: 'invoice.pdf', size: 2_048n },
    ]])
    const invoice = tree.entry(15)
    const policy = new SelectionPolicy(false)
    policy.set(invoice, [tree.rootIdText], true)
    const frozen = policy.snapshot()

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, frozen, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'single-file',
      file: invoice,
      sourcePath: ['invoice.pdf'],
      exactBytes: 2_048n,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])

    // Even without draft ancestry hints (e.g. restored from IndexedDB), matching
    // every positive override at the root stops discovery before loading siblings.
    tree.loadedDirectories.length = 0
    const unhinted = frozenSelectionPolicy(false, frozen.canonicalRules)
    const unhintedShape = await inspectSelectionShape(tree.descriptor, tree.catalog, unhinted, new AbortController().signal)
    expect(unhintedShape).toEqual(shape)
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])

    tree.loadedDirectories.length = 0
    const discovered: string[] = []
    for await (const item of discoverSelection(tree.descriptor, tree.catalog, unhinted, [], new AbortController().signal)) {
      discovered.push(`${item.kind}:${item.relativePath.join('/')}`)
    }
    expect(discovered).toEqual(['file:invoice.pdf'])
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])
  })

  it('verifies deep explicitly selected single files along authenticated ancestor hints without scanning siblings', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 20, name: 'unused-root-1', failLoad: true },
      { kind: 'directory', seed: 21, name: 'unused-root-2', failLoad: true },
      {
        kind: 'directory',
        seed: 22,
        name: 'level1',
        entries: [
          { kind: 'directory', seed: 23, name: 'unused-l1', failLoad: true },
          {
            kind: 'directory',
            seed: 24,
            name: 'level2',
            entries: [{
              kind: 'directory',
              seed: 25,
              name: 'level3',
              entries: [{
                kind: 'directory',
                seed: 26,
                name: 'level4',
                entries: [{
                  kind: 'directory',
                  seed: 27,
                  name: 'level5',
                  entries: [
                    { kind: 'directory', seed: 28, name: 'unused-l5', failLoad: true },
                    { kind: 'file', seed: 29, name: 'invoice.pdf', size: 4_096n },
                  ],
                }],
              }],
            }],
          },
        ],
      },
    ]])
    const ancestry = [
      tree.rootIdText,
      tree.entry(22).idText,
      tree.entry(24).idText,
      tree.entry(25).idText,
      tree.entry(26).idText,
      tree.entry(27).idText,
    ]
    const invoice = tree.entry(29)
    const policy = new SelectionPolicy(false)
    policy.set(invoice, ancestry, true)
    const frozen = policy.snapshot()

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, frozen, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'single-file',
      file: invoice,
      sourcePath: ['level1', 'level2', 'level3', 'level4', 'level5', 'invoice.pdf'],
      exactBytes: 4_096n,
    })
    expect(tree.loadedDirectories).toEqual(ancestry)

    tree.loadedDirectories.length = 0
    const items: string[] = []
    for await (const item of discoverSelection(
      tree.descriptor,
      tree.catalog,
      frozen,
      shape.kind === 'single-file' ? shape.sourcePath.slice(0, -1) : [],
      new AbortController().signal,
    )) {
      items.push(`${item.kind}:${item.relativePath.join('/')}`)
    }
    expect(items).toEqual(['file:invoice.pdf'])
    expect(tree.loadedDirectories).toEqual(ancestry)
  })

  it('scopes folder probing and discovery to the selected folder without touching unrelated siblings', async () => {
    const tree = makeCatalogTree([[
      {
        kind: 'directory',
        seed: 30,
        name: 'project',
        entries: [
          { kind: 'directory', seed: 31, name: 'node_modules', failLoad: true },
          {
            kind: 'directory',
            seed: 32,
            name: 'docs',
            entries: [
              { kind: 'directory', seed: 33, name: 'drafts', failLoad: true },
              { kind: 'file', seed: 34, name: 'guide.md', size: 512n },
            ],
          },
        ],
      },
    ]])
    const policy = new SelectionPolicy(false)
    const projectId = tree.entry(30).idText
    const docs = tree.entry(32)
    policy.set(docs, [tree.rootIdText, projectId], true)
    policy.set(tree.entry(33), [tree.rootIdText, projectId, docs.idText], false)
    const frozen = policy.snapshot()

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, frozen, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'docs',
      rootPath: ['project', 'docs'],
      knownFileCount: 1,
      knownBytes: 512n,
      complete: true,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText, projectId, docs.idText])

    tree.loadedDirectories.length = 0
    const failedDirs: string[] = []
    const items: string[] = []
    const sources: string[] = []
    for await (const item of discoverSelection(
      tree.descriptor,
      tree.catalog,
      frozen,
      shape.kind === 'tree' ? shape.rootPath : [],
      new AbortController().signal,
      { onDirectoryFailed: id => failedDirs.push(id) },
    )) {
      items.push(`${item.kind}:${item.relativePath.join('/')}`)
      sources.push(`${item.kind}:${item.sourcePath.join('/')}`)
    }
    expect(failedDirs).toEqual([])
    expect(items).toEqual(['directory:', 'file:guide.md'])
    expect(sources).toEqual(['directory:project/docs', 'file:project/docs/guide.md'])
    expect(tree.loadedDirectories).toEqual([tree.rootIdText, projectId, docs.idText])
  })

  it('falls back to sibling search when a draft ancestry hint is stale so hints never hide selected targets', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 40, name: 'stale-dir', entries: [] },
      {
        kind: 'directory',
        seed: 41,
        name: 'actual-dir',
        entries: [{ kind: 'file', seed: 42, name: 'moved.pdf', size: 1_024n }],
      },
    ]])
    const moved = tree.entry(42)
    const basePolicy = frozenSelectionPolicy(false, [{
      kind: 'file',
      id: moved.id.slice(),
      selected: true,
    }])
    const staleHintPolicy: FrozenSelectionPolicy = Object.freeze<FrozenSelectionPolicy>({
      defaultSelected: basePolicy.defaultSelected,
      canonicalRules: basePolicy.canonicalRules,
      draftRules: Object.freeze([{
        kind: 'file' as const,
        id: moved.id.slice(),
        selected: true,
        ancestry: Object.freeze([tree.rootIdText, tree.entry(40).idText]),
      }]),
      selected: (entry, ancestry) => basePolicy.selected(entry, ancestry),
      directorySelected: (dirId, ancestry) => basePolicy.directorySelected(dirId, ancestry),
      decision: (entry, ancestry) => basePolicy.decision(entry, ancestry),
      shouldDiscover: (dirId, ancestry) => basePolicy.shouldDiscover(dirId, ancestry),
    })

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, staleHintPolicy, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'single-file',
      file: moved,
      sourcePath: ['actual-dir', 'moved.pdf'],
      exactBytes: 1_024n,
    })

    const items: string[] = []
    for await (const item of discoverSelection(
      tree.descriptor,
      tree.catalog,
      staleHintPolicy,
      shape.kind === 'single-file' ? shape.sourcePath.slice(0, -1) : [],
      new AbortController().signal,
    )) {
      items.push(`${item.kind}:${item.sourcePath.join('/')}`)
    }
    expect(items).toEqual(['file:actual-dir/moved.pdf'])
  })

  it('locates a re-selected file inside an excluded branch without scanning unrelated excluded directories', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 50, name: 'normal', entries: [{ kind: 'file', seed: 51, name: 'a.txt', size: 10n }] },
      {
        kind: 'directory',
        seed: 52,
        name: 'excluded-with-keep',
        entries: [
          { kind: 'directory', seed: 53, name: 'ignored-sub', failLoad: true },
          { kind: 'file', seed: 54, name: 'keep.pdf', size: 20n },
        ],
      },
      { kind: 'directory', seed: 55, name: 'excluded-other', failLoad: true },
    ]])
    const policy = new SelectionPolicy(true)
    const excludedWithKeep = tree.entry(52)
    policy.set(excludedWithKeep, [tree.rootIdText], false)
    policy.set(tree.entry(55), [tree.rootIdText], false)
    policy.set(tree.entry(54), [tree.rootIdText, excludedWithKeep.idText], true)
    const frozen = policy.snapshot()

    const items: string[] = []
    for await (const item of discoverSelection(tree.descriptor, tree.catalog, frozen, [], new AbortController().signal)) {
      items.push(`${item.kind}:${item.relativePath.join('/')}`)
    }
    expect(items).toEqual(['directory:normal', 'file:excluded-with-keep/keep.pdf', 'file:normal/a.txt'])
    expect(tree.loadedDirectories).toEqual([tree.rootIdText, excludedWithKeep.idText, tree.entry(50).idText])
  })

  it('reads across unselected parent pages until all positive targets match and stops before unnecessary later pages', async () => {
    const tree = makeCatalogTree([
      [
        { kind: 'directory', seed: 60, name: 'a-dir', failLoad: true },
        { kind: 'directory', seed: 61, name: 'b-dir', failLoad: true },
        { kind: 'directory', seed: 62, name: 'c-dir', failLoad: true },
        { kind: 'directory', seed: 63, name: 'd-dir', failLoad: true },
      ],
      [
        { kind: 'file', seed: 64, name: 'page-two.pdf', size: 99n },
      ],
      [
        { kind: 'directory', seed: 65, name: 'page-three-dir', failLoad: true },
      ],
    ])
    const target = tree.entry(64)
    const policy = new SelectionPolicy(false)
    policy.set(target, [tree.rootIdText], true)
    const frozen = policy.snapshot()

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, frozen, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'single-file',
      file: target,
      sourcePath: ['page-two.pdf'],
      exactBytes: 99n,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])
    expect(tree.loadedPages).not.toContain(`${tree.rootIdText}#2`)
  })
})
