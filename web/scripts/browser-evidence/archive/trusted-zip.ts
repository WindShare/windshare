import { Readable } from 'node:stream'
import { createInflateRaw } from 'node:zlib'
import {
  type ArchiveByteSource,
  TrustedZipFailure,
  type TrustedZipEntry,
  type TrustedZipEntryVisitor,
  type TrustedZipLimits,
  type TrustedZipScanSummary,
} from './trusted-zip-contract.ts'

export {
  type ArchiveByteSource,
  TrustedZipFailure,
  type TrustedZipEntry,
  type TrustedZipEntryVisitor,
  type TrustedZipFailureKind,
  type TrustedZipLimits,
  type TrustedZipScanSummary,
} from './trusted-zip-contract.ts'

const END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x0605_4b50
const CENTRAL_DIRECTORY_HEADER_SIGNATURE = 0x0201_4b50
const LOCAL_FILE_HEADER_SIGNATURE = 0x0403_4b50
const DATA_DESCRIPTOR_SIGNATURE = 0x0807_4b50
const END_OF_CENTRAL_DIRECTORY_BYTES = 22
const MAXIMUM_ZIP_COMMENT_BYTES = 65_535
const END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES =
  END_OF_CENTRAL_DIRECTORY_BYTES + MAXIMUM_ZIP_COMMENT_BYTES
const CENTRAL_DIRECTORY_HEADER_BYTES = 46
const LOCAL_FILE_HEADER_BYTES = 30
const DATA_DESCRIPTOR_BYTES = 12
const SIGNED_DATA_DESCRIPTOR_BYTES = 16
const ZIP64_EXTRA_FIELD_ID = 0x0001
const AES_EXTRA_FIELD_ID = 0x9901
const INFOZIP_UNICODE_PATH_EXTRA_FIELD_ID = 0x7075
const ZIP64_UINT16 = 0xffff
const ZIP64_UINT32 = 0xffff_ffff
const GENERAL_PURPOSE_ENCRYPTED = 1 << 0
const GENERAL_PURPOSE_DEFLATE_OPTION_MASK = (1 << 1) | (1 << 2)
const GENERAL_PURPOSE_DATA_DESCRIPTOR = 1 << 3
const GENERAL_PURPOSE_PATCHED_DATA = 1 << 5
const GENERAL_PURPOSE_STRONG_ENCRYPTION = 1 << 6
const GENERAL_PURPOSE_UTF8 = 1 << 11
const GENERAL_PURPOSE_MASKED_LOCAL_HEADER = 1 << 13
const SUPPORTED_GENERAL_PURPOSE_FLAGS =
  GENERAL_PURPOSE_DEFLATE_OPTION_MASK |
  GENERAL_PURPOSE_DATA_DESCRIPTOR |
  GENERAL_PURPOSE_UTF8
const STORE_COMPRESSION_METHOD = 0
const DEFLATE_COMPRESSION_METHOD = 8
const MAXIMUM_SUPPORTED_ZIP_VERSION = 20
const ARCHIVE_READ_CHUNK_BYTES = 64 * 1_024

interface EndOfCentralDirectory {
  readonly offset: number
  readonly entryCount: number
  readonly centralDirectoryOffset: number
  readonly centralDirectoryBytes: number
  readonly archiveBaseOffset: number
}

interface CentralDirectoryEntry extends TrustedZipEntry {
  readonly flags: number
  readonly compressionMethod: number
  readonly modifiedTime: number
  readonly modifiedDate: number
  readonly crc32: number
  readonly localHeaderOffset: number
  readonly rawPath: Uint8Array
  readonly versionNeeded: number
  dataOffset?: number
}

interface LocalHeaderAuthority {
  readonly versionNeeded: number
  readonly flags: number
  readonly compressionMethod: number
  readonly modifiedTime: number
  readonly modifiedDate: number
  readonly crc32: number
  readonly compressedBytes: number
  readonly expandedBytes: number
  readonly pathBytes: number
  readonly extraBytes: number
}

/**
 * Parses the security-relevant ZIP subset with repository-reviewed code only.
 * Ambiguous extensions fail closed because accepting two interpretations would
 * let the uploader and guard disagree about which bytes an attachment contains.
 */
