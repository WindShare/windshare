import { describe, expect, it, vi } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, byteRange, type ByteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { discoverSelection, inspectSelectionShape } from '../../src/transfer/discovery'
import { TransferJob } from '../../src/transfer/job'
import type { TransferFileRequest, TransferOutputFile, TransferOutputSession } from '../../src/transfer/output-port'

function id(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

function makeShareFixture(files: readonly { readonly seed: number; readonly name: string; readonly bytes: Uint8Array<ArrayBuffer>; readonly revSeed?: number }[]) {
  const shareInstance = id(1)
  const rootId = id(2)
  const entries: CatalogEntry[] = files.map(file => Object.freeze({
    kind: 'file' as const,
    id: id(file.seed),
    idText: encodeBase64Url(id(file.seed)),
    name: file.name,
    expectedSize: BigInt(file.bytes.byteLength),
  }))
  const committed: CommittedDirectory = Object.freeze({
    directoryId: rootId,
    directoryIdText: encodeBase64Url(rootId),
    generation: id(3),
    generationText: encodeBase64Url(id(3)),
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId: rootId,
    directoryIdText: committed.directoryIdText,
    generation: committed.generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze(entries),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: committed.directoryIdText,
  }) as unknown as ShareDescriptor
  const catalog = Object.freeze({
    loadDirectory: async () => committed,
    page: async () => page,
  }) as unknown as CatalogClient

  const byId = new Map(files.map(file => [encodeBase64Url(id(file.seed)), file]))
  const revisions: RevisionReader = {
    open: async (fileId: Uint8Array) => {
      const key = encodeBase64Url(fileId)
      const file = byId.get(key)
      if (file === undefined) throw new Error('missing file')
      const rev = id(file.revSeed ?? (file.seed + 50))
      const revDesc: FileRevisionDescriptor = Object.freeze({
        shareInstance,
        shareInstanceId: descriptor.shareInstanceId,
        fileId: id(file.seed),
        fileIdText: key,
        fileRevision: rev,
        fileRevisionText: encodeBase64Url(rev),
        exactSize: BigInt(file.bytes.byteLength),
        geometry: new FileGeometry(BigInt(file.bytes.byteLength), 8n),
      })
      return Object.freeze({
        descriptor: revDesc,
        leaseId: id(99),
        release: async () => undefined,
      })
    },
  }
  const readRanges: ByteRange[] = []
  const broker: BlockRangeReader = {
    readRange: async function* (revDesc, _leaseId, span) {
      readRanges.push(span)
      const file = byId.get(revDesc.fileIdText)!
      for (let pos = span.start; pos < span.end; pos += 4n) {
        const end = pos + 4n < span.end ? pos + 4n : span.end
        yield Object.freeze({
          offset: pos,
          data: file.bytes.subarray(Number(pos), Number(end)),
        })
      }
    },
  }
  return { descriptor, catalog, revisions, broker, readRanges }
}

describe('transfer discovery and job execution', () => {
  it('classifies single-file and multi-file selections cleanly', async () => {
    const single = makeShareFixture([{ seed: 10, name: 'photo.jpg', bytes: new Uint8Array([1, 2, 3, 4]) }])
    const shape = await inspectSelectionShape(single.descriptor, single.catalog, new SelectionPolicy(true).snapshot(), new AbortController().signal)
    expect(shape).toMatchObject({ kind: 'single-file', exactBytes: 4n })

    const empty = await inspectSelectionShape(single.descriptor, single.catalog, new SelectionPolicy(false).snapshot(), new AbortController().signal)
    expect(empty).toEqual({ kind: 'empty' })
  })

  it('downloads only missing byte ranges on resume and detects source revision changes', async () => {
    const payload = new Uint8Array([10, 11, 12, 13, 14, 15, 16, 17])
    const fixture = makeShareFixture([{ seed: 10, name: 'doc.bin', bytes: payload, revSeed: 60 }])
    const target = new Uint8Array(payload.byteLength)
    target.set(payload.subarray(0, 4), 0)

    let checkpointCalls = 0
    const output: TransferOutputSession = {
      sessionId: 'sess-1',
      capabilities: {
        maxConcurrentFiles: 2,
        periodicCheckpoint: { intervalBytes: 4n, intervalMilliseconds: 60_000 },
      },
      openFile: async (req: TransferFileRequest): Promise<TransferOutputFile> => ({
        completedRanges: [byteRange(0n, 4n)],
        alreadyCompleted: false,
        savedRevision: req.revision,
        savedSize: req.exactSize,
        writeAt: async (offset, data) => { target.set(data, Number(offset)) },
        checkpoint: async () => { checkpointCalls += 1 },
        commit: async () => undefined,
        pause: async () => undefined,
        abort: async () => undefined,
      }),
    }

    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: fixture.revisions,
      broker: fixture.broker,
      output,
    }).run()

    expect(result.status).toBe('completed')
    expect(result.completedFiles).toBe(1n)
    expect(checkpointCalls).toBe(1)
    expect(fixture.readRanges).toEqual([byteRange(4n, 8n)])
    expect([...target]).toEqual([...payload])

    // Now simulate a mismatched saved revision -> must report source-changed without overwriting.
    const staleOutput: TransferOutputSession = {
      ...output,
      openFile: async (): Promise<TransferOutputFile> => ({
        completedRanges: [byteRange(0n, 4n)],
        alreadyCompleted: false,
        savedRevision: encodeBase64Url(id(77)),
        savedSize: 8n,
        writeAt: async () => { throw new Error('must not write to changed revision') },
        checkpoint: async () => undefined,
        commit: async () => undefined,
        pause: async () => undefined,
        abort: async () => undefined,
      }),
    }
    const staleResult = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: staleOutput,
    }).run()
    expect(staleResult.status).toBe('source-changed')
    expect(staleResult.sourceRevisionFailures?.count).toBe(1n)
  })

  it('pauses cleanly mid-stream and preserves written ranges', async () => {
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
    const fixture = makeShareFixture([{ seed: 20, name: 'movie.bin', bytes: payload }])
    let pausedRanges: readonly ByteRange[] = []
    const jobRef: { current?: TransferJob } = {}

    let checkpointCalls = 0
    const output: TransferOutputSession = {
      sessionId: 'sess-pause',
      capabilities: { maxConcurrentFiles: 1 },
      openFile: async (): Promise<TransferOutputFile> => ({
        completedRanges: [],
        alreadyCompleted: false,
        writeAt: async () => { jobRef.current?.pause('user') },
        checkpoint: async () => { checkpointCalls += 1 },
        commit: async () => undefined,
        pause: async ranges => { pausedRanges = ranges },
        abort: async () => undefined,
      }),
    }

    const job = new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: fixture.revisions,
      broker: fixture.broker,
      output,
    })
    jobRef.current = job

    const result = await job.run()
    expect(result.status).toBe('paused')
    expect(result.pauseReason).toBe('user')
    expect(checkpointCalls).toBe(0)
    expect(pausedRanges).toEqual([byteRange(0n, 4n)])

    const discovered: string[] = []
    for await (const item of discoverSelection(fixture.descriptor, fixture.catalog, new SelectionPolicy(true).snapshot(), [], new AbortController().signal)) {
      if (item.kind === 'file') discovered.push(item.entry.name)
    }
    expect(discovered).toEqual(['movie.bin'])
  })

  it('skips periodic checkpoints during normal transfer when periodicCheckpoint is omitted', async () => {
    const payload = new Uint8Array([0x53, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11])
    const fixture = makeShareFixture([{ seed: 30, name: 'stream.bin', bytes: payload }])
    const target = new Uint8Array(payload.byteLength)
    let checkpointCalls = 0
    let commitCalls = 0

    const output: TransferOutputSession = {
      sessionId: 'sess-no-periodic',
      capabilities: { maxConcurrentFiles: 1 },
      openFile: async (): Promise<TransferOutputFile> => ({
        completedRanges: [],
        alreadyCompleted: false,
        writeAt: async (offset, data) => { target.set(data, Number(offset)) },
        checkpoint: async () => { checkpointCalls += 1 },
        commit: async () => { commitCalls += 1 },
        pause: async () => undefined,
        abort: async () => undefined,
      }),
    }

    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: fixture.revisions,
      broker: fixture.broker,
      output,
    }).run()

    expect(result.status).toBe('completed')
    expect(checkpointCalls).toBe(0)
    expect(commitCalls).toBe(1)
    expect([...target]).toEqual([...payload])
  })

  it('stamps periodic checkpoint completion time so a slow flush does not trigger back-to-back checkpoints', async () => {
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
    const fixture = makeShareFixture([{ seed: 31, name: 'slow.bin', bytes: payload }])
    let nowMs = 1_000
    let checkpointCalls = 0
    const nowSpy = vi.spyOn(Date, 'now').mockImplementation(() => nowMs)

    try {
      const output: TransferOutputSession = {
        sessionId: 'sess-slow-cp',
        capabilities: {
          maxConcurrentFiles: 1,
          periodicCheckpoint: { intervalBytes: 1_000n, intervalMilliseconds: 10_000 },
        },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          alreadyCompleted: false,
          writeAt: async offset => {
            // Slice 1 arrives after the 10 s threshold; subsequent slices arrive 1 s apart.
            nowMs += offset === 0n ? 11_000 : 1_000
          },
          checkpoint: async () => {
            checkpointCalls += 1
            // Simulate a slow flush exceeding intervalMilliseconds (15 s > 10 s).
            nowMs += 15_000
          },
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      }

      const result = await new TransferJob({
        descriptor: fixture.descriptor,
        catalog: fixture.catalog,
        selection: new SelectionPolicy(true).snapshot(),
        revisions: fixture.revisions,
        broker: fixture.broker,
        output,
      }).run()

      expect(result.status).toBe('completed')
      expect(checkpointCalls).toBe(1)
    } finally {
      nowSpy.mockRestore()
    }
  })

  it('derives result root paths accurately across single-folder, subfolder, multi-root, and partial selections', async () => {
    const shareInstance = id(1)
    const rootId = id(2)
    const photosId = id(10)
    const tripId = id(11)
    const otherId = id(12)
    const docsId = id(13)
    const photoAId = id(20)
    const photoBId = id(21)
    const tripOneId = id(22)
    const tripSubId = id(23)
    const tripTwoId = id(24)
    const docId = id(25)

    const dir = (rawId: Uint8Array<ArrayBuffer>, name: string): Extract<CatalogEntry, { kind: 'directory' }> =>
      Object.freeze({ kind: 'directory', id: rawId, idText: encodeBase64Url(rawId), name })
    const file = (rawId: Uint8Array<ArrayBuffer>, name: string, size: bigint): Extract<CatalogEntry, { kind: 'file' }> =>
      Object.freeze({ kind: 'file', id: rawId, idText: encodeBase64Url(rawId), name, expectedSize: size })

    const makeTreeCatalog = (tree: ReadonlyMap<string, readonly CatalogEntry[]>) => {
      const committedById = new Map<string, { committed: CommittedDirectory; page: CatalogPage }>()
      for (const [dirIdText, entries] of tree) {
        const rawDirId = dirIdText === encodeBase64Url(rootId) ? rootId : id(0)
        const committed: CommittedDirectory = Object.freeze({
          directoryId: rawDirId,
          directoryIdText: dirIdText,
          generation: id(3),
          generationText: encodeBase64Url(id(3)),
          pageCount: 1,
          entryCount: entries.length,
          omittedCount: 0n,
          terminalCommitment: new Uint8Array(32),
        })
        const page: CatalogPage = Object.freeze({
          shareInstance,
          directoryId: rawDirId,
          directoryIdText: dirIdText,
          generation: committed.generation,
          generationText: committed.generationText,
          pageIndex: 0,
          terminal: true,
          previousCommitment: new Uint8Array(32),
          entries: Object.freeze([...entries]),
          omittedCount: 0n,
          objectCommitment: new Uint8Array(32),
          encodedBytes: 64,
        })
        committedById.set(dirIdText, { committed, page })
      }
      const descriptor: ShareDescriptor = Object.freeze({
        shareInstance,
        shareInstanceId: encodeBase64Url(shareInstance),
        syntheticRoot: rootId,
        syntheticRootId: encodeBase64Url(rootId),
      }) as unknown as ShareDescriptor
      const catalog = Object.freeze({
        loadDirectory: async (dirId: Uint8Array) => committedById.get(encodeBase64Url(dirId))!.committed,
        page: async (committed: CommittedDirectory) => committedById.get(committed.directoryIdText)!.page,
      }) as unknown as CatalogClient
      return { descriptor, catalog }
    }

    const photosEntry = dir(photosId, 'Photos')
    const tripEntry = dir(tripId, 'Trip')
    const otherEntry = dir(otherId, 'Other')
    const docsEntry = dir(docsId, 'Docs')
    const photoA = file(photoAId, 'a.jpg', 10n)
    const photoB = file(photoBId, 'b.jpg', 20n)
    const tripOne = file(tripOneId, '1.jpg', 30n)
    const tripSub = dir(tripSubId, 'sub')
    const tripTwo = file(tripTwoId, '2.jpg', 40n)
    const docC = file(docId, 'c.pdf', 50n)

    // 1. Sole folder with multiple files and subdirs (triggers probe early-stop) still anchors at ['Photos'].
    const singleFolder = makeTreeCatalog(new Map([
      [encodeBase64Url(rootId), [photosEntry]],
      [photosEntry.idText, [photoA, photoB, tripEntry, otherEntry]],
      [tripEntry.idText, [tripOne, tripSub]],
      [tripSub.idText, [tripTwo]],
      [otherEntry.idText, []],
    ]))
    const wholeFolderShape = await inspectSelectionShape(
      singleFolder.descriptor,
      singleFolder.catalog,
      new SelectionPolicy(true).snapshot(),
      new AbortController().signal,
    )
    expect(wholeFolderShape).toMatchObject({ kind: 'tree', suggestedName: 'Photos', rootPath: ['Photos'] })

    const folderItems: Array<{ kind: string; relative: readonly string[]; source: readonly string[] }> = []
    for await (const item of discoverSelection(
      singleFolder.descriptor,
      singleFolder.catalog,
      new SelectionPolicy(true).snapshot(),
      wholeFolderShape.kind === 'tree' ? wholeFolderShape.rootPath : [],
      new AbortController().signal,
    )) {
      folderItems.push({ kind: item.kind, relative: item.relativePath, source: item.sourcePath })
    }
    expect(folderItems).toContainEqual({ kind: 'directory', relative: [], source: ['Photos'] })
    expect(folderItems).toContainEqual({ kind: 'file', relative: ['a.jpg'], source: ['Photos', 'a.jpg'] })
    expect(folderItems).toContainEqual({ kind: 'directory', relative: ['Trip', 'sub'], source: ['Photos', 'Trip', 'sub'] })
    expect(folderItems).toContainEqual({ kind: 'file', relative: ['Trip', 'sub', '2.jpg'], source: ['Photos', 'Trip', 'sub', '2.jpg'] })

    // 2. Selecting only subfolder Photos/Trip anchors at ['Photos', 'Trip'] with suggestedName 'Trip'.
    const tripPolicy = new SelectionPolicy(false)
    tripPolicy.set(tripEntry, [singleFolder.descriptor.syntheticRootId, photosEntry.idText], true)
    const tripShape = await inspectSelectionShape(
      singleFolder.descriptor,
      singleFolder.catalog,
      tripPolicy.snapshot(),
      new AbortController().signal,
    )
    expect(tripShape).toMatchObject({ kind: 'tree', suggestedName: 'Trip', rootPath: ['Photos', 'Trip'] })

    // 3. Selecting only two files inside Photos anchors at ['Photos'].
    const twoPhotosPolicy = new SelectionPolicy(false)
    twoPhotosPolicy.set(photoA, [singleFolder.descriptor.syntheticRootId, photosEntry.idText], true)
    twoPhotosPolicy.set(photoB, [singleFolder.descriptor.syntheticRootId, photosEntry.idText], true)
    const twoPhotosShape = await inspectSelectionShape(
      singleFolder.descriptor,
      singleFolder.catalog,
      twoPhotosPolicy.snapshot(),
      new AbortController().signal,
    )
    expect(twoPhotosShape).toMatchObject({ kind: 'tree', suggestedName: 'Photos', rootPath: ['Photos'] })

    // 4. Selecting only one file inside a folder with sibling subdirs remains single-file.
    const onePhotoPolicy = new SelectionPolicy(false)
    onePhotoPolicy.set(photoA, [singleFolder.descriptor.syntheticRootId, photosEntry.idText], true)
    const onePhotoShape = await inspectSelectionShape(
      singleFolder.descriptor,
      singleFolder.catalog,
      onePhotoPolicy.snapshot(),
      new AbortController().signal,
    )
    expect(onePhotoShape).toMatchObject({ kind: 'single-file', sourcePath: ['Photos', 'a.jpg'] })

    // 5. Multiple root files use synthetic root ('windshare-download') rather than first filename.
    const multiFiles = makeShareFixture([
      { seed: 41, name: 'a.jpg', bytes: new Uint8Array([1, 2]) },
      { seed: 42, name: 'b.jpg', bytes: new Uint8Array([3, 4]) },
    ])
    const multiFileShape = await inspectSelectionShape(
      multiFiles.descriptor,
      multiFiles.catalog,
      new SelectionPolicy(true).snapshot(),
      new AbortController().signal,
    )
    expect(multiFileShape).toMatchObject({ kind: 'tree', suggestedName: 'windshare-download', rootPath: [] })

    // 6. Multiple root folders use synthetic root, while unchecking Docs anchors cleanly at ['Photos'].
    const multiRoots = makeTreeCatalog(new Map([
      [encodeBase64Url(rootId), [photosEntry, docsEntry]],
      [photosEntry.idText, [photoA]],
      [docsEntry.idText, [docC]],
    ]))
    const bothRootsShape = await inspectSelectionShape(
      multiRoots.descriptor,
      multiRoots.catalog,
      new SelectionPolicy(true).snapshot(),
      new AbortController().signal,
    )
    expect(bothRootsShape).toMatchObject({ kind: 'tree', suggestedName: 'windshare-download', rootPath: [] })

    const onlyPhotosPolicy = new SelectionPolicy(true)
    onlyPhotosPolicy.set(docsEntry, [multiRoots.descriptor.syntheticRootId], false)
    const onlyPhotosShape = await inspectSelectionShape(
      multiRoots.descriptor,
      multiRoots.catalog,
      onlyPhotosPolicy.snapshot(),
      new AbortController().signal,
    )
    expect(onlyPhotosShape).toMatchObject({ kind: 'tree', suggestedName: 'Photos', rootPath: ['Photos'] })
  })
})
