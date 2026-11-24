import {
  BlobReader,
  Reader,
  Uint8ArrayWriter,
  ZipReader,
  type FileEntry,
} from '@zip.js/zip.js'
import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import { NATIVE_WRITE_CHUNK_BYTES } from '../../src/output/native-object/client'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import { ObjectCheckpointCoordinator } from '../../src/output/native-object/coordinator'
import type { ReceiveStore } from '../../src/output/store/database'
import {
  fileRecordId,
  type ReceiveFileRecord,
  type ZipSpoolChunkRecord,
} from '../../src/output/store/types'
import { zipMemberCoordinates } from '../../src/output/zip/member'
import {
  ZIP_UINT16_SENTINEL,
  ZIP_UINT32_SENTINEL,
  encodeZipCentralDirectoryRecord,
  encodeZipDataDescriptor,
  encodeZipEndRecords,
  encodeZipLocalHeader,
  normalizeZipEntry,
  planZipEntry,
  requiresZip64End,
} from '../../src/output/zip/policy'
import { buildPartialZipBlob, ProgressiveZipWriter } from '../../src/output/zip/progressive-zip'
import { StreamingZipWriter } from '../../src/output/zip/streaming-zip'

interface SparseSegment {
  readonly offset: bigint
  readonly bytes: Uint8Array
}

/**
 * Sparse NativeObjectIO that represents multi-GiB offsets without allocating gigabytes of RAM,
 * enabling real ZipReader verification of >4 GiB ZIP64 structures in milliseconds.
 */
class SparseNativeObjectIO implements NativeObjectIO {
  readonly #segments: SparseSegment[] = []
  #length = 0n

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    if (data.byteLength === 0) return
    const copy = data.slice()
    this.#segments.push({ offset, bytes: copy })
    const end = offset + BigInt(copy.byteLength)
    if (end > this.#length) this.#length = end
  }

  async truncate(length: bigint): Promise<void> {
    this.#length = length
    for (let index = this.#segments.length - 1; index >= 0; index -= 1) {
      const seg = this.#segments[index]!
      if (seg.offset >= length) {
        this.#segments.splice(index, 1)
      } else if (seg.offset + BigInt(seg.bytes.byteLength) > length) {
        const keep = Number(length - seg.offset)
        this.#segments[index] = { offset: seg.offset, bytes: seg.bytes.subarray(0, keep) }
      }
    }
  }

  async size(): Promise<bigint> {
    return this.#length
  }

  async flush(): Promise<void> { /* In-memory sparse test backend. */ }
  async close(): Promise<void> { /* In-memory sparse test backend. */ }

  readRange(start: bigint, end: bigint): Uint8Array<ArrayBuffer> {
    const length = Number(end - start)
    const output = new Uint8Array(length)
    for (const seg of this.#segments) {
      const segEnd = seg.offset + BigInt(seg.bytes.byteLength)
      if (segEnd <= start || seg.offset >= end) continue
      const copyStart = seg.offset > start ? seg.offset : start
      const copyEnd = segEnd < end ? segEnd : end
      const srcOffset = Number(copyStart - seg.offset)
      const dstOffset = Number(copyStart - start)
      const count = Number(copyEnd - copyStart)
      output.set(seg.bytes.subarray(srcOffset, srcOffset + count), dstOffset)
    }
    return output
  }
}

class SparseZipReader extends Reader<SparseNativeObjectIO> {
  readonly #io: SparseNativeObjectIO

  constructor(io: SparseNativeObjectIO) {
    super(io)
    this.#io = io
  }

  override async init(): Promise<void> {
    this.size = Number(await this.#io.size())
  }

  override async readUint8Array(index: number, length: number): Promise<Uint8Array> {
    const start = BigInt(index)
    const total = await this.#io.size()
    const remaining = start >= total ? 0n : total - start
    const count = remaining < BigInt(length) ? remaining : BigInt(length)
    return this.#io.readRange(start, start + count)
  }
}

class MemoryZipStore {
  readonly files = new Map<string, ReceiveFileRecord>()
  readonly spool = new Map<string, Uint8Array>()

  async putFile(record: ReceiveFileRecord): Promise<void> {
    this.files.set(record.id, record)
  }

  async getFile(taskId: string, fileId: string): Promise<ReceiveFileRecord | undefined> {
    return this.files.get(fileRecordId(taskId, fileId))
  }

  async listTaskFiles(taskId: string): Promise<readonly ReceiveFileRecord[]> {
    return [...this.files.values()].filter(row => row.taskId === taskId)
  }

  async putZipSpoolChunk(chunk: ZipSpoolChunkRecord): Promise<void> {
    this.spool.set(`${chunk.taskId}:${chunk.index}`, chunk.bytes.slice())
  }