export async function scanTrustedZip(
  source: ArchiveByteSource,
  limits: TrustedZipLimits,
  visitor: TrustedZipEntryVisitor,
): Promise<TrustedZipScanSummary> {
  validateSourceAndLimits(source, limits)
  const end = await readEndOfCentralDirectory(source)
  if (end.entryCount > limits.maximumEntries) {
    throw new TrustedZipFailure(
      'archive-entry-limit',
      'ZIP entry count exceeds the trusted scanner limit',
      { observedEntryCount: end.entryCount },
    )
  }
  const entries = await readCentralDirectory(source, end, limits)
  const expandedBytes = entries.reduce((total, entry) => {
    const prospective = checkedAdd(total, entry.expandedBytes, 'ZIP expanded byte count overflows')
    if (prospective > limits.maximumExpandedBytes) {
      throw new TrustedZipFailure(
        'archive-expanded-byte-limit',
        'ZIP expanded bytes exceed the trusted scanner limit',
        { observedExpandedBytes: prospective },
      )
    }
    return prospective
  }, 0)
  await bindLocalFileRecords(source, end, entries)
  let observedExpandedBytes = 0
  for (const entry of entries) {
    await visitor.start(entry)
    const entryExpandedBytes = await visitEntryBytes(source, entry, visitor, limits, observedExpandedBytes)
    observedExpandedBytes = checkedAdd(
      observedExpandedBytes,
      entryExpandedBytes,
      'ZIP observed expanded byte count overflows',
    )
    await visitor.end(entry)
  }
  if (observedExpandedBytes !== expandedBytes) {
    throw invalidArchive('ZIP observed expanded bytes differ from central-directory authority')
  }
  return Object.freeze({
    entryCount: entries.length,
    expandedBytes: observedExpandedBytes,
    archiveBaseOffset: end.archiveBaseOffset,
  })
}

function validateSourceAndLimits(source: ArchiveByteSource, limits: TrustedZipLimits): void {
  requireSafeCounter(source.byteLength, 'ZIP source byte length')
  requireSafeCounter(limits.maximumEntries, 'ZIP maximum entry count')
  requireSafeCounter(limits.maximumExpandedBytes, 'ZIP maximum expanded bytes')
  if (!Number.isSafeInteger(limits.maximumPathBytes) || limits.maximumPathBytes < 1) {
    throw new TypeError('ZIP maximum path bytes must be a positive safe integer')
  }
}

async function readEndOfCentralDirectory(source: ArchiveByteSource): Promise<EndOfCentralDirectory> {
  if (source.byteLength < END_OF_CENTRAL_DIRECTORY_BYTES) {
    throw invalidArchive('ZIP end-of-central-directory record is absent')
  }
  const searchBytes = Math.min(source.byteLength, END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES)
  const searchOffset = source.byteLength - searchBytes
  const tail = await readExactly(source, searchOffset, searchBytes)
  const candidates: number[] = []
  for (let index = tail.byteLength - END_OF_CENTRAL_DIRECTORY_BYTES; index >= 0; index -= 1) {
    if (uint32(tail, index) !== END_OF_CENTRAL_DIRECTORY_SIGNATURE) continue
    const commentBytes = uint16(tail, index + 20)
    if (searchOffset + index + END_OF_CENTRAL_DIRECTORY_BYTES + commentBytes === source.byteLength) {
      candidates.push(index)
    }
  }
  if (candidates.length === 0) throw invalidArchive('ZIP end-of-central-directory record is absent')
  if (candidates.length !== 1) throw invalidArchive('ZIP end-of-central-directory authority is ambiguous')
  const relativeOffset = candidates[0]
  if (relativeOffset === undefined) throw new TypeError('ZIP end record candidate invariant failed')
  const offset = searchOffset + relativeOffset
  const diskNumber = uint16(tail, relativeOffset + 4)
  const centralDirectoryDisk = uint16(tail, relativeOffset + 6)
  const entriesOnDisk = uint16(tail, relativeOffset + 8)
  const entryCount = uint16(tail, relativeOffset + 10)
  const centralDirectoryBytes = uint32(tail, relativeOffset + 12)
  const centralDirectoryOffset = uint32(tail, relativeOffset + 16)
  if (
    diskNumber !== 0 || centralDirectoryDisk !== 0 || entriesOnDisk !== entryCount
  ) throw invalidArchive('multi-disk ZIP archives are unsupported')
  if (
    entriesOnDisk === ZIP64_UINT16 || entryCount === ZIP64_UINT16 ||
    centralDirectoryBytes === ZIP64_UINT32 || centralDirectoryOffset === ZIP64_UINT32
  ) throw invalidArchive('ZIP64 archives are unsupported')
  const nominalCentralDirectoryEnd = checkedAdd(
    centralDirectoryOffset,
    centralDirectoryBytes,
    'ZIP central-directory range overflows',
  )
  if (nominalCentralDirectoryEnd > offset) {
    throw invalidArchive('ZIP central directory overlaps its end record')
  }
  // A single uniform displacement is the only accepted preamble model. This
  // admits self-extracting diagnostic ZIPs without accepting hidden gaps.
  const archiveBaseOffset = offset - nominalCentralDirectoryEnd
  return Object.freeze({
    offset,
    entryCount,
    centralDirectoryOffset,
    centralDirectoryBytes,
    archiveBaseOffset,
  })
}

