import { createHash } from 'node:crypto'
import { TextDecoder } from 'node:util'
import { crc32, inflateRawSync } from 'node:zlib'

import {
  STABILITY_RESULT_SCHEMA_VERSION,
  STABILITY_STARTED_EVENT_SCHEMA_VERSION,
  parseStabilityResult,
  parseStabilityStartedEvent,
  stabilityEvidenceDigest,
} from './result.mjs'

export const MAXIMUM_ARTIFACT_ARCHIVE_BYTES = 64 * 1024
export const MAXIMUM_STABILITY_RESULT_BYTES = 8 * 1024

const END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x06054b50
const CENTRAL_DIRECTORY_FILE_SIGNATURE = 0x02014b50
const LOCAL_FILE_HEADER_SIGNATURE = 0x04034b50
const DATA_DESCRIPTOR_SIGNATURE = 0x08074b50
const END_OF_CENTRAL_DIRECTORY_BYTES = 22
const CENTRAL_DIRECTORY_FILE_BYTES = 46
const LOCAL_FILE_HEADER_BYTES = 30
const ZIP64_SENTINEL = 0xffffffff
const DATA_DESCRIPTOR_FLAG = 1 << 3
const ENCRYPTED_FLAG = 1
const STORED_COMPRESSION = 0
const DEFLATE_COMPRESSION = 8
const MAXIMUM_COMPRESSION_RATIO = 64
const MAXIMUM_ARCHIVE_ENTRIES = 64

export function sha256ArtifactDigest(value) {
  return `sha256:${createHash('sha256').update(asBuffer(value)).digest('hex')}`
}

export function parseStabilityResultArchive(value) {
  const archive = asBuffer(value)
  if (archive.length === 0 || archive.length > MAXIMUM_ARTIFACT_ARCHIVE_BYTES) {
    throw new Error(`stability artifact archive exceeds ${MAXIMUM_ARTIFACT_ARCHIVE_BYTES} bytes`)
  }

  const documents = readEntries(archive)
    .filter((entry) =>
      entry.readable &&
      !entry.directory &&
      entry.uncompressedSize <= MAXIMUM_STABILITY_RESULT_BYTES)
    .map((entry) => readDocument(entry))
    .filter((document) => document !== undefined)

  const startedCandidates = []
  const resultCandidates = []
  for (const document of documents) {
    let envelope
    try {
      envelope = JSON.parse(document.text)
    } catch {
      continue
    }
    if (envelope === null || typeof envelope !== 'object' || Array.isArray(envelope)) continue
    if (envelope.schema_version === STABILITY_STARTED_EVENT_SCHEMA_VERSION) {
      startedCandidates.push(Object.freeze({
        event: parseStabilityStartedEvent(document.text),
        digest: stabilityEvidenceDigest(document.bytes),
      }))
    } else if (envelope.schema_version === STABILITY_RESULT_SCHEMA_VERSION) {
      resultCandidates.push(parseStabilityResult(document.text))
    }
  }

  if (startedCandidates.length !== 1) {
    throw new Error(startedCandidates.length === 0
      ? 'stability artifact has no structured started event'
      : 'stability artifact contains duplicate structured started events')
  }
  if (resultCandidates.length !== 1) {
    throw new Error(resultCandidates.length === 0
      ? 'stability artifact has no structured finished result'
      : 'stability artifact contains duplicate structured finished results')
  }

  const [{ event: started, digest }] = startedCandidates
  const [result] = resultCandidates
  const identityMatches =
    started.workflow_run_id === result.workflow_run_id &&
    started.workflow_run_attempt === result.workflow_run_attempt &&
    started.commit_sha === result.commit_sha &&
    started.workflow_job === result.workflow_job &&
    started.operating_system === result.operating_system &&
    started.suite === result.suite &&
    started.invocation_id === result.invocation_id &&
    started.evidence_epoch === result.evidence_epoch &&
    result.started_event_sha256 === digest
  if (!identityMatches) {
    throw new Error('stability artifact started and finished evidence disagree')
  }
  return result
}

