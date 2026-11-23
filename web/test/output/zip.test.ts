import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { assert, describe, expect, it, vi } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import { planArtifactOffers } from '../../src/output/backends/contract'
import { openNewOPFSSession, reopenOPFSSession } from '../../src/output/backends/opfs'
import { openPortableSession } from '../../src/output/backends/portable'
import { NATIVE_WRITE_CHUNK_BYTES } from '../../src/output/native-object/client'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import { ObjectCheckpointCoordinator } from '../../src/output/native-object/coordinator'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../../src/output/zip/progressive-zip'
import { StreamingZipWriter } from '../../src/output/zip/streaming-zip'
import { inspectSelectionShape } from '../../src/transfer/discovery'
import { TransferJob } from '../../src/transfer/job'
import { artifactRootPath, selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'

class MemoryNativeObjectIO implements NativeObjectIO {
  bytes = new Uint8Array(0)
  readonly events: string[] = []
  #writeGate: Promise<void> | undefined

  pauseWrites(): () => void {
    let release!: () => void
    this.#writeGate = new Promise<void>(resolve => { release = resolve })
    return () => {
      this.#writeGate = undefined
      release()
    }
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    if (this.#writeGate !== undefined) await this.#writeGate
    const start = Number(offset)
    const end = start + data.byteLength
    if (end > this.bytes.byteLength) {
      const next = new Uint8Array(end)
      next.set(this.bytes)
      this.bytes = next
    }
    this.bytes.set(data, start)
  }

  async truncate(length: bigint): Promise<void> {
    const len = Number(length)
    const next = new Uint8Array(len)
    next.set(this.bytes.subarray(0, Math.min(len, this.bytes.byteLength)))
    this.bytes = next
  }

  async size(): Promise<bigint> { return BigInt(this.bytes.byteLength) }
  async flush(): Promise<void> { this.events.push('io.flush') }
  async close(): Promise<void> { this.events.push('io.close') }
}

class MemoryFileStore {
  readonly files = new Map<string, ReceiveFileRecord>()
  readonly events: string[]

  constructor(events: string[]) { this.events = events }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    this.files.set(record.id, record)
    this.events.push(`putFile:${record.fileId}:${record.status}`)
  }

  async getFile(taskId: string, fileId: string): Promise<ReceiveFileRecord | undefined> {
    return this.files.get(`${taskId}:${fileId}`)
  }

  async listTaskFiles(taskId: string): Promise<readonly ReceiveFileRecord[]> {
    return [...this.files.values()].filter(file => file.taskId === taskId)
  }
}

async function readZipArchive(blob: Blob): Promise<{
  readonly files: Map<string, Uint8Array>
  readonly directories: Set<string>
}> {
  const reader = new ZipReader(new BlobReader(blob), { checkSignature: true })
  try {
    const entries = await reader.getEntries()
    const files = new Map<string, Uint8Array>()
    const directories = new Set<string>()
    for (const entry of entries) {
      if (entry.directory) {
        directories.add(entry.filename)
      } else if ('getData' in entry) {
        files.set(entry.filename, await (entry as FileEntry).getData(new Uint8ArrayWriter()))
      }
    }
    return { files, directories }
  } finally {
    await reader.close()
  }
}

async function readZipEntries(blob: Blob): Promise<Map<string, Uint8Array>> {
  return (await readZipArchive(blob)).files
}

function fixedId(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

function makeDirectoryPage(
  shareInstance: Uint8Array<ArrayBuffer>,
  directoryId: Uint8Array<ArrayBuffer>,
  entries: readonly CatalogEntry[],
): { readonly committed: CommittedDirectory; readonly page: CatalogPage } {
  const generation = fixedId(3)
  const committed: CommittedDirectory = Object.freeze({
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    generation,
    generationText: encodeBase64Url(generation),
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId,
    directoryIdText: committed.directoryIdText,
    generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze([...entries]),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  return { committed, page }
}

function createMockDownloadWindow(): {
  readonly windowPort: BrowserDownloadWindow
  readonly blobs: Blob[]
} {
  const blobs: Blob[] = []
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        if (blob instanceof Blob) blobs.push(blob)
        return `blob:mock-${blobs.length}`
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => undefined,
        remove: () => undefined,
      }) as unknown as HTMLAnchorElement,
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Pick<Document, 'createElement' | 'body'>,
    setTimeout: (() => 0) as unknown as typeof setTimeout,
    clearTimeout: () => undefined,
  }
  return { windowPort, blobs }
}

describe('ZIP writers (portable streaming & OPFS progressive)', () => {
  it('writes valid Store-mode ZIP archives via StreamingZipWriter and enforces size caps', async () => {
    const zip = new StreamingZipWriter({ taskId: 'stream-1', maxBytes: 1024n * 1024n })
    await zip.addDirectory(['album'])
    const a = zip.beginFile(['album', 'hello.txt'], 5n)
    a.write(new TextEncoder().encode('hello'))
    await a.close()
    const b = zip.beginFile(['album', 'empty.txt'], 0n)
    await b.close()

    const blob = await zip.finalize()
    const files = await readZipEntries(blob)
    expect(new TextDecoder().decode(files.get('album/hello.txt'))).toBe('hello')
    expect(files.get('album/empty.txt')).toEqual(new Uint8Array(0))

    const capped = new StreamingZipWriter({ taskId: 'stream-2', maxBytes: 32n })
    expect(() => capped.beginFile(['big.bin'], 100n)).toThrow(/limit/u)
  })

  it('supports out-of-order range resume, flush-before-IDB checkpoints, partial export, and finalization in ProgressiveZipWriter', async () => {
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const coord = new ObjectCheckpointCoordinator({ io, operationId: 'pzip-1', objectId: 'pzip-1.part' })
    const payloadA = new TextEncoder().encode('alpha-content-0123')
    const payloadB = new TextEncoder().encode('beta-second-file!!')

    const writer1 = await ProgressiveZipWriter.open('pzip-1', store as unknown as ReceiveStore, coord)
    await writer1.ensureDirectory('dir-1', ['docs'])
    const fileA = await writer1.openFile({
      fileId: 'fa', path: ['docs', 'a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    })
    // Out-of-order writes on file A.
    await writer1.writeFileSlice(fileA, 6n, payloadA.subarray(6))
    await writer1.writeFileSlice(fileA, 0n, payloadA.subarray(0, 6))
    await writer1.commitFile(fileA, [byteRange(0n, BigInt(payloadA.byteLength))])

    // Flush must precede IDB putFile!
    const flushIdx = io.events.indexOf('io.flush')
    const putIdx = io.events.indexOf('putFile:fa:completed')
    expect(flushIdx).toBeGreaterThanOrEqual(0)
    expect(putIdx).toBeGreaterThan(flushIdx)

    // Write half of file B and pause via checkpoint.
    const fileB = await writer1.openFile({
      fileId: 'fb', path: ['docs', 'b.txt'], revision: 'r2', exactSize: BigInt(payloadB.byteLength),
    })
    await writer1.writeFileSlice(fileB, 0n, payloadB.subarray(0, 8))
    await writer1.checkpointFile(fileB, [byteRange(0n, 8n)])

    // Partial ZIP export must include completed file A and exclude incomplete file B.
    const partialBlob = buildPartialZipBlob(new File([io.bytes], 'pzip-1.part'), await store.listTaskFiles('pzip-1'))
    const partialFiles = await readZipEntries(partialBlob)
    expect([...partialFiles.keys()]).toEqual(['docs/a.txt'])
    expect(partialFiles.get('docs/a.txt')).toEqual(payloadA)

    // Reopen fresh writer over same OPFS bytes + store and complete file B.
    const writer2 = await ProgressiveZipWriter.open('pzip-1', store as unknown as ReceiveStore, coord)
    expect(writer2.completedFile('fa')).toEqual({ size: BigInt(payloadA.byteLength) })
    expect(writer2.completedFile('fb')).toBeUndefined()

    const reopenedB = await writer2.openFile({
      fileId: 'fb', path: ['docs', 'b.txt'], revision: 'r2', exactSize: BigInt(payloadB.byteLength),
    })
    expect(reopenedB.completedRanges).toEqual([byteRange(0n, 8n)])
    await writer2.writeFileSlice(reopenedB, 8n, payloadB.subarray(8))
    await writer2.commitFile(reopenedB, [byteRange(0n, BigInt(payloadB.byteLength))])

    const finalSize = await writer2.finalize()
    const finalFiles = await readZipEntries(new Blob([io.bytes.subarray(0, Number(finalSize))]))
    expect(finalFiles.get('docs/a.txt')).toEqual(payloadA)
    expect(finalFiles.get('docs/b.txt')).toEqual(payloadB)
  })

  it('recovers a truncated ZIP across a second interrupted repair without advertising missing payloads', async () => {
    const taskId = 'cut-archive'
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const open = () => ProgressiveZipWriter.open(taskId, store as unknown as ReceiveStore,
      new ObjectCheckpointCoordinator({ io, operationId: taskId, objectId: 'archive.part' }))
    const writer = await open()
    const data = Uint8Array.of(1, 2, 3, 4, 5, 6)
    const request = (fileId: string, exactSize = 6n) => ({ fileId, path: [fileId], exactSize, revision: 'r' })
    const whole = await writer.openFile(request('z-whole'))
    await writer.writeFileSlice(whole, 0n, data)
    await writer.commitFile(whole, [byteRange(0n, 6n)])
    const split = await writer.openFile(request('y-split'))
    await writer.writeFileSlice(split, 0n, data.slice(0, 2))
    await writer.writeFileSlice(split, 4n, data.slice(4))
    await writer.checkpointFile(split, [byteRange(0n, 2n), byteRange(4n, 6n)])
    const lost = await writer.openFile(request('a-lost'))
    await writer.writeFileSlice(lost, 0n, data)
    await writer.commitFile(lost, [byteRange(0n, 6n)])
    await writer.ensureDirectory('d', ['empty-dir'])
    const empty = await writer.openFile(request('empty', 0n))
    await writer.commitFile(empty, [])
    const original = io.bytes.slice()
    await io.truncate(split.record.zipEntry.payloadOffset + 2n)

    // IndexedDB's file-id order need not match physical ZIP layout.
    vi.spyOn(store, 'listTaskFiles').mockImplementation(async () => [...store.files.values()].reverse())
    const partialArchive = async () => readZipArchive(buildPartialZipBlob(
      new File([io.bytes], 'cut.part'), await store.listTaskFiles(taskId)))
    expect((await partialArchive()).files).toEqual(new Map([['z-whole', data], ['empty', new Uint8Array()]]))

    // Crash while rebuilding the directory beyond the lost payload. Its slot is already non-exportable.
    const write = io.writeAt.bind(io)
    const directoryOffset = (await store.getFile(taskId, '__dir__:d'))!.zipEntry!.localHeaderOffset
    vi.spyOn(io, 'writeAt').mockImplementation(async (offset, bytes) => {
      if (offset === directoryOffset) throw new Error('repair interrupted')
      await write(offset, bytes)
    })
    await expect(open()).rejects.toThrow('repair interrupted')
    expect(await store.getFile(taskId, 'a-lost')).toMatchObject({ status: 'receiving', completedRanges: [] })
    expect(await store.getFile(taskId, 'y-split')).toMatchObject({ completedRanges: [[0n, 2n]] })
    vi.mocked(io.writeAt).mockRestore()

    const resumed = await open()
    expect(resumed.completedFile('z-whole')).toEqual({ size: 6n })
    expect(resumed.completedFile('empty')).toEqual({ size: 0n })
    expect(resumed.completedFile('a-lost')).toBeUndefined()
    expect((await partialArchive()).files.has('a-lost')).toBe(false)
    const splitAgain = await resumed.openFile(request('y-split'))
    expect(splitAgain.completedRanges).toEqual([byteRange(0n, 2n)])
    await resumed.writeFileSlice(splitAgain, 0n, Uint8Array.of(99, 99))
    await resumed.writeFileSlice(splitAgain, 2n, data.slice(2))
    await resumed.commitFile(splitAgain, [byteRange(0n, 6n)])
    await expect(resumed.openFile(request('a-lost', 7n))).rejects.toMatchObject({ fault: { code: 'revision-changed' } })
    const lostAgain = await resumed.openFile(request('a-lost'))
    await expect(resumed.writeFileSlice(lostAgain, 5n, data)).rejects.toThrow(/exceeds/u)
    await resumed.writeFileSlice(lostAgain, 0n, data)
    await resumed.commitFile(lostAgain, [byteRange(0n, 6n)])
    await resumed.ensureDirectory('d', ['empty-dir'])
    await resumed.finalize()
    for (const row of await store.listTaskFiles(taskId)) {
      const { localHeaderOffset, payloadOffset } = row.zipEntry!
      expect(io.bytes.slice(Number(localHeaderOffset), Number(payloadOffset)))
        .toEqual(original.slice(Number(localHeaderOffset), Number(payloadOffset)))
    }
    const archive = await readZipArchive(new Blob([io.bytes]))
    expect(archive.directories).toEqual(new Set(['empty-dir/']))
    expect(archive.files).toEqual(new Map([['z-whole', data], ['y-split', data], ['a-lost', data], ['empty', new Uint8Array()]]))
  })

  it('restores a missing descriptor without fetching an intact payload, and rejects an empty partial export', async () => {
    const taskId = 'cut-descriptor'
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const open = () => ProgressiveZipWriter.open(taskId, store as unknown as ReceiveStore,
      new ObjectCheckpointCoordinator({ io, operationId: taskId, objectId: 'archive.part' }))
    const writer = await open()
    const data = Uint8Array.of(3, 1, 4)
    const file = await writer.openFile({ fileId: 'f', path: ['f'], revision: 'r', exactSize: 3n })
    await writer.writeFileSlice(file, 0n, data)
    await writer.commitFile(file, [byteRange(0n, 3n)])
    const committed = await store.listTaskFiles(taskId)
    expect(() => buildPartialZipBlob(new File([], 'evicted'), committed)).toThrow(/No completed files/u)
    const { descriptorOffset, endOffset } = file.record.zipEntry
    const descriptor = io.bytes.slice(Number(descriptorOffset), Number(endOffset))
    await io.truncate(descriptorOffset + 1n)
    const resumed = await open()
    expect(resumed.completedFile('f')).toEqual({ size: 3n })
    await resumed.finalize()
    expect(io.bytes.slice(Number(descriptorOffset), Number(endOffset))).toEqual(descriptor)
    expect((await readZipEntries(new Blob([io.bytes]))).get('f')).toEqual(data)
  })

  it('defers directory entries until the active StreamingZipWriter file closes and rejects illegal concurrent states', async () => {
    const zip = new StreamingZipWriter({ taskId: 'stream-interleave', maxBytes: 1024n * 1024n })
    const fileA = zip.beginFile(['dirA', 'data.bin'], 4n)

    // Interleaved directory additions during an open file must not splice headers into fileA.
    await zip.addDirectory(['dirB'])
    await zip.addDirectory(['dirB', 'nested'])

    expect(() => zip.beginFile(['dirA', 'overlap.bin'], 1n)).toThrow(/still open/u)
    await expect(zip.finalize()).rejects.toThrow(/still open/u)

    fileA.write(new Uint8Array([0x01, 0x02, 0x03, 0x04]))
    await fileA.close()
    expect(() => fileA.write(new Uint8Array([0x05]))).toThrow(/no longer open/u)
    await expect(fileA.close()).rejects.toThrow(/no longer open/u)

    const fileB = zip.beginFile(['dirB', 'nested', 'ok.txt'], 2n)
    fileB.write(new TextEncoder().encode('ok'))
    await fileB.close()

    const archive = await readZipArchive(await zip.finalize())
    expect([...archive.files.get('dirA/data.bin')!]).toEqual([0x01, 0x02, 0x03, 0x04])
    expect(new TextDecoder().decode(archive.files.get('dirB/nested/ok.txt'))).toBe('ok')
    expect(archive.directories).toEqual(new Set(['dirB/', 'dirB/nested/']))

    // Aborting an active file taints the sequential writer so no corrupted archive can be finalized.
    const aborted = new StreamingZipWriter({ taskId: 'stream-abort', maxBytes: 1024n * 1024n })
    const open = aborted.beginFile(['partial.bin'], 8n)
    open.write(new Uint8Array([1, 2, 3, 4]))
    open.abort()
    expect(() => open.write(new Uint8Array([5]))).toThrow(/no longer open/u)
    expect(() => aborted.beginFile(['next.bin'], 1n)).toThrow(/aborted/u)
    await expect(aborted.addDirectory(['after'])).rejects.toThrow(/aborted/u)
    await expect(aborted.finalize()).rejects.toThrow(/aborted/u)
  })

  it('produces a valid portable ZIP through TransferJob when directories are discovered mid-file', async () => {
    const shareInstance = fixedId(1)
    const rootId = fixedId(2)
    const dirAId = fixedId(10)
    const dirBId = fixedId(11)
    const nestedId = fixedId(12)
    const fileId = fixedId(20)
    const payload = new Uint8Array([0x01, 0x02, 0x03, 0x04])

    const pages = new Map([
      [encodeBase64Url(rootId), makeDirectoryPage(shareInstance, rootId, [
        Object.freeze({ kind: 'directory', id: dirAId, idText: encodeBase64Url(dirAId), name: 'dirA' }),
        Object.freeze({ kind: 'directory', id: dirBId, idText: encodeBase64Url(dirBId), name: 'dirB' }),
      ])],
      [encodeBase64Url(dirAId), makeDirectoryPage(shareInstance, dirAId, [
        Object.freeze({ kind: 'file', id: fileId, idText: encodeBase64Url(fileId), name: 'data.bin', expectedSize: BigInt(payload.byteLength) }),
      ])],
      [encodeBase64Url(dirBId), makeDirectoryPage(shareInstance, dirBId, [
        Object.freeze({ kind: 'directory', id: nestedId, idText: encodeBase64Url(nestedId), name: 'nested' }),
      ])],
      [encodeBase64Url(nestedId), makeDirectoryPage(shareInstance, nestedId, [])],
    ])

    let releasePayload!: () => void
    const payloadGate = new Promise<void>(resolve => { releasePayload = resolve })
    const descriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: encodeBase64Url(rootId),
    }) as unknown as ShareDescriptor

    const catalog = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => pages.get(encodeBase64Url(directoryId))!.committed,
      page: async (committed: CommittedDirectory) => pages.get(committed.directoryIdText)!.page,
    }) as unknown as CatalogClient

    const rev = fixedId(70)
    const revisions: RevisionReader = {
      open: async () => Object.freeze({
        descriptor: Object.freeze({
          shareInstance,
          shareInstanceId: descriptor.shareInstanceId,
          fileId,
          fileIdText: encodeBase64Url(fileId),
          fileRevision: rev,
          fileRevisionText: encodeBase64Url(rev),
          exactSize: BigInt(payload.byteLength),
          geometry: new FileGeometry(BigInt(payload.byteLength), 8n),
        }) as FileRevisionDescriptor,
        leaseId: fixedId(99),
        release: async () => undefined,
      }),
    }

    const broker: BlockRangeReader = {
      readRange: async function* () {
        // Hold the file payload until discovery has called ensureDirectory for dirB/nested.
        await payloadGate
        yield Object.freeze({ offset: 0n, data: payload })
      },
    }

    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(descriptor.shareInstanceId, descriptor.syntheticRootId, selection)
    const { windowPort, blobs } = createMockDownloadWindow()
    const session = openPortableSession({
      task: {
        taskId: 'portable-interleave-task',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'bundle.zip' }),
        lifecycle: initialReceiveState('portable-interleave-task'),
      },
      windowPort,
    })

    const wrappedOutput = Object.freeze({
      ...session.transferOutput,
      ensureDirectory: async (request: Parameters<NonNullable<typeof session.transferOutput.ensureDirectory>>[0], signal: AbortSignal) => {
        await session.transferOutput.ensureDirectory!(request, signal)
        if (request.path.join('/') === 'dirB/nested') releasePayload()
      },
    })

    const result = await new TransferJob({
      descriptor,
      catalog,
      selection,
      revisions,
      broker,
      output: wrappedOutput,
    }).run()

    assert(result.status === 'completed')
    const settled = await session.settle(result)
    expect(settled.lifecycle.kind).toBe('download-started')
    expect(blobs).toHaveLength(1)

    const archive = await readZipArchive(blobs[0]!)
    expect([...archive.files.get('dirA/data.bin')!]).toEqual([0x01, 0x02, 0x03, 0x04])
    expect(archive.directories).toEqual(new Set(['dirA/', 'dirB/', 'dirB/nested/']))
  })

  it('reserves non-overlapping slots and monotonic length when directories and files open concurrently', async () => {
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const coord = new ObjectCheckpointCoordinator({ io, operationId: 'pzip-race', objectId: 'pzip-race.part' })
    const writer = await ProgressiveZipWriter.open('pzip-race', store as unknown as ReceiveStore, coord)

    const payloadA = new TextEncoder().encode('A'.repeat(128))
    const payloadB = new TextEncoder().encode('B'.repeat(64))

    // Hold the first write so directory and file allocations overlap across await points.
    const releaseWrites = io.pauseWrites()
    const dirOnePromise = writer.ensureDirectory('dir-1', ['d'])
    const fileAPromise = writer.openFile({
      fileId: 'fa', path: ['a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    })
    const dirTwoPromise = writer.ensureDirectory('dir-2', ['d', 'sub'])
    const fileBPromise = writer.openFile({
      fileId: 'fb', path: ['d', 'sub', 'b.txt'], revision: 'r2', exactSize: BigInt(payloadB.byteLength),
    })

    // Deduplicate concurrent allocations targeting the same directory or file id.
    const dupDirPromise = writer.ensureDirectory('dir-1', ['d'])
    const dupFilePromise = writer.openFile({
      fileId: 'fa', path: ['a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    })

    releaseWrites()
    const [, fileA, , fileB, , dupFileA] = await Promise.all([
      dirOnePromise,
      fileAPromise,
      dirTwoPromise,
      fileBPromise,
      dupDirPromise,
      dupFilePromise,
    ])
    expect(dupFileA.record.zipEntry).toEqual(fileA.record.zipEntry)

    const persisted = (await store.listTaskFiles('pzip-race'))
      .map(row => row.zipEntry!)
      .sort((left, right) => left.sequence - right.sequence)
    expect(persisted).toHaveLength(4)
    expect(persisted.map(entry => entry.sequence)).toEqual([1, 2, 3, 4])

    // Every reserved slot must begin exactly where the previous slot ended.
    let cursor = 0n
    for (const entry of persisted) {
      expect(entry.localHeaderOffset).toBe(cursor)
      expect(entry.endOffset).toBeGreaterThan(entry.localHeaderOffset)
      cursor = entry.endOffset
    }
    expect(writer.allocatedLength).toBe(cursor)

    await writer.writeFileSlice(fileA, 0n, payloadA)
    await writer.commitFile(fileA, [byteRange(0n, BigInt(payloadA.byteLength))])
    await writer.writeFileSlice(fileB, 0n, payloadB)
    await writer.commitFile(fileB, [byteRange(0n, BigInt(payloadB.byteLength))])

    const finalSize = await writer.finalize()
    const archiveBlob = new Blob([io.bytes.subarray(0, Number(finalSize))])
    const reader = new ZipReader(new BlobReader(archiveBlob))
    try {
      const entries = await reader.getEntries()
      expect(entries.map(entry => entry.filename)).toEqual(['d/', 'a.txt', 'd/sub/', 'd/sub/b.txt'])
      const files = await readZipEntries(archiveBlob)
      expect(files.get('a.txt')).toEqual(payloadA)
      expect(files.get('d/sub/b.txt')).toEqual(payloadB)
    } finally {
      await reader.close()
    }
  })
})

describe('ProgressiveZipWriter finalization batching', () => {
  it('coalesces a small archive central directory and EOCD into one worker write', async () => {
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const coord = new ObjectCheckpointCoordinator({
      io, operationId: 'pzip-batch-small', objectId: 'small.part',
    })
    const writer = await ProgressiveZipWriter.open('pzip-batch-small', store as unknown as ReceiveStore, coord)
    await writer.ensureDirectory('d1', ['docs'])
    await writer.ensureDirectory('d2', ['docs', 'sub'])
    for (let index = 0; index < 6; index += 1) {
      const payload = new TextEncoder().encode(`small-file-${index}`)
      const opened = await writer.openFile({
        fileId: `sf-${index}`, path: ['docs', 'sub', `file-${index}.txt`], revision: `r-${index}`,
        exactSize: BigInt(payload.byteLength),
      })
      await writer.writeFileSlice(opened, 0n, payload)
      await writer.commitFile(opened, [byteRange(0n, BigInt(payload.byteLength))])
    }

    const tailStart = writer.allocatedLength
    const writeSpy = vi.spyOn(io, 'writeAt')
    const finalSize = await writer.finalize()
    expect(writeSpy).toHaveBeenCalledTimes(1)
    const [offset, bytes] = writeSpy.mock.calls[0]!
    expect(offset).toBe(tailStart)
    expect(bytes.byteLength).toBeLessThanOrEqual(NATIVE_WRITE_CHUNK_BYTES)
    expect(tailStart + BigInt(bytes.byteLength)).toBe(finalSize)

    const archive = await readZipArchive(new Blob([io.bytes.subarray(0, Number(finalSize))]))
    expect(archive.directories).toEqual(new Set(['docs/', 'docs/sub/']))
    expect(archive.files.size).toBe(6)
    expect(new TextDecoder().decode(archive.files.get('docs/sub/file-5.txt'))).toBe('small-file-5')
  })

  it('splits a central directory exceeding one worker chunk into contiguous bounded batches', async () => {
    const io = new MemoryNativeObjectIO()
    const store = new MemoryFileStore(io.events)
    const coord = new ObjectCheckpointCoordinator({
      io, operationId: 'pzip-batch-large', objectId: 'large.part',
    })
    const writer = await ProgressiveZipWriter.open('pzip-batch-large', store as unknown as ReceiveStore, coord)
    const entryCount = 300
    const namePrefix = 'n'.repeat(980)
    const expectedPayloads = new Map<string, Uint8Array>()
    for (let index = 0; index < entryCount; index += 1) {
      const name = `${namePrefix}-${String(index).padStart(4, '0')}.bin`
      const payload = index === 0 || index === 150 || index === entryCount - 1
        ? new TextEncoder().encode(`payload-${index}`)
        : new Uint8Array(0)
      const opened = await writer.openFile({
        fileId: `lf-${index}`, path: ['bulk', name], revision: `r-${index}`, exactSize: BigInt(payload.byteLength),
      })
      if (payload.byteLength > 0) await writer.writeFileSlice(opened, 0n, payload)
      await writer.commitFile(opened, payload.byteLength > 0 ? [byteRange(0n, BigInt(payload.byteLength))] : [])
      expectedPayloads.set(`bulk/${name}`, payload)
    }

    const tailStart = writer.allocatedLength
    const writeSpy = vi.spyOn(io, 'writeAt')
    const finalSize = await writer.finalize()
    expect(writeSpy).toHaveBeenCalledTimes(2)
    let cursor = tailStart
    for (const [offset, bytes] of writeSpy.mock.calls) {
      expect(offset).toBe(cursor)
      expect(bytes.byteLength).toBeGreaterThan(0)
      expect(bytes.byteLength).toBeLessThanOrEqual(NATIVE_WRITE_CHUNK_BYTES)
      cursor += BigInt(bytes.byteLength)
    }
    expect(cursor).toBe(finalSize)

    const archive = await readZipArchive(new Blob([io.bytes.subarray(0, Number(finalSize))]))
    expect(archive.files.size).toBe(entryCount)
    for (const [entryName, expected] of expectedPayloads) {
      expect(archive.files.get(entryName)).toEqual(expected)
    }
  })
})

describe('ZIP result-root layout integration', () => {
  it('preserves a single result-root folder inside Portable and OPFS ZIP archives across fresh runs and resume', async () => {
    const shareInstance = fixedId(1)
    const rootId = fixedId(2)
    const photosId = fixedId(10)
    const tripId = fixedId(11)
    const aId = fixedId(20)
    const bId = fixedId(21)
    const payloadA = new TextEncoder().encode('alpha-photo')
    const payloadB = new TextEncoder().encode('beta-photo')

    const photosEntry: CatalogEntry = Object.freeze({ kind: 'directory', id: photosId, idText: encodeBase64Url(photosId), name: 'Photos' })
    const tripEntry: CatalogEntry = Object.freeze({ kind: 'directory', id: tripId, idText: encodeBase64Url(tripId), name: 'Trip' })
    const fileA: CatalogEntry = Object.freeze({ kind: 'file', id: aId, idText: encodeBase64Url(aId), name: 'a.jpg', expectedSize: BigInt(payloadA.byteLength) })
    const fileB: CatalogEntry = Object.freeze({ kind: 'file', id: bId, idText: encodeBase64Url(bId), name: '1.jpg', expectedSize: BigInt(payloadB.byteLength) })

    const pages = new Map([
      [encodeBase64Url(rootId), makeDirectoryPage(shareInstance, rootId, [photosEntry])],
      [photosEntry.idText, makeDirectoryPage(shareInstance, photosId, [fileA, tripEntry])],
      [tripEntry.idText, makeDirectoryPage(shareInstance, tripId, [fileB])],
    ])
    const payloads = new Map([[fileA.idText, payloadA], [fileB.idText, payloadB]])
    const descriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: encodeBase64Url(rootId),
    }) as unknown as ShareDescriptor
    const catalog = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array) => pages.get(encodeBase64Url(directoryId))!.committed,
      page: async (committed: CommittedDirectory) => pages.get(committed.directoryIdText)!.page,
    }) as unknown as CatalogClient
    const revisions: RevisionReader = {
      open: async (fileId: Uint8Array) => {
        const key = encodeBase64Url(fileId)
        const bytes = payloads.get(key)!
        const rev = fixedId(71)
        return Object.freeze({
          descriptor: Object.freeze({
            shareInstance,
            shareInstanceId: descriptor.shareInstanceId,
            fileId,
            fileIdText: key,
            fileRevision: rev,
            fileRevisionText: encodeBase64Url(rev),
            exactSize: BigInt(bytes.byteLength),
            geometry: new FileGeometry(BigInt(bytes.byteLength), 32n),
          }) as FileRevisionDescriptor,
          leaseId: fixedId(99),
          release: async () => undefined,
        })
      },
    }
    const broker: BlockRangeReader = {
      readRange: async function* (revDesc, _leaseId, span) {
        const bytes = payloads.get(revDesc.fileIdText)!
        yield Object.freeze({ offset: span.start, data: bytes.subarray(Number(span.start), Number(span.end)) })
      },
    }

    // 1. Portable ZIP of subfolder Photos/Trip -> Trip.zip containing Trip/ and Trip/1.jpg.
    const tripPolicy = new SelectionPolicy(false)
    tripPolicy.set(tripEntry, [descriptor.syntheticRootId, photosEntry.idText], true)
    const tripSelection = tripPolicy.snapshot()
    const tripSpec = selectionSpecFromPolicy(descriptor.shareInstanceId, descriptor.syntheticRootId, tripSelection)
    const tripKey = selectionKey(tripSpec)
    const tripShape = await inspectSelectionShape(descriptor, catalog, tripSelection, new AbortController().signal)
    const tripOffers = planArtifactOffers({
      epoch: 1n,
      selectionKey: tripKey,
      shape: tripShape,
      capabilities: { indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null },
    })
    if (tripOffers.kind !== 'ready' || tripOffers.zip === null) throw new Error('expected portable zip offer')
    expect(tripOffers.zip.suggestedName).toBe('Trip.zip')
    expect(tripOffers.zip.artifact.rootPath).toEqual(['Photos', 'Trip'])

    const portableWindow = createMockDownloadWindow()
    const portableSession = openPortableSession({
      task: {
        taskId: 'portable-trip',
        selection: tripSpec,
        selectionKey: tripKey,
        artifact: tripOffers.zip.artifact,
        lifecycle: initialReceiveState('portable-trip'),
      },
      windowPort: portableWindow.windowPort,
    })
    const portableResult = await new TransferJob({
      descriptor,
      catalog,
      selection: tripSelection,
      revisions,
      broker,
      output: portableSession.transferOutput,
      rootPath: artifactRootPath(tripOffers.zip.artifact),
    }).run()
    assert(portableResult.status === 'completed')
    await portableSession.settle(portableResult)
    const tripArchive = await readZipArchive(portableWindow.blobs[0]!)
    expect(tripArchive.directories).toEqual(new Set(['Trip/']))
    expect([...tripArchive.files.keys()]).toEqual(['Trip/1.jpg'])
    expect(tripArchive.files.get('Trip/1.jpg')).toEqual(payloadB)

    // 2. OPFS progressive ZIP of whole folder Photos/ across pause + reopen -> Photos/a.jpg and Photos/Trip/1.jpg.
    const wholeSelection = new SelectionPolicy(true).snapshot()
    const wholeSpec = selectionSpecFromPolicy(descriptor.shareInstanceId, descriptor.syntheticRootId, wholeSelection)
    const wholeKey = selectionKey(wholeSpec)
    const wholeShape = await inspectSelectionShape(descriptor, catalog, wholeSelection, new AbortController().signal)
    const wholeOffers = planArtifactOffers({
      epoch: 2n,
      selectionKey: wholeKey,
      shape: wholeShape,
      capabilities: { indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 1_000_000n },
    })
    if (wholeOffers.kind !== 'ready' || wholeOffers.zip === null) throw new Error('expected opfs zip offer')
    expect(wholeOffers.zip.suggestedName).toBe('Photos.zip')

    const io = new MemoryNativeObjectIO()
    const tasks = new Map<string, ReceiveTaskRecord>()
    const fileStore = new MemoryFileStore(io.events)
    const store = Object.freeze({
      putTask: async (record: ReceiveTaskRecord) => { tasks.set(record.taskId, record) },
      getTask: async (taskId: string) => tasks.get(taskId),
      listTasks: async () => [...tasks.values()],
      deleteTask: async (taskId: string) => { tasks.delete(taskId) },
      putFile: (record: ReceiveFileRecord) => fileStore.putFile(record),
      getFile: (taskId: string, fileId: string) => fileStore.getFile(taskId, fileId),
      listTaskFiles: (taskId: string) => fileStore.listTaskFiles(taskId),
    }) as unknown as ReceiveStore
    const opfsFileHandle = {
      getFile: async () => new File([io.bytes], 'opfs-photos.part'),
    } as unknown as FileSystemFileHandle
    const opfsDirHandle = {
      getDirectoryHandle: async () => opfsDirHandle,
      getFileHandle: async () => opfsFileHandle,
    } as unknown as FileSystemDirectoryHandle
    const opfsEnv = Object.freeze({
      getRootDirectory: async () => opfsDirHandle,
      openNativeObject: async () => io,
    })
    const opfsWindow = createMockDownloadWindow()
    const opfsSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-photos',
        selection: wholeSpec,
        selectionKey: wholeKey,
        artifact: wholeOffers.zip.artifact,
        lifecycle: initialReceiveState('opfs-photos'),
      },
      store,
      env: opfsEnv,
      windowPort: opfsWindow.windowPort,
    })
    const pausedTask = await opfsSession.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 0n,
      discoveredBytes: 0n,
      discoveryComplete: false,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await opfsSession.close()

    const reopenedOpfs = await reopenOPFSSession({
      task: pausedTask,
      store,
      env: opfsEnv,
      windowPort: opfsWindow.windowPort,
    })
    const opfsResult = await new TransferJob({
      descriptor,
      catalog,
      selection: wholeSelection,
      revisions,
      broker,
      output: reopenedOpfs.transferOutput,
      rootPath: artifactRootPath(pausedTask.artifact),
    }).run()
    assert(opfsResult.status === 'completed')
    await reopenedOpfs.settle(opfsResult)
    await reopenedOpfs.close()

    const opfsArchive = await readZipArchive(opfsWindow.blobs[0]!)
    expect(opfsArchive.directories).toEqual(new Set(['Photos/', 'Photos/Trip/']))
    expect([...opfsArchive.files.keys()].sort()).toEqual(['Photos/Trip/1.jpg', 'Photos/a.jpg'])
    expect(opfsArchive.files.get('Photos/a.jpg')).toEqual(payloadA)
    expect(opfsArchive.files.get('Photos/Trip/1.jpg')).toEqual(payloadB)
  })
})
