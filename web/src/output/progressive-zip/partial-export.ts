import type { CompleteZipEntrySpan } from './archive'
import {
  checkedZipAdd, encodeZipCentralDirectoryRecord, encodeZipDataDescriptor,
  encodeZipEndRecords, encodeZipLocalHeader, normalizeZipEntry, planZipEntry, requiresZip64End,
} from '../zip-layout/policy'

export const MAX_PARTIAL_ZIP_BLOB_ENTRIES = 16_384
export const DEFAULT_PARTIAL_ZIP_DETACH_BYTES = 16n * 1024n * 1024n

export interface PartialZipExportInput {
  /** Both scans must read the same committed checkpoint while its reader lease is held. */
  readonly entries: () => AsyncIterable<CompleteZipEntrySpan>
  readonly source: Blob
  readonly output: WritableStream<Uint8Array>
  readonly signal: AbortSignal
}

export interface PartialZipBlobInput {
  readonly entries: () => AsyncIterable<CompleteZipEntrySpan>
  readonly source: Blob
  readonly signal: AbortSignal
  readonly Blob?: typeof Blob
  readonly maxEntries?: number
  readonly detachMaxBytes?: bigint
}

export interface PartialZipBlobResult {
  readonly blob: Blob
  readonly entryCount: bigint
  readonly exactBytes: bigint
  /** True when payload bytes were copied off the OPFS File so the reader lease can end immediately. */
  readonly detached: boolean
}

/** Explicit partial saving writes directly to the chosen destination, leaving recovery unchanged. */
export async function exportCompleteZipEntries(input: PartialZipExportInput): Promise<{
  readonly entryCount: bigint
  readonly exactBytes: bigint
}> {
  const writer = input.output.getWriter()
  let offset = 0n
  let entryCount = 0n
  try {
    for await (const span of input.entries()) {
      input.signal.throwIfAborted()
      const plan = partialEntryPlan(span, offset)
      await writer.write(encodeZipLocalHeader(plan))
      const end = checkedZipAdd(span.payloadOffset, span.length)
      if (end > BigInt(input.source.size) || end > BigInt(Number.MAX_SAFE_INTEGER)) {
        throw new RangeError('partial export span exceeds its committed source')
      }
      const reader = input.source.slice(Number(span.payloadOffset), Number(end)).stream().getReader()
      try {
        for (;;) {
          input.signal.throwIfAborted()
          const chunk = await reader.read()
          if (chunk.done) break
          await writer.write(chunk.value)
        }
      } finally {
        await reader.cancel().catch(() => undefined)
        reader.releaseLock()
      }
      await writer.write(encodeZipDataDescriptor(plan, span.crc32))
      offset = checkedZipAdd(offset, plan.entryStreamBytes)
      entryCount += 1n
    }
    const centralDirectoryOffset = offset
    let entryOffset = 0n
    let replayCount = 0n
    // Replaying committed pages avoids retaining a second in-memory entry catalogue.
    for await (const span of input.entries()) {
      input.signal.throwIfAborted()
      const plan = partialEntryPlan(span, entryOffset)
      const record = encodeZipCentralDirectoryRecord(plan, span.crc32)
      await writer.write(record)
      offset = checkedZipAdd(offset, BigInt(record.byteLength))
      entryOffset = checkedZipAdd(entryOffset, plan.entryStreamBytes)
      replayCount += 1n
    }
    if (replayCount !== entryCount || entryOffset !== centralDirectoryOffset) {
      throw new TypeError('partial export checkpoint changed between scans')
    }
    const centralDirectoryBytes = offset - centralDirectoryOffset
    const end = encodeZipEndRecords({
      entryCount, centralDirectoryOffset, centralDirectoryBytes,
      zip64EndRequired: requiresZip64End({ entryCount, centralDirectoryOffset, centralDirectoryBytes }),
    })
    for (const bytes of [end.zip64End, end.zip64Locator, end.classicEnd]) {
      if (bytes === undefined) continue
      await writer.write(bytes)
      offset = checkedZipAdd(offset, BigInt(bytes.byteLength))
    }
    await writer.close()
    return Object.freeze({ entryCount, exactBytes: offset })
  } catch (error) {
    await writer.abort(error).catch(() => undefined)
    throw error
  } finally {
    writer.releaseLock()
  }
}