async function readCentralDirectory(
  source: ArchiveByteSource,
  end: EndOfCentralDirectory,
  limits: TrustedZipLimits,
): Promise<CentralDirectoryEntry[]> {
  const centralDirectoryStart = checkedAdd(
    end.archiveBaseOffset,
    end.centralDirectoryOffset,
    'ZIP central-directory offset overflows',
  )
  const centralDirectoryEnd = checkedAdd(
    centralDirectoryStart,
    end.centralDirectoryBytes,
    'ZIP central-directory range overflows',
  )
  if (centralDirectoryEnd !== end.offset) {
    throw invalidArchive('ZIP central directory is not contiguous with its end record')
  }
  const entries: CentralDirectoryEntry[] = []
  let cursor = centralDirectoryStart
  for (let index = 0; index < end.entryCount; index += 1) {
    const fixed = await readExactly(source, cursor, CENTRAL_DIRECTORY_HEADER_BYTES)
    if (uint32(fixed, 0) !== CENTRAL_DIRECTORY_HEADER_SIGNATURE) {
      throw invalidArchive('ZIP central-directory entry signature is invalid')
    }
    const versionNeeded = uint16(fixed, 6)
    const flags = uint16(fixed, 8)
    const compressionMethod = uint16(fixed, 10)
    const modifiedTime = uint16(fixed, 12)
    const modifiedDate = uint16(fixed, 14)
    const crc32 = uint32(fixed, 16)
    const compressedBytes = uint32(fixed, 20)
    const expandedBytes = uint32(fixed, 24)
    const pathBytes = uint16(fixed, 28)
    const extraBytes = uint16(fixed, 30)
    const commentBytes = uint16(fixed, 32)
    const diskStart = uint16(fixed, 34)
    const localHeaderOffset = uint32(fixed, 42)
    validateEntryFeatures({
      versionNeeded,
      flags,
      compressionMethod,
      compressedBytes,
      expandedBytes,
      diskStart,
      localHeaderOffset,
    })
    if (pathBytes === 0 || pathBytes > limits.maximumPathBytes) {
      throw new TrustedZipFailure('archive-path', 'ZIP entry path exceeds the trusted scanner limit')
    }
    const variableBytes = checkedAdd(
      checkedAdd(pathBytes, extraBytes, 'ZIP central-directory metadata length overflows'),
      commentBytes,
      'ZIP central-directory metadata length overflows',
    )
    const nextCursor = checkedAdd(
      cursor,
      checkedAdd(CENTRAL_DIRECTORY_HEADER_BYTES, variableBytes, 'ZIP central-directory record overflows'),
      'ZIP central-directory record overflows',
    )
    if (nextCursor > centralDirectoryEnd) {
      throw invalidArchive('ZIP central-directory entry exceeds its declared range')
    }
    const rawPath = await readExactly(source, cursor + CENTRAL_DIRECTORY_HEADER_BYTES, pathBytes)
    const extra = await readExactly(
      source,
      cursor + CENTRAL_DIRECTORY_HEADER_BYTES + pathBytes,
      extraBytes,
    )
    validateExtraFields(extra)
    const path = decodeEntryPath(rawPath, flags)
    const directory = path.endsWith('/')
    if (directory && (compressedBytes !== 0 || expandedBytes !== 0 || crc32 !== 0)) {
      throw invalidArchive('ZIP directory entry carries file content authority')
    }
    entries.push({
      path,
      directory,
      compressedBytes,
      expandedBytes,
      flags,
      compressionMethod,
      modifiedTime,
      modifiedDate,
      crc32,
      localHeaderOffset,
      rawPath,
      versionNeeded,
    })
    cursor = nextCursor
  }
  if (cursor !== centralDirectoryEnd) {
    throw invalidArchive('ZIP central-directory size does not match its entry records')
  }
  return entries
}