function readEntries(archive) {
  const endOffset = findEndOfCentralDirectory(archive)
  const diskNumber = archive.readUInt16LE(endOffset + 4)
  const directoryDisk = archive.readUInt16LE(endOffset + 6)
  const entriesOnDisk = archive.readUInt16LE(endOffset + 8)
  const totalEntries = archive.readUInt16LE(endOffset + 10)
  const directorySize = archive.readUInt32LE(endOffset + 12)
  const directoryOffset = archive.readUInt32LE(endOffset + 16)
  if (diskNumber !== 0 || directoryDisk !== 0 || entriesOnDisk !== totalEntries) {
    throw new Error('multi-disk stability artifacts are unsupported')
  }
  if (totalEntries < 1 || totalEntries > MAXIMUM_ARCHIVE_ENTRIES) {
    throw new Error('stability artifact entry count is invalid')
  }
  if ([directoryOffset, directorySize].includes(ZIP64_SENTINEL)) {
    throw new Error('ZIP64 stability artifacts are unsupported')
  }
  if (directoryOffset + directorySize !== endOffset) {
    throw new Error('stability artifact central directory is malformed')
  }

  const entries = []
  let offset = directoryOffset
  for (let index = 0; index < totalEntries; index += 1) {
    if (
      offset + CENTRAL_DIRECTORY_FILE_BYTES > endOffset ||
      archive.readUInt32LE(offset) !== CENTRAL_DIRECTORY_FILE_SIGNATURE
    ) {
      throw new Error('stability artifact central directory signature is invalid')
    }
    const flags = archive.readUInt16LE(offset + 8)
    const compression = archive.readUInt16LE(offset + 10)
    const checksum = archive.readUInt32LE(offset + 16)
    const compressedSize = archive.readUInt32LE(offset + 20)
    const uncompressedSize = archive.readUInt32LE(offset + 24)
    const nameLength = archive.readUInt16LE(offset + 28)
    const extraLength = archive.readUInt16LE(offset + 30)
    const commentLength = archive.readUInt16LE(offset + 32)
    const startDisk = archive.readUInt16LE(offset + 34)
    const localOffset = archive.readUInt32LE(offset + 42)
    if (
      startDisk !== 0 ||
      [compressedSize, uncompressedSize, localOffset].includes(ZIP64_SENTINEL)
    ) {
      throw new Error('ZIP64 or multi-disk stability artifact entries are unsupported')
    }
    const nameBytes = boundedSlice(
      archive,
      offset + CENTRAL_DIRECTORY_FILE_BYTES,
      nameLength,
      'central directory name',
    )
    const name = decodeEntryName(nameBytes)

    const entryEnd = offset + CENTRAL_DIRECTORY_FILE_BYTES + nameLength + extraLength + commentLength
    if (entryEnd > endOffset) throw new Error('stability artifact central directory entry is malformed')
    const local = readLocalEntry(archive, directoryOffset, {
      name,
      nameBytes,
      flags,
      compression,
      checksum,
      compressedSize,
      uncompressedSize,
      localOffset,
    })
    entries.push(Object.freeze({
      name,
      directory: name.endsWith('/'),
      readable:
        (flags & ENCRYPTED_FLAG) === 0 &&
        (compression === STORED_COMPRESSION || compression === DEFLATE_COMPRESSION),
      compression,
      crc: checksum,
      compressedSize,
      uncompressedSize,
      data: local.data,
      localStart: localOffset,
      localEnd: local.end,
    }))
    offset = entryEnd
  }
  if (offset !== endOffset) throw new Error('stability artifact central directory size disagrees')

  const byOffset = [...entries].sort((left, right) => left.localStart - right.localStart)
  for (let index = 1; index < byOffset.length; index += 1) {
    if (byOffset[index].localStart < byOffset[index - 1].localEnd) {
      throw new Error('stability artifact local entries overlap')
    }
  }
  return entries
}