  async getZipSpoolChunk(taskId: string, index: number): Promise<Uint8Array | undefined> {
    return this.spool.get(`${taskId}:${index}`)
  }

  async deleteZipSpool(taskId: string): Promise<void> {
    const prefix = `${taskId}:`
    for (const key of [...this.spool.keys()]) {
      if (key.startsWith(prefix)) this.spool.delete(key)
    }
  }
}

describe('ZIP64 encoding boundaries and scale invariants', () => {
  it('switches cleanly from classic ZIP32 to ZIP64 at exact 16-bit count and 32-bit size/offset thresholds', () => {
    const underSize = planZipEntry(
      normalizeZipEntry({ kind: 'file', path: ['a.bin'], exactSize: ZIP_UINT32_SENTINEL - 1n }),
      ZIP_UINT32_SENTINEL - 1n,
    )
    expect(underSize).toMatchObject({
      zip64Size: false,
      zip64Offset: false,
      versionNeeded: 20,
      localExtraBytes: 0n,
      descriptorBytes: 16n,
      centralZip64ValueCount: 0,
      centralExtraBytes: 0n,
    })

    const exactSize = planZipEntry(
      normalizeZipEntry({ kind: 'file', path: ['a.bin'], exactSize: ZIP_UINT32_SENTINEL }),
      0n,
    )
    expect(exactSize).toMatchObject({
      zip64Size: true,
      zip64Offset: false,
      versionNeeded: 45,
      localExtraBytes: 20n,
      descriptorBytes: 24n,
      centralZip64ValueCount: 2,
      centralExtraBytes: 20n,
    })
    expect(encodeZipLocalHeader(exactSize).byteLength).toBe(Number(exactSize.localHeaderBytes))
    expect(encodeZipDataDescriptor(exactSize, 0x1234_5678).byteLength).toBe(24)
    expect(encodeZipCentralDirectoryRecord(exactSize, 0x1234_5678).byteLength).toBe(Number(exactSize.centralRecordBytes))

    const exactBoth = planZipEntry(
      normalizeZipEntry({ kind: 'file', path: ['b.bin'], exactSize: ZIP_UINT32_SENTINEL }),
      ZIP_UINT32_SENTINEL,
    )
    expect(exactBoth).toMatchObject({
      zip64Size: true,
      zip64Offset: true,
      versionNeeded: 45,
      centralZip64ValueCount: 3,
      centralExtraBytes: 28n,
    })

    const classicLayout = {
      entryCount: ZIP_UINT16_SENTINEL - 1n,
      centralDirectoryOffset: ZIP_UINT32_SENTINEL - 1n,
      centralDirectoryBytes: ZIP_UINT32_SENTINEL - 1n,
    }
    expect(requiresZip64End(classicLayout)).toBe(false)
    expect(encodeZipEndRecords({ ...classicLayout, zip64EndRequired: false }).zip64End).toBeUndefined()

    for (const crossing of [
      { ...classicLayout, entryCount: ZIP_UINT16_SENTINEL },
      { ...classicLayout, centralDirectoryOffset: ZIP_UINT32_SENTINEL },
      { ...classicLayout, centralDirectoryBytes: ZIP_UINT32_SENTINEL },
    ]) {
      expect(requiresZip64End(crossing)).toBe(true)
      const encoded = encodeZipEndRecords({ ...crossing, zip64EndRequired: true })
      expect(encoded.zip64End?.byteLength).toBe(56)
      expect(encoded.zip64Locator?.byteLength).toBe(20)
      expect(encoded.classicEnd.byteLength).toBe(22)
    }
  })

  it('finalizes and partially exports ProgressiveZipWriter archives crossing the 4 GiB ZIP64 boundary', async () => {
    const taskId = 'zip64-over-4gib'
    const fiveGiB = 5_000_000_000n
    const io = new SparseNativeObjectIO()
    const store = new MemoryZipStore()

    // Seed a completed 5 GiB member so the next entry lands strictly above 4 GiB without
    // allocating or hashing 5 GiB in RAM.
    const bigPlan = planZipEntry(normalizeZipEntry({ kind: 'file', path: ['big.bin'], exactSize: fiveGiB }), 0n)
    const bigCoords = zipMemberCoordinates(bigPlan)
    await io.writeAt(bigPlan.localHeaderOffset, encodeZipLocalHeader(bigPlan))
    await io.writeAt(bigCoords.descriptorOffset, encodeZipDataDescriptor(bigPlan, 0))
    await store.putFile({
      id: fileRecordId(taskId, 'big'),
      taskId,
      fileId: 'big',
      revision: 'r-big',
      path: ['big.bin'],
      size: fiveGiB,
      completedRanges: [[0n, fiveGiB]],
      status: 'completed',
      zipEntry: {
        sequence: 1,
        kind: 'file',
        localHeaderOffset: bigPlan.localHeaderOffset,
        payloadOffset: bigCoords.payloadOffset,
        descriptorOffset: bigCoords.descriptorOffset,
        endOffset: bigCoords.endOffset,
        crcRanges: [],
        crc32: 0,
      },
    })

    const coord = new ObjectCheckpointCoordinator({ io, operationId: taskId, objectId: 'over-4gib.part' })
    const writer = await ProgressiveZipWriter.open(taskId, store as unknown as ReceiveStore, coord)
    const tailPayload = new TextEncoder().encode('beyond-4gib-offset!')
    const after = await writer.openFile({
      fileId: 'after',
      path: ['nested', 'after-4gib.txt'],
      revision: 'r-after',
      exactSize: BigInt(tailPayload.byteLength),
    })
    expect(after.record.zipEntry.localHeaderOffset).toBeGreaterThan(ZIP_UINT32_SENTINEL)
    expect(after.plan.zip64Offset).toBe(true)
    await writer.writeFileSlice(after, 0n, tailPayload)
    await writer.commitFile(after, [byteRange(0n, BigInt(tailPayload.byteLength))])

    const finalSize = await writer.finalize()
    expect(finalSize).toBeGreaterThan(fiveGiB)

    const reader = new ZipReader(new SparseZipReader(io), { checkSignature: true, useWebWorkers: false })
    try {
      const entries = await reader.getEntries()
      expect(entries.map(entry => entry.filename)).toEqual(['big.bin', 'nested/after-4gib.txt'])
      const bigEntry = entries[0]!
      const afterEntry = entries[1]! as FileEntry
      expect(bigEntry.uncompressedSize).toBe(Number(fiveGiB))
      expect(bigEntry.zip64).toBe(true)
      expect(afterEntry.zip64).toBe(true)
      const extracted = await afterEntry.getData(new Uint8ArrayWriter())
      expect(new TextDecoder().decode(extracted)).toBe('beyond-4gib-offset!')
    } finally {
      await reader.close()
    }

    // Partial export must also slice a completed entry sitting past 4 GiB when the 5 GiB entry is incomplete.
    const bigRow = (await store.getFile(taskId, 'big'))!
    await store.putFile({ ...bigRow, status: 'receiving' })
    const sparseFile = {
      size: Number(finalSize),
      slice(start: number, end: number): Blob {
        return new Blob([io.readRange(BigInt(start), BigInt(end))])
      },
    } as unknown as File
    const partialBlob = buildPartialZipBlob(sparseFile, await store.listTaskFiles(taskId))
    const partialReader = new ZipReader(new BlobReader(partialBlob), { checkSignature: true, useWebWorkers: false })
    try {
      const partialEntries = await partialReader.getEntries()
      expect(partialEntries.map(entry => entry.filename)).toEqual(['nested/after-4gib.txt'])
      const data = await (partialEntries[0]! as FileEntry).getData(new Uint8ArrayWriter())
      expect(new TextDecoder().decode(data)).toBe('beyond-4gib-offset!')
    } finally {
      await partialReader.close()
    }
  })

  it('finalizes 65,536 entries with ZIP64 EOCD and bounded BatchedTailWriter chunks readable by ZipReader', async () => {
    const taskId = 'zip64-65536'
    const seededCount = Number(ZIP_UINT16_SENTINEL)
    const io = new SparseNativeObjectIO()
    const store = new MemoryZipStore()

    let cursor = 0n
    for (let index = 0; index < seededCount; index += 1) {
      const fileId = `e${index}`
      const name = `f${index.toString(36)}.txt`
      const plan = planZipEntry(normalizeZipEntry({ kind: 'file', path: [name], exactSize: 0n }), cursor)
      const coords = zipMemberCoordinates(plan)
      store.files.set(fileRecordId(taskId, fileId), {
        id: fileRecordId(taskId, fileId),
        taskId,
        fileId,
        revision: 'r',
        path: [name],
        size: 0n,
        completedRanges: [],
        status: 'completed',
        zipEntry: {
          sequence: index + 1,
          kind: 'file',
          localHeaderOffset: plan.localHeaderOffset,
          payloadOffset: coords.payloadOffset,
          descriptorOffset: coords.descriptorOffset,
          endOffset: coords.endOffset,
          crcRanges: [],
          crc32: 0,
        },
      })
      cursor = coords.endOffset
    }
    // Establish physical EOF past the seeded zero-byte slots so recovery treats them as intact.
    await io.truncate(cursor)

    const coord = new ObjectCheckpointCoordinator({ io, operationId: taskId, objectId: 'many.part' })
    const writer = await ProgressiveZipWriter.open(taskId, store as unknown as ReceiveStore, coord)
    const lastPayload = new TextEncoder().encode('entry-65536-payload')
    const last = await writer.openFile({
      fileId: 'last',
      path: ['last-65536.txt'],
      revision: 'r-last',
      exactSize: BigInt(lastPayload.byteLength),
    })
    await writer.writeFileSlice(last, 0n, lastPayload)
    await writer.commitFile(last, [byteRange(0n, BigInt(lastPayload.byteLength))])

    const writeSpy = vi.spyOn(io, 'writeAt')
    const finalSize = await writer.finalize()
    expect(writeSpy.mock.calls.length).toBeGreaterThan(10)
    for (const [, bytes] of writeSpy.mock.calls) {
      expect(bytes.byteLength).toBeGreaterThan(0)
      expect(bytes.byteLength).toBeLessThanOrEqual(NATIVE_WRITE_CHUNK_BYTES)
    }

    // Classic EOCD must carry 0xffff entry sentinels pointing at the ZIP64 EOCD.
    const classicTail = io.readRange(finalSize - 22n, finalSize)
    const classicView = new DataView(classicTail.buffer, classicTail.byteOffset, classicTail.byteLength)
    expect(classicView.getUint32(0, true)).toBe(0x0605_4b50)
    expect(classicView.getUint16(8, true)).toBe(0xffff)
    expect(classicView.getUint16(10, true)).toBe(0xffff)

    const reader = new ZipReader(new SparseZipReader(io), { checkSignature: true, useWebWorkers: false })
    try {
      const entries = await reader.getEntries()
      expect(entries).toHaveLength(seededCount + 1)
      const tailEntry = entries.at(-1)! as FileEntry
      expect(tailEntry.filename).toBe('last-65536.txt')
      const data = await tailEntry.getData(new Uint8ArrayWriter())
      expect(new TextDecoder().decode(data)).toBe('entry-65536-payload')
    } finally {
      await reader.close()
    }
  })

  it('spools StreamingZipWriter central directory across multiple zip-spool chunks and cleans up on finalize and abort', async () => {
    const store = new MemoryZipStore()
    const putSpy = vi.spyOn(store, 'putZipSpoolChunk')
    const writer = new StreamingZipWriter({
      taskId: 'stream-spool-ok',
      store: store as unknown as ReceiveStore,
      maxBytes: 4n * 1024n * 1024n,
    })

    // ~300 entries with 900-byte names produce >256 KiB of central directory records (>1 spool chunk).
    const prefix = 'p'.repeat(900)
    const entryCount = 300
    for (let index = 0; index < entryCount; index += 1) {
      const payload = index === 0 || index === entryCount - 1
        ? new TextEncoder().encode(`marker-${index}`)
        : new Uint8Array(0)
      const file = writer.beginFile([`${prefix}-${String(index).padStart(4, '0')}.txt`], BigInt(payload.byteLength))
      if (payload.byteLength > 0) file.write(payload)
      await file.close()
    }
    expect(putSpy.mock.calls.length).toBeGreaterThanOrEqual(1)

    const blob = await writer.finalize()
    expect(putSpy.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(store.spool.size).toBe(0)

    const reader = new ZipReader(new BlobReader(blob), { checkSignature: true, useWebWorkers: false })
    try {
      const entries = await reader.getEntries()
      expect(entries).toHaveLength(entryCount)
      const firstData = await (entries[0]! as FileEntry).getData(new Uint8ArrayWriter())
      const lastData = await (entries.at(-1)! as FileEntry).getData(new Uint8ArrayWriter())
      expect(new TextDecoder().decode(firstData)).toBe('marker-0')
      expect(new TextDecoder().decode(lastData)).toBe(`marker-${entryCount - 1}`)
    } finally {
      await reader.close()
    }

    // Aborting after spilling chunks must also purge the zip-spool store.
    const aborted = new StreamingZipWriter({
      taskId: 'stream-spool-abort',
      store: store as unknown as ReceiveStore,
      maxBytes: 4n * 1024n * 1024n,
    })
    for (let index = 0; index < entryCount; index += 1) {
      const file = aborted.beginFile([`${prefix}-${String(index).padStart(4, '0')}.txt`], 0n)
      await file.close()
    }
    expect(store.spool.size).toBeGreaterThanOrEqual(1)
    await aborted.abort()
    expect(store.spool.size).toBe(0)
  })
})