function validateEntryFeatures(entry: {
  readonly versionNeeded: number
  readonly flags: number
  readonly compressionMethod: number
  readonly compressedBytes: number
  readonly expandedBytes: number
  readonly diskStart: number
  readonly localHeaderOffset: number
}): void {
  if (entry.versionNeeded > MAXIMUM_SUPPORTED_ZIP_VERSION) {
    throw invalidArchive('ZIP entry requires an unsupported format version')
  }
  if (entry.diskStart !== 0) throw invalidArchive('multi-disk ZIP entries are unsupported')
  if (
    entry.compressedBytes === ZIP64_UINT32 || entry.expandedBytes === ZIP64_UINT32 ||
    entry.localHeaderOffset === ZIP64_UINT32
  ) throw invalidArchive('ZIP64 entries are unsupported')
  if (
    (entry.flags & (
      GENERAL_PURPOSE_ENCRYPTED |
      GENERAL_PURPOSE_PATCHED_DATA |
      GENERAL_PURPOSE_STRONG_ENCRYPTION |
      GENERAL_PURPOSE_MASKED_LOCAL_HEADER
    )) !== 0
  ) throw invalidArchive('encrypted or masked ZIP entries cannot be scanned')
  if ((entry.flags & ~SUPPORTED_GENERAL_PURPOSE_FLAGS) !== 0) {
    throw invalidArchive('ZIP entry uses unsupported general-purpose flags')
  }
  if (
    entry.compressionMethod !== STORE_COMPRESSION_METHOD &&
    entry.compressionMethod !== DEFLATE_COMPRESSION_METHOD
  ) throw invalidArchive('ZIP entry uses an unsupported compression method')
  if (
    entry.compressionMethod === STORE_COMPRESSION_METHOD &&
    (entry.flags & GENERAL_PURPOSE_DEFLATE_OPTION_MASK) !== 0
  ) throw invalidArchive('stored ZIP entry carries deflate-only flags')
  if (
    entry.compressionMethod === STORE_COMPRESSION_METHOD &&
    entry.compressedBytes !== entry.expandedBytes
  ) throw invalidArchive('stored ZIP entry compressed and expanded sizes differ')
}

async function bindLocalFileRecords(
  source: ArchiveByteSource,
  end: EndOfCentralDirectory,
  entries: CentralDirectoryEntry[],
): Promise<void> {
  const centralDirectoryStart = end.archiveBaseOffset + end.centralDirectoryOffset
  const ordered = orderedLocalEntries(entries, end, centralDirectoryStart)
  if (ordered.length === 0) {
    return
  }
  for (let index = 0; index < ordered.length; index += 1) {
    const entry = ordered[index]
    if (entry === undefined) throw new TypeError('ZIP entry ordering invariant failed')
    const nextEntry = ordered[index + 1]
    const nextOffset = nextEntry === undefined
      ? centralDirectoryStart
      : checkedAdd(end.archiveBaseOffset, nextEntry.localHeaderOffset, 'ZIP local-header offset overflows')
    await bindLocalFileRecord(source, end.archiveBaseOffset, entry, nextOffset)
  }
}

function orderedLocalEntries(
  entries: CentralDirectoryEntry[],
  end: EndOfCentralDirectory,
  centralDirectoryStart: number,
): CentralDirectoryEntry[] {
  const ordered = [...entries].sort((left, right) => left.localHeaderOffset - right.localHeaderOffset)
  if (ordered.length === 0) {
    if (centralDirectoryStart !== end.archiveBaseOffset) {
      throw invalidArchive('empty ZIP contains unaccounted local-record bytes')
    }
    return ordered
  }
  if (ordered[0]?.localHeaderOffset !== 0) {
    throw invalidArchive('ZIP local records do not begin at the archive base')
  }
  return ordered
}