function readLocalEntry(archive, directoryOffset, expected) {
  const { localOffset } = expected
  if (
    localOffset + LOCAL_FILE_HEADER_BYTES > directoryOffset ||
    archive.readUInt32LE(localOffset) !== LOCAL_FILE_HEADER_SIGNATURE
  ) {
    throw new Error('stability artifact local header is malformed')
  }
  const localFlags = archive.readUInt16LE(localOffset + 6)
  const localCompression = archive.readUInt16LE(localOffset + 8)
  const localChecksum = archive.readUInt32LE(localOffset + 14)
  const localCompressedSize = archive.readUInt32LE(localOffset + 18)
  const localUncompressedSize = archive.readUInt32LE(localOffset + 22)
  const localNameLength = archive.readUInt16LE(localOffset + 26)
  const localExtraLength = archive.readUInt16LE(localOffset + 28)
  if (localFlags !== expected.flags || localCompression !== expected.compression) {
    throw new Error('stability artifact local header disagrees with its central directory')
  }
  const localName = boundedSlice(
    archive,
    localOffset + LOCAL_FILE_HEADER_BYTES,
    localNameLength,
    'local header name',
  )
  if (!localName.equals(expected.nameBytes)) throw new Error('stability artifact entry names disagree')

  const dataOffset = localOffset + LOCAL_FILE_HEADER_BYTES + localNameLength + localExtraLength
  const data = boundedSlice(archive, dataOffset, expected.compressedSize, 'compressed entry')
  const dataEnd = dataOffset + expected.compressedSize
  if ((expected.flags & DATA_DESCRIPTOR_FLAG) === 0) {
    if (
      localChecksum !== expected.checksum ||
      localCompressedSize !== expected.compressedSize ||
      localUncompressedSize !== expected.uncompressedSize
    ) {
      throw new Error('stability artifact local sizes disagree with its central directory')
    }
    return { data, end: dataEnd }
  }
  return {
    data,
    end: validateDataDescriptor(
      archive,
      dataEnd,
      directoryOffset,
      expected.checksum,
      expected.compressedSize,
      expected.uncompressedSize,
    ),
  }
}

function readDocument(entry) {
  if (entry.uncompressedSize < 1) return undefined
  if (
    entry.compressedSize < 1 ||
    entry.uncompressedSize > entry.compressedSize * MAXIMUM_COMPRESSION_RATIO
  ) {
    return undefined
  }
  let bytes
  try {
    bytes = decompressEntry(entry)
  } catch {
    return undefined
  }
  if (
    bytes.length !== entry.uncompressedSize ||
    (crc32(bytes) >>> 0) !== entry.crc
  ) {
    return undefined
  }
  let text
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return undefined
  }
  return Object.freeze({ bytes, text })
}

function findEndOfCentralDirectory(archive) {
  for (let offset = archive.length - END_OF_CENTRAL_DIRECTORY_BYTES; offset >= 0; offset -= 1) {
    if (archive.readUInt32LE(offset) !== END_OF_CENTRAL_DIRECTORY_SIGNATURE) continue
    const commentLength = archive.readUInt16LE(offset + 20)
    if (offset + END_OF_CENTRAL_DIRECTORY_BYTES + commentLength === archive.length) return offset
  }
  throw new Error('stability artifact has no valid ZIP end record')
}

function validateDataDescriptor(archive, start, directoryOffset, checksum, compressedSize, uncompressedSize) {
  let offset = start
  let length = 12
  if (
    offset + 16 <= directoryOffset &&
    archive.readUInt32LE(offset) === DATA_DESCRIPTOR_SIGNATURE
  ) {
    offset += 4
    length = 16
  }
  if (start + length > directoryOffset) {
    throw new Error('stability artifact data descriptor is malformed')
  }
  if (
    archive.readUInt32LE(offset) !== checksum ||
    archive.readUInt32LE(offset + 4) !== compressedSize ||
    archive.readUInt32LE(offset + 8) !== uncompressedSize
  ) {
    throw new Error('stability artifact data descriptor disagrees with its central directory')
  }
  return start + length
}

function decompressEntry(entry) {
  if (entry.compression === STORED_COMPRESSION) return Buffer.from(entry.data)
  try {
    return inflateRawSync(entry.data, { maxOutputLength: MAXIMUM_STABILITY_RESULT_BYTES })
  } catch (cause) {
    throw new Error('stability artifact entry cannot be safely decompressed', { cause })
  }
}

function decodeEntryName(value) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(value)
  } catch (cause) {
    throw new Error('stability artifact entry name is not UTF-8', { cause })
  }
}

function boundedSlice(buffer, start, length, label) {
  if (
    !Number.isSafeInteger(start) ||
    !Number.isSafeInteger(length) ||
    start < 0 ||
    length < 0 ||
    start + length > buffer.length
  ) {
    throw new Error(`stability artifact ${label} escapes the archive`)
  }
  return buffer.subarray(start, start + length)
}

function asBuffer(value) {
  if (Buffer.isBuffer(value)) return value
  if (value instanceof Uint8Array) return Buffer.from(value.buffer, value.byteOffset, value.byteLength)
  if (value instanceof ArrayBuffer) return Buffer.from(value)
  throw new Error('stability artifact archive must be bytes')
}
