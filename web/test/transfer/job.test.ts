import { describe, expect, it, vi } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import { CatalogPageStoreError, type CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { BlockLaneAttemptsError, type BlockRangeReader } from '../../src/content/broker'
import { BlockInactivityTimeoutError } from '../../src/content/scheduling/block-response-wait'
import { SessionRuntimeError } from '../../src/session/runtime-types'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { FileGeometry, byteRange, type ByteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import {
  RevisionReopenedDuringRecoveryError,
  type RevisionReader,
} from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { discoverSelection, inspectSelectionShape } from '../../src/transfer/discovery'
import { TransferJob } from '../../src/transfer/job'
import type { TransferFileRequest, TransferOutputFile, TransferOutputSession } from '../../src/transfer/output-port'

import { id, makeShareFixture, outputFile } from './share-fixture'

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
        resumable: true,
        partialOutput: true,
        periodicCheckpoint: { intervalBytes: 4n, intervalMilliseconds: 60_000 },
      },
      openFile: async (req: TransferFileRequest): Promise<TransferOutputFile> => ({
        completedRanges: [byteRange(0n, 4n)],
        savedRevision: req.revision,
        savedSize: req.exactSize,
        writeAt: async (offset, data) => { target.set(data, Number(offset)) },
        checkpoint: async () => { checkpointCalls += 1; return 'saved' },
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
        savedRevision: encodeBase64Url(id(77)),
        savedSize: 8n,
        writeAt: async () => { throw new Error('must not write to changed revision') },
        checkpoint: async () => 'saved',
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
      capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
      openFile: async (): Promise<TransferOutputFile> => ({
        completedRanges: [],
        writeAt: async () => { jobRef.current?.pause('user') },
        checkpoint: async () => { checkpointCalls += 1; return 'saved' },
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
      capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
      openFile: async (): Promise<TransferOutputFile> => ({
        completedRanges: [],
        writeAt: async (offset, data) => { target.set(data, Number(offset)) },
        checkpoint: async () => { checkpointCalls += 1; return 'saved' },
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
          resumable: true,
          partialOutput: true,
          periodicCheckpoint: { intervalBytes: 1_000n, intervalMilliseconds: 10_000 },
        },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async offset => {
            // Slice 1 arrives after the 10 s threshold; subsequent slices arrive 1 s apart.
            nowMs += offset === 0n ? 11_000 : 1_000
          },
          checkpoint: async () => {
            checkpointCalls += 1
            // Simulate a slow flush exceeding intervalMilliseconds (15 s > 10 s).
            nowMs += 15_000
            return 'saved'
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

describe('transfer job resume completion skip', () => {
  it('skips remote revision opens for locally completed files on resume even if a completed source vanished', async () => {
    const payloadA = new Uint8Array([1, 2, 3, 4])
    const payloadB = new Uint8Array([5, 6, 7, 8, 9, 10])
    const payloadC = new Uint8Array([11, 12, 13, 14, 15, 16, 17, 18])
    const fixture = makeShareFixture([
      { seed: 41, name: 'done-a.bin', bytes: payloadA, missing: true },
      { seed: 42, name: 'done-b.bin', bytes: payloadB },
      { seed: 43, name: 'pending-c.bin', bytes: payloadC },
    ])
    const completed = new Map<string, bigint>([
      [encodeBase64Url(id(41)), BigInt(payloadA.byteLength)],
      [encodeBase64Url(id(42)), BigInt(payloadB.byteLength)],
    ])
    const writtenC = new Uint8Array(payloadC.byteLength)
    const openedOutputs: string[] = []
    let lastProgressBytes = 0n

    const output: TransferOutputSession = {
      sessionId: 'sess-skip-completed',
      capabilities: { maxConcurrentFiles: 2, resumable: true, partialOutput: true },
      checkCompleted: async fileId => {
        const size = completed.get(fileId)
        return size === undefined ? undefined : { size }
      },
      openFile: async (req: TransferFileRequest): Promise<TransferOutputFile> => {
        openedOutputs.push(req.fileId)
        return {
          completedRanges: [],
          writeAt: async (offset, data) => { writtenC.set(data, Number(offset)) },
          checkpoint: async () => 'saved',
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }
      },
    }

    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: fixture.revisions,
      broker: fixture.broker,
      output,
      onProgress: progress => { lastProgressBytes = progress.materializedBytes },
    }).run()

    const pendingId = encodeBase64Url(id(43))
    expect(result.status).toBe('completed')
    expect(result.completedFiles).toBe(3n)
    expect(result.completedBytes).toBe(BigInt(payloadA.byteLength + payloadB.byteLength + payloadC.byteLength))
    expect(lastProgressBytes).toBe(result.completedBytes)
    expect(fixture.openedFileIds).toEqual([pendingId])
    expect(openedOutputs).toEqual([pendingId])
    expect([...writtenC]).toEqual([...payloadC])
  })
})

function makeDescriptor(input: Readonly<{
  shareInstance: Uint8Array<ArrayBuffer>
  fileId: Uint8Array<ArrayBuffer>
  revision: Uint8Array<ArrayBuffer>
  exactSize: bigint
  sourceMetadata?: string
}>): FileRevisionDescriptor {
  return Object.freeze({
    shareInstance: input.shareInstance,
    shareInstanceId: encodeBase64Url(input.shareInstance),
    fileId: input.fileId,
    fileIdText: encodeBase64Url(input.fileId),
    fileRevision: input.revision,
    fileRevisionText: encodeBase64Url(input.revision),
    exactSize: input.exactSize,
    geometry: new FileGeometry(input.exactSize, 4n),
    ...(input.sourceMetadata === undefined ? {} : { sourceMetadata: input.sourceMetadata }),
  })
}

function makeSingleFileCatalog(fileId: Uint8Array<ArrayBuffer>, name: string, exactSize: bigint) {
  const shareInstance = id(1)
  const rootId = id(2)
  const generation = id(3)
  const entry: CatalogEntry = Object.freeze({
    kind: 'file',
    id: fileId,
    idText: encodeBase64Url(fileId),
    name,
    expectedSize: exactSize,
  })
  const committed: CommittedDirectory = Object.freeze({
    directoryId: rootId,
    directoryIdText: encodeBase64Url(rootId),
    generation,
    generationText: encodeBase64Url(generation),
    pageCount: 1,
    entryCount: 1,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId: rootId,
    directoryIdText: committed.directoryIdText,
    generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze([entry]),
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
  return { shareInstance, descriptor, catalog }
}

describe('reopened revision readmission', () => {
  const metaA = encodeBase64Url(new Uint8Array(32).fill(0x2a))
  const metaB = encodeBase64Url(new Uint8Array(32).fill(0x5b))

  it('checkpoints and resumes across repeated metadata-agreement reopens without double-counting progress', async () => {
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
    const fileId = id(51)
    const { shareInstance, descriptor, catalog } = makeSingleFileCatalog(fileId, 'movie.bin', BigInt(payload.byteLength))

    let openCount = 0
    const revDescriptors = [
      makeDescriptor({ shareInstance, fileId, revision: id(61), exactSize: 12n, sourceMetadata: metaA }),
      makeDescriptor({ shareInstance, fileId, revision: id(62), exactSize: 12n, sourceMetadata: metaA }),
      makeDescriptor({ shareInstance, fileId, revision: id(63), exactSize: 12n, sourceMetadata: metaA }),
    ]
    const revisions: RevisionReader = {
      open: async () => {
        const desc = revDescriptors[Math.min(openCount, revDescriptors.length - 1)]!
        openCount += 1
        return Object.freeze({ descriptor: desc, leaseId: id(openCount), release: async () => undefined })
      },
    }
    const readSpans: ByteRange[] = []
    const broker: BlockRangeReader = {
      readRange: async function* (desc, _leaseId, span) {
        readSpans.push(span)
        const sliceEnd = span.start + 4n < span.end ? span.start + 4n : span.end
        yield Object.freeze({ offset: span.start, data: payload.subarray(Number(span.start), Number(sliceEnd)) })
        if (sliceEnd < span.end) {
          const nextDesc = revDescriptors[openCount]!
          throw new RevisionReopenedDuringRecoveryError(desc, nextDesc)
        }
      },
    }

    const disk = new Uint8Array(payload.byteLength)
    let savedRanges: readonly ByteRange[] = []
    let savedRevision: string | undefined
    let savedSourceMetadata: string | undefined
    let checkpointCalls = 0
    let abortCalls = 0
    let lastMaterialized = 0n
    let lastWritten = 0n

    const output: TransferOutputSession = {
      sessionId: 'sess-reopen-progress',
      capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
      openFile: async (req: TransferFileRequest): Promise<TransferOutputFile> => ({
        completedRanges: savedRanges,
        ...(savedRevision === undefined ? {} : { savedRevision }),
        ...(savedSourceMetadata === undefined ? {} : { savedSourceMetadata }),
        savedSize: req.exactSize,
        writeAt: async (offset, data) => { disk.set(data, Number(offset)) },
        checkpoint: async ranges => {
          checkpointCalls += 1
          savedRanges = ranges
          savedRevision = req.revision
          savedSourceMetadata = req.sourceMetadata
          return 'saved'
        },
        commit: async ranges => {
          savedRanges = ranges
          savedRevision = req.revision
          savedSourceMetadata = req.sourceMetadata
        },
        pause: async () => undefined,
        abort: async () => { abortCalls += 1 },
      }),
    }

    const result = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions,
      broker,
      output,
      onProgress: progress => {
        lastMaterialized = progress.materializedBytes
        lastWritten = progress.writtenBytes
      },
    }).run()

    expect(result.status).toBe('completed')
    expect(openCount).toBe(3)
    expect(checkpointCalls).toBe(2)
    expect(abortCalls).toBe(0)
    expect(readSpans).toEqual([byteRange(0n, 12n), byteRange(4n, 12n), byteRange(8n, 12n)])
    expect([...disk]).toEqual([...payload])
    expect(lastMaterialized).toBe(12n)
    expect(lastWritten).toBe(12n)
  })

  it('bounds stalled reopens and rejects non-resumable, changed, and unavailable reopens', async () => {
    const fileId = id(52)
    const { shareInstance, descriptor, catalog } = makeSingleFileCatalog(fileId, 'clip.bin', 8n)
    const prev = makeDescriptor({ shareInstance, fileId, revision: id(71), exactSize: 8n, sourceMetadata: metaA })
    const sameMetaNext = makeDescriptor({ shareInstance, fileId, revision: id(72), exactSize: 8n, sourceMetadata: metaA })
    const changedMetaNext = makeDescriptor({ shareInstance, fileId, revision: id(73), exactSize: 8n, sourceMetadata: metaB })
    const noMetaPrev = makeDescriptor({ shareInstance, fileId, revision: id(74), exactSize: 8n })
    const noMetaNext = makeDescriptor({ shareInstance, fileId, revision: id(75), exactSize: 8n })

    // 1. Repeated zero-progress reopens stop after one stalled retry.
    let stalledOpens = 0
    let stalledAborts = 0
    const stalledResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => {
          stalledOpens += 1
          return Object.freeze({
            descriptor: stalledOpens === 1 ? prev : sameMetaNext,
            leaseId: id(stalledOpens),
            release: async () => undefined,
          })
        },
      },
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new RevisionReopenedDuringRecoveryError(prev, sameMetaNext))
        },
      },
      output: {
        sessionId: 'sess-stalled',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => { stalledAborts += 1 },
        }),
      },
    }).run()
    expect(stalledResult.status).toBe('failed')
    expect(stalledOpens).toBe(2)
    expect(stalledAborts).toBe(2)

    // 2. Non-resumable backend does not checkpoint or retry on metadata-agreement reopen.
    let portableOpens = 0
    let portableCheckpoints = 0
    let portableAborts = 0
    const nonResumableResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => {
          portableOpens += 1
          return Object.freeze({ descriptor: prev, leaseId: id(1), release: async () => undefined })
        },
      },
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new RevisionReopenedDuringRecoveryError(prev, sameMetaNext))
        },
      },
      output: {
        sessionId: 'sess-non-resumable',
        capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => { portableCheckpoints += 1; return 'skipped' },
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => { portableAborts += 1 },
        }),
      },
    }).run()
    expect(nonResumableResult.status).toBe('failed')
    expect(portableOpens).toBe(1)
    expect(portableCheckpoints).toBe(0)
    expect(portableAborts).toBe(1)

    // 3. Observed source change reports source-changed immediately; unavailable metadata fails without retry.
    const changedResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: async () => Object.freeze({ descriptor: prev, leaseId: id(1), release: async () => undefined }) },
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new RevisionReopenedDuringRecoveryError(prev, changedMetaNext))
        },
      },
      output: {
        sessionId: 'sess-changed',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    }).run()
    expect(changedResult.status).toBe('source-changed')

    let unavailableOpens = 0
    const unavailableResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => {
          unavailableOpens += 1
          return Object.freeze({ descriptor: noMetaPrev, leaseId: id(1), release: async () => undefined })
        },
      },
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new RevisionReopenedDuringRecoveryError(noMetaPrev, noMetaNext))
        },
      },
      output: {
        sessionId: 'sess-unavailable',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    }).run()
    expect(unavailableResult.status).toBe('failed')
    expect(unavailableOpens).toBe(1)
  })

  it('pauses with storage-full when checkpointing for reopen retry hits quota exhaustion', async () => {
    const fileId = id(53)
    const { shareInstance, descriptor, catalog } = makeSingleFileCatalog(fileId, 'huge.bin', 8n)
    const prev = makeDescriptor({ shareInstance, fileId, revision: id(81), exactSize: 8n, sourceMetadata: metaA })
    const next = makeDescriptor({ shareInstance, fileId, revision: id(82), exactSize: 8n, sourceMetadata: metaA })
    let openCount = 0

    const result = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => {
          openCount += 1
          return Object.freeze({ descriptor: prev, leaseId: id(1), release: async () => undefined })
        },
      },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: new Uint8Array([1, 2, 3, 4]) })
          throw new RevisionReopenedDuringRecoveryError(prev, next)
        },
      },
      output: {
        sessionId: 'sess-quota',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => { throw new DOMException('Disk full', 'QuotaExceededError') },
          commit: async () => undefined,
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    }).run()

    expect(result.status).toBe('paused')
    expect(result.pauseReason).toBe('storage-full')
    expect(openCount).toBe(1)
  })
})