/**
 * Assembles complete entries as a browser-downloadable ZIP Blob without writing a second
 * OPFS file. Small exports detach payload bytes so the retained OPFS file is immediately
 * free for continuation or cleanup; larger exports keep zero-copy File.slice() references.
 */
export async function buildCompleteZipBlob(input: PartialZipBlobInput): Promise<PartialZipBlobResult> {
  const maxEntries = input.maxEntries ?? MAX_PARTIAL_ZIP_BLOB_ENTRIES
  if (!Number.isSafeInteger(maxEntries) || maxEntries <= 0) {
    throw new RangeError('partial export entry limit must be a positive safe integer')
  }
  const detachMaxBytes = input.detachMaxBytes ?? DEFAULT_PARTIAL_ZIP_DETACH_BYTES
  if (detachMaxBytes < 0n) {
    throw new RangeError('partial export detach byte limit must be non-negative')
  }
  const bodyParts: BlobPart[] = []
  const centralParts: Uint8Array<ArrayBuffer>[] = []
  let offset = 0n
  let centralDirectoryBytes = 0n
  let entryCount = 0n
  for await (const span of input.entries()) {
    input.signal.throwIfAborted()
    if (entryCount >= BigInt(maxEntries)) {
      throw new DOMException(
        'Partial browser download exceeds the maximum supported entry count',
        'NotSupportedError',
      )
    }
    const plan = partialEntryPlan(span, offset)
    const end = checkedZipAdd(span.payloadOffset, span.length)
    if (end > BigInt(input.source.size) || end > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new RangeError('partial export span exceeds its committed source')
    }
    bodyParts.push(encodeZipLocalHeader(plan))
    if (span.length > 0n) {
      bodyParts.push(input.source.slice(Number(span.payloadOffset), Number(end)))
    }
    bodyParts.push(encodeZipDataDescriptor(plan, span.crc32))
    const record = encodeZipCentralDirectoryRecord(plan, span.crc32)
    centralParts.push(record)
    offset = checkedZipAdd(offset, plan.entryStreamBytes)
    centralDirectoryBytes = checkedZipAdd(centralDirectoryBytes, BigInt(record.byteLength))
    entryCount += 1n
  }
  const centralDirectoryOffset = offset
  let exactBytes = checkedZipAdd(centralDirectoryOffset, centralDirectoryBytes)
  const end = encodeZipEndRecords({
    entryCount, centralDirectoryOffset, centralDirectoryBytes,
    zip64EndRequired: requiresZip64End({ entryCount, centralDirectoryOffset, centralDirectoryBytes }),
  })
  for (const bytes of [end.zip64End, end.zip64Locator, end.classicEnd]) {
    if (bytes === undefined) continue
    centralParts.push(bytes)
    exactBytes = checkedZipAdd(exactBytes, BigInt(bytes.byteLength))
  }
  const detached = exactBytes <= detachMaxBytes
  const parts: BlobPart[] = detached
    ? await materializeDetachedParts(bodyParts, centralParts, input.signal)
    : [...bodyParts, ...centralParts]
  const BlobConstructor = input.Blob ?? Blob
  const blob = new BlobConstructor(parts, { type: 'application/zip' })
  if (BigInt(blob.size) !== exactBytes) {
    throw new TypeError('partial export Blob length differs from its ZIP plan')
  }
  return Object.freeze({ blob, entryCount, exactBytes, detached })
}

async function materializeDetachedParts(
  bodyParts: readonly BlobPart[],
  centralParts: readonly Uint8Array<ArrayBuffer>[],
  signal: AbortSignal,
): Promise<BlobPart[]> {
  const detached: BlobPart[] = []
  for (const part of bodyParts) {
    signal.throwIfAborted()
    detached.push(part instanceof Uint8Array ? part : await (part as Blob).arrayBuffer())
  }
  detached.push(...centralParts)
  return detached
}

function partialEntryPlan(span: CompleteZipEntrySpan, offset: bigint) {
  return planZipEntry(span.entry.zipPlan ?? normalizeZipEntry({
    kind: span.entry.kind, path: span.entry.path, exactSize: span.length,
  }), offset)
}
