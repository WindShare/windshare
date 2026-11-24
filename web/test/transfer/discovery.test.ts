import { describe, expect, it } from 'vitest'
import { DirectoryFailureError, type CatalogClient } from '../../src/catalog/client'
import {
  CatalogPageStoreError,
  MemoryCatalogPageStore,
  type CommittedDirectory,
} from '../../src/catalog/page-store'
import {
  DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
  type CatalogEntry,
  type CatalogPage,
  type ShareDescriptor,
} from '../../src/catalog/records'
import { frozenSelectionPolicy, SelectionPolicy, type FrozenSelectionPolicy } from '../../src/catalog/selection'
import { RemoteOperationError } from '../../src/content/session-operations'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createReceivedProtocolError } from '../../src/session/diagnostics'
import { createDiagnosticIdentity } from '../../src/session/identities'
import {
  CONSECUTIVE_CAPACITY_FAILURE_LIMIT,
  discoverSelection,
  inspectSelectionShape,
} from '../../src/transfer/discovery'

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
      knownFileCount: 0,
      knownBytes: 0n,
      complete: false,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText, projectId])

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

function treeWithFirstDirectoryFailure(failureForFirst: Error) {
  const tree = makeCatalogTree([[
    { kind: 'directory', seed: 120, name: 'dir-1', entries: [] },
    { kind: 'directory', seed: 121, name: 'dir-2', entries: [{ kind: 'file', seed: 122, name: 'later.txt', size: 10n }] },
    { kind: 'directory', seed: 123, name: 'dir-3', entries: [{ kind: 'file', seed: 124, name: 'also-ok.txt', size: 20n }] },
  ]])
  const firstIdText = tree.entry(120).idText
  const loaded: string[] = []
  const catalog: CatalogClient = Object.freeze({
    loadDirectory: async (directoryId: Uint8Array) => {
      const idText = encodeBase64Url(directoryId)
      loaded.push(idText)
      if (idText === firstIdText) throw failureForFirst
      return tree.catalog.loadDirectory(directoryId)
    },
    page: (committed: CommittedDirectory, pageIndex: number) => tree.catalog.page(committed, pageIndex),
  }) as unknown as CatalogClient
  return { tree, catalog, loaded, firstIdText }
}