async function bindLocalFileRecord(
  source: ArchiveByteSource,
  archiveBaseOffset: number,
  entry: CentralDirectoryEntry,
  nextOffset: number,
): Promise<void> {
  const localOffset = checkedAdd(
    archiveBaseOffset,
    entry.localHeaderOffset,
    'ZIP local-header offset overflows',
  )
  if (nextOffset <= localOffset) throw invalidArchive('ZIP local records overlap or share offsets')
  const fixed = await readExactly(source, localOffset, LOCAL_FILE_HEADER_BYTES)
  const local = parseLocalHeader(fixed)
  validateLocalHeaderAgreement(local, entry)
  await validateLocalPathAndExtra(source, localOffset, local, entry)
  const dataOffset = localDataOffset(localOffset, local)
  const dataEnd = checkedAdd(dataOffset, entry.compressedBytes, 'ZIP compressed data range overflows')
  if (dataEnd > nextOffset) throw invalidArchive('ZIP compressed data overlaps another record')
  await validateLocalPayloadAuthority(source, local, entry, dataEnd, nextOffset)
  entry.dataOffset = dataOffset
}

function parseLocalHeader(fixed: Uint8Array): LocalHeaderAuthority {
  if (uint32(fixed, 0) !== LOCAL_FILE_HEADER_SIGNATURE) {
    throw invalidArchive('ZIP local-file header signature is invalid')
  }
  return Object.freeze({
    versionNeeded: uint16(fixed, 4),
    flags: uint16(fixed, 6),
    compressionMethod: uint16(fixed, 8),
    modifiedTime: uint16(fixed, 10),
    modifiedDate: uint16(fixed, 12),
    crc32: uint32(fixed, 14),
    compressedBytes: uint32(fixed, 18),
    expandedBytes: uint32(fixed, 22),
    pathBytes: uint16(fixed, 26),
    extraBytes: uint16(fixed, 28),
  })
}

function validateLocalHeaderAgreement(
  local: LocalHeaderAuthority,
  entry: CentralDirectoryEntry,
): void {
  if (
    local.versionNeeded !== entry.versionNeeded || local.flags !== entry.flags ||
    local.compressionMethod !== entry.compressionMethod ||
    local.modifiedTime !== entry.modifiedTime || local.modifiedDate !== entry.modifiedDate
  ) throw invalidArchive('ZIP local-file header contradicts its central-directory entry')
  if (local.pathBytes !== entry.rawPath.byteLength) {
    throw invalidArchive('ZIP local and central entry path lengths differ')
  }
}

async function validateLocalPathAndExtra(
  source: ArchiveByteSource,
  localOffset: number,
  local: LocalHeaderAuthority,
  entry: CentralDirectoryEntry,
): Promise<void> {
  const rawPath = await readExactly(source, localOffset + LOCAL_FILE_HEADER_BYTES, local.pathBytes)
  if (!equalBytes(rawPath, entry.rawPath)) {
    throw invalidArchive('ZIP local and central entry paths differ')
  }
  const extra = await readExactly(
    source,
    localOffset + LOCAL_FILE_HEADER_BYTES + local.pathBytes,
    local.extraBytes,
  )
  validateExtraFields(extra)
}

function localDataOffset(localOffset: number, local: LocalHeaderAuthority): number {
  return checkedAdd(
    localOffset,
    checkedAdd(
      LOCAL_FILE_HEADER_BYTES,
      checkedAdd(local.pathBytes, local.extraBytes, 'ZIP local metadata length overflows'),
      'ZIP local-header length overflows',
    ),
    'ZIP local data offset overflows',
  )
}

async function validateLocalPayloadAuthority(
  source: ArchiveByteSource,
  local: LocalHeaderAuthority,
  entry: CentralDirectoryEntry,
  dataEnd: number,
  nextOffset: number,
): Promise<void> {
  if ((entry.flags & GENERAL_PURPOSE_DATA_DESCRIPTOR) !== 0) {
    validateDescriptorPlaceholders(local, entry)
    await validateDataDescriptor(source, dataEnd, nextOffset, entry)
    return
  }
  if (
    local.crc32 !== entry.crc32 || local.compressedBytes !== entry.compressedBytes ||
    local.expandedBytes !== entry.expandedBytes
  ) throw invalidArchive('ZIP local sizes or CRC contradict central-directory authority')
  if (dataEnd !== nextOffset) throw invalidArchive('ZIP local record contains unaccounted trailing bytes')
}

function validateDescriptorPlaceholders(
  local: LocalHeaderAuthority,
  entry: CentralDirectoryEntry,
): void {
  if (
    !zeroOrEqual(local.crc32, entry.crc32) ||
    !zeroOrEqual(local.compressedBytes, entry.compressedBytes) ||
    !zeroOrEqual(local.expandedBytes, entry.expandedBytes)
  ) throw invalidArchive('ZIP local descriptor placeholders contradict central-directory authority')
}

