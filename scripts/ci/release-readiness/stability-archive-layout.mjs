export const STABILITY_ARCHIVE_LAYOUT_ERROR_CODE = 'stability-archive-layout-invalid'

const END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x06054b50
const CENTRAL_DIRECTORY_FILE_SIGNATURE = 0x02014b50
const LOCAL_FILE_HEADER_SIGNATURE = 0x04034b50
const DATA_DESCRIPTOR_SIGNATURE = 0x08074b50
const END_OF_CENTRAL_DIRECTORY_BYTES = 22
const CENTRAL_DIRECTORY_FILE_BYTES = 46
const LOCAL_FILE_HEADER_BYTES = 30
const DATA_DESCRIPTOR_BYTES = 12
const SIGNED_DATA_DESCRIPTOR_BYTES = 16
const DATA_DESCRIPTOR_FLAG = 1 << 3
const ZIP64_SENTINEL = 0xffffffff
const MAXIMUM_LAYOUT_ENTRIES = 64

export class StabilityArchiveLayoutError extends Error {
  constructor(message) {
    super(message)
    this.name = 'StabilityArchiveLayoutError'
    this.code = STABILITY_ARCHIVE_LAYOUT_ERROR_CODE
  }
}

export function prevalidateStabilityArchiveLayout(value) {
  const archive = asBuffer(value)
  const endOffset = findEndOfCentralDirectory(archive)
  const diskNumber = archive.readUInt16LE(endOffset + 4)
  const directoryDisk = archive.readUInt16LE(endOffset + 6)
  const entriesOnDisk = archive.readUInt16LE(endOffset + 8)
  const totalEntries = archive.readUInt16LE(endOffset + 10)
  const directorySize = archive.readUInt32LE(endOffset + 12)
  const directoryOffset = archive.readUInt32LE(endOffset + 16)
  if (
    diskNumber !== 0 || directoryDisk !== 0 || entriesOnDisk !== totalEntries ||
    totalEntries < 1 || totalEntries > MAXIMUM_LAYOUT_ENTRIES ||
    directoryOffset === ZIP64_SENTINEL || directorySize === ZIP64_SENTINEL ||
    directoryOffset + directorySize !== endOffset
  ) {
    throw layoutError('stability archive directory layout is unsupported')
  }

  let centralOffset = directoryOffset
  for (let index = 0; index < totalEntries; index += 1) {
    requireRange(
      archive,
      centralOffset,
      CENTRAL_DIRECTORY_FILE_BYTES,
      endOffset,
      'central directory header',
    )
    if (archive.readUInt32LE(centralOffset) !== CENTRAL_DIRECTORY_FILE_SIGNATURE) {
      throw layoutError('stability archive central directory signature is invalid')
    }
    const flags = archive.readUInt16LE(centralOffset + 8)
    const compression = archive.readUInt16LE(centralOffset + 10)
    const compressedSize = archive.readUInt32LE(centralOffset + 20)
    const nameLength = archive.readUInt16LE(centralOffset + 28)
    const extraLength = archive.readUInt16LE(centralOffset + 30)
    const commentLength = archive.readUInt16LE(centralOffset + 32)
    const startDisk = archive.readUInt16LE(centralOffset + 34)
    const localOffset = archive.readUInt32LE(centralOffset + 42)
    if (
      startDisk !== 0 || compressedSize === ZIP64_SENTINEL || localOffset === ZIP64_SENTINEL
    ) {
      throw layoutError('ZIP64 or multi-disk stability archive entries are unsupported')
    }
    centralOffset = checkedEnd(
      centralOffset,
      CENTRAL_DIRECTORY_FILE_BYTES + nameLength + extraLength + commentLength,
      endOffset,
      'central directory entry',
    )

    requireRange(
      archive,
      localOffset,
      LOCAL_FILE_HEADER_BYTES,
      directoryOffset,
      'local file header',
    )
    if (archive.readUInt32LE(localOffset) !== LOCAL_FILE_HEADER_SIGNATURE) {
      throw layoutError('stability archive local header signature is invalid')
    }
    const localFlags = archive.readUInt16LE(localOffset + 6)
    const localCompression = archive.readUInt16LE(localOffset + 8)
    const localCompressedSize = archive.readUInt32LE(localOffset + 18)
    if (
      localFlags !== flags || localCompression !== compression ||
      (flags & DATA_DESCRIPTOR_FLAG) === 0 && localCompressedSize !== compressedSize
    ) {
      throw layoutError('stability archive local layout disagrees with its central directory')
    }
    const localNameLength = archive.readUInt16LE(localOffset + 26)
    const localExtraLength = archive.readUInt16LE(localOffset + 28)
    const dataOffset = checkedEnd(
      localOffset,
      LOCAL_FILE_HEADER_BYTES + localNameLength + localExtraLength,
      directoryOffset,
      'local file header fields',
    )
    const dataEnd = checkedEnd(
      dataOffset,
      compressedSize,
      directoryOffset,
      'entry payload',
    )
    if ((flags & DATA_DESCRIPTOR_FLAG) !== 0) {
      const descriptorBytes =
        dataEnd + 4 <= directoryOffset &&
          archive.readUInt32LE(dataEnd) === DATA_DESCRIPTOR_SIGNATURE
          ? SIGNED_DATA_DESCRIPTOR_BYTES
          : DATA_DESCRIPTOR_BYTES
      checkedEnd(dataEnd, descriptorBytes, directoryOffset, 'entry data descriptor')
    }
  }
  if (centralOffset !== endOffset) {
    throw layoutError('stability archive central directory size disagrees')
  }
}

function findEndOfCentralDirectory(archive) {
  for (
    let offset = archive.length - END_OF_CENTRAL_DIRECTORY_BYTES;
    offset >= 0;
    offset -= 1
  ) {
    if (archive.readUInt32LE(offset) !== END_OF_CENTRAL_DIRECTORY_SIGNATURE) continue
    const commentLength = archive.readUInt16LE(offset + 20)
    if (offset + END_OF_CENTRAL_DIRECTORY_BYTES + commentLength === archive.length) return offset
  }
  throw layoutError('stability archive has no valid ZIP end record')
}

function requireRange(archive, start, length, limit, label) {
  checkedEnd(start, length, limit, label)
  if (start + length > archive.length) throw layoutError(`stability archive ${label} escapes its bytes`)
}

function checkedEnd(start, length, limit, label) {
  if (
    !Number.isSafeInteger(start) || !Number.isSafeInteger(length) ||
    start < 0 || length < 0 || start + length > limit
  ) {
    throw layoutError(`stability archive ${label} overlaps protected ZIP structures`)
  }
  return start + length
}

function asBuffer(value) {
  if (Buffer.isBuffer(value)) return value
  if (value instanceof Uint8Array) return Buffer.from(value.buffer, value.byteOffset, value.byteLength)
  if (value instanceof ArrayBuffer) return Buffer.from(value)
  throw layoutError('stability archive layout input must be bytes')
}

function layoutError(message) {
  return new StabilityArchiveLayoutError(message)
}
