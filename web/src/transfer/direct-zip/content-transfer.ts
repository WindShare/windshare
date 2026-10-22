import type { V2ShareDescriptor } from '../../catalog/v2-records'
import { bigintToSafeNumber, byteRange } from '../../content/geometry'
import {
  V2_BLOCK_BROKER_PARALLEL_READS,
  type V2BlockRangeReader,
} from '../../content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../content/v2-session-services'
import {
  canonicalDigest, canonicalFrame, canonicalRecord, canonicalText, canonicalU64,
} from '../../output/workspace/canonical'
import { validateOpenedFileRevision } from '../job/file-authority'
import type { DirectZipOrderedFileV1, DirectZipOutputSessionV1 } from './model'

export interface DirectZipContentTransferOptionsV1 {
  readonly descriptor: V2ShareDescriptor
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly output: DirectZipOutputSessionV1
  readonly signal: AbortSignal
  readonly onInitialDurable: (bytes: bigint) => void
  readonly onWriteAcknowledged: (bytes: bigint) => void
  readonly onComplete: (exactSize: bigint) => void
}

/** One authenticated revision lease remains live until its ordered member is settled. */
export async function transferDirectZipFileV1(
  options: DirectZipContentTransferOptionsV1,
  file: DirectZipOrderedFileV1,
): Promise<void> {
  let opened: V2OpenedRevision | undefined
  let primaryFailure: unknown
  try {
    opened = validateOpenedFileRevision(
      options.descriptor,
      file.pending.entry,
      await options.revisions.open(file.pending.entry.id, options.signal),
    )
    const transaction = await options.output.beginFile(file, Object.freeze({
      fileId: opened.descriptor.fileIdText,
      revision: opened.descriptor.fileRevisionText,
      exactSize: opened.descriptor.exactSize,
      // Revision identity authenticates the complete geometry; the explicit label
      // keeps range authority distinct from a transient remote lease identifier.
      rangeAuthority: await canonicalDigest(canonicalRecord('windshare/source-range', 1, [
        canonicalFrame(canonicalText(opened.descriptor.fileRevisionText)),
        canonicalU64(opened.descriptor.geometry.blockSize),
      ])),
    }), options.signal)
    let offset = transaction.resumeOffset
    options.onInitialDurable(offset)
    for await (const block of readAtomicBlocks(options, opened, offset)) {
      await transaction.write(offset, block.data, options.signal)
      options.onWriteAcknowledged(BigInt(block.data.byteLength))
      offset = block.end
      // EOF leads straight into member completion and may be the archive's last
      // write. A cut here would copy the whole prefix merely to append its tail.
      if (offset < opened.descriptor.exactSize) await transaction.observeCheckpoint(options.signal)
    }
    await transaction.commit(options.signal)
    options.onComplete(opened.descriptor.exactSize)
  } catch (error) {
    primaryFailure = error
  }

  let releaseFailure: unknown
  if (opened !== undefined) {
    try {
      await opened.release()
    } catch (error) {
      releaseFailure = error
    }
  }
  if (primaryFailure !== undefined && releaseFailure !== undefined) {
    throw new AggregateError(
      [primaryFailure, releaseFailure],
      'direct ZIP content transfer and revision release both failed',
      { cause: primaryFailure },
    )
  }
  if (primaryFailure !== undefined) throw primaryFailure
  if (releaseFailure !== undefined) throw releaseFailure
}

async function* readAtomicBlocks(
  options: DirectZipContentTransferOptionsV1,
  opened: V2OpenedRevision,
  start: bigint,
): AsyncGenerator<{ readonly data: Uint8Array<ArrayBuffer>; readonly end: bigint }> {
  const { exactSize, geometry } = opened.descriptor
  if (start === exactSize) return
  let covered = start
  let blockStart = start
  let blockEnd = minimum((start / geometry.blockSize + 1n) * geometry.blockSize, exactSize)
  let assembled: Uint8Array<ArrayBuffer> | undefined
  // One range lets the broker prefetch across blocks while this generator keeps
  // member writes and checkpoint observations ordered and bounded in memory.
  for await (const slice of options.broker.readRange(
    opened.descriptor, opened.leaseId, byteRange(start, exactSize),
    { signal: options.signal, maximumParallel: V2_BLOCK_BROKER_PARALLEL_READS, priority: 'download' },
  )) {
    options.signal.throwIfAborted()
    if (!(slice.data instanceof Uint8Array) || slice.data.byteLength === 0 ||
        slice.offset !== covered || covered + BigInt(slice.data.byteLength) > exactSize) {
      throw new Error('direct ZIP range reader escaped, overlapped, or left a source gap')
    }
    let consumed = 0
    while (consumed < slice.data.byteLength) {
      const count = Math.min(slice.data.byteLength - consumed,
        bigintToSafeNumber(blockEnd - covered, 'direct ZIP block remainder'))
      const part = slice.data.subarray(consumed, consumed + count)
      // Production slices already match block geometry. Borrow that retained
      // copy; fragmented collaborators need at most one additional block.
      if (assembled === undefined && covered === blockStart && BigInt(count) === blockEnd - blockStart) {
        assembled = part
      } else {
        assembled ??= new Uint8Array(bigintToSafeNumber(blockEnd - blockStart, 'direct ZIP atomic source range'))
        assembled.set(part, bigintToSafeNumber(covered - blockStart, 'direct ZIP atomic slice offset'))
      }
      covered += BigInt(count)
      consumed += count
      if (covered === blockEnd) {
        yield { data: assembled, end: blockEnd }
        assembled = undefined
        blockStart = blockEnd
        blockEnd = minimum(blockEnd + geometry.blockSize, exactSize)
      }
    }
  }
  if (covered !== exactSize) throw new Error('direct ZIP range reader ended before its authenticated range')
}

function minimum(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}