async function validateDataDescriptor(
  source: ArchiveByteSource,
  dataEnd: number,
  nextOffset: number,
  entry: CentralDirectoryEntry,
): Promise<void> {
  const descriptorBytes = nextOffset - dataEnd
  if (descriptorBytes !== DATA_DESCRIPTOR_BYTES && descriptorBytes !== SIGNED_DATA_DESCRIPTOR_BYTES) {
    throw invalidArchive('ZIP data descriptor has an unsupported or ambiguous length')
  }
  const encoded = await readExactly(source, dataEnd, descriptorBytes)
  const valueOffset = descriptorBytes === SIGNED_DATA_DESCRIPTOR_BYTES ? 4 : 0
  if (
    descriptorBytes === SIGNED_DATA_DESCRIPTOR_BYTES &&
    uint32(encoded, 0) !== DATA_DESCRIPTOR_SIGNATURE
  ) throw invalidArchive('ZIP signed data descriptor has an invalid signature')
  if (
    uint32(encoded, valueOffset) !== entry.crc32 ||
    uint32(encoded, valueOffset + 4) !== entry.compressedBytes ||
    uint32(encoded, valueOffset + 8) !== entry.expandedBytes
  ) throw invalidArchive('ZIP data descriptor contradicts central-directory authority')
}

async function visitEntryBytes(
  source: ArchiveByteSource,
  entry: CentralDirectoryEntry,
  visitor: TrustedZipEntryVisitor,
  limits: TrustedZipLimits,
  previouslyExpandedBytes: number,
): Promise<number> {
  if (entry.dataOffset === undefined) throw new TypeError('ZIP local record was not bound')
  let crc = 0xffff_ffff
  let expandedBytes = 0
  const consume = async (value: Uint8Array): Promise<void> => {
    const chunk = Uint8Array.from(value)
    expandedBytes = checkedAdd(expandedBytes, chunk.byteLength, 'ZIP entry output length overflows')
    const total = checkedAdd(
      previouslyExpandedBytes,
      expandedBytes,
      'ZIP observed expanded byte count overflows',
    )
    if (total > limits.maximumExpandedBytes) {
      throw new TrustedZipFailure(
        'archive-expanded-byte-limit',
        'ZIP expanded bytes exceed the trusted scanner limit',
        { observedExpandedBytes: total },
      )
    }
    if (expandedBytes > entry.expandedBytes) {
      throw invalidArchive('ZIP entry expands beyond its central-directory size')
    }
    crc = updateCrc32(crc, chunk)
    await visitor.chunk(entry, chunk)
  }
  if (entry.compressionMethod === STORE_COMPRESSION_METHOD) {
    for await (const chunk of readRange(source, entry.dataOffset, entry.compressedBytes)) {
      await consume(chunk)
    }
  } else {
    await inflateEntry(source, entry, consume)
  }
  if (expandedBytes !== entry.expandedBytes) {
    throw invalidArchive('ZIP entry expanded size differs from central-directory authority')
  }
  if (((crc ^ 0xffff_ffff) >>> 0) !== entry.crc32) {
    throw invalidArchive('ZIP entry CRC-32 differs from central-directory authority')
  }
  return expandedBytes
}

async function inflateEntry(
  source: ArchiveByteSource,
  entry: CentralDirectoryEntry,
  consume: (chunk: Uint8Array) => Promise<void>,
): Promise<void> {
  const compressed = Readable.from(readRange(source, entry.dataOffset ?? 0, entry.compressedBytes))
  const inflated = compressed.pipe(createInflateRaw())
  try {
    for await (const value of inflated) {
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value as Uint8Array)
      try {
        await consume(chunk)
      } catch (cause) {
        inflated.destroy()
        compressed.destroy()
        throw new VisitorFailure(cause)
      }
    }
    if (inflated.bytesWritten !== entry.compressedBytes) {
      throw invalidArchive('ZIP deflate stream did not consume its complete compressed range')
    }
  } catch (cause) {
    if (cause instanceof VisitorFailure) throw cause.cause
    throw invalidArchive('ZIP deflate stream is malformed', cause)
  }
}

class VisitorFailure {
  readonly cause: unknown

  constructor(cause: unknown) {
    this.cause = cause
  }
}

