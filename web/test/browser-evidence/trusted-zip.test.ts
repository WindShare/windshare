import { deflateRawSync } from 'node:zlib'

import { describe, expect, it } from 'vitest'

import {
  scanTrustedZip,
  type ArchiveByteSource,
  type TrustedZipEntry,
} from '../../scripts/browser-evidence/archive/trusted-zip.ts'

const LOCAL_HEADER_BYTES = 30
const CENTRAL_HEADER_BYTES = 46
const END_RECORD_BYTES = 22
const UTF8_FLAG = 1 << 11
const DESCRIPTOR_FLAG = 1 << 3

interface FixtureEntry {
  readonly path: string
  readonly data: Uint8Array
  readonly compressionMethod?: 0 | 8
  readonly descriptor?: boolean
  readonly compressedSuffix?: Uint8Array
}

interface BuiltZip {
  readonly bytes: Buffer
  readonly centralOffset: number
  readonly localOffsets: readonly number[]
  readonly descriptorOffsets: readonly (number | null)[]
}

describe('repository-trusted ZIP scanner', () => {
  it('streams stored and deflated entries while verifying CRC, descriptors, and a uniform preamble', async () => {
    const built = buildZip([
      { path: 'logs/stored.txt', data: Buffer.from('stored') },
      { path: 'logs/deflated.txt', data: Buffer.from('deflated'), compressionMethod: 8, descriptor: true },
    ], Buffer.from('MZ trusted preamble'))
    const observed = new Map<string, Buffer[]>()
    const summary = await scan(built.bytes, {
      start(entry) { observed.set(entry.path, []) },
      chunk(entry, bytes) { observed.get(entry.path)?.push(Buffer.from(bytes)) },
      end() {},
    })
    expect(summary).toMatchObject({ entryCount: 2, expandedBytes: 14 })
    expect(Buffer.concat(observed.get('logs/stored.txt') ?? []).toString()).toBe('stored')
    expect(Buffer.concat(observed.get('logs/deflated.txt') ?? []).toString()).toBe('deflated')
  })

  it('rejects CRC corruption and deflate bytes left outside the authenticated stream', async () => {
    const crcCorrupt = buildZip([{ path: 'crc.txt', data: Buffer.from('payload') }])
    const payloadOffset = LOCAL_HEADER_BYTES + Buffer.byteLength('crc.txt')
    crcCorrupt.bytes.writeUInt8(crcCorrupt.bytes.readUInt8(payloadOffset) ^ 0xff, payloadOffset)
    await expect(scan(crcCorrupt.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })

    const trailingDeflate = buildZip([{
      path: 'deflate.txt',
      data: Buffer.from('payload'),
      compressionMethod: 8,
      compressedSuffix: Buffer.from([0xde, 0xad]),
    }])
    await expect(scan(trailingDeflate.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })
  })

  it('rejects local/central contradictions, duplicate local authority, and descriptor corruption', async () => {
    const methodMismatch = buildZip([{ path: 'method.txt', data: Buffer.from('payload') }])
    methodMismatch.bytes.writeUInt16LE(8, 8)
    await expect(scan(methodMismatch.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })

    const overlap = buildZip([
      { path: 'first.txt', data: Buffer.from('first') },
      { path: 'second.txt', data: Buffer.from('second') },
    ])
    const secondCentral = overlap.centralOffset + CENTRAL_HEADER_BYTES + Buffer.byteLength('first.txt')
    overlap.bytes.writeUInt32LE(0, secondCentral + 42)
    await expect(scan(overlap.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })

    const descriptor = buildZip([{
      path: 'descriptor.txt',
      data: Buffer.from('payload'),
      descriptor: true,
    }])
    const descriptorOffset = descriptor.descriptorOffsets[0]
    if (descriptorOffset === null || descriptorOffset === undefined) {
      throw new Error('fixture did not create a descriptor')
    }
    descriptor.bytes.writeUInt32LE(0, descriptorOffset)
    await expect(scan(descriptor.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })
  })

  it('rejects trailing bytes, encryption, ambiguous end authority, and non-UTF-8 names', async () => {
    const trailing = buildZip([{ path: 'file.txt', data: Buffer.from('payload') }])
    await expect(scan(Buffer.concat([trailing.bytes, Buffer.from([0])]))).rejects.toMatchObject({
      kind: 'invalid-archive',
    })

    const encrypted = buildZip([{ path: 'encrypted.txt', data: Buffer.from('payload') }])
    encrypted.bytes.writeUInt16LE(UTF8_FLAG | 1, 6)
    encrypted.bytes.writeUInt16LE(UTF8_FLAG | 1, encrypted.centralOffset + 8)
    await expect(scan(encrypted.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })

    const ambiguous = buildZip([{ path: 'ambiguous.txt', data: Buffer.from('payload') }])
    const endOffset = ambiguous.bytes.byteLength - END_RECORD_BYTES
    const comment = Buffer.alloc(END_RECORD_BYTES)
    comment.writeUInt32LE(0x0605_4b50, 0)
    comment.writeUInt16LE(0, 4)
    comment.writeUInt16LE(0, 6)
    comment.writeUInt16LE(0, 8)
    comment.writeUInt16LE(0, 10)
    comment.writeUInt32LE(0, 12)
    comment.writeUInt32LE(0, 16)
    comment.writeUInt16LE(0, 20)
    ambiguous.bytes.writeUInt16LE(comment.byteLength, endOffset + 20)
    await expect(scan(Buffer.concat([ambiguous.bytes, comment]))).rejects.toMatchObject({
      kind: 'invalid-archive',
    })

    const legacy = buildZip([{ path: 'legacy.txt', data: Buffer.from('payload') }])
    legacy.bytes.writeUInt16LE(0, 6)
    legacy.bytes.writeUInt16LE(0, legacy.centralOffset + 8)
    legacy.bytes[LOCAL_HEADER_BYTES] = 0x80
    legacy.bytes[legacy.centralOffset + CENTRAL_HEADER_BYTES] = 0x80
    await expect(scan(legacy.bytes)).rejects.toMatchObject({ kind: 'invalid-archive' })
  })

  it('reports declared entry and expanded-byte bombs with bounded typed evidence', async () => {
    const entries = buildZip([
      { path: 'one.txt', data: Buffer.from('one') },
      { path: 'two.txt', data: Buffer.from('two') },
    ])
    await expect(scan(entries.bytes, undefined, { maximumEntries: 1 })).rejects.toMatchObject({
      kind: 'archive-entry-limit',
      observedEntryCount: 2,
    })

    const expanded = buildZip([{ path: 'expanded.txt', data: Buffer.from('four') }])
    await expect(scan(expanded.bytes, undefined, { maximumExpandedBytes: 3 })).rejects.toMatchObject({
      kind: 'archive-expanded-byte-limit',
      observedExpandedBytes: 4,
    })
  })
})

