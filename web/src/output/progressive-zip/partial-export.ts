import type { CompleteZipEntrySpan } from './archive'
import {
  checkedZipAdd, encodeZipCentralDirectoryRecord, encodeZipDataDescriptor,
  encodeZipEndRecords, encodeZipLocalHeader, normalizeZipEntry, planZipEntry, requiresZip64End,
} from '../zip-layout/policy'

export interface PartialZipExportInput {
  /** Both scans must read the same committed checkpoint while its reader lease is held. */
  readonly entries: () => AsyncIterable<CompleteZipEntrySpan>
  readonly source: Blob
  readonly output: WritableStream<Uint8Array>
  readonly signal: AbortSignal
}

/** Explicit partial saving writes directly to the chosen destination, leaving recovery unchanged. */
export async function exportCompleteZipEntries(input: PartialZipExportInput): Promise<{
  readonly entryCount: bigint
  readonly exactBytes: bigint
}> {
  const writer = input.output.getWriter()
  let offset = 0n
  let entryCount = 0n
  let zip64 = false
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
      zip64 ||= plan.zip64Size || plan.zip64Offset
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
      zip64EndRequired: zip64 || requiresZip64End({ entryCount, centralDirectoryOffset, centralDirectoryBytes }),
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

function partialEntryPlan(span: CompleteZipEntrySpan, offset: bigint) {
  return planZipEntry(span.entry.zipPlan ?? normalizeZipEntry({
    kind: span.entry.kind, path: span.entry.path, exactSize: span.length,
  }), offset)
}