describe('folder offer probing and directory failure tolerance', () => {
  it('propagates a relay denial before probing or downloading later directories', async () => {
    const denied = new RelayReceiverError('denied', { relayError: { code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0 } })
    const { tree, catalog, loaded, firstIdText } = treeWithFirstDirectoryFailure(denied)
    const selection = new SelectionPolicy(false)
    selection.set(tree.entry(122), [tree.rootIdText, tree.entry(121).idText], true)
    const unhinted = frozenSelectionPolicy(false, selection.snapshot().canonicalRules)
    const signal = new AbortController().signal
    await expect(inspectSelectionShape(tree.descriptor, catalog, unhinted, signal)).rejects.toBe(denied)
    expect(loaded).toEqual([tree.rootIdText, firstIdText])

    loaded.length = 0
    const discovery = discoverSelection(tree.descriptor, catalog, unhinted, [], signal)
    await expect(discovery.next()).rejects.toBe(denied)
    expect(loaded).toEqual([tree.rootIdText, firstIdText])
  })

  it('offers folder downloads without probing unreadable or slow selected subtrees', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 70, name: 'photos', entries: [{ kind: 'file', seed: 71, name: 'ok.jpg', size: 128n }] },
      { kind: 'directory', seed: 72, name: 'unreadable', failLoad: true },
    ]])
    const wholeShare = new SelectionPolicy(true).snapshot()

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, wholeShare, new AbortController().signal)
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'windshare-download',
      rootPath: [],
      knownFileCount: 0,
      knownBytes: 0n,
      complete: false,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])

    // Transfer discovery still delivers the readable photo and reports the failed subtree.
    tree.loadedDirectories.length = 0
    const failedDirs: string[] = []
    const discovered: string[] = []
    for await (const item of discoverSelection(
      tree.descriptor,
      tree.catalog,
      wholeShare,
      shape.kind === 'tree' ? shape.rootPath : [],
      new AbortController().signal,
      { onDirectoryFailed: id => failedDirs.push(id) },
    )) {
      discovered.push(`${item.kind}:${item.relativePath.join('/')}`)
    }
    expect(discovered).toEqual(['directory:photos', 'directory:unreadable', 'file:photos/ok.jpg'])
    expect(failedDirs).toEqual([tree.entry(72).idText])
  })

  it('anchors a selected single folder immediately without loading its contents', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 80, name: 'Movies', failLoad: true },
    ]])
    const movies = tree.entry(80)
    const policy = new SelectionPolicy(false)
    policy.set(movies, [tree.rootIdText], true)

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, policy.snapshot(), new AbortController().signal)
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'Movies',
      rootPath: ['Movies'],
      knownFileCount: 0,
      knownBytes: 0n,
      complete: false,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])
  })

  it('tolerates unreadable unselected siblings during unhinted fallback search and fails when nothing is confirmed', async () => {
    const tree = makeCatalogTree([[
      { kind: 'directory', seed: 90, name: 'broken-sibling', failLoad: true },
      {
        kind: 'directory',
        seed: 91,
        name: 'healthy-dir',
        entries: [{ kind: 'file', seed: 92, name: 'report.pdf', size: 256n }],
      },
    ]])
    const report = tree.entry(92)
    const missingId = makeId(99)

    // Found target in healthy sibling: the failed unselected sibling cannot hold any target.
    const foundPolicy = frozenSelectionPolicy(false, [{ kind: 'file', id: report.id.slice(), selected: true }])
    const foundShape = await inspectSelectionShape(tree.descriptor, tree.catalog, foundPolicy, new AbortController().signal)
    expect(foundShape).toEqual({
      kind: 'single-file',
      file: report,
      sourcePath: ['healthy-dir', 'report.pdf'],
      exactBytes: 256n,
    })

    // One confirmed target plus one unlocated target when another directory failed: degrade to incomplete tree.
    const partialPolicy = frozenSelectionPolicy(false, [
      { kind: 'file', id: report.id.slice(), selected: true },
      { kind: 'file', id: missingId.slice(), selected: true },
    ])
    const partialShape = await inspectSelectionShape(tree.descriptor, tree.catalog, partialPolicy, new AbortController().signal)
    expect(partialShape).toEqual({
      kind: 'tree',
      suggestedName: 'windshare-download',
      rootPath: [],
      knownFileCount: 1,
      knownBytes: 256n,
      complete: false,
    })

    // Zero confirmed targets with a directory failure: surface the error instead of claiming empty.
    const unreachablePolicy = frozenSelectionPolicy(false, [{ kind: 'file', id: missingId.slice(), selected: true }])
    await expect(
      inspectSelectionShape(tree.descriptor, tree.catalog, unreachablePolicy, new AbortController().signal),
    ).rejects.toThrow(`Directory ${tree.entry(90).idText} should not have been loaded`)
  })

  it('follows hinted re-selected descendants inside a selected folder without touching unrelated siblings', async () => {
    const tree = makeCatalogTree([[
      {
        kind: 'directory',
        seed: 100,
        name: 'project',
        entries: [
          { kind: 'directory', seed: 101, name: 'unreadable-child', failLoad: true },
          {
            kind: 'directory',
            seed: 102,
            name: 'excluded-sub',
            entries: [{ kind: 'file', seed: 103, name: 'keep.pdf', size: 42n }],
          },
        ],
      },
      { kind: 'directory', seed: 104, name: 'unrelated-sibling', failLoad: true },
    ]])
    const project = tree.entry(100)
    const excludedSub = tree.entry(102)
    const keep = tree.entry(103)
    const policy = new SelectionPolicy(false)
    policy.set(project, [tree.rootIdText], true)
    policy.set(excludedSub, [tree.rootIdText, project.idText], false)
    policy.set(keep, [tree.rootIdText, project.idText, excludedSub.idText], true)

    const shape = await inspectSelectionShape(tree.descriptor, tree.catalog, policy.snapshot(), new AbortController().signal)
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'project',
      rootPath: ['project'],
      knownFileCount: 1,
      knownBytes: 42n,
      complete: false,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText, project.idText, excludedSub.idText])
  })

  it('stops scanning later pages of a selected multi-page directory once the tree root is settled', async () => {
    const tree = makeCatalogTree([
      [
        { kind: 'directory', seed: 110, name: 'dir-a', failLoad: true },
        { kind: 'directory', seed: 111, name: 'dir-b', failLoad: true },
      ],
      [
        { kind: 'directory', seed: 112, name: 'dir-c', failLoad: true },
      ],
    ])

    const shape = await inspectSelectionShape(
      tree.descriptor,
      tree.catalog,
      new SelectionPolicy(true).snapshot(),
      new AbortController().signal,
    )
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'windshare-download',
      rootPath: [],
      knownFileCount: 0,
      knownBytes: 0n,
      complete: false,
    })
    expect(tree.loadedDirectories).toEqual([tree.rootIdText])
    expect(tree.loadedPages).toEqual([`${tree.rootIdText}#0`])
  })
})