async function scan(
  bytes: Uint8Array,
  visitor: {
    start(entry: TrustedZipEntry): void
    chunk(entry: TrustedZipEntry, chunk: Uint8Array): void
    end(entry: TrustedZipEntry): void
  } = { start() {}, chunk() {}, end() {} },
  limitOverrides: { readonly maximumEntries?: number; readonly maximumExpandedBytes?: number } = {},
) {
  const frozen = Uint8Array.from(bytes)
  const source: ArchiveByteSource = {
    byteLength: frozen.byteLength,
    async readExactly(offset, length) { return frozen.slice(offset, offset + length) },
  }
  return scanTrustedZip(source, {
    maximumEntries: limitOverrides.maximumEntries ?? 100,
    maximumExpandedBytes: limitOverrides.maximumExpandedBytes ?? 1_024 * 1_024,
    maximumPathBytes: 1_024,
  }, visitor)
}

function buildZip(entries: readonly FixtureEntry[], preamble = Buffer.alloc(0)): BuiltZip {
  const localRecords: Buffer[] = []
  const centralRecords: Buffer[] = []
  const localOffsets: number[] = []
  const descriptorOffsets: (number | null)[] = []
  let localOffset = 0
  for (const entry of entries) {
    const path = Buffer.from(entry.path, 'utf8')
    const data = Buffer.from(entry.data)
    const method = entry.compressionMethod ?? 0
    const coreCompressed = method === 8 ? deflateRawSync(data) : data
    const compressed = Buffer.concat([coreCompressed, Buffer.from(entry.compressedSuffix ?? [])])
    const crc = crc32(data)
    const flags = UTF8_FLAG | (entry.descriptor === true ? DESCRIPTOR_FLAG : 0)
    const local = Buffer.alloc(LOCAL_HEADER_BYTES)
    local.writeUInt32LE(0x0403_4b50, 0)
    local.writeUInt16LE(20, 4)
    local.writeUInt16LE(flags, 6)
    local.writeUInt16LE(method, 8)
    if (entry.descriptor !== true) {
      local.writeUInt32LE(crc, 14)
      local.writeUInt32LE(compressed.byteLength, 18)
      local.writeUInt32LE(data.byteLength, 22)
    }
    local.writeUInt16LE(path.byteLength, 26)
    const descriptor = entry.descriptor === true ? Buffer.alloc(12) : Buffer.alloc(0)
    if (entry.descriptor === true) {
      descriptor.writeUInt32LE(crc, 0)
      descriptor.writeUInt32LE(compressed.byteLength, 4)
      descriptor.writeUInt32LE(data.byteLength, 8)
    }
    localOffsets.push(localOffset)
    descriptorOffsets.push(entry.descriptor === true
      ? preamble.byteLength + localOffset + local.byteLength + path.byteLength + compressed.byteLength
      : null)
    localRecords.push(local, path, compressed, descriptor)

    const central = Buffer.alloc(CENTRAL_HEADER_BYTES)
    central.writeUInt32LE(0x0201_4b50, 0)
    central.writeUInt16LE(0x0314, 4)
    central.writeUInt16LE(20, 6)
    central.writeUInt16LE(flags, 8)
    central.writeUInt16LE(method, 10)
    central.writeUInt32LE(crc, 16)
    central.writeUInt32LE(compressed.byteLength, 20)
    central.writeUInt32LE(data.byteLength, 24)
    central.writeUInt16LE(path.byteLength, 28)
    central.writeUInt32LE(localOffset, 42)
    centralRecords.push(central, path)
    localOffset += local.byteLength + path.byteLength + compressed.byteLength + descriptor.byteLength
  }
  const central = Buffer.concat(centralRecords)
  const end = Buffer.alloc(END_RECORD_BYTES)
  end.writeUInt32LE(0x0605_4b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(central.byteLength, 12)
  end.writeUInt32LE(localOffset, 16)
  return {
    bytes: Buffer.concat([preamble, ...localRecords, central, end]),
    centralOffset: preamble.byteLength + localOffset,
    localOffsets,
    descriptorOffsets,
  }
}

const CRC_TABLE = Uint32Array.from({ length: 256 }, (_, index) => {
  let value = index
  for (let bit = 0; bit < 8; bit += 1) {
    value = (value & 1) === 0 ? value >>> 1 : 0xedb8_8320 ^ (value >>> 1)
  }
  return value >>> 0
})

function crc32(bytes: Uint8Array): number {
  let value = 0xffff_ffff
  for (const byte of bytes) {
    value = (CRC_TABLE[(value ^ byte) & 0xff] ?? 0) ^ (value >>> 8)
  }
  return (value ^ 0xffff_ffff) >>> 0
}
