import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import { ObjectCheckpointCoordinator } from '../../src/output/native-object/coordinator'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord } from '../../src/output/store/types'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../../src/output/zip/progressive-zip'
import { StreamingZipWriter } from '../../src/output/zip/streaming-zip'

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

async function readZipEntries(blob: Blob): Promise<Map<string, Uint8Array>> {
  const reader = new ZipReader(new BlobReader(blob))
  try {
    const entries = await reader.getEntries()
    const result = new Map<string, Uint8Array>()
    for (const entry of entries) {
      if ('getData' in entry && !entry.directory) {
        result.set(entry.filename, await (entry as FileEntry).getData(new Uint8ArrayWriter()))
      }
    }
    return result
  } finally {
    await reader.close()
  }
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
    const reopenedA = await writer2.openFile({
      fileId: 'fa', path: ['docs', 'a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    })
    expect(reopenedA.alreadyCompleted).toBe(true)

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