describe('catalog capacity isolation and saturation halting', () => {
  it('isolates a single oversized directory capacity failure and continues past healthy siblings', async () => {
    const singleDirectoryCapacityCases: readonly Error[] = [
      new CatalogPageStoreError('resource-limit', 'Catalog cache exceeded its share entry or metadata budget'),
      new DirectoryFailureError(Object.freeze({
        shareInstance: makeId(1),
        directoryId: makeId(120),
        directoryIdText: encodeBase64Url(makeId(120)),
        attemptId: makeId(200),
        attemptIdText: encodeBase64Url(makeId(200)),
        code: DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
        kind: 'resource-limit',
        retryable: false,
      })),
      new RemoteOperationError(createReceivedProtocolError({
        requestKind: 'list_children',
        correlation: Object.freeze({
          protocolSessionId: createDiagnosticIdentity('protocol_session', makeId(9)),
          protocolOperationId: createDiagnosticIdentity('protocol_operation', 12n),
        }),
        content: {
          scope: 'directory',
          code: DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
          retryable: false,
        },
      })),
    ]

    for (const capacityError of singleDirectoryCapacityCases) {
      const { tree, catalog, loaded, firstIdText } = treeWithFirstDirectoryFailure(capacityError)
      const failed: unknown[] = []
      const discoveredFiles: string[] = []
      for await (const item of discoverSelection(
        tree.descriptor,
        catalog,
        new SelectionPolicy(true).snapshot(),
        [],
        new AbortController().signal,
        { onDirectoryFailed: (_id, error) => failed.push(error) },
      )) {
        if (item.kind === 'file') discoveredFiles.push(item.relativePath.join('/'))
      }
      expect(loaded).toEqual([
        tree.rootIdText,
        firstIdText,
        tree.entry(121).idText,
        tree.entry(123).idText,
      ])
      expect(discoveredFiles).toEqual(['dir-2/later.txt', 'dir-3/also-ok.txt'])
      expect(failed).toEqual([capacityError])
    }
  })

  it('halts immediately on a saturated browser cache while isolating oversized directories against MemoryCatalogPageStore', async () => {
    const wrapWithMemoryStore = (
      tree: ReturnType<typeof makeCatalogTree>,
      store: MemoryCatalogPageStore,
      encodedBytesByDir: ReadonlyMap<string, number>,
    ) => {
      const loaded: string[] = []
      const catalog: CatalogClient = Object.freeze({
        loadDirectory: async (directoryId: Uint8Array) => {
          const idText = encodeBase64Url(directoryId)
          loaded.push(idText)
          const existing = await store.loadDirectory(idText)
          if (existing !== undefined) return existing
          const committed = await tree.catalog.loadDirectory(directoryId)
          await store.begin(idText)
          try {
            const encodedBytes = encodedBytesByDir.get(idText) ?? 64
            for (let pageIndex = 0; pageIndex < committed.pageCount; pageIndex += 1) {
              const rawPage = await tree.catalog.page(committed, pageIndex)
              await store.stage({ ...rawPage, encodedBytes })
            }
            await store.commit(committed)
            return committed
          } catch (error) {
            await store.abort(idText)
            throw error
          }
        },
        page: async (committed: CommittedDirectory, pageIndex: number) => {
          const stored = await store.loadPage(committed, pageIndex)
          if (stored === undefined) throw new Error(`Missing stored page ${pageIndex}`)
          return stored
        },
      }) as unknown as CatalogClient
      return { catalog, loaded }
    }

    // 1. Oversized multi-page directory aborts and releases staging memory so its healthy sibling commits.
    const oversizedTree = makeCatalogTree([[
      {
        kind: 'directory',
        seed: 130,
        name: 'huge-dir',
        pages: [
          [{ kind: 'file', seed: 131, name: 'h0.bin', size: 10n }],
          [{ kind: 'file', seed: 132, name: 'h1.bin', size: 10n }],
        ],
      },
      {
        kind: 'directory',
        seed: 133,
        name: 'healthy-dir',
        entries: [{ kind: 'file', seed: 134, name: 'keep.txt', size: 12n }],
      },
    ]])
    const oversizedStore = new MemoryCatalogPageStore({
      maxEntries: 32,
      // Root (~3.3 KiB) + 1 page of huge-dir (~4.6 KiB) fits; page 2 (~4.6 KiB) overflows
      // 11 KiB and rolls back, leaving ~7.7 KiB so healthy-dir (~2.8 KiB) still commits.
      maxMetadataBytes: 11_000,
    })
    const oversizedClient = wrapWithMemoryStore(
      oversizedTree,
      oversizedStore,
      new Map([[oversizedTree.entry(130).idText, 512]]),
    )
    const oversizedFailures: unknown[] = []
    const oversizedFiles: string[] = []
    for await (const item of discoverSelection(
      oversizedTree.descriptor,
      oversizedClient.catalog,
      new SelectionPolicy(true).snapshot(),
      [],
      new AbortController().signal,
      { onDirectoryFailed: (_id, error) => oversizedFailures.push(error) },
    )) {
      if (item.kind === 'file') oversizedFiles.push(item.relativePath.join('/'))
    }
    expect(oversizedFiles).toEqual(['healthy-dir/keep.txt'])
    expect(oversizedFailures).toHaveLength(1)
    expect(oversizedFailures[0]).toMatchObject({
      name: 'CatalogPageStoreError',
      kind: 'resource-limit',
      storeExhausted: false,
    })

    // 2. Truly saturated committed store halts immediately without querying remaining siblings.
    const saturatedTree = makeCatalogTree([[
      { kind: 'directory', seed: 140, name: 'fills-cache', entries: [{ kind: 'file', seed: 141, name: 'a.txt', size: 5n }] },
      { kind: 'directory', seed: 142, name: 'hits-ceiling', entries: [{ kind: 'file', seed: 143, name: 'b.txt', size: 5n }] },
      { kind: 'directory', seed: 144, name: 'never-queried', entries: [{ kind: 'file', seed: 145, name: 'c.txt', size: 5n }] },
    ]])
    // Root: 2048 + 3*512 + 256 = 3840 B; fills-cache: 2048 + 512 + 256 = 2816 B -> 6656 B committed.
    // Leaving 1344 B (< 2052 B minimum page footprint) marks hits-ceiling as storeExhausted.
    const saturatedStore = new MemoryCatalogPageStore({
      maxEntries: 32,
      maxMetadataBytes: 8_000,
    })
    const saturatedClient = wrapWithMemoryStore(saturatedTree, saturatedStore, new Map())
    const saturatedFailures: unknown[] = []
    const saturatedFiles: string[] = []
    for await (const item of discoverSelection(
      saturatedTree.descriptor,
      saturatedClient.catalog,
      new SelectionPolicy(true).snapshot(),
      [],
      new AbortController().signal,
      { onDirectoryFailed: (_id, error) => saturatedFailures.push(error) },
    )) {
      if (item.kind === 'file') saturatedFiles.push(item.relativePath.join('/'))
    }
    expect(saturatedFiles).toEqual(['fills-cache/a.txt'])
    expect(saturatedClient.loaded).toEqual([
      saturatedTree.rootIdText,
      saturatedTree.entry(140).idText,
      saturatedTree.entry(142).idText,
    ])
    expect(saturatedFailures).toHaveLength(1)
    expect(saturatedFailures[0]).toMatchObject({
      name: 'CatalogPageStoreError',
      kind: 'resource-limit',
      storeExhausted: true,
    })
  })

  it('halts after consecutive permanent capacity failures, resets after a healthy directory, and ignores retryable contention', async () => {
    const permanentFailureFor = (seed: number) => new DirectoryFailureError(Object.freeze({
      shareInstance: makeId(1),
      directoryId: makeId(seed),
      directoryIdText: encodeBase64Url(makeId(seed)),
      attemptId: makeId(seed + 50),
      attemptIdText: encodeBase64Url(makeId(seed + 50)),
      code: DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
      kind: 'resource-limit',
      retryable: false,
    }))
    const retryableFailureFor = (seed: number) => new DirectoryFailureError(Object.freeze({
      shareInstance: makeId(1),
      directoryId: makeId(seed),
      directoryIdText: encodeBase64Url(makeId(seed)),
      attemptId: makeId(seed + 80),
      attemptIdText: encodeBase64Url(makeId(seed + 80)),
      code: DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT,
      kind: 'resource-limit',
      retryable: true,
      retryAfterMilliseconds: 250,
    }))

    // 1. Consecutive permanent capacity failures (even across an unrelated local error) stop traversal.
    const saturatedTree = makeCatalogTree([[
      { kind: 'directory', seed: 150, name: 'cap-1', entries: [] },
      { kind: 'directory', seed: 151, name: 'local-perm', entries: [] },
      { kind: 'directory', seed: 152, name: 'cap-2', entries: [] },
      { kind: 'directory', seed: 153, name: 'cap-3', entries: [] },
      { kind: 'directory', seed: 154, name: 'cap-4', entries: [] },
      { kind: 'directory', seed: 155, name: 'unreached', entries: [{ kind: 'file', seed: 156, name: 'skip.txt', size: 1n }] },
    ]])
    const saturatedLoaded: string[] = []
    const saturatedCatalog: CatalogClient = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => {
        const idText = encodeBase64Url(directoryId)
        saturatedLoaded.push(idText)
        if (idText === saturatedTree.entry(151).idText) throw new Error('permission denied')
        if ([150, 152, 153, 154].some(seed => saturatedTree.entry(seed).idText === idText)) {
          throw permanentFailureFor(150)
        }
        return saturatedTree.catalog.loadDirectory(directoryId)
      },
      page: (committed: CommittedDirectory, pageIndex: number) => saturatedTree.catalog.page(committed, pageIndex),
    }) as unknown as CatalogClient
    const saturatedFiles: string[] = []
    for await (const item of discoverSelection(
      saturatedTree.descriptor,
      saturatedCatalog,
      new SelectionPolicy(true).snapshot(),
      [],
      new AbortController().signal,
    )) {
      if (item.kind === 'file') saturatedFiles.push(item.relativePath.join('/'))
    }
    expect(saturatedFiles).toEqual([])
    expect(CONSECUTIVE_CAPACITY_FAILURE_LIMIT).toBe(4)
    expect(saturatedLoaded).toEqual([
      saturatedTree.rootIdText,
      saturatedTree.entry(150).idText,
      saturatedTree.entry(151).idText,
      saturatedTree.entry(152).idText,
      saturatedTree.entry(153).idText,
      saturatedTree.entry(154).idText,
    ])

    // 2. A healthy sibling resets the consecutive counter so later healthy siblings still download.
    const intermittentTree = makeCatalogTree([[
      { kind: 'directory', seed: 160, name: 'big-1', entries: [] },
      { kind: 'directory', seed: 161, name: 'big-2', entries: [] },
      { kind: 'directory', seed: 162, name: 'big-3', entries: [] },
      { kind: 'directory', seed: 163, name: 'ok-mid', entries: [{ kind: 'file', seed: 164, name: 'mid.txt', size: 4n }] },
      { kind: 'directory', seed: 165, name: 'big-4', entries: [] },
      { kind: 'directory', seed: 166, name: 'big-5', entries: [] },
      { kind: 'directory', seed: 167, name: 'ok-end', entries: [{ kind: 'file', seed: 168, name: 'end.txt', size: 4n }] },
    ]])
    const bigIds = new Set([160, 161, 162, 165, 166].map(seed => intermittentTree.entry(seed).idText))
    const intermittentCatalog: CatalogClient = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => {
        const idText = encodeBase64Url(directoryId)
        if (bigIds.has(idText)) throw permanentFailureFor(160)
        return intermittentTree.catalog.loadDirectory(directoryId)
      },
      page: (committed: CommittedDirectory, pageIndex: number) => intermittentTree.catalog.page(committed, pageIndex),
    }) as unknown as CatalogClient
    const intermittentFiles: string[] = []
    for await (const item of discoverSelection(
      intermittentTree.descriptor,
      intermittentCatalog,
      new SelectionPolicy(true).snapshot(),
      [],
      new AbortController().signal,
    )) {
      if (item.kind === 'file') intermittentFiles.push(item.relativePath.join('/'))
    }
    expect(intermittentFiles).toEqual(['ok-mid/mid.txt', 'ok-end/end.txt'])

    // 3. Retryable budget contention never counts toward the permanent capacity stop threshold.
    const retryableTree = makeCatalogTree([[
      { kind: 'directory', seed: 170, name: 'busy-1', entries: [] },
      { kind: 'directory', seed: 171, name: 'busy-2', entries: [] },
      { kind: 'directory', seed: 172, name: 'busy-3', entries: [] },
      { kind: 'directory', seed: 173, name: 'busy-4', entries: [] },
      { kind: 'directory', seed: 174, name: 'busy-5', entries: [] },
      { kind: 'directory', seed: 175, name: 'healthy', entries: [{ kind: 'file', seed: 176, name: 'later.txt', size: 10n }] },
    ]])
    const busyIds = new Set([170, 171, 172, 173, 174].map(seed => retryableTree.entry(seed).idText))
    const retryableCatalog: CatalogClient = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => {
        const idText = encodeBase64Url(directoryId)
        if (busyIds.has(idText)) throw retryableFailureFor(170)
        return retryableTree.catalog.loadDirectory(directoryId)
      },
      page: (committed: CommittedDirectory, pageIndex: number) => retryableTree.catalog.page(committed, pageIndex),
    }) as unknown as CatalogClient
    const retryableFiles: string[] = []
    for await (const item of discoverSelection(
      retryableTree.descriptor,
      retryableCatalog,
      new SelectionPolicy(true).snapshot(),
      [],
      new AbortController().signal,
    )) {
      if (item.kind === 'file') retryableFiles.push(item.relativePath.join('/'))
    }
    expect(retryableFiles).toEqual(['healthy/later.txt'])
  })
})