async function* readRange(
  source: ArchiveByteSource,
  offset: number,
  length: number,
): AsyncGenerator<Uint8Array> {
  let consumed = 0
  while (consumed < length) {
    const chunkBytes = Math.min(ARCHIVE_READ_CHUNK_BYTES, length - consumed)
    yield await readExactly(source, offset + consumed, chunkBytes)
    consumed += chunkBytes
  }
}

async function readExactly(
  source: ArchiveByteSource,
  offset: number,
  length: number,
): Promise<Uint8Array> {
  requireSafeCounter(offset, 'ZIP read offset')
  requireSafeCounter(length, 'ZIP read length')
  if (checkedAdd(offset, length, 'ZIP read range overflows') > source.byteLength) {
    throw invalidArchive('ZIP record exceeds the archive byte range')
  }
  const bytes = await source.readExactly(offset, length)
  if (bytes.byteLength !== length) throw invalidArchive('ZIP source returned a short read')
  return Uint8Array.from(bytes)
}

function validateExtraFields(extra: Uint8Array): void {
  let cursor = 0
  while (cursor < extra.byteLength) {
    if (extra.byteLength - cursor < 4) throw invalidArchive('ZIP extra field header is truncated')
    const identifier = uint16(extra, cursor)
    const valueBytes = uint16(extra, cursor + 2)
    cursor = checkedAdd(cursor, 4, 'ZIP extra field offset overflows')
    const next = checkedAdd(cursor, valueBytes, 'ZIP extra field range overflows')
    if (next > extra.byteLength) throw invalidArchive('ZIP extra field value is truncated')
    if (identifier === ZIP64_EXTRA_FIELD_ID) throw invalidArchive('ZIP64 extra fields are unsupported')
    if (identifier === AES_EXTRA_FIELD_ID) throw invalidArchive('AES-encrypted ZIP entries are unsupported')
    if (identifier === INFOZIP_UNICODE_PATH_EXTRA_FIELD_ID) {
      throw invalidArchive('ambiguous alternate ZIP path encodings are unsupported')
    }
    cursor = next
  }
}

function decodeEntryPath(rawPath: Uint8Array, flags: number): string {
  if ((flags & GENERAL_PURPOSE_UTF8) !== 0) {
    try {
      return new TextDecoder('utf-8', { fatal: true }).decode(rawPath)
    } catch (cause) {
      throw invalidArchive('ZIP entry path is not valid UTF-8', cause)
    }
  }
  if ([...rawPath].some((byte) => byte > 0x7f)) {
    throw invalidArchive('legacy non-ASCII ZIP entry path encoding is unsupported')
  }
  return Buffer.from(rawPath).toString('ascii')
}

function checkedAdd(left: number, right: number, message: string): number {
  const result = left + right
  if (!Number.isSafeInteger(result) || result < 0) throw invalidArchive(message)
  return result
}

function requireSafeCounter(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError(`${label} must be a non-negative safe integer`)
  }
}

function uint16(bytes: Uint8Array, offset: number): number {
  return (bytes[offset] ?? 0) | ((bytes[offset + 1] ?? 0) << 8)
}

function uint32(bytes: Uint8Array, offset: number): number {
  return (
    (bytes[offset] ?? 0) |
    ((bytes[offset + 1] ?? 0) << 8) |
    ((bytes[offset + 2] ?? 0) << 16) |
    ((bytes[offset + 3] ?? 0) << 24)
  ) >>> 0
}

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((byte, index) => byte === right[index])
}

function zeroOrEqual(value: number, expected: number): boolean {
  return value === 0 || value === expected
}

function invalidArchive(message: string, cause?: unknown): TrustedZipFailure {
  return new TrustedZipFailure(
    'invalid-archive',
    message,
    cause === undefined ? {} : { cause },
  )
}

const CRC32_TABLE = Uint32Array.from({ length: 256 }, (_, index) => {
  let value = index
  for (let bit = 0; bit < 8; bit += 1) {
    value = (value & 1) === 0 ? value >>> 1 : 0xedb8_8320 ^ (value >>> 1)
  }
  return value >>> 0
})

function updateCrc32(crc: number, bytes: Uint8Array): number {
  let value = crc
  for (const byte of bytes) {
    value = (CRC32_TABLE[(value ^ byte) & 0xff] ?? 0) ^ (value >>> 8)
  }
  return value >>> 0
}