describe('TransferJob resumable interruption handling', () => {
  it('pauses resumable transfers with interrupted reason instead of aborting written bytes on transport failure', async () => {
    const fileId = id(54)
    const { shareInstance, descriptor, catalog } = makeSingleFileCatalog(fileId, 'movie.mkv', 12n)
    const rev = makeDescriptor({ shareInstance, fileId, revision: id(83), exactSize: 12n })
    let pausedRanges: readonly ByteRange[] = []
    let pauseCalls = 0
    let abortCalls = 0

    const result = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => Object.freeze({ descriptor: rev, leaseId: id(1), release: async () => undefined }),
      },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: new Uint8Array([1, 2, 3, 4]) })
          throw new BlockLaneAttemptsError([
            new BlockInactivityTimeoutError('receiving_fragments', 15_000, 0, { lane: { laneId: 1, laneEpoch: 0 } }),
          ])
        },
      },
      output: {
        sessionId: 'sess-interrupted',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => undefined,
          pause: async ranges => {
            pauseCalls += 1
            pausedRanges = ranges
          },
          abort: async () => { abortCalls += 1 },
        }),
      },
    }).run()

    expect(result.status).toBe('paused')
    expect(result.pauseReason).toBe('interrupted')
    expect(pauseCalls).toBe(1)
    expect(abortCalls).toBe(0)
    expect(pausedRanges).toEqual([byteRange(0n, 4n)])
  })

  it.each([false, true])('stops the remaining selection on relay denial, preserving completed files (first file saved: %s)', async saved => {
    const files = [10, 11, 12, 13].map(seed => ({ seed, name: String(seed), bytes: Uint8Array.of(seed) }))
    const fixture = makeShareFixture(files)
    const denied = new RelayReceiverError('denied', { relayError: { code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0 } })
    const open = vi.fn(async (fileId: Uint8Array) => {
      if (fileId[0] === 12) throw denied
      if (fileId[0] === 11 || !saved) throw new Error('File unavailable')
      return fixture.revisions.open(fileId)
    })
    const result = await new TransferJob({
      ...fixture,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open },
      output: {
        sessionId: 'denied',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()
    expect(result.error).toBe(denied)
    expect(result.status).toBe(saved ? 'partial' : 'failed')
    expect(result.completedFiles).toBe(saved ? 1n : 0n)
    expect(open.mock.calls.map(([id]) => id[0])).toEqual([10, 11, 12])
  })

  it('aborts 0-byte placeholders on resumable interruption and keeps non-resumable and share-stopped failures terminal', async () => {
    const fileId = id(55)
    const { shareInstance, descriptor, catalog } = makeSingleFileCatalog(fileId, 'clip.mkv', 8n)
    const rev = makeDescriptor({ shareInstance, fileId, revision: id(84), exactSize: 8n })

    // 1. Resumable backend with 0 bytes written cleans up empty placeholder and still pauses cleanly.
    let zeroPauseCalls = 0
    let zeroAbortCalls = 0
    const zeroResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: async () => Object.freeze({ descriptor: rev, leaseId: id(1), release: async () => undefined }) },
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new SessionRuntimeError('lane', 'relay dropped'))
        },
      },
      output: {
        sessionId: 'sess-zero-interrupted',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({
          pause: async () => { zeroPauseCalls += 1 },
          abort: async () => { zeroAbortCalls += 1 },
        }),
      },
    }).run()
    expect(zeroResult.status).toBe('paused')
    expect(zeroResult.pauseReason).toBe('interrupted')
    expect(zeroPauseCalls).toBe(0)
    expect(zeroAbortCalls).toBe(1)

    // 2. Non-resumable backend still aborts and fails.
    let portableAborts = 0
    const portableResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: async () => Object.freeze({ descriptor: rev, leaseId: id(1), release: async () => undefined }) },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: new Uint8Array([1, 2]) })
          throw new BlockLaneAttemptsError([new SessionRuntimeError('lane', 'lane lost')])
        },
      },
      output: {
        sessionId: 'sess-portable-interrupted',
        capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false },
        openFile: async () => outputFile({ abort: async () => { portableAborts += 1 } }),
      },
    }).run()
    expect(portableResult.status).toBe('failed')
    expect(portableAborts).toBe(1)

    // 3. Permanent share stop remains terminal even on resumable backends.
    let stoppedAborts = 0
    const stoppedResult = await new TransferJob({
      descriptor,
      catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: async () => Object.freeze({ descriptor: rev, leaseId: id(1), release: async () => undefined }) },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: new Uint8Array([1, 2]) })
          throw new RelayReceiverError('stopped', {
            relayError: { code: RELAY_ERROR.stopped, retryAfterMilliseconds: 0 },
          })
        },
      },
      output: {
        sessionId: 'sess-share-stopped',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({ abort: async () => { stoppedAborts += 1 } }),
      },
    }).run()
    expect(stoppedResult.status).toBe('failed')
    expect(stoppedAborts).toBe(1)

    // 4. Discovery-time transient lane outage pauses resumable backends instead of failing discovery.
    const discoveryResult = await new TransferJob({
      descriptor,
      catalog: {
        loadDirectory: async () => { throw new SessionRuntimeError('lane', 'catalog lane dropped') },
        page: async () => { throw new Error('unused') },
      } as unknown as CatalogClient,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: { open: async () => { throw new Error('unused') } },
      broker: { readRange: async function* () { yield { offset: 0n, data: new Uint8Array(0) } } },
      output: {
        sessionId: 'sess-discovery-interrupted',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => { throw new Error('unused') },
      },
    }).run()
    expect(discoveryResult.status).toBe('paused')
    expect(discoveryResult.pauseReason).toBe('interrupted')
  })

  it('skips a single oversized directory while downloading healthy siblings and halts immediately when browser cache is saturated', async () => {
    const shareInstance = id(1)
    const rootId = id(2)
    const firstDirId = id(10)
    const limitDirId = id(11)
    const siblingDirId = id(12)
    const rootFileId = id(20)
    const siblingFileId = id(21)
    const rootIdText = encodeBase64Url(rootId)
    const firstDirIdText = encodeBase64Url(firstDirId)
    const limitDirIdText = encodeBase64Url(limitDirId)
    const siblingDirIdText = encodeBase64Url(siblingDirId)
    const payload = new Uint8Array([1, 2, 3, 4])

    const rootEntries: readonly CatalogEntry[] = Object.freeze([
      Object.freeze({ kind: 'file', id: rootFileId, idText: encodeBase64Url(rootFileId), name: 'ready.bin', expectedSize: 4n }),
      Object.freeze({ kind: 'directory', id: firstDirId, idText: firstDirIdText, name: 'local-error' }),
      Object.freeze({ kind: 'directory', id: limitDirId, idText: limitDirIdText, name: 'hits-limit' }),
      Object.freeze({ kind: 'directory', id: siblingDirId, idText: siblingDirIdText, name: 'healthy-sibling' }),
    ])
    const siblingEntries: readonly CatalogEntry[] = Object.freeze([
      Object.freeze({ kind: 'file', id: siblingFileId, idText: encodeBase64Url(siblingFileId), name: 'sibling.bin', expectedSize: 4n }),
    ])
    const makeCommitted = (directoryId: Uint8Array<ArrayBuffer>, directoryIdText: string, genSeed: number, entries: readonly CatalogEntry[]): CommittedDirectory => Object.freeze({
      directoryId,
      directoryIdText,
      generation: id(genSeed),
      generationText: encodeBase64Url(id(genSeed)),
      pageCount: 1,
      entryCount: entries.length,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    })
    const makePage = (committed: CommittedDirectory, entries: readonly CatalogEntry[]): CatalogPage => Object.freeze({
      shareInstance,
      directoryId: committed.directoryId,
      directoryIdText: committed.directoryIdText,
      generation: committed.generation,
      generationText: committed.generationText,
      pageIndex: 0,
      terminal: true,
      previousCommitment: new Uint8Array(32),
      entries,
      omittedCount: 0n,
      objectCommitment: new Uint8Array(32),
      encodedBytes: 128,
    })

    const rootCommitted = makeCommitted(rootId, rootIdText, 50, rootEntries)
    const siblingCommitted = makeCommitted(siblingDirId, siblingDirIdText, 51, siblingEntries)
    const rootPage = makePage(rootCommitted, rootEntries)
    const siblingPage = makePage(siblingCommitted, siblingEntries)
    const descriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: rootIdText,
    }) as unknown as ShareDescriptor

    // 1. A single oversized directory is skipped while healthy siblings continue downloading.
    const oversizedError = new CatalogPageStoreError('resource-limit', 'Catalog cache exceeded its share entry or metadata budget')
    const oversizedQueries: string[] = []
    const oversizedCommittedPaths: string[] = []
    const oversizedCatalog = {
      loadDirectory: async (directoryId: Uint8Array) => {
        const dirText = encodeBase64Url(directoryId)
        oversizedQueries.push(dirText)
        if (dirText === rootIdText) return rootCommitted
        if (dirText === firstDirIdText) throw new Error('local directory unreadable')
        if (dirText === limitDirIdText) throw oversizedError
        if (dirText === siblingDirIdText) return siblingCommitted
        throw new Error(`unexpected directory query ${dirText}`)
      },
      page: async (committed: CommittedDirectory) => (
        committed.directoryIdText === rootIdText ? rootPage : siblingPage
      ),
    } as unknown as CatalogClient

    const oversizedResult = await new TransferJob({
      descriptor,
      catalog: oversizedCatalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async (fileId: Uint8Array) => Object.freeze({
          descriptor: makeDescriptor({ shareInstance, fileId: fileId.slice(), revision: id(60), exactSize: 4n }),
          leaseId: id(1),
          release: async () => undefined,
        }),
      },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: payload })
        },
      },
      output: {
        sessionId: 'sess-oversized-dir',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (file): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => { oversizedCommittedPaths.push(file.path.join('/')) },
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    }).run()

    expect(oversizedResult.status).toBe('partial')
    expect(oversizedResult.completedFiles).toBe(2n)
    expect(oversizedCommittedPaths).toEqual(['ready.bin', 'healthy-sibling/sibling.bin'])
    expect(oversizedResult.failedDirectories).toBe(2n)
    expect(oversizedResult.error).toBe(oversizedError)
    expect(oversizedQueries).toEqual([rootIdText, firstDirIdText, limitDirIdText, siblingDirIdText])

    // 2. A provably saturated browser cache halts remaining discovery while finishing discovered files.
    const saturatedError = new CatalogPageStoreError(
      'resource-limit',
      'Catalog cache exceeded its share entry or metadata budget',
      { storeExhausted: true },
    )
    const saturatedQueries: string[] = []
    const saturatedCatalog = {
      loadDirectory: async (directoryId: Uint8Array) => {
        const dirText = encodeBase64Url(directoryId)
        saturatedQueries.push(dirText)
        if (dirText === rootIdText) return rootCommitted
        if (dirText === firstDirIdText) throw new Error('local directory unreadable')
        if (dirText === limitDirIdText) throw saturatedError
        throw new Error(`unexpected directory query ${dirText}`)
      },
      page: async () => rootPage,
    } as unknown as CatalogClient

    let saturatedCommittedFiles = 0
    const saturatedResult = await new TransferJob({
      descriptor,
      catalog: saturatedCatalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: {
        open: async () => Object.freeze({
          descriptor: makeDescriptor({ shareInstance, fileId: rootFileId, revision: id(61), exactSize: 4n }),
          leaseId: id(2),
          release: async () => undefined,
        }),
      },
      broker: {
        readRange: async function* () {
          yield Object.freeze({ offset: 0n, data: payload })
        },
      },
      output: {
        sessionId: 'sess-saturated-cache',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async (): Promise<TransferOutputFile> => ({
          completedRanges: [],
          writeAt: async () => undefined,
          checkpoint: async () => 'saved',
          commit: async () => { saturatedCommittedFiles += 1 },
          pause: async () => undefined,
          abort: async () => undefined,
        }),
      },
    }).run()

    expect(saturatedResult.status).toBe('partial')
    expect(saturatedResult.completedFiles).toBe(1n)
    expect(saturatedCommittedFiles).toBe(1)
    expect(saturatedResult.failedDirectories).toBe(2n)
    expect(saturatedResult.error).toBe(saturatedError)
    expect(saturatedQueries).toEqual([rootIdText, firstDirIdText, limitDirIdText])
  })
})
